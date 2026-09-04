package tui

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lmurphy/agentwarden/internal/agent"
	"github.com/lmurphy/agentwarden/internal/enforce"
	"github.com/lmurphy/agentwarden/internal/tool"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

// stubRunner returns a canned result, optionally blocking until released.
type stubRunner struct {
	result  agent.Result
	err     error
	release chan struct{}
	started chan struct{}
}

func (r *stubRunner) Run(ctx context.Context, _ string) (agent.Result, error) {
	if r.started != nil {
		close(r.started)
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		}
	}
	return r.result, r.err
}

func testGates() []workflow.Gate {
	required := true
	return []workflow.Gate{
		{ID: "unit", Command: []string{"go", "test", "./..."}, Required: &required},
		{ID: "integration", Command: []string{"make", "itest"}, Required: &required},
	}
}

func newModel(t *testing.T, runner Runner) *Model {
	t.Helper()
	m := New(Options{
		Runner:    runner,
		Gates:     testGates(),
		Governed:  true,
		ModelName: "ollama/qwen3.5",
		State:     workflow.StatePlanning,
	})
	m.resize(100, 30)
	return m
}

// TestTickAdvancesAnimationWhileBusy checks the 30fps ticker drives the frame
// counter and reschedules itself while something is moving.
func TestTickAdvancesAnimationWhileBusy(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.busy = true

	before := m.tick
	updated, cmd := m.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("a tick should schedule the next frame while busy")
	}
	if updated.(*Model).tick != before+1 {
		t.Errorf("tick = %d, want %d", updated.(*Model).tick, before+1)
	}
}

// TestTickerStopsWhenIdle: at 30fps an always-on ticker repaints the whole
// frame 30 times a second while the user is just reading.
func TestTickerStopsWhenIdle(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.ticking = true

	_, cmd := m.Update(tickMsg(time.Now()))
	if cmd != nil {
		t.Error("an idle model should stop ticking")
	}
	if m.ticking {
		t.Error("the model should record that the ticker stopped")
	}
}

// TestRunningGateKeepsTicking: gate progress is animated, so the ticker must
// survive an idle model as long as a gate is in flight.
func TestRunningGateKeepsTicking(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.gates.Start("unit")

	if !m.animating() {
		t.Fatal("a running gate counts as animating")
	}
	if _, cmd := m.Update(tickMsg(time.Now())); cmd == nil {
		t.Error("a running gate should keep the ticker alive")
	}
}

// TestGateStartResumesIdleTicker covers a gate beginning after the ticker has
// already stopped.
func TestGateStartResumesIdleTicker(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.ticking = false

	if _, cmd := m.Update(gateStartMsg{id: "unit"}); cmd == nil {
		t.Fatal("a gate start should produce commands")
	}
	if !m.ticking {
		t.Error("a gate start should restart the ticker")
	}
}

// TestResumeAnimationDoesNotDoubleUp guards against two tickers running at
// once, which would double the frame rate.
func TestResumeAnimationDoesNotDoubleUp(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.busy = true
	m.ticking = true

	if cmd := m.resumeAnimation(); cmd != nil {
		t.Error("an already-running ticker should not be restarted")
	}
}

func TestFrameRateIsThirty(t *testing.T) {
	if FrameRate != 30 {
		t.Errorf("FrameRate = %d, want 30", FrameRate)
	}
	// The scheduled interval must match the declared rate.
	if got := time.Second / FrameRate; got != time.Second/30 {
		t.Errorf("frame interval = %s", got)
	}
}

// TestSpinnerAnimates checks successive frames differ, and that the sequence
// cycles rather than running off the end.
func TestSpinnerAnimates(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < len(spinnerFrames)*3; i++ {
		seen[spinnerFrame(i)] = true
	}
	if len(seen) != len(spinnerFrames) {
		t.Errorf("saw %d distinct frames, want %d", len(seen), len(spinnerFrames))
	}
	// It must be stable across a full cycle.
	period := len(spinnerFrames) * 3
	if spinnerFrame(0) != spinnerFrame(period) {
		t.Error("the spinner should cycle")
	}
	if spinnerFrame(0) == spinnerFrame(3) {
		t.Error("the frame should advance within a cycle")
	}
}

func TestGatePaneRendersEveryStatus(t *testing.T) {
	pane := NewGatePane(testGates())
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pane.now = func() time.Time { return fixed }

	// All pending initially.
	view := pane.View(0)
	if !strings.Contains(view, "pending") {
		t.Errorf("want pending gates:\n%s", view)
	}
	if pane.Running() {
		t.Error("nothing should be running yet")
	}

	// One running: the spinner and elapsed time appear.
	pane.Start("unit")
	if !pane.Running() {
		t.Error("unit should be running")
	}
	pane.now = func() time.Time { return fixed.Add(2500 * time.Millisecond) }
	view = pane.View(0)
	if !strings.Contains(view, "2.5s") {
		t.Errorf("want elapsed time:\n%s", view)
	}

	// One passed.
	pane.Finish(workflow.Receipt{GateID: "unit", Success: true, DurationMS: 2400})
	view = pane.View(0)
	if !strings.Contains(view, glyphPass) {
		t.Errorf("want a pass glyph:\n%s", view)
	}
	if pane.Running() {
		t.Error("unit has finished")
	}

	// One failed, with its reason surfaced so the user can see why.
	pane.Start("integration")
	pane.Finish(workflow.Receipt{
		GateID: "integration", Success: false, FailureReason: "command_failed",
	})
	view = pane.View(0)
	if !strings.Contains(view, glyphFail) || !strings.Contains(view, "command_failed") {
		t.Errorf("want a failure and its reason:\n%s", view)
	}
}

func TestGatePaneShowsRunningOutput(t *testing.T) {
	pane := NewGatePane(testGates())
	pane.Start("unit")
	pane.Output("unit", "  ok   pkg/api  0.4s  ")

	view := pane.View(0)
	if !strings.Contains(view, "ok   pkg/api") {
		t.Errorf("want the latest output line:\n%s", view)
	}
}

func TestGatePaneReset(t *testing.T) {
	pane := NewGatePane(testGates())
	pane.Start("unit")
	pane.Finish(workflow.Receipt{GateID: "unit", Success: true})
	pane.Reset()

	view := pane.View(0)
	if strings.Contains(view, glyphPass) {
		t.Errorf("reset should clear results:\n%s", view)
	}
	if pane.Running() {
		t.Error("reset should clear running state")
	}
}

func TestGatePaneEmpty(t *testing.T) {
	if got := NewGatePane(nil).View(0); got != "" {
		t.Errorf("an empty pane should render nothing, got %q", got)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "0.5s"},
		{2400 * time.Millisecond, "2.4s"},
		{59 * time.Second, "59.0s"},
		{60 * time.Second, "1:00"},
		{47*time.Second + time.Minute, "1:47"},
		{10 * time.Minute, "10:00"},
	}
	for _, tc := range tests {
		if got := formatDuration(tc.d); got != tc.want {
			t.Errorf("formatDuration(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly-10", 10, "exactly-10"},
		{"much too long here", 8, "much to…"},
		{"unicode ünïcödé", 10, "unicode ü…"},
		{"x", 1, "x"},
	}
	for _, tc := range tests {
		if got := truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

// TestStatusBarReflectsMode is what tells the user whether they are governed.
func TestStatusBarReflectsMode(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(m *Model)
		wantWord string
	}{
		{"governed shows state", func(m *Model) {}, "planning"},
		{"plain is labelled", func(m *Model) { m.Governed = false }, "plain"},
		{"auto is labelled", func(m *Model) { m.Auto = true }, "auto"},
		{"model name shown", func(m *Model) {}, "qwen3.5"},
		{"error surfaced", func(m *Model) { m.err = errors.New("boom") }, "boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(t, &stubRunner{})
			tc.mutate(m)
			if got := m.statusBar(); !strings.Contains(got, tc.wantWord) {
				t.Errorf("status bar should mention %q:\n%s", tc.wantWord, got)
			}
		})
	}
}

func TestBusyStatusShowsSpinner(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.busy = true
	if got := m.statusBar(); !strings.Contains(got, "thinking") {
		t.Errorf("want a busy indicator:\n%s", got)
	}
	m.activeTool = "bash"
	if got := m.statusBar(); !strings.Contains(got, "bash") {
		t.Errorf("want the active tool named:\n%s", got)
	}
}

// TestPlainCommandNeedsASwitcher: without something able to disengage the
// enforcer, /plain must refuse rather than relabel the status bar. That
// mismatch was the original bug.
func TestPlainCommandNeedsASwitcher(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.switcher = nil
	if !m.Governed {
		t.Fatal("precondition: should start governed")
	}

	handled, _ := m.command("/plain")
	if !handled {
		t.Fatal("/plain should be handled as a command")
	}
	if !m.Governed {
		t.Error("/plain must not claim to drop governance it cannot drop")
	}
	if !strings.Contains(strings.Join(m.transcript, "\n"), "cannot switch mode") {
		t.Errorf("the refusal should be explained: %v", m.transcript)
	}
}

func TestAutoCommandToggles(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.command("/auto")
	if !m.Auto {
		t.Error("/auto should enable auto-approval")
	}
	m.command("/auto")
	if m.Auto {
		t.Error("/auto should toggle back off")
	}
}

func TestCommandsHandled(t *testing.T) {
	tests := []struct {
		input       string
		wantHandled bool
	}{
		{"/help", true},
		{"/plain", true},
		{"/auto", true},
		{"/gates", true},
		{"/clear", true},
		{"/nonsense", true},
		{"not a command", false},
		{"tell me about /help", false},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			m := newModel(t, &stubRunner{})
			handled, _ := m.command(tc.input)
			if handled != tc.wantHandled {
				t.Errorf("command(%q) handled = %v, want %v", tc.input, handled, tc.wantHandled)
			}
		})
	}
}

func TestClearCommandEmptiesTranscript(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.transcript = []string{"one", "two"}
	m.command("/clear")
	if len(m.transcript) != 0 {
		t.Errorf("transcript should be cleared, got %v", m.transcript)
	}
}

func TestUnknownCommandIsReported(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.command("/definitely-not-real")
	joined := strings.Join(m.transcript, "\n")
	if !strings.Contains(joined, "unknown command") {
		t.Errorf("want an unknown-command message:\n%s", joined)
	}
}

func TestCtrlWTogglesGatePane(t *testing.T) {
	m := newModel(t, &stubRunner{})
	before := m.showGates
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlW})
	if updated.(*Model).showGates == before {
		t.Error("ctrl+w should toggle the gate pane")
	}
}

// TestCtrlCCancelsRunWhenBusy: interrupting a long gate suite should not lose
// the session.
func TestCtrlCCancelsRunWhenBusy(t *testing.T) {
	m := newModel(t, &stubRunner{})
	cancelled := false
	m.busy = true
	m.cancel = func() { cancelled = true }

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !cancelled {
		t.Error("ctrl+c while busy should cancel the run")
	}
	if cmd != nil {
		t.Error("ctrl+c while busy should not quit")
	}
}

func TestCtrlCQuitsWhenIdle(t *testing.T) {
	m := newModel(t, &stubRunner{})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Error("ctrl+c while idle should quit")
	}
}

func TestBlockedMessageIsSurfaced(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.Update(blockedMsg{reason: "tool \"edit\" is not available in state planning"})

	joined := strings.Join(m.transcript, "\n")
	if !strings.Contains(joined, "blocked") || !strings.Contains(joined, "not available") {
		t.Errorf("a block should be visible to the user:\n%s", joined)
	}
}

func TestGateMessagesUpdatePane(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.Update(gateStartMsg{id: "unit"})
	if !m.gates.Running() {
		t.Error("a gate start should mark the pane running")
	}
	m.Update(gateDoneMsg{receipt: workflow.Receipt{GateID: "unit", Success: true, DurationMS: 100}})
	if m.gates.Running() {
		t.Error("a receipt should end the running state")
	}
	if !strings.Contains(m.gates.View(0), glyphPass) {
		t.Error("the pane should show the pass")
	}
}

func TestTextDeltaAccumulates(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.Update(deltaMsg("Hello"))
	m.Update(deltaMsg(", world"))
	if got := m.streaming.String(); got != "Hello, world" {
		t.Errorf("streaming = %q", got)
	}
}

func TestDoneMovesStreamingIntoTranscript(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.busy = true
	m.Update(deltaMsg("the answer"))
	m.Update(doneMsg{result: agent.Result{Steps: 1}})

	if m.busy {
		t.Error("done should clear the busy flag")
	}
	if m.streaming.Len() != 0 {
		t.Error("streaming should be flushed")
	}
	if !strings.Contains(stripANSI(strings.Join(m.transcript, "\n")), "the answer") {
		t.Errorf("the reply should land in the transcript: %v", m.transcript)
	}
}

func TestDoneRecordsError(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.busy = true
	m.Update(doneMsg{err: errors.New("provider unreachable")})
	if m.err == nil {
		t.Fatal("the error should be recorded")
	}
	if !strings.Contains(m.statusBar(), "provider unreachable") {
		t.Error("the error should be visible in the status bar")
	}
}

func TestToolMessagesRecordOutcome(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.Update(toolMsg{name: "bash"})
	if m.activeTool != "bash" {
		t.Errorf("activeTool = %q", m.activeTool)
	}
	m.Update(toolMsg{name: "bash", finished: true, isError: true})
	if m.activeTool != "" {
		t.Error("a finished tool should clear the active marker")
	}
	if !strings.Contains(strings.Join(m.transcript, "\n"), glyphFail) {
		t.Errorf("a failed tool should be marked:\n%v", m.transcript)
	}
}

func TestViewRendersWithoutPanic(t *testing.T) {
	sizes := [][2]int{{100, 30}, {40, 10}, {200, 60}, {20, 6}}
	for _, size := range sizes {
		m := newModel(t, &stubRunner{})
		m.transcript = []string{"# Heading", "some **markdown**"}
		m.busy = true
		m.gates.Start("unit")
		m.resize(size[0], size[1])

		if view := m.View(); view == "" {
			t.Errorf("View() empty at %dx%d", size[0], size[1])
		}
	}
}

func TestResizeIgnoresDegenerateSizes(t *testing.T) {
	m := newModel(t, &stubRunner{})
	width := m.viewport.Width
	m.resize(0, 0)
	if m.viewport.Width != width {
		t.Error("a zero size should be ignored rather than breaking layout")
	}
}

func TestObserverDoesNotBlockWhenChannelFull(t *testing.T) {
	// A one-slot channel that is already full: progress frames must be
	// dropped rather than stalling the model loop.
	events := make(chan tea.Msg, 1)
	events <- deltaMsg("filler")
	obs := &observer{events: events}

	done := make(chan struct{})
	go func() {
		obs.TextDelta("dropped")
		obs.Blocked(tool.Call{Name: "edit"}, enforce.Decision{Reason: "no"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the observer must never block the loop")
	}
}

func TestSubmitMarksBusy(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	runner := &stubRunner{started: started, release: release}
	m := newModel(t, runner)

	cmd := m.submit("do the thing")
	if !m.busy {
		t.Error("submit should mark the model busy")
	}
	if !strings.Contains(strings.Join(m.transcript, "\n"), "do the thing") {
		t.Error("the prompt should appear in the transcript")
	}

	// submit returns a batch (the run plus a ticker restart), so the batch has
	// to be unwrapped rather than invoked directly.
	for _, c := range runBatch(t, cmd) {
		go c()
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the runner should have been invoked")
	}
	close(release)
}

// runBatch unwraps a tea.Batch into its component commands. A batch Cmd
// returns a BatchMsg rather than doing the work itself.
func runBatch(t *testing.T, cmd tea.Cmd) []tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		return msg
	default:
		// Not a batch: hand back a command that replays the message.
		return []tea.Cmd{func() tea.Msg { return msg }}
	}
}

// stripANSI removes styling so an assertion tests content, not presentation.
// It delegates to the package's own helper so there is one implementation.
func stripANSI(s string) string { return plainText(s) }

var _ tea.Model = (*Model)(nil)

// TestResizeDoesNotChangeGlamourStyle guards a real bug: resize used to
// rebuild the markdown renderer with glamour.WithAutoStyle(), which queries
// the terminal for its background colour. Because resize runs on a
// WindowSizeMsg — after Bubble Tea has taken over stdin — the terminal's reply
// was read as keyboard input and appeared in the prompt as literal junk
// (`]11;rgb:0000/0000/0000\[1;1R`).
func TestResizeDoesNotChangeGlamourStyle(t *testing.T) {
	m := New(Options{Runner: &stubRunner{}, GlamourStyle: "light"})
	if m.glamourStyle != "light" {
		t.Fatalf("glamourStyle = %q, want the style passed in", m.glamourStyle)
	}

	for _, size := range [][2]int{{100, 30}, {40, 12}, {200, 60}} {
		m.resize(size[0], size[1])
		if m.glamourStyle != "light" {
			t.Fatalf("resize changed the style to %q", m.glamourStyle)
		}
		if m.renderer == nil {
			t.Fatal("resize should keep a working renderer")
		}
	}

	// Rendering must still work at the pinned style.
	if out := m.render("# Heading"); out == "" {
		t.Error("render produced nothing")
	}
}

// TestNewNeverAutoDetects: an empty style must fall back to a fixed one, not
// probe the terminal, because New's callers are not all pre-program.
func TestNewNeverAutoDetects(t *testing.T) {
	m := New(Options{Runner: &stubRunner{}})
	if m.glamourStyle == "" {
		t.Fatal("a style should always be resolved")
	}
	if m.glamourStyle == "auto" {
		t.Error("the style must never be \"auto\": that re-queries the terminal on resize")
	}
}

// TestNoTerminalQueryingAPIsInModel is a source-level guard. The failure it
// prevents is invisible to a normal unit test — it only shows up as garbage in
// a real terminal — and the fix is easy to undo by reflex, so the constraint is
// asserted directly.
//
// Terminal detection belongs in DetectTheme, which runs before the Bubble Tea
// program starts.
func TestNoTerminalQueryingAPIsInModel(t *testing.T) {
	// Only call expressions count, so the comments explaining this constraint
	// do not trip it.
	banned := map[string]string{
		"WithAutoStyle":     "queries the terminal background; use WithStandardStyle with a style from DetectTheme",
		"HasDarkBackground": "queries the terminal; call it only from DetectTheme, before the program starts",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// theme.go is where detection is supposed to live.
		if name == "theme.go" {
			continue
		}

		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			var fn string
			switch target := call.Fun.(type) {
			case *ast.SelectorExpr:
				fn = target.Sel.Name
			case *ast.Ident:
				fn = target.Name
			}
			if why, forbidden := banned[fn]; forbidden {
				t.Errorf("%s calls %s, which %s", name, fn, why)
			}
			return true
		})
	}
}

// fakeSwitcher records real mode switches, standing in for the app.
type fakeSwitcher struct {
	available bool
	governed  bool
	state     workflow.State
	calls     []bool
	err       error
}

func (s *fakeSwitcher) SetGoverned(on bool) error {
	s.calls = append(s.calls, on)
	if s.err != nil {
		return s.err
	}
	s.governed = on
	if on {
		s.state = workflow.StatePlanning
	} else {
		s.state = ""
	}
	return nil
}

func (s *fakeSwitcher) GovernanceAvailable() bool     { return s.available }
func (s *fakeSwitcher) WorkflowState() workflow.State { return s.state }

func newSwitchableModel(t *testing.T, governed, available bool) (*Model, *fakeSwitcher) {
	t.Helper()
	sw := &fakeSwitcher{available: available, governed: governed}
	if governed {
		sw.state = workflow.StatePlanning
	}
	m := New(Options{
		Runner:   &stubRunner{},
		Switcher: sw,
		Gates:    testGates(),
		Governed: governed,
		State:    sw.state,
	})
	m.resize(120, 30)
	return m, sw
}

// TestCtrlGTogglesGovernance is the shortcut the status bar advertises.
func TestCtrlGTogglesGovernance(t *testing.T) {
	m, sw := newSwitchableModel(t, true, true)

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	if m.Governed {
		t.Error("ctrl+g should drop governance")
	}
	if len(sw.calls) != 1 || sw.calls[0] != false {
		t.Fatalf("the switcher should have been asked to disengage: %v", sw.calls)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	if !m.Governed {
		t.Error("ctrl+g should re-engage governance")
	}
	if len(sw.calls) != 2 || sw.calls[1] != true {
		t.Fatalf("the switcher should have been asked to engage: %v", sw.calls)
	}
	if m.State != workflow.StatePlanning {
		t.Errorf("state should come back from the switcher, got %q", m.State)
	}
}

// TestSwitchDelegatesRatherThanRelabelling guards the bug this replaced:
// /plain used to flip a display flag while the enforcer stayed active, so the
// message claimed something that had not happened.
func TestSwitchDelegatesRatherThanRelabelling(t *testing.T) {
	m, sw := newSwitchableModel(t, true, true)

	m.command("/plain")
	if len(sw.calls) == 0 {
		t.Fatal("/plain must delegate to the switcher, not just relabel the status bar")
	}
	if sw.governed {
		t.Error("the switcher should now be ungoverned")
	}
}

// TestFailedSwitchDoesNotClaimSuccess: if the switch is refused, the UI must
// not report a mode it is not in.
func TestFailedSwitchDoesNotClaimSuccess(t *testing.T) {
	m, sw := newSwitchableModel(t, false, true)
	sw.err = errors.New("no git work tree")

	m.setGoverned(true)
	if m.Governed {
		t.Error("a refused switch must leave the mode unchanged")
	}
	joined := strings.Join(m.transcript, "\n")
	if !strings.Contains(joined, "no git work tree") {
		t.Errorf("the reason should be surfaced:\n%s", joined)
	}
}

func TestCannotGovernWithoutPolicy(t *testing.T) {
	m, sw := newSwitchableModel(t, false, false)

	m.setGoverned(true)
	if len(sw.calls) != 0 {
		t.Error("the switcher should not be called when governance is unavailable")
	}
	if m.Governed {
		t.Error("mode should be unchanged")
	}
	if !strings.Contains(strings.Join(m.transcript, "\n"), "no workflow policy") {
		t.Errorf("the reason should be explained: %v", m.transcript)
	}
}

// TestModeIndicatorShowsCurrentMode: the left side reports where you are.
func TestModeIndicatorShowsCurrentMode(t *testing.T) {
	tests := []struct {
		name      string
		governed  bool
		available bool
		wantWord  string
	}{
		{"governed shows the state", true, true, "governed:planning"},
		{"plain is labelled", false, true, "plain"},
		{"plain with no policy says why", false, false, "no policy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newSwitchableModel(t, tc.governed, tc.available)
			if got := stripANSI(m.modeIndicator()); !strings.Contains(got, tc.wantWord) {
				t.Errorf("indicator = %q, want it to mention %q", got, tc.wantWord)
			}
		})
	}
}

// TestHintNamesTheActionNotTheState is the discoverability property: the hint
// tells you what the key will do, so it reads as an offer.
func TestHintNamesTheActionNotTheState(t *testing.T) {
	governed, _ := newSwitchableModel(t, true, true)
	hint := stripANSI(strings.Join(governed.hints(), " "))
	if !strings.Contains(hint, "ctrl+g plain") {
		t.Errorf("a governed session should offer the way out: %q", hint)
	}
	if !strings.Contains(hint, "ctrl+w gates") {
		t.Errorf("a governed session should offer the gate pane: %q", hint)
	}

	plain, _ := newSwitchableModel(t, false, true)
	hint = stripANSI(strings.Join(plain.hints(), " "))
	if !strings.Contains(hint, "ctrl+g govern") {
		t.Errorf("a plain session should offer the way in: %q", hint)
	}
	// The gate pane is meaningless without governance.
	if strings.Contains(hint, "gates") {
		t.Errorf("a plain session should not advertise gates: %q", hint)
	}
}

// TestNoSwitchHintWhenUnavailable: never offer a key that can only fail.
func TestNoSwitchHintWhenUnavailable(t *testing.T) {
	m, _ := newSwitchableModel(t, false, false)
	if hint := stripANSI(strings.Join(m.hints(), " ")); strings.Contains(hint, "ctrl+g") {
		t.Errorf("ctrl+g should not be offered when it cannot work: %q", hint)
	}
}

// TestStatusBarShowsBothModeAndShortcut is the thing that was missing: the bar
// has to say where you are *and* how to change it.
func TestStatusBarShowsBothModeAndShortcut(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	bar := stripANSI(m.statusBar())

	for _, want := range []string{"governed:planning", "ctrl+g plain"} {
		if !strings.Contains(bar, want) {
			t.Errorf("status bar should contain %q:\n%s", want, bar)
		}
	}
}

func TestSwitchRefusedWhileBusy(t *testing.T) {
	m, sw := newSwitchableModel(t, true, true)
	m.busy = true

	m.setGoverned(false)
	if len(sw.calls) != 0 {
		t.Error("mode must not change mid-turn")
	}
	if !strings.Contains(strings.Join(m.transcript, "\n"), "cancel the current turn") {
		t.Errorf("the user should be told why: %v", m.transcript)
	}
}

func TestSwitchToSameModeIsNoted(t *testing.T) {
	m, sw := newSwitchableModel(t, true, true)
	m.setGoverned(true)
	if len(sw.calls) != 0 {
		t.Error("switching to the current mode should be a no-op")
	}
	if !strings.Contains(strings.Join(m.transcript, "\n"), "already in governed") {
		t.Errorf("the user should be told: %v", m.transcript)
	}
}

func TestGovernCommandAliases(t *testing.T) {
	for _, cmd := range []string{"/govern", "/gated"} {
		t.Run(cmd, func(t *testing.T) {
			m, sw := newSwitchableModel(t, false, true)
			m.command(cmd)
			if len(sw.calls) != 1 || !sw.calls[0] {
				t.Errorf("%s should engage governance: %v", cmd, sw.calls)
			}
		})
	}
}

func TestModeCommandToggles(t *testing.T) {
	m, sw := newSwitchableModel(t, true, true)
	m.command("/mode")
	if len(sw.calls) != 1 || sw.calls[0] {
		t.Errorf("/mode should toggle to plain: %v", sw.calls)
	}
}

// TestHelpAdvertisesTheShortcut: /help is where a user looks first.
func TestHelpAdvertisesTheShortcut(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.command("/help")
	help := stripANSI(strings.Join(m.transcript, "\n"))
	for _, want := range []string{"ctrl+g", "/govern", "/plain"} {
		if !strings.Contains(help, want) {
			t.Errorf("/help should mention %q:\n%s", want, help)
		}
	}
}

// TestGatePaneFollowsMode: gates are meaningless when ungoverned.
func TestGatePaneFollowsMode(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	if !m.showGates {
		t.Fatal("a governed session with gates should show the pane")
	}
	m.setGoverned(false)
	if m.showGates {
		t.Error("dropping governance should hide the gate pane")
	}
	m.setGoverned(true)
	if !m.showGates {
		t.Error("re-engaging should bring it back")
	}
}

// fakeModels records real model switches, standing in for the app.
type fakeModels struct {
	refs    []string
	current string
	calls   []string
	err     error
	// windows maps a reference to its declared context window.
	windows map[string]int
}

func (f *fakeModels) ModelRefs() []string { return f.refs }

func (f *fakeModels) DescribeModel(ref string) string { return "desc:" + ref }

func (f *fakeModels) CurrentModel() string { return f.current }

func (f *fakeModels) ContextWindow() int { return f.windows[f.current] }

func (f *fakeModels) SetModel(ref string) error {
	f.calls = append(f.calls, ref)
	if f.err != nil {
		return f.err
	}
	f.current = ref
	return nil
}

func newPickableModel(t *testing.T, refs ...string) (*Model, *fakeModels) {
	t.Helper()
	if len(refs) == 0 {
		refs = []string{"ollama/qwen3.5", "gw/sonnet", "gw/nemotron"}
	}
	fm := &fakeModels{refs: refs, current: refs[0]}
	m := New(Options{
		Runner:    &stubRunner{},
		Switcher:  &fakeSwitcher{available: true, governed: true, state: workflow.StatePlanning},
		Models:    fm,
		Gates:     testGates(),
		Governed:  true,
		ModelName: refs[0],
	})
	m.resize(120, 30)
	return m, fm
}

// TestCtrlPOpensModelPicker is the shortcut the status bar advertises.
func TestCtrlPOpensModelPicker(t *testing.T) {
	m, fm := newPickableModel(t)

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	if !m.picker.IsOpen() {
		t.Fatal("ctrl+p should open the model picker")
	}
	if m.picker.Len() != len(fm.refs) {
		t.Errorf("picker has %d rows, want %d", m.picker.Len(), len(fm.refs))
	}
	// It should start on the model currently in use.
	if got, _ := m.picker.Selected(); got.Value != fm.current {
		t.Errorf("picker starts on %q, want the current model %q", got.Value, fm.current)
	}
}

// TestPickerSelectionSwitchesModel is the substance: selecting a row must
// delegate, not just relabel the status bar.
func TestPickerSelectionSwitchesModel(t *testing.T) {
	m, fm := newPickableModel(t)

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if m.picker.IsOpen() {
		t.Error("confirming should close the picker")
	}
	if len(fm.calls) != 1 || fm.calls[0] != "gw/sonnet" {
		t.Fatalf("the switcher should have been asked for gw/sonnet: %v", fm.calls)
	}
	if m.ModelName != "gw/sonnet" {
		t.Errorf("ModelName = %q, want the new model", m.ModelName)
	}
	if !strings.Contains(stripANSI(m.statusBar()), "gw/sonnet") {
		t.Error("the status bar should show the new model")
	}
}

func TestPickerEscapeCancels(t *testing.T) {
	m, fm := newPickableModel(t)

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if m.picker.IsOpen() {
		t.Error("esc should close the picker")
	}
	if len(fm.calls) != 0 {
		t.Errorf("cancelling must not switch: %v", fm.calls)
	}
	if m.ModelName != "ollama/qwen3.5" {
		t.Errorf("ModelName = %q, want it unchanged", m.ModelName)
	}
}

// TestPickerOwnsEnterWhileOpen: without this, enter would submit a prompt
// instead of selecting a row.
func TestPickerOwnsEnterWhileOpen(t *testing.T) {
	m, _ := newPickableModel(t)
	m.input.SetValue("a prompt I am typing")

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if m.busy {
		t.Error("enter should have selected a row, not submitted the prompt")
	}
	if m.input.Value() != "a prompt I am typing" {
		t.Errorf("the typed prompt should survive, got %q", m.input.Value())
	}
}

// TestPickerSwallowsTypingWhileOpen: keys must not leak into the input box
// behind the overlay.
func TestPickerSwallowsTypingWhileOpen(t *testing.T) {
	m, _ := newPickableModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if strings.Contains(m.input.Value(), "x") {
		t.Errorf("typing should not reach the input while picking, got %q", m.input.Value())
	}
}

func TestFailedModelSwitchDoesNotRelabel(t *testing.T) {
	m, fm := newPickableModel(t)
	fm.err = errors.New("unknown provider \"gw\"")

	m.applyModel("gw/sonnet")
	if m.ModelName != "ollama/qwen3.5" {
		t.Error("a refused switch must leave the label unchanged")
	}
	if !strings.Contains(strings.Join(m.transcript, "\n"), "unknown provider") {
		t.Errorf("the reason should be surfaced: %v", m.transcript)
	}
}

func TestSwitchingToCurrentModelIsNoted(t *testing.T) {
	m, fm := newPickableModel(t)
	m.applyModel("ollama/qwen3.5")
	if len(fm.calls) != 0 {
		t.Error("switching to the current model should be a no-op")
	}
	if !strings.Contains(strings.Join(m.transcript, "\n"), "already using") {
		t.Errorf("the user should be told: %v", m.transcript)
	}
}

func TestModelCommandWithArgumentSwitchesDirectly(t *testing.T) {
	for _, cmd := range []string{"/model gw/sonnet", "/provider gw/sonnet"} {
		t.Run(cmd, func(t *testing.T) {
			m, fm := newPickableModel(t)
			m.command(cmd)
			if len(fm.calls) != 1 || fm.calls[0] != "gw/sonnet" {
				t.Errorf("%s should switch directly: %v", cmd, fm.calls)
			}
			if m.picker.IsOpen() {
				t.Error("an explicit argument should not open the picker")
			}
		})
	}
}

func TestBareModelCommandOpensPicker(t *testing.T) {
	m, fm := newPickableModel(t)
	m.command("/model")
	if !m.picker.IsOpen() {
		t.Error("/model with no argument should offer the list")
	}
	if len(fm.calls) != 0 {
		t.Error("opening the list should not switch anything")
	}
}

// TestSingleModelNeedsNoPicker: offering a one-item list is just noise.
func TestSingleModelNeedsNoPicker(t *testing.T) {
	m, _ := newPickableModel(t, "ollama/qwen3.5")
	m.openModelPicker()
	if m.picker.IsOpen() {
		t.Error("a single configured model should not open a picker")
	}
	if !strings.Contains(strings.Join(m.transcript, "\n"), "only one model") {
		t.Errorf("the user should be told why: %v", m.transcript)
	}
}

func TestNoModelsConfiguredIsExplained(t *testing.T) {
	fm := &fakeModels{}
	m := New(Options{Runner: &stubRunner{}, Models: fm})
	m.resize(120, 30)

	m.openModelPicker()
	if m.picker.IsOpen() {
		t.Error("nothing to pick")
	}
	if !strings.Contains(strings.Join(m.transcript, "\n"), "no models are configured") {
		t.Errorf("the user should be told: %v", m.transcript)
	}
}

func TestModelSwitchRefusedWhileBusy(t *testing.T) {
	m, fm := newPickableModel(t)
	m.busy = true

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	if m.picker.IsOpen() {
		t.Error("the model must not change mid-turn")
	}
	if len(fm.calls) != 0 {
		t.Error("nothing should have been switched")
	}
	if !strings.Contains(strings.Join(m.transcript, "\n"), "cancel the current turn") {
		t.Errorf("the user should be told why: %v", m.transcript)
	}
}

// TestModelHintShownOnlyWhenUseful: never advertise a key that opens a
// one-item list.
func TestModelHintShownOnlyWhenUseful(t *testing.T) {
	many, _ := newPickableModel(t)
	if !strings.Contains(stripANSI(strings.Join(many.hints(), " ")), "ctrl+p model") {
		t.Errorf("several models should advertise the shortcut: %v", many.hints())
	}

	one, _ := newPickableModel(t, "ollama/qwen3.5")
	if strings.Contains(stripANSI(strings.Join(one.hints(), " ")), "ctrl+p") {
		t.Errorf("a single model should not advertise it: %v", one.hints())
	}
}

func TestHelpAdvertisesModelSwitch(t *testing.T) {
	m, _ := newPickableModel(t)
	m.command("/help")
	help := stripANSI(strings.Join(m.transcript, "\n"))
	for _, want := range []string{"ctrl+p", "/model"} {
		if !strings.Contains(help, want) {
			t.Errorf("/help should mention %q:\n%s", want, help)
		}
	}
}

func TestPickerIsRenderedInTheFrame(t *testing.T) {
	m, _ := newPickableModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})

	view := stripANSI(m.View())
	if !strings.Contains(view, "Switch model") {
		t.Errorf("the open picker should appear in the frame:\n%s", view)
	}
	// It must not crowd out the input box.
	if !strings.Contains(view, "Ask anything") {
		t.Errorf("the input should still be visible:\n%s", view)
	}
}

func TestModelSwitchWithoutSwitcherIsRefused(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.models = nil
	m.applyModel("gw/sonnet")
	if !strings.Contains(strings.Join(m.transcript, "\n"), "cannot switch model") {
		t.Errorf("the refusal should be explained: %v", m.transcript)
	}
}

// typeAndSubmit simulates typing a prompt, pressing enter, and the turn
// completing. The completion matters: submitting marks the model busy, and a
// busy model ignores enter, so without it only the first prompt would land.
func typeAndSubmit(t *testing.T, m *Model, prompt string) {
	t.Helper()
	m.input.SetValue(prompt)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.Update(doneMsg{})
}

func pressUp(m *Model)   { m.Update(tea.KeyMsg{Type: tea.KeyUp}) }
func pressDown(m *Model) { m.Update(tea.KeyMsg{Type: tea.KeyDown}) }

// TestUpArrowRecallsPreviousPrompt is the headline behaviour.
func TestUpArrowRecallsPreviousPrompt(t *testing.T) {
	m := newModel(t, &stubRunner{})
	typeAndSubmit(t, m, "fix the failing test")

	if m.input.Value() != "" {
		t.Fatalf("submitting should clear the input, got %q", m.input.Value())
	}

	pressUp(m)
	if got := m.input.Value(); got != "fix the failing test" {
		t.Errorf("input = %q, want the previous prompt", got)
	}
}

func TestUpArrowWalksBackThroughHistory(t *testing.T) {
	m := newModel(t, &stubRunner{})
	for _, p := range []string{"first", "second", "third"} {
		typeAndSubmit(t, m, p)
	}

	for _, want := range []string{"third", "second", "first"} {
		pressUp(m)
		if got := m.input.Value(); got != want {
			t.Fatalf("input = %q, want %q", got, want)
		}
	}
	// Holding up at the oldest entry stays put.
	pressUp(m)
	if got := m.input.Value(); got != "first" {
		t.Errorf("input = %q, want it to hold at the oldest", got)
	}
}

func TestDownArrowWalksForwardAndRestoresDraft(t *testing.T) {
	m := newModel(t, &stubRunner{})
	typeAndSubmit(t, m, "older")
	typeAndSubmit(t, m, "newer")

	m.input.SetValue("half-written thought")
	pressUp(m)
	pressUp(m)
	if got := m.input.Value(); got != "older" {
		t.Fatalf("input = %q, want older", got)
	}

	pressDown(m)
	if got := m.input.Value(); got != "newer" {
		t.Fatalf("input = %q, want newer", got)
	}
	pressDown(m)
	if got := m.input.Value(); got != "half-written thought" {
		t.Errorf("input = %q, want the draft restored", got)
	}
}

// TestUpArrowMovesCursorInMultilineDraft: history must not hijack the arrow
// key while there is a draft to navigate.
func TestUpArrowMovesCursorInMultilineDraft(t *testing.T) {
	m := newModel(t, &stubRunner{})
	typeAndSubmit(t, m, "a previous prompt")

	m.input.SetValue("line one\nline two")
	m.input.CursorEnd()
	if m.input.Line() == 0 {
		t.Fatal("precondition: cursor should be on the second line")
	}

	pressUp(m)
	// The draft survives; the cursor just moved up a line.
	if got := m.input.Value(); got != "line one\nline two" {
		t.Errorf("input = %q, want the draft untouched", got)
	}
	if m.input.Line() != 0 {
		t.Errorf("cursor line = %d, want 0", m.input.Line())
	}

	// Now on the first line, up recalls history.
	pressUp(m)
	if got := m.input.Value(); got != "a previous prompt" {
		t.Errorf("input = %q, want the recalled prompt", got)
	}
}

// TestDownArrowMovesCursorInsteadOfSteppingForward is the symmetric case.
func TestDownArrowMovesCursorInRecalledMultilineEntry(t *testing.T) {
	m := newModel(t, &stubRunner{})
	typeAndSubmit(t, m, "line one\nline two")
	typeAndSubmit(t, m, "single")

	pressUp(m)
	pressUp(m) // the multi-line entry
	if got := m.input.Value(); got != "line one\nline two" {
		t.Fatalf("input = %q", got)
	}
	// CursorStart moves to the start of the current line, not of the text, so
	// stepping up is what reaches line 0.
	m.input.CursorUp()
	if m.input.Line() != 0 {
		t.Fatalf("precondition: cursor should be on the first line, got %d", m.input.Line())
	}

	pressDown(m)
	// Still the same entry: down moved the cursor within it.
	if got := m.input.Value(); got != "line one\nline two" {
		t.Errorf("input = %q, want the entry unchanged", got)
	}
}

func TestDownArrowDoesNothingWhenNotBrowsing(t *testing.T) {
	m := newModel(t, &stubRunner{})
	typeAndSubmit(t, m, "something")
	m.input.SetValue("current draft")

	pressDown(m)
	if got := m.input.Value(); got != "current draft" {
		t.Errorf("input = %q, want it untouched", got)
	}
}

func TestUpArrowWithEmptyHistoryDoesNothing(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.input.SetValue("typing")
	pressUp(m)
	if got := m.input.Value(); got != "typing" {
		t.Errorf("input = %q, want it untouched", got)
	}
}

// TestSubmittingResetsBrowsing: after sending, up should give the newest
// entry, not resume mid-walk.
func TestSubmittingResetsBrowsing(t *testing.T) {
	m := newModel(t, &stubRunner{})
	typeAndSubmit(t, m, "one")
	typeAndSubmit(t, m, "two")

	pressUp(m)
	pressUp(m) // now showing "one"
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	pressUp(m)
	if got := m.input.Value(); got != "one" {
		t.Errorf("input = %q, want the just-submitted prompt", got)
	}
}

func TestRecalledPromptCanBeEditedAndResubmitted(t *testing.T) {
	m := newModel(t, &stubRunner{})
	typeAndSubmit(t, m, "run the tests")

	pressUp(m)
	// Cursor sits at the end, so typing appends.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" again")})
	if got := m.input.Value(); got != "run the tests again" {
		t.Fatalf("input = %q, want the edit appended", got)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.history.Entries(); len(got) != 2 || got[1] != "run the tests again" {
		t.Errorf("entries = %v, want the edited prompt recorded", got)
	}
}

// TestSlashCommandsAreRecallable: a mistyped command is exactly what one wants
// to arrow back to.
func TestSlashCommandsAreRecallable(t *testing.T) {
	m := newModel(t, &stubRunner{})
	typeAndSubmit(t, m, "/modle")

	pressUp(m)
	if got := m.input.Value(); got != "/modle" {
		t.Errorf("input = %q, want the mistyped command back", got)
	}
}

func TestBlankSubmissionIsNotRecorded(t *testing.T) {
	m := newModel(t, &stubRunner{})
	typeAndSubmit(t, m, "real prompt")
	m.input.SetValue("   ")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if got := m.history.Entries(); len(got) != 1 {
		t.Errorf("entries = %v, want only the real prompt", got)
	}
}

// TestPickerKeepsArrowKeysWhileOpen: the picker is checked first, so browsing
// models must not also walk the prompt history.
func TestPickerKeepsArrowKeysWhileOpen(t *testing.T) {
	m, _ := newPickableModel(t)
	typeAndSubmit(t, m, "a previous prompt")
	m.busy = false

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	before := m.input.Value()

	pressUp(m)
	pressDown(m)
	if m.input.Value() != before {
		t.Errorf("the input changed while the picker was open: %q", m.input.Value())
	}
	if !m.picker.IsOpen() {
		t.Error("arrow keys should still be driving the picker")
	}
}

func gateReceipt(id string, success bool, ms int64, reason, stdout string) workflow.Receipt {
	code := 0
	if !success {
		code = 1
	}
	return workflow.Receipt{
		GateID: id, Success: success, ExitCode: &code,
		FailureReason: reason, DurationMS: ms, Stdout: stdout,
	}
}

// TestGateDetailShownByDefault: the point of the pane is to show what the
// runtime did, so hiding it is opt-in.
func TestGateDetailShownByDefault(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.Update(gateStartMsg{id: "unit"})
	m.Update(gateOutputMsg{id: "unit", line: "ok  pkg/api  0.4s"})

	view := stripANSI(m.gates.View(0))
	if !strings.Contains(view, "ok  pkg/api") {
		t.Errorf("streamed output should be visible without asking:\n%s", view)
	}
	if !strings.Contains(view, "$ go test ./...") {
		t.Errorf("the command should be shown:\n%s", view)
	}
	if m.gates.Collapsed(0) {
		t.Error("gates should start expanded")
	}
}

// TestStreamedOutputReachesThePane closes the loop that was previously dead:
// GateOutput was declared but never called, so nothing streamed at all.
func TestStreamedOutputReachesThePane(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.Update(gateStartMsg{id: "unit"})
	for _, line := range []string{"first line", "second line", "third line"} {
		m.Update(gateOutputMsg{id: "unit", line: line})
	}

	view := stripANSI(m.gates.View(0))
	for _, want := range []string{"first line", "second line", "third line"} {
		if !strings.Contains(view, want) {
			t.Errorf("output %q should be shown:\n%s", want, view)
		}
	}
}

func TestGateToggleHidesAndRestoresDetail(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.Update(gateStartMsg{id: "unit"})
	m.Update(gateOutputMsg{id: "unit", line: "revealing detail"})

	m.gates.Toggle(0)
	view := stripANSI(m.gates.View(0))
	if strings.Contains(view, "revealing detail") {
		t.Errorf("collapsed detail should be hidden:\n%s", view)
	}
	// Nothing should vanish silently: the header says how much is hidden.
	if !strings.Contains(view, "1 lines hidden") {
		t.Errorf("a collapsed gate should say what it is hiding:\n%s", view)
	}
	if !strings.Contains(view, "unit") {
		t.Errorf("the header stays visible:\n%s", view)
	}

	m.gates.Toggle(0)
	if !strings.Contains(stripANSI(m.gates.View(0)), "revealing detail") {
		t.Error("toggling again should restore the detail")
	}
}

// TestCollapseMarkerReflectsState: the marker is the click affordance, so it
// has to be right.
func TestCollapseMarkerReflectsState(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.Update(gateStartMsg{id: "unit"})
	m.Update(gateOutputMsg{id: "unit", line: "x"})

	if !strings.Contains(stripANSI(m.gates.View(0)), "▾") {
		t.Error("an expanded gate should show the open marker")
	}
	m.gates.Toggle(0)
	if !strings.Contains(stripANSI(m.gates.View(0)), "▸") {
		t.Error("a collapsed gate should show the closed marker")
	}
}

func TestGateWithNoDetailHasNoMarker(t *testing.T) {
	// A pending gate with no command and no output has nothing to expand.
	pane := NewGatePane([]workflow.Gate{{ID: "bare"}})
	view := stripANSI(pane.View(0))
	for _, marker := range []string{"▾", "▸"} {
		if strings.Contains(view, marker) {
			t.Errorf("a gate with nothing to show should not offer a toggle:\n%s", view)
		}
	}
}

// TestClickTogglesGate is the interaction the request asked for.
func TestClickTogglesGate(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.Update(gateStartMsg{id: "unit"})
	m.Update(gateOutputMsg{id: "unit", line: "detail line"})
	// Render so the pane records its row mapping and absolute position.
	_ = m.View()

	// The first gate's header is the first content row, one below the border.
	// The row is recomputed after each render because collapsing shrinks the
	// pane, which moves it down the screen.
	clickFirstGate := func() {
		m.Update(tea.MouseMsg{
			Action: tea.MouseActionPress,
			Button: tea.MouseButtonLeft,
			Y:      m.gateTop + 1,
		})
	}

	clickFirstGate()
	if !m.gates.Collapsed(0) {
		t.Error("clicking a gate header should collapse it")
	}

	_ = m.View()
	clickFirstGate()
	if m.gates.Collapsed(0) {
		t.Error("clicking again should expand it")
	}
}

func TestClickOutsideGatePaneIsIgnored(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.Update(gateStartMsg{id: "unit"})
	_ = m.View()

	for _, y := range []int{0, m.gateTop, m.height - 1} {
		m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: y})
	}
	if m.gates.Collapsed(0) {
		t.Error("clicks outside a gate header should not toggle anything")
	}
}

func TestNonLeftClicksIgnored(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.Update(gateStartMsg{id: "unit"})
	_ = m.View()

	y := m.gateTop + 1
	for _, msg := range []tea.MouseMsg{
		{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, Y: y},
		{Action: tea.MouseActionMotion, Button: tea.MouseButtonNone, Y: y},
		{Action: tea.MouseActionPress, Button: tea.MouseButtonRight, Y: y},
		{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp, Y: y},
	} {
		m.Update(msg)
	}
	if m.gates.Collapsed(0) {
		t.Error("only a left press should toggle")
	}
}

func TestClickIgnoredWhenPaneHidden(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.Update(gateStartMsg{id: "unit"})
	_ = m.View()
	y := m.gateTop + 1

	m.showGates = false
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: y})
	if m.gates.Collapsed(0) {
		t.Error("a hidden pane should not receive clicks")
	}
}

// TestClickOnDetailRowTogglesItsGate: clicking anywhere in a gate's block
// should act on that gate, not miss.
func TestClickOnDetailRowTogglesItsGate(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.Update(gateStartMsg{id: "unit"})
	m.Update(gateOutputMsg{id: "unit", line: "some output"})
	_ = m.View()

	// Row 0 is the header, row 1 the command, row 2 the output line.
	m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		Y: m.gateTop + 1 + 2,
	})
	if !m.gates.Collapsed(0) {
		t.Error("clicking a detail row should toggle the gate it belongs to")
	}
}

// TestKeyboardTogglesGate: a TUI must be usable without a mouse.
func TestKeyboardTogglesGate(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.Update(gateStartMsg{id: "unit"})
	m.Update(gateOutputMsg{id: "unit", line: "detail"})

	if m.gates.Selected() != -1 {
		t.Fatal("nothing should be selected initially")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.gates.Selected() != 0 {
		t.Fatalf("tab should select the first gate, got %d", m.gates.Selected())
	}

	m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	if !m.gates.Collapsed(0) {
		t.Error("space should toggle the selected gate")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.gates.Selected() != 1 {
		t.Errorf("tab should advance to the next gate, got %d", m.gates.Selected())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.gates.Selected() != 0 {
		t.Errorf("shift+tab should step back, got %d", m.gates.Selected())
	}
}

// TestSpaceStillTypesWhenNoGateSelected: the toggle must not steal the space
// bar from the prompt.
func TestSpaceStillTypesWhenNoGateSelected(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.gates.ClearSelection()
	m.input.SetValue("two")

	m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("words")})
	if got := m.input.Value(); got != "two words" {
		t.Errorf("input = %q, want the space to reach the prompt", got)
	}
}

func TestCtrlOFoldsAndUnfoldsEverything(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.Update(gateStartMsg{id: "unit"})
	m.Update(gateOutputMsg{id: "unit", line: "a"})
	m.Update(gateStartMsg{id: "integration"})
	m.Update(gateOutputMsg{id: "integration", line: "b"})

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	if !m.gates.Collapsed(0) || !m.gates.Collapsed(1) {
		t.Error("ctrl+o should collapse every gate")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	if m.gates.Collapsed(0) || m.gates.Collapsed(1) {
		t.Error("ctrl+o again should expand every gate")
	}

	// A mixed state expands, since revealing is the safer default.
	m.gates.Toggle(0)
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	if m.gates.Collapsed(0) || m.gates.Collapsed(1) {
		t.Error("a partially collapsed pane should expand")
	}
}

// TestFailingGateShowsItsOutput is why this matters: a blocked completion has
// to be explainable from the screen.
func TestFailingGateShowsItsOutput(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.Update(gateStartMsg{id: "unit"})
	m.Update(gateDoneMsg{receipt: gateReceipt("unit", false, 2400, "command_failed",
		"--- FAIL: TestAdd\n    add_test.go:7: Add(2,3) = -1, want 5\n")})

	view := stripANSI(m.gates.View(0))
	for _, want := range []string{"unit", "command_failed", "want 5"} {
		if !strings.Contains(view, want) {
			t.Errorf("view should contain %q:\n%s", want, view)
		}
	}
}

// TestReceiptOutputUsedWhenNothingStreamed covers a command that buffered its
// output, or a runner that does not stream.
func TestReceiptOutputUsedWhenNothingStreamed(t *testing.T) {
	pane := NewGatePane(testGates())
	pane.Finish(gateReceipt("unit", true, 100, "", "buffered line\n"))

	if !strings.Contains(stripANSI(pane.View(0)), "buffered line") {
		t.Error("the receipt's captured output should be shown when nothing streamed")
	}
}

// TestStreamedOutputNotOverwrittenByReceipt: the streamed lines are the live
// record and should not be replaced when the receipt lands.
func TestStreamedOutputNotOverwrittenByReceipt(t *testing.T) {
	pane := NewGatePane(testGates())
	pane.Start("unit")
	pane.Output("unit", "streamed line")
	pane.Finish(gateReceipt("unit", true, 100, "", "different captured text\n"))

	view := stripANSI(pane.View(0))
	if !strings.Contains(view, "streamed line") {
		t.Errorf("streamed output should survive:\n%s", view)
	}
	if strings.Contains(view, "different captured text") {
		t.Errorf("it should not be duplicated from the receipt:\n%s", view)
	}
}

// TestLongOutputShowsTail: a failure's cause is at the end.
func TestLongOutputShowsTail(t *testing.T) {
	pane := NewGatePane(testGates())
	pane.Start("unit")
	for i := range 40 {
		pane.Output("unit", fmt.Sprintf("line %d", i))
	}

	view := stripANSI(pane.View(0))
	if !strings.Contains(view, "line 39") {
		t.Errorf("the last line should be visible:\n%s", view)
	}
	if strings.Contains(view, "line 0\n") {
		t.Errorf("early lines should be trimmed:\n%s", view)
	}
	if !strings.Contains(view, "earlier lines") {
		t.Errorf("the trim should be acknowledged:\n%s", view)
	}
}

func TestOutputIsCapped(t *testing.T) {
	pane := NewGatePane(testGates())
	pane.Start("unit")
	for i := range maxOutputLines + 250 {
		pane.Output("unit", fmt.Sprintf("line %d", i))
	}
	if got := len(pane.gates[0].Output); got != maxOutputLines {
		t.Errorf("retained %d lines, want the cap of %d", got, maxOutputLines)
	}
}

func TestBlankOutputLinesIgnored(t *testing.T) {
	pane := NewGatePane(testGates())
	pane.Start("unit")
	for _, line := range []string{"", "   ", "\t", "real"} {
		pane.Output("unit", line)
	}
	if got := pane.gates[0].Output; len(got) != 1 || got[0] != "real" {
		t.Errorf("output = %v, want only the real line", got)
	}
}

func TestOutputForUnknownGateIgnored(t *testing.T) {
	pane := NewGatePane(testGates())
	pane.Output("nonexistent", "line")
	for i := range pane.gates {
		if len(pane.gates[i].Output) != 0 {
			t.Error("output for an unknown gate should be dropped")
		}
	}
}

// TestResetKeepsExpansionState: collapsing is a display preference, not run
// state, so a re-run should not undo it.
func TestResetKeepsExpansionState(t *testing.T) {
	pane := NewGatePane(testGates())
	pane.Start("unit")
	pane.Output("unit", "old run")
	pane.Toggle(0)

	pane.Reset()
	if !pane.Collapsed(0) {
		t.Error("expansion state should survive a reset")
	}
	if len(pane.gates[0].Output) != 0 {
		t.Error("output should be cleared by a reset")
	}
}

func TestGateSelectClamps(t *testing.T) {
	pane := NewGatePane(testGates())
	pane.Select(-5)
	if pane.Selected() != 0 {
		t.Errorf("first Select should land on 0, got %d", pane.Selected())
	}
	pane.Select(-5)
	if pane.Selected() != 0 {
		t.Errorf("selection should clamp at the top, got %d", pane.Selected())
	}
	pane.Select(99)
	if pane.Selected() != pane.Len()-1 {
		t.Errorf("selection should clamp at the bottom, got %d", pane.Selected())
	}
}

func TestGateSelectOnEmptyPaneIsSafe(t *testing.T) {
	pane := NewGatePane(nil)
	pane.Select(1)
	pane.Toggle(0)
	pane.ToggleAll()
	if pane.View(0) != "" {
		t.Error("an empty pane renders nothing")
	}
}

func TestGateAtRowOutOfRange(t *testing.T) {
	pane := NewGatePane(testGates())
	pane.View(0)
	for _, row := range []int{-1, 9999} {
		if got := pane.GateAtRow(row); got != -1 {
			t.Errorf("GateAtRow(%d) = %d, want -1", row, got)
		}
	}
}

func TestPaneGrowthTriggersRelayout(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	before := m.viewport.Height

	m.Update(gateStartMsg{id: "unit"})
	for i := range 6 {
		m.Update(gateOutputMsg{id: "unit", line: fmt.Sprintf("line %d", i)})
	}
	// The pane got taller, so the viewport must have given up rows.
	if m.viewport.Height >= before {
		t.Errorf("viewport height %d should have shrunk from %d as the pane grew",
			m.viewport.Height, before)
	}
	// And the input must still be on screen.
	if !strings.Contains(stripANSI(m.View()), "Ask anything") {
		t.Error("the input should still be visible")
	}
}

// captureClipboard replaces the clipboard writer and returns what was written.
func captureClipboard(m *Model) *string {
	var last string
	m.writeClipboard = func(text string) error {
		last = text
		return nil
	}
	return &last
}

// fillTranscript adds enough entries to make the viewport scrollable.
func fillTranscript(m *Model, n int) {
	for i := range n {
		m.transcript = append(m.transcript, fmt.Sprintf("entry %d", i))
	}
	m.refresh()
}

func TestFollowsNewestOutputByDefault(t *testing.T) {
	m := newModel(t, &stubRunner{})
	if !m.follow {
		t.Fatal("a new session should follow the newest output")
	}
	fillTranscript(m, 100)
	if !m.viewport.AtBottom() {
		t.Error("following should keep the viewport at the bottom")
	}
}

// TestScrollingUpStopsFollowing is the property that makes reading history
// possible: streamed output must not yank you back to the bottom.
func TestScrollingUpStopsFollowing(t *testing.T) {
	m := newModel(t, &stubRunner{})
	fillTranscript(m, 100)

	m.Update(tea.KeyMsg{Type: tea.KeyShiftUp})
	if m.follow {
		t.Fatal("scrolling up should stop following")
	}
	offset := m.viewport.YOffset

	// New output arrives; the view must stay where the reader put it.
	m.Update(deltaMsg("a fresh token"))
	if m.viewport.YOffset != offset {
		t.Errorf("view jumped from %d to %d while scrolled back", offset, m.viewport.YOffset)
	}
	if m.viewport.AtBottom() {
		t.Error("the view should still be scrolled back")
	}
}

func TestEndResumesFollowing(t *testing.T) {
	m := newModel(t, &stubRunner{})
	fillTranscript(m, 100)
	m.Update(tea.KeyMsg{Type: tea.KeyShiftUp})
	if m.follow {
		t.Fatal("precondition: should have stopped following")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if !m.follow {
		t.Error("end should resume following")
	}
	if !m.viewport.AtBottom() {
		t.Error("end should jump to the newest output")
	}
}

func TestScrollKeys(t *testing.T) {
	tests := []struct {
		name string
		key  tea.KeyMsg
		// wantUp is true when the key should move the view backwards.
		wantUp bool
	}{
		{"pgup", tea.KeyMsg{Type: tea.KeyPgUp}, true},
		{"shift+up", tea.KeyMsg{Type: tea.KeyShiftUp}, true},
		{"home", tea.KeyMsg{Type: tea.KeyHome}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(t, &stubRunner{})
			fillTranscript(m, 200)
			before := m.viewport.YOffset

			m.Update(tc.key)
			if tc.wantUp && m.viewport.YOffset >= before {
				t.Errorf("%s should scroll back: %d -> %d", tc.name, before, m.viewport.YOffset)
			}
		})
	}

	// And forward again.
	m := newModel(t, &stubRunner{})
	fillTranscript(m, 200)
	m.Update(tea.KeyMsg{Type: tea.KeyHome})
	top := m.viewport.YOffset
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyPgDown},
		{Type: tea.KeyShiftDown},
	} {
		m.Update(key)
	}
	if m.viewport.YOffset <= top {
		t.Errorf("pgdown/shift+down should scroll forward from %d, got %d", top, m.viewport.YOffset)
	}
}

// TestMouseWheelScrolls is what most people will reach for first.
func TestMouseWheelScrolls(t *testing.T) {
	m := newModel(t, &stubRunner{})
	fillTranscript(m, 200)
	before := m.viewport.YOffset

	m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	if m.viewport.YOffset >= before {
		t.Fatalf("wheel up should scroll back: %d -> %d", before, m.viewport.YOffset)
	}
	if m.follow {
		t.Error("wheel up should stop following")
	}

	up := m.viewport.YOffset
	m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if m.viewport.YOffset <= up {
		t.Errorf("wheel down should scroll forward: %d -> %d", up, m.viewport.YOffset)
	}
}

// TestWheelScrollDoesNotToggleGates: the wheel must not be mistaken for a
// click on the gate pane.
func TestWheelScrollDoesNotToggleGates(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.Update(gateStartMsg{id: "unit"})
	m.Update(gateOutputMsg{id: "unit", line: "detail"})
	_ = m.View()

	m.Update(tea.MouseMsg{
		Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress,
		Y: m.gateTop + 1,
	})
	if m.gates.Collapsed(0) {
		t.Error("scrolling over the gate pane should not collapse a gate")
	}
}

// TestMouseReleaseEnablesNativeSelection: capturing the mouse is what stops a
// terminal drag-selecting, so it has to be releasable.
func TestMouseReleaseEnablesNativeSelection(t *testing.T) {
	m := newModel(t, &stubRunner{})
	if !m.mouseCaptured {
		t.Fatal("the mouse starts captured, for wheel and click support")
	}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if m.mouseCaptured {
		t.Error("ctrl+s should release the mouse")
	}
	if cmd == nil {
		t.Fatal("releasing should emit a command telling the terminal")
	}
	// The command must be the disable message, or the terminal keeps sending.
	if _, ok := cmd().(interface{}); !ok {
		t.Error("expected a message from the disable command")
	}
	if !strings.Contains(strings.Join(m.transcript, "\n"), "drag to select") {
		t.Errorf("the user should be told what changed: %v", m.transcript)
	}
	// It shows in the status bar, since behaviour has changed.
	if !strings.Contains(stripANSI(m.statusBar()), "select") {
		t.Error("released mouse should be visible in the status bar")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if !m.mouseCaptured {
		t.Error("ctrl+s again should re-capture")
	}
}

func TestCopyLastMessage(t *testing.T) {
	m := newModel(t, &stubRunner{})
	copied := captureClipboard(m)
	m.Update(deltaMsg("an older reply"))
	m.Update(doneMsg{})
	m.Update(deltaMsg("the newest reply"))
	m.Update(doneMsg{})

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
	if *copied != "the newest reply" {
		t.Errorf("copied %q, want the newest reply", *copied)
	}
	if !strings.Contains(stripANSI(strings.Join(m.transcript, "\n")), "copied the last reply") {
		t.Errorf("the copy should be confirmed: %v", m.transcript)
	}
}

func TestCopyStripsStyling(t *testing.T) {
	m := newModel(t, &stubRunner{})
	copied := captureClipboard(m)
	// The reply is glamour-rendered in the transcript, so what gets copied
	// must be the text rather than the escape sequences.
	m.Update(deltaMsg("# Heading\n\nSome **bold** text."))
	m.Update(doneMsg{})

	m.command("/copy")
	if strings.Contains(*copied, "\x1b[") {
		t.Errorf("copied text should carry no escape sequences: %q", *copied)
	}
	if !strings.Contains(*copied, "Heading") {
		t.Errorf("copied %q, want the readable text", *copied)
	}
}

func TestCopyAllTakesWholeTranscript(t *testing.T) {
	m := newModel(t, &stubRunner{})
	copied := captureClipboard(m)
	m.transcript = []string{"first", "second", "third"}

	m.command("/copy all")
	for _, want := range []string{"first", "second", "third"} {
		if !strings.Contains(*copied, want) {
			t.Errorf("copied %q, want it to include %q", *copied, want)
		}
	}
}

func TestCopyIncludesStreamingReply(t *testing.T) {
	m := newModel(t, &stubRunner{})
	copied := captureClipboard(m)
	m.Update(deltaMsg("a reply still arriving"))

	m.command("/copy")
	if *copied != "a reply still arriving" {
		t.Errorf("copied %q, want the in-flight reply", *copied)
	}
}

func TestCopyEmptyTranscriptIsReported(t *testing.T) {
	m := newModel(t, &stubRunner{})
	captureClipboard(m)
	m.command("/copy")
	if !strings.Contains(strings.Join(m.transcript, "\n"), "nothing to copy") {
		t.Errorf("an empty copy should say so: %v", m.transcript)
	}
}

// TestCopyFailureIsReported covers a headless machine with no clipboard tool.
func TestCopyFailureIsReported(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.Update(deltaMsg("something"))
	m.Update(doneMsg{})
	m.writeClipboard = func(string) error { return errors.New("no clipboard utility found") }

	m.command("/copy")
	joined := strings.Join(m.transcript, "\n")
	if !strings.Contains(joined, "could not copy") || !strings.Contains(joined, "no clipboard utility") {
		t.Errorf("the failure should be surfaced: %v", m.transcript)
	}
}

func TestScrolledIndicatorInStatusBar(t *testing.T) {
	m := newModel(t, &stubRunner{})
	fillTranscript(m, 200)
	if strings.Contains(stripANSI(m.statusBar()), "scrolled") {
		t.Error("a followed view should not advertise a scroll position")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyHome})
	if !strings.Contains(stripANSI(m.statusBar()), "scrolled") {
		t.Errorf("a scrolled view should say so:\n%s", stripANSI(m.statusBar()))
	}
}

func TestClearResumesFollowing(t *testing.T) {
	m := newModel(t, &stubRunner{})
	fillTranscript(m, 100)
	m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m.command("/clear")
	if !m.follow {
		t.Error("clearing should resume following")
	}
}

func TestHelpAdvertisesScrollAndCopy(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.command("/help")
	help := stripANSI(strings.Join(m.transcript, "\n"))
	for _, want := range []string{"/copy", "ctrl+y", "ctrl+s", "pgup", "shift+↑"} {
		if !strings.Contains(help, want) {
			t.Errorf("/help should mention %q:\n%s", want, help)
		}
	}
}

// TestCopyIgnoresUIChrome is a regression test for a real bug: copying twice
// put "copied the last reply (19 lines)" on the clipboard, because the
// confirmation note had become the last transcript entry.
func TestCopyIgnoresUIChrome(t *testing.T) {
	m := newModel(t, &stubRunner{})
	copied := captureClipboard(m)

	m.Update(deltaMsg("the actual reply"))
	m.Update(doneMsg{})

	m.command("/copy")
	if *copied != "the actual reply" {
		t.Fatalf("copied %q, want the reply", *copied)
	}

	// Copying again must still yield the reply, not the confirmation note.
	m.command("/copy")
	if *copied != "the actual reply" {
		t.Errorf("copied %q on the second attempt, want the reply again", *copied)
	}
}

// TestCopyIgnoresPromptsAndToolLines: only assistant replies are "the last
// reply"; the prompt echo and tool markers are chrome.
func TestCopyIgnoresPromptsAndToolLines(t *testing.T) {
	m := newModel(t, &stubRunner{})
	copied := captureClipboard(m)

	m.Update(deltaMsg("a real reply"))
	m.Update(doneMsg{})
	// Chrome lands after the reply.
	m.Update(toolMsg{name: "bash", finished: true})
	m.Update(blockedMsg{reason: "some refusal"})

	m.command("/copy")
	if *copied != "a real reply" {
		t.Errorf("copied %q, want the reply rather than the chrome", *copied)
	}
}

func TestCopyAllStillIncludesEverything(t *testing.T) {
	m := newModel(t, &stubRunner{})
	copied := captureClipboard(m)

	m.Update(deltaMsg("the reply"))
	m.Update(doneMsg{})
	m.Update(toolMsg{name: "bash", finished: true})

	m.command("/copy all")
	for _, want := range []string{"the reply", "bash"} {
		if !strings.Contains(*copied, want) {
			t.Errorf("copied %q, want it to include %q", *copied, want)
		}
	}
}

// TestActionFeedbackIsVisibleWhenScrolledBack is the other regression: a
// confirmation appended while scrolled up landed off-screen, so the action
// looked like it had done nothing.
func TestActionFeedbackIsVisibleWhenScrolledBack(t *testing.T) {
	m := newModel(t, &stubRunner{})
	captureClipboard(m)
	m.Update(deltaMsg("a reply"))
	m.Update(doneMsg{})
	fillTranscript(m, 200)

	m.Update(tea.KeyMsg{Type: tea.KeyHome})
	if m.follow {
		t.Fatal("precondition: should be scrolled away from the bottom")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
	if !m.follow {
		t.Error("an action's confirmation must be brought into view")
	}
	if !m.viewport.AtBottom() {
		t.Error("the view should have jumped to show the confirmation")
	}
}

// TestStreamedOutputDoesNotJumpWhileScrolledBack is the opposite case: model
// output must not drag the view around while history is being read.
func TestStreamedOutputDoesNotJumpWhileScrolledBack(t *testing.T) {
	m := newModel(t, &stubRunner{})
	fillTranscript(m, 200)
	m.Update(tea.KeyMsg{Type: tea.KeyHome})
	offset := m.viewport.YOffset

	for range 5 {
		m.Update(deltaMsg("more streamed output\n"))
	}
	m.Update(toolMsg{name: "read", finished: true})

	if m.viewport.YOffset != offset {
		t.Errorf("view moved from %d to %d while reading back", offset, m.viewport.YOffset)
	}
	if m.follow {
		t.Error("streamed output should not resume following")
	}
}

func TestClearResetsLastReply(t *testing.T) {
	m := newModel(t, &stubRunner{})
	captureClipboard(m)
	m.Update(deltaMsg("a reply"))
	m.Update(doneMsg{})

	m.command("/clear")
	m.command("/copy")
	if !strings.Contains(stripANSI(strings.Join(m.transcript, "\n")), "nothing to copy") {
		t.Errorf("after clearing there is no reply to copy: %v", m.transcript)
	}
}
