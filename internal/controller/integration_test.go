package controller

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lmurphy/agentwarden/internal/enforce"
	"github.com/lmurphy/agentwarden/internal/session"
	"github.com/lmurphy/agentwarden/internal/workflow"
	"gopkg.in/yaml.v3"
)

// realRepo scaffolds a git repository containing a Go package whose test
// fails, mirroring the manual verification scenario.
func realRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module demo\n\ngo 1.25\n")
	// Deliberately wrong, so the gate fails on the first run.
	write("add.go", "package demo\n\nfunc Add(a, b int) int { return a - b }\n")
	write("add_test.go", `package demo

import "testing"

func TestAdd(t *testing.T) {
	if got := Add(2, 3); got != 5 {
		t.Fatalf("Add(2,3) = %d, want 5", got)
	}
}
`)

	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
		{"add", "-A"},
		{"commit", "-qm", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

const realPolicy = `
version: 1
roles:
  orchestrator: orchestrator
  planner: tech-lead
  implementer: engineer
  reviewer: qa-engineer
gates:
  - id: unit
    command: ["go", "test", "./..."]
    required: true
    timeout_seconds: 120
`

// TestRealGateBlocksThenPasses is the end-to-end proof of the feature this
// project exists for: a required gate is executed by the runtime against a
// real repository, completion is refused while it fails, and permitted only
// once it genuinely passes.
func TestRealGateBlocksThenPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real go test invocation")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}

	dir := realRepo(t)
	policy, err := workflow.ParsePolicy(strings.NewReader(realPolicy), yaml.Unmarshal)
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}

	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	store := session.NewStore(filepath.Join(dir, ".agentwarden", "state"))
	finger := enforce.NewGitFingerprinter(dir)
	gates := enforce.NewGateRunner(enforce.ExecRunner{}, finger, dir, clock, nil)
	machine := workflow.NewMachineWithTransitions(policy.Transitions(), clock)
	ctl := New(policy, machine, store, gates, finger, clock)

	ctx := context.Background()

	task, err := ctl.Start("fix the Add function")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := ctl.SubmitPlan(task.ID, "tech-lead", "correct the operator", []string{"go test passes"}); err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if _, err := ctl.SubmitImplementation(task.ID, "engineer", "claimed to fix it", []string{"add.go"}); err != nil {
		t.Fatalf("SubmitImplementation: %v", err)
	}

	// The engineer claimed a fix but changed nothing, so the real gate fails.
	outcome := ctl.Verify(ctx, task.ID)
	if outcome.Error != nil {
		t.Fatalf("Verify: %v", outcome.Error)
	}
	if outcome.Set.Passed {
		t.Fatal("the gate must fail: the bug is still present")
	}
	if outcome.Task.State != workflow.StateImplementing {
		t.Errorf("state = %s, want a return to implementing", outcome.Task.State)
	}

	receipt := outcome.Set.Receipts[0]
	if receipt.Success || receipt.FailureReason != enforce.ReasonCommandFailed {
		t.Errorf("unexpected receipt: success=%v reason=%q", receipt.Success, receipt.FailureReason)
	}
	// The real failure output must reach the model, not a generic message.
	if !strings.Contains(receipt.Stdout+receipt.Stderr, "want 5") {
		t.Errorf("the receipt should carry the real test output:\n%s\n%s", receipt.Stdout, receipt.Stderr)
	}

	// Completion is refused while the gate fails.
	if _, err := ctl.Complete(ctx, task.ID, "orchestrator"); !errors.Is(err, workflow.ErrStaleVerification) {
		t.Errorf("completion should be refused, got %v", err)
	}

	// Now actually fix the bug.
	if err := os.WriteFile(filepath.Join(dir, "add.go"),
		[]byte("package demo\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ctl.SubmitImplementation(task.ID, "engineer", "fixed the operator", []string{"add.go"}); err != nil {
		t.Fatalf("SubmitImplementation: %v", err)
	}
	outcome = ctl.Verify(ctx, task.ID)
	if outcome.Error != nil {
		t.Fatalf("Verify: %v", outcome.Error)
	}
	if !outcome.Set.Passed {
		t.Fatalf("the gate should pass now: %s / %s",
			outcome.Set.Receipts[0].FailureReason, outcome.Set.Receipts[0].Stdout)
	}
	if outcome.Task.State != workflow.StateQAReview {
		t.Fatalf("state = %s, want qa_review", outcome.Task.State)
	}

	// QA and completion now succeed.
	if _, err := ctl.SubmitQA(ctx, task.ID, "qa-engineer", enforce.VerdictApproved, "verified"); err != nil {
		t.Fatalf("SubmitQA: %v", err)
	}
	done, err := ctl.Complete(ctx, task.ID, "orchestrator")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if done.State != workflow.StateComplete {
		t.Errorf("state = %s, want complete", done.State)
	}
}

// TestRealEditAfterPassInvalidatesEvidence is the staleness guarantee against
// a real working tree: touching a file after the gate passed reopens
// verification.
func TestRealEditAfterPassInvalidatesEvidence(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real go test invocation")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}

	dir := realRepo(t)
	// Start from working code so the gate passes.
	if err := os.WriteFile(filepath.Join(dir, "add.go"),
		[]byte("package demo\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	policy, err := workflow.ParsePolicy(strings.NewReader(realPolicy), yaml.Unmarshal)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	store := session.NewStore(filepath.Join(dir, ".agentwarden", "state"))
	finger := enforce.NewGitFingerprinter(dir)
	gates := enforce.NewGateRunner(enforce.ExecRunner{}, finger, dir, clock, nil)
	machine := workflow.NewMachineWithTransitions(policy.Transitions(), clock)
	ctl := New(policy, machine, store, gates, finger, clock)
	ctx := context.Background()

	task, _ := ctl.Start("keep it working")
	ctl.SubmitPlan(task.ID, "tech-lead", "no change needed", []string{"tests pass"})
	ctl.SubmitImplementation(task.ID, "engineer", "already correct", nil)

	outcome := ctl.Verify(ctx, task.ID)
	if outcome.Error != nil || !outcome.Set.Passed {
		t.Fatalf("the gate should pass: err=%v receipts=%+v", outcome.Error, outcome.Set.Receipts)
	}

	// An edit lands after the gate passed but before review.
	if err := os.WriteFile(filepath.Join(dir, "sneaky.go"),
		[]byte("package demo\n\nvar Sneaky = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = ctl.SubmitQA(ctx, task.ID, "qa-engineer", enforce.VerdictApproved, "lgtm")
	if !errors.Is(err, workflow.ErrStaleVerification) {
		t.Fatalf("an edit after the gate passed must invalidate it, got %v", err)
	}

	reloaded, err := ctl.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != workflow.StateVerifying {
		t.Errorf("state = %s, want a return to verifying", reloaded.State)
	}
	if len(reloaded.Receipts) != 0 {
		t.Error("stale receipts should have been discarded")
	}
}
