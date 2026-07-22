package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Image is a binary attachment carried alongside a user message. Desc holds a
// vision-model description, filled in when the active model can't see images so
// a text stand-in can be sent instead. W/H record pixel dimensions when known
// (screenshots), so coordinate guidance can reference the delivered image size.
type Image struct {
	MIME string
	Data []byte
	Desc string
	W    int
	H    int
}

// GuardModel is the small, cheap model used to judge whether a mutating action
// is safe enough to auto-accept. VisionModel is the fallback model used to
// describe images for non-vision models when none is discovered from the API.
const (
	GuardModel  = "navy:gpt-5.4-mini"
	VisionModel = "navy:claude-haiku-4.5"
)

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

func knownModelInfo(id string) (ModelInfo, bool) {
	id = strings.TrimSpace(id)
	switch id {
	case "gpt-5.5":
		id = DefaultModel
	case "claude-haiku-4.5":
		id = VisionModel
	case "gpt-5.4-mini":
		id = GuardModel
	case "gemini-3.5-flash":
		id = "navy:gemini-3.5-flash"
	}
	switch id {
	case DefaultModel:
		return ModelInfo{ID: DefaultModel, Label: "GPT-5.5", Company: "OpenAI", Reasoning: true}, true
	case VisionModel:
		return ModelInfo{ID: VisionModel, Label: "Claude Haiku 4.5", Company: "Anthropic", Vision: true}, true
	case GuardModel:
		return ModelInfo{ID: GuardModel, Label: "GPT-5.4 Mini", Company: "OpenAI", Vision: true}, true
	case "navy:gemini-3.5-flash":
		return ModelInfo{ID: id, Label: "Gemini 3.5 Flash", Company: "Google", Vision: true}, true
	default:
		return ModelInfo{}, false
	}
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

func (c *Client) supportsVision(model string) bool {
	if normalizeModelID(model) == DefaultModel {
		return false
	}
	if c.vision[model] {
		return true
	}
	info, ok := knownModelInfo(model)
	return ok && info.Vision
}

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
		content := m.Content
		var described strings.Builder
		for i, img := range m.Images {
			if d := strings.TrimSpace(img.Desc); d != "" {
				fmt.Fprintf(&described, "\n\n[Image %d of %d — %s can't see images, so a vision model described it:]\n%s",
					i+1, len(m.Images), displayModel(c.cfg.Model), d)
			}
		}
		if described.Len() > 0 {
			content = strings.TrimSpace(content + described.String())
		} else {
			note := fmt.Sprintf("[%d image%s attached — %s has no image input, so you cannot see them]", len(m.Images), plural(len(m.Images)), displayModel(c.cfg.Model))
			if strings.TrimSpace(content) == "" {
				content = note
			} else {
				content += "\n\n" + note
			}
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
	return c.send(ctx, body)
}

// send POSTs a pre-built request body (which carries its own model) and parses
// the non-streaming reply. It backs complete as well as the auxiliary calls
// (image description, safety guard) that talk to a different model.
func (c *Client) send(ctx context.Context, body []byte) (*ChatResult, error) {
	if c.cfg.APIKey == "" {
		return nil, fmt.Errorf("no API key set — add NOCTURNE_API to your environment or .env, or use /key")
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

// visionDescribePrompt asks a vision model to transcribe an image thoroughly so
// a text-only model can reason about it as if it had seen it.
const visionDescribePrompt = `You are the "eyes" of another AI model that cannot see images. ` +
	`Describe this image in exhaustive, faithful detail so that model can fully understand it from your text alone. ` +
	`Transcribe ALL visible text verbatim (including code, labels, numbers, and error messages), ` +
	`and describe layout, UI elements, colors, diagrams, charts, and anything relevant. ` +
	`Be precise and complete; do not summarize away details. Output only the description.`

// screenshotDescribePrompt is the computer-use variant: the receiver will act
// on the screen with coordinate tools, so element positions matter as much as
// content.
const screenshotDescribePrompt = `You are the "eyes" of another AI model that is remotely controlling this computer ` +
	`through coordinate-based tools (click, type, scroll) but cannot see the screen itself. ` +
	`Describe this screenshot so it can act as if it saw it: ` +
	`name the visible apps/windows and which one is focused, transcribe important visible text verbatim ` +
	`(window titles, button labels, field contents, URLs, error messages), and describe the layout. ` +
	`For every interactive element the model might want to click (buttons, links, tabs, fields, menu items, icons), ` +
	`give its approximate CENTER as (x, y) in PIXELS relative to this image, and state the image's total pixel ` +
	`dimensions. Be precise and compact. Output only the description.`

// DescribeImages fills in a text description for each image using a vision
// model, so a non-vision model can be given the description in place of the
// image. Already-described images are left untouched. On error it returns the
// images described so far alongside the error.
func (c *Client) DescribeImages(ctx context.Context, model string, imgs []Image) ([]Image, error) {
	out := make([]Image, len(imgs))
	copy(out, imgs)
	for i := range out {
		if strings.TrimSpace(out[i].Desc) != "" {
			continue
		}
		desc, err := c.describeOne(ctx, model, out[i])
		if err != nil {
			return out, err
		}
		out[i].Desc = desc
	}
	return out, nil
}

func (c *Client) describeOne(ctx context.Context, model string, img Image) (string, error) {
	return c.describeOnePrompt(ctx, model, visionDescribePrompt, img)
}

// DescribeScreenshot renders a screen capture as text for a non-vision model,
// using the computer-use prompt that includes element coordinates.
func (c *Client) DescribeScreenshot(ctx context.Context, model string, img Image) (string, error) {
	return c.describeOnePrompt(ctx, model, screenshotDescribePrompt, img)
}

func (c *Client) describeOnePrompt(ctx context.Context, model, prompt string, img Image) (string, error) {
	url := "data:" + img.MIME + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
	parts := []contentPart{
		{Type: "text", Text: prompt},
		{Type: "image_url", ImageURL: &imageURL{URL: url}},
	}
	req := chatRequest{Model: model, Messages: []wireMessage{{Role: "user", Content: parts}}}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	res, err := c.send(ctx, body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Text), nil
}

// guardSystemPrompt instructs the small guard model to classify an action.
const guardSystemPrompt = `You are a safety gate for an autonomous coding agent working inside a developer's project. ` +
	`Given a proposed action (a file edit/write/delete/rename, or a shell command), decide whether it is safe to run ` +
	`AUTOMATICALLY without asking the human. Reply with exactly one word on the first line — SAFE or UNSAFE — then a short reason.

Treat as UNSAFE anything destructive, irreversible, or dangerous: rm -rf or bulk deletion, deleting files you did not just create, ` +
	`overwriting or writing outside the project, git push --force / history rewrites, piping the internet into a shell (curl … | sh), ` +
	`sudo, chmod/chown on system paths, disk/partition operations, killing processes, editing secrets/credentials or .env, ` +
	`sending data to remote hosts, or anything you are unsure about.

Treat as SAFE ordinary local development: creating or editing source/config files inside the project, running builds, tests, linters, ` +
	`formatters, git status/add/commit/diff/log, listing or reading files, installing declared dependencies. When in doubt, answer UNSAFE.`

// CheckSafety asks the guard model whether an action can be auto-accepted. It
// returns (safe, reason, err); on any transport error the caller should fail
// closed (prompt the human).
func (c *Client) CheckSafety(ctx context.Context, model, action, contextInfo string) (bool, string, error) {
	if strings.TrimSpace(contextInfo) == "" {
		contextInfo = "(none provided)"
	}
	user := "The user's current goal: " + oneLine(contextInfo, 400) +
		"\n\nProposed action:\n" + action +
		"\n\nIs this safe to run automatically? Answer SAFE or UNSAFE, then a brief reason."
	req := chatRequest{Model: model, Messages: []wireMessage{
		{Role: "system", Content: guardSystemPrompt},
		{Role: "user", Content: user},
	}}
	body, err := json.Marshal(req)
	if err != nil {
		return false, "", err
	}
	res, err := c.send(ctx, body)
	if err != nil {
		return false, "", err
	}
	safe, reason := parseSafety(res.Text)
	return safe, reason, nil
}

// parseSafety reads the guard model's verdict. It fails closed: only an explicit
// SAFE verdict returns true.
func parseSafety(text string) (bool, string) {
	t := strings.TrimSpace(text)
	upper := strings.ToUpper(t)
	reason := oneLine(strings.TrimLeft(t, " \n"), 200)
	// Strip a leading verdict word from the reason for a tidier message.
	if i := strings.IndexAny(reason, ".:—-\n "); i > 0 {
		if w := strings.ToUpper(reason[:i]); w == "SAFE" || w == "UNSAFE" {
			reason = strings.TrimSpace(strings.TrimLeft(reason[i:], ".:—- "))
		}
	}
	switch {
	case strings.HasPrefix(upper, "UNSAFE"):
		return false, reason
	case strings.HasPrefix(upper, "SAFE"):
		return true, reason
	case strings.Contains(upper, "UNSAFE"):
		return false, reason
	case strings.Contains(upper, "SAFE"):
		return true, reason
	}
	return false, reason
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

// ErrInvalidKey marks a key the server actively rejected (401/403) — expired
// or unknown — as opposed to a network failure, which leaves the key unproven.
var ErrInvalidKey = errors.New("invalid or expired API key")

// NewKeyURL is where users create a replacement API key.
const NewKeyURL = "https://nocturne.lol/account"

// ValidateKey checks the configured API key against the completion endpoint —
// the only route that actually authenticates (the GET account routes answer
// anonymously for any key). It POSTs a deliberately empty request: the server
// rejects a dead key with 401 before ever looking at the body, and answers a
// live key with a 400 for the empty payload, so validating costs no tokens.
// It returns ErrInvalidKey when the server rejects the key (401/403), a
// generic error for server failures, and the transport error unchanged when
// the server can't be reached — callers can tell "rejected" apart from
// "couldn't check" with errors.Is(err, ErrInvalidKey).
func (c *Client) ValidateKey(ctx context.Context) error {
	if c.cfg.APIKey == "" {
		return fmt.Errorf("no API key set")
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/api/ai"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrInvalidKey
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 500 {
		return fmt.Errorf("API %d: server error while checking the key", resp.StatusCode)
	}
	return nil
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

	out := make([]ModelInfo, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		out = append(out, ModelInfo{
			ID: m.ID, Label: m.Label, Company: m.Company,
			Reasoning: m.Reasoning, Vision: m.Vision, Premium: m.Premium,
			MaxTokens: m.MaxTokens, InPrice: m.Pricing.In, OutPrice: m.Pricing.Out,
		})
	}
	return out, cfg.DefaultModel, nil
}

// UsageStats mirrors GET /api/ai/usage — lifetime and rolling-week token spend.
type UsageStats struct {
	Authenticated bool    `json:"authenticated"`
	WeekTokens    int64   `json:"weekTokens"`
	WeekCost      float64 `json:"weekCost"`
	AllTokens     int64   `json:"allTokens"`
	AllCost       float64 `json:"allCost"`
}

// Credits mirrors GET /api/ai/credits — remaining balance and the daily cap.
type Credits struct {
	Authenticated bool    `json:"authenticated"`
	Unlimited     bool    `json:"unlimited"`
	Dollars       float64 `json:"dollars"`
	Daily         struct {
		Used    int64 `json:"used"`
		Cap     int64 `json:"cap"`
		ResetAt int64 `json:"resetAt"` // unix millis
	} `json:"daily"`
}

// getJSON performs an authenticated GET and decodes the JSON body into v.
func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	if c.cfg.APIKey == "" {
		return fmt.Errorf("no API key set")
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 429 {
		return fmt.Errorf("rate limited — try again in a moment")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("API %d: %s", resp.StatusCode, oneLine(string(data), 160))
	}
	return json.Unmarshal(data, v)
}

// FetchUsage returns lifetime/weekly token usage for the account.
func (c *Client) FetchUsage(ctx context.Context) (*UsageStats, error) {
	var u UsageStats
	if err := c.getJSON(ctx, "/api/ai/usage", &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// FetchCredits returns the remaining balance and daily quota for the account.
func (c *Client) FetchCredits(ctx context.Context) (*Credits, error) {
	var cr Credits
	if err := c.getJSON(ctx, "/api/ai/credits", &cr); err != nil {
		return nil, err
	}
	return &cr, nil
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
