package app

import (
	"context"
	"fmt"
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

// toolResult pairs a call with its textual output for feeding back.
type toolResult struct {
	Name   string
	Output string
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
	}
	return name
}

// needsApproval reports whether a tool mutates state / runs code and must be
// confirmed by the user (unless auto-accept is on).
func needsApproval(name string) bool {
	switch canonicalTool(name) {
	case "write_file", "edit_file", "run_command", "delete", "rename", "import_github":
		return true
	}
	return false
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
	case "run_command":
		return fmt.Sprintf("%s: %s", tc.Name, oneLine(argStr(tc.Args, "command"), 80))
	case "delete":
		return fmt.Sprintf("delete(%s)", argStr(tc.Args, "path"))
	case "rename":
		return fmt.Sprintf("rename(%s → %s)", argStr(tc.Args, "from"), argStr(tc.Args, "to"))
	case "import_github":
		return fmt.Sprintf("import_github(%s)", repoArg(tc.Args))
	case "ask":
		return "ask: " + oneLine(argStr(tc.Args, "question"), 70)
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
	}
	return ""
}

// execute dispatches a tool call and returns its textual result. Errors are
// returned as text (prefixed "Error:") rather than Go errors so the model can
// read and recover from them.
func execute(tc ToolCall, workdir string) string {
	if _, bad := tc.Args["__parse_error"]; bad {
		raw, _ := tc.Args["__raw"].(string)
		return fmt.Sprintf("TOOL CALL FAILED: the arguments for %q were not valid JSON, so nothing ran. "+
			"Re-send the <tool name=%q> call with exactly one valid JSON object (no extra braces, no trailing text). You sent: %s",
			tc.Name, tc.Name, raw)
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
	case "run_command":
		return runCommandTool(workdir, tc.Args)
	case "delete":
		return deleteFileTool(workdir, tc.Args)
	case "rename":
		return renameFileTool(workdir, tc.Args)
	case "import_github":
		return importGithubTool(workdir, tc.Args)
	case "ask":
		// ask is normally intercepted by the interactive loop; this is the
		// non-interactive fallback so headless runs don't stall.
		return "ask is unavailable in this mode. Proceed with your best judgment and state any assumption you made."
	case "finish":
		if s := strings.TrimSpace(argStr(tc.Args, "summary")); s != "" {
			return s
		}
		return "Task complete."
	default:
		return "Error: unknown tool " + tc.Name
	}
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
		return "Error: read_file requires a 'path'"
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
		return "Error: write_file requires a 'path'"
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
			return fmt.Sprintf("EDIT FAILED: old_string matches %d places in %s (ignoring whitespace) — NO changes were made. Add more context, or use write_file.", n, path)
		}
	}

	return "EDIT FAILED: old_string was not found in " + path + ", so NO changes were made. " +
		"Re-read the file and copy the exact text (including indentation) into old_string, or use write_file to rewrite the file.\n" +
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
		return "Error: run_command requires a 'command'"
	}
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", command)
	} else {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd = exec.CommandContext(ctx, shell, "-c", command)
	}
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

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
