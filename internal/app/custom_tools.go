package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// CustomTool is a user-installed shell-backed tool. The model sees Name,
// Description, and Args in the system prompt. When it calls the tool, Nocturne
// expands Command and runs it locally after the normal approval flow.
type CustomTool struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Command     string            `json:"command"`
	Args        map[string]string `json:"args,omitempty"`
	Provider    string            `json:"provider,omitempty"`
}

var customToolNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z_]*$`)

var builtinToolNames = map[string]bool{
	"open": true, "read_file": true, "write": true, "create": true, "write_file": true,
	"edit_file": true, "delete": true, "rename": true, "list_dir": true, "search": true,
	"run": true, "run_command": true, "import_github": true, "github": true, "import": true, "clone": true,
	"install_skill": true, "install": true, "skill": true, "skill_install": true, "install_tool": true,
	"ask": true, "cowork": true, "finish": true, "task": true, "agent": true, "subagent": true, "spawn": true,
	"screenshot": true, "click": true, "move_mouse": true, "scroll": true, "type_text": true, "key_press": true, "open_app": true,
}

func validateCustomTool(t CustomTool) error {
	if t.Name == "" {
		return fmt.Errorf("tool name is required")
	}
	if !customToolNameRE.MatchString(t.Name) {
		return fmt.Errorf("invalid tool name %q: use letters and underscores only", t.Name)
	}
	if builtinToolNames[t.Name] || builtinToolNames[canonicalTool(t.Name)] {
		return fmt.Errorf("%q is a built-in tool name", t.Name)
	}
	if strings.TrimSpace(t.Command) == "" {
		return fmt.Errorf("tool %q needs a command", t.Name)
	}
	for arg := range t.Args {
		if !customToolNameRE.MatchString(arg) {
			return fmt.Errorf("tool %q has invalid arg name %q", t.Name, arg)
		}
	}
	return nil
}

func normalizeCustomTools(in []CustomTool) []CustomTool {
	out := make([]CustomTool, 0, len(in))
	seen := map[string]bool{}
	for _, t := range in {
		t.Name = strings.TrimSpace(t.Name)
		t.Description = strings.TrimSpace(t.Description)
		t.Command = strings.TrimSpace(t.Command)
		t.Provider = strings.TrimSpace(t.Provider)
		if t.Args == nil {
			t.Args = map[string]string{}
		}
		if validateCustomTool(t) != nil || seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func findCustomTool(tools []CustomTool, name string) (CustomTool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return CustomTool{}, false
}

func (c *Config) AddCustomTool(t CustomTool) error {
	t.Name = strings.TrimSpace(t.Name)
	t.Description = strings.TrimSpace(t.Description)
	t.Command = strings.TrimSpace(t.Command)
	if t.Args == nil {
		t.Args = map[string]string{}
	}
	if err := validateCustomTool(t); err != nil {
		return err
	}
	for i, existing := range c.Tools {
		if existing.Name == t.Name {
			c.Tools[i] = t
			c.Tools = normalizeCustomTools(c.Tools)
			return nil
		}
	}
	c.Tools = normalizeCustomTools(append(c.Tools, t))
	return nil
}

func (c *Config) RemoveCustomTool(name string) bool {
	for i, t := range c.Tools {
		if t.Name == strings.TrimSpace(name) {
			c.Tools = append(c.Tools[:i], c.Tools[i+1:]...)
			return true
		}
	}
	return false
}

func importToolProvider(c *Config, src string) (int, error) {
	data, err := readProviderBytes(strings.TrimSpace(src))
	if err != nil {
		return 0, err
	}
	tools, err := parseToolProvider(data, src)
	if err != nil {
		return 0, err
	}
	if len(tools) == 0 {
		return 0, fmt.Errorf("provider contains no tools")
	}
	for _, t := range tools {
		if err := c.AddCustomTool(t); err != nil {
			return 0, err
		}
	}
	return len(tools), nil
}

func readProviderBytes(src string) ([]byte, error) {
	if src == "" {
		return nil, fmt.Errorf("provider path or URL is required")
	}
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	}
	return os.ReadFile(src)
}

func parseToolProvider(data []byte, provider string) ([]CustomTool, error) {
	var wrapped struct {
		Tools []CustomTool `json:"tools"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Tools != nil {
		for i := range wrapped.Tools {
			if wrapped.Tools[i].Provider == "" {
				wrapped.Tools[i].Provider = provider
			}
		}
		return normalizeCustomTools(wrapped.Tools), nil
	}
	var tools []CustomTool
	if err := json.Unmarshal(data, &tools); err != nil {
		return nil, fmt.Errorf("provider must be a JSON array or object with a tools array: %w", err)
	}
	for i := range tools {
		if tools[i].Provider == "" {
			tools[i].Provider = provider
		}
	}
	return normalizeCustomTools(tools), nil
}

func customToolsPrompt(tools []CustomTool) string {
	tools = normalizeCustomTools(tools)
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Custom tools\n")
	b.WriteString("These user-installed tools run local shell commands after the normal approval flow. Call them like built-in tools with JSON args.\n")
	for _, t := range tools {
		b.WriteString("- " + t.Name)
		if t.Description != "" {
			b.WriteString(" — " + t.Description)
		}
		if len(t.Args) == 0 {
			b.WriteString(". Args: {}.\n")
			continue
		}
		keys := make([]string, 0, len(t.Args))
		for k := range t.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%q: string", k))
		}
		b.WriteString(". Args: {" + strings.Join(parts, ", ") + "}.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

var commandPlaceholderRE = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z_]*)\s*\}\}`)

func executeCustomTool(workdir string, t CustomTool, args map[string]any) string {
	cmd, err := expandCustomCommand(t.Command, args)
	if err != nil {
		return "Error: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "cmd", "/C", cmd)
	} else {
		c = exec.CommandContext(ctx, "sh", "-c", cmd)
	}
	c.Dir = workdir
	c.Env = append(os.Environ(), "NOCTURNE_TOOL_NAME="+t.Name)
	if b, err := json.Marshal(args); err == nil {
		c.Env = append(c.Env, "NOCTURNE_TOOL_ARGS="+string(b))
	}
	for k, v := range args {
		if customToolNameRE.MatchString(k) {
			c.Env = append(c.Env, "NOCTURNE_ARG_"+strings.ToUpper(k)+"="+fmt.Sprint(v))
		}
	}
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &out
	err = c.Run()
	text := strings.TrimRight(out.String(), "\n")
	if ctx.Err() == context.DeadlineExceeded {
		return clip(fmt.Sprintf("Error: custom tool %s timed out after %s\n%s", t.Name, cmdTimeout, text))
	}
	if err != nil {
		if text == "" {
			return "Error: " + err.Error()
		}
		return clip("Error: " + err.Error() + "\n" + text)
	}
	if text == "" {
		text = fmt.Sprintf("custom tool %s completed", t.Name)
	}
	return clip(text)
}

func expandCustomCommand(template string, args map[string]any) (string, error) {
	missing := ""
	cmd := commandPlaceholderRE.ReplaceAllStringFunc(template, func(match string) string {
		parts := commandPlaceholderRE.FindStringSubmatch(match)
		name := parts[1]
		v, ok := args[name]
		if !ok {
			missing = name
			return ""
		}
		return shellQuote(fmt.Sprint(v))
	})
	if missing != "" {
		return "", fmt.Errorf("missing required arg %q for custom tool command", missing)
	}
	return cmd, nil
}

func shellQuote(s string) string {
	if runtime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return `'` + strings.ReplaceAll(s, `'`, `'"'"'`) + `'`
}

func skillURLArg(a map[string]any) string {
	for _, k := range []string{"url", "src", "source", "path"} {
		if v := strings.TrimSpace(argStr(a, k)); v != "" {
			return v
		}
	}
	return ""
}

func installSkillTool(c *Config, a map[string]any) string {
	msg, err := installSkill(c, skillURLArg(a))
	if err != nil {
		return "Error: " + err.Error()
	}
	return msg
}

func installSkill(c *Config, src string) (string, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return "", fmt.Errorf("install_skill requires a 'url'")
	}
	if looksLikeGitHubRepo(src) {
		return installGitSkill(c, src)
	}
	data, err := readProviderBytes(src)
	if err != nil {
		return "", err
	}
	if tools, err := parseSkillManifest(data, src, ""); err == nil && len(tools) > 0 {
		return addInstalledTools(c, tools)
	}
	return installSingleScriptSkill(c, src, data)
}

func parseSkillManifest(data []byte, provider, installDir string) ([]CustomTool, error) {
	if tools, err := parseToolProvider(data, provider); err == nil && len(tools) > 0 {
		return toolsWithInstallDir(tools, installDir), nil
	}
	var one CustomTool
	if err := json.Unmarshal(data, &one); err == nil && one.Name != "" {
		if one.Provider == "" {
			one.Provider = provider
		}
		return toolsWithInstallDir([]CustomTool{one}, installDir), nil
	}
	var skills struct {
		Skills []CustomTool `json:"skills"`
	}
	if err := json.Unmarshal(data, &skills); err == nil && len(skills.Skills) > 0 {
		for i := range skills.Skills {
			if skills.Skills[i].Provider == "" {
				skills.Skills[i].Provider = provider
			}
		}
		return toolsWithInstallDir(skills.Skills, installDir), nil
	}
	return nil, fmt.Errorf("not a Nocturne skill manifest")
}

func toolsWithInstallDir(tools []CustomTool, dir string) []CustomTool {
	if dir == "" {
		return normalizeCustomTools(tools)
	}
	for i := range tools {
		tools[i].Command = commandInDir(tools[i].Command, dir)
	}
	return normalizeCustomTools(tools)
}

func commandInDir(cmd, dir string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" || dir == "" {
		return cmd
	}
	if runtime.GOOS == "windows" {
		return "cd /d " + shellQuote(dir) + " && " + cmd
	}
	return "cd " + shellQuote(dir) + " && " + cmd
}

func addInstalledTools(c *Config, tools []CustomTool) (string, error) {
	if len(tools) == 0 {
		return "", fmt.Errorf("skill contains no tools")
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if err := c.AddCustomTool(t); err != nil {
			return "", err
		}
		names = append(names, t.Name)
	}
	if err := c.Save(); err != nil {
		return "", err
	}
	sort.Strings(names)
	return "Installed skill: " + strings.Join(names, ", "), nil
}

func installSingleScriptSkill(c *Config, src string, data []byte) (string, error) {
	name := skillNameFromSource(src)
	if name == "" {
		name = "skill"
	}
	dir := filepath.Join(configDir(), "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	file := filepath.Base(sourcePath(src))
	if file == "." || file == string(filepath.Separator) || file == "" {
		file = name
	}
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, data, 0o755); err != nil {
		return "", err
	}
	t := CustomTool{
		Name:        name,
		Description: "Installed from " + src,
		Command:     commandForScript(path),
		Args:        map[string]string{},
		Provider:    src,
	}
	return addInstalledTools(c, []CustomTool{t})
}

func installGitSkill(c *Config, src string) (string, error) {
	repoURL := normalizeRepoURL(src)
	name := skillNameFromSource(src)
	if name == "" {
		name = repoBaseName(repoURL)
	}
	dir := filepath.Join(configDir(), "skills", name)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", repoURL, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if ctx.Err() == context.DeadlineExceeded {
			msg = "timed out after " + cmdTimeout.String()
		}
		return "", fmt.Errorf("git clone failed: %s", oneLine(msg, 300))
	}
	if manifest, err := findSkillManifest(dir); err == nil {
		data, err := os.ReadFile(manifest)
		if err != nil {
			return "", err
		}
		tools, err := parseSkillManifest(data, src, filepath.Dir(manifest))
		if err != nil {
			return "", err
		}
		return addInstalledTools(c, tools)
	}
	tool, err := inferRepoTool(src, dir, name)
	if err != nil {
		return "", err
	}
	return addInstalledTools(c, []CustomTool{tool})
}

func findSkillManifest(dir string) (string, error) {
	for _, rel := range []string{"nocturne.json", "skill.json", "skills.json", "tools.json", filepath.Join(".nocturne", "tools.json")} {
		p := filepath.Join(dir, rel)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

func inferRepoTool(provider, dir, name string) (CustomTool, error) {
	for _, rel := range []string{"run.sh", "skill.sh", "main.py", "skill.py", "index.js", "main.js"} {
		p := filepath.Join(dir, rel)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			_ = os.Chmod(p, 0o755)
			return CustomTool{
				Name:        name,
				Description: "Installed from " + provider,
				Command:     commandInDir(commandForScript(p), dir),
				Args:        map[string]string{},
				Provider:    provider,
			}, nil
		}
	}
	return CustomTool{}, fmt.Errorf("couldn't find a skill manifest or obvious entry point in %s", provider)
}

func commandForScript(path string) string {
	q := shellQuote(path)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py":
		return "python3 " + q
	case ".js", ".mjs", ".cjs":
		return "node " + q
	case ".rb":
		return "ruby " + q
	case ".sh", ".bash", ".zsh":
		return "sh " + q
	default:
		return q
	}
}

func looksLikeGitHubRepo(src string) bool {
	u, err := url.Parse(src)
	if err != nil || u.Host == "" || strings.ToLower(u.Host) != "github.com" {
		return false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 {
		return false
	}
	return parts[0] != "" && parts[1] != "" && !strings.Contains(parts[1], ".")
}

func skillNameFromSource(src string) string {
	path := sourcePath(src)
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	base = strings.TrimSuffix(base, ".git")
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' {
			b.WriteRune(r)
		} else if (r >= '0' && r <= '9') && b.Len() > 0 {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

func sourcePath(src string) string {
	if u, err := url.Parse(src); err == nil && u.Path != "" {
		return u.Path
	}
	return src
}
