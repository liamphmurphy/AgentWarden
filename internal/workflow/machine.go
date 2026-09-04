package workflow

import "fmt"

// builtinTransitions is the default graph, ported from the proven plugin
// implementation. Any (state, action) pair absent from this table is denied,
// so adding a state can never accidentally widen what is reachable.
var builtinTransitions = map[State]map[Action]State{
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
	StateBlocked: {
		ActionResumed:   StatePlanning,
		ActionCancelled: StateCancelled,
	},
	// StateComplete and StateCancelled are terminal: no outbound entries.
}

// Machine evaluates transitions for a task. A Machine built from a policy with
// custom states uses that graph; otherwise it uses the builtin one.
type Machine struct {
	transitions map[State]map[Action]State
	clock       Clock
}

// NewMachine returns a Machine using the builtin transition graph.
func NewMachine(clock Clock) *Machine {
	return &Machine{transitions: builtinTransitions, clock: clock}
}

// NewMachineWithTransitions returns a Machine over a caller-supplied graph,
// used when a policy declares custom states.
func NewMachineWithTransitions(t map[State]map[Action]State, clock Clock) *Machine {
	return &Machine{transitions: t, clock: clock}
}

// IsTerminal reports whether no action can leave the state.
func (m *Machine) IsTerminal(s State) bool {
	return len(m.transitions[s]) == 0
}

// Actions lists the actions legal from a state, for banner rendering.
func (m *Machine) Actions(s State) []Action {
	out := make([]Action, 0, len(m.transitions[s]))
	for a := range m.transitions[s] {
		out = append(out, a)
	}
	return out
}

// activeState resolves the state a block should return to. Blocked and
// terminal states are not themselves resumable targets.
func activeState(s State) State {
	switch s {
	case StateBlocked, StateComplete, StateCancelled:
		return ""
	default:
		return s
	}
}

// Transition applies action to task, mutating it in place. It returns an error
// and leaves the task untouched when the pair is not permitted, so a rejected
// transition can never partially advance a task.
func (m *Machine) Transition(task *Task, action Action, actor string, metadata map[string]string) error {
	byAction, ok := m.transitions[task.State]
	if !ok || len(byAction) == 0 {
		return fmt.Errorf("%w: %s is terminal, cannot apply %s", ErrInvalidTransition, task.State, action)
	}
	target, ok := byAction[action]
	if !ok {
		return fmt.Errorf("%w: %s is not permitted from %s", ErrInvalidTransition, action, task.State)
	}

	from := task.State
	resume := task.ResumeState

	// Blocking remembers where to come back to; resuming prefers that memory
	// over the table's target. Unlike the plugin, a missing memory falls back
	// to the table target rather than making the task unresumable.
	switch action {
	case ActionBlocked:
		if prev := activeState(from); prev != "" {
			resume = prev
		}
	case ActionResumed:
		if task.ResumeState != "" {
			target = task.ResumeState
		}
		resume = ""
	}

	now := m.clock.Now()
	task.State = target
	task.ResumeState = resume
	task.Revision++
	task.UpdatedAt = now
	task.Events = append(task.Events, Event{
		Sequence:  len(task.Events) + 1,
		From:      from,
		To:        target,
		Action:    action,
		Actor:     actor,
		Timestamp: now,
		Metadata:  metadata,
	})
	return nil
}
