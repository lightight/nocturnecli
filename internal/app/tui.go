package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// autoCompactFraction is the share of the selected model's context window at
// which the conversation is summarised automatically, leaving headroom for the
// next user turn, system prompt, tool results, and model output.
const autoCompactFraction = 0.90

// maxInputRows caps how tall the input box can grow as text wraps.
const maxInputRows = 10

const (
	goalSubagentTimeout = 8 * time.Hour
	goalSubagentRounds  = 100
	// maxStreamRecoveries bounds automatic retries after an interrupted stream so
	// a persistently failing upstream cannot loop and burn API calls forever.
	maxStreamRecoveries = 2
)

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
	{"/tools", "list installed custom tools"},
	{"/report", "anonymous e2e-encrypted debug report — view · send · never"},
	{"/tool-import", "install a tool/skill from one URL or path"},
	{"/tool-add", "install one custom shell tool"},
	{"/tool-remove", "remove an installed custom tool"},
	{"/image", "attach an image file"},
	{"/cd", "change the working directory"},
	{"/usage", "usage dashboard — status · usage · stats"},
	{"/cowork", "computer use — see & control this computer"},
	{"/plan", "plan mode — read-only exploration, approve to execute"},
	{"/goal", "goal mode — autonomous long-running task mode"},
	{"/compact", "summarize the conversation to free up context"},
	{"/skip-questions", "toggle auto-skipping AI question popups"},
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

	mode     mode
	spinning bool
	cowork   bool // computer-use mode: screen control + whole filesystem
	plan     bool // plan mode: read-only exploration until the user approves
	goal     bool // goal mode: long autonomous tasks with background work

	lines       []string // transcript blocks (the scrollback)
	messages    []ChatMessage
	attachments []Image
	queuedInput []string // prompts/slash commands submitted while the agent is busy

	pending          []ToolCall
	results          []toolResult
	confirm          ToolCall
	guardReason      string // why the guard flagged m.confirm (empty for a plain ask)
	emptyNudges      int    // consecutive empty-reply nudges sent this turn
	streamRecoveries int    // consecutive interrupted-stream retries this turn
	badCalls         badCallBreaker

	// anonymous problem reports (opt-in, e2e-encrypted)
	health      *healthTracker
	reportAsked bool // hint already shown this session

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

	// completed background commands/sub-agent batches waiting to be handed back to the AI
	backgroundDone []backgroundCommandResult
	subagentDone   []subagentBatchResult

	// compact sub-agent progress grid
	subagents      map[string]*subagentBatchView
	subagentOrder  []string
	nextSubagentID int

	// /remote connection progress (async handshake with the relay)
	remoteConnecting bool
	remoteFrame      int
}

// currentVision reports whether the selected model can see attached images.
func (m *tuiModel) currentVision() bool {
	return m.client.supportsVision(m.cfg.Model)
}

func (m *tuiModel) currentModelInfo() (ModelInfo, bool) {
	id := normalizeModelID(m.cfg.Model)
	for _, md := range m.models {
		if normalizeModelID(md.ID) == id {
			return md, true
		}
	}
	return knownModelInfo(id)
}

func (m *tuiModel) contextLimit() int {
	if md, ok := m.currentModelInfo(); ok && md.MaxTokens > 0 {
		return md.MaxTokens
	}
	return fallbackContextLimit(m.cfg.Model)
}

func (m *tuiModel) autoCompactThreshold() int {
	limit := m.contextLimit()
	if limit <= 0 {
		return 0
	}
	return int(float64(limit) * autoCompactFraction)
}

func (m *tuiModel) shouldAutoCompact() bool {
	threshold := m.autoCompactThreshold()
	return threshold > 0 && m.ctxTokens >= threshold && !m.compacting
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

type backgroundCommandDoneMsg struct{ result backgroundCommandResult }

type subagentTaskSpec struct {
	ID     string
	Prompt string
	Label  string
}

type subagentTaskState struct {
	ID      string
	Label   string
	Percent int
	Latest  string
	Done    bool
	Err     string
}

type subagentBatchView struct {
	ID         string
	Label      string
	Background bool
	Started    time.Time
	Tasks      []*subagentTaskState
}

type subagentProgressMsg struct {
	BatchID string
	TaskID  string
	Percent int
	Latest  string
	Done    bool
	Err     string
}

type subagentBatchResult struct {
	BatchID    string
	Background bool
	Report     string
}

type subagentBatchDoneMsg struct {
	Result subagentBatchResult
}

func remoteTickCmd() tea.Cmd {
	return tea.Tick(90*time.Millisecond, func(time.Time) tea.Msg { return remoteTickMsg{} })
}

func waitBackgroundCommandDoneCmd() tea.Cmd {
	return func() tea.Msg {
		return backgroundCommandDoneMsg{result: <-backgroundCommandResults}
	}
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
		health:          newHealthTracker(),
		remoteDraftSent: "\xff",
		subagents:       map[string]*subagentBatchView{},
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
	// In the alt-screen there is no native scrollback. Instead of enabling full
	// mouse tracking (which steals drag selection from the terminal), ask xterm-
	// compatible terminals to translate wheel events into cursor keys while the
	// alt-screen is active. This preserves normal click/drag text selection.
	fmt.Fprint(os.Stdout, "\x1b[?1007h")
	defer fmt.Fprint(os.Stdout, "\x1b[?1007l")

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithFilter(filterLeaks))
	m.program = p
	_, err := p.Run()
	m.remote.Stop()
	return err
}

func (m *tuiModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.fetchModelsCmd(""), m.validateKeyCmd(), m.startupUpdateCmd(), waitBackgroundCommandDoneCmd())
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

	case reportDoneMsg:
		if msg.err != nil {
			m.push(stErr.Render("  ✗ couldn't send report: " + msg.err.Error()))
		} else {
			m.push(stOK.Render("  ✓ report sent — sealed so only the team can open it. thank you!"))
		}
		return m, nil

	case streamDeltaMsg:
		return m.handleStreamDelta(msg)

	case toolDoneMsg:
		return m.handleToolDone(msg)

	case subagentProgressMsg:
		m.handleSubagentProgress(msg)
		return m, nil

	case subagentBatchDoneMsg:
		return m.handleSubagentBatchDone(msg.Result)

	case backgroundCommandDoneMsg:
		return m.handleBackgroundCommandDone(msg.result)

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
		// Route the mouse wheel to the transcript so scrolling up reveals
		// earlier output (the alt-screen has no native scrollback). Other mouse
		// events are ignored so drag selection remains controlled by the terminal.
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
		// Route arrows to the transcript wherever they can't do anything else:
		// in the input when it has a single line (arrows can't move the cursor
		// there) and no "/" menu is open. While the bot is busy, still let
		// arrows edit a multi-line draft; otherwise they keep scrolling the
		// transcript.
		busy := m.mode == modeThinking || m.mode == modeStreaming
		idleInput := m.mode == modeInput && !m.showSlash && !strings.Contains(m.ta.Value(), "\n")
		busySingleLineDraft := busy && !strings.Contains(m.ta.Value(), "\n")
		if idleInput || busySingleLineDraft {
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
			return m, nil
		}
		return m.handleBusyInputKey(msg)
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

func (m *tuiModel) handleBusyInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Let the user prepare input while the model is thinking, streaming, or
	// running tools. Slash commands execute immediately; normal messages queue
	// until the current agent turn settles so we don't start a second request.
	if msg.Paste {
		if img, ok := tryPasteImage(string(msg.Runes), m.work); ok {
			m.attachments = append(m.attachments, img)
			m.follow = true
			m.syncViewport()
			return m, nil
		}
	}

	switch msg.Type {
	case tea.KeyCtrlD:
		m.quitting = true
		return m, tea.Quit
	case tea.KeyCtrlV:
		return m, grabImageCmd()
	case tea.KeyEnter:
		if msg.Alt {
			m.ta.InsertString("\n")
			m.refreshSlash()
			return m, nil
		}
		return m.queueBusyDraft()
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.refreshSlash()
	return m, cmd
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
	m.push(renderUser(text, len(imgs), m.width))
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
	m.streamRecoveries = 0
	m.messages = append(m.messages, ChatMessage{Role: "user", Content: text})
	m.noteTitle(text)
	m.push(renderUser(text, 0, m.width))
	return m, m.startReply()
}

func (m *tuiModel) queueBusyDraft() (tea.Model, tea.Cmd) {
	raw := strings.TrimSpace(m.ta.Value())
	if raw == "" {
		return m, nil
	}
	m.ta.Reset()
	m.showSlash = false
	m.refreshSlash()
	if strings.HasPrefix(raw, "/") && !m.slashStartsReply(raw) {
		return m.runSlash(raw, false)
	}
	m.queueText(raw)
	return m, nil
}

// slashStartsReply reports whether a slash command would start another agent
// request. While a turn is busy those commands must queue like normal prompts;
// running them immediately would replace the active stream and mix replies.
func (m *tuiModel) slashStartsReply(raw string) bool {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return false
	}
	cmd := strings.ToLower(fields[0])
	arg := strings.TrimSpace(strings.TrimPrefix(raw, fields[0]))
	switch cmd {
	case "/goal":
		switch strings.ToLower(arg) {
		case "", "on", "start", "true", "1", "off", "stop", "false", "0":
			return false
		default:
			return true
		}
	case "/plan":
		return m.plan // the second /plan approves the plan and starts execution
	case "/compact":
		return len(m.messages) > 0
	case "/init":
		return true
	default:
		return false
	}
}

func (m *tuiModel) queueText(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	m.queuedInput = append(m.queuedInput, text)
	note := "queued: " + oneLine(text, 80)
	m.push(stHint.Render("  " + note))
	m.toRemote("status", note)
}

func (m *tuiModel) runQueuedInput() (bool, tea.Cmd) {
	if len(m.queuedInput) == 0 || m.busy() || m.mode != modeInput {
		return false, nil
	}
	text := m.queuedInput[0]
	m.queuedInput = m.queuedInput[1:]
	if strings.HasPrefix(text, "/") {
		_, cmd := m.runSlash(text, false)
		return true, cmd
	}
	_, cmd := m.submitText(text)
	return true, cmd
}

func (m *tuiModel) settleIdleCmd() tea.Cmd {
	if ran, cmd := m.runQueuedInput(); ran {
		return cmd
	}
	if len(m.backgroundDone) > 0 {
		return m.startBackgroundCommandReply()
	}
	if len(m.subagentDone) > 0 {
		return m.startBackgroundSubagentReply()
	}
	if m.shouldAutoCompact() {
		return m.compactCmd(true)
	}
	return nil
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

func (m *tuiModel) busy() bool {
	return m.mode == modeThinking || m.mode == modeStreaming || m.mode == modeConfirm || m.mode == modeAsk
}

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
	go m.client.ChatStream(ctx, systemPromptModeWithTools(m.work, m.cowork, m.plan, m.goal, m.cfg.Level == "extended", m.cfg.Tools), append([]ChatMessage(nil), m.messages...), ch)
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
	system := systemPromptModeWithTools(m.work, m.cowork, m.plan, m.goal, m.cfg.Level == "extended", m.cfg.Tools)
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
	if canonicalTool(tc.Name) == "install_skill" {
		cfg := m.cfg
		return func() tea.Msg {
			out := installSkillTool(cfg, tc.Args)
			return toolDoneMsg{name: tc.Name, output: out}
		}
	}
	if t, ok := findCustomTool(m.cfg.Tools, tc.Name); ok {
		work := m.work
		return func() tea.Msg {
			out := executeCustomTool(work, t, tc.Args)
			return toolDoneMsg{name: tc.Name, output: out}
		}
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
		out, img := executeWithImageWithTools(tc, work, vision, describe, m.cfg.Tools)
		add, del := codeChange(tc, out)
		return toolDoneMsg{name: tc.Name, output: out, image: img, added: add, removed: del}
	}
}

// runTaskCmd runs one or more nested sub-agent loops. Args may be either the
// classic {"prompt":"...","description":"..."} shape or a batch:
// {"tasks":[{"prompt":"...","description":"..."}],"background":true}.
func (m *tuiModel) runTaskCmd(tc ToolCall) tea.Cmd {
	tasks := m.taskSpecsFromArgs(tc.Args)
	if len(tasks) == 0 {
		return func() tea.Msg {
			return toolDoneMsg{name: tc.Name, output: "Error: task requires a prompt or non-empty tasks array"}
		}
	}
	m.nextSubagentID++
	batchID := fmt.Sprintf("subagents-%d", m.nextSubagentID)
	background := argBool(tc.Args, "background") || strings.EqualFold(argStr(tc.Args, "mode"), "background")
	if argBool(tc.Args, "wait") {
		background = false
	}
	m.registerSubagentBatch(batchID, tasks, background)
	where := ""
	if background {
		where = " in background"
	}
	m.push(stHint.Render(fmt.Sprintf("  ⏳ %d sub-agent%s started%s", len(tasks), plural(len(tasks)), where)))
	cfg := m.cfg
	client := m.client
	work := m.work
	goal := m.goal
	program := m.program
	return func() tea.Msg {
		result := runSubagentBatch(batchID, tasks, background, goal, cfg, client, work, program)
		if background {
			if program != nil {
				program.Send(subagentBatchDoneMsg{Result: result})
			}
			return toolDoneMsg{name: tc.Name, output: fmt.Sprintf("Started %d background sub-agent%s. The CLI will report back when all are finished.", len(tasks), plural(len(tasks)))}
		}
		return toolDoneMsg{name: tc.Name, output: result.Report}
	}
}

// pushToolCall prints a tool-call header.
func (m *tuiModel) pushToolCall(tc ToolCall) {
	m.push(renderToolCall(tc))
}

func (m *tuiModel) taskSpecsFromArgs(args map[string]interface{}) []subagentTaskSpec {
	return taskSpecsFromArgs(args)
}

func taskSpecsFromArgs(args map[string]interface{}) []subagentTaskSpec {
	var specs []subagentTaskSpec
	add := func(prompt, label string) {
		prompt = strings.TrimSpace(prompt)
		if prompt == "" {
			return
		}
		label = strings.TrimSpace(label)
		if label == "" {
			label = prompt
		}
		specs = append(specs, subagentTaskSpec{ID: fmt.Sprintf("t%d", len(specs)+1), Prompt: prompt, Label: oneLine(label, 70)})
	}
	if raw, ok := args["tasks"]; ok {
		for _, it := range toInterfaceSlice(raw) {
			switch v := it.(type) {
			case string:
				add(v, "")
			case map[string]interface{}:
				add(argStr(v, "prompt"), firstNonEmpty(argStr(v, "description"), argStr(v, "label")))
			}
		}
	}
	if raw, ok := args["prompts"]; ok {
		for _, it := range toInterfaceSlice(raw) {
			if s, ok := it.(string); ok {
				add(s, "")
			}
		}
	}
	add(argStr(args, "prompt"), firstNonEmpty(argStr(args, "description"), argStr(args, "label")))
	return specs
}

func toInterfaceSlice(v interface{}) []interface{} {
	switch x := v.(type) {
	case []interface{}:
		return x
	case []string:
		out := make([]interface{}, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	default:
		return nil
	}
}

func (m *tuiModel) registerSubagentBatch(batchID string, tasks []subagentTaskSpec, background bool) {
	if m.subagents == nil {
		m.subagents = map[string]*subagentBatchView{}
	}
	view := &subagentBatchView{ID: batchID, Background: background, Started: time.Now()}
	for _, t := range tasks {
		view.Tasks = append(view.Tasks, &subagentTaskState{ID: t.ID, Label: t.Label, Percent: 1, Latest: "queued"})
	}
	m.subagents[batchID] = view
	m.subagentOrder = append(m.subagentOrder, batchID)
}

func runSubagentBatch(batchID string, tasks []subagentTaskSpec, background, goal bool, cfg *Config, client *Client, work string, program *tea.Program) subagentBatchResult {
	timeout := 10 * time.Minute
	maxRounds := maxSubagentRounds
	if goal {
		timeout = goalSubagentTimeout
		maxRounds = goalSubagentRounds
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	type itemResult struct {
		idx    int
		label  string
		report string
		err    error
	}
	results := make([]itemResult, len(tasks))
	ch := make(chan itemResult, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		i, task := i, task
		wg.Add(1)
		go func() {
			defer wg.Done()
			send := func(p subagentProgress) {
				if program != nil {
					program.Send(subagentProgressMsg{BatchID: batchID, TaskID: task.ID, Percent: p.Percent, Latest: p.Latest})
				}
			}
			report, err := runSubagentWithProgress(ctx, cfg, client, work, task.Prompt, maxRounds, send)
			latest := "finished"
			if err != nil {
				latest = "failed: " + oneLine(err.Error(), 90)
			}
			if program != nil {
				program.Send(subagentProgressMsg{BatchID: batchID, TaskID: task.ID, Percent: 100, Latest: latest, Done: true, Err: errString(err)})
			}
			ch <- itemResult{idx: i, label: task.Label, report: report, err: err}
		}()
	}
	wg.Wait()
	close(ch)
	for r := range ch {
		results[r.idx] = r
	}
	var b strings.Builder
	if background {
		b.WriteString("Background sub-agent batch completed. Continue responding to the user based on these results.\n")
	} else {
		b.WriteString("Sub-agent batch completed.\n")
	}
	for _, r := range results {
		b.WriteString("\n## ")
		b.WriteString(r.label)
		b.WriteString("\n")
		if r.err != nil {
			b.WriteString("Error: ")
			b.WriteString(r.err.Error())
			b.WriteString("\n")
			continue
		}
		b.WriteString(strings.TrimSpace(r.report))
		b.WriteString("\n")
	}
	return subagentBatchResult{BatchID: batchID, Background: background, Report: strings.TrimSpace(b.String())}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
		return m, m.settleIdleCmd()
	}
	return m.finishReply(msg.text, msg.usage, msg.quota)
}

func (m *tuiModel) handleStreamDelta(msg streamDeltaMsg) (tea.Model, tea.Cmd) {
	ev := msg.ev
	if ev.Err != nil {
		if cmd := m.recoverStreamError(ev.Err); cmd != nil {
			return m, cmd
		}
		m.replyError(ev.Err)
		return m, m.settleIdleCmd()
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
	if errors.Is(err, ErrStreamClosedEarly) {
		m.health.recordStreamError()
	} else {
		m.health.recordAPIError()
	}
	m.maybeReportHint()
}

// maybeReportHint nudges the user toward /report once per session when the
// session has hit enough reliability hiccups. Nothing is ever sent without an
// explicit /report send.
func (m *tuiModel) maybeReportHint() {
	if m.reportAsked || m.cfg.ReportOptOut || m.health.issues() < reportHintThreshold {
		return
	}
	m.reportAsked = true
	m.push(stHint.Render(fmt.Sprintf("  ◗ nocturne hit %d hiccups this session — /report view shows an anonymous, end-to-end-encrypted debug report; /report send shares it with the team (nothing is sent automatically)", m.health.issues())))
}

type reportDoneMsg struct{ err error }

// runReportSlash handles /report [view|send|never]. The payload is shown in
// full before any send so the user can verify it carries nothing identifying.
func (m *tuiModel) runReportSlash(arg string) (tea.Model, tea.Cmd) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "never", "off", "optout":
		m.cfg.ReportOptOut = true
		if err := m.cfg.Save(); err != nil {
			m.push(stErr.Render("  couldn't save preference: " + err.Error()))
			break
		}
		m.push(stOK.Render("  ✓ got it — no more report hints (and reports are never sent automatically anyway)"))
	case "send":
		payload := buildReport(m.health, m.cfg, m.ver, len(m.messages), m.sessionStart)
		if payload.Counts["rounds"] == 0 && m.health.issues() == 0 {
			m.push(stHint.Render("  no hiccups recorded this session — sending anyway, it helps calibrate the baseline"))
		}
		m.push(stHint.Render("  sending anonymous report " + payload.ID + "…"))
		cfg := m.cfg
		return m, func() tea.Msg { return reportDoneMsg{err: sendReport(cfg, payload)} }
	case "", "view":
		payload := buildReport(m.health, m.cfg, m.ver, len(m.messages), m.sessionStart)
		pretty, _ := json.MarshalIndent(payload, "", "  ")
		m.push(stDim.Render("  this is the ENTIRE report — no prompts, paths, commands, file contents, or account data:"))
		m.push("```json\n" + string(pretty) + "\n```")
		m.push(stHint.Render("  /report send to share it (e2e-encrypted to the team's key) · /report never to hide these hints"))
	default:
		m.push(stErr.Render("  usage: /report [view|send|never]"))
	}
	return m, nil
}

func (m *tuiModel) finishReply(text string, usage Usage, quota Quota) (tea.Model, tea.Cmd) {
	m.cancel = nil
	m.streamCh = nil
	m.streamBuf = ""
	m.streamRecoveries = 0

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
		if stalledReply(narration, calls) && m.emptyNudges < maxEmptyNudges {
			m.emptyNudges++
			m.health.recordEmptyReply()
			m.push(stHint.Render("  ↻ reply had no tool call — asking the model to continue"))
			m.messages = append(m.messages, ChatMessage{Role: "user", Content: emptyReplyNudge})
			return m, m.startReply()
		}
		m.emptyNudges = 0
		m.mode = modeInput
		if out := m.renderAssistant(narration); out != "" {
			m.push(out)
		}
		m.toRemote("assistant", narration)
		m.persistSession()
		m.maybeReportHint()
		return m, m.settleIdleCmd()
	}
	m.emptyNudges = 0

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
			return m.settleIdleCmd()
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

	if out, bad := diagnoseBadToolCall(tc, m.cfg.Tools); bad {
		m.pushToolCall(tc)
		m.health.recordBadCall(tc, out)
		if m.badCalls.add(tc) {
			m.health.recordBreakerTrip()
			m.push(stErr.Render(fmt.Sprintf("  ✗ model repeated the same bad tool call (%s) %d times — stopping here so it doesn't burn API calls. Try rephrasing, or a different model.", tc.Name, maxBadCallRepeats)))
			m.pending = nil
			m.results = nil
			m.mode = modeInput
			m.persistSession()
			m.maybeReportHint()
			return m.settleIdleCmd()
		}
		m.push(stErr.Render("  ✗ bad tool call — automatically asking model to retry"))
		m.toRemote("tool", "● "+tc.summarize())
		m.pending = m.pending[1:]
		m.results = append(m.results, toolResult{Name: tc.Name, Output: out})
		return m.advanceTools()
	}
	m.badCalls.reset()

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

	if _, ok := findCustomTool(m.cfg.Tools, tc.Name); ok {
		m.mode = modeConfirm
		m.confirm = tc
		m.guardReason = ""
		m.toRemote("status", "waiting for approval in the terminal: "+tc.summarize())
		return nil
	}

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
	return m.settleIdleCmd()
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
	if m.cfg.SkipQuestions {
		if len(m.pending) > 0 {
			m.pending = m.pending[1:]
		}
		out := "Question skipped because /skip-questions is on. Continue with the safest reasonable default, or proceed without that information if possible. Question was: " + m.askQ
		if len(m.askOpts) > 0 {
			out += " Options: " + strings.Join(m.askOpts, ", ")
		}
		m.results = append(m.results, toolResult{Name: "ask", Output: out})
		m.push(stHint.Render("  ↷ question skipped (/skip-questions on): " + oneLine(m.askQ, 120)))
		m.toRemote("tool", "  └ question skipped")
		m.askQ = ""
		m.askOpts = nil
		m.askSel = 0
		return m.advanceTools()
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
	m.mode = modeThinking
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

func (m *tuiModel) handleSubagentProgress(msg subagentProgressMsg) {
	b := m.subagents[msg.BatchID]
	if b == nil {
		return
	}
	for _, t := range b.Tasks {
		if t.ID != msg.TaskID {
			continue
		}
		t.Percent = msg.Percent
		t.Latest = msg.Latest
		t.Done = msg.Done
		t.Err = msg.Err
		break
	}
	m.syncViewport()
}

func (m *tuiModel) handleSubagentBatchDone(r subagentBatchResult) (tea.Model, tea.Cmd) {
	m.markSubagentBatchDone(r.BatchID)
	m.push(renderToolResult("Background sub-agent batch finished.\n" + r.Report))
	m.toRemote("tool", "  └ background sub-agents finished")
	cmd := waitBackgroundCommandDoneCmd()
	if m.busy() || m.mode != modeInput {
		m.subagentDone = append(m.subagentDone, r)
		return m, cmd
	}
	m.subagentDone = append(m.subagentDone, r)
	return m, tea.Batch(cmd, m.startBackgroundSubagentReply())
}

func (m *tuiModel) markSubagentBatchDone(batchID string) {
	b := m.subagents[batchID]
	if b == nil {
		return
	}
	for _, t := range b.Tasks {
		t.Done = true
		if t.Percent < 100 {
			t.Percent = 100
		}
		if t.Latest == "" || t.Latest == "queued" {
			t.Latest = "finished"
		}
	}
}

func (m *tuiModel) handleBackgroundCommandDone(r backgroundCommandResult) (tea.Model, tea.Cmd) {
	m.push(renderToolResult(formatBackgroundCommandNotification(r)))
	m.toRemote("tool", "  └ background finished: "+oneLine(r.Command, 80))
	cmd := waitBackgroundCommandDoneCmd()
	if m.busy() || m.mode != modeInput {
		m.backgroundDone = append(m.backgroundDone, r)
		return m, cmd
	}
	m.backgroundDone = append(m.backgroundDone, r)
	return m, tea.Batch(cmd, m.startBackgroundCommandReply())
}

func (m *tuiModel) startBackgroundCommandReply() tea.Cmd {
	if len(m.backgroundDone) == 0 || m.busy() || m.mode != modeInput {
		return nil
	}
	r := m.backgroundDone[0]
	m.backgroundDone = m.backgroundDone[1:]
	m.messages = append(m.messages, ChatMessage{Role: "user", Content: buildBackgroundCommandResult(r)})
	return m.startReply()
}

func (m *tuiModel) startBackgroundSubagentReply() tea.Cmd {
	if len(m.subagentDone) == 0 || m.busy() || m.mode != modeInput {
		return nil
	}
	r := m.subagentDone[0]
	m.subagentDone = m.subagentDone[1:]
	m.messages = append(m.messages, ChatMessage{Role: "user", Content: r.Report})
	return m.startReply()
}

func formatBackgroundCommandNotification(r backgroundCommandResult) string {
	status := "finished"
	if r.Err != nil {
		status = "failed: " + oneLine(r.Err.Error(), 200)
	}
	return fmt.Sprintf("Background command %s (pid %d, ran %s). Log: %s\n%s", status, r.PID, r.Finished.Sub(r.Started).Round(time.Millisecond), r.LogPath, tailFileForPrompt(r.LogPath))
}

func buildBackgroundCommandResult(r backgroundCommandResult) string {
	status := "finished successfully"
	if r.Err != nil {
		status = "exited with error: " + r.Err.Error()
	}
	return fmt.Sprintf("Background command completed. Continue responding to the user based on this result.\n\nCommand: %s\nPID: %d\nStatus: %s\nRuntime: %s\nLog path: %s\n\nRecent log output:\n%s", r.Command, r.PID, status, r.Finished.Sub(r.Started).Round(time.Millisecond), r.LogPath, tailFileForPrompt(r.LogPath))
}

func tailFileForPrompt(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(could not read log: " + err.Error() + ")"
	}
	const max = 12000
	if len(b) > max {
		b = b[len(b)-max:]
		if i := bytes.IndexByte(b, '\n'); i >= 0 && i+1 < len(b) {
			b = b[i+1:]
		}
		return "…\n" + string(b)
	}
	if len(b) == 0 {
		return "(log is empty)"
	}
	return string(b)
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
		return m, m.settleIdleCmd()
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
			m.push(renderUser(c, len(msg.Images), m.width))
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
	if strings.HasPrefix(text, "/") {
		m.toRemote("user", text)
		before := len(m.lines)
		next, cmd := m.runSlash(text, true)
		if tm, ok := next.(*tuiModel); ok {
			tm.forwardRemoteFeedback(before)
		}
		return next, cmd
	}
	if m.busy() {
		m.queueText(text)
		return m, nil
	}
	m.follow = true
	m.messages = append(m.messages, ChatMessage{Role: "user", Content: text})
	m.noteTitle(text)
	m.push(renderUser(text, 0, m.width) + " " + stDim.Render("(remote)"))
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
// still-open tool blocks become a tidy "● preparing tool call…" line instead
// of raw tool tags scrolling by.
// cleanToolStream scans instead of regex-replacing so huge tool arguments never
// grow the visible preview and partial blocks are hidden immediately.
func cleanToolStream(buf, marker string) string {
	lower := strings.ToLower(buf)
	openResult := toolOpenTag + "_result"
	resultClose := string(rune(60)) + "/tool_result>"
	var b strings.Builder
	for pos := 0; pos < len(buf); {
		i := strings.Index(lower[pos:], toolOpenTag)
		if i < 0 {
			b.WriteString(buf[pos:])
			break
		}
		i += pos
		b.WriteString(buf[pos:i])

		if strings.HasPrefix(lower[i:], openResult) {
			close := strings.Index(lower[i:], resultClose)
			if close < 0 {
				pos = len(buf)
			} else {
				pos = i + close + len(resultClose)
			}
			continue
		}

		b.WriteString(marker)
		gtRel := strings.IndexByte(buf[i:], '>')
		if gtRel < 0 {
			pos = len(buf)
			break
		}
		bodyStart := i + gtRel + 1
		_, closeEnd := findCloseToolTag(buf, bodyStart)
		if closeEnd < 0 {
			pos = len(buf)
			break
		}
		pos = closeEnd
	}
	return strings.TrimRight(b.String(), " \n")
}

func streamPreview(buf string) string {
	return cleanToolStream(buf, stAccent.Render("●")+" "+stDim.Render("preparing tool call…"))
}

func (m *tuiModel) recoverStreamError(err error) tea.Cmd {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	text := m.streamBuf
	hasPartial := strings.TrimSpace(text) != ""
	if m.streamRecoveries >= maxStreamRecoveries || (!hasPartial && !errors.Is(err, ErrStreamClosedEarly) && !isTransientAIError(err)) {
		return nil
	}
	m.streamRecoveries++
	if m.health != nil {
		m.health.recordStreamError()
	}
	m.cancel = nil
	m.streamCh = nil
	m.streamBuf = ""

	if !hasPartial {
		m.push(stHint.Render("  ↻ stream interrupted before the reply started — retrying"))
		return m.startReply()
	}

	// Keep the partial assistant turn in history so the retry can see exactly
	// where it stopped instead of guessing from a detached error message.
	m.messages = append(m.messages, ChatMessage{Role: "assistant", Content: text})
	narration, calls := parseResponse(text)
	if narration != "" {
		m.push(m.renderAssistant(narration))
		m.toRemote("stream", narration)
	}

	if hasIncompleteToolBlock(text) {
		m.push(stHint.Render("  ↻ stream ended mid-tool-call — asking the model to resend it"))
		m.messages = append(m.messages, ChatMessage{Role: "user", Content: toolCallRecoveryPrompt(text)})
		return m.startReply()
	}
	if len(calls) > 0 {
		m.push(stHint.Render("  ↻ stream ended early — recovered complete tool call(s)"))
		m.pending = calls
		m.results = nil
		return m.advanceTools()
	}

	m.push(stHint.Render("  ↻ stream interrupted — asking the model to continue"))
	m.messages = append(m.messages, ChatMessage{Role: "user", Content: streamContinuationPrompt})
	return m.startReply()
}

const streamContinuationPrompt = "The stream was interrupted after your previous partial reply. Continue from exactly where it stopped. Do not repeat text or tool calls already present above."

func hasIncompleteToolBlock(text string) bool {
	lower := strings.ToLower(text)
	openResult := toolOpenTag + "_result"
	for pos := 0; pos < len(text); {
		i := strings.Index(lower[pos:], toolOpenTag)
		if i < 0 {
			return false
		}
		i += pos
		if strings.HasPrefix(lower[i:], openResult) {
			pos = i + len(toolOpenTag)
			continue
		}
		gtRel := strings.IndexByte(text[i:], '>')
		if gtRel < 0 {
			return true
		}
		bodyStart := i + gtRel + 1
		_, closeEnd := findCloseToolTag(text, bodyStart)
		if closeEnd < 0 {
			return true
		}
		pos = closeEnd
	}
	return false
}

func toolCallRecoveryPrompt(text string) string {
	name := ""
	if i := strings.LastIndex(strings.ToLower(text), toolOpenTag); i >= 0 {
		if gt := strings.IndexByte(text[i:], '>'); gt >= 0 {
			name, _ = parseToolStartName(text[i : i+gt+1])
		} else if m := toolStartName.FindStringSubmatch(text[i:]); len(m) == 2 {
			name = m[1]
		}
	}
	which := ""
	if name != "" {
		which = " for " + name
	}
	return "Your previous tool call" + which + " was incomplete because the stream ended before a closing tool tag, so no tool ran. Re-send exactly one complete tool block with one valid JSON object and a closing tool tag. Do not include prose before the tool call."
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
	if grid := m.subagentGridView(); grid != "" {
		b.WriteString(grid)
		b.WriteString("\n")
	}
	b.WriteString(m.inputBox())
	b.WriteString("\n")
	b.WriteString(m.statusLine())
	return b.String()
}

func (m *tuiModel) subagentGridView() string {
	if len(m.subagentOrder) == 0 {
		return ""
	}
	var rows []string
	for _, id := range m.subagentOrder {
		b := m.subagents[id]
		if b == nil {
			continue
		}
		active := false
		for _, t := range b.Tasks {
			if !t.Done {
				active = true
				break
			}
		}
		if !active {
			continue
		}
		for _, t := range b.Tasks {
			mark := "◌"
			style := stHint
			if t.Done {
				mark = "✓"
				style = stOK
			}
			if t.Err != "" {
				mark = "✗"
				style = stErr
			}
			latest := t.Latest
			if latest == "" {
				latest = "working"
			}
			rows = append(rows, fmt.Sprintf("%s %3d%% %s — %s", style.Render(mark), t.Percent, oneLine(t.Label, 34), stDim.Render(oneLine(latest, 54))))
		}
	}
	if len(rows) == 0 {
		return ""
	}
	if len(rows) > 8 {
		rows = append(rows[:8], stDim.Render(fmt.Sprintf("  … %d more sub-agents", len(rows)-8)))
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
		Render(stTitle.Render("Sub-agents") + "\n" + strings.Join(rows, "\n"))
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
		return " " + m.sp.View() + " " + stDim.Render(fmt.Sprintf("Thinking… (%s · esc to interrupt)", m.elapsed())) + stHint.Render(" · "+m.contextUsageString())
	case modeStreaming:
		if strings.TrimSpace(m.streamBuf) == "" {
			return " " + m.sp.View() + " " + stDim.Render(fmt.Sprintf("Thinking… (%s · esc to interrupt)", m.elapsed())) + stHint.Render(" · "+m.contextUsageString())
		}
		return " " + m.sp.View() + " " + stDim.Render(fmt.Sprintf("streaming… (%s · esc to interrupt)", m.elapsed())) + stHint.Render(" · "+m.contextUsageString())
	default:
		scroll := "wheel/pgup/pgdn scroll · drag selects"
		line := "  " + m.contextUsageString() + " · enter ↵ send · alt+↵ newline · ctrl+v paste image · " + scroll + " · / commands"
		var badges []string
		if m.cowork {
			badges = append(badges, "cowork")
		}
		if m.goal {
			badges = append(badges, "goal")
		}
		if len(badges) > 0 {
			line = stAccent.Render("  "+strings.Join(badges, " · ")) + stHint.Render(" · ") + strings.TrimLeft(line, " ")
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

func (m *tuiModel) enableGoalMode() {
	if m.cfg.Level != "extended" {
		m.cfg.Level = "extended"
		_ = m.cfg.Save()
	}
	m.push(stOK.Render("  goal mode on — autonomous long-running tasks enabled"))
	m.push(stHint.Render("  thinking level set to extended; use /goal off to disable goal mode"))
}

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
	case "/tools":
		m.push(m.customToolsList())
	case "/report":
		return m.runReportSlash(arg)
	case "/skip-questions", "/questions":
		valid := true
		switch strings.ToLower(arg) {
		case "", "toggle":
			m.cfg.SkipQuestions = !m.cfg.SkipQuestions
		case "on", "true", "1", "yes":
			m.cfg.SkipQuestions = true
		case "off", "false", "0", "no":
			m.cfg.SkipQuestions = false
		default:
			valid = false
		}
		if !valid {
			m.push(stErr.Render("  usage: /skip-questions [on|off]"))
			break
		}
		if err := m.cfg.Save(); err != nil {
			m.push(stErr.Render("  couldn't save setting: " + err.Error()))
			break
		}
		if m.cfg.SkipQuestions {
			m.push(stOK.Render("  AI question popups will be skipped"))
		} else {
			m.push(stHint.Render("  AI question popups enabled"))
		}
	case "/tool-import":
		if arg == "" {
			m.push(stErr.Render("  usage: /tool-import <tool-or-skill-url-or-path>"))
			break
		}
		msg, err := installSkill(m.cfg, arg)
		if err != nil {
			m.push(stErr.Render("  couldn't import tool: " + err.Error()))
			break
		}
		m.push(stOK.Render("  tool imported — " + msg))
	case "/install":
		if arg == "" {
			m.push(stErr.Render("  usage: /install <skill-url-or-path>"))
			break
		}
		msg, err := installSkill(m.cfg, arg)
		if err != nil {
			m.push(stErr.Render("  couldn't install skill: " + err.Error()))
			break
		}
		m.push(stOK.Render("  " + msg))

	case "/tool-remove", "/tools-remove":
		if arg == "" {
			m.push(stErr.Render("  usage: /tool-remove <name>"))
			break
		}
		if !m.cfg.RemoveCustomTool(arg) {
			m.push(stErr.Render("  no custom tool named " + strings.TrimSpace(arg)))
			break
		}
		if err := m.cfg.Save(); err != nil {
			m.push(stErr.Render("  removed but couldn't save config: " + err.Error()))
			break
		}
		m.push(stOK.Render("  removed custom tool " + strings.TrimSpace(arg)))
	case "/tool-add", "/tools-add":
		name, desc, command, toolArgs, ok := parseToolAddArgs(arg)
		if !ok {
			m.push(stErr.Render("  usage: /tool-add <name> <command> [--desc text] [--arg name:description ...]"))
			break
		}
		if err := m.cfg.AddCustomTool(CustomTool{Name: name, Description: desc, Command: command, Args: toolArgs, Provider: "manual"}); err != nil {
			m.push(stErr.Render("  couldn't add tool: " + err.Error()))
			break
		}
		if err := m.cfg.Save(); err != nil {
			m.push(stErr.Render("  added but couldn't save config: " + err.Error()))
			break
		}
		m.push(stOK.Render("  installed custom tool " + name))
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
			if m.goal {
				m.goal = false
				m.push(stHint.Render("  goal mode off — plan and goal modes can't run together"))
			}
			m.push(stOK.Render("  plan mode on — the agent will explore read-only and propose a plan"))
			m.push(stHint.Render("  run /plan again to approve the plan and execute it"))
		} else {
			m.push(stOK.Render("  plan approved — executing"))
			return m.submitText("Plan approved — go ahead and implement it.")
		}
	case "/goal":
		switch strings.ToLower(arg) {
		case "off", "stop", "false", "0":
			m.goal = false
			m.push(stHint.Render("  goal mode off"))
		case "", "on", "start", "true", "1":
			m.goal = !m.goal
			if m.goal {
				if m.plan {
					m.plan = false
					m.push(stHint.Render("  plan mode off — plan and goal modes can't run together"))
				}
				m.enableGoalMode()
			} else {
				m.push(stHint.Render("  goal mode off"))
			}
		default:
			m.goal = true
			if m.plan {
				m.plan = false
				m.push(stHint.Render("  plan mode off — plan and goal modes can't run together"))
			}
			m.enableGoalMode()
			return m.submitText(arg)
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

func (m *tuiModel) customToolsList() string {
	tools := normalizeCustomTools(m.cfg.Tools)
	if len(tools) == 0 {
		return stHint.Render("  no custom tools installed")
	}
	var b strings.Builder
	b.WriteString(stTitle.Render("  Custom tools") + "\n")
	for _, t := range tools {
		line := "  " + stAccent.Render(t.Name)
		if t.Description != "" {
			line += " " + stDim.Render("— "+t.Description)
		}
		b.WriteString(line + "\n")
		b.WriteString("    " + stDim.Render("command: "+t.Command) + "\n")
		if len(t.Args) > 0 {
			keys := make([]string, 0, len(t.Args))
			for k := range t.Args {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, k+": "+t.Args[k])
			}
			b.WriteString("    " + stDim.Render("args: "+strings.Join(parts, ", ")) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func parseToolAddArgs(arg string) (name, desc, command string, toolArgs map[string]string, ok bool) {
	fields := strings.Fields(arg)
	if len(fields) < 2 {
		return "", "", "", nil, false
	}
	name = fields[0]
	var cmdParts []string
	toolArgs = map[string]string{}
	for i := 1; i < len(fields); i++ {
		switch fields[i] {
		case "--desc", "--description":
			i++
			var parts []string
			for i < len(fields) && !strings.HasPrefix(fields[i], "--") {
				parts = append(parts, fields[i])
				i++
			}
			i--
			desc = strings.Join(parts, " ")
		case "--arg":
			i++
			if i >= len(fields) {
				return "", "", "", nil, false
			}
			k, v, found := strings.Cut(fields[i], ":")
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if !found {
				v = "string"
			}
			if k == "" {
				return "", "", "", nil, false
			}
			toolArgs[k] = v
		default:
			cmdParts = append(cmdParts, fields[i])
		}
	}
	command = strings.TrimSpace(strings.Join(cmdParts, " "))
	return name, desc, command, toolArgs, name != "" && command != ""
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

// modelMeta renders the pricing/context/tags suffix for a model, omitting
// pricing when it's unknown (some models we inject manually have none).
func modelMeta(md ModelInfo) string {
	var parts []string
	if md.InPrice > 0 || md.OutPrice > 0 {
		parts = append(parts, fmt.Sprintf("$%g/$%g", md.InPrice, md.OutPrice))
	}
	if md.MaxTokens > 0 {
		parts = append(parts, "context "+formatTokenLimit(md.MaxTokens))
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

func fallbackContextLimit(model string) int {
	if md, ok := knownModelInfo(model); ok && md.MaxTokens > 0 {
		return md.MaxTokens
	}
	return 256_000
}

func formatTokenLimit(n int) string {
	if n >= 1_000_000 && n%1_000_000 == 0 {
		return fmt.Sprintf("%dmil", n/1_000_000)
	}
	if n >= 1_000_000 {
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1_000_000)) + "mil"
	}
	if n >= 1000 && n%1000 == 0 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return commas(n)
}

func (m *tuiModel) contextUsageString() string {
	limit := m.contextLimit()
	if limit <= 0 {
		return fmt.Sprintf("context: ?%% %s/?", commas(m.ctxTokens))
	}
	pct := 0
	if m.ctxTokens > 0 {
		pct = int(math.Round(float64(m.ctxTokens) * 100 / float64(limit)))
	}
	return fmt.Sprintf("context: %d%% %s/%s", pct, abbrev(int64(m.ctxTokens)), formatTokenLimit(limit))
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
