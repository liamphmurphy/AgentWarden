package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

// testStages is the builtin spine with agents assigned, as main.go builds it.
func testStages() []Stage {
	return []Stage{
		{State: workflow.StatePlanning, Agent: "tech-lead"},
		{State: workflow.StateImplementing, Agent: "engineer"},
		{State: workflow.StateVerifying},
		{State: workflow.StateQAReview, Agent: "qa-engineer"},
		{State: workflow.StateReadyToComplete, Agent: "orchestrator"},
		{State: workflow.StateComplete},
	}
}

func newStatusPane() *StatusPane {
	return &StatusPane{
		Stages:        testStages(),
		State:         workflow.StateImplementing,
		Governed:      true,
		ContextWindow: 32_000,
		Usage: TokenUsage{
			Sent: 45_100, Received: 2_300, Prompt: 12_400, Turns: 7, Reported: true,
		},
	}
}

// paneText renders a pane and strips styling so assertions read the content
// rather than the escape sequences.
func paneText(p *StatusPane, width, height int) string {
	return plainText(p.View(width, height))
}

func TestStatusPaneShowsStageContextAndTokens(t *testing.T) {
	out := paneText(newStatusPane(), 120, 30)
	for _, want := range []string{
		"Stage", "implementing", "engineer",
		"Context", "38%", "12.4k of 32k",
		"Tokens", "sent", "45.1k", "recv", "2.3k", "turns", "7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pane is missing %q:\n%s", want, out)
		}
	}
}

// The panel must be one fixed column: a variable width would reflow the
// transcript every time a token count grew a digit.
func TestStatusPaneWidthIsFixed(t *testing.T) {
	p := newStatusPane()
	wide := p.View(200, 30)
	if got := lipglossWidth(wide); got != statusPaneWidth {
		t.Errorf("width = %d, want %d", got, statusPaneWidth)
	}

	p.Usage.Sent = 1_234_567
	p.State = workflow.StateReadyToComplete
	if got := lipglossWidth(p.View(200, 30)); got != statusPaneWidth {
		t.Errorf("width with long values = %d, want %d", got, statusPaneWidth)
	}
}

func TestStatusPaneMarksProgress(t *testing.T) {
	p := newStatusPane()
	p.State = workflow.StateQAReview
	out := paneText(p, 120, 30)

	// Stages already passed are ticked, the current one is pointed at, and
	// later ones are neither.
	for _, want := range []string{
		glyphPass + " planning",
		glyphPass + " implementing",
		"▸ qa_review",
		glyphPending + " ready_to_complete",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pane is missing %q:\n%s", want, out)
		}
	}
}

// A recovery stage is not on the spine, so it must still be named rather than
// leaving every row unmarked.
func TestStatusPaneNamesOffPathState(t *testing.T) {
	p := newStatusPane()
	p.State = workflow.StateChangesRequested
	out := paneText(p, 120, 30)
	if !strings.Contains(out, "▸ changes_requested") {
		t.Errorf("off-path state not named:\n%s", out)
	}
	if strings.Contains(out, "▸ implementing") {
		t.Errorf("an off-path state must not mark a spine row:\n%s", out)
	}
}

func TestStatusPaneHidesStageWhenUngoverned(t *testing.T) {
	p := newStatusPane()
	p.Governed = false
	p.State = ""
	out := paneText(p, 120, 30)
	if strings.Contains(out, "Stage") || strings.Contains(out, "implementing") {
		t.Errorf("ungoverned pane shows a stage:\n%s", out)
	}
	// Cost and context still apply without governance.
	if !strings.Contains(out, "Tokens") || !strings.Contains(out, "Context") {
		t.Errorf("ungoverned pane lost its counters:\n%s", out)
	}
}

// A governed policy with no walkable spine still knows its state.
func TestStatusPaneWithoutStagesShowsBareState(t *testing.T) {
	p := newStatusPane()
	p.Stages = nil
	out := paneText(p, 120, 30)
	if !strings.Contains(out, "▸ implementing") {
		t.Errorf("bare state not shown:\n%s", out)
	}
}

func TestStatusPaneUnreportedUsage(t *testing.T) {
	p := newStatusPane()
	p.Usage = TokenUsage{}
	out := paneText(p, 120, 30)
	if strings.Contains(out, "0%") {
		t.Errorf("unmeasured usage shown as 0%%:\n%s", out)
	}
	if strings.Count(out, "not reported") != 2 {
		t.Errorf("both counters should say they are unreported:\n%s", out)
	}
}

// Without a declared window there is no percentage to show, and the panel
// should name the config key rather than guess a size.
func TestStatusPaneWithoutContextWindow(t *testing.T) {
	p := newStatusPane()
	p.ContextWindow = 0
	out := paneText(p, 120, 30)
	if !strings.Contains(out, "12.4k in prompt") {
		t.Errorf("prompt size not shown:\n%s", out)
	}
	if !strings.Contains(out, "contextWindow") {
		t.Errorf("config key not named:\n%s", out)
	}
	if strings.Contains(out, "%") {
		t.Errorf("a percentage was invented from an unknown window:\n%s", out)
	}
}

func TestStatusPaneHiddenInNarrowTerminal(t *testing.T) {
	p := newStatusPane()
	if got := p.View(minWidthForStatusPane-1, 30); got != "" {
		t.Errorf("pane rendered at narrow width: %q", got)
	}
	if got := p.View(minWidthForStatusPane, 30); got == "" {
		t.Error("pane hidden at its minimum width")
	}
}

// The panel shares its row with the transcript, so it may never be taller
// than the height it is given.
func TestStatusPaneRespectsHeight(t *testing.T) {
	p := newStatusPane()
	for height := 4; height <= 30; height++ {
		out := p.View(120, height)
		if out == "" {
			continue
		}
		if got := lipglossHeight(out); got > height {
			t.Errorf("height %d rendered %d rows:\n%s", height, got, out)
		}
	}
}

// Context and cost are the sections that must survive a short terminal; the
// stage list is the one that gives way.
func TestStatusPaneKeepsCountersWhenShort(t *testing.T) {
	out := paneText(newStatusPane(), 120, 9)
	if !strings.Contains(out, "Context") || !strings.Contains(out, "Tokens") {
		t.Errorf("counters dropped in a short pane:\n%s", out)
	}
}

// A long pipeline must keep the current stage on screen, since its position
// is the whole point of the section.
func TestStatusPaneWindowsLongPipeline(t *testing.T) {
	p := newStatusPane()
	var stages []Stage
	for i := range 20 {
		stages = append(stages, Stage{State: workflow.State(string(rune('a'+i)) + "_stage")})
	}
	p.Stages = stages
	p.State = stages[17].State

	out := paneText(p, 120, 20)
	if !strings.Contains(out, "▸ "+string(stages[17].State)) {
		t.Errorf("current stage scrolled out of view:\n%s", out)
	}
}

func TestContextMeterEscalates(t *testing.T) {
	tests := []struct {
		prompt int
		want   string
	}{
		{prompt: 3_200, want: "10%"},
		{prompt: 24_000, want: "75%"},
		{prompt: 31_000, want: "96%"},
	}
	for _, tt := range tests {
		p := newStatusPane()
		p.Usage.Prompt = tt.prompt
		if out := paneText(p, 120, 30); !strings.Contains(out, tt.want) {
			t.Errorf("prompt %d: missing %q:\n%s", tt.prompt, tt.want, out)
		}
	}

	// The colour must change with pressure, or the number is the only
	// warning. Comparing the rendered string would prove nothing here, since
	// tests run without a TTY and lipgloss strips colour.
	if contextStyle(0.2).GetForeground() == contextStyle(0.95).GetForeground() {
		t.Error("a nearly full context looks the same as an empty one")
	}
}

func TestBarFillsProportionally(t *testing.T) {
	tests := []struct {
		fraction float64
		filled   int
	}{
		{0, 0},
		{0.001, 1}, // a real but tiny usage is not drawn as empty
		{0.5, 6},
		{1, 12},
		{2, 12}, // an over-full window clamps rather than overflowing the row
		{-1, 0},
	}
	for _, tt := range tests {
		got := bar(tt.fraction, contextBarCells)
		if n := strings.Count(got, "█"); n != tt.filled {
			t.Errorf("bar(%v) filled %d cells, want %d", tt.fraction, n, tt.filled)
		}
		if len([]rune(got)) != contextBarCells {
			t.Errorf("bar(%v) = %q, want %d cells", tt.fraction, got, contextBarCells)
		}
	}
}

func TestFormatCount(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1k"},
		{12_400, "12.4k"},
		{99_949, "99.9k"},
		{104_000, "104k"},
		{1_250_000, "1.2M"},
		{-5, "0"},
	}
	for _, tt := range tests {
		if got := formatCount(tt.n); got != tt.want {
			t.Errorf("formatCount(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestStageWindow(t *testing.T) {
	tests := []struct {
		name             string
		total, current   int
		visible          int
		wantFrom, wantTo int
	}{
		{"fits", 6, 3, 8, 0, 6},
		{"centres on current", 20, 10, 5, 8, 13},
		{"clamps at start", 20, 0, 5, 0, 5},
		{"clamps at end", 20, 19, 5, 15, 20},
		{"off path anchors at start", 20, -1, 5, 0, 5},
	}
	for _, tt := range tests {
		from, to := stageWindow(tt.total, tt.current, tt.visible)
		if from != tt.wantFrom || to != tt.wantTo {
			t.Errorf("%s: stageWindow(%d,%d,%d) = %d,%d, want %d,%d",
				tt.name, tt.total, tt.current, tt.visible, from, to, tt.wantFrom, tt.wantTo)
		}
	}
}

// --- integration with the model ---

func TestModelReservesColumnForStatusPane(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.resize(120, 30)
	if got := m.viewport.Width; got != 120-statusPaneWidth {
		t.Errorf("viewport width = %d, want %d", got, 120-statusPaneWidth)
	}
	if !strings.Contains(plainText(m.View()), "Tokens") {
		t.Error("status pane absent from the view")
	}

	// Hiding it must return the column to the transcript.
	m.toggleStatus()
	if got := m.viewport.Width; got != 120 {
		t.Errorf("viewport width after hiding = %d, want 120", got)
	}
	if strings.Contains(plainText(m.View()), "Tokens") {
		t.Error("status pane still rendered after being hidden")
	}
}

func TestStatusPaneToggleKey(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.resize(120, 30)
	if !m.showStatus {
		t.Fatal("the panel should start visible")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	if m.showStatus {
		t.Error("ctrl+t did not hide the panel")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	if !m.showStatus {
		t.Error("ctrl+t did not bring the panel back")
	}
}

func TestStatusPaneCommandToggles(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.resize(120, 30)
	for _, cmd := range []string{"/stats", "/status"} {
		before := m.showStatus
		if handled, _ := m.command(cmd); !handled {
			t.Fatalf("%s was not handled", cmd)
		}
		if m.showStatus == before {
			t.Errorf("%s did not toggle the panel", cmd)
		}
	}
}

// Turning the panel on in a terminal too narrow for it changes nothing on
// screen, so it must say why.
func TestStatusPaneExplainsNarrowTerminal(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.resize(minWidthForStatusPane-10, 30)
	m.toggleStatus() // off
	m.toggleStatus() // on again, but too narrow to draw
	if !strings.Contains(plainText(m.transcriptText()), "columns") {
		t.Errorf("no explanation for the missing panel:\n%s", m.transcriptText())
	}
}

func TestUsageMsgUpdatesPane(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.resize(120, 30)
	m.status.ContextWindow = 10_000

	m.Update(usageMsg{prompt: 1_000, completion: 200})
	m.Update(usageMsg{prompt: 2_500, completion: 300})

	got := m.status.Usage
	if got.Turns != 2 {
		t.Errorf("turns = %d, want 2", got.Turns)
	}
	if got.Sent != 3_500 || got.Received != 500 {
		t.Errorf("sent/recv = %d/%d, want 3500/500", got.Sent, got.Received)
	}
	// Context is the newest prompt, not the sum: the conversation is resent
	// every turn, so summing would report 35% of the window as used.
	if got.Prompt != 2_500 {
		t.Errorf("prompt = %d, want 2500", got.Prompt)
	}
	if out := plainText(m.View()); !strings.Contains(out, "25%") {
		t.Errorf("context percentage not rendered:\n%s", out)
	}
}

// An endpoint that reports no usage must leave the counters unclaimed.
func TestObserverIgnoresMissingUsage(t *testing.T) {
	m := newModel(t, &stubRunner{})
	obs := m.Observer()
	obs.TurnFinished(nil)
	select {
	case msg := <-m.events:
		t.Fatalf("a nil usage produced %T", msg)
	default:
	}

	obs.TurnFinished(&provider.Usage{PromptTokens: 11, CompletionTokens: 3})
	select {
	case msg := <-m.events:
		got, ok := msg.(usageMsg)
		if !ok {
			t.Fatalf("got %T, want usageMsg", msg)
		}
		if got.prompt != 11 || got.completion != 3 {
			t.Errorf("usage = %+v", got)
		}
	default:
		t.Fatal("usage was not reported to the UI")
	}
}

func TestStatusPaneFollowsWorkflowState(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.resize(120, 30)
	m.status.Stages = testStages()

	m.Update(stateMsg(workflow.StateQAReview))
	if m.status.State != workflow.StateQAReview {
		t.Errorf("pane state = %q, want qa_review", m.status.State)
	}
	if out := plainText(m.View()); !strings.Contains(out, "▸ qa_review") {
		t.Errorf("advanced stage not shown:\n%s", out)
	}
}

// Switching off governance must clear the stage section, since there is no
// longer a state machine in force.
func TestStatusPaneFollowsModeSwitch(t *testing.T) {
	m, _ := newSwitchableModel(t, true, true)
	m.resize(120, 30)
	m.status.Stages = testStages()

	m.setGoverned(false)
	if m.status.Governed {
		t.Error("pane still claims governance")
	}
	if out := plainText(m.View()); strings.Contains(out, "▸ planning") {
		t.Errorf("stage shown in plain mode:\n%s", out)
	}

	m.setGoverned(true)
	if !m.status.Governed || m.status.State != workflow.StatePlanning {
		t.Errorf("pane did not follow re-engagement: %+v", m.status)
	}
}

// The window belongs to the model, so switching model must re-read it.
func TestStatusPaneFollowsModelSwitch(t *testing.T) {
	m, fm := newPickableModel(t, "ollama/small", "gw/large")
	fm.windows = map[string]int{"ollama/small": 8_000, "gw/large": 200_000}
	m.status.ContextWindow = 8_000
	m.status.Usage = TokenUsage{Prompt: 4_000, Reported: true}

	if out := plainText(m.View()); !strings.Contains(out, "50%") {
		t.Fatalf("expected 50%% of the small window:\n%s", out)
	}

	m.applyModel("gw/large")
	if m.status.ContextWindow != 200_000 {
		t.Errorf("context window = %d, want 200000", m.status.ContextWindow)
	}
	if out := plainText(m.View()); !strings.Contains(out, "2%") {
		t.Errorf("expected 2%% of the large window:\n%s", out)
	}
}

// The gate pane sits below the joined transcript row, so a click must still
// land on the right gate once the panel is beside it.
func TestGateClickMappingSurvivesStatusPane(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.resize(120, 30)
	m.showGates = true
	m.gates.Start("unit")
	m.resize(120, 30)

	view := plainText(m.View())
	row := -1
	for i, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "unit") {
			row = i
			break
		}
	}
	if row < 0 {
		t.Fatalf("gate row not found:\n%s", view)
	}

	collapsed := m.gates.gates[0].Collapsed
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: row})
	if m.gates.gates[0].Collapsed == collapsed {
		t.Errorf("click on row %d did not toggle the gate", row)
	}
}
