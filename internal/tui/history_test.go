package tui

import "testing"

func TestHistoryStartsEmpty(t *testing.T) {
	h := NewHistory(0)
	if h.Len() != 0 {
		t.Errorf("Len() = %d, want 0", h.Len())
	}
	if h.Browsing() {
		t.Error("a fresh history is not browsing")
	}
	if _, ok := h.Prev("draft"); ok {
		t.Error("there is nothing to recall")
	}
	if _, ok := h.Next(); ok {
		t.Error("there is nothing to step forward to")
	}
}

// TestHistoryRecallsMostRecentFirst is the core behaviour: one press of up
// gives the last thing you sent.
func TestHistoryRecallsMostRecentFirst(t *testing.T) {
	h := NewHistory(0)
	h.Add("first")
	h.Add("second")
	h.Add("third")

	for _, want := range []string{"third", "second", "first"} {
		got, ok := h.Prev("")
		if !ok {
			t.Fatalf("expected to recall %q", want)
		}
		if got != want {
			t.Errorf("recalled %q, want %q", got, want)
		}
	}
}

// TestHistoryHoldsAtOldest: a held arrow key must not wrap around to recent
// prompts, which would look like the list jumping.
func TestHistoryHoldsAtOldest(t *testing.T) {
	h := NewHistory(0)
	h.Add("only")

	for i := range 5 {
		got, ok := h.Prev("")
		if !ok || got != "only" {
			t.Fatalf("press %d: got (%q, %v), want (only, true)", i, got, ok)
		}
	}
}

// TestHistoryPreservesDraft is the property that stops a glance back through
// history from discarding a half-written prompt.
func TestHistoryPreservesDraft(t *testing.T) {
	h := NewHistory(0)
	h.Add("earlier")
	h.Add("later")

	if got, _ := h.Prev("my unfinished thought"); got != "later" {
		t.Fatalf("recalled %q, want later", got)
	}
	if got, _ := h.Prev(""); got != "earlier" {
		t.Fatalf("recalled %q, want earlier", got)
	}

	// Forward again, back to the newest entry, then past it to the draft.
	if got, _ := h.Next(); got != "later" {
		t.Errorf("stepping forward should reach later")
	}
	got, ok := h.Next()
	if !ok {
		t.Fatal("stepping past the newest entry should restore the draft")
	}
	if got != "my unfinished thought" {
		t.Errorf("restored %q, want the draft", got)
	}
	if h.Browsing() {
		t.Error("restoring the draft ends browsing")
	}
}

// TestHistoryOnlyStashesDraftOnce: the draft is what was typed, not whatever
// entry happened to be displayed on a later press.
func TestHistoryOnlyStashesDraftOnce(t *testing.T) {
	h := NewHistory(0)
	h.Add("a")
	h.Add("b")

	h.Prev("real draft")
	h.Prev("b") // the input now holds "b"; this must not overwrite the draft

	h.Next()
	if got, _ := h.Next(); got != "real draft" {
		t.Errorf("restored %q, want the original draft", got)
	}
}

func TestHistoryNextWithoutBrowsingIsNoop(t *testing.T) {
	h := NewHistory(0)
	h.Add("a")
	if _, ok := h.Next(); ok {
		t.Error("stepping forward while showing the draft should do nothing")
	}
}

// TestHistoryAddResetsBrowsing: after submitting, up should recall the newest
// entry rather than resuming from wherever the last browse left off.
func TestHistoryAddResetsBrowsing(t *testing.T) {
	h := NewHistory(0)
	h.Add("one")
	h.Add("two")
	h.Prev("")
	h.Prev("")
	if !h.Browsing() {
		t.Fatal("precondition: should be browsing")
	}

	h.Add("three")
	if h.Browsing() {
		t.Error("adding an entry should leave browsing")
	}
	if got, _ := h.Prev(""); got != "three" {
		t.Errorf("recalled %q, want the newest entry", got)
	}
}

func TestHistoryIgnoresBlankPrompts(t *testing.T) {
	h := NewHistory(0)
	for _, blank := range []string{"", "   ", "\n", "\t\n "} {
		h.Add(blank)
	}
	if h.Len() != 0 {
		t.Errorf("blank prompts should not be retained, got %v", h.Entries())
	}
}

func TestHistoryTrimsWhitespace(t *testing.T) {
	h := NewHistory(0)
	h.Add("  padded  ")
	if got := h.Entries(); len(got) != 1 || got[0] != "padded" {
		t.Errorf("entries = %v, want [padded]", got)
	}
}

// TestHistoryCollapsesConsecutiveDuplicates: re-running the same command
// twice should not mean pressing up twice to get past it.
func TestHistoryCollapsesConsecutiveDuplicates(t *testing.T) {
	h := NewHistory(0)
	h.Add("go test ./...")
	h.Add("go test ./...")
	h.Add("go test ./...")
	if h.Len() != 1 {
		t.Errorf("entries = %v, want one", h.Entries())
	}

	// Non-consecutive repeats are kept: the order tells you what you did.
	h.Add("go build ./...")
	h.Add("go test ./...")
	if got := h.Entries(); len(got) != 3 {
		t.Errorf("entries = %v, want three", got)
	}
}

// TestHistoryEnforcesLimit stops a long session growing the list without bound.
func TestHistoryEnforcesLimit(t *testing.T) {
	h := NewHistory(3)
	for _, p := range []string{"a", "b", "c", "d", "e"} {
		h.Add(p)
	}
	got := h.Entries()
	if len(got) != 3 {
		t.Fatalf("entries = %v, want three", got)
	}
	// The oldest are dropped, not the newest.
	for i, want := range []string{"c", "d", "e"} {
		if got[i] != want {
			t.Errorf("[%d] = %q, want %q", i, got[i], want)
		}
	}
}

func TestHistoryDefaultLimit(t *testing.T) {
	for _, limit := range []int{0, -1} {
		if NewHistory(limit).limit != defaultHistoryLimit {
			t.Errorf("NewHistory(%d) should fall back to the default", limit)
		}
	}
}

func TestHistoryResetClearsDraft(t *testing.T) {
	h := NewHistory(0)
	h.Add("a")
	h.Prev("draft")
	h.Reset()

	if h.Browsing() {
		t.Error("Reset should end browsing")
	}
	if h.draft != "" {
		t.Errorf("Reset should forget the draft, got %q", h.draft)
	}
}

func TestHistoryEntriesIsACopy(t *testing.T) {
	h := NewHistory(0)
	h.Add("a")
	entries := h.Entries()
	entries[0] = "mutated"
	if h.Entries()[0] != "a" {
		t.Error("Entries should return a copy")
	}
}
