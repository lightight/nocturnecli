package app

import (
	"strings"
	"testing"
)

func TestParseTruncatedCall(t *testing.T) {
	// Reply cut off mid-tool-call (output limit): the call must surface as a
	// __truncated error call instead of vanishing, and the prose before the
	// tag must survive.
	narr, calls := parseResponse("Let me write that file.\n<tool name=\"write\">\n{\"path\": \"big.go\", \"content\": \"package main")
	if len(calls) != 1 {
		t.Fatalf("want 1 synthesized call, got %d (%+v)", len(calls), calls)
	}
	if calls[0].Name != "write" {
		t.Fatalf("truncated call name: %q", calls[0].Name)
	}
	if _, ok := calls[0].Args["__truncated"]; !ok {
		t.Fatalf("truncated call missing __truncated marker: %+v", calls[0].Args)
	}
	if !strings.Contains(narr, "Let me write that file.") {
		t.Fatalf("prose lost: %q", narr)
	}
	if strings.Contains(narr, "package main") {
		t.Fatalf("truncated fragment leaked into prose: %q", narr)
	}
}

func TestParseTruncatedAfterCompleteCall(t *testing.T) {
	// A complete call followed by a cut-off one: both must surface.
	_, calls := parseResponse(`<tool name="open">
{"path": "a.go"}
</tool>
<tool name="edit_file">
{"path": "a.go", "old_string": "x`)
	if len(calls) != 2 {
		t.Fatalf("want 2 calls, got %d (%+v)", len(calls), calls)
	}
	if _, ok := calls[1].Args["__truncated"]; !ok {
		t.Fatalf("second call should be marked truncated: %+v", calls[1].Args)
	}
}

func TestParseTruncatedTagOnly(t *testing.T) {
	// Even a bare, unfinished opening tag must not vanish silently.
	_, calls := parseResponse("Working on it.\n<tool name=\"edit_fi")
	if len(calls) != 1 {
		t.Fatalf("want 1 synthesized call, got %d", len(calls))
	}
	if _, ok := calls[0].Args["__truncated"]; !ok {
		t.Fatalf("missing __truncated: %+v", calls[0].Args)
	}
}

func TestParseInvalidEscapeRepaired(t *testing.T) {
	// Regex with \. (invalid JSON escape) — seen in real sessions on search.
	_, calls := parseResponse(`<tool name="search">
{"pattern": "ta\.Focus|ta\.Blur", "path": "internal/app"}
</tool>`)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	if _, bad := calls[0].Args["__parse_error"]; bad {
		t.Fatalf("invalid escape not repaired: %+v", calls[0].Args)
	}
	if got := argStr(calls[0].Args, "pattern"); got != `ta\.Focus|ta\.Blur` {
		t.Fatalf("pattern mangled: %q", got)
	}
}

func TestParseArgsErrorDetail(t *testing.T) {
	// On unrepairable JSON the specific error is reported for retry feedback.
	detail, ok := parseArgs(`{"pattern": "x", }`, &map[string]any{})
	if ok {
		t.Fatal("expected failure")
	}
	if detail == "" {
		t.Fatal("missing error detail")
	}
}

func TestEmptyReply(t *testing.T) {
	if !emptyReply("  \n\t ", nil) {
		t.Fatal("whitespace-only reply should count as empty")
	}
	if emptyReply("done", nil) {
		t.Fatal("prose reply is not empty")
	}
	if emptyReply("", []ToolCall{{Name: "open"}}) {
		t.Fatal("reply with calls is not empty")
	}
}

func TestStalledReply(t *testing.T) {
	// Intent-only replies — a promise to act with no tool call — must count as
	// stalled so the loop nudges the model instead of ending the turn.
	for _, s := range []string{
		"I’ll inspect the project structure first so I can build the app.",
		"Let me check the current directory.",
		"I will create the files now.",
		"I'm going to run the tests.",
		"Creating `index.html` in the current directory.",
		"Checking the config loader",
	} {
		if !stalledReply(s, nil) {
			t.Fatalf("intent-only reply not detected as stalled: %q", s)
		}
	}
	// Real answers and past-tense summaries must NOT be nudged.
	for _, s := range []string{
		"Hi! What would you like to work on?",
		"Fixed the parser and added tests — `go test ./...` passes.",
		"The files in /tmp are: a.txt, b.txt.",
		"Done.\n\n- Created index.html\n- Verified it renders",
	} {
		if stalledReply(s, nil) {
			t.Fatalf("final answer wrongly flagged as stalled: %q", s)
		}
	}
	// Replies with tool calls are never stalled, intent phrasing or not.
	if stalledReply("I’ll check the directory.", []ToolCall{{Name: "list_dir"}}) {
		t.Fatal("reply with calls must not be stalled")
	}
	if !stalledReply("  ", nil) {
		t.Fatal("whitespace-only reply should still be stalled")
	}
}

func TestParseHallucinatedResultNotTruncated(t *testing.T) {
	// A hallucinated <tool_result> block must be stripped, not reported as a
	// truncated tool call.
	narr, calls := parseResponse(`<tool_result name="open">fake output</tool_result>`)
	if len(calls) != 0 {
		t.Fatalf("hallucinated result produced calls: %+v", calls)
	}
	if strings.TrimSpace(narr) != "" {
		t.Fatalf("hallucinated result leaked into prose: %q", narr)
	}
}
