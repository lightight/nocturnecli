package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

type mode int

const (
	modeInput mode = iota
	modeThinking
	modeStreaming
	modeConfirm
	modePicker
	modeResume
)

// autoCompactThreshold is the context size (tokens) at which the conversation
// is summarised automatically, to stay clear of the ~1M ceiling.
const autoCompactThreshold = 900_000

// maxInputRows caps how tall the input box can grow as text wraps.
const maxInputRows = 10

// slashItem is one entry in the "/" command menu.
type slashItem struct {
	name string
	desc string
}

var slashCommands = []slashItem{
	{"/help", "show commands"},
	{"/model", "pick a model (or /model <id>)"},
	{"/models", "list available models"},
	{"/level", "thinking: off · normal · extended"},
	{"/key", "set & save your API key"},
	{"/paste", "attach an image from the clipboard"},
	{"/image", "attach an image file"},
	{"/auto", "toggle auto-accept for edits & commands"},
	{"/stream", "toggle live response streaming"},
	{"/mouse", "toggle wheel-scroll vs terminal text selection"},
	{"/cd", "change the working directory"},
	{"/tokens", "show token usage & quota"},
	{"/compact", "summarize the conversation to free up context"},
	{"/resume", "resume a saved chat from this directory"},
	{"/new", "start a new chat"},
	{"/remote", "control this session from your browser (paired, e2e-encrypted)"},
	{"/clear", "clear the conversation"},
	{"/init", "generate a NOCTURNE.md for the project"},
	{"/update", "update Nocturne to the latest release"},
	{"/exit", "quit"},
}

// tuiModel is the full-screen bubbletea program state.
type tuiModel struct {
	cfg    *Config
	client *Client
	work   string
	ver    string

	ta textarea.Model
	vp viewport.Model
	sp spinner.Model
	rd *glamour.TermRenderer

	width, height int
	ready         bool
	follow        bool // keep the transcript pinned to the bottom
	mouseOff      bool // user disabled mouse capture (restores text selection)

	mode     mode
	spinning bool

	lines       []string // transcript blocks (the scrollback)
	messages    []ChatMessage
	autoAccept  bool
	attachments []Image

	pending []ToolCall
	results []toolResult
	confirm ToolCall

	streamCh  chan StreamEvent
	streamBuf string

	cancel  context.CancelFunc
	started time.Time

	lastQuota Quota
	tokens    int

	// "/" command menu
	showSlash    bool
	slashMatches []slashItem
	slashSel     int

	// model picker
	models    []ModelInfo
	modelsDef string
	pickerSel int

	// compaction & sessions
	ctxTokens    int    // approx current context size, for auto-compaction
	compacting   bool   // a compaction request is in flight
	sessionID    string // file id for the current saved session
	sessTitle    string // stable title (first real user message), kept across compaction
	sessionStart time.Time
	sessions     []Session // loaded for the /resume picker
	resumeSel    int

	program *tea.Program // set after creation, for cross-goroutine Send
	remote  *remoteHub   // the /remote bridge, when running

	quitting bool
}

// currentVision reports whether the selected model can see attached images.
func (m *tuiModel) currentVision() bool {
	for _, md := range m.models {
		if md.ID == m.cfg.Model {
			return md.Vision
		}
	}
	return false
}

// --- messages --------------------------------------------------------------

type apiRespMsg struct {
	text  string
	usage Usage
	quota Quota
	err   error
}

type toolDoneMsg struct {
	name   string
	output string
}

type imageGrabbedMsg struct {
	img Image
	err error
}

type streamDeltaMsg struct{ ev StreamEvent }

type updateDoneMsg struct {
	text string
	err  error
}

type modelsLoadedMsg struct {
	models []ModelInfo
	def    string
	err    error
	action string // "" (cache only), "picker", or "list"
}

type compactDoneMsg struct {
	summary string
	auto    bool
	err     error
}

// --- lifecycle -------------------------------------------------------------

func newModel(cfg *Config, version string) *tuiModel {
	work, _ := os.Getwd()

	ta := textarea.New()
	ta.Placeholder = "Ask Nocturne to build, fix, explain…  (type / for commands)"
	ta.Prompt = "› " // plain so the textarea's width math is correct; styled below
	ta.FocusedStyle.Prompt = stUser
	ta.BlurredStyle.Prompt = stUser
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(maxInputRows) // tall internally so it never scrolls; we trim blank rows when drawing
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = stAccent

	m := &tuiModel{
		cfg: cfg, client: NewClient(cfg), work: work, ver: version,
		ta: ta, sp: sp, vp: viewport.New(0, 0), mode: modeInput, follow: true,
		sessionID: newSessionID(), sessionStart: time.Now(),
	}
	m.rebuildRenderer()
	return m
}

// oscReportLeak matches a terminal colour report (OSC 10/11/12 reply) that has
// leaked into the key stream. bubbletea queries the background colour at
// startup, and on terminals that answer slowly (e.g. macOS Terminal.app) the
// reply arrives after the parser's window. It surfaces as three key events —
// "alt+]", the body "11;rgb:213d/2743/33e7", then "alt+\" — which we drop here
// before they reach the UI. (Plain "]" / "\" typing is unaffected: the leaked
// delimiters carry the Alt modifier, real typing doesn't.)
var oscReportLeak = regexp.MustCompile(`\d{1,3};rgb:[0-9a-fA-F/]+`)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

func ansiStrip(s string) string { return ansiRe.ReplaceAllString(s, "") }

func filterLeaks(_ tea.Model, msg tea.Msg) tea.Msg {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return msg
	}
	s := string(k.Runes)
	if oscReportLeak.MatchString(s) {
		return nil
	}
	if k.Alt && (s == "]" || s == `\`) {
		return nil
	}
	return msg
}

func startTUI(cfg *Config, version string) error {
	// Pin the colour profile (from env) and a dark background up front so
	// lipgloss/termenv never *query* the terminal themselves.
	lipgloss.SetColorProfile(termenv.EnvColorProfile())
	lipgloss.SetHasDarkBackground(true)

	m := newModel(cfg, version)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithFilter(filterLeaks))
	m.program = p
	_, err := p.Run()
	m.remote.Stop()
	return err
}

func (m *tuiModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.fetchModelsCmd(""))
}

func (m *tuiModel) fetchModelsCmd(action string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		models, def, err := client.FetchModels(ctx)
		return modelsLoadedMsg{models: models, def: def, err: err, action: action}
	}
}

// --- update ----------------------------------------------------------------

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ta.SetWidth(msg.Width - 6)
		m.rebuildRenderer()
		if !m.ready {
			m.ready = true
			m.greet()
		}
		m.syncViewport()
		return m, nil

	case spinner.TickMsg:
		if !m.busy() {
			m.spinning = false
			return m, nil
		}
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		m.syncViewport() // refresh streamed-text tail / elapsed
		return m, cmd

	case apiRespMsg:
		return m.handleAPIResp(msg)

	case streamDeltaMsg:
		return m.handleStreamDelta(msg)

	case toolDoneMsg:
		return m.handleToolDone(msg)

	case imageGrabbedMsg:
		if msg.err != nil {
			m.push(stErr.Render("  ✗ " + msg.err.Error()))
		} else {
			m.attachments = append(m.attachments, msg.img)
			m.push(stOK.Render(fmt.Sprintf("  📎 image attached (%d KB) — add a message and press enter", len(msg.img.Data)/1024)))
		}
		return m, nil

	case updateDoneMsg:
		if msg.err != nil {
			m.push(stErr.Render("  ✗ update: " + msg.err.Error()))
		} else {
			m.push(stOK.Render("  ⟳ " + msg.text))
		}
		return m, nil

	case modelsLoadedMsg:
		if msg.err != nil {
			if msg.action != "" {
				m.push(stErr.Render("  ✗ models: " + msg.err.Error()))
			}
			return m, nil
		}
		// gpt-5.5 (the default) and any custom current model aren't in the
		// published list, so add them up front so they're always selectable.
		m.models = ensureModels(msg.models, DefaultModel, m.cfg.Model)
		m.modelsDef = msg.def
		m.client.SetModels(m.models)
		switch msg.action {
		case "picker":
			m.openPicker()
		case "list":
			m.push(m.modelsList())
		}
		return m, nil

	case compactDoneMsg:
		return m.handleCompactDone(msg)

	case remoteSubmitMsg:
		return m.handleRemoteSubmit(msg.text)

	case tea.MouseMsg:
		// Route the mouse wheel to the transcript so scrolling up reveals
		// earlier output (the alt-screen has no native scrollback).
		if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			m.follow = m.vp.AtBottom()
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *tuiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Scroll the transcript in any mode; track whether we're pinned to bottom.
	switch msg.Type {
	case tea.KeyPgUp, tea.KeyPgDown:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		m.follow = m.vp.AtBottom()
		return m, cmd
	}

	switch m.mode {
	case modeThinking, modeStreaming:
		switch msg.Type {
		case tea.KeyCtrlC:
			m.quitting = true
			return m, tea.Quit
		case tea.KeyEsc:
			if m.cancel != nil {
				m.cancel()
			}
		}
		return m, nil
	case modeConfirm:
		return m.handleConfirmKey(msg)
	case modePicker:
		return m.handlePickerKey(msg)
	case modeResume:
		return m.handleResumeKey(msg)
	default:
		return m.handleInputKey(msg)
	}
}

func (m *tuiModel) openPicker() {
	m.mode = modePicker
	m.pickerSel = 0
	for i, md := range m.models {
		if md.ID == m.cfg.Model {
			m.pickerSel = i
			break
		}
	}
}

func (m *tuiModel) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.models)
	if n == 0 {
		m.mode = modeInput
		return m, nil
	}
	switch msg.Type {
	case tea.KeyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case tea.KeyEsc:
		m.mode = modeInput
	case tea.KeyUp:
		m.pickerSel = (m.pickerSel - 1 + n) % n
	case tea.KeyDown:
		m.pickerSel = (m.pickerSel + 1) % n
	case tea.KeyEnter:
		id := normalizeModelID(m.models[m.pickerSel].ID)
		m.cfg.Model = id
		_ = m.cfg.Save()
		m.mode = modeInput
		m.push(stOK.Render("  model set to " + stAccent.Render(id)))
	}
	return m, nil
}

func (m *tuiModel) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Image paste: a bracketed paste that is just an image path → attach it.
	if msg.Paste {
		if img, ok := tryPasteImage(string(msg.Runes), m.work); ok {
			m.attachments = append(m.attachments, img)
			m.push(stOK.Render("  📎 image attached"))
			return m, nil
		}
	}

	// "/" menu navigation.
	if m.showSlash {
		switch msg.Type {
		case tea.KeyUp:
			m.slashSel = (m.slashSel - 1 + len(m.slashMatches)) % len(m.slashMatches)
			return m, nil
		case tea.KeyDown:
			m.slashSel = (m.slashSel + 1) % len(m.slashMatches)
			return m, nil
		case tea.KeyTab:
			m.ta.SetValue(m.slashMatches[m.slashSel].name + " ")
			m.ta.CursorEnd()
			m.refreshSlash()
			return m, nil
		case tea.KeyEsc:
			m.showSlash = false
			return m, nil
		case tea.KeyEnter:
			if !msg.Alt {
				m.ta.SetValue(m.slashMatches[m.slashSel].name)
				return m.submit()
			}
		}
	}

	switch msg.Type {
	case tea.KeyCtrlC:
		if strings.TrimSpace(m.ta.Value()) != "" {
			m.ta.Reset()
			m.refreshSlash()
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit
	case tea.KeyCtrlD:
		m.quitting = true
		return m, tea.Quit
	case tea.KeyCtrlV:
		return m, grabImageCmd()
	case tea.KeyEnter:
		if msg.Alt {
			m.ta.InsertString("\n")
			return m, nil
		}
		return m.submit()
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.refreshSlash()
	return m, cmd
}

func (m *tuiModel) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		m.quitting = true
		return m, tea.Quit
	}
	switch strings.ToLower(msg.String()) {
	case "y", "enter":
		return m.approve(false)
	case "a":
		return m.approve(true)
	case "n", "esc":
		return m.deny()
	}
	return m, nil
}

// refreshSlash recomputes the "/" menu from the current input.
func (m *tuiModel) refreshSlash() {
	val := m.ta.Value()
	if strings.HasPrefix(val, "/") && !strings.ContainsAny(val, " \n") {
		m.slashMatches = filterSlash(val)
		m.showSlash = len(m.slashMatches) > 0
		if m.slashSel >= len(m.slashMatches) {
			m.slashSel = 0
		}
	} else {
		m.showSlash = false
		m.slashSel = 0
	}
}

func filterSlash(prefix string) []slashItem {
	prefix = strings.ToLower(prefix)
	var out []slashItem
	for _, c := range slashCommands {
		if strings.HasPrefix(c.name, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// --- submitting ------------------------------------------------------------

func (m *tuiModel) submit() (tea.Model, tea.Cmd) {
	raw := strings.TrimSpace(m.ta.Value())
	m.ta.Reset()
	m.showSlash = false
	m.follow = true // snap back to the bottom on a new action

	if raw == "" && len(m.attachments) == 0 {
		return m, nil
	}
	if strings.HasPrefix(raw, "/") {
		return m.runSlash(raw)
	}

	text, inline := extractInlineImages(raw, m.work)
	imgs := append(m.attachments, inline...)
	m.attachments = nil

	m.messages = append(m.messages, ChatMessage{Role: "user", Content: text, Images: imgs})
	m.noteTitle(text)
	m.push(renderUser(text, len(imgs)))
	m.toRemote("user", text)
	if len(imgs) > 0 && !m.currentVision() {
		m.push(stHint.Render("  note: " + m.cfg.Model + " can't see images — pick a vision model with /model (e.g. navy:claude-haiku-4.5)"))
	}
	return m, m.startReply()
}

func (m *tuiModel) submitText(text string) (tea.Model, tea.Cmd) {
	m.follow = true
	m.messages = append(m.messages, ChatMessage{Role: "user", Content: text})
	m.noteTitle(text)
	m.push(renderUser(text, 0))
	return m, m.startReply()
}

// noteTitle records the first real user message as the session's stable title.
func (m *tuiModel) noteTitle(text string) {
	if m.sessTitle != "" {
		return
	}
	t := strings.ReplaceAll(strings.TrimSpace(text), "\n", " ")
	if len([]rune(t)) > 60 {
		t = string([]rune(t)[:60]) + "…"
	}
	m.sessTitle = t
}

func (m *tuiModel) busy() bool { return m.mode == modeThinking || m.mode == modeStreaming }

func (m *tuiModel) startReply() tea.Cmd {
	m.started = time.Now()
	if m.cfg.Stream {
		return m.startStream()
	}
	m.mode = modeThinking
	return tea.Batch(m.startSpinner(), m.callAPICmd())
}

func (m *tuiModel) startStream() tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mode = modeStreaming
	m.streamBuf = ""
	ch := make(chan StreamEvent, 128)
	m.streamCh = ch
	go m.client.ChatStream(ctx, systemPrompt(m.work), append([]ChatMessage(nil), m.messages...), ch)
	return tea.Batch(m.startSpinner(), waitDelta(ch))
}

func waitDelta(ch chan StreamEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return streamDeltaMsg{ev: StreamEvent{Done: true}}
		}
		return streamDeltaMsg{ev: ev}
	}
}

func (m *tuiModel) callAPICmd() tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	system := systemPrompt(m.work)
	msgs := append([]ChatMessage(nil), m.messages...)
	client := m.client
	return func() tea.Msg {
		res, err := client.Chat(ctx, system, msgs)
		cancel()
		if err != nil {
			return apiRespMsg{err: err}
		}
		return apiRespMsg{text: res.Text, usage: res.Usage, quota: res.Quota}
	}
}

func grabImageCmd() tea.Cmd {
	return func() tea.Msg {
		img, err := grabClipboardImage()
		return imageGrabbedMsg{img: img, err: err}
	}
}

func (m *tuiModel) runToolCmd(tc ToolCall) tea.Cmd {
	work := m.work
	return func() tea.Msg {
		return toolDoneMsg{name: tc.Name, output: execute(tc, work)}
	}
}

func (m *tuiModel) startSpinner() tea.Cmd {
	if m.spinning {
		return nil
	}
	m.spinning = true
	return m.sp.Tick
}

// --- agent loop ------------------------------------------------------------

func (m *tuiModel) handleAPIResp(msg apiRespMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.replyError(msg.err)
		return m, nil
	}
	return m.finishReply(msg.text, msg.usage, msg.quota)
}

func (m *tuiModel) handleStreamDelta(msg streamDeltaMsg) (tea.Model, tea.Cmd) {
	ev := msg.ev
	if ev.Err != nil {
		m.replyError(ev.Err)
		return m, nil
	}
	if ev.Done {
		return m.finishReply(m.streamBuf, ev.Usage, ev.Quota)
	}
	m.streamBuf += ev.Delta
	m.syncViewport()
	if m.remote != nil {
		m.toRemote("stream", cleanToolStream(m.streamBuf, "[preparing tool call…]"))
	}
	return m, waitDelta(m.streamCh)
}

func (m *tuiModel) replyError(err error) {
	m.cancel = nil
	m.streamCh = nil
	m.streamBuf = ""
	m.mode = modeInput
	if errors.Is(err, context.Canceled) {
		m.push(stHint.Render("  ✗ interrupted"))
		m.toRemote("status", "interrupted")
		return
	}
	m.push(stErr.Render("  ✗ " + err.Error()))
	m.toRemote("status", "✗ "+err.Error())
}

func (m *tuiModel) finishReply(text string, usage Usage, quota Quota) (tea.Model, tea.Cmd) {
	m.cancel = nil
	m.streamCh = nil
	m.streamBuf = ""

	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	m.tokens += usage.TotalTokens
	if quota.Cap > 0 {
		m.lastQuota = quota
	}
	// inputTokens reflects the size of the context we just sent; add this
	// turn's output to estimate the live context size for auto-compaction.
	if usage.InputTokens > 0 {
		m.ctxTokens = usage.InputTokens + usage.OutputTokens
	}
	m.messages = append(m.messages, ChatMessage{Role: "assistant", Content: text})

	narration, calls := parseResponse(text)

	if len(calls) == 0 {
		m.mode = modeInput
		if out := m.renderAssistant(narration); out != "" {
			m.push(out)
		}
		m.toRemote("assistant", narration)
		m.persistSession()
		if m.ctxTokens >= autoCompactThreshold && !m.compacting {
			return m, m.compactCmd(true)
		}
		return m, nil
	}

	if narration != "" {
		m.push(m.renderAssistant(narration))
		m.toRemote("stream", narration)
	}
	m.pending = calls
	m.results = nil
	return m, m.advanceTools()
}

func (m *tuiModel) advanceTools() tea.Cmd {
	if len(m.pending) == 0 {
		if len(m.results) == 0 {
			m.mode = modeInput
			return nil
		}
		m.messages = append(m.messages, ChatMessage{Role: "user", Content: buildToolResults(m.results)})
		m.results = nil
		return m.startReply()
	}

	tc := m.pending[0]
	m.push(renderToolCall(tc))
	m.toRemote("tool", "● "+tc.summarize())

	if needsApproval(tc.Name) && !m.autoAccept {
		m.mode = modeConfirm
		m.confirm = tc
		m.toRemote("status", "waiting for approval in the terminal: "+tc.summarize())
		return nil
	}

	m.pending = m.pending[1:]
	m.mode = modeThinking
	return tea.Batch(m.startSpinner(), m.runToolCmd(tc))
}

func (m *tuiModel) handleToolDone(msg toolDoneMsg) (tea.Model, tea.Cmd) {
	m.results = append(m.results, toolResult{Name: msg.name, Output: msg.output})
	m.push(renderToolResult(msg.output))
	m.toRemote("tool", "  └ "+oneLine(firstLine(msg.output), 100))
	return m, m.advanceTools()
}

// --- compaction ------------------------------------------------------------

func (m *tuiModel) compactCmd(auto bool) tea.Cmd {
	m.compacting = true
	m.mode = modeThinking
	m.started = time.Now()
	if auto {
		m.push(stHint.Render("  ⓘ context is getting large — compacting automatically…"))
	} else {
		m.push(stHint.Render("  compacting conversation…"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	msgs := append([]ChatMessage(nil), m.messages...)
	client := m.client
	return tea.Batch(m.startSpinner(), func() tea.Msg {
		summary, err := client.Summarize(ctx, msgs)
		cancel()
		return compactDoneMsg{summary: summary, auto: auto, err: err}
	})
}

func (m *tuiModel) handleCompactDone(msg compactDoneMsg) (tea.Model, tea.Cmd) {
	m.compacting = false
	m.cancel = nil
	m.mode = modeInput
	if msg.err != nil {
		if errors.Is(msg.err, context.Canceled) {
			m.push(stHint.Render("  ✗ compaction interrupted"))
		} else {
			m.push(stErr.Render("  ✗ compact: " + msg.err.Error()))
		}
		return m, nil
	}
	before := m.ctxTokens
	m.messages = []ChatMessage{{
		Role:    "user",
		Content: "Summary of the conversation so far (the full history was compacted to save context):\n\n" + msg.summary,
	}}
	m.ctxTokens = 0
	// Keep the summary only in the API context — don't print it to the user.
	m.push(stOK.Render("  ✓ compacted") + stDim.Render(fmt.Sprintf("  (context was ~%s tokens, now summarized — history preserved above)", commas(before))))
	m.persistSession()
	return m, nil
}

// --- sessions / resume -----------------------------------------------------

func (m *tuiModel) persistSession() {
	if len(m.messages) == 0 {
		return
	}
	title := m.sessTitle
	if title == "" {
		title = sessionTitle(m.messages)
	}
	_ = saveSession(Session{
		ID:       m.sessionID,
		CWD:      m.work,
		Model:    m.cfg.Model,
		Title:    title,
		Started:  m.sessionStart,
		Updated:  time.Now(),
		Messages: m.messages,
	})
}

func (m *tuiModel) openResume() (tea.Model, tea.Cmd) {
	m.sessions = listSessions(m.work)
	// Don't offer the session we're currently in.
	filtered := m.sessions[:0]
	for _, s := range m.sessions {
		if s.ID != m.sessionID {
			filtered = append(filtered, s)
		}
	}
	m.sessions = filtered
	if len(m.sessions) == 0 {
		m.push(stHint.Render("  no saved chats in this directory yet"))
		return m, nil
	}
	m.mode = modeResume
	m.resumeSel = 0
	return m, nil
}

func (m *tuiModel) handleResumeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.sessions)
	if n == 0 {
		m.mode = modeInput
		return m, nil
	}
	switch msg.Type {
	case tea.KeyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case tea.KeyEsc:
		m.mode = modeInput
	case tea.KeyUp:
		m.resumeSel = (m.resumeSel - 1 + n) % n
	case tea.KeyDown:
		m.resumeSel = (m.resumeSel + 1) % n
	case tea.KeyEnter:
		m.restoreSession(m.sessions[m.resumeSel])
	}
	return m, nil
}

func (m *tuiModel) restoreSession(s Session) {
	m.mode = modeInput
	m.messages = s.Messages
	m.sessionID = s.ID
	m.sessionStart = s.Started
	m.sessTitle = s.Title
	m.ctxTokens = 0
	if s.Model != "" {
		m.cfg.Model = s.Model
	}

	m.lines = nil
	m.greet()
	m.push(stOK.Render("  ✓ resumed: ") + stDim.Render(s.Title))
	for _, msg := range s.Messages {
		switch msg.Role {
		case "user":
			c := strings.TrimSpace(msg.Content)
			if c == "" || strings.HasPrefix(c, "<tool_result") {
				continue
			}
			m.push(renderUser(c, len(msg.Images)))
		case "assistant":
			if narr, _ := parseResponse(msg.Content); narr != "" {
				m.push(m.renderAssistant(narr))
			}
		}
	}
}

// --- remote control --------------------------------------------------------

// toRemote pushes a semantic event to any connected browser.
func (m *tuiModel) toRemote(kind, text string) {
	if m.remote != nil && strings.TrimSpace(text) != "" {
		m.remote.broadcast(remoteEvent{Kind: kind, Text: text})
	}
}

// handleRemoteSubmit processes a message that arrived from the browser.
func (m *tuiModel) handleRemoteSubmit(text string) (tea.Model, tea.Cmd) {
	text = strings.TrimSpace(text)
	if text == "" {
		return m, nil
	}
	if strings.HasPrefix(text, "/") {
		m.toRemote("status", "slash commands only work in the terminal")
		return m, nil
	}
	if m.busy() {
		m.toRemote("status", "busy — wait for the current reply to finish")
		return m, nil
	}
	m.follow = true
	m.messages = append(m.messages, ChatMessage{Role: "user", Content: text})
	m.noteTitle(text)
	m.push(renderUser(text, 0) + " " + stDim.Render("(remote)"))
	m.toRemote("user", text)
	return m, m.startReply()
}

func (m *tuiModel) remoteInfo() string {
	h := m.remote
	var b strings.Builder
	b.WriteString(stTitle.Render("  Remote control") + stDim.Render("  · end-to-end encrypted") + "\n")
	b.WriteString("  open  " + stAccent.Render(h.url) + "\n")
	b.WriteString("  code  " + stTitle.Render(h.code) + "\n")
	b.WriteString(stDim.Render("  Open the link on any device, anywhere, and enter the code to pair.\n"))
	b.WriteString(stDim.Render("  Everything is end-to-end encrypted — the relay can't read it. /remote off to stop."))
	return b.String()
}

func (m *tuiModel) approve(always bool) (tea.Model, tea.Cmd) {
	tc := m.confirm
	m.confirm = ToolCall{}
	if always {
		m.autoAccept = true
		m.push(stHint.Render("  auto-accept enabled for this session"))
	}
	if len(m.pending) > 0 {
		m.pending = m.pending[1:]
	}
	m.mode = modeThinking
	return m, tea.Batch(m.startSpinner(), m.runToolCmd(tc))
}

func (m *tuiModel) deny() (tea.Model, tea.Cmd) {
	tc := m.confirm
	m.confirm = ToolCall{}
	if len(m.pending) > 0 {
		m.pending = m.pending[1:]
	}
	m.results = append(m.results, toolResult{Name: tc.Name, Output: "User declined this action."})
	m.push(stHint.Render("  ✗ skipped"))
	return m, m.advanceTools()
}

// --- transcript / viewport -------------------------------------------------

// greet seeds the transcript with the banner and tips.
func (m *tuiModel) greet() {
	m.follow = true
	m.lines = append(m.lines, banner(m.ver, m.cfg.Model, prettyPath(m.work)), "", tips())
	if m.cfg.APIKey == "" {
		m.lines = append(m.lines, "", stErr.Render("  No API key found. Set NOCTURNE_API (env or .env), or run /key noct_…"))
	}
}

// push appends a block to the transcript and scrolls to the bottom.
func (m *tuiModel) push(s string) {
	if strings.TrimSpace(s) == "" {
		return
	}
	m.lines = append(m.lines, "", s)
	m.syncViewport()
}

// syncViewport recomputes the viewport content (transcript + any live stream).
func (m *tuiModel) syncViewport() {
	if !m.ready {
		return
	}
	m.vp.Width = m.width
	m.vp.Height = m.viewportHeight()
	content := strings.Join(m.lines, "\n")
	if m.mode == modeStreaming && m.streamBuf != "" {
		if preview := streamPreview(m.streamBuf); preview != "" {
			content += "\n\n" + preview
		}
	}
	m.vp.SetContent(content)
	if m.follow {
		m.vp.GotoBottom()
	}
}

// viewportHeight is the transcript height: the screen minus the bottom UI.
func (m *tuiModel) viewportHeight() int {
	h := m.height - lipgloss.Height(m.bottomView())
	if h < 3 {
		return 3
	}
	return h
}

// streamPreview cleans in-flight streamed text for live display: complete or
// still-open <tool> blocks become a tidy "● preparing tool call…" line instead
// of the raw "<tool name=…" tags scrolling by.
// cleanToolStream replaces complete or partial <tool> blocks in the in-flight
// text with marker, so neither the terminal nor the browser shows raw tags.
func cleanToolStream(buf, marker string) string {
	const ph = "\x00TOOLCALL\x00"
	buf = toolBlock.ReplaceAllString(buf, ph)
	buf = toolBlockAlt.ReplaceAllString(buf, ph)
	buf = resultBlock.ReplaceAllString(buf, "")
	if i := strings.Index(buf, "<tool"); i >= 0 {
		buf = buf[:i] + ph
	}
	buf = strings.ReplaceAll(buf, ph, marker)
	return strings.TrimRight(buf, " \n")
}

func streamPreview(buf string) string {
	return cleanToolStream(buf, stAccent.Render("●")+" "+stDim.Render("preparing tool call…"))
}

// --- views -----------------------------------------------------------------

func (m *tuiModel) View() string {
	if m.quitting || !m.ready {
		return ""
	}
	bottom := m.bottomView()
	vh := m.height - lipgloss.Height(bottom)
	if vh < 3 {
		vh = 3
	}
	m.vp.Width = m.width
	m.vp.Height = vh
	return m.vp.View() + "\n" + bottom
}

func (m *tuiModel) bottomView() string {
	if m.mode == modePicker {
		return m.pickerView()
	}
	if m.mode == modeResume {
		return m.resumeView()
	}
	if m.mode == modeConfirm {
		return m.confirmView()
	}
	var b strings.Builder
	if m.showSlash {
		b.WriteString(m.slashMenuView())
		b.WriteString("\n")
	}
	if n := len(m.attachments); n > 0 {
		b.WriteString(stAccent.Render(fmt.Sprintf("  📎 %d image%s attached\n", n, plural(n))))
	}
	b.WriteString(m.inputBox())
	b.WriteString("\n")
	b.WriteString(m.statusLine())
	return b.String()
}

func (m *tuiModel) inputBox() string {
	w := m.width - 2
	if w < 10 {
		w = 10
	}
	border := colDim
	if m.busy() {
		border = colAccent
	}
	// The textarea is kept tall internally so it never scrolls; trim its blank
	// trailing rows (which render as just the "›" prompt) so the box only spans
	// the lines actually in use — but always keep one row per logical line so
	// blank lines from alt+enter stay visible.
	promptMark := strings.TrimSpace(m.ta.Prompt)
	minKeep := strings.Count(m.ta.Value(), "\n") + 1
	rows := strings.Split(m.ta.View(), "\n")
	for len(rows) > minKeep {
		t := strings.TrimSpace(ansiStrip(rows[len(rows)-1]))
		if t != "" && t != promptMark {
			break
		}
		rows = rows[:len(rows)-1]
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Width(w).
		Render(strings.Join(rows, "\n"))
}

func (m *tuiModel) statusLine() string {
	switch m.mode {
	case modeThinking:
		return " " + m.sp.View() + " " + stDim.Render(fmt.Sprintf("Thinking… (%s · esc to interrupt)", m.elapsed()))
	case modeStreaming:
		if strings.TrimSpace(m.streamBuf) == "" {
			return " " + m.sp.View() + " " + stDim.Render(fmt.Sprintf("Thinking… (%s · esc to interrupt)", m.elapsed()))
		}
		return " " + m.sp.View() + " " + stDim.Render(fmt.Sprintf("streaming… (%s · esc to interrupt)", m.elapsed()))
	default:
		return stHint.Render("  enter ↵ send · alt+↵ newline · ctrl+v paste image · scroll ↕ wheel/pgup · / commands")
	}
}

func (m *tuiModel) slashMenuView() string {
	var rows []string
	for i, it := range m.slashMatches {
		name := padRight(it.name, 12)
		if i == m.slashSel {
			rows = append(rows, stPrim.Render("› "+name)+" "+stDim.Render(it.desc))
		} else {
			rows = append(rows, "  "+stAccent.Render(name)+" "+stDim.Render(it.desc))
		}
	}
	w := m.width - 2
	if w < 10 {
		w = 10
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colDim).
		Width(w).
		Render(strings.Join(rows, "\n"))
}

// pickerView renders the scrollable model selector.
func (m *tuiModel) pickerView() string {
	const win = 12
	n := len(m.models)
	start := m.pickerSel - win/2
	if start < 0 {
		start = 0
	}
	if start+win > n {
		start = max(0, n-win)
	}
	end := min(n, start+win)

	rows := []string{stTitle.Render("Select a model") +
		stDim.Render(fmt.Sprintf("   %d available · ↑/↓ move · enter select · esc cancel", n))}
	if start > 0 {
		rows = append(rows, stDim.Render("  ⋮"))
	}
	for i := start; i < end; i++ {
		md := m.models[i]
		id := padRight(md.ID, 30)
		meta := stDim.Render(modelMeta(md))
		if md.ID == m.cfg.Model {
			meta += stOK.Render("  ✓ current")
		}
		if i == m.pickerSel {
			rows = append(rows, stPrim.Render("› "+id)+" "+meta)
		} else {
			rows = append(rows, "  "+stAccent.Render(id)+" "+meta)
		}
	}
	if end < n {
		rows = append(rows, stDim.Render("  ⋮"))
	}

	w := m.width - 2
	if w < 10 {
		w = 10
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colPrimary).
		Padding(0, 1).
		Width(w).
		Render(strings.Join(rows, "\n"))
}

// resumeView renders the saved-chat picker for /resume.
func (m *tuiModel) resumeView() string {
	const win = 10
	n := len(m.sessions)
	start := m.resumeSel - win/2
	if start < 0 {
		start = 0
	}
	if start+win > n {
		start = max(0, n-win)
	}
	end := min(n, start+win)

	rows := []string{stTitle.Render("Resume a chat") +
		stDim.Render(fmt.Sprintf("   %d saved here · ↑/↓ move · enter open · esc cancel", n))}
	if start > 0 {
		rows = append(rows, stDim.Render("  ⋮"))
	}
	for i := start; i < end; i++ {
		s := m.sessions[i]
		title := s.Title
		if len([]rune(title)) > 52 {
			title = string([]rune(title)[:52]) + "…"
		}
		meta := stDim.Render(fmt.Sprintf("%s · %d msgs", humanizeTime(s.Updated), countUserMsgs(s.Messages)))
		if i == m.resumeSel {
			rows = append(rows, stPrim.Render("› "+title)+"  "+meta)
		} else {
			rows = append(rows, "  "+stAccent.Render(title)+"  "+meta)
		}
	}
	if end < n {
		rows = append(rows, stDim.Render("  ⋮"))
	}

	w := m.width - 2
	if w < 10 {
		w = 10
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colPrimary).
		Padding(0, 1).
		Width(w).
		Render(strings.Join(rows, "\n"))
}

func humanizeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("Jan 2, 15:04")
	}
}

func countUserMsgs(msgs []ChatMessage) int {
	n := 0
	for _, msg := range msgs {
		if msg.Role == "user" && !strings.HasPrefix(strings.TrimSpace(msg.Content), "<tool_result") {
			n++
		}
	}
	return n
}

func (m *tuiModel) confirmView() string {
	tc := m.confirm
	w := m.width - 2
	if w > 90 {
		w = 90
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colPrimary).
		Padding(0, 1).
		Width(w)

	content := stTitle.Render(confirmTitle(tc.Name))
	if d := tc.details(m.work); d != "" {
		content += "\n" + stDim.Render(d)
	}
	q := "  " + stAccent.Render("Proceed?") + "   " +
		stOK.Render("(y)") + " yes    " +
		stPrim.Render("(a)") + " yes + auto-accept    " +
		stErr.Render("(n)") + " no"
	return box.Render(content) + "\n" + q
}

func (m *tuiModel) renderAssistant(md string) string {
	if strings.TrimSpace(md) == "" {
		return ""
	}
	if m.rd != nil {
		if out, err := m.rd.Render(md); err == nil {
			return strings.TrimRight(out, "\n")
		}
	}
	return md
}

// rebuildRenderer creates a markdown renderer with a fixed dark style. Using a
// fixed style (rather than auto-detect) avoids the terminal background-color
// query whose late reply could otherwise leak into the input.
func (m *tuiModel) rebuildRenderer() {
	w := m.width
	if w <= 0 || w > 100 {
		w = 100
	}
	if r, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(w-4)); err == nil {
		m.rd = r
	}
}

func (m *tuiModel) elapsed() time.Duration { return time.Since(m.started).Truncate(time.Second) }

// --- slash commands --------------------------------------------------------

func (m *tuiModel) runSlash(line string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(line)
	cmd := strings.ToLower(fields[0])
	arg := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))

	switch cmd {
	case "/help", "/?":
		m.push(helpText())
	case "/exit", "/quit", "/q":
		m.quitting = true
		return m, tea.Quit
	case "/clear", "/new":
		m.messages = nil
		m.tokens = 0
		m.ctxTokens = 0
		m.sessionID = newSessionID()
		m.sessionStart = time.Now()
		m.sessTitle = ""
		m.lines = nil
		m.greet()
		if cmd == "/new" {
			m.push(stHint.Render("  started a new chat"))
		} else {
			m.push(stHint.Render("  context cleared — new session"))
		}
	case "/model":
		if arg != "" {
			id := normalizeModelID(arg)
			m.cfg.Model = id
			_ = m.cfg.Save()
			m.push(stOK.Render("  model set to " + id))
			break
		}
		if len(m.models) == 0 {
			m.push(stHint.Render("  fetching models…"))
			return m, m.fetchModelsCmd("picker")
		}
		m.openPicker()
		return m, nil
	case "/models":
		if len(m.models) == 0 {
			m.push(stHint.Render("  fetching models…"))
			return m, m.fetchModelsCmd("list")
		}
		m.push(m.modelsList())
	case "/level":
		if arg == "" {
			m.push(stHint.Render("  thinking level: " + levelLabel(m.cfg.Level)))
			break
		}
		a := strings.ToLower(arg)
		if a != "off" && a != "normal" && a != "extended" {
			m.push(stErr.Render("  usage: /level off | normal | extended"))
			break
		}
		m.cfg.Level = a
		_ = m.cfg.Save()
		m.push(stOK.Render("  thinking level: " + a))
	case "/key":
		if arg == "" {
			m.push(stErr.Render("  usage: /key noct_…"))
		} else {
			m.cfg.SetAPIKey(arg)
			_ = m.cfg.Save()
			m.push(stOK.Render("  API key saved"))
		}
	case "/auto":
		m.autoAccept = !m.autoAccept
		m.push(stHint.Render("  auto-accept " + onOff(m.autoAccept)))
	case "/stream":
		m.cfg.Stream = !m.cfg.Stream
		_ = m.cfg.Save()
		m.push(stHint.Render("  streaming " + onOff(m.cfg.Stream)))
	case "/mouse":
		m.mouseOff = !m.mouseOff
		if m.mouseOff {
			m.push(stHint.Render("  mouse off — wheel scroll disabled; drag to select/copy text. Scroll with PgUp/PgDn."))
			return m, tea.DisableMouse
		}
		m.push(stHint.Render("  mouse on — scroll with the wheel (hold Option/Shift to select text)"))
		return m, tea.EnableMouseCellMotion
	case "/update":
		m.push(stHint.Render("  ⟳ checking for updates…"))
		return m, updateCmd()
	case "/remote":
		switch strings.ToLower(arg) {
		case "off", "stop":
			if m.remote != nil {
				m.remote.Stop()
				m.remote = nil
				m.push(stHint.Render("  remote stopped"))
			} else {
				m.push(stHint.Render("  remote isn't running"))
			}
		default:
			if m.remote != nil {
				m.push(m.remoteInfo())
				break
			}
			hub, err := startRemote(func(rm remoteSubmitMsg) { m.program.Send(rm) })
			if err != nil {
				m.push(stErr.Render("  ✗ remote: " + err.Error()))
				break
			}
			m.remote = hub
			m.push(m.remoteInfo())
		}
	case "/paste":
		return m, grabImageCmd()
	case "/image":
		if arg == "" {
			m.push(stErr.Render("  usage: /image <path>"))
			break
		}
		p := arg
		if !filepath.IsAbs(p) {
			p = filepath.Join(m.work, p)
		}
		img, err := loadImageFile(p)
		if err != nil {
			m.push(stErr.Render("  " + err.Error()))
		} else {
			m.attachments = append(m.attachments, img)
			m.push(stOK.Render("  attached " + arg))
		}
	case "/cwd", "/pwd":
		m.push(stHint.Render("  " + m.work))
	case "/cd":
		if arg == "" {
			m.push(stErr.Render("  usage: /cd <dir>"))
			break
		}
		p := arg
		if !filepath.IsAbs(p) {
			p = filepath.Join(m.work, p)
		}
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			m.push(stErr.Render("  not a directory: " + arg))
		} else {
			m.work = p
			m.push(stOK.Render("  cwd: " + prettyPath(p)))
		}
	case "/tokens", "/usage":
		m.push(m.usageText())
	case "/compact":
		if len(m.messages) == 0 {
			m.push(stHint.Render("  nothing to compact yet"))
			break
		}
		return m, m.compactCmd(false)
	case "/resume":
		return m.openResume()
	case "/init":
		return m.submitText(initPrompt)
	default:
		m.push(stErr.Render("  unknown command: " + cmd + "  (try /help)"))
	}
	return m, nil
}

func updateCmd() tea.Cmd {
	return func() tea.Msg {
		text, err := doUpdate(false)
		return updateDoneMsg{text: text, err: err}
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (m *tuiModel) usageText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  session tokens: %s\n", commas(m.tokens))
	if m.ctxTokens > 0 {
		fmt.Fprintf(&b, "  context: ~%s tokens (auto-compacts near %s) · /compact to do it now\n",
			commas(m.ctxTokens), commas(autoCompactThreshold))
	}
	switch q := m.lastQuota; {
	case q.Unlimited:
		b.WriteString("  daily quota: unlimited\n")
	case q.Cap > 0:
		fmt.Fprintf(&b, "  daily quota: %s / %s used · %s remaining\n", commas(q.Used), commas(q.Cap), commas(q.Remaining))
	}
	fmt.Fprintf(&b, "  model: %s · thinking: %s", m.cfg.Model, levelLabel(m.cfg.Level))
	return stHint.Render(b.String())
}

// modelsList renders the available models for the transcript.
func (m *tuiModel) modelsList() string {
	var b strings.Builder
	b.WriteString(stTitle.Render(fmt.Sprintf("  %d models", len(m.models))) + "\n")
	for _, md := range m.models {
		row := "  " + stAccent.Render(padRight(md.ID, 30)) + " " + stDim.Render(modelMeta(md))
		if md.ID == m.cfg.Model {
			row += stOK.Render("  ✓")
		}
		b.WriteString(row + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ensureModels prepends any of ids not already present (e.g. gpt-5.5, which
// the API doesn't list but still serves) so they're selectable in the picker.
func ensureModels(models []ModelInfo, ids ...string) []ModelInfo {
	have := make(map[string]bool, len(models))
	for _, m := range models {
		have[m.ID] = true
	}
	var extra []ModelInfo
	for _, id := range ids {
		if id != "" && !have[id] {
			have[id] = true
			extra = append(extra, ModelInfo{ID: id})
		}
	}
	return append(extra, models...)
}

// modelMeta renders the pricing/tags suffix for a model, omitting pricing when
// it's unknown (the models we inject manually have none).
func modelMeta(md ModelInfo) string {
	var parts []string
	if md.InPrice > 0 || md.OutPrice > 0 {
		parts = append(parts, fmt.Sprintf("$%g/$%g", md.InPrice, md.OutPrice))
	}
	var tags []string
	if md.Reasoning {
		tags = append(tags, "reasoning")
	}
	if md.Vision {
		tags = append(tags, "vision")
	}
	if md.Premium {
		tags = append(tags, "premium")
	}
	if len(tags) > 0 {
		parts = append(parts, "· "+strings.Join(tags, " · "))
	}
	return strings.Join(parts, "  ")
}

func levelLabel(l string) string {
	if l == "" {
		return "normal (default)"
	}
	return l
}

// commas formats an int with thousands separators.
func commas(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// --- helpers / static text -------------------------------------------------

// tryPasteImage attaches an image when a bracketed paste is just a path to one
// (e.g. dragging a file into the terminal). Returns false for ordinary text.
func tryPasteImage(pasted, work string) (Image, bool) {
	s := strings.TrimSpace(pasted)
	if s == "" || strings.ContainsAny(s, "\n") {
		return Image{}, false
	}
	s = strings.Trim(s, `'"`)
	s = strings.TrimPrefix(s, "file://")
	s = strings.ReplaceAll(s, `\ `, " ")
	if _, ok := imageExts[strings.ToLower(filepath.Ext(s))]; !ok {
		return Image{}, false
	}
	p := s
	if !filepath.IsAbs(p) {
		p = filepath.Join(work, p)
	}
	img, err := loadImageFile(p)
	if err != nil {
		return Image{}, false
	}
	return img, true
}

func confirmTitle(name string) string {
	switch name {
	case "run_command":
		return "Run command"
	case "write_file":
		return "Write file"
	case "edit_file":
		return "Edit file"
	}
	return name
}

func helpText() string {
	var b strings.Builder
	b.WriteString(stTitle.Render("  Commands") + "\n")
	for _, r := range slashCommands {
		b.WriteString("  " + stAccent.Render(padRight(r.name, 12)) + " " + stDim.Render(r.desc) + "\n")
	}
	b.WriteString("\n  " + stDim.Render("Tip: drop an image path into your message, e.g. ") + stAccent.Render("explain ./diagram.png"))
	return strings.TrimRight(b.String(), "\n")
}

const initPrompt = `Explore this project — list the directory, read the README and any package manifests and entry points — then create a file named NOCTURNE.md at the project root. Document concisely: what the project is, how to build / run / test it, the directory layout, and key conventions. Use write_file to create it, then give a one-line summary.`

func padRight(s string, n int) string {
	for len([]rune(s)) < n {
		s += " "
	}
	return s
}

func prettyPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
