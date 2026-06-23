package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Session is a saved conversation, scoped to the directory it ran in so
// /resume only offers chats from the current project.
type Session struct {
	ID       string        `json:"id"`
	CWD      string        `json:"cwd"`
	Model    string        `json:"model"`
	Title    string        `json:"title"`
	Started  time.Time     `json:"started"`
	Updated  time.Time     `json:"updated"`
	Messages []ChatMessage `json:"messages"`
}

func sessionsDir() string { return filepath.Join(configDir(), "sessions") }

func newSessionID() string { return time.Now().Format("20060102-150405") }

// saveSession writes the session to disk (one JSON file per session). Image
// bytes are dropped to keep files small — they were already processed.
func saveSession(s Session) error {
	if len(s.Messages) == 0 {
		return nil
	}
	dir := sessionsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	s.Updated = time.Now()

	msgs := make([]ChatMessage, len(s.Messages))
	for i, m := range s.Messages {
		m.Images = nil
		msgs[i] = m
	}
	s.Messages = msgs

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, s.ID+".json"), data, 0o644)
}

// listSessions returns saved sessions for cwd, newest first.
func listSessions(cwd string) []Session {
	entries, err := os.ReadDir(sessionsDir())
	if err != nil {
		return nil
	}
	var out []Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(sessionsDir(), e.Name()))
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(data, &s) != nil || len(s.Messages) == 0 {
			continue
		}
		if cwd != "" && s.CWD != cwd {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// sessionTitle derives a short label from the first real user message.
func sessionTitle(msgs []ChatMessage) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		c := strings.TrimSpace(m.Content)
		if c == "" || strings.HasPrefix(c, "<tool_result") {
			continue
		}
		c = strings.ReplaceAll(c, "\n", " ")
		if len(c) > 60 {
			c = c[:60] + "…"
		}
		return c
	}
	return "(untitled)"
}
