package app

import (
	"strings"
	"testing"
)

func TestDefaultModelImageUsesTextFallback(t *testing.T) {
	cfg := &Config{Model: DefaultModel}
	client := NewClient(cfg)
	client.SetModels(nil)

	wire := client.toWire([]ChatMessage{{
		Role:    "user",
		Content: "describe",
		Images:  []Image{{MIME: "image/png", Data: []byte("png"), Desc: "a test image"}},
	}})
	content, ok := wire[0].Content.(string)
	if !ok {
		t.Fatalf("default model image content encoded as %T, want string fallback", wire[0].Content)
	}
	if !strings.Contains(content, "a test image") {
		t.Fatalf("fallback content = %q, want image description included", content)
	}
}

func TestEnsureModelsCarriesKnownVisionMetadata(t *testing.T) {
	models := ensureModels(nil, DefaultModel, VisionModel)
	if len(models) != 2 {
		t.Fatalf("ensureModels len = %d, want 2", len(models))
	}
	if models[0].ID != DefaultModel || models[0].Vision {
		t.Fatalf("default model = %#v, want non-direct-vision fallback model", models[0])
	}
	if models[1].ID != VisionModel || !models[1].Vision {
		t.Fatalf("vision model = %#v, want direct vision model", models[1])
	}
}

func TestCurrentVisionUsesDirectImageSupport(t *testing.T) {
	m := newModel(&Config{Model: DefaultModel}, "test")
	if m.currentVision() {
		t.Fatal("default model should use Haiku description fallback for images")
	}
	m.cfg.Model = VisionModel
	if !m.currentVision() {
		t.Fatal("Haiku should use direct image input")
	}
}

func TestVisionModelIDPrefersHaiku(t *testing.T) {
	m := newModel(&Config{Model: "navy:text-only"}, "test")
	m.models = []ModelInfo{
		{ID: "navy:some-other-vision", Vision: true},
		{ID: VisionModel, Vision: true},
	}
	if got := m.visionModelID(); got != VisionModel {
		t.Fatalf("visionModelID = %q, want %q", got, VisionModel)
	}
}

func TestLastMessageHasImages(t *testing.T) {
	if lastMessageHasImages(nil) {
		t.Fatal("nil messages should not have images")
	}
	if lastMessageHasImages([]ChatMessage{{Role: "user", Content: "text"}}) {
		t.Fatal("text-only message should not have images")
	}
	if !lastMessageHasImages([]ChatMessage{{Role: "user", Images: []Image{{MIME: "image/png"}}}}) {
		t.Fatal("image message should have images")
	}
}

func TestAttachmentLabelDoesNotShareInputBorderLine(t *testing.T) {
	m := newModel(&Config{Model: DefaultModel}, "test")
	m.width = 80
	m.attachments = []Image{{MIME: "image/png", Data: []byte("png")}}

	lines := strings.Split(ansiStrip(m.bottomView()), "\n")
	if len(lines) < 2 {
		t.Fatalf("bottomView rendered %d lines, want attachment line plus input box", len(lines))
	}
	if !strings.Contains(lines[0], "image attached") {
		t.Fatalf("first line = %q, want attachment label", lines[0])
	}
	if strings.Contains(lines[0], "╭") || strings.Contains(lines[0], "─") {
		t.Fatalf("attachment line contains input border: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "╭") {
		t.Fatalf("input border should start on its own line, got %q", lines[1])
	}
}
