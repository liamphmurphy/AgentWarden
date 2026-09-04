package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

// Layout constants for the side panel.
const (
	// statusPaneWidth is the panel's total width including border and padding.
	statusPaneWidth = 26
	// minWidthForStatusPane is the terminal width below which the panel is
	// dropped. A narrow terminal is better spent on the transcript than on a
	// column that would squeeze it to nothing.
	minWidthForStatusPane = 72
	// minStagesShown is how many stage rows must fit before the pipeline is
	// worth drawing at all.
	minStagesShown = 3
	// contextBarCells is the width of the context meter.
	contextBarCells = 12
)

// Stage is one step of the workflow spine, with the agent that owns it.
type Stage struct {
	State workflow.State
	// Agent is the agent ID the policy assigns to this stage, or "" when the
	// stage has no owner (verifying is run by the runtime, not a model).
	Agent string
}

// TokenUsage is what the panel knows about token spend.
//
// Sent and Received accumulate over the whole session, so they answer "what
// has this cost". Prompt is the newest turn's prompt size only, which is a
// different question: because every turn resends the conversation, the newest
// prompt is how full the context actually is. Summing prompts would double
// count and read as though the window had overflowed long ago.
type TokenUsage struct {
	Sent     int
	Received int
	Prompt   int
	Turns    int
	// Reported is false until an endpoint actually returns usage. Streaming
	// endpoints may omit it, and showing zeros would look like a free session
	// rather than an unmeasured one.
	Reported bool
}

// StatusPane renders the right-hand column: where the workflow is, how full
// the context is, and what the session has spent.
//
// It exists because all three are invisible until they matter — you discover a
// full context when a request fails, and the stage when a tool is refused.
type StatusPane struct {
	// Stages is the workflow spine, empty when no state machine applies.
	Stages []Stage
	// State is the current workflow state, "" when ungoverned.
	State workflow.State
	// Governed reports whether the enforcer is active. The stage section is
	// dropped when it is not, since there is no stage to be in.
	Governed bool
	// ContextWindow is the active model's window in tokens, 0 when unknown.
	ContextWindow int
	// Usage is the running token count.
	Usage TokenUsage
}

// View renders the panel to the given width and at most height rows,
// returning "" when it should not be shown.
//
// Height is a cap rather than a target: the panel is joined beside the
// transcript, and a panel taller than the viewport would push the input box
// off the bottom of the screen.
func (p *StatusPane) View(width, height int) string {
	if width < minWidthForStatusPane || height < 4 {
		return ""
	}

	// The border takes two columns and the padding two more.
	inner := statusPaneWidth - 4
	// Two border rows.
	rows := p.rows(inner, height-2)
	if len(rows) == 0 {
		return ""
	}
	// Width is the block inside the border; the padding lives within it.
	return stylePane.Width(statusPaneWidth - 2).Render(strings.Join(rows, "\n"))
}

// rows builds the panel's content lines, budgeting the sections against the
// rows available. Context and tokens are fixed-size and always shown; the
// stage list is the only section that can be trimmed, so it absorbs the
// shortfall.
func (p *StatusPane) rows(inner, budget int) []string {
	context := p.contextRows(inner)
	tokens := p.tokenRows(inner)

	// Each following section costs a blank separator row.
	fixed := len(context) + len(tokens) + 2
	var stages []string
	if remaining := budget - fixed; remaining >= minStagesShown+1 {
		stages = p.stageRows(inner, remaining-1)
	}

	sections := [][]string{}
	if len(stages) > 0 {
		sections = append(sections, stages)
	}
	sections = append(sections, context, tokens)

	var out []string
	for i, section := range sections {
		if i > 0 {
			out = append(out, "")
		}
		out = append(out, section...)
	}
	if len(out) > budget {
		out = out[:budget]
	}
	return out
}

// stageRows renders the workflow spine with the current stage marked.
func (p *StatusPane) stageRows(inner, budget int) []string {
	if !p.Governed || p.State == "" {
		return nil
	}

	rows := []string{styleAccent.Render("Stage")}
	if len(p.Stages) == 0 {
		// A governed session whose policy has no walkable spine still knows
		// which state it is in, and that is the fact worth showing.
		return append(rows, "▸ "+truncate(string(p.State), inner-2))
	}

	states := make([]workflow.State, len(p.Stages))
	for i, stage := range p.Stages {
		states[i] = stage.State
	}
	current := workflow.PipelineIndex(states, p.State)

	// The current stage names its owner on the next row, so the agent doing
	// the work is visible without widening the panel.
	owner := ""
	if current >= 0 && p.Stages[current].Agent != "" {
		owner = p.Stages[current].Agent
	}

	// Room for the header, an owner row, and an off-path row if needed.
	visible := budget - 1
	if owner != "" {
		visible--
	}
	offPath := current < 0
	if offPath {
		visible--
	}
	if visible < minStagesShown {
		return nil
	}

	from, to := stageWindow(len(p.Stages), current, visible)
	for i := from; i < to; i++ {
		rows = append(rows, p.stageRow(i, current, inner))
		if i == current && owner != "" {
			rows = append(rows, "  "+styleMuted.Render(truncate("↳ "+owner, inner-2)))
		}
	}
	if offPath {
		// blocked, cancelled, or a recovery stage: it has no place on the
		// spine, so it is named beneath rather than silently unmarked.
		rows = append(rows, styleWarn.Render(truncate("▸ "+string(p.State), inner)))
	}
	return rows
}

// stageRow renders one stage relative to the current one.
func (p *StatusPane) stageRow(index, current, inner int) string {
	name := truncate(string(p.Stages[index].State), inner-2)
	switch {
	case current >= 0 && index < current:
		return styleOK.Render(glyphPass+" ") + styleMuted.Render(name)
	case index == current:
		return styleAccent.Render("▸ " + name)
	default:
		return styleMuted.Render(glyphPending + " " + name)
	}
}

// stageWindow picks which slice of a long pipeline to show, keeping the
// current stage in view.
func stageWindow(total, current, visible int) (int, int) {
	if total <= visible {
		return 0, total
	}
	if current < 0 {
		// Off the spine: the start is the most informative anchor, since
		// there is no position to centre on.
		return 0, visible
	}
	from := current - visible/2
	if from < 0 {
		from = 0
	}
	if from+visible > total {
		from = total - visible
	}
	return from, from + visible
}

// contextRows renders how full the model's context is.
func (p *StatusPane) contextRows(inner int) []string {
	rows := []string{styleAccent.Render("Context")}

	switch {
	case !p.Usage.Reported:
		// No usage from the endpoint: say so, rather than drawing an empty
		// meter that would read as "plenty of room".
		return append(rows, styleMuted.Render("not reported"))
	case p.ContextWindow <= 0:
		// The size is a per-model config value, so an unset window is a
		// config gap rather than an unknowable fact. Name the key.
		return append(rows,
			styleMuted.Render(formatCount(p.Usage.Prompt)+" in prompt"),
			styleMuted.Render("set contextWindow"))
	}

	fraction := float64(p.Usage.Prompt) / float64(p.ContextWindow)
	percent := int(fraction * 100)
	style := contextStyle(fraction)

	label := fmt.Sprintf("%d%%", percent)
	pad := inner - len("Context") - len(label)
	if pad < 1 {
		pad = 1
	}
	rows[0] += strings.Repeat(" ", pad) + style.Render(label)
	rows = append(rows,
		style.Render(bar(fraction, contextBarCells)),
		styleMuted.Render(fmt.Sprintf("%s of %s",
			formatCount(p.Usage.Prompt), formatCount(p.ContextWindow))))
	return rows
}

// tokenRows renders cumulative spend.
func (p *StatusPane) tokenRows(inner int) []string {
	rows := []string{styleAccent.Render("Tokens")}
	if !p.Usage.Reported {
		return append(rows, styleMuted.Render("not reported"))
	}
	return append(rows,
		keyValue("sent", formatCount(p.Usage.Sent), inner),
		keyValue("recv", formatCount(p.Usage.Received), inner),
		keyValue("turns", fmt.Sprint(p.Usage.Turns), inner))
}

// keyValue renders a label left and a value right within width.
func keyValue(label, value string, width int) string {
	pad := width - len(label) - len(value)
	if pad < 1 {
		pad = 1
	}
	return styleMuted.Render(label) + strings.Repeat(" ", pad) + value
}

// contextStyle colours the meter by how close the window is to full, so a
// context about to overflow is visible without reading the number.
func contextStyle(fraction float64) lipgloss.Style {
	switch {
	case fraction >= 0.9:
		return styleFail
	case fraction >= 0.7:
		return styleWarn
	default:
		return styleOK
	}
}

// bar renders a proportional meter. A non-zero fraction always fills at least
// one cell, so a small but real usage is not drawn as empty.
func bar(fraction float64, cells int) string {
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	filled := int(fraction*float64(cells) + 0.5)
	if filled == 0 && fraction > 0 {
		filled = 1
	}
	if filled > cells {
		filled = cells
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", cells-filled)
}

// formatCount renders a token count compactly: exact below a thousand, then
// thousands, then millions. Panel width is scarce and the exact digit count of
// a 45,000-token session is never the question being asked.
func formatCount(n int) string {
	switch {
	case n < 0:
		return "0"
	case n < 1000:
		return fmt.Sprint(n)
	case n < 100_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1_000_000), ".0") + "M"
	}
}
