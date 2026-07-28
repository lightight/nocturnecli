package app

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// ToolCall is a parsed request from the model to run one tool.
type ToolCall struct {
	Name string
	Args map[string]any
}

// toolResult pairs a call with its textual output for feeding back. Image is
// set only by screenshot: it is attached to the tool-results message so a
// vision model can see the screen.
type toolResult struct {
	Name   string
	Output string
	Image  *Image
}

// backgroundCommandResult is emitted when a command started with
// {"background": true} exits. The TUI listens for these and surfaces them as
// asynchronous notifications, so long-running commands are no longer fire-and-
// forget from the user's perspective.
type backgroundCommandResult struct {
	PID      int
	Command  string
	LogPath  string
	Started  time.Time
	Finished time.Time
	Err      error
}

var backgroundCommandResults = make(chan backgroundCommandResult, 64)

func publishBackgroundCommandResult(r backgroundCommandResult) {
	select {
	case backgroundCommandResults <- r:
	default:
		// Avoid blocking a reaper goroutine if no UI is listening or the buffer is
		// full. The command has already exited and been waited on; dropping the
		// notification is better than leaking a goroutine.
	}
}

const (
	maxToolOutput = 20000 // chars fed back to the model per tool
	cmdTimeout    = 120 * time.Second
)

// canonicalTool maps a tool name — including the friendly names and aliases
// the model is offered — to the canonical implementation it dispatches to.
// Display code keeps the name the model actually emitted; only behaviour is
// routed through this.
func canonicalTool(name string) string {
	switch name {
	case "write", "create":
		return "write_file"
	case "open":
		return "read_file"
	case "run":
		return "run_command"
	case "github", "import", "clone":
		return "import_github"
	case "install", "skill", "skill_install", "install_tool":
		return "install_skill"
	case "agent", "subagent", "spawn":
		return "task"
	}
	return name
}

// needsApproval reports whether a tool mutates state / runs code and must be
// confirmed by the user (unless auto-accept is on).
func needsApproval(name string) bool {
	switch canonicalTool(name) {
	case "write_file", "edit_file", "run_command", "delete", "rename", "import_github", "install_skill",
		"click", "move_mouse", "scroll", "type_text", "key_press", "open_app", "task":
		return true
	}
	return false
}

// diagnoseBadToolCall catches malformed tool calls before approval/execution and
// turns them into a tool result. Feeding that result back into the agent loop
// makes the model immediately retry the call, with a concrete explanation of
// what was wrong, while guaranteeing nothing was run for the bad call.
// diagnoseMalformedCall handles calls that never produced usable arguments:
// cut-off (truncated) blocks and invalid JSON. Shared by diagnoseBadToolCall
// and the plain-execute fallback so every path words the failure the same way.
func diagnoseMalformedCall(tc ToolCall) (string, bool) {
	if _, bad := tc.Args["__truncated"]; bad {
		raw, _ := tc.Args["__raw"].(string)
		fix := "Re-send the same call complete, ending with its closing tag. If you were writing a large file or long content, " +
			"split it: write the first part, then append the rest with one or more edit_file calls, so the reply is not cut off again."
		if raw != "" {
			fix += " The fragment you sent began: " + raw
		}
		return retryToolCallMessage(tc.Name, "the tool call was cut off before its closing tag — the reply ended mid-call, usually because it was too long", fix), true
	}
	if _, bad := tc.Args["__parse_error"]; bad {
		raw, _ := tc.Args["__raw"].(string)
		problem := fmt.Sprintf("the arguments for %q were not valid JSON", tc.Name)
		if detail, _ := tc.Args["__err"].(string); detail != "" {
			problem += " (JSON error: " + detail + ")"
		}
		return retryToolCallMessage(tc.Name, problem,
			"Re-send the same tool call with exactly one valid JSON object between the tags: no markdown fences, extra braces, comments, or trailing text. Bad arguments seen: "+raw), true
	}
	return "", false
}

func diagnoseBadToolCall(tc ToolCall, tools []CustomTool) (string, bool) {
	if out, bad := diagnoseMalformedCall(tc); bad {
		return out, true
	}

	if _, ok := findCustomTool(tools, tc.Name); ok {
		return "", false
	}
	canon := canonicalTool(tc.Name)
	if !builtinToolNames[tc.Name] && !builtinToolNames[canon] {
		return retryToolCallMessage(tc.Name, fmt.Sprintf("%q is not a known tool", tc.Name),
			"Use one of the advertised tool names exactly. Known tools: "+knownToolNames(tools)+"."), true
	}

	switch canon {
	case "read_file":
		if missingStringArg(tc.Args, "path") {
			return retryToolCallMessage(tc.Name, "missing required string argument 'path'", `Call it as {"path":"file.ext"}.`), true
		}
	case "write_file":
		if missingStringArg(tc.Args, "path") {
			return retryToolCallMessage(tc.Name, "missing required string argument 'path'", `Call it as {"path":"file.ext","content":"..."}.`), true
		}
	case "edit_file":
		if missingStringArg(tc.Args, "path") || missingStringArg(tc.Args, "old_string") {
			return retryToolCallMessage(tc.Name, "edit_file requires non-empty 'path' and 'old_string' arguments", `Open the file first, then call edit_file with verbatim old_string and the desired new_string.`), true
		}
	case "search":
		if missingStringArg(tc.Args, "pattern") {
			return retryToolCallMessage(tc.Name, "missing required string argument 'pattern'", `Call it as {"pattern":"regex","path":"optional/dir"}.`), true
		}
	case "web_search":
		if missingStringArg(tc.Args, "query") {
			return retryToolCallMessage(tc.Name, "missing required string argument 'query'", `Call it as {"query":"search terms"}.`), true
		}
	case "run_command":
		if missingStringArg(tc.Args, "command") {
			return retryToolCallMessage(tc.Name, "missing required string argument 'command'", `Call it as {"command":"shell command"}.`), true
		}
	case "delete":
		if missingStringArg(tc.Args, "path") {
			return retryToolCallMessage(tc.Name, "missing required string argument 'path'", `Call it as {"path":"file.ext"}.`), true
		}
	case "rename":
		if missingStringArg(tc.Args, "from") || missingStringArg(tc.Args, "to") {
			return retryToolCallMessage(tc.Name, "rename requires non-empty 'from' and 'to' arguments", `Call it as {"from":"old/path","to":"new/path"}.`), true
		}
	case "import_github":
		if strings.TrimSpace(repoArg(tc.Args)) == "" {
			return retryToolCallMessage(tc.Name, "missing required 'repo' argument", `Call it as {"repo":"owner/name"}.`), true
		}
	case "ask":
		if missingStringArg(tc.Args, "question") {
			return retryToolCallMessage(tc.Name, "missing required string argument 'question'", `Call it as {"question":"...","options":["..."]}.`), true
		}
	case "task":
		if missingStringArg(tc.Args, "prompt") {
			return retryToolCallMessage(tc.Name, "missing required string argument 'prompt'", `Call it as {"prompt":"self-contained task","description":"short label"}.`), true
		}
	case "type_text":
		if missingStringArg(tc.Args, "text") {
			return retryToolCallMessage(tc.Name, "missing required string argument 'text'", `Call it as {"text":"text to type"}.`), true
		}
	case "key_press":
		if missingStringArg(tc.Args, "key") {
			return retryToolCallMessage(tc.Name, "missing required string argument 'key'", `Call it as {"key":"enter"}.`), true
		}
	case "open_app":
		if missingStringArg(tc.Args, "target") {
			return retryToolCallMessage(tc.Name, "missing required string argument 'target'", `Call it as {"target":"/path/or/App/or/URL"}.`), true
		}
	}

	return "", false
}

func retryToolCallMessage(name, problem, fix string) string {
	return "BAD TOOL CALL: nothing was run for " + name + ". No filesystem, shell, or GUI action happened. " +
		"Where the bot went wrong: " + problem + ". " +
		"Automatically restarting the tool step: re-emit the corrected <tool name=\"" + name + "\"> call now and then wait for its result. " + fix
}

// maxBadCallRepeats bounds how many times the agent loops feed back the SAME
// malformed call before stopping. A model that keeps re-emitting an identical
// broken call would otherwise burn every remaining round (each one a paid API
// call) retrying something that cannot succeed.
const maxBadCallRepeats = 3

// badCallBreaker tracks consecutive identical diagnosed-bad tool calls.
type badCallBreaker struct {
	key string
	n   int
}

// add records one diagnosed-bad call and reports whether the loop should stop
// instead of feeding it back yet again.
func (b *badCallBreaker) add(tc ToolCall) bool {
	key := tc.Name + "\x00" + fmt.Sprint(tc.Args)
	if key == b.key {
		b.n++
	} else {
		b.key, b.n = key, 1
	}
	return b.n >= maxBadCallRepeats
}

// reset clears the streak — call it whenever a well-formed call runs.
func (b *badCallBreaker) reset() { b.key, b.n = "", 0 }

// maxEmptyNudges bounds how many times the agent loops poke the model after it
// replies with no tool call and no message (pure whitespace, or only a
// hallucinated <tool_result> block) before giving up.
const maxEmptyNudges = 2

// emptyReply reports whether a parsed reply carries nothing at all: no tool
// calls and no user-visible narration. Models occasionally derail into streams
// of whitespace or hallucinated tool results; ending the turn there looks like
// the task finished when nothing happened.
func emptyReply(narration string, calls []ToolCall) bool {
	return len(calls) == 0 && strings.TrimSpace(narration) == ""
}

// emptyReplyNudge is fed back as a user message so the model continues instead
// of the turn silently ending on an empty reply.
const emptyReplyNudge = "Your previous reply contained no tool call and no message — only whitespace or a stray tool-result block — so nothing happened. " +
	"Continue the task now: emit the next <tool> call and wait for its result, or if the task is genuinely complete, reply with a short summary."

func missingStringArg(args map[string]any, key string) bool {
	return strings.TrimSpace(argStr(args, key)) == ""
}

func knownToolNames(tools []CustomTool) string {
	names := make([]string, 0, len(builtinToolNames)+len(tools))
	for name := range builtinToolNames {
		names = append(names, name)
	}
	for _, t := range tools {
		if strings.TrimSpace(t.Name) != "" {
			names = append(names, t.Name)
		}
	}
	sort.Strings(names)
	if len(names) > 40 {
		names = append(names[:40], "…")
	}
	return strings.Join(names, ", ")
}

// argStr / argBool / argInt safely pull typed values out of the decoded JSON.
func argStr(a map[string]any, k string) string {
	if v, ok := a[k].(string); ok {
		return v
	}
	return ""
}

func argInt(a map[string]any, k string) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func argBool(a map[string]any, k string) bool {
	v, _ := a[k].(bool)
	return v
}

// summarize renders a one-line label for a tool call, e.g. read_file(main.go).
// It keeps the name the model emitted (so aliases like open/run/create show as
// typed) while routing the argument layout through the canonical name.
func (tc ToolCall) summarize() string {
	switch canonicalTool(tc.Name) {
	case "read_file", "write_file":
		return fmt.Sprintf("%s(%s)", tc.Name, argStr(tc.Args, "path"))
	case "edit_file":
		return fmt.Sprintf("edit_file(%s)", argStr(tc.Args, "path"))
	case "list_dir":
		p := argStr(tc.Args, "path")
		if p == "" {
			p = "."
		}
		return fmt.Sprintf("list_dir(%s)", p)
	case "search":
		return fmt.Sprintf("search(%q)", argStr(tc.Args, "pattern"))
	case "web_search":
		return fmt.Sprintf("web_search(%q)", argStr(tc.Args, "query"))
	case "run_command":
		return fmt.Sprintf("%s: %s", tc.Name, oneLine(argStr(tc.Args, "command"), 80))
	case "delete":
		return fmt.Sprintf("delete(%s)", argStr(tc.Args, "path"))
	case "rename":
		return fmt.Sprintf("rename(%s → %s)", argStr(tc.Args, "from"), argStr(tc.Args, "to"))
	case "import_github":
		return fmt.Sprintf("import_github(%s)", repoArg(tc.Args))
	case "install_skill":
		return fmt.Sprintf("install_skill(%s)", skillURLArg(tc.Args))
	case "ask":
		return "ask: " + oneLine(argStr(tc.Args, "question"), 70)
	case "cowork":
		return "cowork: enable computer use"
	case "task":
		label := argStr(tc.Args, "description")
		if label == "" {
			label = argStr(tc.Args, "prompt")
		}
		return "task: " + oneLine(label, 70)
	case "screenshot":
		return "screenshot()"
	case "click":
		s := fmt.Sprintf("click(%d, %d)", argInt(tc.Args, "x"), argInt(tc.Args, "y"))
		if b := argStr(tc.Args, "button"); b != "" && b != "left" {
			s = b + " " + s
		}
		if argBool(tc.Args, "double") {
			s = "double " + s
		}
		return s
	case "move_mouse":
		return fmt.Sprintf("move_mouse(%d, %d)", argInt(tc.Args, "x"), argInt(tc.Args, "y"))
	case "scroll":
		return fmt.Sprintf("scroll(dy=%d)", argInt(tc.Args, "dy"))
	case "type_text":
		return fmt.Sprintf("type_text(%q)", oneLine(argStr(tc.Args, "text"), 40))
	case "key_press":
		key := argStr(tc.Args, "key")
		if mods := toStrings(tc.Args["modifiers"]); len(mods) > 0 {
			key = strings.Join(mods, "+") + "+" + key
		}
		return "key_press(" + key + ")"
	case "open_app":
		return "open_app(" + argStr(tc.Args, "target") + ")"
	case "finish":
		return "finish"
	default:
		return tc.Name
	}
}

// details returns a multi-line preview shown in the confirmation prompt.
func (tc ToolCall) details(workdir string) string {
	switch canonicalTool(tc.Name) {
	case "run_command":
		return "$ " + argStr(tc.Args, "command")
	case "write_file":
		content := argStr(tc.Args, "content")
		return fmt.Sprintf("write %d bytes to %s", len(content), argStr(tc.Args, "path"))
	case "edit_file":
		return fmt.Sprintf("in %s:\n- %s\n+ %s",
			argStr(tc.Args, "path"),
			oneLine(argStr(tc.Args, "old_string"), 160),
			oneLine(argStr(tc.Args, "new_string"), 160))
	case "delete":
		return "remove " + argStr(tc.Args, "path")
	case "rename":
		return fmt.Sprintf("%s → %s", argStr(tc.Args, "from"), argStr(tc.Args, "to"))
	case "import_github":
		return "git clone " + normalizeRepoURL(repoArg(tc.Args)) + " (public repo)"
	case "install_skill":
		return "install skill/tool from " + skillURLArg(tc.Args)
	case "cowork":
		if t := argStr(tc.Args, "task"); t != "" {
			return "enable computer use for: " + t
		}
		return "enable computer use (see & control the screen, whole filesystem)"
	case "task":
		return "run a sub-agent with full tool access on:\n" + oneLine(argStr(tc.Args, "prompt"), 300)
	case "click":
		return fmt.Sprintf("%s at (%d, %d)", firstNonEmpty(argStr(tc.Args, "button"), "left")+" click",
			argInt(tc.Args, "x"), argInt(tc.Args, "y"))
	case "move_mouse":
		return fmt.Sprintf("move pointer to (%d, %d)", argInt(tc.Args, "x"), argInt(tc.Args, "y"))
	case "scroll":
		return fmt.Sprintf("scroll dy=%d dx=%d", argInt(tc.Args, "dy"), argInt(tc.Args, "dx"))
	case "type_text":
		return "type: " + oneLine(argStr(tc.Args, "text"), 160)
	case "key_press":
		key := argStr(tc.Args, "key")
		if mods := toStrings(tc.Args["modifiers"]); len(mods) > 0 {
			key = strings.Join(mods, "+") + "+" + key
		}
		return "press " + key
	case "open_app":
		return "open " + argStr(tc.Args, "target")
	}
	return ""
}

// execute dispatches a tool call and returns its textual result. Errors are
// returned as text (prefixed "Error:") rather than Go errors so the model can
// read and recover from them.
func execute(tc ToolCall, workdir string) string {
	if out, bad := diagnoseMalformedCall(tc); bad {
		return out
	}
	switch canonicalTool(tc.Name) {
	case "read_file":
		return readFileTool(workdir, tc.Args)
	case "write_file":
		return writeFileTool(workdir, tc.Args)
	case "edit_file":
		return editFileTool(workdir, tc.Args)
	case "list_dir":
		return listDirTool(workdir, tc.Args)
	case "search":
		return searchTool(workdir, tc.Args)
	case "web_search":
		return webSearchTool(tc.Args)
	case "run_command":
		return runCommandTool(workdir, tc.Args)
	case "delete":
		return deleteFileTool(workdir, tc.Args)
	case "rename":
		return renameFileTool(workdir, tc.Args)
	case "import_github":
		return importGithubTool(workdir, tc.Args)
	case "install_skill":
		return "the install_skill tool is handled by the agent loop and can't run here."
	case "screenshot":
		out, _ := screenshotTool()
		return out
	case "click":
		return clickTool(tc.Args)
	case "move_mouse":
		return moveMouseTool(tc.Args)
	case "scroll":
		return scrollTool(tc.Args)
	case "type_text":
		return typeTextTool(tc.Args)
	case "key_press":
		return keyPressTool(tc.Args)
	case "open_app":
		return openAppTool(workdir, tc.Args)
	case "cowork":
		// Enabling cowork is handled by the interactive loop / headless driver,
		// which flip the mode themselves; this is the plain-execute fallback.
		return "cowork mode enabled — you can now see and control the screen and use the whole filesystem."
	case "ask":
		// ask is normally intercepted by the interactive loop; this is the
		// non-interactive fallback so headless runs don't stall.
		return "ask is unavailable in this mode. Proceed with your best judgment and state any assumption you made."
	case "task":
		// task is normally intercepted by the interactive loop / headless driver,
		// which run the sub-agent themselves; this is the plain-execute fallback.
		return "the task tool is handled by the agent loop and can't run here."
	case "finish":
		if s := strings.TrimSpace(argStr(tc.Args, "summary")); s != "" {
			return s
		}
		return "Task complete."
	default:
		return "Error: unknown tool " + tc.Name
	}
}

// executeWithImage runs a tool call like execute, but also returns the image
// a screenshot captured (nil for every other tool). When the active model
// can't see images (vision=false), the screenshot is instead handed to
// describe — a vision model that renders it as text, coordinates included —
// so a text-only model can still drive the screen. describe may be nil, in
// which case a non-vision model gets a refusal.
func executeWithImage(tc ToolCall, workdir string, vision bool, describe func(Image) (string, error)) (string, *Image) {
	return executeWithImageWithTools(tc, workdir, vision, describe, nil)
}

func executeWithImageWithTools(tc ToolCall, workdir string, vision bool, describe func(Image) (string, error), tools []CustomTool) (string, *Image) {
	if t, ok := findCustomTool(tools, tc.Name); ok {
		return executeCustomTool(workdir, t, tc.Args), nil
	}
	if canonicalTool(tc.Name) != "screenshot" {
		return execute(tc, workdir), nil
	}
	out, img := screenshotTool()
	if img == nil || vision {
		return out, img
	}
	if describe == nil {
		return "Error: the active model can't see screenshots — switch to a vision model (e.g. /model claude-haiku-4.5) and try again.", nil
	}
	desc, err := describe(*img)
	if err != nil {
		return "Error: screenshot captured but the vision model couldn't describe it: " + oneLine(err.Error(), 200) +
			". Switch to a vision model (e.g. /model claude-haiku-4.5) to see screenshots directly.", nil
	}
	return out + "\n\nThe active model can't see images, so a vision model described the screenshot:\n\n" + desc, nil
}

// resolve makes a possibly-relative path absolute against the workdir.
func resolve(workdir, p string) string {
	if p == "" {
		return workdir
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(workdir, p)
}

func clip(s string) string {
	if len(s) > maxToolOutput {
		return s[:maxToolOutput] + fmt.Sprintf("\n… [truncated, %d more chars]", len(s)-maxToolOutput)
	}
	return s
}

func readFileTool(workdir string, a map[string]any) string {
	path := argStr(a, "path")
	if path == "" {
		return "Error: open requires a 'path'"
	}
	data, err := os.ReadFile(resolve(workdir, path))
	if err != nil {
		return "Error: " + err.Error()
	}
	lines := strings.Split(string(data), "\n")
	offset := argInt(a, "offset")
	limit := argInt(a, "limit")
	start := 0
	if offset > 0 {
		start = offset - 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
	}
	if b.Len() == 0 {
		return "(empty file)"
	}
	return clip(b.String())
}

func writeFileTool(workdir string, a map[string]any) string {
	path := argStr(a, "path")
	if path == "" {
		return "Error: write requires a 'path'"
	}
	full := resolve(workdir, path)
	if dir := filepath.Dir(full); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	content := argStr(a, "content")
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), path)
}

func editFileTool(workdir string, a map[string]any) string {
	path := argStr(a, "path")
	oldStr := argStr(a, "old_string")
	newStr := argStr(a, "new_string")
	if path == "" || oldStr == "" {
		return "EDIT FAILED: edit_file requires 'path' and 'old_string'."
	}
	full := resolve(workdir, path)
	data, err := os.ReadFile(full)
	if err != nil {
		return "EDIT FAILED: " + err.Error()
	}
	content := string(data)
	replaceAll := argBool(a, "replace_all")

	if oldStr == newStr {
		return "EDIT FAILED: old_string and new_string are identical, so no change was made to " + path + "."
	}

	// Tier 1 — exact match.
	if cnt := strings.Count(content, oldStr); cnt > 0 {
		if cnt > 1 && !replaceAll {
			return fmt.Sprintf("EDIT FAILED: old_string occurs %d times in %s and is not unique — NO changes were made. Add surrounding context to make it unique, or set replace_all=true.", cnt, path)
		}
		out := strings.Replace(content, oldStr, newStr, 1)
		applied := 1
		if replaceAll {
			out = strings.ReplaceAll(content, oldStr, newStr)
			applied = cnt
		}
		if err := os.WriteFile(full, []byte(out), 0o644); err != nil {
			return "EDIT FAILED: " + err.Error()
		}
		return fmt.Sprintf("EDIT APPLIED: %s (%d replacement%s).", path, applied, plural(applied))
	}

	// Tier 2 — whitespace-tolerant, line-based match (handles trailing
	// whitespace, then indentation differences) so a near-miss still lands.
	for _, trimLead := range []bool{false, true} {
		if start, end, n := flexibleMatch(content, oldStr, trimLead); n == 1 {
			replacement := newStr
			if trimLead {
				replacement = reindent(content[start:end], oldStr, newStr)
			}
			out := content[:start] + replacement + content[end:]
			if err := os.WriteFile(full, []byte(out), 0o644); err != nil {
				return "EDIT FAILED: " + err.Error()
			}
			return fmt.Sprintf("EDIT APPLIED: %s (matched ignoring whitespace).", path)
		} else if n > 1 && !replaceAll {
			return fmt.Sprintf("EDIT FAILED: old_string matches %d places in %s (ignoring whitespace) — NO changes were made. Add more context, or use write.", n, path)
		}
	}

	return "EDIT FAILED: old_string was not found in " + path + ", so NO changes were made. " +
		"Re-read the file and copy the exact text (including indentation) into old_string, or use write to rewrite the file.\n" +
		closestHint(content, oldStr)
}

// flexibleMatch finds a unique run of lines in content equal to old after
// normalising whitespace (always trailing; leading too when trimLead). It
// returns the byte range of that run and how many matches were found.
func flexibleMatch(content, old string, trimLead bool) (start, end, count int) {
	clines := strings.Split(content, "\n")
	olines := strings.Split(strings.Trim(old, "\n"), "\n")
	if len(olines) == 0 {
		return 0, 0, 0
	}
	norm := func(s string) string {
		if trimLead {
			return strings.TrimSpace(s)
		}
		return strings.TrimRight(s, " \t\r")
	}
	on := make([]string, len(olines))
	for i, l := range olines {
		on[i] = norm(l)
	}

	var starts []int
	for i := 0; i+len(on) <= len(clines); i++ {
		ok := true
		for j := range on {
			if norm(clines[i+j]) != on[j] {
				ok = false
				break
			}
		}
		if ok {
			starts = append(starts, i)
		}
	}
	if len(starts) != 1 {
		return 0, 0, len(starts)
	}

	li := starts[0]
	for i := 0; i < li; i++ {
		start += len(clines[i]) + 1
	}
	end = start
	for j := 0; j < len(on); j++ {
		end += len(clines[li+j])
		if j < len(on)-1 {
			end++ // newline between matched lines
		}
	}
	return start, end, 1
}

// reindent shifts new_string's indentation to match the file region that was
// matched, so a leading-whitespace mismatch in old_string doesn't corrupt the file.
func reindent(region, old, new string) string {
	fileIndent := leadingWS(firstLine(region))
	oldIndent := leadingWS(firstLine(strings.Trim(old, "\n")))
	lines := strings.Split(strings.Trim(new, "\n"), "\n")
	for i, ln := range lines {
		// Re-base each line: swap the block's old base indent for the file's,
		// preserving any indentation nested beyond the base.
		lines[i] = fileIndent + strings.TrimPrefix(ln, oldIndent)
	}
	return strings.Join(lines, "\n")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func leadingWS(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// closestHint shows the file region most similar to old_string so the model
// can correct its next attempt.
func closestHint(content, old string) string {
	target := ""
	for _, l := range strings.Split(old, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			target = t
			break
		}
	}
	if target == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	best, bestScore := -1, 0
	for i, l := range lines {
		if s := commonPrefixLen(strings.TrimSpace(l), target); s > bestScore {
			bestScore, best = s, i
		}
	}
	if best < 0 || bestScore < 4 {
		return ""
	}
	lo, hi := max(0, best-2), min(len(lines), best+3)
	var b strings.Builder
	b.WriteString("Closest matching text in the file:\n")
	for i := lo; i < hi; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
	}
	return strings.TrimRight(b.String(), "\n")
}

func commonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func listDirTool(workdir string, a map[string]any) string {
	dir := resolve(workdir, argStr(a, "path"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "Error: " + err.Error()
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(empty directory)"
	}
	return clip(strings.Join(names, "\n"))
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, ".next": true, "target": true, "__pycache__": true,
}

func searchTool(workdir string, a map[string]any) string {
	pattern := argStr(a, "pattern")
	if pattern == "" {
		return "Error: search requires a 'pattern'"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "Error: bad regex: " + err.Error()
	}
	root := resolve(workdir, argStr(a, "path"))
	var out []string
	const maxMatches = 200
	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if len(out) >= maxMatches {
			return filepath.SkipAll
		}
		data, err := os.ReadFile(p)
		if err != nil || isBinary(data) {
			return nil
		}
		rel, _ := filepath.Rel(workdir, p)
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				out = append(out, fmt.Sprintf("%s:%d: %s", rel, i+1, oneLine(line, 200)))
				if len(out) >= maxMatches {
					break
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return "Error: " + walkErr.Error()
	}
	if len(out) == 0 {
		return "No matches."
	}
	return clip(strings.Join(out, "\n"))
}

func webSearchTool(a map[string]any) string {
	query := strings.TrimSpace(argStr(a, "query"))
	if query == "" {
		return "Error: web_search requires a 'query'"
	}
	limit := argInt(a, "limit")
	if limit <= 0 || limit > 10 {
		limit = 5
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	u := "https://www.bing.com/search?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "Error: " + err.Error()
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Nocturne/1.0; +https://nocturne.lol)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "Error: web search failed: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Sprintf("Error: web search failed: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "Error: " + err.Error()
	}

	results := parseBingResults(string(data), limit)
	if len(results) == 0 {
		return "No web results found."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Web search results for %q:\n", query)
	for i, r := range results {
		fmt.Fprintf(&b, "\n%d. %s\n   %s", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "\n   %s", r.Snippet)
		}
		b.WriteByte('\n')
	}
	return clip(strings.TrimRight(b.String(), "\n"))
}

type webResult struct {
	Title   string
	URL     string
	Snippet string
}

var (
	bingResultRE = regexp.MustCompile(`(?is)<li class="b_algo".*?<h2[^>]*>\s*<a[^>]+href="([^"]+)"[^>]*>(.*?)</a>.*?(?:<p[^>]*>(.*?)</p>)?`)
	htmlTagRE    = regexp.MustCompile(`(?s)<[^>]+>`)
	whitespaceRE = regexp.MustCompile(`\s+`)
)

func parseBingResults(page string, limit int) []webResult {
	matches := bingResultRE.FindAllStringSubmatch(page, -1)
	results := make([]webResult, 0, min(limit, len(matches)))
	seen := map[string]bool{}
	for _, m := range matches {
		if len(results) >= limit {
			break
		}
		u := cleanHTML(m[1])
		title := cleanHTML(m[2])
		snippet := ""
		if len(m) > 3 {
			snippet = cleanHTML(m[3])
		}
		if title == "" || u == "" || seen[u] {
			continue
		}
		seen[u] = true
		results = append(results, webResult{Title: title, URL: u, Snippet: snippet})
	}
	return results
}

func cleanHTML(s string) string {
	s = htmlTagRE.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = whitespaceRE.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func isBinary(data []byte) bool {
	n := len(data)
	if n > 1024 {
		n = 1024
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

func runCommandTool(workdir string, a map[string]any) string {
	command := argStr(a, "command")
	if command == "" {
		return "Error: run requires a 'command'"
	}
	if argBool(a, "background") {
		return runBackgroundCommandTool(workdir, a, command)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()

	cmd := shellCommand(ctx, command)
	cmd.Dir = workdir

	out, err := cmd.CombinedOutput()
	result := strings.TrimRight(string(out), "\n")
	if ctx.Err() == context.DeadlineExceeded {
		result += fmt.Sprintf("\n[timed out after %s]", cmdTimeout)
	} else if err != nil {
		result += "\n[exit: " + err.Error() + "]"
	}
	if strings.TrimSpace(result) == "" {
		result = "(no output)"
	}
	return clip(result)
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		// Force UTF-8 for both the console output encoding (how native command
		// output is captured) and PowerShell's own pipeline encoding, so
		// non-ASCII in the result isn't mangled into mojibake.
		wrapped := "[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; " +
			"$OutputEncoding=[System.Text.Encoding]::UTF8; " + command
		if ctx != nil {
			return exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", wrapped)
		}
		return exec.Command("powershell", "-NoProfile", "-Command", wrapped)
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	if ctx != nil {
		return exec.CommandContext(ctx, shell, "-c", command)
	}
	return exec.Command(shell, "-c", command)
}

func runBackgroundCommandTool(workdir string, a map[string]any, command string) string {
	logPath := firstNonEmpty(argStr(a, "log"), argStr(a, "log_path"))
	if logPath == "" {
		logPath = filepath.Join(".nocturne", "background", time.Now().Format("20060102-150405.000000000")+".log")
	}
	fullLog := resolve(workdir, logPath)
	if err := os.MkdirAll(filepath.Dir(fullLog), 0o755); err != nil {
		return "Error: " + err.Error()
	}
	logFile, err := os.OpenFile(fullLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "Error: " + err.Error()
	}

	cmd := shellCommand(nil, command)
	cmd.Dir = workdir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	started := time.Now()
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return "Error: " + err.Error()
	}
	pid := cmd.Process.Pid

	go func() {
		err := cmd.Wait()
		_ = logFile.Close()
		publishBackgroundCommandResult(backgroundCommandResult{
			PID:      pid,
			Command:  command,
			LogPath:  logPath,
			Started:  started,
			Finished: time.Now(),
			Err:      err,
		})
	}()

	return fmt.Sprintf("Started background command (pid %d). Output is being appended to %s", pid, logPath)
}

func deleteFileTool(workdir string, a map[string]any) string {
	path := argStr(a, "path")
	if path == "" {
		return "Error: delete requires a 'path'"
	}
	full := resolve(workdir, path)
	info, err := os.Stat(full)
	if err != nil {
		return "Error: " + err.Error()
	}
	if info.IsDir() {
		return "Error: " + path + " is a directory; delete only removes files."
	}
	if err := os.Remove(full); err != nil {
		return "Error: " + err.Error()
	}
	return "Deleted " + path
}

func renameFileTool(workdir string, a map[string]any) string {
	from := argStr(a, "from")
	to := argStr(a, "to")
	if from == "" || to == "" {
		return "Error: rename requires 'from' and 'to'"
	}
	src := resolve(workdir, from)
	dst := resolve(workdir, to)
	if _, err := os.Stat(src); err != nil {
		return "Error: " + err.Error()
	}
	if _, err := os.Stat(dst); err == nil {
		return "Error: destination " + to + " already exists; choose another name or delete it first."
	}
	if dir := filepath.Dir(dst); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := os.Rename(src, dst); err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("Renamed %s → %s", from, to)
}

// importGithubTool shallow-clones a public repo into the working directory so
// its files are available to the other tools.
func importGithubTool(workdir string, a map[string]any) string {
	repo := repoArg(a)
	if repo == "" {
		return "Error: import_github requires a 'repo' (owner/name or a GitHub URL)"
	}
	url := normalizeRepoURL(repo)

	dest := argStr(a, "dir")
	if dest == "" {
		dest = argStr(a, "path")
	}
	if dest == "" {
		dest = repoBaseName(url)
	}
	full := resolve(workdir, dest)
	if _, err := os.Stat(full); err == nil {
		return "Error: destination " + dest + " already exists; choose another 'dir' or delete it first."
	}

	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", url, full)
	cmd.Dir = workdir
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if ctx.Err() == context.DeadlineExceeded {
			msg = "timed out after " + cmdTimeout.String()
		}
		return "Error: git clone failed: " + oneLine(msg, 300)
	}

	entries, _ := os.ReadDir(full)
	var names []string
	for _, e := range entries {
		name := e.Name()
		if name == ".git" {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("Cloned %s into %s/ (%d top-level entr%s):\n%s",
		url, dest, len(names), pluralY(len(names)), clip(strings.Join(names, "\n")))
}

// repoArg pulls the repo identifier from the common arg names a model might use.
func repoArg(a map[string]any) string {
	for _, k := range []string{"repo", "url", "repository", "name"} {
		if v := strings.TrimSpace(argStr(a, k)); v != "" {
			return v
		}
	}
	return ""
}

// normalizeRepoURL turns "owner/name" or "github.com/owner/name" into a full
// https clone URL, and leaves real URLs (https / git@) untouched.
func normalizeRepoURL(repo string) string {
	repo = strings.TrimSpace(repo)
	if strings.HasPrefix(repo, "http://") || strings.HasPrefix(repo, "https://") || strings.HasPrefix(repo, "git@") {
		return repo
	}
	repo = strings.TrimPrefix(repo, "github.com/")
	return "https://github.com/" + strings.Trim(repo, "/")
}

// repoBaseName derives a clone directory name from a repo URL/slug.
func repoBaseName(url string) string {
	u := strings.TrimSuffix(strings.TrimRight(url, "/"), ".git")
	if i := strings.LastIndexByte(u, '/'); i >= 0 && i+1 < len(u) {
		return u[i+1:]
	}
	if u == "" {
		return "repo"
	}
	return u
}

// toStrings converts a decoded JSON array (e.g. ask "options") to []string,
// dropping non-strings and blanks.
func toStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// codeChange estimates lines added/removed by a successful write or edit, for
// the /usage dashboard's "code changes" stat.
func codeChange(tc ToolCall, output string) (add, del int) {
	switch canonicalTool(tc.Name) {
	case "write_file":
		if strings.HasPrefix(output, "Wrote ") {
			return lineCount(argStr(tc.Args, "content")), 0
		}
	case "edit_file":
		if strings.HasPrefix(output, "EDIT APPLIED") {
			return lineCount(argStr(tc.Args, "new_string")), lineCount(argStr(tc.Args, "old_string"))
		}
	}
	return 0, 0
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
