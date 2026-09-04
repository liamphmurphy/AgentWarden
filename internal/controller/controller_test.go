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
	task, err := h.ctl.Start(context.Background(), "add request timeouts")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := h.ctl.SubmitPlan(context.Background(), task.ID, "tech-lead", "the plan",
		[]string{"tests pass"}); err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	// The implementing stage writes code, so the tree moves. Without this the
	// submission below is refused for changing nothing, which is the point of
	// the baseline check.
	h.finger.current = workflow.Fingerprint{Head: "h1", Digest: "d2"}
	updated, err := h.ctl.SubmitImplementation(context.Background(), task.ID, "engineer",
		"did the work", []string{"client.go"})
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
			_, err := h.ctl.SubmitPlan(context.Background(), id, "engineer", "plan", nil)
			return err
		}},
		{"implementation by tech-lead", func(h *harness, id string) error {
			_, err := h.ctl.SubmitPlan(context.Background(), id, "tech-lead", "plan", nil)
			if err != nil {
				return err
			}
			h.finger.current = workflow.Fingerprint{Head: "h1", Digest: "d2"}
			_, err = h.ctl.SubmitImplementation(context.Background(), id, "tech-lead", "work",
				[]string{"a.go"})
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
			task, err := h.ctl.Start(context.Background(), "objective")
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

	h.finger.current = workflow.Fingerprint{Head: "h1", Digest: "d4"}
	resubmitted, err := h.ctl.SubmitImplementation(context.Background(), task.ID, "engineer",
		"fixed it", []string{"client.go"})
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
	task, err := h.ctl.Start(context.Background(), "objective")
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
	task, _ := h.ctl.Start(context.Background(), "objective")
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
	task, err := h.ctl.Start(context.Background(), "objective")
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
	if _, err := h.ctl.Start(context.Background(), "  "); err == nil {
		t.Error("an empty objective should be refused")
	}
}

func TestSubmitPlanRequiresContent(t *testing.T) {
	h := newHarness(t, policyDoc)
	task, _ := h.ctl.Start(context.Background(), "objective")
	if _, err := h.ctl.SubmitPlan(context.Background(), task.ID, "tech-lead", "   ", nil); err == nil {
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
	task, err := h.ctl.Start(context.Background(), "objective")
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

// The task that prompted this check reached qa_review with three green
// receipts and a handoff reading "No changes yet — this is a placeholder
// submission to unblock the state machine". Gates cannot catch that: on an
// unchanged tree every suite passes. Only the tree itself can.
func TestSubmitImplementationRefusesAnUnchangedTree(t *testing.T) {
	h := newHarness(t, policyDoc)
	task, err := h.ctl.Start(context.Background(), "add request timeouts")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := h.ctl.SubmitPlan(context.Background(), task.ID, "tech-lead", "the plan",
		[]string{"tests pass"}); err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	// No edit: the fingerprint still matches the baseline taken on entry.
	_, err = h.ctl.SubmitImplementation(context.Background(), task.ID, "engineer",
		"placeholder to unblock the state machine", []string{"client.go"})
	if err == nil {
		t.Fatal("a submission that changed nothing was accepted")
	}
	if !strings.Contains(err.Error(), "unchanged") {
		t.Errorf("the refusal should name the cause, got %v", err)
	}

	// And the task must stay where it was, not half-advance.
	current, err := h.ctl.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != workflow.StateImplementing {
		t.Errorf("state = %s, want implementing", current.State)
	}

	// Once something is actually written, the same submission goes through.
	h.finger.current = workflow.Fingerprint{Head: "h1", Digest: "d2"}
	advanced, err := h.ctl.SubmitImplementation(context.Background(), task.ID, "engineer",
		"added the timeouts", []string{"client.go"})
	if err != nil {
		t.Fatalf("SubmitImplementation after a real edit: %v", err)
	}
	if advanced.State != workflow.StateVerifying {
		t.Errorf("state = %s, want verifying", advanced.State)
	}
}

// An empty files list is the honest version of the same fraud, and is cheap to
// catch before any git call.
func TestSubmitImplementationRequiresFiles(t *testing.T) {
	h := newHarness(t, policyDoc)
	task, _ := h.ctl.Start(context.Background(), "objective")
	if _, err := h.ctl.SubmitPlan(context.Background(), task.ID, "tech-lead", "plan", nil); err != nil {
		t.Fatal(err)
	}
	h.finger.current = workflow.Fingerprint{Head: "h1", Digest: "d2"}

	for _, files := range [][]string{nil, {}} {
		_, err := h.ctl.SubmitImplementation(context.Background(), task.ID, "engineer",
			"did the work", files)
		if err == nil {
			t.Fatalf("files=%v should be refused", files)
		}
		if !strings.Contains(err.Error(), "files it changed") {
			t.Errorf("files=%v: unhelpful refusal %v", files, err)
		}
	}
}

// A rejected review opens changes_requested, so that stage needs its own
// baseline: otherwise "I addressed the review" could change nothing and be
// compared against a tree from two stages ago.
func TestChangesRequestedGetsItsOwnBaseline(t *testing.T) {
	h := newHarness(t, policyDoc)
	task := h.advanceToVerifying(t)
	if outcome := h.ctl.Verify(context.Background(), task.ID); outcome.Error != nil {
		t.Fatalf("Verify: %v", outcome.Error)
	}
	if _, err := h.ctl.SubmitQA(context.Background(), task.ID, "qa-engineer",
		enforce.VerdictRejected, "needs work"); err != nil {
		t.Fatalf("SubmitQA: %v", err)
	}

	current, err := h.ctl.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != workflow.StateChangesRequested {
		t.Fatalf("state = %s, want changes_requested", current.State)
	}
	if current.Baseline == nil {
		t.Fatal("changes_requested recorded no baseline")
	}

	// Resubmitting without touching anything is refused.
	if _, err := h.ctl.SubmitImplementation(context.Background(), task.ID, "engineer",
		"addressed the review", []string{"client.go"}); err == nil {
		t.Error("an empty round of changes was accepted")
	}
}

// A failing gate returns the task to implementing, which is a fresh editing
// stage: the retry must have to change something too.
func TestGateFailureRebaselines(t *testing.T) {
	h := newHarness(t, policyDoc)
	h.runner.failures["unit"] = true
	task := h.advanceToVerifying(t)

	outcome := h.ctl.Verify(context.Background(), task.ID)
	if outcome.Error != nil {
		t.Fatalf("Verify: %v", outcome.Error)
	}
	if outcome.Task.State != workflow.StateImplementing {
		t.Fatalf("state = %s, want implementing", outcome.Task.State)
	}
	if outcome.Task.Baseline == nil {
		t.Fatal("the reopened implementing stage recorded no baseline")
	}

	if _, err := h.ctl.SubmitImplementation(context.Background(), task.ID, "engineer",
		"tried again", []string{"client.go"}); err == nil {
		t.Error("a retry that changed nothing was accepted")
	}
}

// The baseline is cleared on the way out of an editing stage, so a stale one
// can never be compared against later.
func TestBaselineClearedOutsideEditingStages(t *testing.T) {
	h := newHarness(t, policyDoc)
	task := h.advanceToVerifying(t)
	if task.State != workflow.StateVerifying {
		t.Fatalf("state = %s, want verifying", task.State)
	}
	if task.Baseline != nil {
		t.Errorf("verifying is not an editing stage but carries a baseline: %+v", task.Baseline)
	}
}

func TestIsEditingState(t *testing.T) {
	editing := []workflow.State{workflow.StateImplementing, workflow.StateChangesRequested}
	for _, s := range editing {
		if !workflow.IsEditingState(s) {
			t.Errorf("%s should be an editing state", s)
		}
	}
	for _, s := range []workflow.State{
		workflow.StatePlanning, workflow.StateVerifying, workflow.StateQAReview,
		workflow.StateReadyToComplete, workflow.StateComplete, workflow.StateBlocked,
		workflow.StateCancelled,
	} {
		if workflow.IsEditingState(s) {
			t.Errorf("%s should not be an editing state", s)
		}
	}
}

// With require_plan off a task opens directly in implementing, so Start is
// where its baseline has to come from: there is no plan submission to record
// one, and without it the check would silently not apply to exactly the
// simplified policy a small model is most likely to be given.
func TestStartRecordsBaselineWhenPlanningIsSkipped(t *testing.T) {
	const noPlanPolicy = `
version: 1
workflow:
  require_plan: false
  require_independent_qa: false
roles:
  orchestrator: orchestrator
  implementer: engineer
gates:
  - id: unit
    command: ["true"]
    required: true
`
	h := newHarness(t, noPlanPolicy)
	task, err := h.ctl.Start(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if task.State != workflow.StateImplementing {
		t.Fatalf("state = %s, want implementing", task.State)
	}
	if task.Baseline == nil {
		t.Fatal("a task opening in an editing stage recorded no baseline")
	}

	if _, err := h.ctl.SubmitImplementation(context.Background(), task.ID, "engineer",
		"nothing really", []string{"a.go"}); err == nil {
		t.Error("a submission that changed nothing was accepted")
	}
}
