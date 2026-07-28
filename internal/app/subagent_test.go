package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSystemPromptPlanMode(t *testing.T) {
	off := systemPromptMode("/tmp/work", false, false, false, false)
	if strings.Contains(off, "Plan mode") {
		t.Error("base prompt should not mention plan mode")
	}
	on := systemPromptMode("/tmp/work", false, true, false, false)
	for _, want := range []string{"Plan mode", "read-only", "/plan"} {
		if !strings.Contains(on, want) {
			t.Errorf("plan-mode prompt missing %q", want)
		}
	}
}

func TestSystemPromptTaskTool(t *testing.T) {
	off := systemPromptMode("/tmp/work", false, false, false, false)
	if strings.Contains(off, "Sub-agents") {
		t.Error("base prompt should not advertise the task tool")
	}
	on := systemPromptMode("/tmp/work", false, false, false, true)
	for _, want := range []string{"Sub-agents", "task", "sub-agent", "prompt"} {
		if !strings.Contains(on, want) {
			t.Errorf("extended-level prompt missing %q", want)
		}
	}
}

// TestRunSubagent drives a sub-agent loop against a fake API: the first reply
// requests a (read-only) tool, the second finishes with a report. It verifies
// the loop executes tools, returns the report, forces extended thinking, and
// doesn't offer the sub-agent the task tool itself.
func TestRunSubagent(t *testing.T) {
	var levels []string
	var systems []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Level    string `json:"level"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		levels = append(levels, req.Level)
		for _, m := range req.Messages {
			if m.Role == "system" {
				if s, ok := m.Content.(string); ok {
					systems = append(systems, s)
				}
			}
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"ok":true,"response":"<tool name=\"list_dir\">\n{\"path\":\".\"}\n</tool>"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"response":"report: everything is done"}`))
	}))
	defer srv.Close()

	cfg := &Config{APIKey: "noct_test", BaseURL: srv.URL, Model: "test-model", Level: "off"}
	report, err := runSubagent(context.Background(), cfg, NewClient(cfg), t.TempDir(), "do the thing")
	if err != nil {
		t.Fatalf("runSubagent: %v", err)
	}
	if report != "report: everything is done" {
		t.Errorf("report = %q", report)
	}
	if calls != 2 {
		t.Errorf("expected 2 API calls (tool round + final), got %d", calls)
	}
	for i, lv := range levels {
		if lv != "extended" {
			t.Errorf("request %d level = %q, want extended (forced for sub-agents)", i, lv)
		}
	}
	if len(systems) == 0 {
		t.Fatal("no system prompt seen")
	}
	if strings.Contains(systems[0], "Sub-agents") {
		t.Error("sub-agent system prompt should not offer the task tool (one nesting level only)")
	}
	// Config must not be mutated by the forced level.
	if cfg.Level != "off" {
		t.Errorf("caller config level mutated to %q", cfg.Level)
	}
}
