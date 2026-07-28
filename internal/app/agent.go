package app

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// toolOpenTag/toolCloseTag are built without embedding the literal sentinel in
// source strings that the outer tool parser also scans.
var toolOpenTag = string(rune(60)) + "tool"
var toolCloseTag = string(rune(60)) + "/tool"

// toolBlock remains for stream-cleaning; parseResponse uses scanToolBlocks so a
// closing tag inside a JSON string cannot terminate the call early.
var toolBlock = regexp.MustCompile(`(?is)` + toolOpenTag + `\s+name\s*=\s*["']?([a-zA-Z][a-zA-Z0-9_-]*)["']?\s*>(.*?)` + toolCloseTag + `\s*>`)

// resultBlock matches tool-result blocks produced by the CLI. The model
// sometimes hallucinates them inside its own reply, so strip them from display.
var resultBlock = regexp.MustCompile(`(?s)` + toolOpenTag + `_result[^>]*>.*?` + string(rune(60)) + `/tool_result>`)

// toolBlockAlt matches function-call-style tags some models emit, so they can
// be normalised to the canonical named form before scanning.
var toolBlockAlt = regexp.MustCompile(`(?s)` + toolOpenTag + `>\s*([a-zA-Z][a-zA-Z0-9_-]*)\s*\(\s*(\{.*?\})\s*\)\s*` + toolCloseTag + `\s*>`)

var toolStartName = regexp.MustCompile(`(?is)\bname\s*=\s*["']?([a-zA-Z][a-zA-Z0-9_-]*)["']?`)

type parsedToolBlock struct {
	start int
	end   int
	name  string
	raw   string
}

// parseResponse splits a model reply into prose shown to the user and tool
// calls requested by the model.
func parseResponse(text string) (narration string, calls []ToolCall) {
	text = toolBlockAlt.ReplaceAllString(text, toolOpenTag+` name="$1">$2`+toolCloseTag+`>`)
	matches := scanToolBlocks(text)

	// A reply can end mid-tool-call (output limit or derailment) leaving an
	// opening <tool> tag with no closing tag. Surface it as an error call so
	// the agent loops feed it back and the model resends, instead of the call
	// silently vanishing and the turn ending with nothing done.
	inc, hasInc := findIncompleteToolCall(text, matches)

	if len(matches) == 0 && !hasInc {
		narration = resultBlock.ReplaceAllString(text, "")
		return strings.TrimSpace(narration), nil
	}

	// Prose is the reply minus every tool span: complete blocks, plus the
	// truncated fragment (which runs to the next complete block or the end).
	type span struct{ start, end int }
	spans := make([]span, 0, len(matches)+1)
	for _, mb := range matches {
		spans = append(spans, span{mb.start, mb.end})
	}
	if hasInc {
		incEnd := len(text)
		for _, mb := range matches {
			if mb.start > inc.start {
				incEnd = mb.start
				break
			}
		}
		spans = append(spans, span{inc.start, incEnd})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })

	var prose strings.Builder
	last := 0
	for _, s := range spans {
		if s.start < last {
			continue
		}
		prose.WriteString(text[last:s.start])
		last = s.end
	}
	prose.WriteString(text[last:])

	for _, mb := range matches {
		raw := strings.TrimSpace(mb.raw)
		raw = stripFences(raw)

		tc := ToolCall{Name: mb.name, Args: map[string]any{}}
		if raw != "" {
			if detail, ok := parseArgs(raw, &tc.Args); !ok {
				tc.Args = map[string]any{"__parse_error": true, "__raw": oneLine(raw, 300)}
				if detail != "" {
					tc.Args["__err"] = detail
				}
			}
		}
		calls = append(calls, tc)
	}

	if hasInc {
		name := inc.name
		if name == "" {
			name = "unknown"
		}
		calls = append(calls, ToolCall{Name: name, Args: map[string]any{
			"__truncated": true, "__raw": oneLine(strings.TrimSpace(inc.raw), 300)}})
	}

	narration = resultBlock.ReplaceAllString(prose.String(), "")
	narration = strings.TrimSpace(narration)
	return narration, dedupeCalls(calls)
}

// incompleteToolCall is a <tool> opening that never reached its closing tag.
type incompleteToolCall struct {
	start int
	name  string
	raw   string // the fragment after the opening tag
}

// findIncompleteToolCall locates the first <tool ...> opening in text that is
// not part of a complete scanned block or a hallucinated <tool_result> block.
func findIncompleteToolCall(text string, matches []parsedToolBlock) (incompleteToolCall, bool) {
	lower := strings.ToLower(text)
	openResult := toolOpenTag + "_result"
	resultSpans := resultBlock.FindAllStringIndex(text, -1)
	inSpans := func(i int) (int, bool) {
		for _, sp := range resultSpans {
			if i >= sp[0] && i < sp[1] {
				return sp[1], true
			}
		}
		return 0, false
	}
	for pos := 0; pos < len(text); {
		i := strings.Index(lower[pos:], toolOpenTag)
		if i < 0 {
			break
		}
		i += pos
		if strings.HasPrefix(lower[i:], openResult) {
			if end, ok := inSpans(i); ok {
				pos = end
			} else {
				pos = i + len(toolOpenTag)
			}
			continue
		}
		inside := false
		for _, mb := range matches {
			if i >= mb.start && i < mb.end {
				inside = true
				pos = mb.end
				break
			}
		}
		if inside {
			continue
		}
		if end, ok := inSpans(i); ok {
			pos = end
			continue
		}
		// Not a complete block: the call was cut off. Grab whatever name and
		// body fragment exist after the tag.
		name := ""
		gtRel := strings.IndexByte(text[i:], '>')
		if gtRel >= 0 {
			name, _ = parseToolStartName(text[i : i+gtRel+1])
			return incompleteToolCall{start: i, name: name, raw: text[i+gtRel+1:]}, true
		}
		if m := toolStartName.FindStringSubmatch(text[i:]); len(m) == 2 {
			name = m[1]
		}
		return incompleteToolCall{start: i, name: name}, true
	}
	return incompleteToolCall{}, false
}

func scanToolBlocks(text string) []parsedToolBlock {
	var out []parsedToolBlock
	lower := strings.ToLower(text)
	openResult := toolOpenTag + "_result"
	for pos := 0; pos < len(text); {
		i := strings.Index(lower[pos:], toolOpenTag)
		if i < 0 {
			break
		}
		i += pos
		if strings.HasPrefix(lower[i:], openResult) {
			pos = i + len(toolOpenTag)
			continue
		}
		gtRel := strings.IndexByte(text[i:], '>')
		if gtRel < 0 {
			break
		}
		gt := i + gtRel
		name, ok := parseToolStartName(text[i : gt+1])
		if !ok {
			pos = gt + 1
			continue
		}
		bodyStart := gt + 1
		bodyEnd, tagEnd := findToolBodyEnd(text, bodyStart)
		if tagEnd < 0 {
			pos = bodyStart
			continue
		}
		out = append(out, parsedToolBlock{start: i, end: tagEnd, name: name, raw: text[bodyStart:bodyEnd]})
		pos = tagEnd
	}
	return out
}

func parseToolStartName(tag string) (string, bool) {
	m := toolStartName.FindStringSubmatch(tag)
	if len(m) != 2 {
		return "", false
	}
	return m[1], true
}

func findToolBodyEnd(text string, start int) (int, int) {
	if _, objEnd, ok := extractJSONObjectSpan(text[start:]); ok {
		if closeStart, closeEnd := findCloseToolTag(text, start+objEnd); closeEnd >= 0 {
			return closeStart, closeEnd
		}
	}
	return findCloseToolTag(text, start)
}

func findCloseToolTag(text string, pos int) (int, int) {
	lower := strings.ToLower(text)
	for pos < len(text) {
		i := strings.Index(lower[pos:], toolCloseTag)
		if i < 0 {
			return -1, -1
		}
		i += pos
		gtRel := strings.IndexByte(text[i:], '>')
		if gtRel < 0 {
			return -1, -1
		}
		return i, i + gtRel + 1
	}
	return -1, -1
}

// parseArgs decodes a tool-call argument object, tolerating the malformed JSON
// weaker models emit: extra trailing braces, code fences, trailing junk, raw
// newlines/tabs inside string values, and invalid backslash escapes (e.g. a
// regex written as \. instead of \\.), all of which strict JSON forbids. It
// tries the raw text plus repaired variants, each both strictly and via
// first-balanced-object extraction. On failure it returns a short description
// of the JSON error so the retry feedback can say exactly what was wrong.
func parseArgs(raw string, out *map[string]any) (string, bool) {
	var firstErr string
	for _, cand := range []string{raw, repairControlChars(raw), repairInvalidEscapes(raw), repairInvalidEscapes(repairControlChars(raw))} {
		m := map[string]any{}
		if err := json.Unmarshal([]byte(cand), &m); err == nil {
			*out = m
			return "", true
		} else if firstErr == "" {
			firstErr = err.Error()
		}
		if obj, ok := extractJSONObject(cand); ok {
			m2 := map[string]any{}
			if err := json.Unmarshal([]byte(obj), &m2); err == nil {
				*out = m2
				return "", true
			} else if firstErr == "" {
				firstErr = err.Error()
			}
		}
	}
	return firstErr, false
}

// repairInvalidEscapes doubles backslashes that start an invalid JSON escape
// inside a string literal. Models writing regexes often emit \. \s \( etc.,
// which JSON rejects; doubling the backslash preserves the intended text.
func repairInvalidEscapes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inStr {
			if c == '"' {
				inStr = true
			}
			b.WriteByte(c)
			continue
		}
		if c == '\\' && i+1 < len(s) {
			n := s[i+1]
			valid := strings.IndexByte(`"\/bfnrt`, n) >= 0 || (n == 'u' && i+5 < len(s) && isHex4(s[i+2:i+6]))
			if valid {
				b.WriteByte(c)
				b.WriteByte(n)
				i++
				continue
			}
			b.WriteString(`\\`)
			continue
		}
		if c == '"' {
			inStr = false
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isHex4(s string) bool {
	if len(s) < 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// repairControlChars escapes literal newlines/tabs/returns that appear inside
// JSON string literals, which some models emit instead of \n / \t.
func repairControlChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inStr {
			if c == '"' {
				inStr = true
			}
			b.WriteByte(c)
			continue
		}
		switch {
		case esc:
			esc = false
			b.WriteByte(c)
		case c == '\\':
			esc = true
			b.WriteByte(c)
		case c == '"':
			inStr = false
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// extractJSONObject returns the first balanced {…} object in s, honouring
// string literals and escapes so braces inside strings do not confuse it.
func extractJSONObject(s string) (string, bool) {
	start, end, ok := extractJSONObjectSpan(s)
	if !ok {
		return "", false
	}
	return s[start:end], true
}

// extractJSONObjectSpan returns the byte span of the first balanced {…} object.
func extractJSONObjectSpan(s string) (int, int, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return 0, 0, false
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return start, i + 1, true
			}
		}
	}
	return 0, 0, false
}

// dedupeCalls drops exact-duplicate calls (same tool + same args) that the
// model occasionally emits twice in one turn, which would otherwise run the
// same command or write the same file twice.
func dedupeCalls(calls []ToolCall) []ToolCall {
	seen := map[string]bool{}
	out := calls[:0]
	for _, c := range calls {
		key := c.Name + "\x00" + fmt.Sprint(c.Args)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

// stripFences removes a wrapping ```lang ... ``` if the model added one.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

// buildToolResults formats executed tool outputs into a single user turn that
// is appended to the conversation and sent back to the model.
func buildToolResults(results []toolResult) string {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "<tool_result name=%q>\n%s\n</tool_result>\n", r.Name, r.Output)
	}
	return strings.TrimRight(b.String(), "\n")
}

// systemPrompt is the operating manual handed to the model each turn.
func systemPrompt(workdir string) string {
	return systemPromptMode(workdir, false, false, false, false)
}

// systemPromptMode builds the system prompt, appending the computer-use
// section when cowork mode is active, the plan-mode section when plan mode is
// on, the goal-mode section for long autonomous tasks, and the task
// (sub-agent) tool when subagents are available (extended thinking level).
// Sub-agents get those flags false, so they can't spawn sub-agents of their own.
func systemPromptMode(workdir string, cowork, plan, goal, subagents bool) string {
	return systemPromptModeWithTools(workdir, cowork, plan, goal, subagents, nil)
}

func systemPromptModeWithTools(workdir string, cowork, plan, goal, subagents bool, tools []CustomTool) string {
	base := strings.ReplaceAll(loadSystemTemplate(), "{{CWD}}", workdir) +
		"\nHost OS: " + runtime.GOOS + "."
	if prompt := customToolsPrompt(tools); prompt != "" {
		base += "\n\n" + prompt
	}
	if subagents {
		base += "\n\n" + taskTemplate
	}
	if plan {
		base += "\n\n" + planTemplate
	}
	if goal {
		base += "\n\n" + goalTemplate
	}
	if !cowork {
		return base
	}
	home, _ := os.UserHomeDir()
	return base + "\n\n" + strings.ReplaceAll(coworkTemplate, "{{HOME}}", home)
}

const planTemplate = `# Plan mode
Plan mode is ON. Your job now is to EXPLORE and PLAN, not to execute.

- Use only the read-only tools: open, list_dir, search, web_search. Do NOT call write / edit_file /
  delete / rename / run / import_github / task or any screen tool — the CLI will refuse them.
- Investigate the codebase until you understand what needs to change, then reply with a
  concrete step-by-step plan: real files, functions, and commands, in the order to do them.
- Do NOT call finish with the plan; present it as a normal message and stop.
- End by telling the user to run /plan again to approve the plan and start executing.`

const goalTemplate = `# Goal mode
Goal mode is ON. Work autonomously until the user's objective is complete or you are genuinely blocked.

- Long tasks are expected. Keep going across tool results, background-command notifications,
  failed attempts, retries, and verification loops instead of stopping early.
- For servers, watchers, polling loops, downloads, scheduled waits, and other long-running
  commands, prefer run with {"background": true, "log": "..."}. Then inspect the log or
  wait for the CLI's background-completion result before deciding what to do next.
- Use the available tools proactively: read files, run tests, curl or query web endpoints,
  delegate independent chunks with task, and check logs/artifacts until you have evidence.
- If a tool fails, read the exact tool result, correct the command or JSON syntax, and retry.
  Do not claim success for failed or unverified work.
- Do not finish just because work has started in the background. Use finish only after the
  goal is complete and verified, or when blocked with a clear reason and next step.`

const taskTemplate = `# Sub-agents — the task tool
- task — delegate self-contained sub-tasks to sub-agents, each with its own agent loop and full
  tool access (aliases: agent, subagent). Args:
    {"prompt": string, "description"?: string} — a single sub-task, or
    {"tasks": [{"prompt": string, "description"?: string}, …], "background"?: bool} — several
    sub-agents running concurrently (a plain ["…", "…"] "prompts" array works too).
  By default the call blocks until every sub-agent finishes and all reports come back as one
  tool result. With "background": true it returns immediately and the CLI hands you the
  combined report as a later message when all of them are done — keep helping the user in the
  meantime, and don't claim their results before that report arrives. The user sees a compact
  live grid with each sub-agent's progress while they run.
  Use it for independent chunks of work that need many steps; each sub-agent runs with extended
  thinking and its final report comes back as the tool result. Do not delegate work you can
  do yourself in a step or two, and make every prompt fully self-contained — the sub-agents
  can't see this conversation.`

const coworkTemplate = `# Cowork mode — computer use
Cowork mode is ON. You are no longer confined to the working directory: you may
navigate the ENTIRE filesystem (the user's home is {{HOME}}). Use absolute paths
with open / write / edit_file / list_dir / search / run to work anywhere on this
computer.

You can also SEE and CONTROL the screen, like a coworker sitting at the machine.
Additional tools:

- screenshot — capture the screen. Args: {}. If you can see images, the capture comes back with the
  result; if you can't, a vision model describes it for you as text (including the pixel coordinates of
  clickable elements), so either way you know what is on screen.
- click — click at screen coordinates. Args: {"x": int, "y": int, "button"?: "left"|"right"|"middle", "double"?: bool}.
- move_mouse — move the pointer. Args: {"x": int, "y": int}.
- scroll — scroll at the pointer (optionally after moving it). Args: {"dy": int, "dx"?: int, "x"?: int, "y"?: int}. dy > 0 scrolls down.
- type_text — type text into whatever window has focus. Args: {"text": string}.
- key_press — press a key, optionally with modifiers. Args: {"key": string, "modifiers"?: [string]}.
  key is a name like "enter", "tab", "escape", "space", "backspace", "delete", "up"/"down"/"left"/"right",
  "home", "end", "pageup", "pagedown", "f1"–"f12", or a single character; modifiers are "cmd", "ctrl", "alt", "shift".
- open_app — open an application, file, folder, or URL. Args: {"target": string}.

How to drive the screen (observe → act → observe):
1. Call screenshot to see the current screen.
2. Decide ONE action, perform it, then screenshot again to check the result.
3. Coordinates: each screenshot result reports the screen's size in click units AND the
   delivered image's pixel size, with the ratio between them. Multiply a pixel position
   you see in the image by that ratio to get click/move/scroll coordinates.
4. GUI actions are slower and riskier than files and shells: prefer the normal
   file/command tools whenever they can do the job, and use the screen only for
   what truly needs a GUI. Ask the user before visibly destructive actions.`

//go:embed system_prompt.txt
var embeddedSystemTemplate string

func loadSystemTemplate() string {
	if path := strings.TrimSpace(os.Getenv("NOCTURNE_SYSTEM_PROMPT_FILE")); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimRight(string(b), "\n")
		}
	}
	return strings.TrimRight(embeddedSystemTemplate, "\n")
}
