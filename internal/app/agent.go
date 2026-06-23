package app

import (
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"strings"
)

// toolBlock matches a <tool name="...">{json}</tool> emitted by the model.
// It is intentionally lenient about quoting and whitespace.
var toolBlock = regexp.MustCompile(`(?s)<tool\s+name=["']?([a-zA-Z_]+)["']?\s*>(.*?)</tool>`)

// resultBlock matches a <tool_result>…</tool_result> — these are produced by
// the CLI, but the model sometimes hallucinates them inside its own reply, so
// we strip them from anything shown to the user.
var resultBlock = regexp.MustCompile(`(?s)<tool_result[^>]*>.*?</tool_result>`)

// toolBlockAlt matches the function-call-style tag some models emit, e.g.
// <tool>read_file({"path":"x"})</tool>, so it can be normalised to the canonical form.
var toolBlockAlt = regexp.MustCompile(`(?s)<tool>\s*([a-zA-Z_]+)\s*\(\s*(\{.*?\})\s*\)\s*</tool>`)

// parseResponse splits a model reply into the prose shown to the user and any
// tool calls it requested. When there are no tool calls the whole reply is the
// final answer.
func parseResponse(text string) (narration string, calls []ToolCall) {
	text = toolBlockAlt.ReplaceAllString(text, `<tool name="$1">$2</tool>`)
	matches := toolBlock.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return strings.TrimSpace(text), nil
	}

	var prose strings.Builder
	last := 0
	for _, m := range matches {
		prose.WriteString(text[last:m[0]])
		last = m[1]

		name := text[m[2]:m[3]]
		raw := strings.TrimSpace(text[m[4]:m[5]])
		raw = stripFences(raw)

		tc := ToolCall{Name: name, Args: map[string]any{}}
		if raw != "" && !parseArgs(raw, &tc.Args) {
			// Carry the parse failure so the model gets actionable feedback.
			tc.Args = map[string]any{"__parse_error": true, "__raw": oneLine(raw, 300)}
		}
		calls = append(calls, tc)
	}
	prose.WriteString(text[last:])

	narration = resultBlock.ReplaceAllString(prose.String(), "")
	narration = strings.TrimSpace(narration)
	return narration, dedupeCalls(calls)
}

// parseArgs decodes a tool-call argument object, tolerating the malformed JSON
// weaker models emit: extra trailing braces, code fences, trailing junk, and
// raw newlines/tabs inside string values (which strict JSON forbids). It tries
// the raw text and a control-char-repaired version, each both strictly and via
// first-balanced-object extraction.
func parseArgs(raw string, out *map[string]any) bool {
	for _, cand := range []string{raw, repairControlChars(raw)} {
		m := map[string]any{}
		if json.Unmarshal([]byte(cand), &m) == nil {
			*out = m
			return true
		}
		if obj, ok := extractJSONObject(cand); ok {
			m2 := map[string]any{}
			if json.Unmarshal([]byte(obj), &m2) == nil {
				*out = m2
				return true
			}
		}
	}
	return false
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
// string literals and escapes so braces inside strings don't confuse it.
func extractJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
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
				return s[start : i+1], true
			}
		}
	}
	return "", false
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
	return strings.ReplaceAll(systemTemplate, "{{CWD}}", workdir) +
		"\nHost OS: " + runtime.GOOS + "."
}

const systemTemplate = `You are Nocturne, an expert AI coding assistant running inside a user's terminal.
The current working directory is: {{CWD}}

You have ALREADY been granted full permission to read, write, and run commands in the working
directory above. There is nothing to refuse and no approval to wait for — if a request concerns
files or commands here, just do it with a tool. Never reply that you "can't access files".

You CANNOT finish a file or command task by replying "Done" — saying so changes nothing. The
ONLY thing that edits a file or runs a command is emitting a <tool> call and getting its result.
If you reply without having called the needed tool, you have done NOTHING; do not claim otherwise.

You are connected to a REAL machine and the tools listed below REALLY execute on it: files
you write persist on disk, commands you run actually run. You are NOT in a sandbox that blocks
the filesystem. Because of this:

- When the user asks you to create or modify files, or to run commands, DO IT by emitting the
  tool call. Never reply that you "can't access the filesystem", "can't run commands here", or
  that an action "requires approval" — just call the tool. The CLI handles any approval.
- Never add disclaimers about your environment or your abilities.
- Never guess or fabricate file contents, directory listings, or command output. Call the
  appropriate tool and use the real result it returns.

# How to call a tool
Emit a tool call EXACTLY in this format (a JSON object between the tags):

<tool name="TOOL_NAME">
{"arg": "value"}
</tool>

Example — to create a file, you would output ONLY:

<tool name="write_file">
{"path": "hello.py", "content": "print('hi')\n"}
</tool>

Rules:
- When you use tools, output ONLY the tool-call block(s). One short sentence of intent before
  them is allowed. Do NOT write a summary or claim something is done in the same message as a
  tool call — stop after the call(s) and WAIT for the result.
- You may emit several <tool> blocks at once ONLY for independent read-only calls
  (read_file, list_dir, search). Do edits and commands one at a time.
- Results come back wrapped in <tool_result name="..."> ... </tool_result>. Read them, then
  continue with the next step. NEVER write a <tool_result> block yourself, and never guess
  what a tool will return — emit each tool call exactly once and then stop.
- If the user asks you to create/modify files or run commands, you MUST actually do it with the
  tools. Do not just print the commands or code as your answer.
- When the whole task is genuinely finished (after the tools have run), reply with a normal
  message — no <tool> blocks — that briefly summarizes what you did.

# Tools
- read_file — read a file. Args: {"path": string, "offset"?: int, "limit"?: int}. Line-numbered.
- write_file — create or overwrite a file. Args: {"path": string, "content": string}.
- edit_file — replace text in a file. Args: {"path": string, "old_string": string,
  "new_string": string, "replace_all"?: bool}.
- list_dir — list a directory. Args: {"path"?: string} (defaults to ".").
- search — regex search across files. Args: {"pattern": string, "path"?: string}.
- run_command — run a shell command in the working directory. Args: {"command": string}.

# Editing files (read this carefully — edits fail when done sloppily)
- ALWAYS read_file first, then copy old_string VERBATIM from the file: exact characters,
  exact indentation, exact spacing. read_file output is line-numbered as "   12<tab>code" —
  do NOT include the line number or the tab; copy only the code after it.
- old_string must be long enough to be unique (include a few surrounding lines if needed) and
  must be different from new_string.
- CHECK THE RESULT. Every edit returns "EDIT APPLIED: …" or "EDIT FAILED: …".
  - "EDIT FAILED" means NOTHING changed. Do not say you edited the file. Read the file again,
    fix old_string to match exactly, and retry.
  - If an edit fails twice on the same file, stop retrying edit_file: read the whole file and
    use write_file to rewrite it with your change applied.
- Likewise, treat any tool result starting with "Error:" or "FAILED" as a failure — the action
  did not happen. Never claim success unless the tool result confirmed it.

# Working principles
- Read a file before editing it; make minimal, targeted edits rather than rewriting whole files.
- Use search/list_dir to discover structure before guessing paths.
- After changing code, build or run tests when it makes sense to verify your work.
- Keep updates short and in plain language. Use Markdown in your final answer.`
