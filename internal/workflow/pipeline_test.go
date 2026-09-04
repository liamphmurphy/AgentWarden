package workflow

import (
	"reflect"
	"testing"
)

func TestPipelineBuiltin(t *testing.T) {
	got := Pipeline(nil)
	want := []State{
		StatePlanning,
		StateImplementing,
		StateVerifying,
		StateQAReview,
		StateReadyToComplete,
		StateComplete,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Pipeline(builtin) = %v, want %v", got, want)
	}
}

// The recovery stage is reachable only by rejection, so it must not appear on
// the spine: showing it inline would claim every task passes through it.
func TestPipelineExcludesRecoveryStages(t *testing.T) {
	for _, state := range Pipeline(nil) {
		if state == StateChangesRequested {
			t.Fatalf("changes_requested is on the happy path: %v", Pipeline(nil))
		}
	}
}

func TestPipelineSkipsBlockedAndCancelled(t *testing.T) {
	for _, state := range Pipeline(nil) {
		if state == StateBlocked || state == StateCancelled {
			t.Errorf("%s is on the happy path", state)
		}
	}
}

func TestPipelineCustomGraph(t *testing.T) {
	// A policy that adds a research stage ahead of planning's successor.
	transitions := map[State]map[Action]State{
		StatePlanning: {
			ActionPlanSubmitted: "research",
			ActionCancelled:     StateCancelled,
		},
		"research": {
			ActionGatesVerified: StateImplementing,
			ActionBlocked:       StateBlocked,
		},
		StateImplementing: {
			ActionImplementationSubmitted: StateComplete,
			ActionGateFailed:              StatePlanning,
		},
		StateComplete: {},
	}
	got := Pipeline(transitions)
	want := []State{StatePlanning, "research", StateImplementing, StateComplete}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Pipeline(custom) = %v, want %v", got, want)
	}
}

// A graph that loops back must terminate rather than walking forever.
func TestPipelineTerminatesOnCycle(t *testing.T) {
	transitions := map[State]map[Action]State{
		StatePlanning:     {ActionPlanSubmitted: StateImplementing},
		StateImplementing: {ActionGateFailed: StatePlanning},
	}
	got := Pipeline(transitions)
	want := []State{StatePlanning, StateImplementing}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Pipeline(cycle) = %v, want %v", got, want)
	}
}

func TestPipelineWithoutPlanningIsEmpty(t *testing.T) {
	transitions := map[State]map[Action]State{
		"triage": {ActionPlanSubmitted: StateImplementing},
	}
	if got := Pipeline(transitions); got != nil {
		t.Errorf("Pipeline(no planning) = %v, want nil", got)
	}
}

// Two identical graphs built in different map order must walk the same way,
// or the panel would reorder itself between runs.
func TestPipelineIsDeterministic(t *testing.T) {
	first := Pipeline(nil)
	for i := range 20 {
		if got := Pipeline(nil); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d = %v, want %v", i, got, first)
		}
	}
}

func TestPipelineIndex(t *testing.T) {
	pipeline := Pipeline(nil)
	tests := []struct {
		state State
		want  int
	}{
		{StatePlanning, 0},
		{StateImplementing, 1},
		{StateComplete, len(pipeline) - 1},
		{StateChangesRequested, -1},
		{StateBlocked, -1},
		{"nonsense", -1},
	}
	for _, tt := range tests {
		if got := PipelineIndex(pipeline, tt.state); got != tt.want {
			t.Errorf("PipelineIndex(%q) = %d, want %d", tt.state, got, tt.want)
		}
	}
}

// The policy's own graph must feed Pipeline, so a project that declares custom
// states sees its own spine rather than the builtin one.
func TestPipelineFromPolicyStates(t *testing.T) {
	policy := &Policy{
		Version: 1,
		States: map[State]StateRule{
			StatePlanning:     {On: map[Action]State{ActionPlanSubmitted: StateImplementing}},
			StateImplementing: {On: map[Action]State{ActionCompleted: StateComplete}},
			StateComplete:     {},
		},
		Gates: []Gate{{ID: "unit", Command: []string{"true"}}},
	}
	got := Pipeline(policy.Transitions())
	want := []State{StatePlanning, StateImplementing, StateComplete}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Pipeline(policy) = %v, want %v", got, want)
	}
}
