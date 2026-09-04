package tui

import (
	"strings"
	"testing"
)

func choices(values ...string) []Choice {
	out := make([]Choice, 0, len(values))
	for _, v := range values {
		out = append(out, Choice{Value: v, Label: "label for " + v})
	}
	return out
}

func TestPickerStartsClosed(t *testing.T) {
	p := NewPicker(8)
	if p.IsOpen() {
		t.Error("a new picker should be closed")
	}
	if p.View(80) != "" {
		t.Error("a closed picker should render nothing")
	}
	if _, ok := p.Selected(); ok {
		t.Error("a closed picker has no selection")
	}
}

// TestPickerOpensOnCurrent means the highlighted row starts where you already
// are, so confirming by accident is a no-op rather than a surprise switch.
func TestPickerOpensOnCurrent(t *testing.T) {
	p := NewPicker(8)
	p.Open("Switch model", choices("a/1", "b/2", "c/3"), "b/2")

	if !p.IsOpen() {
		t.Fatal("picker should be open")
	}
	got, ok := p.Selected()
	if !ok || got.Value != "b/2" {
		t.Errorf("selected = %+v, want b/2", got)
	}
}

func TestPickerOpensAtTopWhenCurrentAbsent(t *testing.T) {
	p := NewPicker(8)
	p.Open("t", choices("a/1", "b/2"), "not-in-list")
	if got, _ := p.Selected(); got.Value != "a/1" {
		t.Errorf("selected = %q, want the first row", got.Value)
	}
}

func TestPickerWithNoChoicesStaysClosed(t *testing.T) {
	p := NewPicker(8)
	p.Open("t", nil, "")
	if p.IsOpen() {
		t.Error("there is nothing to pick, so it should not open")
	}
}

// TestPickerClampsRatherThanWraps: a held arrow key should stop at the end
// rather than cycling past the row you were aiming for.
func TestPickerClampsRatherThanWraps(t *testing.T) {
	p := NewPicker(8)
	p.Open("t", choices("a", "b", "c"), "a")

	p.Move(-1)
	if got, _ := p.Selected(); got.Value != "a" {
		t.Errorf("moving up from the top should stay, got %q", got.Value)
	}

	p.Move(10)
	if got, _ := p.Selected(); got.Value != "c" {
		t.Errorf("moving past the end should stop at the last row, got %q", got.Value)
	}

	p.Move(-1)
	if got, _ := p.Selected(); got.Value != "b" {
		t.Errorf("selected = %q, want b", got.Value)
	}
}

func TestPickerMoveOnEmptyIsSafe(t *testing.T) {
	p := NewPicker(8)
	p.Move(1)
	p.Move(-1)
	if _, ok := p.Selected(); ok {
		t.Error("no selection expected")
	}
}

func TestPickerCloses(t *testing.T) {
	p := NewPicker(8)
	p.Open("t", choices("a", "b"), "a")
	p.Close()
	if p.IsOpen() || p.View(80) != "" {
		t.Error("a closed picker should render nothing")
	}
}

func TestPickerViewShowsValuesLabelsAndKeys(t *testing.T) {
	p := NewPicker(8)
	p.Open("Switch model", choices("ollama/qwen", "gw/sonnet"), "gw/sonnet")

	view := stripANSI(p.View(100))
	for _, want := range []string{
		"Switch model",
		"ollama/qwen", "label for ollama/qwen",
		"gw/sonnet",
		"enter confirm", "esc cancel",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view should contain %q:\n%s", want, view)
		}
	}
	// The cursor marks the current row.
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "gw/sonnet") && !strings.Contains(line, "›") {
			t.Errorf("the selected row should be marked:\n%s", view)
		}
	}
}

// TestPickerWindowsLongLists keeps a provider with many models from pushing
// the input box off screen.
func TestPickerWindowsLongLists(t *testing.T) {
	values := make([]string, 30)
	for i := range values {
		values[i] = string(rune('a'+i%26)) + "/model"
	}
	p := NewPicker(5)
	p.Open("t", choices(values...), "")

	view := stripANSI(p.View(100))
	rows := 0
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "/model") {
			rows++
		}
	}
	if rows > 5 {
		t.Errorf("window should cap at 5 rows, drew %d:\n%s", rows, view)
	}
	if !strings.Contains(view, "of 30") {
		t.Errorf("a windowed list should show its position:\n%s", view)
	}
}

// TestPickerWindowFollowsCursor: scrolling to the end must reveal the last row.
func TestPickerWindowFollowsCursor(t *testing.T) {
	p := NewPicker(3)
	p.Open("t", choices("a", "b", "c", "d", "e", "f"), "a")
	p.Move(10) // to the end

	view := stripANSI(p.View(100))
	if !strings.Contains(view, "› f") {
		t.Errorf("the cursor row should be visible:\n%s", view)
	}
}

func TestPickerDefaultsMaxRows(t *testing.T) {
	if NewPicker(0).maxRows < 1 {
		t.Error("a non-positive maxRows should fall back to a usable default")
	}
}
