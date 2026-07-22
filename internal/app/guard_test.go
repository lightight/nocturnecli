package app

import "testing"

func TestParseSafety(t *testing.T) {
	cases := []struct {
		in       string
		wantSafe bool
	}{
		{"SAFE — running the test suite is harmless", true},
		{"SAFE", true},
		{"UNSAFE: rm -rf deletes files irreversibly", false},
		{"UNSAFE", false},
		{"unsafe, this pipes the internet into a shell", false},
		{"this looks safe to me", true},
		{"I can't tell", false}, // fail closed
		{"", false},             // fail closed
	}
	for _, c := range cases {
		got, reason := parseSafety(c.in)
		if got != c.wantSafe {
			t.Errorf("parseSafety(%q) = %v, want %v (reason %q)", c.in, got, c.wantSafe, reason)
		}
	}
}

func TestParseSafetyStripsVerdictFromReason(t *testing.T) {
	_, reason := parseSafety("UNSAFE: force-push rewrites history")
	if reason == "" || reason[0] == 'U' {
		t.Errorf("expected verdict word stripped from reason, got %q", reason)
	}
}

func TestDisplayModel(t *testing.T) {
	if got := displayModel("navy:gpt-5.5"); got != "gpt-5.5" {
		t.Errorf("displayModel navy prefix = %q, want gpt-5.5", got)
	}
	if got := displayModel("gpt-5.5"); got != "gpt-5.5" {
		t.Errorf("displayModel no prefix = %q, want gpt-5.5", got)
	}
}

func TestNormalizePerm(t *testing.T) {
	cases := map[string]string{
		"":        PermAsk,
		"ask":     PermAsk,
		"smart":   PermSmart,
		"auto":    PermSmart,
		"bypass":  PermBypass,
		"yolo":    PermBypass,
		"unknown": PermAsk,
	}
	for in, want := range cases {
		if got := normalizePerm(in); got != want {
			t.Errorf("normalizePerm(%q) = %q, want %q", in, got, want)
		}
	}
}
