package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Image is a binary attachment carried alongside a user message.
type Image struct {
	MIME string
	Data []byte
}

// ChatMessage is one turn of the conversation as the CLI tracks it. Images
// are only meaningful on user turns.
type ChatMessage struct {
	Role    string
	Content string
	Images  []Image
}

// Usage / Quota mirror the JSON returned by POST /api/ai.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
}

type Quota struct {
	Used      int  `json:"used"`
	Cap       int  `json:"cap"`
	Remaining int  `json:"remaining"`
	Unlimited bool `json:"unlimited"`
}

// ModelInfo describes a model the account can use (from GET /api/ai/config).
type ModelInfo struct {
	ID        string
	Label     string
	Company   string
	Reasoning bool
	Vision    bool
	Premium   bool
	MaxTokens int
	InPrice   float64 // $ per 1M input tokens
	OutPrice  float64 // $ per 1M output tokens
}

// Client talks to the Nocturne completion endpoint.
type Client struct {
	cfg    *Config
	http   *http.Client
	vision map[string]bool // model id → supports image input
}

func NewClient(cfg *Config) *Client {
	return &Client{
		cfg:    cfg,
		http:   &http.Client{Timeout: 5 * time.Minute},
		vision: map[string]bool{},
	}
}

// SetModels records which models accept image input, so toWire can choose
// between the image-parts array (vision models) and a plain string (the rest,
// which reject array content with a 400).
func (c *Client) SetModels(models []ModelInfo) {
	v := make(map[string]bool, len(models))
	for _, m := range models {
		v[m.ID] = m.Vision
	}
	c.vision = v
}

func (c *Client) supportsVision(model string) bool { return c.vision[model] }

// ChatResult is the parsed, successful response.
type ChatResult struct {
	Text  string
	Usage Usage
	Quota Quota
}

// --- wire types -----------------------------------------------------------

type wireMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string, or []contentPart when sending images
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Stream      bool          `json:"stream,omitempty"`
	Level       string        `json:"level,omitempty"`       // off · normal · extended
	Temperature *float64      `json:"temperature,omitempty"` // 0–2
}

// StreamEvent is one item emitted while streaming a reply. Exactly one of
// Delta / Done / Err is meaningful per event.
type StreamEvent struct {
	Delta string // a chunk of reply text
	Done  bool   // stream finished cleanly (Usage/Quota populated)
	Usage Usage
	Quota Quota
	Err   error
}

// buildBody marshals a request, prepending the system prompt as the first
// message (the endpoint ignores a top-level "system" field when "messages"
// is present).
// fewShot is a tiny demonstration of the read → edit → result → done flow,
// prepended to every request. It strongly cues weaker models to actually emit
// tool calls (instead of refusing or replying "Done" without acting).
var fewShot = []wireMessage{
	{Role: "user", Content: "In config.py set PORT to 80."},
	{Role: "assistant", Content: "<tool name=\"read_file\">\n{\"path\": \"config.py\"}\n</tool>"},
	{Role: "user", Content: "<tool_result name=\"read_file\">\n     1\tPORT = 8080\n</tool_result>"},
	{Role: "assistant", Content: "<tool name=\"edit_file\">\n{\"path\": \"config.py\", \"old_string\": \"PORT = 8080\", \"new_string\": \"PORT = 80\"}\n</tool>"},
	{Role: "user", Content: "<tool_result name=\"edit_file\">\nEDIT APPLIED: config.py (1 replacement).\n</tool_result>"},
	{Role: "assistant", Content: "Done — set `PORT` to 80 in `config.py`."},
}

func (c *Client) buildBody(system string, msgs []ChatMessage, stream, fewshot bool) ([]byte, error) {
	var wire []wireMessage
	if fewshot {
		wire = append(wire, fewShot...)
	}
	wire = append(wire, c.toWire(msgs)...)
	if strings.TrimSpace(system) != "" {
		wire = append([]wireMessage{{Role: "system", Content: system}}, wire...)
	}
	req := chatRequest{
		Model:       c.cfg.Model,
		Messages:    wire,
		Stream:      stream,
		Level:       c.cfg.Level,
		Temperature: c.cfg.Temperature,
	}
	return json.Marshal(req)
}

func (c *Client) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.BaseURL, "/")+"/api/ai", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	return req, nil
}

// do POSTs body, retrying up to twice on transient upstream errors
// (502/503/504) and network failures, as the API recommends for 502s.
func (c *Client) do(ctx context.Context, body []byte, sse bool) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 600 * time.Millisecond):
			}
		}
		req, err := c.newRequest(ctx, body)
		if err != nil {
			return nil, err
		}
		if sse {
			req.Header.Set("Accept", "text/event-stream")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if sc := resp.StatusCode; sc == 502 || sc == 503 || sc == 504 {
			resp.Body.Close()
			lastErr = fmt.Errorf("API %d (upstream temporarily unavailable)", sc)
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

type chatResponse struct {
	OK       bool   `json:"ok"`
	Model    string `json:"model"`
	Response string `json:"response"`
	Error    string `json:"error"`
	Message  string `json:"message"`
	Usage    Usage  `json:"usage"`
	Quota    Quota  `json:"quota"`
}

// toWire converts tracked messages to the request shape. Vision-capable models
// receive images as an OpenAI-style content-parts array with data URLs; other
// models only accept string content (an array 400s), so their images are noted
// in text instead, keeping requests valid and the model honest.
func (c *Client) toWire(msgs []ChatMessage) []wireMessage {
	vision := c.supportsVision(c.cfg.Model)
	out := make([]wireMessage, 0, len(msgs))
	for _, m := range msgs {
		if len(m.Images) == 0 {
			out = append(out, wireMessage{Role: m.Role, Content: m.Content})
			continue
		}
		if vision {
			parts := make([]contentPart, 0, len(m.Images)+1)
			if strings.TrimSpace(m.Content) != "" {
				parts = append(parts, contentPart{Type: "text", Text: m.Content})
			}
			for _, img := range m.Images {
				url := "data:" + img.MIME + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
				parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: url}})
			}
			out = append(out, wireMessage{Role: m.Role, Content: parts})
			continue
		}
		note := fmt.Sprintf("[%d image%s attached — %s has no image input, so you cannot see them]", len(m.Images), plural(len(m.Images)), c.cfg.Model)
		content := m.Content
		if strings.TrimSpace(content) == "" {
			content = note
		} else {
			content += "\n\n" + note
		}
		out = append(out, wireMessage{Role: m.Role, Content: content})
	}
	return out
}

// Chat sends the conversation and returns the model's reply.
func (c *Client) Chat(ctx context.Context, system string, msgs []ChatMessage) (*ChatResult, error) {
	return c.complete(ctx, system, msgs, true)
}

// Summarize asks the model to compress the conversation into a brief that lets
// work continue — used for /compact. The few-shot demo is omitted so it isn't
// folded into the summary.
func (c *Client) Summarize(ctx context.Context, msgs []ChatMessage) (string, error) {
	conv := append(append([]ChatMessage(nil), msgs...), ChatMessage{
		Role: "user",
		Content: "Summarize the conversation ABOVE into a concise brief that lets you continue the work " +
			"without the full history. Include: the user's goal, key decisions, files created/edited and how, " +
			"important facts learned, commands run and their outcomes, and the current state / next steps. " +
			"Use compact bullet points. Output ONLY the summary — no preamble, no tool calls.",
	})
	res, err := c.complete(ctx, "You are a precise technical note-taker.", conv, false)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// complete performs one non-streaming request and parses the reply.
func (c *Client) complete(ctx context.Context, system string, msgs []ChatMessage, fewshot bool) (*ChatResult, error) {
	if c.cfg.APIKey == "" {
		return nil, fmt.Errorf("no API key set — add NOCTURNE_API to your environment or .env, or use /key")
	}

	body, err := c.buildBody(system, msgs, false, fewshot)
	if err != nil {
		return nil, err
	}

	if dbg := os.Getenv("NOCTURNE_DEBUG"); dbg != "" {
		appendDebug(dbg, "REQUEST", string(body))
	}

	resp, err := c.do(ctx, body, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)

	if dbg := os.Getenv("NOCTURNE_DEBUG"); dbg != "" {
		appendDebug(dbg, "RESPONSE", string(data))
	}

	var cr chatResponse
	_ = json.Unmarshal(data, &cr)

	if resp.StatusCode >= 400 || (!cr.OK && cr.Response == "") {
		msg := firstNonEmpty(cr.Error, cr.Message, strings.TrimSpace(string(data)))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("API %d: %s", resp.StatusCode, oneLine(msg, 400))
	}

	return &ChatResult{Text: cr.Response, Usage: cr.Usage, Quota: cr.Quota}, nil
}

// ChatStream streams a reply, sending each StreamEvent on out and closing it
// when finished. It is meant to run in its own goroutine. The SSE payloads
// look like: data: {"type":"delta","text":"…"} … data: {"type":"done",…}.
func (c *Client) ChatStream(ctx context.Context, system string, msgs []ChatMessage, out chan<- StreamEvent) {
	defer close(out)

	if c.cfg.APIKey == "" {
		out <- StreamEvent{Err: fmt.Errorf("no API key set — add NOCTURNE_API to your environment or .env, or use /key")}
		return
	}

	body, err := c.buildBody(system, msgs, true, true)
	if err != nil {
		out <- StreamEvent{Err: err}
		return
	}
	if dbg := os.Getenv("NOCTURNE_DEBUG"); dbg != "" {
		appendDebug(dbg, "REQUEST(stream)", string(body))
	}

	resp, err := c.do(ctx, body, true)
	if err != nil {
		out <- StreamEvent{Err: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		out <- StreamEvent{Err: fmt.Errorf("API %d: %s", resp.StatusCode, oneLine(string(data), 400))}
		return
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			Error string `json:"error"`
			Usage Usage  `json:"usage"`
			Quota Quota  `json:"quota"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "delta":
			if ev.Text != "" {
				out <- StreamEvent{Delta: ev.Text}
			}
		case "done":
			out <- StreamEvent{Done: true, Usage: ev.Usage, Quota: ev.Quota}
		case "error":
			out <- StreamEvent{Err: fmt.Errorf("%s", firstNonEmpty(ev.Error, "stream error"))}
			return
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		out <- StreamEvent{Err: err}
	}
}

// FetchModels lists the models the account can use, plus the account default.
func (c *Client) FetchModels(ctx context.Context) ([]ModelInfo, string, error) {
	if c.cfg.APIKey == "" {
		return nil, "", fmt.Errorf("no API key set")
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/api/ai/config"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("API %d: %s", resp.StatusCode, oneLine(string(data), 200))
	}

	var cfg struct {
		DefaultModel string `json:"defaultModel"`
		Models       []struct {
			ID        string `json:"id"`
			Label     string `json:"label"`
			Company   string `json:"company"`
			Reasoning bool   `json:"reasoning"`
			Vision    bool   `json:"vision"`
			Premium   bool   `json:"premium"`
			MaxTokens int    `json:"maxTokens"`
			Pricing   struct {
				In  float64 `json:"inPerToken"`
				Out float64 `json:"outPerToken"`
			} `json:"pricing"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, "", err
	}

	out := make([]ModelInfo, 0, len(cfg.Models)+1)
	var canonical ModelInfo
	var haveCanonical bool
	for _, m := range cfg.Models {
		info := ModelInfo{
			ID: m.ID, Label: m.Label, Company: m.Company,
			Reasoning: m.Reasoning, Vision: m.Vision, Premium: m.Premium,
			MaxTokens: m.MaxTokens, InPrice: m.Pricing.In, OutPrice: m.Pricing.Out,
		}
		if m.ID == "navy:gpt-5.5" {
			canonical = info
			haveCanonical = true
		}
		out = append(out, info)
	}
	if haveCanonical {
		alias := canonical
		alias.ID = "gpt-5.5"
		if alias.Label != "" {
			alias.Label += " (alias)"
		} else {
			alias.Label = "alias for navy:gpt-5.5"
		}
		out = append(out, alias)
	}
	return out, cfg.DefaultModel, nil
}

// appendDebug logs a labelled blob to the file named by NOCTURNE_DEBUG.
func appendDebug(path, label, body string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "\n===== %s %s =====\n%s\n", label, time.Now().Format(time.RFC3339), body)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
