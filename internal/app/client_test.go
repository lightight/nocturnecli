package app

import (
	"encoding/json"
	"testing"
)

func TestBuildBodyCapsMessagesWithSystem(t *testing.T) {
	c := NewClient(&Config{Model: DefaultModel, BaseURL: DefaultBaseURL, Stream: true})
	msgs := make([]ChatMessage, 60)
	for i := range msgs {
		msgs[i] = ChatMessage{Role: "user", Content: "message"}
	}

	body, err := c.buildBody("system prompt", msgs, false, true)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}

	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if got := len(req.Messages); got != maxAPIMessages {
		t.Fatalf("message count = %d, want %d", got, maxAPIMessages)
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "system prompt" {
		t.Fatalf("first message = %#v, want system prompt", req.Messages[0])
	}
	wantHistory := maxAPIMessages - 1
	if got := len(req.Messages) - 1; got != wantHistory {
		t.Fatalf("history messages kept = %d, want %d", got, wantHistory)
	}
}

func TestBuildBodyCapsSummarizeMessagesWithoutFewShot(t *testing.T) {
	c := NewClient(&Config{Model: DefaultModel, BaseURL: DefaultBaseURL})
	msgs := make([]ChatMessage, 80)
	for i := range msgs {
		msgs[i] = ChatMessage{Role: "assistant", Content: "old"}
	}

	body, err := c.buildBody("summarizer", msgs, false, false)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}

	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if got := len(req.Messages); got != maxAPIMessages {
		t.Fatalf("message count = %d, want %d", got, maxAPIMessages)
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "summarizer" {
		t.Fatalf("first message = %#v, want summarizer system", req.Messages[0])
	}
}

func TestFitWireMessagesKeepsNewestMessages(t *testing.T) {
	msgs := []wireMessage{
		{Role: "user", Content: "oldest"},
		{Role: "assistant", Content: "middle"},
		{Role: "user", Content: "newest"},
	}
	got := fitWireMessages(msgs, 2)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Content != "middle" || got[1].Content != "newest" {
		t.Fatalf("kept %#v, want middle and newest", got)
	}
	if got := fitWireMessages(msgs, 0); got != nil {
		t.Fatalf("budget 0 returned %#v, want nil", got)
	}
}
