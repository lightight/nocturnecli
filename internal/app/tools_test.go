package app

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCanonicalTool(t *testing.T) {
	cases := map[string]string{
		"write":         "write_file",
		"create":        "write_file",
		"open":          "read_file",
		"run":           "run_command",
		"github":        "import_github",
		"import":        "import_github",
		"clone":         "import_github",
		"import_github": "import_github",
		// unmapped names pass through unchanged
		"read_file": "read_file",
		"edit_file": "edit_file",
		"delete":    "delete",
		"rename":    "rename",
		"ask":       "ask",
		"finish":    "finish",
	}
	for in, want := range cases {
		if got := canonicalTool(in); got != want {
			t.Errorf("canonicalTool(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNeedsApprovalViaAlias(t *testing.T) {
	for _, n := range []string{"write", "create", "run", "delete", "rename", "import_github", "github"} {
		if !needsApproval(n) {
			t.Errorf("needsApproval(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"open", "read_file", "list_dir", "search", "ask", "finish"} {
		if needsApproval(n) {
			t.Errorf("needsApproval(%q) = true, want false", n)
		}
	}
}

func TestDeleteFileTool(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "gone.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := deleteFileTool(dir, map[string]any{"path": "gone.txt"}); out != "Deleted gone.txt" {
		t.Fatalf("delete result = %q", out)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatal("file should be gone")
	}
	// missing file → error, dir → refused, no path → error
	if out := deleteFileTool(dir, map[string]any{"path": "nope.txt"}); !startsWith(out, "Error:") {
		t.Errorf("missing file should error, got %q", out)
	}
	if out := deleteFileTool(dir, map[string]any{"path": "."}); !startsWith(out, "Error:") {
		t.Errorf("deleting a directory should error, got %q", out)
	}
	if out := deleteFileTool(dir, map[string]any{}); !startsWith(out, "Error:") {
		t.Errorf("missing path should error, got %q", out)
	}
}

func TestRenameFileTool(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// rename into a nested dir that doesn't exist yet (should be created)
	if out := renameFileTool(dir, map[string]any{"from": "a.txt", "to": "sub/b.txt"}); out != "Renamed a.txt → sub/b.txt" {
		t.Fatalf("rename result = %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "b.txt")); err != nil {
		t.Fatalf("renamed file missing: %v", err)
	}
	// missing 'to', missing source, and existing destination all error
	if out := renameFileTool(dir, map[string]any{"from": "sub/b.txt"}); !startsWith(out, "Error:") {
		t.Errorf("missing 'to' should error, got %q", out)
	}
	if out := renameFileTool(dir, map[string]any{"from": "ghost.txt", "to": "x.txt"}); !startsWith(out, "Error:") {
		t.Errorf("missing source should error, got %q", out)
	}
	_ = os.WriteFile(filepath.Join(dir, "c.txt"), []byte("y"), 0o644)
	if out := renameFileTool(dir, map[string]any{"from": "c.txt", "to": "sub/b.txt"}); !startsWith(out, "Error:") {
		t.Errorf("existing destination should error, got %q", out)
	}
}

func TestNormalizeRepoURL(t *testing.T) {
	cases := map[string]string{
		"octocat/Hello-World":            "https://github.com/octocat/Hello-World",
		"github.com/octocat/Hello-World": "https://github.com/octocat/Hello-World",
		"octocat/Hello-World/":           "https://github.com/octocat/Hello-World",
		"https://github.com/a/b":         "https://github.com/a/b",
		"https://gitlab.com/a/b.git":     "https://gitlab.com/a/b.git",
		"git@github.com:a/b.git":         "git@github.com:a/b.git",
	}
	for in, want := range cases {
		if got := normalizeRepoURL(in); got != want {
			t.Errorf("normalizeRepoURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoBaseName(t *testing.T) {
	cases := map[string]string{
		"https://github.com/octocat/Hello-World":     "Hello-World",
		"https://github.com/octocat/Hello-World.git": "Hello-World",
		"https://github.com/octocat/Hello-World/":    "Hello-World",
		"git@github.com:a/b.git":                     "b",
	}
	for in, want := range cases {
		if got := repoBaseName(in); got != want {
			t.Errorf("repoBaseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoArg(t *testing.T) {
	if got := repoArg(map[string]any{"url": "a/b"}); got != "a/b" {
		t.Errorf("repoArg from url = %q", got)
	}
	if got := repoArg(map[string]any{"repo": "x/y", "url": "z/w"}); got != "x/y" {
		t.Errorf("repo should win over url, got %q", got)
	}
	if got := repoArg(map[string]any{}); got != "" {
		t.Errorf("empty args should give empty repo, got %q", got)
	}
}

func TestToStrings(t *testing.T) {
	got := toStrings([]any{"a", "b", "", 3, "c"})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("toStrings = %#v, want %#v", got, want)
	}
	if toStrings("not an array") != nil {
		t.Error("non-array should give nil")
	}
	if toStrings(nil) != nil {
		t.Error("nil should give nil")
	}
}

// TestExecuteAliases drives the real execute() dispatcher through the aliased
// names to confirm the whole routing path works, not just canonicalTool.
func TestExecuteAliases(t *testing.T) {
	dir := t.TempDir()

	// create (alias of write_file) writes a file...
	if out := execute(ToolCall{Name: "create", Args: map[string]any{"path": "hi.txt", "content": "hello"}}, dir); !startsWith(out, "Wrote ") {
		t.Fatalf("create result = %q", out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "hi.txt")); string(b) != "hello" {
		t.Fatalf("create did not write expected content: %q", b)
	}

	// open (alias of read_file) reads it back (line-numbered)...
	if out := execute(ToolCall{Name: "open", Args: map[string]any{"path": "hi.txt"}}, dir); !contains(out, "hello") {
		t.Fatalf("open result = %q", out)
	}

	// finish returns the summary; ask returns non-blocking guidance.
	if out := execute(ToolCall{Name: "finish", Args: map[string]any{"summary": "all done"}}, dir); out != "all done" {
		t.Errorf("finish result = %q", out)
	}
	if out := execute(ToolCall{Name: "ask", Args: map[string]any{"question": "x?"}}, dir); !contains(out, "best judgment") {
		t.Errorf("ask fallback = %q", out)
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
