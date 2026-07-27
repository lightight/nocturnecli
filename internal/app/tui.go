package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
	modeAsk
	modePerm
	modeDashboard
	modeTrust
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
	{"/model", "pick a model from a list (or /model <id>)"},
	{"/level", "thinking: off · normal · extended"},
	{"/permissions", "how tool actions are approved (ask · auto · bypass)"},
	{"/key", "save your API key (remembered everywhere)"},
	{"/image", "attach an image file"},
	{"/mouse", "capture the mouse for wheel scrolling (off = native text selection)"},
	{"/cd", "change the working directory"},
	{"/usage", "usage dashboard — status · usage · stats"},
	{"/cowork", "computer use — see & control this computer"},
	{"/plan", "plan mode — read-only exploration, approve to execute"},
	{"/compact", "summarize the conversation to free up context"},
	{"/resume", "resume a saved chat from this directory"},
	{"/new", "start a new chat"},
	{"/remote", "control this session from your browser (paired, e2e-encrypted)"},
	{"/clear", "clear the conversation"},
	{"/init", "generate a NOCTURNE.md for the project"},
	{"/party", "🎉 flash the input bar through a rainbow"},
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
	mouseOn       bool // mouse capture enabled for wheel scrolling; off preserves text selection

	mode     mode
	spinning bool
	cowork   bool // computer-use mode: screen control + whole filesystem
	plan     bool // plan mode: read-only exploration until the user approves

	lines       []string // transcript blocks (the scrollback)
	messages    []ChatMessage
	attachments []Image

	pending     []ToolCall
	results     []toolResult
	confirm     ToolCall
	guardReason string // why the guard flagged m.confirm (empty for a plain ask)

	// /permissions picker
	permSel int

	// /usage dashboard
	dashTab      int         // 0 Status · 1 Usage · 2 Stats
	dashRange    int         // 0 all · 1 last-7 · 2 last-30
	usage        *UsageStats // GET /api/ai/usage
	credits      *Credits    // GET /api/ai/credits
	usageErr     error       // last usage/credits fetch error
	dashSessions []Session   // all saved sessions, for the Stats tab
	dashNote     string      // transient footer note (e.g. "copied")
	linesAdded   int         // code changes this session
	linesRemoved int

	// `ask` tool — a question with selectable options, answered in the TUI
	askQ    string
	askOpts []string
	askSel  int

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
	compactAuto  bool   // whether the in-flight compaction was auto-triggered
	sessionID    string // file id for the current saved session
	sessTitle    string // stable title (first real user message), kept across compaction
	sessionStart time.Time
	sessions     []Session // loaded for the /resume picker
	resumeSel    int

	program *tea.Program // set after creation, for cross-goroutine Send
	remote  *remoteHub   // the /remote bridge, when running

	remoteBusySent  bool   // last busy state pushed to the browser
	remoteDraftSent string // last input draft pushed to the browser ("\xff" = never)

	// trust-this-workspace prompt (shown once per untrusted directory)
	trustSel int // 0 = trust, 1 = exit

	ctrlC    bool // a Ctrl+C is "armed": the next one exits (double-press to quit)
	quitting bool

	// /party 🎉 — flash the input bar through a flowing rainbow
	party     bool
	partyTick int

	// repeated-tool-call stack counts, keyed by tool signature
	toolSeen map[string]int

	// /remote connection progress (async handshake with the relay)
	remoteConnecting bool
	remoteFrame      int
}

// currentVision reports whether the selected model can see attached images.
func (m *tuiModel) currentVision() bool {
	return m.client.supportsVision(m.cfg.Model)
}

// --- messages --------------------------------------------------------------

type apiRespMsg struct {
	text  string
	usage Usage
	quota Quota
	err   error
}

type toolDoneMsg struct {
	name    string
	output  string
	image   *Image // screenshot capture, attached to the results message
	added   int    // lines added by a successful write/edit
	removed int    // lines removed
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

type startupUpdateMsg struct {
	info updateInfo
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

// guardResultMsg carries the guard model's verdict on a pending tool call.
type guardResultMsg struct {
	tc     ToolCall
	safe   bool
	reason string
	err    error
}

// imagesDescribedMsg carries images with vision-model descriptions filled in,
// ready to be sent to a non-vision model as text.
type imagesDescribedMsg struct {
	imgs []Image
	err  error
}

// usageLoadedMsg carries the account usage + credits fetched for the dashboard.
type usageLoadedMsg struct {
	usage   *UsageStats
	credits *Credits
	err     error
}

// keyCheckedMsg carries the startup API-key validation result. err is nil when
// the key works, ErrInvalidKey when the server rejected it, or a transport
// error when the server couldn't be reached.
type keyCheckedMsg struct{ err error }

// ctrlCResetMsg disarms the double-press-to-exit state after a short window, so
// a single stray Ctrl+C doesn't leave the CLI primed to quit indefinitely.
type ctrlCResetMsg struct{}

// partyTickMsg advances the /party rainbow animation by one frame.
type partyTickMsg struct{}

// partyCmd schedules the next /party animation frame.
func partyCmd() tea.Cmd {
	return tea.Tick(110*time.Millisecond, func(time.Time) tea.Msg { return partyTickMsg{} })
}

// remoteTickMsg advances the /remote "connecting…" animation by one frame.
type remoteTickMsg struct{}

// remoteReadyMsg carries the result of the async relay handshake.
type remoteReadyMsg struct {
	hub *remoteHub
	err error
}

func remoteTickCmd() tea.Cmd {
	return tea.Tick(90*time.Millisecond, func(time.Time) tea.Msg { return remoteTickMsg{} })
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
	// A waxing/waning moon instead of a spinning circle — on brand for Nocturne.
	sp.Spinner = spinner.Spinner{Frames: moonFrames, FPS: time.Second / 8}
	sp.Style = stAccent

	m := &tuiModel{
		cfg: cfg, client: NewClient(cfg), work: work, ver: version,
		ta: ta, sp: sp, vp: viewport.New(0, 0), mode: modeInput, follow: true,
		sessionID: newSessionID(), sessionStart: time.Now(),
		remoteDraftSent: "\xff",
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

func startTUI(cfg *Config, version string, cowork bool) error {
	// Pin the colour profile (from env) and a dark background up front so
	// lipgloss/termenv never *query* the terminal themselves.
	lipgloss.SetColorProfile(termenv.EnvColorProfile())
	lipgloss.SetHasDarkBackground(true)

	m := newModel(cfg, version)
	m.cowork = cowork
	// Mouse capture starts OFF so drag-select / right-click copy & paste keep
	// working natively; /mouse turns capture on for terminals whose wheel
	// doesn't fall back to arrow keys in the alt screen.
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithFilter(filterLeaks))
	m.program = p
	_, err := p.Run()
	m.remote.Stop()
	return err
}

func (m *tuiModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.fetchModelsCmd(""), m.validateKeyCmd(), m.startupUpdateCmd())
}

// validateKeyCmd verifies the API key with the server in the background so a
// dead key surfaces immediately instead of on the first failed reply. No key
// at all is handled by greet()'s hint, so nothing is sent then.
func (m *tuiModel) validateKeyCmd() tea.Cmd {
	if m.cfg.APIKey == "" {
		return nil
	}
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return keyCheckedMsg{err: client.ValidateKey(ctx)}
	}
}

func (m *tuiModel) startupUpdateCmd() tea.Cmd {
	return func() tea.Msg {
		info, err := checkHostedUpdate()
		return startupUpdateMsg{info: info, err: err}
	}
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

// Update handles one message, then mirrors any changed UI state (busy flag,
// input draft) to a paired browser.
func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	if tm, ok := next.(*tuiModel); ok {
		tm.syncRemoteState()
	}
	return next, cmd
}

func (m *tuiModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ta.SetWidth(m.inputContentWidth())
		m.rebuildRenderer()
		if !m.ready {
			m.ready = true
			m.greet()
		}
		m.syncViewport()
		return m, nil

	case ctrlCResetMsg:
		m.ctrlC = false
		m.syncViewport()
		return m, nil

	case partyTickMsg:
		if !m.party {
			return m, nil
		}
		m.partyTick++
		return m, partyCmd() // re-renders View() with the next rainbow frame

	case remoteTickMsg:
		if !m.remoteConnecting {
			return m, nil
		}
		m.remoteFrame++
		return m, remoteTickCmd()

	case remoteReadyMsg:
		m.remoteConnecting = false
		if msg.err != nil {
			m.push(stErr.Render("  ✗ remote: " + oneLine(msg.err.Error(), 120)))
			return m, nil
		}
		m.remote = msg.hub
		m.push(m.remoteInfo())
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
			m.follow = true
			m.syncViewport()
		}
		return m, nil

	case updateDoneMsg:
		if msg.err != nil {
			m.push(stErr.Render("  ✗ update: " + msg.err.Error()))
		} else {
			m.push(stOK.Render("  ⟳ " + msg.text))
		}
		return m, nil

	case startupUpdateMsg:
		if msg.err != nil {
			return m, nil
		}
		latest := msg.info.Version
		if compareVersions(m.ver, latest) < 0 {
			m.push(stHint.Render(fmt.Sprintf("  update available: %s → %s — run /update to install", m.ver, latest)))
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
		if msg.action == "picker" {
			m.openPicker()
		}
		return m, nil

	case compactDoneMsg:
		return m.handleCompactDone(msg)

	case guardResultMsg:
		return m.handleGuardResult(msg)

	case imagesDescribedMsg:
		return m.handleImagesDescribed(msg)

	case usageLoadedMsg:
		m.usage, m.credits, m.usageErr = msg.usage, msg.credits, msg.err
		if m.mode == modeDashboard {
			m.syncViewport()
		}
		return m, nil

	case keyCheckedMsg:
		switch {
		case msg.err == nil:
			// key works — nothing to say
		case errors.Is(msg.err, ErrInvalidKey):
			m.push(stErr.Render("  ✗ your Nocturne API key is invalid or has expired."))
			m.push(stHint.Render("  create a new one at " + NewKeyURL + " — then run /key noct_…"))
		default:
			// server unreachable or another error: don't lock the user out
			m.push(stDim.Render("  couldn't verify the API key (" + oneLine(msg.err.Error(), 80) + ") — continuing anyway"))
		}
		return m, nil

	case remoteSubmitMsg:
		return m.handleRemoteSubmit(msg)

	case tea.MouseMsg:
		if !m.mouseOn {
			return m, nil
		}
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
	case tea.KeyUp, tea.KeyDown:
		// With mouse capture off (the default) most terminals report the
		// wheel as Up/Down keys in the alt screen. Route arrows to the
		// transcript wherever they can't do anything else: while the bot is
		// busy, or in the input when it has a single line (arrows can't move
		// the cursor there) and no "/" menu is open.
		busy := m.mode == modeThinking || m.mode == modeStreaming
		idleInput := m.mode == modeInput && !m.showSlash && !strings.Contains(m.ta.Value(), "\n")
		if busy || idleInput {
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			m.follow = m.vp.AtBottom()
			return m, cmd
		}
	}

	// Ctrl+C exits on the second press (Claude-Code style). Handled centrally so
	// every mode behaves the same. Any other key disarms it.
	if msg.Type == tea.KeyCtrlC {
		return m.handleCtrlC()
	}
	m.ctrlC = false

	switch m.mode {
	case modeThinking, modeStreaming:
		if msg.Type == tea.KeyEsc {
			if m.cancel != nil {
				m.cancel()
			}
		}
		return m, nil
	case modeConfirm:
		return m.handleConfirmKey(msg)
	case modeAsk:
		return m.handleAskKey(msg)
	case modePicker:
		return m.handlePickerKey(msg)
	case modePerm:
		return m.handlePermKey(msg)
	case modeTrust:
		return m.handleTrustKey(msg)
	case modeDashboard:
		return m.handleDashKey(msg)
	case modeResume:
		return m.handleResumeKey(msg)
	default:
		return m.handleInputKey(msg)
	}
}

// handleCtrlC implements double-press-to-exit. The first press clears a pending
// draft or interrupts a running request; otherwise it arms the exit and shows a
// hint. The second press (within the reset window) actually quits.
func (m *tuiModel) handleCtrlC() (tea.Model, tea.Cmd) {
	// In the input box, the first Ctrl+C just clears a non-empty draft.
	if m.mode == modeInput && strings.TrimSpace(m.ta.Value()) != "" {
		m.ta.Reset()
		m.refreshSlash()
		m.ctrlC = false
		return m, nil
	}
	// While the agent is working, the first Ctrl+C interrupts it and arms exit.
	if (m.mode == modeThinking || m.mode == modeStreaming) && !m.ctrlC {
		if m.cancel != nil {
			m.cancel()
		}
		return m.armCtrlC()
	}
	// A second press within the window exits; the first one arms it.
	if m.ctrlC {
		m.quitting = true
		return m, tea.Quit
	}
	return m.armCtrlC()
}

// armCtrlC marks Ctrl+C as pending-exit and schedules it to disarm shortly, so a
// lone stray press doesn't leave the CLI primed to quit.
func (m *tuiModel) armCtrlC() (tea.Model, tea.Cmd) {
	m.ctrlC = true
	m.syncViewport()
	return m, tea.Tick(3*time.Second, func(time.Time) tea.Msg { return ctrlCResetMsg{} })
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
		m.push(stOK.Render("  model set to " + stAccent.Render(displayModel(id))))
	}
	return m, nil
}

// permOption is one selectable approval mode in the /permissions picker.
type permOption struct {
	mode  string
	label string
	desc  string
}

var permOptions = []permOption{
	{PermAsk, "Ask every time", "confirm each file change or command before it runs — safest"},
	{PermSmart, "Auto-accept safe", "a guard AI approves safe actions and only asks about risky ones"},
	{PermBypass, "Bypass — accept all", "run every command with no checks — use with caution"},
}

func permIndex(mode string) int {
	mode = normalizePerm(mode)
	for i, o := range permOptions {
		if o.mode == mode {
			return i
		}
	}
	return 0
}

func permLabel(mode string) string { return permOptions[permIndex(mode)].label }

func (m *tuiModel) openPerm() {
	m.mode = modePerm
	m.permSel = permIndex(m.cfg.Perm)
}

func (m *tuiModel) setPerm(mode string) {
	m.cfg.Perm = normalizePerm(mode)
	_ = m.cfg.Save()
	msg := "  permissions: " + stAccent.Render(permLabel(m.cfg.Perm))
	if m.cfg.Perm == PermBypass {
		msg += stErr.Render("  ⚠ all actions run without confirmation")
	}
	m.push(stOK.Render(msg))
}

func (m *tuiModel) handlePermKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(permOptions)
	switch msg.Type {
	case tea.KeyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case tea.KeyEsc:
		m.mode = modeInput
	case tea.KeyUp:
		m.permSel = (m.permSel - 1 + n) % n
	case tea.KeyDown:
		m.permSel = (m.permSel + 1) % n
	case tea.KeyEnter:
		m.mode = modeInput
		m.setPerm(permOptions[m.permSel].mode)
	}
	return m, nil
}

// openDashboard enters the /usage dashboard and refreshes account data.
func (m *tuiModel) openDashboard() (tea.Model, tea.Cmd) {
	m.mode = modeDashboard
	m.dashTab = 1 // Usage first (the headline: quota + totals)
	m.dashNote = ""
	m.dashSessions = listSessions("") // all projects, for the Stats heatmap
	m.follow = false
	m.syncViewport()
	m.vp.GotoTop()
	return m, m.fetchUsageCmd()
}

// fetchUsageCmd pulls /api/ai/usage and /api/ai/credits together.
func (m *tuiModel) fetchUsageCmd() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		usage, uErr := client.FetchUsage(ctx)
		credits, cErr := client.FetchCredits(ctx)
		err := uErr
		if err == nil {
			err = cErr
		}
		return usageLoadedMsg{usage: usage, credits: credits, err: err}
	}
}

func (m *tuiModel) handleDashKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.dashNote = ""
	switch msg.Type {
	case tea.KeyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case tea.KeyEsc:
		m.mode = modeInput
		m.follow = true
		m.syncViewport()
		return m, nil
	case tea.KeyRight, tea.KeyTab:
		m.dashTab = (m.dashTab + 1) % len(dashTabs)
		m.syncViewport()
		m.vp.GotoTop()
		return m, nil
	case tea.KeyLeft, tea.KeyShiftTab:
		m.dashTab = (m.dashTab - 1 + len(dashTabs)) % len(dashTabs)
		m.syncViewport()
		m.vp.GotoTop()
		return m, nil
	case tea.KeyCtrlS:
		if err := copyText(ansiStrip(m.dashboardBody())); err != nil {
			m.dashNote = stErr.Render("copy failed: " + err.Error())
		} else {
			m.dashNote = stOK.Render("✓ copied to clipboard")
		}
		return m, nil
	}
	switch strings.ToLower(msg.String()) {
	case "q":
		m.mode = modeInput
		m.follow = true
		m.syncViewport()
	case "h":
		m.dashTab = (m.dashTab - 1 + len(dashTabs)) % len(dashTabs)
		m.syncViewport()
		m.vp.GotoTop()
	case "l":
		m.dashTab = (m.dashTab + 1) % len(dashTabs)
		m.syncViewport()
		m.vp.GotoTop()
	case "r":
		if m.dashTab == 2 { // Stats: cycle the date window
			m.dashRange = (m.dashRange + 1) % len(dashRanges)
			m.syncViewport()
		} else { // otherwise retry the account fetch
			m.usageErr = nil
			m.syncViewport()
			return m, m.fetchUsageCmd()
		}
	}
	return m, nil
}

func (m *tuiModel) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Image paste: a bracketed paste that is just an image path → attach it.
	if msg.Paste {
		if img, ok := tryPasteImage(string(msg.Runes), m.work); ok {
			m.attachments = append(m.attachments, img)
			m.follow = true
			m.syncViewport()
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

// handleAskKey drives the `ask` prompt: arrows / number keys to pick, enter to
// confirm, esc to dismiss.
func (m *tuiModel) handleAskKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case tea.KeyUp:
		if m.askSel > 0 {
			m.askSel--
		}
		return m, nil
	case tea.KeyDown:
		if m.askSel < len(m.askOpts)-1 {
			m.askSel++
		}
		return m, nil
	case tea.KeyEnter:
		if len(m.askOpts) == 0 {
			return m, nil
		}
		return m.answerAsk(m.askOpts[m.askSel])
	case tea.KeyEsc:
		return m.answerAsk("")
	}
	if s := msg.String(); len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
		if i := int(s[0] - '1'); i < len(m.askOpts) {
			return m.answerAsk(m.askOpts[i])
		}
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
		return m.runSlash(raw, false)
	}

	text, inline := extractInlineImages(raw, m.work)
	imgs := append(m.attachments, inline...)
	m.attachments = nil

	m.messages = append(m.messages, ChatMessage{Role: "user", Content: text, Images: imgs})
	m.noteTitle(text)
	m.push(renderUser(text, len(imgs)))
	m.toRemote("user", text)

	// When the active model can't see images, have a vision model describe them
	// first and send that description as text in their place.
	if len(imgs) > 0 && !m.currentVision() {
		m.push(stHint.Render(fmt.Sprintf("  🔍 %s can't see images — describing %d with a vision model…",
			displayModel(m.cfg.Model), len(imgs))))
		m.mode = modeThinking
		m.started = time.Now()
		return m, tea.Batch(m.startSpinner(), m.describeThenReplyCmd(imgs))
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
	if m.cfg.Stream && !lastMessageHasImages(m.messages) {
		return m.startStream()
	}
	m.mode = modeThinking
	return tea.Batch(m.startSpinner(), m.callAPICmd())
}

func lastMessageHasImages(msgs []ChatMessage) bool {
	return len(msgs) > 0 && len(msgs[len(msgs)-1].Images) > 0
}

func (m *tuiModel) startStream() tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mode = modeStreaming
	m.streamBuf = ""
	ch := make(chan StreamEvent, 128)
	m.streamCh = ch
	go m.client.ChatStream(ctx, systemPromptMode(m.work, m.cowork, m.plan, m.cfg.Level == "extended"), append([]ChatMessage(nil), m.messages...), ch)
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
	system := systemPromptMode(m.work, m.cowork, m.plan, m.cfg.Level == "extended")
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
	if canonicalTool(tc.Name) == "task" {
		return m.runTaskCmd(tc)
	}
	work := m.work
	vision := m.currentVision()
	client := m.client
	vm := m.visionModelID()
	// For a non-vision model, screenshots are described by a vision model and
	// handed over as text (with element coordinates for the click/type tools).
	describe := func(img Image) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		return client.DescribeScreenshot(ctx, vm, img)
	}
	return func() tea.Msg {
		out, img := executeWithImage(tc, work, vision, describe)
		add, del := codeChange(tc, out)
		return toolDoneMsg{name: tc.Name, output: out, image: img, added: add, removed: del}
	}
}

// runTaskCmd runs the task tool: a nested sub-agent loop whose final report
// becomes the tool result. Reached via runToolCmd's dispatch, so every approval
// path (ask / smart / bypass) lands here the same way.
func (m *tuiModel) runTaskCmd(tc ToolCall) tea.Cmd {
	m.push(stHint.Render("  ⏳ sub-agent working: " + oneLine(firstNonEmpty(argStr(tc.Args, "description"), argStr(tc.Args, "prompt")), 80)))
	cfg := m.cfg
	client := m.client
	work := m.work
	prompt := argStr(tc.Args, "prompt")
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		report, err := runSubagent(ctx, cfg, client, work, prompt)
		if err != nil {
			return toolDoneMsg{name: tc.Name, output: "Error: " + oneLine(err.Error(), 400)}
		}
		return toolDoneMsg{name: tc.Name, output: report}
	}
}

// pushToolCall prints a tool-call header, collapsing repeats of the exact same
// call into a running "↻×N" stack count instead of stacking duplicate lines.
func (m *tuiModel) pushToolCall(tc ToolCall) {
	if m.toolSeen == nil {
		m.toolSeen = map[string]int{}
	}
	sig := canonicalTool(tc.Name) + "|" + tc.summarize()
	m.toolSeen[sig]++
	line := renderToolCall(tc)
	if n := m.toolSeen[sig]; n > 1 {
		line += "  " + stackBadge(n)
	}
	m.push(line)
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
		// Screenshots ride along on the results message so the model can see
		// what its screen actions produced.
		var imgs []Image
		for _, r := range m.results {
			if r.Image != nil {
				imgs = append(imgs, *r.Image)
			}
		}
		m.messages = append(m.messages, ChatMessage{Role: "user", Content: buildToolResults(m.results), Images: imgs})
		m.results = nil
		return m.startReply()
	}

	tc := m.pending[0]

	// Control tools steer the loop instead of producing a fed-back result.
	switch canonicalTool(tc.Name) {
	case "finish":
		return m.finishTask(tc)
	case "ask":
		return m.beginAsk(tc)
	case "cowork":
		// The model asks to widen its own powers; the user confirms. approve()
		// flips the mode instead of running the tool.
		m.pushToolCall(tc)
		m.toRemote("tool", "● "+tc.summarize())
		m.mode = modeConfirm
		m.confirm = tc
		m.guardReason = ""
		m.toRemote("status", "waiting for approval in the terminal: enable cowork mode")
		return nil
	}

	m.pushToolCall(tc)
	m.toRemote("tool", "● "+tc.summarize())

	// The task tool only exists at extended thinking level.
	if canonicalTool(tc.Name) == "task" && m.cfg.Level != "extended" {
		m.pending = m.pending[1:]
		m.results = append(m.results, toolResult{Name: tc.Name,
			Output: "the task tool requires thinking level 'extended' — set it with /level extended"})
		m.push(stHint.Render("  ✗ task requires /level extended"))
		return m.advanceTools()
	}

	// Plan mode: refuse anything that mutates state and tell the model to keep
	// exploring, mirroring the normal result-feeding path so the loop continues.
	if m.plan && needsApproval(tc.Name) {
		m.pending = m.pending[1:]
		m.results = append(m.results, toolResult{Name: tc.Name,
			Output: "plan mode is ON: edits, commands, and sub-agents are disabled. Finish exploring and present your plan; the user runs /plan to approve and execute."})
		m.push(stHint.Render("  ✗ blocked by plan mode"))
		return m.advanceTools()
	}

	if needsApproval(tc.Name) {
		switch normalizePerm(m.cfg.Perm) {
		case PermBypass:
			// run without any check
		case PermSmart:
			// let the guard model decide; it may still route to a prompt
			return m.guardCheckCmd(tc)
		default: // PermAsk
			m.mode = modeConfirm
			m.confirm = tc
			m.guardReason = ""
			m.toRemote("status", "waiting for approval in the terminal: "+tc.summarize())
			return nil
		}
	}

	m.pending = m.pending[1:]
	m.mode = modeThinking
	return tea.Batch(m.startSpinner(), m.runToolCmd(tc))
}

// guardCheckCmd asks the small guard model whether tc is safe to auto-accept.
func (m *tuiModel) guardCheckCmd(tc ToolCall) tea.Cmd {
	m.mode = modeThinking
	m.started = time.Now()
	m.push(stHint.Render("  🛡 checking whether this is safe to auto-accept…"))
	client := m.client
	action := tc.summarize()
	if d := tc.details(m.work); d != "" {
		action += "\n" + d
	}
	goal := m.lastUserGoal()
	return tea.Batch(m.startSpinner(), func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		safe, reason, err := client.CheckSafety(ctx, GuardModel, action, goal)
		return guardResultMsg{tc: tc, safe: safe, reason: reason, err: err}
	})
}

// handleGuardResult acts on the guard model's verdict: run it when safe, or
// fall back to a manual confirmation (showing the reason) when risky or when
// the check itself failed.
func (m *tuiModel) handleGuardResult(msg guardResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.confirm = msg.tc
		m.guardReason = "couldn't verify safety (" + oneLine(msg.err.Error(), 80) + ") — confirm manually"
		m.mode = modeConfirm
		m.toRemote("status", "waiting for approval in the terminal: "+msg.tc.summarize())
		return m, nil
	}
	if !msg.safe {
		m.confirm = msg.tc
		m.guardReason = msg.reason
		m.mode = modeConfirm
		m.push(stHint.Render("  ⚠ flagged as risky — asking you to confirm"))
		m.toRemote("status", "waiting for approval in the terminal: "+msg.tc.summarize())
		return m, nil
	}
	note := "  ✓ safe — auto-accepted"
	if msg.reason != "" {
		note += stDim.Render("  (" + oneLine(msg.reason, 100) + ")")
	}
	m.push(stOK.Render(note))
	if len(m.pending) > 0 {
		m.pending = m.pending[1:]
	}
	m.mode = modeThinking
	return m, tea.Batch(m.startSpinner(), m.runToolCmd(msg.tc))
}

// lastUserGoal returns the most recent real user message, as brief context for
// the guard model.
func (m *tuiModel) lastUserGoal() string {
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		c := strings.TrimSpace(msg.Content)
		if msg.Role == "user" && c != "" && !strings.HasPrefix(c, "<tool_result") {
			return c
		}
	}
	return ""
}

// describeThenReplyCmd runs the vision model over imgs, then hands back the
// described images so the reply can proceed against a non-vision model.
func (m *tuiModel) describeThenReplyCmd(imgs []Image) tea.Cmd {
	client := m.client
	vm := m.visionModelID()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		described, err := client.DescribeImages(ctx, vm, imgs)
		return imagesDescribedMsg{imgs: described, err: err}
	}
}

// handleImagesDescribed stores the descriptions on the last user message and
// starts the reply.
func (m *tuiModel) handleImagesDescribed(msg imagesDescribedMsg) (tea.Model, tea.Cmd) {
	if len(msg.imgs) > 0 {
		for i := len(m.messages) - 1; i >= 0; i-- {
			if m.messages[i].Role == "user" && len(m.messages[i].Images) > 0 {
				m.messages[i].Images = msg.imgs
				break
			}
		}
	}
	if msg.err != nil {
		m.push(stErr.Render("  ✗ image description failed: " + oneLine(msg.err.Error(), 120)))
		m.push(stHint.Render("  sending what was described (if any) as text"))
	} else {
		m.push(stOK.Render(fmt.Sprintf("  ✓ image described — %s can now reason about it", displayModel(m.cfg.Model))))
	}
	return m, m.startReply()
}

// visionModelID picks a vision-capable model to describe images, preferring one
// the account actually has, falling back to the built-in default.
func (m *tuiModel) visionModelID() string {
	for _, md := range m.models {
		if md.ID == VisionModel && md.Vision {
			return md.ID
		}
	}
	for _, md := range m.models {
		if md.Vision {
			return md.ID
		}
	}
	return VisionModel
}

// finishTask ends the agent loop: it shows the model's summary as the final
// message and returns to input, without feeding anything back.
func (m *tuiModel) finishTask(tc ToolCall) tea.Cmd {
	summary := strings.TrimSpace(argStr(tc.Args, "summary"))
	if summary == "" {
		summary = "Done."
	}
	m.pending = nil
	m.results = nil
	m.mode = modeInput
	if out := m.renderAssistant(summary); out != "" {
		m.push(out)
	}
	m.toRemote("assistant", summary)
	m.persistSession()
	return nil
}

// beginAsk pauses the loop and presents the model's question with selectable
// options; the chosen answer is fed back when the user picks one.
func (m *tuiModel) beginAsk(tc ToolCall) tea.Cmd {
	m.pushToolCall(tc)
	m.toRemote("tool", "● "+tc.summarize())
	m.askQ = strings.TrimSpace(argStr(tc.Args, "question"))
	if m.askQ == "" {
		m.askQ = "Which option?"
	}
	m.askOpts = toStrings(tc.Args["options"])
	if len(m.askOpts) == 0 {
		m.askOpts = []string{"Yes", "No"}
	}
	m.askSel = 0
	m.mode = modeAsk
	m.toRemote("status", "waiting for an answer in the terminal: "+m.askQ)
	return nil
}

// answerAsk records the user's choice as the ask tool's result and resumes.
func (m *tuiModel) answerAsk(choice string) (tea.Model, tea.Cmd) {
	out := "User selected: " + choice
	label := choice
	if choice == "" {
		out = "User dismissed the question without choosing."
		label = "(dismissed)"
	}
	if len(m.pending) > 0 {
		m.pending = m.pending[1:]
	}
	m.results = append(m.results, toolResult{Name: "ask", Output: out})
	m.push(stHint.Render("  → " + label))
	m.toRemote("tool", "  └ "+label)
	m.askQ = ""
	m.askOpts = nil
	m.askSel = 0
	return m, m.advanceTools()
}

func (m *tuiModel) handleToolDone(msg toolDoneMsg) (tea.Model, tea.Cmd) {
	m.linesAdded += msg.added
	m.linesRemoved += msg.removed
	m.results = append(m.results, toolResult{Name: msg.name, Output: msg.output, Image: msg.image})
	m.push(renderToolResult(msg.output))
	m.toRemote("tool", "  └ "+oneLine(firstLine(msg.output), 100))
	return m, m.advanceTools()
}

// --- compaction ------------------------------------------------------------

func (m *tuiModel) compactCmd(auto bool) tea.Cmd {
	m.compacting = true
	m.compactAuto = auto
	m.mode = modeThinking
	m.started = time.Now()
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

// resumableSessions lists saved chats for this directory, excluding the
// session we're currently in.
func (m *tuiModel) resumableSessions() []Session {
	sessions := listSessions(m.work)
	filtered := sessions[:0]
	for _, s := range sessions {
		if s.ID != m.sessionID {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

func (m *tuiModel) openResume() (tea.Model, tea.Cmd) {
	m.sessions = m.resumableSessions()
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

// syncRemoteState mirrors the busy flag and the input draft to a paired
// browser whenever they change, so the web UI's send/stop button and text box
// track the terminal. Called after every update; cheap when nothing changed.
func (m *tuiModel) syncRemoteState() {
	if m.remote == nil {
		return
	}
	if b := m.busy(); b != m.remoteBusySent {
		m.remoteBusySent = b
		text := "0"
		if b {
			text = "1"
		}
		m.remote.broadcast(remoteEvent{Kind: "busy", Text: text})
	}
	if d := m.ta.Value(); d != m.remoteDraftSent {
		m.remoteDraftSent = d
		m.remote.broadcast(remoteEvent{Kind: "input", Text: d})
	}
}

// handleRemoteSubmit processes a message that arrived from the browser.
func (m *tuiModel) handleRemoteSubmit(msg remoteSubmitMsg) (tea.Model, tea.Cmd) {
	switch msg.kind {
	case "hello":
		// A browser just paired: force a full state push on the next sync.
		m.remoteBusySent = !m.busy()
		m.remoteDraftSent = "\xff"
		m.syncRemoteState()
		return m, nil
	case "input":
		if m.ta.Value() != msg.text {
			m.ta.SetValue(msg.text)
			m.refreshSlash()
		}
		return m, nil
	case "interrupt":
		if m.busy() && m.cancel != nil {
			m.cancel()
		}
		return m, nil
	}

	text := strings.TrimSpace(msg.text)
	if text == "" {
		return m, nil
	}
	if m.busy() {
		m.toRemote("status", "busy — wait for the current reply to finish")
		return m, nil
	}
	if strings.HasPrefix(text, "/") {
		m.toRemote("user", text)
		before := len(m.lines)
		next, cmd := m.runSlash(text, true)
		if tm, ok := next.(*tuiModel); ok {
			tm.forwardRemoteFeedback(before)
		}
		return next, cmd
	}
	m.follow = true
	m.messages = append(m.messages, ChatMessage{Role: "user", Content: text})
	m.noteTitle(text)
	m.push(renderUser(text, 0) + " " + stDim.Render("(remote)"))
	m.toRemote("user", text)
	return m, m.startReply()
}

// forwardRemoteFeedback relays the transcript lines a remotely-issued slash
// command just produced, stripped of ANSI, so the browser sees the result.
func (m *tuiModel) forwardRemoteFeedback(before int) {
	if m.remote == nil {
		return
	}
	sent := false
	lines := m.lines[before:]
	if len(lines) > 40 {
		// A command like /resume <n> re-renders a whole chat; don't flood
		// the phone with hundreds of messages.
		m.toRemote("status", fmt.Sprintf("… %d earlier lines omitted …", len(lines)-40))
		lines = lines[len(lines)-40:]
	}
	for _, ln := range lines {
		clean := strings.TrimSpace(ansiStrip(ln))
		if clean == "" {
			continue
		}
		m.toRemote("status", clean)
		sent = true
	}
	if !sent && m.mode != modeInput {
		m.toRemote("status", "that command opened something in the terminal — finish it there")
	}
}

func (m *tuiModel) remoteInfo() string {
	h := m.remote
	relay := h.base
	if u, err := url.Parse(h.base); err == nil && u.Host != "" {
		relay = u.Host
	}

	rows := []string{
		stTitle.Render("🔒 Remote control") + stOK.Render("  ● live") + stDim.Render("  · end-to-end encrypted"),
		"",
		stDim.Render("  open ") + stAccent.Render(h.url),
		stDim.Render("  code ") + stTitle.Render(strings.Join(strings.Split(h.code, ""), " ")),
		"",
		stDim.Render("  1.") + " open the link on any phone or computer",
		stDim.Render("  2.") + " enter the 6-character code to pair",
		stDim.Render("  3.") + " type there — replies & tool activity stream live",
		"",
		stDim.Render("  relay      ") + stHint.Render(relay),
		stDim.Render("  encryption ") + stHint.Render("AES-256-GCM, key derived from the code (never sent)"),
		stDim.Render("  approvals  ") + stHint.Render("still confirmed here at the terminal"),
		"",
		stHint.Render("  /remote off to stop"),
	}

	w := m.width - 2
	if w < 20 {
		w = 20
	}
	if w > 76 {
		w = 76
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colAccent).
		Padding(0, 1).
		Width(w).
		Render(strings.Join(rows, "\n"))
}

func (m *tuiModel) approve(always bool) (tea.Model, tea.Cmd) {
	tc := m.confirm
	m.confirm = ToolCall{}
	m.guardReason = ""
	if always {
		m.cfg.Perm = PermSmart
		_ = m.cfg.Save()
		m.push(stHint.Render("  auto-accept on — a guard AI will approve safe actions and only ask about risky ones (/permissions to change)"))
	}
	if len(m.pending) > 0 {
		m.pending = m.pending[1:]
	}
	if canonicalTool(tc.Name) == "cowork" {
		m.cowork = true
		m.results = append(m.results, toolResult{Name: tc.Name,
			Output: "cowork mode enabled — you can now see and control the screen (screenshot, click, type, scroll, open apps) and use the whole filesystem."})
		m.push(stOK.Render("  ✓ cowork mode on — screen control + whole filesystem"))
		return m, m.advanceTools()
	}
	m.mode = modeThinking
	return m, tea.Batch(m.startSpinner(), m.runToolCmd(tc))
}

func (m *tuiModel) deny() (tea.Model, tea.Cmd) {
	tc := m.confirm
	m.confirm = ToolCall{}
	m.guardReason = ""
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
	m.lines = append(m.lines, banner(m.ver, displayModel(m.cfg.Model), prettyPath(m.work)), "", tips())
	if m.cfg.APIKey == "" {
		m.lines = append(m.lines, "", stErr.Render("  No API key found. Set NOCTURNE_API (env or .env), or run /key noct_…"))
	} else if m.cfg.KeyNeedsPersist() {
		// Key is loaded from env/.env but not yet saved to the private config —
		// a bare /key would make it permanent across every directory.
		m.lines = append(m.lines, stHint.Render("  tip: run /key to remember this key everywhere"))
	}

	// First run in an untrusted directory: ask the user to confirm before the
	// agent can read, edit, or run anything here.
	if !m.cfg.IsTrusted(m.work) {
		m.mode = modeTrust
		m.trustSel = 0
	}
}

// handleTrustKey drives the trust-this-workspace prompt: enter/y trusts and
// remembers the folder, esc/n exits without touching anything.
func (m *tuiModel) handleTrustKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyUp, tea.KeyDown, tea.KeyTab:
		m.trustSel = 1 - m.trustSel
		return m, nil
	case tea.KeyEsc:
		m.quitting = true
		return m, tea.Quit
	case tea.KeyEnter:
		if m.trustSel == 0 {
			return m.acceptTrust()
		}
		m.quitting = true
		return m, tea.Quit
	}
	switch strings.ToLower(msg.String()) {
	case "y":
		return m.acceptTrust()
	case "n", "q":
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

// acceptTrust records the current workspace as trusted and drops into the
// normal input mode.
func (m *tuiModel) acceptTrust() (tea.Model, tea.Cmd) {
	_ = m.cfg.Trust(m.work)
	m.mode = modeInput
	m.push(stOK.Render("  ✓ workspace trusted") + stDim.Render("  — "+prettyPath(m.work)))
	return m, nil
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
	if m.mode == modeDashboard {
		m.vp.SetContent(m.dashboardBody())
		return
	}
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
	if m.mode == modePerm {
		return m.permView()
	}
	if m.mode == modeTrust {
		return m.trustView()
	}
	if m.mode == modeDashboard {
		return m.dashFooter()
	}
	if m.mode == modeResume {
		return m.resumeView()
	}
	if m.mode == modeConfirm {
		return m.confirmView()
	}
	if m.mode == modeAsk {
		return m.askView()
	}
	var b strings.Builder
	if m.showSlash {
		b.WriteString(m.slashMenuView())
		b.WriteString("\n")
	}
	if n := len(m.attachments); n > 0 {
		b.WriteString(stAccent.Render(fmt.Sprintf("  📎 %d image%s attached", n, plural(n))))
		b.WriteString(stDim.Render(" — add a message and press enter"))
		b.WriteString("\n")
	}
	b.WriteString(m.inputBox())
	b.WriteString("\n")
	b.WriteString(m.statusLine())
	return b.String()
}

func (m *tuiModel) inputContentWidth() int {
	w := m.width - 2
	if w < 10 {
		return 10
	}
	return w
}

func (m *tuiModel) inputBox() string {
	w := m.inputContentWidth()
	m.ta.SetWidth(w)
	border := colDim
	if m.busy() {
		border = colAccent
	}
	if m.party {
		// Flash the border and the "›" prompt through the flowing rainbow.
		border = rainbowAt(m.partyTick)
		promptStyle := lipgloss.NewStyle().Foreground(rainbowAt(m.partyTick + 3)).Bold(true)
		m.ta.FocusedStyle.Prompt = promptStyle
		m.ta.BlurredStyle.Prompt = promptStyle
	} else {
		m.ta.FocusedStyle.Prompt = stUser
		m.ta.BlurredStyle.Prompt = stUser
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

// compactStatus renders the animated compaction progress bar. There's no real
// server-side progress, so the fraction eases toward ~97% over time and the
// "✓ compacted" line replaces it when the summary lands.
func (m *tuiModel) compactStatus() string {
	t := time.Since(m.started).Seconds()
	frac := t / (t + 2.5) // asymptotic toward 1
	if frac > 0.97 {
		frac = 0.97
	}
	label := "compacting conversation"
	if m.compactAuto {
		label = "auto-compacting (context is large)"
	}
	return " " + m.sp.View() + " " +
		stHint.Render(label+" ") +
		progressBar(frac, 22) +
		stAccent.Render(fmt.Sprintf(" %3d%%", int(frac*100)))
}

// remoteConnectingStatus renders the animated "establishing encrypted session"
// line while the relay handshake is in flight.
func (m *tuiModel) remoteConnectingStatus() string {
	dots := strings.Repeat("·", 1+(m.remoteFrame%3))
	return " " + moonAt(m.remoteFrame) + " " +
		stAccent.Render("linking remote") + stHint.Render(" — establishing end-to-end encrypted session"+dots)
}

func (m *tuiModel) statusLine() string {
	if m.ctrlC {
		return stErr.Render("  press Ctrl+C again to exit")
	}
	if m.compacting {
		return m.compactStatus()
	}
	if m.remoteConnecting {
		return m.remoteConnectingStatus()
	}
	if m.party && m.mode == modeInput && !m.showSlash {
		return "  " + rainbowText("party mode on · /party again to stop it", m.partyTick)
	}
	switch m.mode {
	case modeThinking:
		return " " + m.sp.View() + " " + stDim.Render(fmt.Sprintf("Thinking… (%s · esc to interrupt)", m.elapsed()))
	case modeStreaming:
		if strings.TrimSpace(m.streamBuf) == "" {
			return " " + m.sp.View() + " " + stDim.Render(fmt.Sprintf("Thinking… (%s · esc to interrupt)", m.elapsed()))
		}
		return " " + m.sp.View() + " " + stDim.Render(fmt.Sprintf("streaming… (%s · esc to interrupt)", m.elapsed()))
	default:
		scroll := "wheel/pgup/pgdn scroll · drag selects · /mouse capture"
		if m.mouseOn {
			scroll = "wheel captured · /mouse to select text"
		}
		line := "  enter ↵ send · alt+↵ newline · ctrl+v paste image · " + scroll + " · / commands"
		if m.cowork {
			line = stAccent.Render("  cowork") + stHint.Render(" · ") + strings.TrimLeft(line, " ")
		}
		return stHint.Render(line)
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
		id := padRight(displayModel(md.ID), 30)
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

// trustView renders the first-run "do you trust this folder?" prompt.
func (m *tuiModel) trustView() string {
	opts := []string{"Yes, I trust this folder", "No, exit"}
	var rows []string
	rows = append(rows,
		stTitle.Render("Do you trust the files in this folder?"),
		stDim.Render("  Nocturne can read, edit, and run commands in:"),
		"  "+stAccent.Render(prettyPath(m.work)),
		stDim.Render("  Only trust folders whose contents you're comfortable running."),
		"",
	)
	for i, o := range opts {
		if i == m.trustSel {
			rows = append(rows, stPrim.Render("› "+o))
		} else {
			rows = append(rows, "  "+stAccent.Render(o))
		}
	}
	rows = append(rows, "", stDim.Render("  ↑/↓ move · enter select · y trust · esc/n exit"))

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

// permView renders the /permissions approval-mode picker.
func (m *tuiModel) permView() string {
	rows := []string{stTitle.Render("How should tool actions be approved?") +
		stDim.Render("   ↑/↓ move · enter select · esc cancel")}
	for i, o := range permOptions {
		label := padRight(o.label, 22)
		tail := stDim.Render(o.desc)
		if o.mode == normalizePerm(m.cfg.Perm) {
			tail += stOK.Render("  ✓ current")
		}
		if i == m.permSel {
			rows = append(rows, stPrim.Render("› "+label)+" "+tail)
		} else {
			rows = append(rows, "  "+stAccent.Render(label)+" "+tail)
		}
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
	if m.guardReason != "" {
		content += "\n" + stErr.Render("⚠ guard: ") + stDim.Render(oneLine(m.guardReason, 160))
	}
	q := "  " + stAccent.Render("Proceed?") + "   " +
		stOK.Render("(y)") + " yes    " +
		stPrim.Render("(a)") + " yes + auto-accept safe    " +
		stErr.Render("(n)") + " no"
	return box.Render(content) + "\n" + q
}

func (m *tuiModel) askView() string {
	w := m.width - 2
	if w > 90 {
		w = 90
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colPrimary).
		Padding(0, 1).
		Width(w)

	var b strings.Builder
	b.WriteString(stTitle.Render(m.askQ))
	for i, opt := range m.askOpts {
		b.WriteString("\n")
		label := fmt.Sprintf("%d. %s", i+1, opt)
		if i == m.askSel {
			b.WriteString(stPrim.Render("› " + label))
		} else {
			b.WriteString("  " + stAccent.Render(label))
		}
	}
	hint := stDim.Render("↑/↓ move · 1–9 pick · enter select · esc skip")
	return box.Render(b.String()) + "\n  " + hint
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

func (m *tuiModel) runSlash(line string, remote bool) (tea.Model, tea.Cmd) {
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
			id := m.resolveModelArg(arg)
			m.cfg.Model = id
			_ = m.cfg.Save()
			m.push(stOK.Render("  model set to " + displayModel(id)))
			break
		}
		if len(m.models) == 0 {
			m.push(stHint.Render("  fetching models…"))
			return m, m.fetchModelsCmd("picker")
		}
		m.openPicker()
		return m, nil
	case "/permissions", "/perm":
		if arg != "" {
			m.setPerm(normalizePerm(arg))
			break
		}
		m.openPerm()
		return m, nil
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
			// Bare /key: remember the key already loaded (from env or .env)
			// in the private config so it's used from any directory.
			if m.cfg.APIKey == "" {
				m.push(stErr.Render("  usage: /key noct_…  (no key is currently loaded to remember)"))
				break
			}
			m.cfg.PersistKey()
		} else {
			m.cfg.SetAPIKey(arg)
		}
		if err := m.cfg.Save(); err != nil {
			m.push(stErr.Render("  couldn't save key: " + err.Error()))
			break
		}
		m.push(stOK.Render("  API key " + m.cfg.MaskedKey() + " saved to " + prettyPath(ConfigPath())))
		if m.cfg.keyFromRealEnv {
			m.push(stHint.Render("  note: NOCTURNE_API is exported in your environment and still overrides the saved key — unset it to use this one"))
		}
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
			if m.remoteConnecting {
				break
			}
			// Handshake off the UI goroutine so the "linking remote…" animation
			// keeps moving instead of the whole TUI freezing on the HTTP round-trip.
			m.remoteConnecting = true
			m.remoteFrame = 0
			send := func(rm remoteSubmitMsg) { m.program.Send(rm) }
			connect := func() tea.Msg {
				hub, err := startRemote(send)
				return remoteReadyMsg{hub: hub, err: err}
			}
			return m, tea.Batch(remoteTickCmd(), connect)
		}
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
			m.follow = true
			m.syncViewport()
		}
	case "/mouse":
		m.mouseOn = !m.mouseOn
		if m.mouseOn {
			m.push(stHint.Render("  mouse on — wheel scrolls the transcript (shift+drag to select in most terminals); /mouse again for native selection"))
			return m, tea.EnableMouseCellMotion
		}
		m.push(stHint.Render("  mouse off — drag to select & copy text; wheel still scrolls in most terminals, PgUp/PgDn always works"))
		return m, tea.DisableMouse
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
	case "/cowork":
		m.cowork = !m.cowork
		if m.cowork {
			m.push(stOK.Render("  cowork mode on — the agent can now see & control the screen and use the whole filesystem"))
			m.push(stHint.Render("  screen clicks/typing may require your OS's accessibility permission for this terminal"))
		} else {
			m.push(stHint.Render("  cowork mode off — back to the working directory"))
		}
	case "/plan":
		m.plan = !m.plan
		if m.plan {
			m.push(stOK.Render("  plan mode on — the agent will explore read-only and propose a plan"))
			m.push(stHint.Render("  run /plan again to approve the plan and execute it"))
		} else {
			m.push(stOK.Render("  plan approved — executing"))
			return m.submitText("Plan approved — go ahead and implement it.")
		}
	case "/usage", "/tokens", "/stats", "/status":
		if remote {
			// No dashboard on a phone — send the usage tab as text, and
			// refresh account data so a repeat shows the latest numbers.
			m.push(m.renderUsage())
			return m, m.fetchUsageCmd()
		}
		model, cmd := m.openDashboard()
		return model, cmd
	case "/compact":
		if len(m.messages) == 0 {
			m.push(stHint.Render("  nothing to compact yet"))
			break
		}
		return m, m.compactCmd(false)
	case "/resume":
		if arg != "" {
			n, err := strconv.Atoi(arg)
			sessions := m.resumableSessions()
			if err != nil || n < 1 || n > len(sessions) {
				m.push(stErr.Render("  usage: /resume <number>  (bare /resume lists saved chats)"))
				break
			}
			m.restoreSession(sessions[n-1])
			break
		}
		if remote {
			// No interactive picker in the browser — list saved chats as
			// text and let them pick one with /resume <n>.
			sessions := m.resumableSessions()
			if len(sessions) == 0 {
				m.push(stHint.Render("  no saved chats in this directory yet"))
				break
			}
			var b strings.Builder
			b.WriteString(stTitle.Render("  saved chats") + stDim.Render("  · /resume <n> to open one") + "\n")
			for i, s := range sessions {
				title := s.Title
				if len([]rune(title)) > 52 {
					title = string([]rune(title)[:52]) + "…"
				}
				meta := stDim.Render(fmt.Sprintf("%s · %d msgs", humanizeTime(s.Updated), countUserMsgs(s.Messages)))
				b.WriteString(fmt.Sprintf("  %s %s  %s\n", stAccent.Render(strconv.Itoa(i+1)+"."), title, meta))
			}
			m.push(strings.TrimRight(b.String(), "\n"))
			break
		}
		return m.openResume()
	case "/init":
		return m.submitText(initPrompt)
	case "/party":
		m.party = !m.party
		if m.party {
			// No transcript banner — the rainbow status line at the bottom says it all.
			return m, partyCmd()
		}
		m.push(stHint.Render("  party mode off"))
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
			if md, ok := knownModelInfo(id); ok {
				extra = append(extra, md)
			} else {
				extra = append(extra, ModelInfo{ID: id})
			}
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
	switch canonicalTool(name) {
	case "run_command":
		return "Run command"
	case "write_file":
		return "Write file"
	case "edit_file":
		return "Edit file"
	case "delete":
		return "Delete file"
	case "rename":
		return "Rename file"
	case "import_github":
		return "Import GitHub repo"
	case "cowork":
		return "Enable cowork mode (computer use)"
	case "click", "move_mouse", "scroll":
		return "Control the mouse"
	case "type_text", "key_press":
		return "Control the keyboard"
	case "open_app":
		return "Open app / URL"
	}
	return name
}

func helpText() string {
	var b strings.Builder
	b.WriteString(stTitle.Render("  Commands") + "\n")
	for _, r := range slashCommands {
		b.WriteString("  " + stAccent.Render(padRight(r.name, 12)) + " " + stDim.Render(r.desc) + "\n")
	}

	b.WriteString("\n" + stTitle.Render("  Keys") + "\n")
	keys := [][2]string{
		{"Enter", "send · Alt+Enter for a newline"},
		{"Ctrl+V", "paste an image from the clipboard"},
		{"PgUp/PgDn", "scroll the transcript · Esc interrupts a running reply"},
		{"Ctrl+C", "press twice to exit (once clears the input / interrupts)"},
		{"/", "open the command menu (↑/↓ move · Tab complete · Enter run)"},
	}
	for _, k := range keys {
		b.WriteString("  " + stAccent.Render(padRight(k[0], 12)) + " " + stDim.Render(k[1]) + "\n")
	}

	b.WriteString("\n" + stTitle.Render("  Permission modes") + stDim.Render("  (switch with /permissions)") + "\n")
	for _, o := range permOptions {
		b.WriteString("  " + stAccent.Render(padRight(o.label, 20)) + " " + stDim.Render(o.desc) + "\n")
	}

	b.WriteString("\n  " + stDim.Render("Tip: drop an image path into your message, e.g. ") + stAccent.Render("explain ./diagram.png"))
	return strings.TrimRight(b.String(), "\n")
}

const initPrompt = `Explore this project — list the directory, read the README and any package manifests and entry points — then create a file named NOCTURNE.md at the project root. Document concisely: what the project is, how to build / run / test it, the directory layout, and key conventions. Use write_file to create it, then give a one-line summary.`

// displayModel strips the internal provider prefix (e.g. "navy:") so model ids
// read cleanly in the UI. The full id is still used when talking to the API.
func displayModel(id string) string {
	return strings.TrimPrefix(id, "navy:")
}

// resolveModelArg maps a user-typed model name to a real model id, accepting
// either the full id or the clean (prefix-stripped) display form.
func (m *tuiModel) resolveModelArg(arg string) string {
	arg = strings.TrimSpace(arg)
	for _, md := range m.models {
		if md.ID == arg || displayModel(md.ID) == arg {
			return md.ID
		}
	}
	if md, ok := knownModelInfo(arg); ok {
		return md.ID
	}
	return normalizeModelID(arg)
}

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
