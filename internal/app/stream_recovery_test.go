package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDoTimeoutDoesNotBoundStreamingBodies(t *testing.T) {
	var nonStreamDeadline, streamDeadline bool
	cfg := &Config{Model: DefaultModel, BaseURL: "http://nocturne.test", APIKey: "test-key"}
	c := NewClient(cfg)
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_, hasDeadline := r.Context().Deadline()
		if r.Header.Get("Accept") == "text/event-stream" {
			streamDeadline = hasDeadline
		} else {
			nonStreamDeadline = hasDeadline
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    r,
		}, nil
	})
	if c.http.Timeout != 0 {
		t.Fatalf("shared HTTP client timeout = %v; streaming bodies must not have a whole-body timeout", c.http.Timeout)
	}

	resp, err := c.do(context.Background(), []byte(`{}`), false)
	if err != nil {
		t.Fatalf("non-stream do: %v", err)
	}
	_ = resp.Body.Close()
	resp, err = c.do(context.Background(), []byte(`{}`), true)
	if err != nil {
		t.Fatalf("stream do: %v", err)
	}
	_ = resp.Body.Close()

	if !nonStreamDeadline {
		t.Fatal("non-stream request lost its bounded context")
	}
	if streamDeadline {
		t.Fatal("streaming request unexpectedly has a whole-body deadline")
	}
}

func TestChatStreamEmitsCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer cannot flush")
			return
		}
		_, _ = fmt.Fprintln(w, `data: {"type":"delta","text":"partial"}`)
		_, _ = fmt.Fprintln(w)
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	cfg := &Config{Model: DefaultModel, BaseURL: srv.URL, APIKey: "test-key"}
	c := NewClient(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan StreamEvent, 4)
	go c.ChatStream(ctx, "system", []ChatMessage{{Role: "user", Content: "hi"}}, out)

	ev := <-out
	if ev.Err != nil || ev.Delta != "partial" {
		t.Fatalf("first event = %+v, want partial delta", ev)
	}
	cancel()
	select {
	case ev = <-out:
		if !errors.Is(ev.Err, context.Canceled) {
			t.Fatalf("post-cancel event = %+v, want context.Canceled", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not report cancellation")
	}
}

func newStreamRecoveryTestModel() *tuiModel {
	cfg := &Config{Model: DefaultModel, BaseURL: "http://127.0.0.1:1", APIKey: "test-key", Stream: false}
	return &tuiModel{
		cfg:    cfg,
		client: NewClient(cfg),
		health: newHealthTracker(),
		mode:   modeStreaming,
	}
}

func TestRecoverStreamErrorRetriesEmptyEarlyClose(t *testing.T) {
	m := newStreamRecoveryTestModel()
	cmd := m.recoverStreamError(ErrStreamClosedEarly)
	if cmd == nil {
		t.Fatal("empty early close was not retried")
	}
	if m.streamRecoveries != 1 {
		t.Fatalf("streamRecoveries = %d, want 1", m.streamRecoveries)
	}
	if len(m.messages) != 0 {
		t.Fatalf("empty retry added misleading history: %#v", m.messages)
	}
	if m.mode != modeThinking {
		t.Fatalf("mode = %v, want modeThinking", m.mode)
	}
}

func TestRecoverStreamErrorPreservesProsePartial(t *testing.T) {
	m := newStreamRecoveryTestModel()
	m.streamBuf = "partial answer"
	cmd := m.recoverStreamError(errors.New("unexpected EOF"))
	if cmd == nil {
		t.Fatal("prose partial was not continued")
	}
	if len(m.messages) != 2 {
		t.Fatalf("messages = %#v, want partial assistant + continuation user", m.messages)
	}
	if m.messages[0].Role != "assistant" || m.messages[0].Content != "partial answer" {
		t.Fatalf("partial assistant message not preserved: %#v", m.messages[0])
	}
	if m.messages[1].Role != "user" || m.messages[1].Content != streamContinuationPrompt {
		t.Fatalf("continuation prompt wrong: %#v", m.messages[1])
	}
}

func TestRecoverStreamErrorUsesIncompleteToolPrompt(t *testing.T) {
	m := newStreamRecoveryTestModel()
	m.streamBuf = "checking first\n<tool name=\"read_file\">\n{\"path\":\"x.go\""
	cmd := m.recoverStreamError(ErrStreamClosedEarly)
	if cmd == nil {
		t.Fatal("incomplete tool call was not retried")
	}
	if len(m.messages) != 2 {
		t.Fatalf("messages = %#v, want assistant partial + recovery user", m.messages)
	}
	if m.messages[0].Role != "assistant" || !strings.Contains(m.messages[0].Content, "<tool name=\"read_file\">") {
		t.Fatalf("partial tool call not preserved: %#v", m.messages[0])
	}
	if m.messages[1].Role != "user" || !strings.Contains(m.messages[1].Content, "read_file") {
		t.Fatalf("recovery prompt does not identify the tool: %#v", m.messages[1])
	}
	if len(m.pending) != 0 {
		t.Fatalf("incomplete call reached execution queue: %#v", m.pending)
	}
}

func TestRecoverStreamErrorSkipsCancellationAndHonorsCap(t *testing.T) {
	m := newStreamRecoveryTestModel()
	m.streamBuf = "partial"
	if cmd := m.recoverStreamError(context.Canceled); cmd != nil {
		t.Fatal("cancellation was incorrectly recovered")
	}
	if m.streamBuf != "partial" {
		t.Fatal("cancellation cleared the partial buffer before replyError")
	}

	m.streamRecoveries = maxStreamRecoveries
	if cmd := m.recoverStreamError(ErrStreamClosedEarly); cmd != nil {
		t.Fatal("recovery continued past the retry cap")
	}
}

func TestBusySlashStartsReplyDetection(t *testing.T) {
	m := &tuiModel{}
	if !m.slashStartsReply("/goal fix the flaky tests") {
		t.Fatal("/goal with a task should queue while busy")
	}
	if m.slashStartsReply("/goal off") {
		t.Fatal("/goal off does not start a reply")
	}
	if m.slashStartsReply("/plan") {
		t.Fatal("first /plan only enables plan mode")
	}
	m.plan = true
	if !m.slashStartsReply("/plan") {
		t.Fatal("second /plan approves and starts execution")
	}
	m.messages = []ChatMessage{{Role: "user", Content: "hi"}}
	if !m.slashStartsReply("/compact") || !m.slashStartsReply("/init") {
		t.Fatal("/compact and /init start agent requests and must queue while busy")
	}
}

func TestGoalAndPlanModesAreExclusive(t *testing.T) {
	cfg := &Config{Model: DefaultModel, Level: "extended"}
	m := &tuiModel{cfg: cfg, health: newHealthTracker(), plan: true}
	_, _ = m.runSlash("/goal", false)
	if !m.goal || m.plan {
		t.Fatalf("after /goal: goal=%v plan=%v, want goal only", m.goal, m.plan)
	}
	if status := m.statusLine(); !strings.Contains(status, "goal") {
		t.Fatalf("status line missing goal badge: %q", status)
	}

	_, _ = m.runSlash("/plan", false)
	if !m.plan || m.goal {
		t.Fatalf("after /plan: goal=%v plan=%v, want plan only", m.goal, m.plan)
	}
}
