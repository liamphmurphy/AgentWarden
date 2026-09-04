package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/lmurphy/agentwarden/internal/workflow"
)

// GateState is one gate's live status in the pane.
type GateState struct {
	ID      string
	Command []string
	// Started is zero until the gate begins.
	Started time.Time
	// Receipt is nil while the gate is still running.
	Receipt *workflow.Receipt
	// LastOutput is the most recent output line, shown while running.
	LastOutput string
}

// Running reports whether the gate is in flight.
func (g GateState) Running() bool {
	return !g.Started.IsZero() && g.Receipt == nil
}

// GatePane renders live gate progress.
//
// Making enforcement visible is much of what makes it tolerable: when a gate
// blocks completion, the user can see which one and why.
type GatePane struct {
	gates []GateState
	// now is injected so the rendered elapsed time is testable.
	now func() time.Time
}

// NewGatePane returns a pane for the policy's gates in declared order.
func NewGatePane(gates []workflow.Gate) *GatePane {
	states := make([]GateState, 0, len(gates))
	for _, gate := range gates {
		states = append(states, GateState{ID: gate.ID, Command: gate.Command})
	}
	return &GatePane{gates: states, now: time.Now}
}

// Reset clears all progress, for a fresh verification run.
func (p *GatePane) Reset() {
	for i := range p.gates {
		p.gates[i].Started = time.Time{}
		p.gates[i].Receipt = nil
		p.gates[i].LastOutput = ""
	}
}

// Start marks a gate as running.
func (p *GatePane) Start(gateID string) {
	if i := p.indexOf(gateID); i >= 0 {
		p.gates[i].Started = p.now()
		p.gates[i].Receipt = nil
	}
}

// Output records the latest output line for a running gate.
func (p *GatePane) Output(gateID, line string) {
	if i := p.indexOf(gateID); i >= 0 {
		p.gates[i].LastOutput = strings.TrimSpace(line)
	}
}

// Finish records a gate's receipt.
func (p *GatePane) Finish(receipt workflow.Receipt) {
	if i := p.indexOf(receipt.GateID); i >= 0 {
		copied := receipt
		p.gates[i].Receipt = &copied
	}
}

func (p *GatePane) indexOf(gateID string) int {
	for i := range p.gates {
		if p.gates[i].ID == gateID {
			return i
		}
	}
	return -1
}

// Running reports whether any gate is in flight, which drives the spinner.
func (p *GatePane) Running() bool {
	for _, gate := range p.gates {
		if gate.Running() {
			return true
		}
	}
	return false
}

// View renders the pane. tick animates the spinner for running gates.
func (p *GatePane) View(tick int) string {
	if len(p.gates) == 0 {
		return ""
	}
	var b strings.Builder
	for i, gate := range p.gates {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.renderGate(gate, tick))
	}
	return stylePane.Render(b.String())
}

func (p *GatePane) renderGate(gate GateState, tick int) string {
	switch {
	case gate.Receipt != nil && gate.Receipt.Success:
		return fmt.Sprintf("%s %-14s %s",
			styleOK.Render(glyphPass), gate.ID,
			styleMuted.Render(formatDuration(time.Duration(gate.Receipt.DurationMS)*time.Millisecond)))

	case gate.Receipt != nil:
		reason := gate.Receipt.FailureReason
		if gate.Receipt.ExitCode != nil && reason == "" {
			reason = fmt.Sprintf("exit %d", *gate.Receipt.ExitCode)
		}
		return fmt.Sprintf("%s %-14s %s",
			styleFail.Render(glyphFail), gate.ID, styleFail.Render(reason))

	case gate.Running():
		line := fmt.Sprintf("%s %-14s %s",
			styleAccent.Render(spinnerFrame(tick)), gate.ID,
			styleMuted.Render(formatDuration(p.now().Sub(gate.Started))))
		if gate.LastOutput != "" {
			line += "\n  " + styleMuted.Render(truncate(gate.LastOutput, 48))
		}
		return line

	default:
		return fmt.Sprintf("%s %-14s %s",
			styleMuted.Render(glyphPending), gate.ID, styleMuted.Render("pending"))
	}
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
