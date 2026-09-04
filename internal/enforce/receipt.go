package enforce

import (
	"fmt"

	"github.com/lmurphy/agentwarden/internal/workflow"
)

// VerificationProblem reports why a task's gate evidence is not currently
// valid, or "" when the evidence stands. This single predicate is what stops
// "the tests passed ten edits ago" from counting as passing now.
//
// Evidence is invalid when the policy changed, when a required gate has no
// passing receipt, when a receipt was produced under a different policy, or
// when the tree has moved since the receipt was written.
func VerificationProblem(task *workflow.Task, policy *workflow.Policy, current workflow.Fingerprint) string {
	if task.PolicyHash != policy.Hash() {
		return "workflow policy changed after this task was created"
	}
	for _, gate := range policy.RequiredGates() {
		receipt, ok := task.Receipts[gate.ID]
		switch {
		case !ok:
			return fmt.Sprintf("required gate %q has no receipt", gate.ID)
		case !receipt.Success:
			return fmt.Sprintf("required gate %q has no passing receipt", gate.ID)
		case receipt.PolicyHash != policy.Hash():
			return fmt.Sprintf("receipt for gate %q was produced under a different policy", gate.ID)
		case !receipt.Repository.Same(current):
			return fmt.Sprintf("repository changed after gate %q passed", gate.ID)
		}
	}
	return ""
}

// QAProblem reports why a QA verdict is not currently usable, or "".
func QAProblem(task *workflow.Task, policy *workflow.Policy, current workflow.Fingerprint) string {
	if task.QA == nil {
		return "no QA verdict recorded"
	}
	if task.QA.Verdict != VerdictApproved {
		return fmt.Sprintf("QA verdict is %q", task.QA.Verdict)
	}
	if task.QA.PolicyHash != policy.Hash() {
		return "QA was performed under a different policy"
	}
	// Completion requires the tree be byte-identical to what QA reviewed.
	if !task.QA.Repository.Same(current) {
		return "repository changed after QA approval"
	}
	return ""
}

// QA verdict values.
const (
	VerdictApproved = "approved"
	VerdictRejected = "rejected"
)

// InvalidateEvidence clears receipts and QA. It is called whenever the inputs
// that evidence was bound to have moved, so stale evidence can never survive
// into a later stage.
func InvalidateEvidence(task *workflow.Task) {
	task.Receipts = map[string]workflow.Receipt{}
	task.QA = nil
}
