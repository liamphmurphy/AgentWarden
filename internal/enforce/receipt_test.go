package enforce

import (
	"strings"
	"testing"

	"github.com/lmurphy/agentwarden/internal/workflow"
	"gopkg.in/yaml.v3"
)

func TestVerificationProblemAcceptsCompleteEvidence(t *testing.T) {
	policy := mustPolicy(t, twoGatePolicy)
	current := workflow.Fingerprint{Head: "h1", Digest: "d1"}
	task := newTask(workflow.StateQAReview, policy)
	task.Receipts["unit"] = passingReceipt("unit", policy, current)
	task.Receipts["integration"] = passingReceipt("integration", policy, current)

	if problem := VerificationProblem(task, policy, current); problem != "" {
		t.Errorf("evidence should be accepted, got %q", problem)
	}
}

// TestVerificationProblemRejectsStaleEvidence enumerates every way evidence
// goes stale. This is the predicate that stops "the tests passed ten edits
// ago" from counting as passing now.
func TestVerificationProblemRejectsStaleEvidence(t *testing.T) {
	current := workflow.Fingerprint{Head: "h1", Digest: "d1"}
	stale := workflow.Fingerprint{Head: "h1", Digest: "d2"}

	tests := []struct {
		name     string
		setup    func(t *testing.T, task *workflow.Task, policy *workflow.Policy)
		wantWord string
	}{
		{
			name:     "no receipts at all",
			setup:    func(*testing.T, *workflow.Task, *workflow.Policy) {},
			wantWord: "no receipt",
		},
		{
			name: "one required gate missing",
			setup: func(_ *testing.T, task *workflow.Task, policy *workflow.Policy) {
				task.Receipts["unit"] = passingReceipt("unit", policy, current)
			},
			wantWord: "no receipt",
		},
		{
			name: "a required gate failed",
			setup: func(_ *testing.T, task *workflow.Task, policy *workflow.Policy) {
				task.Receipts["unit"] = passingReceipt("unit", policy, current)
				failed := passingReceipt("integration", policy, current)
				failed.Success = false
				task.Receipts["integration"] = failed
			},
			wantWord: "no passing receipt",
		},
		{
			name: "tree moved after the gate passed",
			setup: func(_ *testing.T, task *workflow.Task, policy *workflow.Policy) {
				task.Receipts["unit"] = passingReceipt("unit", policy, stale)
				task.Receipts["integration"] = passingReceipt("integration", policy, current)
			},
			wantWord: "repository changed",
		},
		{
			name: "receipt produced under another policy",
			setup: func(_ *testing.T, task *workflow.Task, policy *workflow.Policy) {
				other := passingReceipt("unit", policy, current)
				other.PolicyHash = "some-other-hash"
				task.Receipts["unit"] = other
				task.Receipts["integration"] = passingReceipt("integration", policy, current)
			},
			wantWord: "different policy",
		},
		{
			name: "task predates the current policy",
			setup: func(_ *testing.T, task *workflow.Task, policy *workflow.Policy) {
				task.PolicyHash = "old-hash"
				task.Receipts["unit"] = passingReceipt("unit", policy, current)
				task.Receipts["integration"] = passingReceipt("integration", policy, current)
			},
			wantWord: "policy changed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy := mustPolicy(t, twoGatePolicy)
			task := newTask(workflow.StateQAReview, policy)
			tc.setup(t, task, policy)

			problem := VerificationProblem(task, policy, current)
			if problem == "" {
				t.Fatal("expected the evidence to be rejected")
			}
			if !strings.Contains(problem, tc.wantWord) {
				t.Errorf("problem = %q, want it to mention %q", problem, tc.wantWord)
			}
		})
	}
}

// TestOptionalGateFailureDoesNotInvalidate: an optional gate runs and reports
// but must never block completion.
func TestOptionalGateFailureDoesNotInvalidate(t *testing.T) {
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
	current := workflow.Fingerprint{Head: "h1", Digest: "d1"}
	task := newTask(workflow.StateQAReview, policy)
	task.Receipts["unit"] = passingReceipt("unit", policy, current)
	failed := passingReceipt("lint", policy, current)
	failed.Success = false
	task.Receipts["lint"] = failed

	if problem := VerificationProblem(task, policy, current); problem != "" {
		t.Errorf("a failing optional gate must not invalidate: %q", problem)
	}
}

func TestQAProblem(t *testing.T) {
	current := workflow.Fingerprint{Head: "h1", Digest: "d1"}
	moved := workflow.Fingerprint{Head: "h1", Digest: "d2"}

	tests := []struct {
		name     string
		qa       func(policy *workflow.Policy) *workflow.QA
		wantWord string
	}{
		{
			name:     "missing verdict",
			qa:       func(*workflow.Policy) *workflow.QA { return nil },
			wantWord: "no QA verdict",
		},
		{
			name: "rejected",
			qa: func(p *workflow.Policy) *workflow.QA {
				return &workflow.QA{Verdict: VerdictRejected, PolicyHash: p.Hash(), Repository: current}
			},
			wantWord: "rejected",
		},
		{
			name: "approved under a different policy",
			qa: func(*workflow.Policy) *workflow.QA {
				return &workflow.QA{Verdict: VerdictApproved, PolicyHash: "other", Repository: current}
			},
			wantWord: "different policy",
		},
		{
			// Completion requires the tree be byte-identical to what QA saw.
			name: "tree moved after approval",
			qa: func(p *workflow.Policy) *workflow.QA {
				return &workflow.QA{Verdict: VerdictApproved, PolicyHash: p.Hash(), Repository: moved}
			},
			wantWord: "changed after QA",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy := mustPolicy(t, twoGatePolicy)
			task := newTask(workflow.StateReadyToComplete, policy)
			task.QA = tc.qa(policy)

			problem := QAProblem(task, policy, current)
			if problem == "" {
				t.Fatal("expected QA to be rejected")
			}
			if !strings.Contains(problem, tc.wantWord) {
				t.Errorf("problem = %q, want it to mention %q", problem, tc.wantWord)
			}
		})
	}
}

func TestQAProblemAcceptsFreshApproval(t *testing.T) {
	policy := mustPolicy(t, twoGatePolicy)
	current := workflow.Fingerprint{Head: "h1", Digest: "d1"}
	task := newTask(workflow.StateReadyToComplete, policy)
	task.QA = &workflow.QA{Verdict: VerdictApproved, PolicyHash: policy.Hash(), Repository: current}

	if problem := QAProblem(task, policy, current); problem != "" {
		t.Errorf("fresh approval should be accepted, got %q", problem)
	}
}

func TestInvalidateEvidence(t *testing.T) {
	policy := mustPolicy(t, twoGatePolicy)
	current := workflow.Fingerprint{Head: "h1", Digest: "d1"}
	task := newTask(workflow.StateQAReview, policy)
	task.Receipts["unit"] = passingReceipt("unit", policy, current)
	task.QA = &workflow.QA{Verdict: VerdictApproved}

	InvalidateEvidence(task)

	if len(task.Receipts) != 0 {
		t.Error("receipts should be cleared")
	}
	if task.QA != nil {
		t.Error("QA verdict should be cleared")
	}
}

// TestPolicyEditInvalidatesPassingEvidence is the end-to-end staleness story
// at the policy level: tightening a gate must not leave old evidence valid.
func TestPolicyEditInvalidatesPassingEvidence(t *testing.T) {
	const before = `
version: 1
roles: {implementer: engineer, reviewer: qa-engineer}
gates:
  - id: unit
    command: ["go", "test", "./..."]
`
	original := mustPolicy(t, before)
	current := workflow.Fingerprint{Head: "h1", Digest: "d1"}

	task := newTask(workflow.StateQAReview, original)
	task.Receipts["unit"] = passingReceipt("unit", original, current)
	if problem := VerificationProblem(task, original, current); problem != "" {
		t.Fatalf("evidence should start valid: %q", problem)
	}

	// The same gate ID, now a stricter command.
	tightened, err := workflow.ParsePolicy(strings.NewReader(
		strings.Replace(before, `["go", "test", "./..."]`, `["go", "test", "-race", "./..."]`, 1)),
		yaml.Unmarshal)
	if err != nil {
		t.Fatalf("parse tightened policy: %v", err)
	}

	if problem := VerificationProblem(task, tightened, current); problem == "" {
		t.Error("a tightened gate command must invalidate the old receipt")
	}
}
