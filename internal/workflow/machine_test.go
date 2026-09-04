package workflow

import (
	"errors"
	"testing"
	"time"
)

// fakeClock advances one second per read so event timestamps are distinct and
// deterministic.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.t = c.t.Add(time.Second)
	return c.t
}

func newTask(state State) *Task {
	return &Task{ID: "t1", State: state, Receipts: map[string]Receipt{}}
}

// allStates and allActions drive the exhaustive matrix test below.
var allStates = []State{
	StatePlanning, StateImplementing, StateVerifying, StateQAReview,
	StateChangesRequested, StateReadyToComplete, StateComplete,
	StateBlocked, StateCancelled,
}

var allActions = []Action{
	ActionPlanSubmitted, ActionImplementationSubmitted, ActionGatesVerified,
	ActionGateFailed, ActionQAApproved, ActionQARejected, ActionCodeChanged,
	ActionCompleted, ActionBlocked, ActionResumed, ActionCancelled,
}

// TestTransitionMatrix asserts the entire (state, action) space against a
// golden table. Every pair not listed must be rejected, which is what makes
// the graph deny-by-default rather than merely usually-correct.
func TestTransitionMatrix(t *testing.T) {
	golden := map[State]map[Action]State{
		StatePlanning: {
			ActionPlanSubmitted: StateImplementing,
			ActionBlocked:       StateBlocked,
			ActionCancelled:     StateCancelled,
		},
		StateImplementing: {
			ActionImplementationSubmitted: StateVerifying,
			ActionBlocked:                 StateBlocked,
			ActionCancelled:               StateCancelled,
		},
		StateVerifying: {
			ActionGatesVerified: StateQAReview,
			ActionGateFailed:    StateImplementing,
			ActionBlocked:       StateBlocked,
			ActionCancelled:     StateCancelled,
		},
		StateQAReview: {
			ActionQAApproved:  StateReadyToComplete,
			ActionQARejected:  StateChangesRequested,
			ActionCodeChanged: StateVerifying,
			ActionBlocked:     StateBlocked,
			ActionCancelled:   StateCancelled,
		},
		StateChangesRequested: {
			ActionImplementationSubmitted: StateVerifying,
			ActionBlocked:                 StateBlocked,
			ActionCancelled:               StateCancelled,
		},
		StateReadyToComplete: {
			ActionCompleted:   StateComplete,
			ActionCodeChanged: StateVerifying,
			ActionBlocked:     StateBlocked,
			ActionCancelled:   StateCancelled,
		},
		// blocked+resumed is covered separately: its target depends on the
		// remembered ResumeState rather than being a fixed edge.
		StateBlocked: {
			ActionCancelled: StateCancelled,
		},
		StateComplete:  {},
		StateCancelled: {},
	}

	for _, state := range allStates {
		for _, action := range allActions {
			if state == StateBlocked && action == ActionResumed {
				continue
			}
			want, legal := golden[state][action]
			t.Run(string(state)+"/"+string(action), func(t *testing.T) {
				m := NewMachine(newFakeClock())
				task := newTask(state)
				err := m.Transition(task, action, "actor", nil)

				if !legal {
					if err == nil {
						t.Fatalf("expected %s/%s to be denied, went to %s", state, action, task.State)
					}
					if !errors.Is(err, ErrInvalidTransition) {
						t.Fatalf("want ErrInvalidTransition, got %v", err)
					}
					// A rejected transition must not partially advance.
					if task.State != state {
						t.Errorf("state mutated on rejection: %s -> %s", state, task.State)
					}
					if task.Revision != 0 || len(task.Events) != 0 {
						t.Errorf("revision/events mutated on rejection: rev=%d events=%d",
							task.Revision, len(task.Events))
					}
					return
				}
				if err != nil {
					t.Fatalf("expected %s/%s to be allowed: %v", state, action, err)
				}
				if task.State != want {
					t.Errorf("want %s, got %s", want, task.State)
				}
			})
		}
	}
}

func TestTransitionRecordsAuditEvent(t *testing.T) {
	m := NewMachine(newFakeClock())
	task := newTask(StatePlanning)

	if err := m.Transition(task, ActionPlanSubmitted, "tech-lead", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := m.Transition(task, ActionImplementationSubmitted, "engineer", nil); err != nil {
		t.Fatalf("transition: %v", err)
	}

	if len(task.Events) != 2 {
		t.Fatalf("want 2 events, got %d", len(task.Events))
	}
	first := task.Events[0]
	if first.Sequence != 1 || first.From != StatePlanning || first.To != StateImplementing {
		t.Errorf("unexpected first event: %+v", first)
	}
	if first.Actor != "tech-lead" || first.Metadata["k"] != "v" {
		t.Errorf("actor/metadata not recorded: %+v", first)
	}
	if task.Events[1].Sequence != 2 {
		t.Errorf("sequence not monotonic: %d", task.Events[1].Sequence)
	}
	if !task.Events[1].Timestamp.After(first.Timestamp) {
		t.Error("timestamps should advance")
	}
	if task.Revision != 2 {
		t.Errorf("want revision 2, got %d", task.Revision)
	}
}

// TestBlockResumeRoundTrip covers the resume path for every state a task can
// legally be blocked from.
func TestBlockResumeRoundTrip(t *testing.T) {
	blockable := []State{
		StatePlanning, StateImplementing, StateVerifying,
		StateQAReview, StateChangesRequested, StateReadyToComplete,
	}
	for _, origin := range blockable {
		t.Run(string(origin), func(t *testing.T) {
			m := NewMachine(newFakeClock())
			task := newTask(origin)

			if err := m.Transition(task, ActionBlocked, "engineer", nil); err != nil {
				t.Fatalf("block: %v", err)
			}
			if task.State != StateBlocked {
				t.Fatalf("want blocked, got %s", task.State)
			}
			if task.ResumeState != origin {
				t.Fatalf("want resume memory %s, got %s", origin, task.ResumeState)
			}

			if err := m.Transition(task, ActionResumed, "orchestrator", nil); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if task.State != origin {
				t.Errorf("want resume to %s, got %s", origin, task.State)
			}
			if task.ResumeState != "" {
				t.Errorf("resume memory should be cleared, got %s", task.ResumeState)
			}
		})
	}
}

// TestResumeWithoutMemoryFallsBack is fix #4: the plugin left a blocked task
// with no ResumeState permanently unresumable. Here it falls back to the
// table target instead of erroring.
func TestResumeWithoutMemoryFallsBack(t *testing.T) {
	m := NewMachine(newFakeClock())
	task := newTask(StateBlocked)
	task.ResumeState = ""

	if err := m.Transition(task, ActionResumed, "orchestrator", nil); err != nil {
		t.Fatalf("resume with no memory should fall back, got %v", err)
	}
	if task.State != StatePlanning {
		t.Errorf("want fallback to planning, got %s", task.State)
	}
}

func TestTerminalStates(t *testing.T) {
	m := NewMachine(newFakeClock())
	for _, s := range []State{StateComplete, StateCancelled} {
		if !m.IsTerminal(s) {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []State{StatePlanning, StateBlocked, StateReadyToComplete} {
		if m.IsTerminal(s) {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

func TestCustomTransitions(t *testing.T) {
	custom := map[State]map[Action]State{
		"research":    {ActionPlanSubmitted: StatePlanning},
		StatePlanning: {ActionCancelled: StateCancelled},
	}
	m := NewMachineWithTransitions(custom, newFakeClock())
	task := newTask("research")

	if err := m.Transition(task, ActionPlanSubmitted, "researcher", nil); err != nil {
		t.Fatalf("custom transition: %v", err)
	}
	if task.State != StatePlanning {
		t.Errorf("want planning, got %s", task.State)
	}
	// The builtin edge planning->implementing must not leak into a custom graph.
	if err := m.Transition(task, ActionPlanSubmitted, "x", nil); err == nil {
		t.Error("builtin edges should not apply to a custom graph")
	}
}

func TestFingerprintSame(t *testing.T) {
	tests := []struct {
		name string
		a, b Fingerprint
		want bool
	}{
		{"identical", Fingerprint{"h1", "d1"}, Fingerprint{"h1", "d1"}, true},
		{"digest differs", Fingerprint{"h1", "d1"}, Fingerprint{"h1", "d2"}, false},
		{"head differs", Fingerprint{"h1", "d1"}, Fingerprint{"h2", "d1"}, false},
		{"zero never matches zero", Fingerprint{}, Fingerprint{}, false},
		{"empty head", Fingerprint{"", "d1"}, Fingerprint{"", "d1"}, false},
		{"unborn head is usable", Fingerprint{"UNBORN", "d1"}, Fingerprint{"UNBORN", "d1"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Same(tc.b); got != tc.want {
				t.Errorf("Same() = %v, want %v", got, tc.want)
			}
		})
	}
}
