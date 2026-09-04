package enforce

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

func newEnforcer(t *testing.T, doc string) (*Enforcer, *workflow.Policy) {
	t.Helper()
	policy := mustPolicy(t, doc)
	machine := workflow.NewMachineWithTransitions(policy.Transitions(), newFakeClock())
	return New(policy, machine, ".agentwarden/workflow.yml"), policy
}

func call(name, args string) provider.ToolCall {
	return provider.ToolCall{ID: "c1", Name: name, Args: args}
}

// TestInterceptBlocksMaskedTool covers the belt-and-braces case: masking
// should mean the model never sees `edit` in planning, but if it invents the
// name anyway the call is still refused.
func TestInterceptBlocksMaskedTool(t *testing.T) {
	e, policy := newEnforcer(t, twoGatePolicy)
	task := newTask(workflow.StatePlanning, policy)
	sess := &Session{Role: workflow.RolePlanner, AgentID: "tech-lead"}

	d := e.Intercept(task, sess, call(ToolEdit, `{"path":"a.go"}`))
	if d.Allow {
		t.Fatal("edit must be refused in planning")
	}
	if !strings.Contains(d.Reason, ToolEdit) {
		t.Errorf("reason should name the tool, got %q", d.Reason)
	}
	if !strings.Contains(d.Correction, ToolSubmitPlan) {
		t.Errorf("correction should name the required next action, got %q", d.Correction)
	}
	if sess.Violations != 1 {
		t.Errorf("violations = %d, want 1", sess.Violations)
	}
}

// TestEscalationLadder is the core small-model mechanism: a repeated
// violation escalates from advice, to a pinned tool_choice, to the runtime
// performing the action itself.
func TestEscalationLadder(t *testing.T) {
	e, policy := newEnforcer(t, twoGatePolicy)
	task := newTask(workflow.StatePlanning, policy)
	sess := &Session{Role: workflow.RolePlanner}

	// First violation: advice only, model retains control.
	first := e.Intercept(task, sess, call(ToolEdit, `{"path":"a.go"}`))
	if first.Allow || first.ForceTool != "" || first.AutoPerform != "" {
		t.Fatalf("first violation should only warn, got %+v", first)
	}

	// Second: pin the next call.
	second := e.Intercept(task, sess, call(ToolEdit, `{"path":"a.go"}`))
	if second.ForceTool != ToolSubmitPlan {
		t.Errorf("second violation should force %s, got %q", ToolSubmitPlan, second.ForceTool)
	}
	if second.AutoPerform != "" {
		t.Error("second violation should not yet auto-perform")
	}

	// Third: the runtime takes over.
	third := e.Intercept(task, sess, call(ToolEdit, `{"path":"a.go"}`))
	if third.AutoPerform != ToolSubmitPlan {
		t.Errorf("third violation should auto-perform %s, got %q", ToolSubmitPlan, third.AutoPerform)
	}

	// Beyond the ladder it stays at the last rung rather than panicking.
	fourth := e.Intercept(task, sess, call(ToolEdit, `{"path":"a.go"}`))
	if fourth.AutoPerform != ToolSubmitPlan {
		t.Errorf("ladder should saturate at the last rung, got %+v", fourth)
	}
}

func TestCustomEscalationLadder(t *testing.T) {
	e, policy := newEnforcer(t, `
version: 1
roles: {implementer: engineer, reviewer: qa-engineer}
gates:
  - id: unit
    command: ["true"]
states:
  planning:
    allow_tools: [read]
    on_violation: [force]
    on: {plan_submitted: implementing}
  implementing:
    on: {cancelled: cancelled}
`)
	task := newTask(workflow.StatePlanning, policy)
	sess := &Session{}

	// A single-rung ladder forces immediately rather than warning first.
	d := e.Intercept(task, sess, call(ToolEdit, `{"path":"a.go"}`))
	if d.ForceTool != ToolSubmitPlan {
		t.Errorf("a [force] ladder should force on the first violation, got %+v", d)
	}
}

// TestDelegationContractRejectsLazyCalls: the task schema is what makes a
// weak model actually decompose work instead of delegating "fix it".
func TestDelegationContractRejectsLazyCalls(t *testing.T) {
	e, policy := newEnforcer(t, `
version: 1
roles: {implementer: engineer, reviewer: qa-engineer}
gates:
  - id: unit
    command: ["true"]
states:
  planning:
    allow_tools: [read]
    delegate_to: [engineer]
    on: {plan_submitted: implementing}
  implementing:
    on: {cancelled: cancelled}
`)

	tests := []struct {
		name     string
		args     string
		wantWord string
	}{
		{"no subagent", `{"objective":"do the thing properly with detail","acceptance_criteria":["x"]}`, "name a subagent"},
		{"objective too short", `{"subagent":"engineer","objective":"fix it","acceptance_criteria":["x"]}`, "at least"},
		{"no acceptance criteria", `{"subagent":"engineer","objective":"implement request timeouts in the client"}`, "acceptance criterion"},
		{"malformed json", `{"subagent":`, "valid JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task := newTask(workflow.StatePlanning, policy)
			d := e.Intercept(task, &Session{}, call(ToolTask, tc.args))
			if d.Allow {
				t.Fatal("a lazy delegation must be refused")
			}
			if !strings.Contains(d.Reason, tc.wantWord) {
				t.Errorf("reason = %q, want it to mention %q", d.Reason, tc.wantWord)
			}
			// The refusal must show the correct shape, not just complain.
			if !strings.Contains(d.Correction, "acceptance_criteria") {
				t.Error("correction should include the argument skeleton")
			}
		})
	}
}

func TestDelegationContractAcceptsWellFormedCall(t *testing.T) {
	e, policy := newEnforcer(t, `
version: 1
roles: {implementer: engineer, reviewer: qa-engineer}
gates:
  - id: unit
    command: ["true"]
states:
  planning:
    allow_tools: [read]
    delegate_to: [engineer]
    on: {plan_submitted: implementing}
  implementing:
    on: {cancelled: cancelled}
`)
	task := newTask(workflow.StatePlanning, policy)
	args := `{"subagent":"engineer","objective":"add request timeouts to the HTTP client","acceptance_criteria":["integration suite passes"],"files_in_scope":["client.go"]}`

	if d := e.Intercept(task, &Session{}, call(ToolTask, args)); !d.Allow {
		t.Errorf("a well-formed delegation should be allowed: %+v", d)
	}
}

// TestPublicationBlockedUntilComplete covers argv parsing, which the plugin
// did with a regex over the raw string and never tested.
func TestPublicationBlockedUntilComplete(t *testing.T) {
	e, policy := newEnforcer(t, twoGatePolicy)

	blocked := []string{
		`{"command":"git push"}`,
		`{"command":"git push origin main"}`,
		`{"command":"git -c protocol.version=2 push"}`,
		`{"command":"git --no-pager push"}`,
		`{"command":"/usr/bin/git push"}`,
		`{"command":["git","-C","/tmp/repo","push"]}`,
		`{"command":"gh pr merge 42"}`,
		`{"command":"gh release create v1"}`,
	}
	for _, args := range blocked {
		t.Run(args, func(t *testing.T) {
			task := newTask(workflow.StateImplementing, policy)
			if d := e.Intercept(task, &Session{}, call(ToolBash, args)); d.Allow {
				t.Errorf("publication should be blocked: %s", args)
			}
		})
	}

	allowed := []string{
		`{"command":"git status"}`,
		`{"command":"git diff"}`,
		`{"command":"git show HEAD"}`,
		`{"command":"go test ./..."}`,
		`{"command":"git log --oneline"}`,
	}
	for _, args := range allowed {
		t.Run(args, func(t *testing.T) {
			task := newTask(workflow.StateImplementing, policy)
			if d := e.Intercept(task, &Session{}, call(ToolBash, args)); !d.Allow {
				t.Errorf("should be permitted: %s (%s)", args, d.Reason)
			}
		})
	}
}

func TestPublicationAllowedOnceComplete(t *testing.T) {
	e, policy := newEnforcer(t, twoGatePolicy)
	task := newTask(workflow.StateComplete, policy)

	// Reaching bash in the complete state requires it be visible there; the
	// point of this test is that the publication rule itself lifts.
	if IsPublishCommand([]string{"git", "push"}) != true {
		t.Fatal("precondition: git push should be recognized as publication")
	}
	d := e.Intercept(task, &Session{}, call(ToolBash, `{"command":"git push"}`))
	if !d.Allow && strings.Contains(d.Reason, "publication") {
		t.Error("the publication rule should lift once the task is complete")
	}
}

func TestPolicyEditBlocked(t *testing.T) {
	e, policy := newEnforcer(t, twoGatePolicy)
	task := newTask(workflow.StateImplementing, policy)

	for _, args := range []string{
		`{"path":".agentwarden/workflow.yml"}`,
		`{"file_path":".agentwarden/workflow.yml"}`,
		`{"path":"./.agentwarden/../.agentwarden/workflow.yml"}`,
	} {
		t.Run(args, func(t *testing.T) {
			d := e.Intercept(task, &Session{}, call(ToolEdit, args))
			if d.Allow {
				t.Error("editing the active policy must be refused")
			}
		})
	}

	if d := e.Intercept(task, &Session{}, call(ToolEdit, `{"path":"src/main.go"}`)); !d.Allow {
		t.Errorf("ordinary edits should be allowed: %s", d.Reason)
	}
}

// TestOnTurnEndDetectsMissingHandoff closes the gap that had no plugin
// equivalent: OpenCode exposed no session-end hook, so a role that stopped
// without handing off needed a human to notice.
func TestOnTurnEndDetectsMissingHandoff(t *testing.T) {
	e, policy := newEnforcer(t, twoGatePolicy)
	task := newTask(workflow.StatePlanning, policy)
	sess := &Session{Role: workflow.RolePlanner}

	d := e.OnTurnEnd(task, sess, []string{ToolRead, ToolGrep})
	if d.Allow {
		t.Fatal("ending a planning turn without submitting a plan must be caught")
	}
	if !strings.Contains(d.Correction, ToolSubmitPlan) {
		t.Errorf("correction should name the handoff, got %q", d.Correction)
	}
}

func TestOnTurnEndAcceptsHandoff(t *testing.T) {
	e, policy := newEnforcer(t, twoGatePolicy)
	task := newTask(workflow.StatePlanning, policy)

	d := e.OnTurnEnd(task, &Session{}, []string{ToolRead, ToolSubmitPlan})
	if !d.Allow {
		t.Errorf("a turn that handed off should be accepted: %+v", d)
	}
}

func TestOnTurnEndIgnoresStatesWithoutHandoff(t *testing.T) {
	e, policy := newEnforcer(t, twoGatePolicy)
	for _, state := range []workflow.State{workflow.StateVerifying, workflow.StateComplete} {
		task := newTask(state, policy)
		if d := e.OnTurnEnd(task, &Session{}, nil); !d.Allow {
			t.Errorf("state %s has no mandatory handoff, got %+v", state, d)
		}
	}
}

// TestOnCompleteRefusesUntilGatesPass is the headline guarantee.
func TestOnCompleteRefusesUntilGatesPass(t *testing.T) {
	e, policy := newEnforcer(t, twoGatePolicy)
	current := workflow.Fingerprint{Head: "h1", Digest: "d1"}
	task := newTask(workflow.StateReadyToComplete, policy)

	d := e.OnComplete(task, current)
	if d.Allow {
		t.Fatal("completion must be refused with no gate evidence")
	}
	if !strings.Contains(d.Correction, "unit") || !strings.Contains(d.Correction, "integration") {
		t.Errorf("correction should list the pending gates, got %q", d.Correction)
	}

	// Passing gates but no QA is still incomplete.
	task.Receipts["unit"] = passingReceipt("unit", policy, current)
	task.Receipts["integration"] = passingReceipt("integration", policy, current)
	if d := e.OnComplete(task, current); d.Allow {
		t.Error("completion still requires QA approval")
	}

	// With gates and a fresh approval it goes through.
	task.QA = &workflow.QA{Verdict: VerdictApproved, PolicyHash: policy.Hash(), Repository: current}
	if d := e.OnComplete(task, current); !d.Allow {
		t.Errorf("completion should be permitted: %s", d.Reason)
	}

	// An edit after approval invalidates it again.
	moved := workflow.Fingerprint{Head: "h1", Digest: "d2"}
	if d := e.OnComplete(task, moved); d.Allow {
		t.Error("an edit after QA approval must reopen verification")
	}
}

// TestToolChoicePinsForcedTool proves the mechanism the plugin could not
// reach: the next turn is constrained at the API level.
func TestToolChoicePinsForcedTool(t *testing.T) {
	e, policy := newEnforcer(t, twoGatePolicy)
	task := newTask(workflow.StatePlanning, policy)

	if tc := e.ToolChoice(task, &Session{}); tc != nil {
		t.Errorf("no constraint expected by default, got %+v", tc)
	}

	sess := &Session{ForcedTool: ToolSubmitPlan}
	tc := e.ToolChoice(task, sess)
	if tc == nil || tc.Mode != provider.ToolChoiceFunction || tc.Name != ToolSubmitPlan {
		t.Fatalf("want tool_choice pinned to %s, got %+v", ToolSubmitPlan, tc)
	}
}

// TestToolChoicePinsWhenBudgetSpent: once the direct-work budget is spent the
// handoff is the only way out of the state.
func TestToolChoicePinsWhenBudgetSpent(t *testing.T) {
	e, policy := newEnforcer(t, `
version: 1
roles: {implementer: engineer, reviewer: qa-engineer}
gates:
  - id: unit
    command: ["true"]
states:
  planning:
    allow_tools: [read, grep]
    max_direct_tool_calls: 2
    on: {plan_submitted: implementing}
  implementing:
    on: {cancelled: cancelled}
`)
	task := newTask(workflow.StatePlanning, policy)
	sess := &Session{}

	for i := 0; i < 2; i++ {
		if d := e.Intercept(task, sess, call(ToolRead, `{"path":"a.go"}`)); !d.Allow {
			t.Fatalf("read %d should be allowed: %s", i, d.Reason)
		}
		if tc := e.ToolChoice(task, sess); i == 0 && tc != nil {
			t.Error("budget should not bind before it is spent")
		}
	}

	tc := e.ToolChoice(task, sess)
	if tc == nil || tc.Name != ToolSubmitPlan {
		t.Errorf("spent budget should pin the handoff, got %+v", tc)
	}
}

// TestHandoffCallsDoNotConsumeBudget: the way out of a state must not be
// counted as direct work.
func TestHandoffCallsDoNotConsumeBudget(t *testing.T) {
	e, policy := newEnforcer(t, twoGatePolicy)
	task := newTask(workflow.StatePlanning, policy)
	sess := &Session{}

	e.Intercept(task, sess, call(ToolSubmitPlan, `{"plan":"x"}`))
	if sess.DirectToolCalls != 0 {
		t.Errorf("handoff should not count as direct work, got %d", sess.DirectToolCalls)
	}

	e.Intercept(task, sess, call(ToolRead, `{"path":"a.go"}`))
	if sess.DirectToolCalls != 1 {
		t.Errorf("read should count as direct work, got %d", sess.DirectToolCalls)
	}
}

// TestBannerStatesTheAuthoritativeContext: weak models lose instructions from
// a distant system prompt, so the banner is re-injected every turn.
func TestBannerStatesTheAuthoritativeContext(t *testing.T) {
	e, policy := newEnforcer(t, twoGatePolicy)
	task := newTask(workflow.StatePlanning, policy)
	sess := &Session{Role: workflow.RolePlanner, AgentID: "tech-lead"}
	visible := MaskTools(policy, task.State, allTools())

	banner := e.Banner(task, sess, visible)

	for _, want := range []string{
		"planning",          // the state
		"tech-lead",         // who the model is
		ToolRead,            // what it may call
		ToolSubmitPlan,      // the required next action
		"integration, unit", // what is still pending (sorted)
		"cannot be called",  // the closing constraint
	} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner should mention %q:\n%s", want, banner)
		}
	}
	// A masked tool must not be advertised.
	if strings.Contains(banner, ToolEdit) {
		t.Errorf("banner must not advertise a masked tool:\n%s", banner)
	}
}

func TestBannerReportsPassingGates(t *testing.T) {
	e, policy := newEnforcer(t, twoGatePolicy)
	current := workflow.Fingerprint{Head: "h1", Digest: "d1"}
	task := newTask(workflow.StateReadyToComplete, policy)
	task.Receipts["unit"] = passingReceipt("unit", policy, current)
	task.Receipts["integration"] = passingReceipt("integration", policy, current)

	banner := e.Banner(task, &Session{}, nil)
	if !strings.Contains(banner, "all required gates passing") {
		t.Errorf("banner should report passing gates:\n%s", banner)
	}
}

func TestArgumentSkeletonIsValidJSON(t *testing.T) {
	for _, name := range []string{ToolTask, ToolSubmitPlan, ToolSubmitImplementation, ToolSubmitQA} {
		t.Run(name, func(t *testing.T) {
			skeleton := ArgumentSkeleton(name)
			if skeleton == "" {
				t.Fatal("expected a skeleton")
			}
			var parsed map[string]any
			if err := json.Unmarshal([]byte(skeleton), &parsed); err != nil {
				t.Errorf("skeleton must be valid JSON so the model can copy it: %v", err)
			}
		})
	}
	if ArgumentSkeleton(ToolRead) != "" {
		t.Error("tools without a contract should have no skeleton")
	}
}

func TestResetStateCounters(t *testing.T) {
	sess := &Session{DirectToolCalls: 3, Violations: 2, ForcedTool: ToolTask}
	ResetStateCounters(sess)
	if sess.DirectToolCalls != 0 || sess.Violations != 0 || sess.ForcedTool != "" {
		t.Errorf("counters should reset per state, got %+v", sess)
	}
}
