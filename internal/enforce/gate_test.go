package enforce

import (
	"context"
	"testing"

	"github.com/lmurphy/agentwarden/internal/workflow"
)

func TestRunGateProducesPassingReceipt(t *testing.T) {
	policy := mustPolicy(t, twoGatePolicy)
	runner := newFakeRunner()
	fp := newFakeFingerprinter()
	progress := &recordingProgress{}
	gr := NewGateRunner(runner, fp, ".", newFakeClock(), progress)

	gate, _ := policy.Gate("unit")
	receipt, err := gr.RunGate(context.Background(), gate, policy.Hash())
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if !receipt.Success || receipt.FailureReason != "" {
		t.Fatalf("want pass, got success=%v reason=%q", receipt.Success, receipt.FailureReason)
	}
	if receipt.PolicyHash != policy.Hash() {
		t.Error("receipt must be bound to the policy hash")
	}
	if !receipt.Repository.Same(fp.current) {
		t.Error("receipt must record the verified tree")
	}
	if len(progress.started) != 1 || len(progress.finished) != 1 {
		t.Errorf("progress not reported: started=%v finished=%d", progress.started, len(progress.finished))
	}
}

// TestClassifyPrecedence pins the failure-reason ordering: a moving tree
// outranks the command's own verdict, because a pass against a shifting tree
// proves nothing.
func TestClassifyPrecedence(t *testing.T) {
	before := workflow.Fingerprint{Head: "h1", Digest: "d1"}
	moved := workflow.Fingerprint{Head: "h1", Digest: "d2"}

	tests := []struct {
		name       string
		outcome    RunOutcome
		after      workflow.Fingerprint
		wantPass   bool
		wantReason string
	}{
		{"clean pass", RunOutcome{ExitCode: exitCode(0)}, before, true, ""},
		{"non-zero exit", RunOutcome{ExitCode: exitCode(1)}, before, false, ReasonCommandFailed},
		{"no exit code", RunOutcome{}, before, false, ReasonCommandFailed},
		{"timeout", RunOutcome{TimedOut: true, ExitCode: exitCode(1)}, before, false, ReasonTimedOut},
		{"start failure", RunOutcome{StartFailed: true}, before, false, ReasonCommandStartFailed},
		{"start failure outranks timeout", RunOutcome{StartFailed: true, TimedOut: true}, before, false, ReasonCommandStartFailed},
		{"moved tree outranks a pass", RunOutcome{ExitCode: exitCode(0)}, moved, false, ReasonWorktreeChanged},
		{"moved tree outranks start failure", RunOutcome{StartFailed: true}, moved, false, ReasonWorktreeChanged},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pass, reason := classify(tc.outcome, before, tc.after)
			if pass != tc.wantPass || reason != tc.wantReason {
				t.Errorf("got (%v, %q), want (%v, %q)", pass, reason, tc.wantPass, tc.wantReason)
			}
		})
	}
}

// TestGateMutatingTreeIsRejected covers a gate whose own command dirties the
// tree it was verifying.
func TestGateMutatingTreeIsRejected(t *testing.T) {
	policy := mustPolicy(t, twoGatePolicy)
	fp := newFakeFingerprinter()
	runner := newFakeRunner()
	runner.onRun = func(string) {
		fp.current = workflow.Fingerprint{Head: "h1", Digest: "dirty"}
	}
	gr := NewGateRunner(runner, fp, ".", newFakeClock(), nil)

	gate, _ := policy.Gate("unit")
	receipt, err := gr.RunGate(context.Background(), gate, policy.Hash())
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if receipt.Success || receipt.FailureReason != ReasonWorktreeChanged {
		t.Errorf("want worktree_changed, got success=%v reason=%q", receipt.Success, receipt.FailureReason)
	}
}

func TestRunGatesShortCircuitsOnRequiredFailure(t *testing.T) {
	policy := mustPolicy(t, twoGatePolicy)
	runner := newFakeRunner()
	runner.outcomes["unit"] = RunOutcome{ExitCode: exitCode(1)}
	gr := NewGateRunner(runner, newFakeFingerprinter(), ".", newFakeClock(), nil)

	set, err := gr.RunGates(context.Background(), policy.GatesFor(workflow.StateVerifying), policy.Hash())
	if err != nil {
		t.Fatalf("RunGates: %v", err)
	}
	if set.Passed {
		t.Error("batch should not pass")
	}
	if set.FirstFailure != "unit" {
		t.Errorf("FirstFailure = %q, want unit", set.FirstFailure)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "unit" {
		t.Errorf("integration should not have run, calls=%v", runner.calls)
	}
}

func TestRunGatesRunsAllOnSuccessInOrder(t *testing.T) {
	policy := mustPolicy(t, twoGatePolicy)
	runner := newFakeRunner()
	gr := NewGateRunner(runner, newFakeFingerprinter(), ".", newFakeClock(), nil)

	set, err := gr.RunGates(context.Background(), policy.GatesFor(workflow.StateVerifying), policy.Hash())
	if err != nil {
		t.Fatalf("RunGates: %v", err)
	}
	if !set.Passed {
		t.Error("batch should pass")
	}
	want := []string{"unit", "integration"}
	if len(runner.calls) != 2 || runner.calls[0] != want[0] || runner.calls[1] != want[1] {
		t.Errorf("calls = %v, want %v", runner.calls, want)
	}
}

// TestOptionalGateRunsAndDoesNotBlock is fix #1. The plugin validated
// `required: false` but filtered such gates out of every execution path,
// making them dead config.
func TestOptionalGateRunsAndDoesNotBlock(t *testing.T) {
	policy := mustPolicy(t, `
version: 1
roles: {implementer: engineer, reviewer: qa-engineer}
gates:
  - id: unit
    command: ["true"]
    required: true
  - id: lint
    command: ["true"]
    required: false
`)
	runner := newFakeRunner()
	runner.outcomes["lint"] = RunOutcome{ExitCode: exitCode(1)}
	gr := NewGateRunner(runner, newFakeFingerprinter(), ".", newFakeClock(), nil)

	set, err := gr.RunGates(context.Background(), policy.GatesFor(workflow.StateVerifying), policy.Hash())
	if err != nil {
		t.Fatalf("RunGates: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("optional gate must still execute, calls=%v", runner.calls)
	}
	if !set.Passed {
		t.Error("a failing optional gate must not block the batch")
	}
	if len(set.Receipts) != 2 || set.Receipts[1].Success {
		t.Error("the optional failure should still be reported in a receipt")
	}
}

func TestRunGatesAsyncDelivers(t *testing.T) {
	policy := mustPolicy(t, twoGatePolicy)
	gr := NewGateRunner(newFakeRunner(), newFakeFingerprinter(), ".", newFakeClock(), nil)

	result := <-gr.RunGatesAsync(context.Background(), policy.GatesFor(workflow.StateVerifying), policy.Hash())
	if result.Err != nil {
		t.Fatalf("async run: %v", result.Err)
	}
	if !result.Set.Passed || len(result.Set.Receipts) != 2 {
		t.Errorf("unexpected async result: %+v", result.Set)
	}
}

// TestRunGatesRespectsCancellation proves a long suite is interruptible
// rather than blocking for its full timeout.
func TestRunGatesRespectsCancellation(t *testing.T) {
	policy := mustPolicy(t, twoGatePolicy)
	ctx, cancel := context.WithCancel(context.Background())
	runner := newFakeRunner()
	runner.onRun = func(gateID string) {
		if gateID == "unit" {
			cancel()
		}
	}
	gr := NewGateRunner(runner, newFakeFingerprinter(), ".", newFakeClock(), nil)

	_, err := gr.RunGates(ctx, policy.GatesFor(workflow.StateVerifying), policy.Hash())
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if len(runner.calls) != 1 {
		t.Errorf("second gate should not run after cancel, calls=%v", runner.calls)
	}
}
