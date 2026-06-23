package app

import (
	"strings"
	"testing"
)

func TestStreamPreviewHidesToolTags(t *testing.T) {
	// A partial, not-yet-closed tool tag must not show raw.
	out := streamPreview("I'll edit the file.\n<tool name=\"edit_fi")
	if strings.Contains(out, "<tool") {
		t.Fatalf("raw partial tool tag leaked: %q", out)
	}
	if !strings.Contains(out, "preparing tool") {
		t.Fatalf("missing tool marker: %q", out)
	}
	if !strings.Contains(out, "I'll edit the file.") {
		t.Fatalf("narration lost: %q", out)
	}
	// A complete tool block must also be hidden.
	out2 := streamPreview("Doing it.\n<tool name=\"read_file\">\n{\"path\":\"a\"}\n</tool>")
	if strings.Contains(out2, "<tool") || strings.Contains(out2, "read_file\"") {
		t.Fatalf("complete tool block leaked: %q", out2)
	}
}

func TestParseExtraBrace(t *testing.T) {
	// Trailing extra "}" that broke gpt-5.4-mini.
	_, calls := parseResponse(`<tool name="edit_file">
{"path": "a.py", "old_string": "x", "new_string": "y"}}
</tool>`)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	if got := argStr(calls[0].Args, "path"); got != "a.py" {
		t.Fatalf("path not parsed from malformed JSON: %q", got)
	}
}

func TestParseLiteralNewlines(t *testing.T) {
	// Raw newline inside a JSON string (invalid JSON) — kimi-k2.6 does this.
	_, calls := parseResponse("<tool name=\"edit_file\">\n{\"path\": \"a.py\", \"old_string\": \"PORT = 8080\nDEBUG = False\", \"new_string\": \"PORT = 80\"}\n</tool>")
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	if got := argStr(calls[0].Args, "old_string"); got != "PORT = 8080\nDEBUG = False" {
		t.Fatalf("old_string not repaired: %q", got)
	}
}

func TestParseAltFunctionStyle(t *testing.T) {
	// <tool>read_file({...})</tool> function-call style.
	_, calls := parseResponse(`<tool>read_file({"path": "a.py"})</tool>`)
	if len(calls) != 1 || calls[0].Name != "read_file" {
		t.Fatalf("alt format not normalised: %+v", calls)
	}
	if got := argStr(calls[0].Args, "path"); got != "a.py" {
		t.Fatalf("alt format path: %q", got)
	}
}

func TestParseDeduplicates(t *testing.T) {
	// One malformed + one valid identical call should collapse to one.
	_, calls := parseResponse(`<tool name="edit_file">
{"path": "a", "old_string": "x", "new_string": "y"}}
</tool>
<tool name="edit_file">
{"path": "a", "old_string": "x", "new_string": "y"}
</tool>`)
	if len(calls) != 1 {
		t.Fatalf("want 1 deduped call, got %d", len(calls))
	}
}

func TestParseNoTools(t *testing.T) {
	narr, calls := parseResponse("Just a plain answer, no tools.")
	if len(calls) != 0 || narr != "Just a plain answer, no tools." {
		t.Fatalf("plain answer mishandled: %q %+v", narr, calls)
	}
}
