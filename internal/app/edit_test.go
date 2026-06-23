package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runEdit(t *testing.T, initial, old, new string, replaceAll bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"path": p, "old_string": old, "new_string": new}
	if replaceAll {
		args["replace_all"] = true
	}
	res := editFileTool(dir, args)
	data, _ := os.ReadFile(p)
	return res, string(data)
}

func TestEditExact(t *testing.T) {
	res, got := runEdit(t, "alpha\nbeta\ngamma\n", "beta", "BETA", false)
	if !strings.HasPrefix(res, "EDIT APPLIED") {
		t.Fatalf("want APPLIED, got %q", res)
	}
	if got != "alpha\nBETA\ngamma\n" {
		t.Fatalf("bad content: %q", got)
	}
}

func TestEditTrailingWhitespaceTolerant(t *testing.T) {
	// File line has trailing spaces; the model's old_string doesn't.
	res, got := runEdit(t, "func main() {   \n\tx := 1\n}\n", "func main() {\n\tx := 1\n}", "func main() {\n\tx := 2\n}", false)
	if !strings.HasPrefix(res, "EDIT APPLIED") {
		t.Fatalf("want APPLIED, got %q", res)
	}
	if !strings.Contains(got, "x := 2") {
		t.Fatalf("edit not applied: %q", got)
	}
}

func TestEditIndentationTolerant(t *testing.T) {
	// File is tab-indented; the model used spaces and the wrong amount.
	initial := "class A:\n\tdef run(self):\n\t\treturn 1\n"
	res, got := runEdit(t, initial, "def run(self):\n    return 1", "def run(self):\n    return 2", false)
	if !strings.HasPrefix(res, "EDIT APPLIED") {
		t.Fatalf("want APPLIED, got %q", res)
	}
	if strings.Contains(got, "return 1") || !strings.Contains(got, "return 2") {
		t.Fatalf("edit not applied: %q", got)
	}
	// Relative indentation must be preserved: "return 2" deeper than "def run".
	defIndent := leadingWS(lineWith(got, "def run"))
	retIndent := leadingWS(lineWith(got, "return 2"))
	if len(retIndent) <= len(defIndent) {
		t.Fatalf("relative indentation lost: def=%q ret=%q in %q", defIndent, retIndent, got)
	}
}

func lineWith(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

func TestEditNotFoundFails(t *testing.T) {
	res, got := runEdit(t, "one\ntwo\nthree\n", "TWELVE", "X", false)
	if !strings.HasPrefix(res, "EDIT FAILED") {
		t.Fatalf("want FAILED, got %q", res)
	}
	if got != "one\ntwo\nthree\n" {
		t.Fatalf("file should be unchanged: %q", got)
	}
}

func TestEditNonUniqueFails(t *testing.T) {
	res, got := runEdit(t, "x\nx\nx\n", "x", "y", false)
	if !strings.HasPrefix(res, "EDIT FAILED") {
		t.Fatalf("want FAILED for non-unique, got %q", res)
	}
	if got != "x\nx\nx\n" {
		t.Fatalf("file should be unchanged: %q", got)
	}
}

func TestEditReplaceAll(t *testing.T) {
	res, got := runEdit(t, "x\nx\nx\n", "x", "y", true)
	if !strings.HasPrefix(res, "EDIT APPLIED") {
		t.Fatalf("want APPLIED, got %q", res)
	}
	if got != "y\ny\ny\n" {
		t.Fatalf("replace_all failed: %q", got)
	}
}

func TestEditNoOpFails(t *testing.T) {
	res, _ := runEdit(t, "a\n", "a", "a", false)
	if !strings.HasPrefix(res, "EDIT FAILED") {
		t.Fatalf("identical strings should fail, got %q", res)
	}
}
