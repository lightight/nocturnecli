package app

import (
	"strings"
	"testing"
)

func TestBadCallBreaker(t *testing.T) {
	bad := ToolCall{Name: "write", Args: map[string]any{"__parse_error": true, "__raw": "{oops"}}
	var b badCallBreaker
	if b.add(bad) || b.add(bad) {
		t.Fatal("breaker tripped before the repeat limit")
	}
	if !b.add(bad) {
		t.Fatal("breaker did not trip on the third identical bad call")
	}

	// A different bad call restarts the streak.
	b = badCallBreaker{}
	other := ToolCall{Name: "search", Args: map[string]any{"__parse_error": true, "__raw": "{nah"}}
	b.add(bad)
	b.add(other)
	if b.add(other) {
		t.Fatal("breaker tripped early after the key changed")
	}

	// A good call resets the streak.
	b = badCallBreaker{}
	b.add(bad)
	b.add(bad)
	b.reset()
	if b.add(bad) || b.add(bad) {
		t.Fatal("breaker did not reset after a good call")
	}
}

func TestNewSessionIDsAreUnique(t *testing.T) {
	a, c := newSessionID(), newSessionID()
	if a == c {
		t.Fatalf("session IDs collide: %q", a)
	}
	if !strings.Contains(a, "-") {
		t.Fatalf("session ID missing random suffix: %q", a)
	}
}

func TestExecuteMalformedCallFallback(t *testing.T) {
	out := execute(ToolCall{Name: "write", Args: map[string]any{
		"__parse_error": true, "__raw": "{bad", "__err": "unexpected end of JSON input"}}, ".")
	if !strings.Contains(out, "BAD TOOL CALL") || !strings.Contains(out, "unexpected end of JSON input") {
		t.Fatalf("fallback message missing detail: %q", out)
	}
	out = execute(ToolCall{Name: "write", Args: map[string]any{"__truncated": true, "__raw": "{\"path\":"}}, ".")
	if !strings.Contains(out, "cut off") {
		t.Fatalf("truncated fallback message wrong: %q", out)
	}
}

func TestLoadSystemTemplateIgnoresCwd(t *testing.T) {
	// A system_prompt.txt in the working directory must NOT override the
	// embedded template — only NOCTURNE_SYSTEM_PROMPT_FILE may.
	t.Setenv("NOCTURNE_SYSTEM_PROMPT_FILE", "")
	got := loadSystemTemplate()
	if !strings.Contains(got, "You are Nocturne") {
		t.Fatalf("embedded template not used: %q", got[:80])
	}
}
