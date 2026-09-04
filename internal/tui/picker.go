package tui

import (
	"fmt"
	"strings"
)

// Choice is one selectable row.
type Choice struct {
	// Value is what gets returned on selection.
	Value string
	// Label is a human-readable description shown beside the value.
	Label string
}

// Picker is a small single-select list.
//
// It is hand-rolled rather than using bubbles/list because it needs to be a
// few lines tall, non-scrolling in the common case, and driven entirely by
// keys the main model already intercepts.
type Picker struct {
	title   string
	choices []Choice
	cursor  int
	open    bool
	// maxRows bounds the visible window so a provider with many models cannot
	// push the input box off screen.
	maxRows int
}

// NewPicker returns a closed picker.
func NewPicker(maxRows int) *Picker {
	if maxRows < 1 {
		maxRows = 8
	}
	return &Picker{maxRows: maxRows}
}

// Open shows the picker, positioning the cursor on current if it is present.
func (p *Picker) Open(title string, choices []Choice, current string) {
	p.title = title
	p.choices = choices
	p.open = len(choices) > 0
	p.cursor = 0
	for i, c := range choices {
		if c.Value == current {
			p.cursor = i
			break
		}
	}
}

// Close hides the picker.
func (p *Picker) Close() { p.open = false }

// IsOpen reports whether the picker is showing.
func (p *Picker) IsOpen() bool { return p.open }

// Len reports how many choices are listed.
func (p *Picker) Len() int { return len(p.choices) }

// Move shifts the cursor by delta, clamping at both ends. Clamping rather than
// wrapping keeps held arrow keys from cycling past the intended row.
func (p *Picker) Move(delta int) {
	if len(p.choices) == 0 {
		return
	}
	p.cursor += delta
	if p.cursor < 0 {
		p.cursor = 0
	}
	if p.cursor >= len(p.choices) {
		p.cursor = len(p.choices) - 1
	}
}

// Selected returns the highlighted choice.
func (p *Picker) Selected() (Choice, bool) {
	if !p.open || p.cursor < 0 || p.cursor >= len(p.choices) {
		return Choice{}, false
	}
	return p.choices[p.cursor], true
}

// window returns the slice of choices to draw and the index of the first one,
// keeping the cursor inside the visible rows.
func (p *Picker) window() ([]Choice, int) {
	if len(p.choices) <= p.maxRows {
		return p.choices, 0
	}
	start := p.cursor - p.maxRows/2
	if start < 0 {
		start = 0
	}
	if start > len(p.choices)-p.maxRows {
		start = len(p.choices) - p.maxRows
	}
	return p.choices[start : start+p.maxRows], start
}

// View renders the picker, or "" when closed.
func (p *Picker) View(width int) string {
	if !p.open {
		return ""
	}

	var b strings.Builder
	b.WriteString(styleAccent.Render(p.title))
	b.WriteString(styleMuted.Render("   ↑↓ select · enter confirm · esc cancel"))

	visible, offset := p.window()
	for i, choice := range visible {
		b.WriteString("\n")
		marker, row := "  ", styleMuted
		if offset+i == p.cursor {
			marker, row = "› ", styleAccent
		}
		line := marker + choice.Value
		if choice.Label != "" && choice.Label != choice.Value {
			line += "  " + choice.Label
		}
		b.WriteString(row.Render(truncate(line, max(width-6, 20))))
	}

	if len(p.choices) > len(visible) {
		b.WriteString("\n")
		b.WriteString(styleMuted.Render(fmt.Sprintf("  %d of %d", p.cursor+1, len(p.choices))))
	}
	return stylePane.Render(b.String())
}
