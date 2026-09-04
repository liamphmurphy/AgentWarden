package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/lmurphy/agentwarden/internal/workflow"
)

// maxOutputLines caps retained output per gate. A verbose suite would
// otherwise fill memory and push everything else off screen.
const maxOutputLines = 500

// visibleOutputLines is how many lines an expanded gate shows. The tail is
// what matters: a failure's cause is at the end, not the start.
const visibleOutputLines = 8

// GateState is one gate's live status in the pane.
type GateState struct {
	ID      string
	Command []string
	// Started is zero until the gate begins.
	Started time.Time
	// Receipt is nil while the gate is still running.
	Receipt *workflow.Receipt
	// Output holds the lines streamed so far, oldest first.
	Output []string
	// Collapsed hides this gate's detail. Gates start expanded: the point of
	// the pane is to show what the runtime did, so hiding it is opt-in.
	Collapsed bool
}

// Running reports whether the gate is in flight.
func (g GateState) Running() bool {
	return !g.Started.IsZero() && g.Receipt == nil
}

// Pending reports whether the gate has not started.
func (g GateState) Pending() bool { return g.Started.IsZero() }

// hasDetail reports whether there is anything to show when expanded.
func (g GateState) hasDetail() bool {
	return len(g.Output) > 0 || len(g.Command) > 0
}

// GatePane renders live gate progress with per-gate expandable detail.
//
// Making enforcement visible is much of what makes it tolerable: when a gate
// blocks completion, the user can see which one, what it ran, and what it
// said.
type GatePane struct {
	gates []GateState
	// now is injected so the rendered elapsed time is testable.
	now func() time.Time
	// rowGate maps a rendered content row to a gate index, or -1. It is
	// rebuilt on every View so a click can be resolved to the row under the
	// cursor.
	rowGate []int
	// selected is the keyboard cursor, or -1 when nothing is selected.
	selected int
}

// NewGatePane returns a pane for the policy's gates in declared order.
func NewGatePane(gates []workflow.Gate) *GatePane {
	states := make([]GateState, 0, len(gates))
	for _, gate := range gates {
		states = append(states, GateState{ID: gate.ID, Command: gate.Command})
	}
	return &GatePane{gates: states, now: time.Now, selected: -1}
}

// Reset clears all progress, for a fresh verification run. Expansion state is
// deliberately kept: it is a display preference, not run state.
func (p *GatePane) Reset() {
	for i := range p.gates {
		p.gates[i].Started = time.Time{}
		p.gates[i].Receipt = nil
		p.gates[i].Output = nil
	}
}

// Start marks a gate as running.
func (p *GatePane) Start(gateID string) {
	if i := p.indexOf(gateID); i >= 0 {
		p.gates[i].Started = p.now()
		p.gates[i].Receipt = nil
		p.gates[i].Output = nil
	}
}

// Output appends a streamed line to a gate.
func (p *GatePane) Output(gateID, line string) {
	i := p.indexOf(gateID)
	if i < 0 {
		return
	}
	trimmed := strings.TrimRight(line, " \t\r\n")
	if trimmed == "" {
		return
	}
	p.gates[i].Output = append(p.gates[i].Output, trimmed)
	if len(p.gates[i].Output) > maxOutputLines {
		p.gates[i].Output = p.gates[i].Output[len(p.gates[i].Output)-maxOutputLines:]
	}
}

// Finish records a gate's receipt.
func (p *GatePane) Finish(receipt workflow.Receipt) {
	i := p.indexOf(receipt.GateID)
	if i < 0 {
		return
	}
	copied := receipt
	p.gates[i].Receipt = &copied
	// A gate whose own output never streamed (a fake runner, or a command
	// that buffered) still has the receipt's captured text to show.
	if len(p.gates[i].Output) == 0 {
		p.gates[i].Output = splitNonEmpty(receipt.Stdout, receipt.Stderr)
	}
}

// splitNonEmpty turns captured output into display lines.
func splitNonEmpty(chunks ...string) []string {
	var out []string
	for _, chunk := range chunks {
		for _, line := range strings.Split(chunk, "\n") {
			if trimmed := strings.TrimRight(line, " \t\r"); trimmed != "" {
				out = append(out, trimmed)
			}
		}
	}
	if len(out) > maxOutputLines {
		out = out[len(out)-maxOutputLines:]
	}
	return out
}

func (p *GatePane) indexOf(gateID string) int {
	for i := range p.gates {
		if p.gates[i].ID == gateID {
			return i
		}
	}
	return -1
}

// Len reports how many gates the pane tracks.
func (p *GatePane) Len() int { return len(p.gates) }

// Running reports whether any gate is in flight, which drives the spinner.
func (p *GatePane) Running() bool {
	for _, gate := range p.gates {
		if gate.Running() {
			return true
		}
	}
	return false
}

// Toggle flips one gate's collapsed state.
func (p *GatePane) Toggle(index int) {
	if index < 0 || index >= len(p.gates) {
		return
	}
	p.gates[index].Collapsed = !p.gates[index].Collapsed
	p.selected = index
}

// ToggleAll collapses every gate, or expands them all if any is collapsed.
func (p *GatePane) ToggleAll() {
	anyCollapsed := false
	for _, gate := range p.gates {
		if gate.Collapsed {
			anyCollapsed = true
			break
		}
	}
	for i := range p.gates {
		p.gates[i].Collapsed = !anyCollapsed
	}
}

// Collapsed reports whether a gate is collapsed.
func (p *GatePane) Collapsed(index int) bool {
	if index < 0 || index >= len(p.gates) {
		return false
	}
	return p.gates[index].Collapsed
}

// Select moves the keyboard cursor by delta across gates.
func (p *GatePane) Select(delta int) {
	if len(p.gates) == 0 {
		return
	}
	if p.selected < 0 {
		p.selected = 0
		return
	}
	p.selected += delta
	if p.selected < 0 {
		p.selected = 0
	}
	if p.selected >= len(p.gates) {
		p.selected = len(p.gates) - 1
	}
}

// Selected returns the keyboard cursor position, or -1.
func (p *GatePane) Selected() int { return p.selected }

// ClearSelection removes the keyboard cursor.
func (p *GatePane) ClearSelection() { p.selected = -1 }

// GateAtRow resolves a row within the pane's rendered content to a gate index,
// returning -1 when the row is not a gate header.
//
// The mapping comes from the last View, which is what a click is against.
func (p *GatePane) GateAtRow(row int) int {
	if row < 0 || row >= len(p.rowGate) {
		return -1
	}
	return p.rowGate[row]
}

// View renders the pane. tick animates the spinner for running gates.
func (p *GatePane) View(tick int) string {
	p.rowGate = nil
	if len(p.gates) == 0 {
		return ""
	}

	var rows []string
	for i, gate := range p.gates {
		rows = append(rows, p.renderHeader(i, gate, tick))
		p.rowGate = append(p.rowGate, i)

		if gate.Collapsed || !gate.hasDetail() {
			continue
		}
		for _, line := range p.renderDetail(gate) {
			rows = append(rows, line)
			// Detail rows are not click targets for their own gate header,
			// but clicking them should still hit the gate they belong to.
			p.rowGate = append(p.rowGate, i)
		}
	}
	return stylePane.Render(strings.Join(rows, "\n"))
}

// renderHeader draws one gate's status line.
func (p *GatePane) renderHeader(index int, gate GateState, tick int) string {
	// The marker doubles as the affordance: it shows both state and that the
	// row can be toggled.
	marker := "▾"
	if gate.Collapsed {
		marker = "▸"
	}
	if !gate.hasDetail() {
		marker = " "
	}

	var glyph, detail string
	switch {
	case gate.Receipt != nil && gate.Receipt.Success:
		glyph = styleOK.Render(glyphPass)
		detail = styleMuted.Render(formatDuration(time.Duration(gate.Receipt.DurationMS) * time.Millisecond))
	case gate.Receipt != nil:
		reason := gate.Receipt.FailureReason
		if reason == "" && gate.Receipt.ExitCode != nil {
			reason = fmt.Sprintf("exit %d", *gate.Receipt.ExitCode)
		}
		glyph = styleFail.Render(glyphFail)
		detail = styleFail.Render(reason)
	case gate.Running():
		glyph = styleAccent.Render(spinnerFrame(tick))
		detail = styleMuted.Render(formatDuration(p.now().Sub(gate.Started)))
	default:
		glyph = styleMuted.Render(glyphPending)
		detail = styleMuted.Render("pending")
	}

	name := gate.ID
	if index == p.selected {
		name = styleAccent.Render(gate.ID)
	}
	line := fmt.Sprintf("%s %s %-14s %s", styleMuted.Render(marker), glyph, name, detail)

	// Collapsed gates say how much is hidden, so nothing disappears silently.
	if gate.Collapsed && len(gate.Output) > 0 {
		line += styleMuted.Render(fmt.Sprintf("  (%d lines hidden)", len(gate.Output)))
	}
	return line
}

// renderDetail draws the command and the tail of a gate's output.
func (p *GatePane) renderDetail(gate GateState) []string {
	var rows []string
	if len(gate.Command) > 0 {
		rows = append(rows, "   "+styleMuted.Render("$ "+strings.Join(gate.Command, " ")))
	}

	output := gate.Output
	hidden := 0
	if len(output) > visibleOutputLines {
		hidden = len(output) - visibleOutputLines
		output = output[hidden:]
	}
	if hidden > 0 {
		rows = append(rows, "   "+styleMuted.Render(fmt.Sprintf("… %d earlier lines", hidden)))
	}

	style := styleMuted
	if gate.Receipt != nil && !gate.Receipt.Success {
		// A failure's output is the thing to read, so it is not dimmed.
		style = styleFail
	}
	for _, line := range output {
		rows = append(rows, "   "+style.Render(truncate(line, 100)))
	}
	return rows
}

// formatDuration renders a duration compactly: sub-minute as seconds, longer
// as m:ss.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	return fmt.Sprintf("%d:%02d", minutes, seconds)
}

// truncate shortens s to n runes, adding an ellipsis when it cuts.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}
