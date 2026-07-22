package app

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDashRenderSmoke(t *testing.T) {
	cfg := &Config{Model: "navy:gpt-5.5", Level: "", Perm: PermSmart, Stream: true, APIKey: "noct_abcd1234efgh5678"}
	m := &tuiModel{cfg: cfg, work: "/Users/ben/ai/nocturne", ver: "0.3.0", width: 150, sessionID: "20260706-101112", sessionStart: time.Now().Add(-40 * time.Second), tokens: 2_345_000, linesAdded: 120, linesRemoved: 8}
	m.usage = &UsageStats{Authenticated: true, WeekTokens: 75315, WeekCost: 0.08, AllTokens: 1_839_851, AllCost: 1.47}
	m.credits = &Credits{Authenticated: true, Dollars: 99999999999, Daily: struct {
		Used    int64 `json:"used"`
		Cap     int64 `json:"cap"`
		ResetAt int64 `json:"resetAt"`
	}{Used: 75315, Cap: 1_000_000_000, ResetAt: time.Now().Add(5*time.Hour + 23*time.Minute).UnixMilli()}}

	// synthetic sessions over the last ~40 days
	now := time.Now()
	for i := 0; i < 25; i++ {
		day := now.AddDate(0, 0, -(i * 2))
		m.dashSessions = append(m.dashSessions, Session{
			ID: fmt.Sprintf("s%d", i), Model: "navy:gpt-5.5", Started: day.Add(-2 * time.Hour), Updated: day,
			Messages: []ChatMessage{{Role: "user", Content: "do a thing"}, {Role: "assistant", Content: "ok"}},
		})
	}

	for tab := 0; tab < 3; tab++ {
		m.dashTab = tab
		if out := m.dashboardBody(); out == "" {
			t.Fatalf("tab %d (%s) rendered empty", tab, dashTabs[tab])
		}
	}
	// Cycling the Stats range must stay in bounds and keep rendering.
	for r := 0; r < len(dashRanges)+1; r++ {
		m.dashTab, m.dashRange = 2, r%len(dashRanges)
		_ = m.dashboardBody()
	}
	_ = m.dashFooter()

	// Unauthenticated endpoints: the daily bar must still render from the
	// quota carried on the last chat response (m.lastQuota).
	m.usage = &UsageStats{Authenticated: false}
	m.credits = &Credits{Authenticated: false}
	m.lastQuota = Quota{Used: 75315, Cap: 1_000_000_000}
	m.dashTab = 1
	out := m.dashboardBody()
	if !strings.Contains(ansiStrip(out), "Daily usage") {
		t.Fatalf("expected daily bar from lastQuota fallback, got:\n%s", ansiStrip(out))
	}
	if !strings.Contains(ansiStrip(out), "nocturne.lol") {
		t.Fatalf("expected web-signin hint in fallback, got:\n%s", ansiStrip(out))
	}
}
