package app

import "testing"

func TestAnswerAskLeavesAskModeBeforeContinuing(t *testing.T) {
	m := &tuiModel{
		mode: modeAsk,
		cfg:  &Config{},
		pending: []ToolCall{
			{Name: "ask", Args: map[string]any{"question": "Continue?", "options": []any{"Yes", "No"}}},
		},
		askQ:    "Continue?",
		askOpts: []string{"Yes", "No"},
		askSel:  0,
	}

	next, cmd := m.answerAsk("Yes")
	got := next.(*tuiModel)
	if got.mode == modeAsk {
		t.Fatal("answerAsk left model in modeAsk while resuming the agent loop")
	}
	if got.mode != modeThinking {
		t.Fatalf("mode after ask answer = %v, want modeThinking while continuation request starts", got.mode)
	}
	if len(got.pending) != 0 {
		t.Fatalf("pending ask was not consumed: %d pending", len(got.pending))
	}
	if len(got.results) != 0 {
		t.Fatalf("ask result should have been fed back into messages, still have %d buffered results", len(got.results))
	}
	if len(got.messages) != 1 || got.messages[0].Role != "user" {
		t.Fatalf("ask result was not appended as a user tool-result message: %#v", got.messages)
	}
	if cmd == nil {
		t.Fatal("answerAsk did not return a continuation command")
	}
}
