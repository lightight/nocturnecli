package app

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testKeyClient builds a Client pointed at srv with a dummy key set.
func testKeyClient(srv *httptest.Server) *Client {
	return NewClient(&Config{APIKey: "noct_test", BaseURL: srv.URL})
}

func TestValidateKeyAccepted(t *testing.T) {
	// A live key gets a 400 for the deliberately-empty validation POST (the
	// server rejects the payload, not the key) — anything but 401/403 passes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ai" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("missing bearer token")
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := testKeyClient(srv).ValidateKey(ctx); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
}

func TestValidateKeyRejected(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := testKeyClient(srv).ValidateKey(ctx)
		cancel()
		srv.Close()
		if !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("status %d: got %v, want ErrInvalidKey", code, err)
		}
	}
}

func TestValidateKeyUnreachableIsNotInvalid(t *testing.T) {
	// Nothing listening on this address: the error must NOT be ErrInvalidKey,
	// so startup doesn't lock out offline users.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := NewClient(&Config{APIKey: "noct_test", BaseURL: url}).ValidateKey(ctx)
	if err == nil {
		t.Fatal("expected an error for an unreachable server")
	}
	if errors.Is(err, ErrInvalidKey) {
		t.Fatalf("unreachable server reported as invalid key: %v", err)
	}
}

func TestValidateKeyOtherHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := testKeyClient(srv).ValidateKey(ctx)
	if err == nil || errors.Is(err, ErrInvalidKey) {
		t.Fatalf("500 should be a generic error, got %v", err)
	}
}

func TestLookupKey(t *testing.T) {
	for _, name := range []string{"enter", "tab", "escape", "up", "f5", "pageup", "backspace"} {
		def, ok := lookupKey(name)
		if !ok {
			t.Errorf("named key %q not found", name)
			continue
		}
		if def.macCode < 0 || def.winKey == "" || def.x11Key == "" {
			t.Errorf("named key %q has incomplete mapping: %+v", name, def)
		}
	}
	if def, ok := lookupKey("a"); !ok || def.macCode != -1 {
		t.Errorf("single char should resolve to a typed character, got %+v ok=%v", def, ok)
	}
	if _, ok := lookupKey("notakey"); ok {
		t.Error("unknown multi-char key should not resolve")
	}
}

func TestSendKeysEscape(t *testing.T) {
	got := sendKeysEscape("50%+{tab}\n")
	want := "50{%}{+}{{}tab{}}{ENTER}"
	if got != want {
		t.Errorf("sendKeysEscape: got %q want %q", got, want)
	}
}

func TestCoworkToolApproval(t *testing.T) {
	for _, name := range []string{"click", "move_mouse", "scroll", "type_text", "key_press", "open_app"} {
		if !needsApproval(name) {
			t.Errorf("%s should require approval", name)
		}
	}
	if needsApproval("screenshot") {
		t.Error("screenshot is read-only and should not require approval")
	}
}

func TestCoworkSummaries(t *testing.T) {
	tc := ToolCall{Name: "click", Args: map[string]any{"x": 100.0, "y": 200.0}}
	if got := tc.summarize(); got != "click(100, 200)" {
		t.Errorf("click summarize: %q", got)
	}
	tc = ToolCall{Name: "key_press", Args: map[string]any{"key": "enter", "modifiers": []any{"cmd"}}}
	if got := tc.summarize(); got != "key_press(cmd+enter)" {
		t.Errorf("key_press summarize: %q", got)
	}
	if got := confirmTitle("cowork"); got != "Enable cowork mode (computer use)" {
		t.Errorf("cowork confirmTitle: %q", got)
	}
}

func TestScreenshotNeedsVision(t *testing.T) {
	// With no describe callback, a non-vision model still gets a refusal.
	out, img := executeWithImage(ToolCall{Name: "screenshot", Args: map[string]any{}}, t.TempDir(), false, nil)
	if img != nil {
		t.Error("no image should be returned when the model lacks vision")
	}
	if !strings.Contains(out, "vision model") {
		t.Errorf("expected a vision-model hint, got %q", out)
	}
}

func TestScreenshotDescribedForNonVisionModel(t *testing.T) {
	// The describe callback stands in for the vision model: its text is what
	// the non-vision model receives instead of the image.
	out, img := executeWithImage(ToolCall{Name: "screenshot", Args: map[string]any{}}, t.TempDir(), false,
		func(Image) (string, error) { return "Brave window at (640, 400) showing github.com", nil })
	if img != nil {
		t.Error("described screenshots should not attach the image")
	}
	// On machines without Screen Recording permission the capture itself fails;
	// then the describe callback never runs — both paths are acceptable here.
	if strings.HasPrefix(out, "Error: screenshot failed") {
		t.Skip("no Screen Recording permission in this environment")
	}
	if !strings.Contains(out, "Brave window at (640, 400)") {
		t.Errorf("expected the description in the result, got %q", out)
	}
}

func TestShrinkScreenshot(t *testing.T) {
	// A noisy 3200×2000 capture (worst case for PNG size) must come back as a
	// compact JPEG within maxScreenshotDim, with dimensions recorded.
	rng := image.NewRGBA(image.Rect(0, 0, 3200, 2000))
	seed := uint32(42)
	for i := range rng.Pix {
		seed = seed*1664525 + 1013904223
		rng.Pix[i] = byte(seed >> 24)
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, rng); err != nil {
		t.Fatal(err)
	}
	if raw.Len() < 6*1024*1024 {
		t.Fatalf("fixture should start over the API limit, got %d bytes", raw.Len())
	}

	out := shrinkScreenshot(Image{MIME: "image/png", Data: raw.Bytes()})
	if out.MIME != "image/jpeg" {
		t.Errorf("MIME = %q, want image/jpeg", out.MIME)
	}
	if out.W != maxScreenshotDim || out.H != 980 {
		t.Errorf("dims = %d×%d, want %d×980", out.W, out.H, maxScreenshotDim)
	}
	if len(out.Data) > 4*1024*1024 {
		t.Errorf("shrunk image still too large: %d bytes", len(out.Data))
	}
	if _, err := jpeg.Decode(bytes.NewReader(out.Data)); err != nil {
		t.Errorf("output is not a valid JPEG: %v", err)
	}
}

func TestShrinkScreenshotSmall(t *testing.T) {
	small := image.NewRGBA(image.Rect(0, 0, 800, 600))
	var raw bytes.Buffer
	if err := png.Encode(&raw, small); err != nil {
		t.Fatal(err)
	}
	out := shrinkScreenshot(Image{MIME: "image/png", Data: raw.Bytes()})
	if out.W != 800 || out.H != 600 {
		t.Errorf("small image should keep its size, got %d×%d", out.W, out.H)
	}
}

func TestSystemPromptCowork(t *testing.T) {
	off := systemPromptMode("/tmp/work", false, false, false)
	if strings.Contains(off, "screenshot") || strings.Contains(off, "Cowork mode") {
		t.Error("base prompt should not mention cowork tooling")
	}
	on := systemPromptMode("/tmp/work", true, false, false)
	for _, want := range []string{"screenshot", "click", "type_text", "ENTIRE filesystem", "observe"} {
		if !strings.Contains(on, want) {
			t.Errorf("cowork prompt missing %q", want)
		}
	}
}

func TestBasePromptMentionsCoworkTool(t *testing.T) {
	// The model must know it can self-enable cowork at any time.
	p := systemPrompt("/tmp/work")
	if !strings.Contains(p, "cowork") {
		t.Error("base prompt should list the cowork tool")
	}
}
