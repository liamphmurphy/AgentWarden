package workflow

import "sort"

// Pipeline returns the happy-path walk through a transition graph: the stages
// a task passes through when nothing goes wrong, starting at StatePlanning.
//
// This is a presentation aid, not a property of the policy. A real graph is
// not a line — a failed gate returns to implementing, a rejected review opens
// changes_requested — so there is no single "order" to read off it. What the
// interface needs is a spine to show position against, so the walk takes the
// forward edge at each stage and ignores the rest:
//
//   - the escape hatches every stage has (blocked, cancelled) are skipped,
//     since they are reachable from everywhere and belong to no position;
//   - an edge back to an already-walked stage is skipped, which is what keeps
//     the recovery loops out and guarantees termination;
//   - among what remains the alphabetically-first action wins, so the result
//     is stable across runs rather than following map order.
//
// Stages reachable only by recovery (changes_requested in the builtin graph)
// are deliberately absent. Callers must therefore handle a current state that
// is not in the returned slice.
//
// A graph with no StatePlanning returns nil: the walk has nowhere honest to
// start, and inventing one would draw a spine the policy does not have.
func Pipeline(transitions map[State]map[Action]State) []State {
	if len(transitions) == 0 {
		transitions = builtinTransitions
	}
	if _, ok := transitions[StatePlanning]; !ok {
		return nil
	}

	seen := make(map[State]bool, len(transitions))
	out := make([]State, 0, len(transitions))
	for current := StatePlanning; ; {
		out = append(out, current)
		seen[current] = true

		next, ok := forwardEdge(transitions[current], seen)
		if !ok {
			return out
		}
		current = next
	}
}

// forwardEdge picks the stage to walk to next, or reports that the walk ends
// here.
func forwardEdge(actions map[Action]State, seen map[State]bool) (State, bool) {
	names := make([]string, 0, len(actions))
	for action := range actions {
		names = append(names, string(action))
	}
	sort.Strings(names)

	for _, name := range names {
		if Action(name) == ActionBlocked || Action(name) == ActionCancelled {
			continue
		}
		target := actions[Action(name)]
		if seen[target] || target == StateBlocked || target == StateCancelled {
			continue
		}
		return target, true
	}
	return "", false
}

// PipelineIndex reports where state sits in a pipeline, or -1 when it is off
// the walked path — blocked, cancelled, or a recovery stage.
func PipelineIndex(pipeline []State, state State) int {
	for i, s := range pipeline {
		if s == state {
			return i
		}
	}
	return -1
}
