package tui

import "strings"

// defaultHistoryLimit caps retained prompts. Long sessions would otherwise
// grow the list without bound, and nobody arrows back past a few dozen.
const defaultHistoryLimit = 200

// History is a shell-style prompt history.
//
// Browsing is modal: the first Prev stashes whatever is currently typed as the
// draft, and arrowing forward past the newest entry restores it. Without that,
// a glance back through history would silently discard a half-written prompt.
type History struct {
	// entries is oldest-first.
	entries []string
	// index is the browse position. It equals len(entries) when not browsing,
	// meaning "showing the draft".
	index int
	draft string
	limit int
}

// NewHistory returns an empty history. A limit of zero uses the default.
func NewHistory(limit int) *History {
	if limit <= 0 {
		limit = defaultHistoryLimit
	}
	return &History{limit: limit}
}

// Len reports how many prompts are retained.
func (h *History) Len() int { return len(h.entries) }

// Browsing reports whether the input is currently showing a recalled entry
// rather than the draft.
func (h *History) Browsing() bool { return h.index < len(h.entries) }

// Entries returns the retained prompts, oldest first.
func (h *History) Entries() []string {
	return append([]string(nil), h.entries...)
}

// Add records a submitted prompt and leaves browsing.
//
// Blank prompts are ignored, and a prompt identical to the most recent one is
// not duplicated: re-running the same command twice should not mean pressing
// up twice to get past it.
func (h *History) Add(prompt string) {
	trimmed := strings.TrimSpace(prompt)
	defer h.Reset()

	if trimmed == "" {
		return
	}
	if len(h.entries) > 0 && h.entries[len(h.entries)-1] == trimmed {
		return
	}
	h.entries = append(h.entries, trimmed)
	if len(h.entries) > h.limit {
		h.entries = h.entries[len(h.entries)-h.limit:]
	}
}

// Reset returns to the draft position and forgets the stashed draft.
func (h *History) Reset() {
	h.index = len(h.entries)
	h.draft = ""
}

// Prev steps back towards older entries, returning what should now be in the
// input. current is stashed as the draft on the first step.
//
// It reports false when there is nothing older to show, so the caller can
// leave the input untouched rather than clearing it.
func (h *History) Prev(current string) (string, bool) {
	if len(h.entries) == 0 {
		return "", false
	}
	if !h.Browsing() {
		h.draft = current
	}
	if h.index == 0 {
		// Already at the oldest entry: hold there rather than wrapping, so a
		// held arrow key does not cycle back around to recent prompts.
		return h.entries[0], true
	}
	h.index--
	return h.entries[h.index], true
}

// Next steps forward towards newer entries. Moving past the newest restores
// the draft and leaves browsing.
func (h *History) Next() (string, bool) {
	if !h.Browsing() {
		return "", false
	}
	h.index++
	if !h.Browsing() {
		draft := h.draft
		h.draft = ""
		return draft, true
	}
	return h.entries[h.index], true
}
