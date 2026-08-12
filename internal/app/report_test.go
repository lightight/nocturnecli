package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestReportCryptoRoundtrip(t *testing.T) {
	pub, priv, err := generateReportKey()
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte(`{"v":1,"counts":{"bad_call_json":4}}`)
	env, err := encryptReport(plain, pub)
	if err != nil {
		t.Fatal(err)
	}
	// The envelope must not contain any plaintext.
	if strings.Contains(string(env), "bad_call_json") {
		t.Fatal("envelope leaks plaintext")
	}
	out, err := decryptReport(env, priv)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(plain) {
		t.Fatalf("roundtrip mismatch: %q", out)
	}
	// A different private key must fail.
	_, priv2, _ := generateReportKey()
	if _, err := decryptReport(env, priv2); err == nil {
		t.Fatal("decrypt succeeded with the wrong key")
	}
}

func TestReportPayloadAllowlist(t *testing.T) {
	// The payload is the whole privacy promise: only these top-level keys may
	// ever appear. If a future field gets added, this test forces a conscious
	// privacy review.
	h := newHealthTracker()
	h.recordBadCall(ToolCall{Name: "search", Args: map[string]any{
		"__parse_error": true, "__raw": "{oops", "__err": "invalid character 'F' after object key"}}, "")
	h.recordBadCall(ToolCall{Name: "bogus", Args: map[string]any{}}, "BAD TOOL CALL ... \"bogus\" is not a known tool ...")
	h.recordBadCall(ToolCall{Name: "write", Args: map[string]any{"__truncated": true}}, "")
	h.recordEmptyReply()
	h.recordStreamError()
	h.recordBreakerTrip()
	cfg := &Config{Model: "navy:gpt-5.5", Level: "extended"}
	p := buildReport(h, cfg, "0.3.0", 42, time.Now().Add(-time.Minute))

	data, _ := json.Marshal(p)
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"v": true, "id": true, "time": true, "version": true, "goos": true,
		"goarch": true, "model": true, "level": true, "counts": true,
		"hiccups": true, "tools": true, "json_errors": true, "messages": true,
		"duration_secs": true,
	}
	for k := range m {
		if !allowed[k] {
			t.Fatalf("payload contains non-allowlisted field %q", k)
		}
	}
	// Nothing user-identifying may leak through values either.
	s := string(data)
	for _, bad := range []string{"/Users/", "api_key", "noct_", "cwd", "secret", "{oops"} {
		if strings.Contains(s, bad) {
			t.Fatalf("payload leaks %q: %s", bad, s)
		}
	}
	// The decoder error must be redacted.
	if !strings.Contains(s, `'?'`) || strings.Contains(s, "'F'") {
		t.Fatalf("json error not redacted: %s", s)
	}
	// Categories land where expected.
	if p.Counts["bad_call_json"] != 1 || p.Counts["bad_call_unknown_tool"] != 1 ||
		p.Counts["bad_call_truncated"] != 1 || p.Counts["empty_reply"] != 1 ||
		p.Counts["stream_error"] != 1 || p.Counts["breaker_trip"] != 1 {
		t.Fatalf("counts wrong: %+v", p.Counts)
	}
	if p.Tools[0] != "bogus" && len(p.Tools) != 2 {
		t.Fatalf("tools wrong: %+v", p.Tools)
	}
}

func TestRedactJSONError(t *testing.T) {
	got := redactJSONError("invalid character 'F' after object key:value pair")
	if strings.Contains(got, "'F'") || !strings.Contains(got, "'?'") {
		t.Fatalf("not redacted: %q", got)
	}
}

func TestReportRecipientKey(t *testing.T) {
	// With no env override, the baked-in team key is used and must be a valid
	// 32-byte X25519 public key.
	t.Setenv("NOCTURNE_REPORT_PUBKEY", "")
	pub, err := reportRecipientKey()
	if err != nil {
		t.Fatal(err)
	}
	if pub != reportPublicKeyB64 || len(mustB64(pub)) != 32 {
		t.Fatalf("bad baked key: %q", pub)
	}
	// A bogus override is rejected.
	t.Setenv("NOCTURNE_REPORT_PUBKEY", "not-a-key")
	if _, err := reportRecipientKey(); err == nil {
		t.Fatal("expected error for invalid override")
	}
}

func TestHealthTrackerIssuesExcludesRounds(t *testing.T) {
	h := newHealthTracker()
	h.recordRound()
	h.recordRound()
	if h.issues() != 0 {
		t.Fatalf("rounds counted as issues: %d", h.issues())
	}
	h.recordEmptyReply()
	if h.issues() != 1 {
		t.Fatalf("issues: %d", h.issues())
	}
}

func TestHiccupTimeline(t *testing.T) {
	// The timeline must say WHERE each hiccup happened, in order, without any
	// content — position + category + tool name only.
	h := newHealthTracker()
	h.recordRound()
	h.recordRound()
	h.recordBadCall(ToolCall{Name: "write", Args: map[string]any{"__truncated": true}}, "")
	h.recordRound()
	h.recordEmptyReply()
	if len(h.events) != 2 {
		t.Fatalf("events: %+v", h.events)
	}
	if h.events[0] != (hiccupEvent{At: 2, Kind: "bad_call_truncated", Tool: "write_file"}) {
		t.Fatalf("event 0 wrong: %+v", h.events[0])
	}
	if h.events[1] != (hiccupEvent{At: 3, Kind: "empty_reply"}) {
		t.Fatalf("event 1 wrong: %+v", h.events[1])
	}
	// The timeline is capped so a pathological session can't bloat the report.
	for i := 0; i < maxHiccupEvents+10; i++ {
		h.recordStreamError()
	}
	if len(h.events) != maxHiccupEvents {
		t.Fatalf("timeline not capped: %d events", len(h.events))
	}
}
