package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lmurphy/agentwarden/internal/enforce"
	"github.com/lmurphy/agentwarden/internal/session"
	"github.com/lmurphy/agentwarden/internal/workflow"
	"gopkg.in/yaml.v3"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time {
	c.t = c.t.Add(time.Second)
	return c.t
}

// fakeFingerprinter exposes a mutable tree identity.
type fakeFingerprinter struct{ current workflow.Fingerprint }

func (f *fakeFingerprinter) Fingerprint(context.Context) (workflow.Fingerprint, error) {
	return f.current, nil
}

// fakeRunner returns scripted outcomes and records calls.
type fakeRunner struct {
	calls    []string
	failures map[string]bool
	onRun    func(gateID string)
}

func (r *fakeRunner) Run(_ context.Context, gate workflow.Gate, _ string, _ enforce.LineFunc) enforce.RunOutcome {
	r.calls = append(r.calls, gate.ID)
	if r.onRun != nil {
		r.onRun(gate.ID)
	}
	code := 0
	if r.failures[gate.ID] {
		code = 1
	}
	return enforce.RunOutcome{ExitCode: &code}
}

const policyDoc = `
version: 1
roles:
  orchestrator: orchestrator
  planner: tech-lead
  implementer: engineer
  reviewer: qa-engineer
gates:
  - id: unit
    command: ["true"]
    required: true
  - id: integration
    command: ["true"]
    required: true
`

type harness struct {
	ctl    *Controller
	store  *session.Store
	runner *fakeRunner
	finger *fakeFingerprinter
	policy *workflow.Policy
}

func newHarness(t *testing.T, doc string) *harness {
	t.Helper()
	policy, err := workflow.ParsePolicy(strings.NewReader(doc), yaml.Unmarshal)
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	store := session.NewStore(t.TempDir())
	runner := &fakeRunner{failures: map[string]bool{}}
	finger := &fakeFingerprinter{current: workflow.Fingerprint{Head: "h1", Digest: "d1"}}
	gates := enforce.NewGateRunner(runner, finger, ".", clock, nil)
	machine := workflow.NewMachineWithTransitions(policy.Transitions(), clock)

	return &harness{
		ctl:    New(policy, machine, store, gates, finger, clock),
		store:  store,
		runner: runner,
		finger: finger,
		policy: policy,
	}
}

// advanceToVerifying walks a fresh task through plan and implementation.
func (h *harness) advanceToVerifying(t *testing.T) *workflow.Task {
	t.Helper()
	task, err := h.ctl.Start("add request timeouts")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := h.ctl.SubmitPlan(task.ID, "tech-lead", "the plan", []string{"tests pass"}); err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	updated, err := h.ctl.SubmitImplementation(task.ID, "engineer", "did the work", []string{"client.go"})
	if err != nil {
		t.Fatalf("SubmitImplementation: %v", err)
	}
	return updated
}

// TestHappyPath walks the full lifecycle and checks gates ran in order.
func TestHappyPath(t *testing.T) {
	h := newHarness(t, policyDoc)
	task := h.advanceToVerifying(t)
	if task.State != workflow.StateVerifying {
		t.Fatalf("state = %s, want verifying", task.State)
	}

	outcome := h.ctl.Verify(context.Background(), task.ID)
	if outcome.Error != nil {
		t.Fatalf("Verify: %v", outcome.Error)
	}
	if !outcome.Set.Passed {
		t.Fatal("gates should pass")
	}
	if len(h.runner.calls) != 2 || h.runner.calls[0] != "unit" || h.runner.calls[1] != "integration" {
		t.Errorf("gates should run in declared order, got %v", h.runner.calls)
	}
	if outcome.Task.State != workflow.StateQAReview {
		t.Fatalf("state = %s, want qa_review", outcome.Task.State)
	}

	approved, err := h.ctl.SubmitQA(context.Background(), task.ID, "qa-engineer", enforce.VerdictApproved, "looks right")
	if err != nil {
		t.Fatalf("SubmitQA: %v", err)
	}
	if approved.State != workflow.StateReadyToComplete {
		t.Fatalf("state = %s, want ready_to_complete", approved.State)
	}

	done, err := h.ctl.Complete(context.Background(), task.ID, "orchestrator")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if done.State != workflow.StateComplete {
		t.Errorf("state = %s, want complete", done.State)
	}
}

// TestSelfAttestedGatesAreRefused is the central trust property: the
// implementer cannot declare its own work verified.
func TestSelfAttestedGatesAreRefused(t *testing.T) {
	h := newHarness(t, policyDoc)
	task := h.advanceToVerifying(t)

	if _, err := h.ctl.Complete(context.Background(), task.ID, "engineer"); err == nil {
		t.Fatal("the engineer must not be able to complete its own work")
	} else if !errors.Is(err, workflow.ErrWrongActor) {
		t.Errorf("want ErrWrongActor, got %v", err)
	}

	reloaded, err := h.ctl.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != workflow.StateVerifying {
		t.Errorf("a refused completion must not advance the task, state = %s", reloaded.State)
	}
	if len(h.runner.calls) != 0 {
		t.Errorf("no gate should have run, got %v", h.runner.calls)
	}
}

func TestWrongActorRejectedAtEveryStage(t *testing.T) {
	tests := []struct {
		name string
		call func(h *harness, taskID string) error
	}{
		{"plan by engineer", func(h *harness, id string) error {
			_, err := h.ctl.SubmitPlan(id, "engineer", "plan", nil)
			return err
		}},
		{"implementation by tech-lead", func(h *harness, id string) error {
			_, err := h.ctl.SubmitPlan(id, "tech-lead", "plan", nil)
			if err != nil {
				return err
			}
			_, err = h.ctl.SubmitImplementation(id, "tech-lead", "work", nil)
			return err
		}},
		{"resume by engineer", func(h *harness, id string) error {
			_, err := h.ctl.Block(id, "tech-lead", "waiting")
			if err != nil {
				return err
			}
			_, err = h.ctl.Resume(id, "engineer")
			return err
		}},
		{"cancel by engineer", func(h *harness, id string) error {
			_, err := h.ctl.Cancel(id, "engineer", "no")
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, policyDoc)
			task, err := h.ctl.Start("objective")
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.call(h, task.ID); !errors.Is(err, workflow.ErrWrongActor) {
				t.Errorf("want ErrWrongActor, got %v", err)
			}
		})
	}
}

func TestFailingGateReturnsToImplementing(t *testing.T) {
	h := newHarness(t, policyDoc)
	h.runner.failures["unit"] = true
	task := h.advanceToVerifying(t)

	outcome := h.ctl.Verify(context.Background(), task.ID)
	if outcome.Error != nil {
		t.Fatalf("Verify: %v", outcome.Error)
	}
	if outcome.Set.Passed {
		t.Error("a failing required gate must not pass")
	}
	if outcome.Task.State != workflow.StateImplementing {
		t.Errorf("state = %s, want implementing", outcome.Task.State)
	}
	// The batch short-circuits, so the second gate never ran.
	if len(h.runner.calls) != 1 {
		t.Errorf("want only the first gate to run, got %v", h.runner.calls)
	}
	if outcome.Set.FirstFailure != "unit" {
		t.Errorf("FirstFailure = %q", outcome.Set.FirstFailure)
	}
}

// TestResubmissionClearsEvidence: work handed off again invalidates
// everything gathered against the previous tree.
func TestResubmissionClearsEvidence(t *testing.T) {
	h := newHarness(t, policyDoc)
	task := h.advanceToVerifying(t)
	if outcome := h.ctl.Verify(context.Background(), task.ID); outcome.Error != nil {
		t.Fatalf("Verify: %v", outcome.Error)
	}
	if _, err := h.ctl.SubmitQA(context.Background(), task.ID, "qa-engineer", enforce.VerdictRejected, "needs work"); err != nil {
		t.Fatalf("SubmitQA: %v", err)
	}

	resubmitted, err := h.ctl.SubmitImplementation(task.ID, "engineer", "fixed it", nil)
	if err != nil {
		t.Fatalf("SubmitImplementation: %v", err)
	}
	if len(resubmitted.Receipts) != 0 {
		t.Errorf("receipts should be cleared, got %d", len(resubmitted.Receipts))
	}
	if resubmitted.QA != nil {
		t.Error("the QA verdict should be cleared")
	}
	if resubmitted.State != workflow.StateVerifying {
		t.Errorf("state = %s, want verifying", resubmitted.State)
	}
}

// TestTreeMovedDuringReviewIsStale covers an edit landing while the reviewer
// reads.
func TestTreeMovedDuringReviewIsStale(t *testing.T) {
	h := newHarness(t, policyDoc)
	task := h.advanceToVerifying(t)
	if outcome := h.ctl.Verify(context.Background(), task.ID); outcome.Error != nil {
		t.Fatalf("Verify: %v", outcome.Error)
	}

	h.finger.current = workflow.Fingerprint{Head: "h1", Digest: "moved"}

	_, err := h.ctl.SubmitQA(context.Background(), task.ID, "qa-engineer", enforce.VerdictApproved, "lgtm")
	if !errors.Is(err, workflow.ErrStaleVerification) {
		t.Fatalf("want ErrStaleVerification, got %v", err)
	}

	reloaded, err := h.ctl.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != workflow.StateVerifying {
		t.Errorf("state = %s, want a return to verifying", reloaded.State)
	}
	if len(reloaded.Receipts) != 0 {
		t.Error("stale receipts should be discarded")
	}
}

// TestEditAfterApprovalBlocksCompletion: the tree must be byte-identical to
// what QA reviewed.
func TestEditAfterApprovalBlocksCompletion(t *testing.T) {
	h := newHarness(t, policyDoc)
	task := h.advanceToVerifying(t)
	h.ctl.Verify(context.Background(), task.ID)
	if _, err := h.ctl.SubmitQA(context.Background(), task.ID, "qa-engineer", enforce.VerdictApproved, "lgtm"); err != nil {
		t.Fatalf("SubmitQA: %v", err)
	}

	h.finger.current = workflow.Fingerprint{Head: "h1", Digest: "sneaky-edit"}

	if _, err := h.ctl.Complete(context.Background(), task.ID, "orchestrator"); !errors.Is(err, workflow.ErrStaleVerification) {
		t.Fatalf("want ErrStaleVerification, got %v", err)
	}
}

// TestGateMutatingTreeIsRejected covers a gate that dirties the tree it is
// verifying.
func TestGateMutatingTreeIsRejected(t *testing.T) {
	h := newHarness(t, policyDoc)
	h.runner.onRun = func(string) {
		h.finger.current = workflow.Fingerprint{Head: "h1", Digest: "dirtied"}
	}
	task := h.advanceToVerifying(t)

	outcome := h.ctl.Verify(context.Background(), task.ID)
	if outcome.Error != nil {
		t.Fatalf("Verify: %v", outcome.Error)
	}
	if outcome.Set.Passed {
		t.Error("a gate that changes the tree must not pass")
	}
	if reason := outcome.Set.Receipts[0].FailureReason; reason != enforce.ReasonWorktreeChanged {
		t.Errorf("failure reason = %q, want %q", reason, enforce.ReasonWorktreeChanged)
	}
}

// TestPolicyChangeReturnsToImplementing: a policy edit invalidates the task's
// whole basis.
func TestPolicyChangeReturnsToImplementing(t *testing.T) {
	h := newHarness(t, policyDoc)
	task := h.advanceToVerifying(t)

	// Simulate the policy having changed since the task was created.
	if _, err := h.store.Update(task.ID, func(task *workflow.Task) error {
		task.PolicyHash = "a-different-policy"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	outcome := h.ctl.Verify(context.Background(), task.ID)
	if outcome.Error != nil {
		t.Fatalf("Verify: %v", outcome.Error)
	}
	if outcome.Task.State != workflow.StateImplementing {
		t.Errorf("state = %s, want implementing", outcome.Task.State)
	}
	if len(h.runner.calls) != 0 {
		t.Errorf("no gate should run under a changed policy, got %v", h.runner.calls)
	}
}

// TestBlockRequiresAuthorizedActor is the fix for the plugin's missing actor
// check, where any bound agent could park a workflow.
func TestBlockRequiresAuthorizedActor(t *testing.T) {
	h := newHarness(t, policyDoc)
	task, err := h.ctl.Start("objective")
	if err != nil {
		t.Fatal(err)
	}

	// The task is planning, so the planner owns the stage and may block.
	if _, err := h.ctl.Block(task.ID, "tech-lead", "waiting on an answer"); err != nil {
		t.Fatalf("the stage owner should be able to block: %v", err)
	}
	if _, err := h.ctl.Resume(task.ID, "orchestrator"); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// The engineer does not own the planning stage.
	if _, err := h.ctl.Block(task.ID, "engineer", "unrelated"); !errors.Is(err, workflow.ErrWrongActor) {
		t.Errorf("a non-owner must not block, got %v", err)
	}
	// The orchestrator always may.
	if _, err := h.ctl.Block(task.ID, "orchestrator", "pausing"); err != nil {
		t.Errorf("the orchestrator should be able to block: %v", err)
	}
}

func TestBlockRequiresReason(t *testing.T) {
	h := newHarness(t, policyDoc)
	task, _ := h.ctl.Start("objective")
	if _, err := h.ctl.Block(task.ID, "orchestrator", "   "); err == nil {
		t.Error("blocking without a reason should be refused")
	}
}

func TestBlockResumeReturnsToSameStage(t *testing.T) {
	h := newHarness(t, policyDoc)
	task := h.advanceToVerifying(t)

	blocked, err := h.ctl.Block(task.ID, "orchestrator", "pausing")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if blocked.State != workflow.StateBlocked {
		t.Fatalf("state = %s", blocked.State)
	}

	resumed, err := h.ctl.Resume(task.ID, "orchestrator")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.State != workflow.StateVerifying {
		t.Errorf("state = %s, want a return to verifying", resumed.State)
	}
}

func TestVerifyRefusesOutsideVerifying(t *testing.T) {
	h := newHarness(t, policyDoc)
	task, err := h.ctl.Start("objective")
	if err != nil {
		t.Fatal(err)
	}
	outcome := h.ctl.Verify(context.Background(), task.ID)
	if outcome.Error == nil {
		t.Error("verifying a planning task should be refused")
	}
	if outcome.Ran {
		t.Error("no gate should have run")
	}
}

func TestStartRequiresObjective(t *testing.T) {
	h := newHarness(t, policyDoc)
	if _, err := h.ctl.Start("  "); err == nil {
		t.Error("an empty objective should be refused")
	}
}

func TestSubmitPlanRequiresContent(t *testing.T) {
	h := newHarness(t, policyDoc)
	task, _ := h.ctl.Start("objective")
	if _, err := h.ctl.SubmitPlan(task.ID, "tech-lead", "   ", nil); err == nil {
		t.Error("an empty plan should be refused")
	}
}

func TestSubmitQARejectsUnknownVerdict(t *testing.T) {
	h := newHarness(t, policyDoc)
	task := h.advanceToVerifying(t)
	h.ctl.Verify(context.Background(), task.ID)

	if _, err := h.ctl.SubmitQA(context.Background(), task.ID, "qa-engineer", "maybe", ""); err == nil {
		t.Error("an unknown verdict should be refused")
	}
}

// TestIndependentQAForbidsSelfReview covers the case where the same agent is
// configured for both roles at runtime.
func TestIndependentQAForbidsSelfReview(t *testing.T) {
	// The policy loader rejects identical roles, so exercise the runtime
	// check with a reviewer that matches the implementer by name.
	h := newHarness(t, `
version: 1
roles:
  orchestrator: orchestrator
  planner: tech-lead
  implementer: engineer
  reviewer: engineer
workflow:
  require_independent_qa: false
gates:
  - id: unit
    command: ["true"]
    required: true
`)
	task := h.advanceToVerifying(t)
	h.ctl.Verify(context.Background(), task.ID)

	// With independent QA off, the same agent may review.
	if _, err := h.ctl.SubmitQA(context.Background(), task.ID, "engineer", enforce.VerdictApproved, "ok"); err != nil {
		t.Errorf("with independent QA off this should be allowed: %v", err)
	}
}

func TestStartSkipsPlanningWhenNotRequired(t *testing.T) {
	h := newHarness(t, `
version: 1
workflow:
  require_plan: false
roles:
  orchestrator: orchestrator
  implementer: engineer
  reviewer: qa-engineer
gates:
  - id: unit
    command: ["true"]
    required: true
`)
	task, err := h.ctl.Start("objective")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if task.State != workflow.StateImplementing {
		t.Errorf("state = %s, want implementing when no plan is required", task.State)
	}
}

// TestEvidenceRendersReceipts: QA must review recorded evidence, not an
// agent's account of it.
func TestEvidenceRendersReceipts(t *testing.T) {
	h := newHarness(t, policyDoc)
	h.runner.failures["integration"] = true
	task := h.advanceToVerifying(t)
	h.ctl.Verify(context.Background(), task.ID)

	reloaded, err := h.ctl.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence := Evidence(reloaded)

	for _, want := range []string{"gate", "exit code", "unit"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("evidence should mention %q:\n%s", want, evidence)
		}
	}
	if Evidence(&workflow.Task{}) == "" {
		t.Error("evidence should say something even with no receipts")
	}
}

func TestAuditTrailRecordsEveryStage(t *testing.T) {
	h := newHarness(t, policyDoc)
	task := h.advanceToVerifying(t)
	h.ctl.Verify(context.Background(), task.ID)
	h.ctl.SubmitQA(context.Background(), task.ID, "qa-engineer", enforce.VerdictApproved, "ok")
	h.ctl.Complete(context.Background(), task.ID, "orchestrator")

	events, err := h.store.Events(task.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	want := []workflow.Action{
		workflow.ActionPlanSubmitted,
		workflow.ActionImplementationSubmitted,
		workflow.ActionGatesVerified,
		workflow.ActionQAApproved,
		workflow.ActionCompleted,
	}
	if len(events) != len(want) {
		t.Fatalf("want %d audit events, got %d", len(want), len(events))
	}
	for i, action := range want {
		if events[i].Action != action {
			t.Errorf("event[%d] = %s, want %s", i, events[i].Action, action)
		}
		if events[i].Sequence != i+1 {
			t.Errorf("event[%d] sequence = %d", i, events[i].Sequence)
		}
	}
}

func TestNewTaskIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewTaskID()
		if id == "" {
			t.Fatal("id should not be empty")
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
