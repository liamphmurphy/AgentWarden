package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/lmurphy/agentwarden/internal/agent"
	"github.com/lmurphy/agentwarden/internal/enforce"
	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/tool"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

// tickMsg drives animation at FrameRate.
type tickMsg time.Time

// tick schedules the next animation frame.
func tick() tea.Cmd {
	return tea.Tick(time.Second/FrameRate, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Messages exchanged between the loop goroutine and the UI.
type (
	// deltaMsg is incremental assistant text.
	deltaMsg string
	// toolMsg reports a tool starting or finishing.
	toolMsg struct {
		name     string
		finished bool
		isError  bool
	}
	// blockedMsg reports an enforcer refusal.
	blockedMsg struct{ reason string }
	// gateStartMsg reports a gate beginning.
	gateStartMsg struct{ id string }
	// gateDoneMsg reports a gate receipt.
	gateDoneMsg struct{ receipt workflow.Receipt }
	// doneMsg reports the loop finishing a run.
	doneMsg struct {
		result agent.Result
		err    error
	}
)

// Runner executes a prompt. The TUI is decoupled from the loop so it can be
// exercised without a provider.
type Runner interface {
	Run(ctx context.Context, prompt string) (agent.Result, error)
}

// ModeSwitcher swaps governance on and off for a live session.
//
// The TUI cannot do this itself: enforcement lives in the agent loop's
// governor, so a display flag alone would claim the mode had changed while the
// enforcer stayed active.
type ModeSwitcher interface {
	// SetGoverned activates or deactivates the enforcer, returning an error if
	// the requested mode is unavailable.
	SetGoverned(on bool) error
	// GovernanceAvailable reports whether governance can be switched on at
	// all, which needs a policy that loads.
	GovernanceAvailable() bool
	// WorkflowState reports the current state, or "" when ungoverned.
	WorkflowState() workflow.State
}

// Model is the Bubble Tea model.
type Model struct {
	runner   Runner
	switcher ModeSwitcher
	events   chan tea.Msg

	viewport viewport.Model
	input    textarea.Model
	renderer *glamour.TermRenderer
	gates    *GatePane

	// transcript holds completed exchanges as rendered text.
	transcript []string
	// streaming accumulates the in-flight assistant reply.
	streaming strings.Builder
	// activeTool is the tool currently executing, shown with a spinner.
	activeTool string

	// tick counts animation frames, driving all spinners.
	tick int
	busy bool

	width  int
	height int

	// Governed reports whether the enforcer is active, shown in the status bar.
	Governed bool
	// Auto reports whether tool calls are auto-approved.
	Auto bool
	// ModelName labels the status bar.
	ModelName string
	// State is the current workflow state, or "" when ungoverned.
	State workflow.State

	showGates bool
	err       error
	cancel    context.CancelFunc

	// ticking records whether a frame ticker is in flight, so the rate is not
	// accidentally doubled by restarting an already-running one.
	ticking bool
	// glamourStyle is the style name resolved before the program started.
	// Rebuilding a renderer with WithAutoStyle would re-query the terminal
	// mid-session and leak the reply into the input box.
	glamourStyle string
}

// Options configures a Model.
type Options struct {
	Runner    Runner
	Switcher  ModeSwitcher
	Gates     []workflow.Gate
	Governed  bool
	Auto      bool
	ModelName string
	State     workflow.State
	// GlamourStyle is the style name resolved by DetectTheme before the
	// program starts. Empty falls back to a fixed dark style; it must never
	// be auto-detected here, because resize rebuilds the renderer while
	// Bubble Tea owns stdin.
	GlamourStyle string
}

// New returns a Model ready for tea.NewProgram.
func New(opts Options) *Model {
	input := textarea.New()
	input.Placeholder = "Ask anything, or /help for commands"
	input.Focus()
	input.SetHeight(3)
	input.ShowLineNumbers = false
	input.CharLimit = 0

	style := opts.GlamourStyle
	if style == "" {
		// Never auto-detect here: New may run before the program starts, but
		// resize does not, and both must use the same query-free style. An
		// unspecified style means "do not style" rather than a guess.
		style = styles.NoTTYStyle
	}
	renderer, err := newRenderer(style, 80)
	if err != nil {
		// Rendering is a nicety; plain text is an acceptable fallback.
		renderer = nil
	}

	return &Model{
		glamourStyle: style,
		switcher:     opts.Switcher,
		runner:       opts.Runner,
		events:       make(chan tea.Msg, 256),
		viewport:     viewport.New(80, 20),
		input:        input,
		renderer:     renderer,
		gates:        NewGatePane(opts.Gates),
		Governed:     opts.Governed,
		Auto:         opts.Auto,
		ModelName:    opts.ModelName,
		State:        opts.State,
		showGates:    opts.Governed && len(opts.Gates) > 0,
	}
}

// SetSwitcher attaches the governance switcher, for the same reason SetRunner
// exists: the app is built after the model.
func (m *Model) SetSwitcher(s ModeSwitcher) { m.switcher = s }

// canSwitch reports whether governance can be toggled in this session.
func (m *Model) canSwitch() bool {
	return m.switcher != nil && m.switcher.GovernanceAvailable()
}

// setGoverned performs a real mode switch and reports the outcome in the
// transcript, so the message can never claim more than actually happened.
func (m *Model) setGoverned(on bool) {
	if m.busy {
		m.note(styleWarn, "finish or cancel the current turn before switching mode")
		return
	}
	if on && !m.canSwitch() {
		m.note(styleFail, "governance is unavailable: no workflow policy loaded for this project")
		return
	}
	if m.switcher == nil {
		m.note(styleFail, "this session cannot switch mode")
		return
	}
	if on == m.Governed {
		m.note(styleMuted, fmt.Sprintf("already in %s mode", modeName(on)))
		return
	}
	if err := m.switcher.SetGoverned(on); err != nil {
		m.note(styleFail, "could not switch mode: "+err.Error())
		return
	}

	m.Governed = on
	m.State = m.switcher.WorkflowState()
	m.showGates = on && len(m.gates.gates) > 0
	if on {
		m.note(styleOK, fmt.Sprintf("governed: gates enforced, state %s", m.State))
	} else {
		m.note(styleMuted, "plain: no state machine, no gates, every tool available")
	}
	m.resize(m.width, m.height)
}

// note appends a styled one-line message to the transcript.
func (m *Model) note(style lipgloss.Style, text string) {
	m.transcript = append(m.transcript, "  "+style.Render(text))
	m.refresh()
}

// modeName is the user-facing name for a mode.
func modeName(governed bool) string {
	if governed {
		return "governed"
	}
	return "plain"
}

// SetRunner attaches the executor. It is separate from New so the model can be
// built first and handed to the gate runner as a progress sink.
func (m *Model) SetRunner(r Runner) { m.runner = r }

// newRenderer builds a markdown renderer for a fixed style. WithStandardStyle
// looks the style up in a table, so unlike WithAutoStyle it never touches the
// terminal.
func newRenderer(style string, wrap int) (*glamour.TermRenderer, error) {
	return glamour.NewTermRenderer(
		glamour.WithStandardStyle(style),
		glamour.WithWordWrap(wrap),
	)
}

// animating reports whether anything on screen is in motion, which is what
// decides if the frame ticker needs to keep running.
func (m *Model) animating() bool {
	return m.busy || m.gates.Running()
}

// resumeAnimation restarts the frame ticker if it has gone idle. It is safe to
// call when already ticking only in that it may briefly double the rate, so
// callers guard with animating().
func (m *Model) resumeAnimation() tea.Cmd {
	if m.ticking || !m.animating() {
		return nil
	}
	m.ticking = true
	return tick()
}

// Init starts the animation ticker and the event pump.
func (m *Model) Init() tea.Cmd {
	// One tick primes the loop; the handler decides whether to continue.
	m.ticking = true
	return tea.Batch(tick(), m.waitForEvent(), textarea.Blink)
}

// waitForEvent bridges the loop's channel into the Bubble Tea message stream.
func (m *Model) waitForEvent() tea.Cmd {
	return func() tea.Msg { return <-m.events }
}

// Observer returns an agent.Observer that feeds this model.
func (m *Model) Observer() agent.Observer { return &observer{events: m.events} }

// GateProgress returns an enforce.GateProgress that feeds this model.
func (m *Model) GateProgress() enforce.GateProgress { return &gateProgress{events: m.events} }

// Update handles messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tickMsg:
		m.tick++
		// Stop the ticker when nothing is moving: at 30fps an always-on
		// ticker repaints the whole frame 30 times a second while the user is
		// just reading, which is pure CPU burn. Anything that starts
		// animating restarts it.
		if !m.animating() {
			m.ticking = false
			return m, nil
		}
		return m, tick()

	case deltaMsg:
		m.streaming.WriteString(string(msg))
		m.refresh()
		return m, m.waitForEvent()

	case toolMsg:
		if msg.finished {
			m.activeTool = ""
			status := styleOK.Render(glyphPass)
			if msg.isError {
				status = styleFail.Render(glyphFail)
			}
			m.transcript = append(m.transcript,
				fmt.Sprintf("  %s %s", status, styleMuted.Render(msg.name)))
		} else {
			m.activeTool = msg.name
		}
		m.refresh()
		return m, m.waitForEvent()

	case blockedMsg:
		m.transcript = append(m.transcript,
			styleWarn.Render("  ⚠ blocked: ")+styleMuted.Render(msg.reason))
		m.refresh()
		return m, m.waitForEvent()

	case gateStartMsg:
		m.showGates = true
		m.gates.Start(msg.id)
		// A gate may start after the ticker has gone idle.
		return m, tea.Batch(m.waitForEvent(), m.resumeAnimation())

	case gateDoneMsg:
		m.gates.Finish(msg.receipt)
		return m, m.waitForEvent()

	case doneMsg:
		m.busy = false
		m.activeTool = ""
		m.cancel = nil
		if msg.err != nil {
			m.err = msg.err
		}
		if text := strings.TrimSpace(m.streaming.String()); text != "" {
			m.transcript = append(m.transcript, m.render(text))
		}
		m.streaming.Reset()
		m.refresh()
		return m, m.waitForEvent()
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// handleKey processes key input.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		// While busy, cancel the run rather than quitting, so a long gate
		// suite can be interrupted without losing the session.
		if m.busy && m.cancel != nil {
			m.cancel()
			return m, nil
		}
		return m, tea.Quit

	case "ctrl+d":
		return m, tea.Quit

	case "ctrl+w":
		m.showGates = !m.showGates
		m.resize(m.width, m.height)
		return m, nil

	case "ctrl+g":
		// Toggle governance. The hint in the status bar names whichever
		// direction this will go.
		m.setGoverned(!m.Governed)
		return m, nil

	case "enter":
		if m.busy {
			return m, nil
		}
		prompt := strings.TrimSpace(m.input.Value())
		if prompt == "" {
			return m, nil
		}
		m.input.Reset()
		if handled, cmd := m.command(prompt); handled {
			return m, cmd
		}
		return m, m.submit(prompt)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// command handles slash commands, returning whether it consumed the input.
func (m *Model) command(input string) (bool, tea.Cmd) {
	if !strings.HasPrefix(input, "/") {
		return false, nil
	}
	switch strings.Fields(input)[0] {
	case "/help":
		m.transcript = append(m.transcript, stylePane.Render(strings.Join([]string{
			styleAccent.Render("Commands"),
			"/mode     toggle governed / plain            (ctrl+g)",
			"/govern   enforce the workflow: gates and stages",
			"/plain    drop governance, for quick questions",
			"/auto     toggle auto-approval of tool calls",
			"/gates    toggle the gate pane (ctrl+w)",
			"/clear    clear the transcript",
			"/quit     exit",
		}, "\n")))
	case "/plain":
		m.setGoverned(false)
	case "/govern", "/gated":
		m.setGoverned(true)
	case "/mode":
		m.setGoverned(!m.Governed)
	case "/auto":
		m.Auto = !m.Auto
		m.transcript = append(m.transcript,
			styleMuted.Render(fmt.Sprintf("  auto-approval %s", onOff(m.Auto))))
	case "/gates":
		m.showGates = !m.showGates
	case "/clear":
		m.transcript = nil
	case "/quit", "/exit":
		return true, tea.Quit
	default:
		m.transcript = append(m.transcript,
			styleFail.Render("  unknown command; try /help"))
	}
	m.refresh()
	return true, nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// submit starts a run in the background.
func (m *Model) submit(prompt string) tea.Cmd {
	m.transcript = append(m.transcript, styleUser.Render("› "+prompt))
	m.busy = true
	m.err = nil
	m.gates.Reset()
	m.refresh()

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	run := func() tea.Msg {
		result, err := m.runner.Run(ctx, prompt)
		cancel()
		return doneMsg{result: result, err: err}
	}
	return tea.Batch(run, m.resumeAnimation())
}

// resize recomputes layout.
func (m *Model) resize(width, height int) {
	if width <= 0 || height <= 0 {
		return
	}
	m.width, m.height = width, height

	gateHeight := 0
	if m.showGates {
		gateHeight = lipglossHeight(m.gates.View(m.tick))
	}
	// Reserve rows for the input, the status bar and the gate pane.
	chrome := m.input.Height() + 2 + 1 + gateHeight
	viewportHeight := height - chrome
	if viewportHeight < 3 {
		viewportHeight = 3
	}
	m.viewport.Width = width
	m.viewport.Height = viewportHeight
	m.input.SetWidth(width - 2)

	if m.renderer != nil {
		wrap := width - 4
		if wrap < 20 {
			wrap = 20
		}
		if renderer, err := newRenderer(m.glamourStyle, wrap); err == nil {
			m.renderer = renderer
		}
	}
	m.refresh()
}

// render turns markdown into styled text, falling back to plain text.
func (m *Model) render(text string) string {
	if m.renderer == nil {
		return text
	}
	out, err := m.renderer.Render(text)
	if err != nil {
		return text
	}
	return strings.TrimRight(out, "\n")
}

// refresh rebuilds the viewport content and keeps it pinned to the bottom.
func (m *Model) refresh() {
	parts := append([]string(nil), m.transcript...)
	if streaming := m.streaming.String(); streaming != "" {
		parts = append(parts, m.render(streaming))
	}
	m.viewport.SetContent(strings.Join(parts, "\n"))
	m.viewport.GotoBottom()
}

// View renders the interface.
func (m *Model) View() string {
	var b strings.Builder
	b.WriteString(m.viewport.View())
	b.WriteString("\n")

	if m.showGates {
		if pane := m.gates.View(m.tick); pane != "" {
			b.WriteString(pane)
			b.WriteString("\n")
		}
	}

	b.WriteString(m.input.View())
	b.WriteString("\n")
	b.WriteString(m.statusBar())
	return b.String()
}

// statusBar renders the bottom line: what model is in use, which mode is
// active, and the key that switches it.
func (m *Model) statusBar() string {
	left := []string{m.ModelName, m.modeIndicator()}
	if m.Auto {
		left = append(left, styleWarn.Render("auto"))
	}

	switch {
	case m.err != nil:
		left = append(left, styleFail.Render("error: "+truncate(m.err.Error(), 48)))
	case m.busy && m.activeTool != "":
		left = append(left, styleAccent.Render(spinnerFrame(m.tick)+" "+m.activeTool))
	case m.busy:
		left = append(left, styleAccent.Render(spinnerFrame(m.tick)+" thinking"))
	}

	line := strings.Join(left, styleMuted.Render(" · "))
	hint := styleMuted.Render(strings.Join(m.hints(), " · "))

	gap := m.width - lipglossWidth(line) - lipglossWidth(hint) - 2
	if gap < 1 {
		// Too narrow for both: the mode matters more than the hints.
		return styleStatusBar.Width(m.width).Render(line)
	}
	return styleStatusBar.Width(m.width).Render(line + strings.Repeat(" ", gap) + hint)
}

// modeIndicator renders the active mode. Governed shows the workflow state,
// since that is what decides which tools the model can see.
func (m *Model) modeIndicator() string {
	if m.Governed {
		state := string(m.State)
		if state == "" {
			state = "governed"
		}
		return styleAccent.Render("⛨ governed:" + state)
	}
	if !m.canSwitch() {
		// Say why it cannot be turned on, rather than offering a key that
		// would only report a failure.
		return styleMuted.Render("○ plain (no policy)")
	}
	return styleMuted.Render("○ plain")
}

// hints lists the key bindings shown on the right. The mode hint names the
// action the key performs, not the current state, so it reads as an offer.
func (m *Model) hints() []string {
	var out []string
	if m.canSwitch() {
		if m.Governed {
			out = append(out, "ctrl+g plain")
		} else {
			out = append(out, "ctrl+g govern")
		}
	}
	if m.Governed {
		out = append(out, "ctrl+w gates")
	}
	return append(out, "ctrl+d quit")
}

// observer feeds loop progress into the model's event channel.
type observer struct{ events chan tea.Msg }

func (o *observer) TextDelta(text string) { o.send(deltaMsg(text)) }

func (o *observer) ToolStarted(call tool.Call) {
	o.send(toolMsg{name: call.Name})
}

func (o *observer) ToolFinished(call tool.Call, result tool.Result) {
	o.send(toolMsg{name: call.Name, finished: true, isError: result.IsError})
}

func (o *observer) Blocked(_ tool.Call, decision enforce.Decision) {
	o.send(blockedMsg{reason: decision.Reason})
}

func (o *observer) TurnFinished(*provider.Usage) {}

// send never blocks the loop: a full channel drops a progress frame rather
// than stalling the model.
func (o *observer) send(msg tea.Msg) {
	select {
	case o.events <- msg:
	default:
	}
}

// gateProgress feeds gate lifecycle into the model's event channel.
type gateProgress struct{ events chan tea.Msg }

func (g *gateProgress) GateStarted(gateID string, _ []string) {
	g.send(gateStartMsg{id: gateID})
}

func (g *gateProgress) GateOutput(string, string) {}

func (g *gateProgress) GateFinished(receipt workflow.Receipt) {
	g.send(gateDoneMsg{receipt: receipt})
}

func (g *gateProgress) send(msg tea.Msg) {
	select {
	case g.events <- msg:
	default:
	}
}
