package enforce

import (
	"testing"

	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

// allTools is the full built-in surface, used to prove masking subtracts.
func allTools() []provider.ToolDef {
	names := []string{
		ToolRead, ToolWrite, ToolEdit, ToolBash, ToolGlob, ToolGrep, ToolLS, ToolTask,
		ToolSubmitPlan, ToolSubmitImplementation, ToolSubmitQA,
		ToolStatus, ToolHistory, ToolComplete,
	}
	out := make([]provider.ToolDef, 0, len(names))
	for _, n := range names {
		out = append(out, provider.ToolDef{Name: n})
	}
	return out
}

func maskedNames(policy *workflow.Policy, state workflow.State) map[string]bool {
	got := make(map[string]bool)
	for _, def := range MaskTools(policy, state, allTools()) {
		got[def.Name] = true
	}
	return got
}

// TestMaskingRemovesWriteToolsOutsideImplementation is the central structural
// guarantee: in a read-only state the write tools are *absent* from the
// payload, not merely denied, so there is no instruction to disobey.
func TestMaskingRemovesWriteToolsOutsideImplementation(t *testing.T) {
	policy := mustPolicy(t, twoGatePolicy)

	readOnlyStates := []workflow.State{
		workflow.StatePlanning,
		workflow.StateVerifying,
		workflow.StateReadyToComplete,
		workflow.StateBlocked,
		workflow.StateComplete,
		workflow.StateCancelled,
	}
	for _, state := range readOnlyStates {
		t.Run(string(state), func(t *testing.T) {
			got := maskedNames(policy, state)
			for _, forbidden := range []string{ToolWrite, ToolEdit} {
				if got[forbidden] {
					t.Errorf("%s must not be visible in state %s", forbidden, state)
				}
			}
			if !got[ToolRead] {
				t.Errorf("read should remain available in state %s", state)
			}
		})
	}
}

func TestMaskingPerState(t *testing.T) {
	policy := mustPolicy(t, twoGatePolicy)

	tests := []struct {
		state       workflow.State
		wantVisible []string
		wantHidden  []string
	}{
		{
			state:       workflow.StatePlanning,
			wantVisible: []string{ToolRead, ToolGrep, ToolGlob, ToolSubmitPlan, ToolStatus},
			wantHidden:  []string{ToolEdit, ToolWrite, ToolBash, ToolSubmitImplementation, ToolSubmitQA, ToolTask},
		},
		{
			state:       workflow.StateImplementing,
			wantVisible: []string{ToolEdit, ToolWrite, ToolBash, ToolSubmitImplementation},
			wantHidden:  []string{ToolSubmitPlan, ToolSubmitQA, ToolComplete},
		},
		{
			state:       workflow.StateChangesRequested,
			wantVisible: []string{ToolEdit, ToolWrite, ToolBash, ToolSubmitImplementation},
			wantHidden:  []string{ToolSubmitPlan, ToolSubmitQA},
		},
		{
			// Gates run here; the model gets nothing that could disturb the
			// tree while they execute.
			state:       workflow.StateVerifying,
			wantVisible: []string{ToolRead, ToolStatus},
			wantHidden:  []string{ToolEdit, ToolWrite, ToolBash, ToolSubmitImplementation},
		},
		{
			state:       workflow.StateQAReview,
			wantVisible: []string{ToolRead, ToolBash, ToolSubmitQA},
			wantHidden:  []string{ToolEdit, ToolWrite, ToolSubmitImplementation},
		},
		{
			state:       workflow.StateReadyToComplete,
			wantVisible: []string{ToolRead, ToolComplete},
			wantHidden:  []string{ToolEdit, ToolWrite, ToolBash},
		},
	}

	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			got := maskedNames(policy, tc.state)
			for _, name := range tc.wantVisible {
				if !got[name] {
					t.Errorf("%s should be visible in %s", name, tc.state)
				}
			}
			for _, name := range tc.wantHidden {
				if got[name] {
					t.Errorf("%s should be hidden in %s", name, tc.state)
				}
			}
		})
	}
}

// TestUnknownStateFailsClosed: adding a state must not accidentally grant
// write access.
func TestUnknownStateFailsClosed(t *testing.T) {
	policy := mustPolicy(t, twoGatePolicy)
	got := maskedNames(policy, workflow.State("brand_new_state"))
	for _, forbidden := range []string{ToolEdit, ToolWrite, ToolBash} {
		if got[forbidden] {
			t.Errorf("%s must not be granted to an unknown state", forbidden)
		}
	}
	if !got[ToolRead] {
		t.Error("read-only access should remain")
	}
}

func TestCustomStateAllowToolsOverridesDefaults(t *testing.T) {
	policy := mustPolicy(t, `
version: 1
roles: {implementer: engineer, reviewer: qa-engineer}
gates:
  - id: unit
    command: ["true"]
states:
  planning:
    allow_tools: [read, task]
    delegate_to: [tech-lead]
    on: {plan_submitted: implementing}
  implementing:
    on: {cancelled: cancelled}
`)
	got := maskedNames(policy, workflow.StatePlanning)
	if !got[ToolRead] || !got[ToolTask] {
		t.Error("declared allow_tools should be visible")
	}
	// grep is in the builtin planning set but not in this allow_tools list.
	if got[ToolGrep] {
		t.Error("allow_tools should replace the builtin set, not extend it")
	}
	// The always-visible status tools survive an override.
	if !got[ToolStatus] {
		t.Error("workflow_status should always be visible")
	}
}

// TestDelegationVisibleOnlyWhenDeclared: the task tool appears exactly when a
// state declares delegation targets.
func TestDelegationVisibleOnlyWhenDeclared(t *testing.T) {
	withDelegation := mustPolicy(t, `
version: 1
roles: {implementer: engineer, reviewer: qa-engineer}
gates:
  - id: unit
    command: ["true"]
states:
  planning:
    delegate_to: [tech-lead]
    on: {plan_submitted: implementing}
  implementing:
    on: {cancelled: cancelled}
`)
	if !maskedNames(withDelegation, workflow.StatePlanning)[ToolTask] {
		t.Error("task should be visible where delegation is declared")
	}
	if maskedNames(withDelegation, workflow.StateImplementing)[ToolTask] {
		t.Error("task should be hidden where delegation is not declared")
	}
}

func TestMaskToolsPreservesOrder(t *testing.T) {
	policy := mustPolicy(t, twoGatePolicy)
	got := MaskTools(policy, workflow.StateImplementing, allTools())

	lastIndex := -1
	for _, def := range got {
		idx := -1
		for i, all := range allTools() {
			if all.Name == def.Name {
				idx = i
				break
			}
		}
		if idx <= lastIndex {
			t.Fatalf("masking must preserve input order, %s out of place", def.Name)
		}
		lastIndex = idx
	}
}

func TestHandoffTool(t *testing.T) {
	tests := map[workflow.State]string{
		workflow.StatePlanning:         ToolSubmitPlan,
		workflow.StateImplementing:     ToolSubmitImplementation,
		workflow.StateChangesRequested: ToolSubmitImplementation,
		workflow.StateQAReview:         ToolSubmitQA,
		workflow.StateReadyToComplete:  ToolComplete,
		workflow.StateVerifying:        "",
		workflow.StateComplete:         "",
		workflow.StateBlocked:          "",
	}
	for state, want := range tests {
		if got := HandoffTool(state); got != want {
			t.Errorf("HandoffTool(%s) = %q, want %q", state, got, want)
		}
	}
}
