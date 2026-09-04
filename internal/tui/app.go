package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/atotto/clipboard"
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
	// stateMsg reports that the workflow advanced. Without it the status bar
	// would keep showing the state the session started in.
	stateMsg workflow.State
	// gateOutputMsg carries one streamed output line from a gate.
	gateOutputMsg struct {
		id   string
		line string
	}
	// gateDoneMsg reports a gate receipt.
	gateDoneMsg struct{ receipt workflow.Receipt }
	// usageMsg reports the token accounting for one completed model turn.
	usageMsg struct {
		prompt     int
		completion int
	}
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

// ModelSwitcher changes the provider and model on a live session.
//
// Like ModeSwitcher, this cannot be done from the TUI alone: the provider
// client lives on the agent loop, so a label change would leave requests going
// to the old endpoint.
type ModelSwitcher interface {
	// ModelRefs lists the selectable provider/model references.
	ModelRefs() []string
	// DescribeModel renders a reference for display.
	DescribeModel(ref string) string
	// CurrentModel returns the active reference.
	CurrentModel() string
	// SetModel switches the session to a reference.
	SetModel(ref string) error
	// ContextWindow reports the active model's window in tokens, or 0 when
	// the config does not declare one. It is per-model rather than
	// per-session because switching model changes how full the same
	// conversation is.
	ContextWindow() int
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
	models   ModelSwitcher
	picker   *Picker
	history  *History
	events   chan tea.Msg

	viewport viewport.Model
	input    textarea.Model
	renderer *glamour.TermRenderer
	gates    *GatePane
	status   *StatusPane

	// transcript holds completed exchanges as rendered text.
	transcript []string
	// streaming accumulates the in-flight assistant reply.
	streaming strings.Builder
	// lastReply is the plain text of the most recent assistant reply.
	//
	// It is tracked separately because the transcript also holds UI chrome:
	// prompts, tool lines and the confirmations produced by commands. Copying
	// "the last message" off the end of the transcript would otherwise copy
	// the copy confirmation itself.
	lastReply string
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
	// showStatus controls the right-hand panel. It starts on: token spend and
	// context pressure are the things a session silently accumulates, so they
	// are shown until asked otherwise.
	showStatus bool
	// gateTop is the absolute row where the gate pane starts, recorded on
	// render so a mouse click can be mapped back to a gate row.
	gateTop int
	err     error
	cancel  context.CancelFunc

	// ticking records whether a frame ticker is in flight, so the rate is not
	// accidentally doubled by restarting an already-running one.
	ticking bool
	// follow keeps the viewport pinned to the newest output. It is cleared by
	// scrolling up, so reading back through the transcript is not yanked to
	// the bottom every time a token arrives.
	follow bool
	// writeClipboard is injected so tests can assert what would be copied
	// without depending on a clipboard being present.
	writeClipboard func(string) error
	// mouseCaptured reports whether the program is grabbing mouse events.
	// While it is, the terminal cannot do its own drag-selection, so this can
	// be released to copy text the usual way.
	mouseCaptured bool
	// glamourStyle is the style name resolved before the program started.
	// Rebuilding a renderer with WithAutoStyle would re-query the terminal
	// mid-session and leak the reply into the input box.
	glamourStyle string
}

// Options configures a Model.
type Options struct {
	Runner    Runner
	Switcher  ModeSwitcher
	Models    ModelSwitcher
	Gates     []workflow.Gate
	Governed  bool
	Auto      bool
	ModelName string
	State     workflow.State
	// Stages is the workflow spine shown in the status panel, in order. Empty
	// leaves the panel showing the bare state name.
	Stages []Stage
	// ContextWindow is the starting model's window in tokens, 0 when unknown.
	ContextWindow int
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
		showStatus:   true,
		status: &StatusPane{
			Stages:        opts.Stages,
			State:         opts.State,
			Governed:      opts.Governed,
			ContextWindow: opts.ContextWindow,
		},
		switcher:      opts.Switcher,
		models:        opts.Models,
		picker:        NewPicker(8),
		follow:        true,
		mouseCaptured: true,
		history:       NewHistory(0),
		runner:        opts.Runner,
		events:        make(chan tea.Msg, 256),
		viewport:      viewport.New(80, 20),
		input:         input,
		renderer:      renderer,
		gates:         NewGatePane(opts.Gates),
		Governed:      opts.Governed,
		Auto:          opts.Auto,
		ModelName:     opts.ModelName,
		State:         opts.State,
		showGates:     opts.Governed && len(opts.Gates) > 0,
	}
}

// SetSwitcher attaches the governance switcher, for the same reason SetRunner
// exists: the app is built after the model.
func (m *Model) SetSwitcher(s ModeSwitcher) { m.switcher = s }

// SetModels attaches the model switcher.
func (m *Model) SetModels(s ModelSwitcher) { m.models = s }

// openModelPicker shows the list of configured provider/model references.
func (m *Model) openModelPicker() {
	if m.models == nil {
		m.note(styleFail, "this session cannot switch model")
		return
	}
	refs := m.models.ModelRefs()
	if len(refs) == 0 {
		m.note(styleFail, "no models are configured; add one under \"providers\" in your config")
		return
	}
	if len(refs) == 1 {
		m.note(styleMuted, "only one model is configured: "+refs[0])
		return
	}

	choices := make([]Choice, 0, len(refs))
	for _, ref := range refs {
		choices = append(choices, Choice{Value: ref, Label: m.models.DescribeModel(ref)})
	}
	m.picker.Open("Switch model", choices, m.models.CurrentModel())
	m.resize(m.width, m.height)
}

// applyModel switches the session to a reference and reports the outcome.
func (m *Model) applyModel(ref string) {
	if m.models == nil {
		m.note(styleFail, "this session cannot switch model")
		return
	}
	if ref == m.models.CurrentModel() {
		m.note(styleMuted, "already using "+ref)
		return
	}
	if err := m.models.SetModel(ref); err != nil {
		m.note(styleFail, "could not switch model: "+err.Error())
		return
	}
	m.ModelName = ref
	// The window belongs to the model, so the same conversation is a
	// different fraction of it after a switch.
	m.status.ContextWindow = m.models.ContextWindow()
	m.note(styleOK, "model: "+ref)
}

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
	m.status.Governed = on
	m.status.State = m.State
	m.showGates = on && len(m.gates.gates) > 0
	if on {
		m.note(styleOK, fmt.Sprintf("governed: gates enforced, state %s", m.State))
	} else {
		m.note(styleMuted, "plain: no state machine, no gates, every tool available")
	}
	m.resize(m.width, m.height)
}

// recall replaces the input with a history entry, putting the cursor at the
// end so the recalled prompt can be edited or submitted immediately.
func (m *Model) recall(entry string) {
	m.input.SetValue(entry)
	m.input.CursorEnd()
}

// note appends a styled one-line message to the transcript.
//
// It jumps to the bottom: notes only ever report the result of something the
// user just did, and feedback you cannot see is no feedback at all.
func (m *Model) note(style lipgloss.Style, text string) {
	m.transcript = append(m.transcript, "  "+style.Render(text))
	m.follow = true
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

// StateReporter returns a function that pushes workflow state changes into
// the UI. The app calls it whenever the state machine advances.
func (m *Model) StateReporter() func(workflow.State) {
	return func(state workflow.State) {
		select {
		case m.events <- stateMsg(state):
		default:
			// Dropping a state frame is survivable: the next one corrects it.
		}
	}
}

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

	case tea.MouseMsg:
		return m.handleMouse(msg)

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

	case stateMsg:
		m.State = workflow.State(msg)
		m.status.State = m.State
		return m, m.waitForEvent()

	case usageMsg:
		m.status.Usage.Reported = true
		m.status.Usage.Turns++
		m.status.Usage.Sent += msg.prompt
		m.status.Usage.Received += msg.completion
		// Only the newest prompt measures context pressure: every turn
		// resends the conversation, so accumulating prompts would report a
		// window many times overflowed.
		m.status.Usage.Prompt = msg.prompt
		return m, m.waitForEvent()

	case gateOutputMsg:
		m.gates.Output(msg.id, msg.line)
		// Expanded output changes the pane's height, so the layout has to be
		// recomputed or the viewport would overlap it.
		m.resize(m.width, m.height)
		return m, m.waitForEvent()

	case gateDoneMsg:
		m.gates.Finish(msg.receipt)
		m.resize(m.width, m.height)
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
			m.lastReply = text
		}
		m.streaming.Reset()
		m.refresh()
		return m, m.waitForEvent()
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// toggleMouse releases or regrabs mouse capture, returning the command that
// tells the terminal.
func (m *Model) toggleMouse() tea.Cmd {
	m.mouseCaptured = !m.mouseCaptured
	if m.mouseCaptured {
		m.note(styleMuted, "mouse captured: wheel scrolls, click toggles gates")
		return tea.EnableMouseCellMotion
	}
	m.note(styleOK, "mouse released: drag to select and copy, ctrl+s to re-capture")
	return tea.DisableMouse
}

// toggleStatus shows or hides the side panel, reflowing the transcript into
// the width the panel gives up.
func (m *Model) toggleStatus() {
	m.showStatus = !m.showStatus
	// Turning it on in a narrow terminal changes nothing on screen, so say
	// why rather than appearing to swallow the key.
	if m.showStatus && m.width > 0 && m.width < minWidthForStatusPane {
		m.note(styleMuted, fmt.Sprintf("the panel needs at least %d columns; this terminal has %d",
			minWidthForStatusPane, m.width))
	}
	m.resize(m.width, m.height)
}

// handleMouse resolves wheel scrolling and clicks on the gate pane.
func (m *Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// The wheel scrolls the transcript wherever the pointer is: that is what
	// every other scrollable pane in a terminal does.
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.scrollUp(3)
		return m, nil
	case tea.MouseButtonWheelDown:
		m.scrollDown(3)
		return m, nil
	}

	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	if !m.showGates {
		return m, nil
	}
	// gateTop is the absolute row where the pane's border begins, recorded by
	// the last View: a click arrives against what is currently on screen.
	// One row of border, so content starts a row lower.
	row := msg.Y - m.gateTop - 1
	if index := m.gates.GateAtRow(row); index >= 0 {
		m.gates.Toggle(index)
		m.resize(m.width, m.height)
	}
	return m, nil
}

// handleKey processes key input.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// While a picker is open it owns navigation and confirmation, so it is
	// checked before the normal bindings; otherwise "enter" would submit a
	// prompt instead of selecting a row.
	if m.picker.IsOpen() {
		switch msg.String() {
		case "up", "ctrl+p", "k":
			m.picker.Move(-1)
			return m, nil
		case "down", "ctrl+n", "j":
			m.picker.Move(1)
			return m, nil
		case "pgup":
			m.picker.Move(-5)
			return m, nil
		case "pgdown":
			m.picker.Move(5)
			return m, nil
		case "enter", "tab":
			if choice, ok := m.picker.Selected(); ok {
				m.picker.Close()
				m.applyModel(choice.Value)
			} else {
				m.picker.Close()
			}
			m.resize(m.width, m.height)
			return m, nil
		case "esc", "ctrl+c", "q":
			m.picker.Close()
			m.resize(m.width, m.height)
			return m, nil
		}
		return m, nil
	}

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

	case "ctrl+t":
		// Hide or show the side panel. Reclaiming the column matters when
		// reading wide output such as a diff.
		m.toggleStatus()
		return m, nil

	case "pgup":
		m.viewport.ViewUp()
		m.follow = m.viewport.AtBottom()
		return m, nil

	case "pgdown":
		m.viewport.ViewDown()
		m.follow = m.viewport.AtBottom()
		return m, nil

	case "shift+up":
		m.scrollUp(1)
		return m, nil

	case "shift+down":
		m.scrollDown(1)
		return m, nil

	case "home":
		m.viewport.GotoTop()
		m.follow = false
		return m, nil

	case "end":
		m.viewport.GotoBottom()
		m.follow = true
		return m, nil

	case "ctrl+y":
		// Yank the newest reply, which is what one usually wants to keep.
		m.copyToClipboard(m.lastReplyText(), "the last reply")
		return m, nil

	case "ctrl+s":
		// Release or regrab the mouse. While the program captures it, the
		// terminal cannot drag-select, so this is how text gets copied by
		// hand.
		return m, m.toggleMouse()

	case "ctrl+o":
		// Collapse or expand every gate at once.
		if m.showGates {
			m.gates.ToggleAll()
			m.resize(m.width, m.height)
		}
		return m, nil

	case "tab":
		// Step the gate cursor, so toggling works without a mouse.
		if m.showGates && m.gates.Len() > 0 {
			m.gates.Select(1)
			return m, nil
		}

	case "shift+tab":
		if m.showGates && m.gates.Len() > 0 {
			m.gates.Select(-1)
			return m, nil
		}

	case " ":
		// Space toggles the selected gate, but only when one is selected;
		// otherwise it is just a space in the prompt.
		if m.showGates && m.gates.Selected() >= 0 {
			m.gates.Toggle(m.gates.Selected())
			m.resize(m.width, m.height)
			return m, nil
		}

	case "ctrl+g":
		// Toggle governance. The hint in the status bar names whichever
		// direction this will go.
		m.setGoverned(!m.Governed)
		return m, nil

	case "ctrl+p":
		if m.busy {
			m.note(styleWarn, "finish or cancel the current turn before switching model")
			return m, nil
		}
		m.openModelPicker()
		return m, nil

	case "up":
		// Only recall history from the first line, so up still moves the
		// cursor through a multi-line draft as it normally would.
		if m.input.Line() == 0 {
			if entry, ok := m.history.Prev(m.input.Value()); ok {
				m.recall(entry)
				return m, nil
			}
		}

	case "down":
		// Symmetrically, only step forward from the last line.
		if m.history.Browsing() && m.input.Line() == m.input.LineCount()-1 {
			if entry, ok := m.history.Next(); ok {
				m.recall(entry)
				return m, nil
			}
		}

	case "enter":
		if m.busy {
			return m, nil
		}
		prompt := strings.TrimSpace(m.input.Value())
		if prompt == "" {
			return m, nil
		}
		m.input.Reset()
		// Recorded before dispatch so a slash command is recallable too;
		// mistyped commands are exactly what one wants to arrow back to.
		m.history.Add(prompt)
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
			"/model    switch provider / model            (ctrl+p)",
			"/mode     toggle governed / plain            (ctrl+g)",
			"/govern   enforce the workflow: gates and stages",
			"/plain    drop governance, for quick questions",
			"/auto     toggle auto-approval of tool calls",
			"/gates    toggle the gate pane               (ctrl+w)",
			"/stats    toggle the stage / token panel     (ctrl+t)",
			"          tab selects a gate, space toggles it",
			"          ctrl+o collapses or expands them all",
			"/copy     copy the last reply                (ctrl+y)",
			"/copy all copy the whole transcript",
			"/mouse    release the mouse to select text   (ctrl+s)",
			"/clear    clear the transcript",
			"",
			styleAccent.Render("Scrolling"),
			"pgup/pgdn page · shift+↑/↓ line · home/end ends · wheel",
			"/quit     exit",
		}, "\n")))
	case "/plain":
		m.setGoverned(false)
	case "/govern", "/gated":
		m.setGoverned(true)
	case "/mode":
		m.setGoverned(!m.Governed)
	case "/model", "/provider":
		// With an argument, switch directly; without one, offer the list.
		if fields := strings.Fields(input); len(fields) > 1 {
			m.applyModel(fields[1])
		} else {
			m.openModelPicker()
		}
	case "/auto":
		m.Auto = !m.Auto
		m.transcript = append(m.transcript,
			styleMuted.Render(fmt.Sprintf("  auto-approval %s", onOff(m.Auto))))
	case "/gates":
		m.showGates = !m.showGates
	case "/stats", "/status":
		m.toggleStatus()
	case "/copy":
		// `/copy all` takes the whole conversation; bare `/copy` the last
		// message, which is the common case.
		if fields := strings.Fields(input); len(fields) > 1 && fields[1] == "all" {
			m.copyToClipboard(m.transcriptText(), "the transcript")
		} else {
			m.copyToClipboard(m.lastReplyText(), "the last reply")
		}
	case "/mouse":
		return true, m.toggleMouse()
	case "/clear":
		m.transcript = nil
		m.lastReply = ""
		m.follow = true
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
	pickerHeight := lipglossHeight(m.picker.View(width))
	// Reserve rows for the input, the status bar, the gate pane and the picker.
	chrome := m.input.Height() + 2 + 1 + gateHeight + pickerHeight
	viewportHeight := height - chrome
	if viewportHeight < 3 {
		viewportHeight = 3
	}
	m.viewport.Width = width - m.statusWidth()
	m.viewport.Height = viewportHeight
	m.input.SetWidth(width - 2)

	if m.renderer != nil {
		wrap := m.viewport.Width - 4
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

// refresh rebuilds the viewport content, following the newest output only if
// the reader has not scrolled away from it.
func (m *Model) refresh() {
	parts := append([]string(nil), m.transcript...)
	if streaming := m.streaming.String(); streaming != "" {
		parts = append(parts, m.render(streaming))
	}
	m.viewport.SetContent(strings.Join(parts, "\n"))
	if m.follow {
		m.viewport.GotoBottom()
	}
}

// scrollUp moves the viewport back and stops following.
func (m *Model) scrollUp(lines int) {
	m.viewport.LineUp(lines)
	m.follow = m.viewport.AtBottom()
}

// scrollDown moves the viewport forward, resuming following at the bottom.
func (m *Model) scrollDown(lines int) {
	m.viewport.LineDown(lines)
	m.follow = m.viewport.AtBottom()
}

// transcriptText returns the conversation as plain text, with styling removed
// so it pastes cleanly.
func (m *Model) transcriptText() string {
	parts := append([]string(nil), m.transcript...)
	if streaming := m.streaming.String(); streaming != "" {
		parts = append(parts, streaming)
	}
	return plainText(strings.Join(parts, "\n"))
}

// lastReplyText returns the most recent assistant reply as plain text,
// preferring one still arriving.
func (m *Model) lastReplyText() string {
	if streaming := strings.TrimSpace(m.streaming.String()); streaming != "" {
		return streaming
	}
	return m.lastReply
}

// copyToClipboard puts text on the system clipboard and reports the outcome.
func (m *Model) copyToClipboard(text, what string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		m.note(styleMuted, "nothing to copy")
		return
	}
	write := m.writeClipboard
	if write == nil {
		write = clipboard.WriteAll
	}
	if err := write(trimmed); err != nil {
		// Headless or no clipboard tool: say so rather than silently failing.
		m.note(styleFail, "could not copy: "+err.Error())
		return
	}
	m.note(styleOK, fmt.Sprintf("copied %s (%d lines)", what,
		strings.Count(trimmed, "\n")+1))
}

// statusWidth is the columns the side panel occupies, zero when it is not
// shown. resize needs this before View runs, so the decision lives here
// rather than being inferred from a rendered pane.
func (m *Model) statusWidth() int {
	if !m.showStatus || m.width < minWidthForStatusPane {
		return 0
	}
	return statusPaneWidth
}

// statusView renders the side panel, capped to the transcript's height so a
// tall panel cannot push the input box off the screen.
func (m *Model) statusView() string {
	if m.statusWidth() == 0 {
		return ""
	}
	return m.status.View(m.width, m.viewport.Height)
}

// View renders the interface.
func (m *Model) View() string {
	var b strings.Builder

	// The transcript and the status panel share a row; everything below is
	// full width.
	main := m.viewport.View()
	if pane := m.statusView(); pane != "" {
		main = lipgloss.JoinHorizontal(lipgloss.Top, main, pane)
	}
	b.WriteString(main)
	b.WriteString("\n")

	if pane := m.picker.View(m.width); pane != "" {
		b.WriteString(pane)
		b.WriteString("\n")
	}

	if m.showGates {
		if pane := m.gates.View(m.tick); pane != "" {
			// gateTop is the pane's border row, so it is the count of rows
			// already written: a block of n rows occupies 0..n-1, putting
			// the next block at n. The transcript row is measured after the
			// join, since the side panel can make it taller than the
			// viewport.
			m.gateTop = lipglossHeight(main)
			if picker := m.picker.View(m.width); picker != "" {
				m.gateTop += lipglossHeight(picker)
			}
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

	// Scrolled away from the newest output, or holding the mouse back: both
	// change how the interface behaves, so both are shown.
	if !m.follow {
		left = append(left, styleWarn.Render(fmt.Sprintf("scrolled %d%%",
			int(m.viewport.ScrollPercent()*100))))
	}
	if !m.mouseCaptured {
		left = append(left, styleOK.Render("select"))
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
	if m.models != nil && len(m.models.ModelRefs()) > 1 {
		out = append(out, "ctrl+p model")
	}
	if m.Governed {
		// The pane hint stays whichever way it is showing, since ctrl+w is
		// how you hide it again. The expand/collapse hint is only relevant
		// once there is something on screen to collapse; clicking itself is
		// advertised by the ▾/▸ markers on each row.
		out = append(out, "ctrl+w gates")
		if m.showGates && m.gates.Len() > 0 {
			out = append(out, "ctrl+o fold")
		}
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

func (o *observer) TurnFinished(usage *provider.Usage) {
	if usage == nil {
		// Not every endpoint reports usage on a streamed response. Leaving
		// the counters untouched keeps the panel saying "not reported"
		// instead of claiming the turn was free.
		return
	}
	o.send(usageMsg{prompt: usage.PromptTokens, completion: usage.CompletionTokens})
}

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

func (g *gateProgress) GateOutput(gateID, line string) {
	g.send(gateOutputMsg{id: gateID, line: line})
}

func (g *gateProgress) GateFinished(receipt workflow.Receipt) {
	g.send(gateDoneMsg{receipt: receipt})
}

func (g *gateProgress) send(msg tea.Msg) {
	select {
	case g.events <- msg:
	default:
	}
}
