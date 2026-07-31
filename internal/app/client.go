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
	case "gpt-4o-mini-search-preview", "gemini-3.1-pro-preview", "gemini-3.5-flash",
		"grok-4.3", "grok-4.1-fast-reasoning", "deepseek-v4-pro", "llama-4-scout",
		"mistral-medium-latest", "kimi-k2.6", "nemotron-3-super", "mimo-v2.5-pro",
		"c4ai-aya-expanse-32b", "gpt-4o", "kimi-k2.5", "qwen3.5-397b-a17b",
		"hermes-4-405b", "mistral-medium-3.5":
		id = "navy:" + id
	}

	models := map[string]ModelInfo{
		// The current public /api/ai/config no longer lists navy:gpt-5.5, but keep
		// an entry so older configs that still point at it have sane UI.
		DefaultModel: {ID: DefaultModel, Label: "GPT-5.5", Company: "OpenAI", Reasoning: true, MaxTokens: 500_000},

		"llama-3.3-70b-versatile":                   {ID: "llama-3.3-70b-versatile", Label: "Llama 3.3 70B (Versatile)", Company: "Llama", Reasoning: true, MaxTokens: 256_000, InPrice: 0.5, OutPrice: 2},
		"openai/gpt-oss-120b":                       {ID: "openai/gpt-oss-120b", Label: "GPT-OSS 120B", Company: "ChatGPT", Reasoning: true, MaxTokens: 500_000, InPrice: 0.5, OutPrice: 2},
		"qwen/qwen3-32b":                            {ID: "qwen/qwen3-32b", Label: "Qwen3 32B", Company: "Qwen", Reasoning: true, MaxTokens: 256_000, InPrice: 0.5, OutPrice: 2},
		"meta-llama/llama-4-scout-17b-16e-instruct": {ID: "meta-llama/llama-4-scout-17b-16e-instruct", Label: "Llama 4 Scout (Vision)", Company: "Llama", Vision: true, MaxTokens: 256_000, InPrice: 0.5, OutPrice: 2},

		GuardModel:                        {ID: GuardModel, Label: "ChatGPT 5.4 mini", Company: "ChatGPT", Reasoning: true, Vision: true, MaxTokens: 256_000, InPrice: 0.5, OutPrice: 2},
		VisionModel:                       {ID: VisionModel, Label: "Claude Haiku 4.5", Company: "Claude", Reasoning: true, Vision: true, MaxTokens: 256_000, InPrice: 0.5, OutPrice: 2},
		"navy:gpt-4o-mini-search-preview": {ID: "navy:gpt-4o-mini-search-preview", Label: "GPT-4o mini Search (Preview)", Company: "ChatGPT", Reasoning: true, Vision: true, MaxTokens: 256_000, InPrice: 0.5, OutPrice: 2},
		"navy:gemini-3.1-pro-preview":     {ID: "navy:gemini-3.1-pro-preview", Label: "Gemini 3.1 Pro (Preview)", Company: "Gemini", Reasoning: true, Vision: true, MaxTokens: 500_000, InPrice: 3, OutPrice: 12},
		"navy:gemini-3.5-flash":           {ID: "navy:gemini-3.5-flash", Label: "Gemini 3.5 Flash", Company: "Gemini", Vision: true, MaxTokens: 256_000, InPrice: 0.5, OutPrice: 2},
		"navy:grok-4.3":                   {ID: "navy:grok-4.3", Label: "Grok 4.3", Company: "Grok", Reasoning: true, Vision: true, MaxTokens: 500_000, InPrice: 3, OutPrice: 12},
		"navy:grok-4.1-fast-reasoning":    {ID: "navy:grok-4.1-fast-reasoning", Label: "Grok 4.1 Fast (Reasoning)", Company: "Grok", Reasoning: true, MaxTokens: 256_000, InPrice: 1, OutPrice: 4},
		"navy:deepseek-v4-pro":            {ID: "navy:deepseek-v4-pro", Label: "DeepSeek V4 Pro", Company: "DeepSeek", Reasoning: true, MaxTokens: 500_000, InPrice: 3, OutPrice: 12},
		"navy:llama-4-scout":              {ID: "navy:llama-4-scout", Label: "Llama 4 Scout", Company: "Llama", Reasoning: true, Vision: true, MaxTokens: 256_000, InPrice: 0.5, OutPrice: 2},
		"navy:mistral-medium-latest":      {ID: "navy:mistral-medium-latest", Label: "Mistral Medium", Company: "Mistral", Reasoning: true, MaxTokens: 256_000, InPrice: 1, OutPrice: 4},
		"navy:kimi-k2.6":                  {ID: "navy:kimi-k2.6", Label: "Kimi K2.6", Company: "Kimi", Reasoning: true, MaxTokens: 256_000, InPrice: 1, OutPrice: 4},
		"navy:nemotron-3-super":           {ID: "navy:nemotron-3-super", Label: "Nemotron 3 Super", Company: "Nemotron", Reasoning: true, MaxTokens: 256_000, InPrice: 0.5, OutPrice: 2},
		"navy:mimo-v2.5-pro":              {ID: "navy:mimo-v2.5-pro", Label: "MiMo V2.5 Pro", Company: "MiMo", Reasoning: true, MaxTokens: 500_000, InPrice: 3, OutPrice: 12},
		"navy:c4ai-aya-expanse-32b":       {ID: "navy:c4ai-aya-expanse-32b", Label: "Aya Expanse 32B", Company: "Cohere", Reasoning: true, MaxTokens: 256_000, InPrice: 0.5, OutPrice: 2},
		"navy:gpt-4o":                     {ID: "navy:gpt-4o", Label: "GPT-4o", Company: "ChatGPT", Reasoning: true, Vision: true, MaxTokens: 256_000, InPrice: 1, OutPrice: 4},
		"navy:kimi-k2.5":                  {ID: "navy:kimi-k2.5", Label: "Kimi K2.5", Company: "Kimi", Reasoning: true, MaxTokens: 256_000, InPrice: 1, OutPrice: 4},
		"navy:qwen3.5-397b-a17b":          {ID: "navy:qwen3.5-397b-a17b", Label: "Qwen3.5 397B A17B", Company: "Qwen", Reasoning: true, MaxTokens: 500_000, InPrice: 1, OutPrice: 4},
		"navy:hermes-4-405b":              {ID: "navy:hermes-4-405b", Label: "Hermes 4 405B", Company: "Hermes", Reasoning: true, MaxTokens: 500_000, InPrice: 1, OutPrice: 4},
		"navy:mistral-medium-3.5":         {ID: "navy:mistral-medium-3.5", Label: "Mistral Medium 3.5", Company: "Mistral", Reasoning: true, MaxTokens: 256_000, InPrice: 1, OutPrice: 4},
	}
	md, ok := models[id]
	return md, ok
}

func advertisedContextLimit(md ModelInfo) int {
	if md.MaxTokens >= 256_000 {
		return md.MaxTokens
	}
	id := strings.ToLower(normalizeModelID(md.ID))
	label := strings.ToLower(md.Label)
	name := id + " " + label
	if md.Premium || md.InPrice >= 3 || strings.Contains(name, "pro") || strings.Contains(name, "grok-4.3") || strings.Contains(name, "gpt-5.5") || strings.Contains(name, "120b") || strings.Contains(name, "397b") || strings.Contains(name, "405b") {
		return 500_000
	}
	return 256_000
}

// Client talks to the Nocturne completion endpoint.
type Client struct {
	cfg    *Config
	http   *http.Client
	vision map[string]bool // model id → supports image input
}

func NewClient(cfg *Config) *Client {
	return &Client{
		cfg: cfg,
		// No whole-request timeout here: http.Client.Timeout includes reading the
		// response body, which would cut long SSE streams off mid-reply. Non-SSE
		// requests get a bounded context in do; streams are canceled by the caller.
		http:   &http.Client{},
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

var ErrStreamClosedEarly = errors.New("stream closed before done event")

// buildBody marshals a request, prepending the system prompt as the first
// message (the endpoint ignores a top-level "system" field when "messages"
// is present).
const maxAPIMessages = 50

func (c *Client) buildBody(system string, msgs []ChatMessage, stream, fewshot bool) ([]byte, error) {
	var prefix []wireMessage
	if strings.TrimSpace(system) != "" {
		prefix = append(prefix, wireMessage{Role: "system", Content: system})
	}
	_ = fewshot

	budget := maxAPIMessages - len(prefix)
	history := c.toWire(msgs)
	keptHistory := fitWireMessages(history, budget)
	if len(history) > len(keptHistory) {
		if dbg := os.Getenv("NOCTURNE_DEBUG"); dbg != "" {
			appendDebug(dbg, "REQUEST(message-cap)", fmt.Sprintf("dropping %d old message(s); keeping %d history message(s) + %d prefix message(s) = %d total", len(history)-len(keptHistory), len(keptHistory), len(prefix), len(prefix)+len(keptHistory)))
		}
	}
	wire := append([]wireMessage(nil), prefix...)
	wire = append(wire, keptHistory...)
	req := chatRequest{
		Model:       c.cfg.Model,
		Messages:    wire,
		Stream:      stream,
		Level:       c.cfg.Level,
		Temperature: c.cfg.Temperature,
	}
	return json.Marshal(req)
}

func fitWireMessages(msgs []wireMessage, budget int) []wireMessage {
	if budget <= 0 {
		return nil
	}
	if len(msgs) <= budget {
		return msgs
	}
	return msgs[len(msgs)-budget:]
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

const (
	maxAIRequestAttempts    = 6
	nonStreamRequestTimeout = 5 * time.Minute
)

func waitForRetry(ctx context.Context, attempt int) error {
	delays := []time.Duration{
		600 * time.Millisecond,
		1200 * time.Millisecond,
		2500 * time.Millisecond,
		5 * time.Second,
		10 * time.Second,
	}
	d := delays[len(delays)-1]
	if attempt >= 0 && attempt < len(delays) {
		d = delays[attempt]
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func isTransientHTTPStatus(sc int) bool {
	return sc == http.StatusRequestTimeout || sc == http.StatusTooManyRequests || (sc >= 500 && sc != http.StatusNotImplemented && sc != http.StatusHTTPVersionNotSupported)
}

func isTransientAIText(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "temporarily unavailable") ||
		strings.Contains(text, "temporarily unavalible") ||
		strings.Contains(text, "service unavailable") ||
		strings.Contains(text, "upstream") ||
		strings.Contains(text, "overloaded") ||
		strings.Contains(text, "rate limit") ||
		strings.Contains(text, "try again")
}

func isTransientAIError(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled) && isTransientAIText(err.Error())
}

// do POSTs body, retrying transient upstream errors and network failures.
func (c *Client) do(ctx context.Context, body []byte, sse bool) (*http.Response, error) {
	if !sse {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, nonStreamRequestTimeout)
		defer cancel()
	}

	var lastErr error
	for attempt := 0; attempt < maxAIRequestAttempts; attempt++ {
		if attempt > 0 {
			if err := waitForRetry(ctx, attempt-1); err != nil {
				return nil, err
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
		if sc := resp.StatusCode; isTransientHTTPStatus(sc) {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			msg := strings.TrimSpace(string(data))
			if msg == "" {
				msg = "upstream temporarily unavailable"
			}
			lastErr = fmt.Errorf("API %d: %s", sc, oneLine(msg, 400))
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
// work continue — used for /compact.
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

	var lastErr error
	for attempt := 0; attempt < maxAIRequestAttempts; attempt++ {
		if attempt > 0 {
			if err := waitForRetry(ctx, attempt-1); err != nil {
				return nil, err
			}
		}

		resp, err := c.do(ctx, body, false)
		if err != nil {
			return nil, err
		}

		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

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
			lastErr = fmt.Errorf("API %d: %s", resp.StatusCode, oneLine(msg, 400))
			if isTransientAIError(lastErr) {
				continue
			}
			return nil, lastErr
		}

		return &ChatResult{Text: cr.Response, Usage: cr.Usage, Quota: cr.Quota}, nil
	}
	return nil, lastErr
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

	var lastErr error
	for attempt := 0; attempt < maxAIRequestAttempts; attempt++ {
		if attempt > 0 {
			if err := waitForRetry(ctx, attempt-1); err != nil {
				out <- StreamEvent{Err: err}
				return
			}
		}

		resp, err := c.do(ctx, body, true)
		if err != nil {
			lastErr = err
			if isTransientAIError(err) {
				continue
			}
			out <- StreamEvent{Err: err}
			return
		}

		if resp.StatusCode >= 400 {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("API %d: %s", resp.StatusCode, oneLine(string(data), 400))
			if isTransientAIError(lastErr) {
				continue
			}
			out <- StreamEvent{Err: lastErr}
			return
		}

		gotDone := false
		sentDelta := false
		retry := false
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
					sentDelta = true
					out <- StreamEvent{Delta: ev.Text}
				}
			case "done":
				gotDone = true
				out <- StreamEvent{Done: true, Usage: ev.Usage, Quota: ev.Quota}
			case "error":
				lastErr = fmt.Errorf("%s", firstNonEmpty(ev.Error, "stream error"))
				if !sentDelta && isTransientAIError(lastErr) {
					retry = true
					break
				}
				resp.Body.Close()
				out <- StreamEvent{Err: lastErr}
				return
			}
			if retry || gotDone {
				break
			}
		}
		resp.Body.Close()

		if gotDone {
			return
		}
		if retry {
			continue
		}
		if ctx.Err() != nil {
			out <- StreamEvent{Err: ctx.Err()}
			return
		}
		if err := sc.Err(); err != nil {
			lastErr = err
			if !sentDelta {
				continue
			}
			out <- StreamEvent{Err: err}
			return
		}
		lastErr = ErrStreamClosedEarly
		if !sentDelta {
			continue
		}
		out <- StreamEvent{Err: lastErr}
		return
	}
	if lastErr == nil {
		lastErr = ErrStreamClosedEarly
	}
	out <- StreamEvent{Err: lastErr}
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
		md := ModelInfo{
			ID: m.ID, Label: m.Label, Company: m.Company,
			Reasoning: m.Reasoning, Vision: m.Vision, Premium: m.Premium,
			MaxTokens: m.MaxTokens, InPrice: m.Pricing.In, OutPrice: m.Pricing.Out,
		}
		md.MaxTokens = advertisedContextLimit(md)
		out = append(out, md)
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
