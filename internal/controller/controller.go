// Package controller owns governed task lifecycle: it advances the state
// machine, runs gates itself, and records the evidence.
//
// It is the only producer of receipts and QA verdicts, so an agent's claim
// that a command passed never advances a workflow.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/lmurphy/agentwarden/internal/enforce"
	"github.com/lmurphy/agentwarden/internal/session"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

// Controller advances tasks under policy.
type Controller struct {
	policy  *workflow.Policy
	machine *workflow.Machine
	store   *session.Store
	gates   *enforce.GateRunner
	finger  enforce.Fingerprinter
	clock   workflow.Clock
}

// New wires a Controller.
func New(
	policy *workflow.Policy,
	machine *workflow.Machine,
	store *session.Store,
	gates *enforce.GateRunner,
	finger enforce.Fingerprinter,
	clock workflow.Clock,
) *Controller {
	return &Controller{
		policy:  policy,
		machine: machine,
		store:   store,
		gates:   gates,
		finger:  finger,
		clock:   clock,
	}
}

// NewTaskID returns a short random identifier.
func NewTaskID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// A timestamp is a sufficient fallback for a local identifier.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// Start creates a task in the initial state.
func (c *Controller) Start(objective string) (*workflow.Task, error) {
	if strings.TrimSpace(objective) == "" {
		return nil, fmt.Errorf("a task needs an objective")
	}
	now := c.clock.Now()
	task := &workflow.Task{
		ID:         NewTaskID(),
		Objective:  objective,
		State:      workflow.StatePlanning,
		PolicyHash: c.policy.Hash(),
		Receipts:   map[string]workflow.Receipt{},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if !c.policy.RequirePlan() {
		// Skipping the plan stage starts the work directly.
		task.State = workflow.StateImplementing
	}
	if err := c.store.Create(task); err != nil {
		return nil, err
	}
	return task, nil
}

// Get loads a task.
func (c *Controller) Get(taskID string) (*workflow.Task, error) {
	return c.store.Load(taskID)
}

// requireActor refuses an action taken by the wrong role. Identity is the
// agent ID the session was launched with; this is an enforcement boundary
// inside agentwarden, not a security boundary.
func (c *Controller) requireActor(actual string, role workflow.Role, action string) error {
	expected := c.policy.AgentFor(role)
	if expected == "" || actual == expected {
		return nil
	}
	return fmt.Errorf("%w: %s may only be performed by %s (%s attempted it)",
		workflow.ErrWrongActor, action, expected, actual)
}

// SubmitPlan records a plan and advances to implementation.
func (c *Controller) SubmitPlan(taskID, actor, plan string, criteria []string) (*workflow.Task, error) {
	if strings.TrimSpace(plan) == "" {
		return nil, fmt.Errorf("a plan cannot be empty")
	}
	return c.store.Update(taskID, func(task *workflow.Task) error {
		if err := c.requireActor(actor, workflow.RolePlanner, "submitting a plan"); err != nil {
			return err
		}
		task.Plan = renderPlan(plan, criteria)
		return c.machine.Transition(task, workflow.ActionPlanSubmitted, actor, nil)
	})
}

func renderPlan(plan string, criteria []string) string {
	var b strings.Builder
	b.WriteString(plan)
	if len(criteria) > 0 {
		b.WriteString("\n\nAcceptance criteria:\n")
		for _, c := range criteria {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	return b.String()
}

// SubmitImplementation records a handoff and moves to verification.
//
// Every prior receipt and the QA verdict are cleared: evidence gathered
// against an earlier tree says nothing about this one.
func (c *Controller) SubmitImplementation(taskID, actor, summary string, files []string) (*workflow.Task, error) {
	return c.store.Update(taskID, func(task *workflow.Task) error {
		if err := c.requireActor(actor, workflow.RoleImplementer, "submitting an implementation"); err != nil {
			return err
		}
		task.Handoff = renderHandoff(summary, files)
		task.PolicyHash = c.policy.Hash()
		enforce.InvalidateEvidence(task)
		return c.machine.Transition(task, workflow.ActionImplementationSubmitted, actor, nil)
	})
}

func renderHandoff(summary string, files []string) string {
	var b strings.Builder
	b.WriteString(summary)
	if len(files) > 0 {
		b.WriteString("\n\nFiles changed:\n")
		for _, f := range files {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	return b.String()
}

// GateOutcome reports the result of a verification run.
type GateOutcome struct {
	Set   enforce.GateSet
	Task  *workflow.Task
	Ran   bool
	Error error
}

// Verify runs the gates for the task's current state and records receipts.
//
// Gates run here rather than inside a handoff tool call, so a long suite does
// not block a single tool result for its entire timeout, and the caller can
// cancel it.
func (c *Controller) Verify(ctx context.Context, taskID string) GateOutcome {
	task, err := c.store.Load(taskID)
	if err != nil {
		return GateOutcome{Error: err}
	}
	if task.State != workflow.StateVerifying {
		return GateOutcome{
			Task:  task,
			Error: fmt.Errorf("task is %s, not verifying", task.State),
		}
	}

	// A policy edit invalidates the task's basis entirely, so send it back to
	// implementation rather than verifying against changed rules.
	if task.PolicyHash != c.policy.Hash() {
		updated, updateErr := c.store.Update(taskID, func(task *workflow.Task) error {
			enforce.InvalidateEvidence(task)
			task.PolicyHash = c.policy.Hash()
			return c.machine.Transition(task, workflow.ActionGateFailed, "controller",
				map[string]string{"reason": "policy_changed"})
		})
		return GateOutcome{Task: updated, Error: updateErr}
	}

	gates := c.policy.GatesFor(workflow.StateVerifying)
	set, err := c.gates.RunGates(ctx, gates, c.policy.Hash())
	if err != nil {
		return GateOutcome{Task: task, Set: set, Ran: true, Error: err}
	}

	action := workflow.ActionGatesVerified
	metadata := map[string]string{}
	if !set.Passed {
		action = workflow.ActionGateFailed
		metadata["failed_gate"] = set.FirstFailure
	}
	if !c.policy.RequireIndependentQA() && set.Passed {
		// With no review stage, passing gates go straight to completion.
		action = workflow.ActionGatesVerified
	}

	updated, err := c.store.Update(taskID, func(task *workflow.Task) error {
		for _, receipt := range set.Receipts {
			task.Receipts[receipt.GateID] = receipt
		}
		return c.machine.Transition(task, action, "controller", metadata)
	})
	return GateOutcome{Set: set, Task: updated, Ran: true, Error: err}
}

// SubmitQA records a reviewer's verdict.
func (c *Controller) SubmitQA(ctx context.Context, taskID, actor, verdict, notes string) (*workflow.Task, error) {
	switch verdict {
	case enforce.VerdictApproved, enforce.VerdictRejected:
	default:
		return nil, fmt.Errorf("verdict must be %q or %q", enforce.VerdictApproved, enforce.VerdictRejected)
	}

	fingerprint, err := c.finger.Fingerprint(ctx)
	if err != nil {
		return nil, err
	}

	// Staleness is handled as its own committed update. Update discards
	// mutations when its callback errors, so invalidating evidence and
	// reporting the failure have to be separate steps or the invalidation
	// would be rolled back.
	var staleness string
	if _, err := c.store.Update(taskID, func(task *workflow.Task) error {
		if err := c.requireActor(actor, workflow.RoleReviewer, "submitting a QA verdict"); err != nil {
			return err
		}
		// Independent QA means the reviewer must not be the implementer.
		if c.policy.RequireIndependentQA() {
			if implementer := c.policy.AgentFor(workflow.RoleImplementer); implementer != "" && actor == implementer {
				return fmt.Errorf("%w: independent QA forbids the implementer reviewing its own work",
					workflow.ErrWrongActor)
			}
		}

		// Re-check the evidence at decision time: the tree may have moved
		// while the reviewer was reading.
		if problem := enforce.VerificationProblem(task, c.policy, fingerprint); problem != "" {
			staleness = problem
			enforce.InvalidateEvidence(task)
			return c.machine.Transition(task, workflow.ActionCodeChanged, "controller",
				map[string]string{"reason": problem})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if staleness != "" {
		return nil, fmt.Errorf("%w: %s", workflow.ErrStaleVerification, staleness)
	}

	return c.store.Update(taskID, func(task *workflow.Task) error {
		task.QA = &workflow.QA{
			Verdict:    verdict,
			Actor:      actor,
			Notes:      notes,
			PolicyHash: c.policy.Hash(),
			Repository: fingerprint,
			DecidedAt:  c.clock.Now(),
		}
		action := workflow.ActionQAApproved
		if verdict == enforce.VerdictRejected {
			action = workflow.ActionQARejected
		}
		return c.machine.Transition(task, action, actor, nil)
	})
}

// Complete finishes a task, but only against the exact tree QA reviewed.
func (c *Controller) Complete(ctx context.Context, taskID, actor string) (*workflow.Task, error) {
	fingerprint, err := c.finger.Fingerprint(ctx)
	if err != nil {
		return nil, err
	}
	return c.store.Update(taskID, func(task *workflow.Task) error {
		if err := c.requireActor(actor, workflow.RoleOrchestrator, "completing a task"); err != nil {
			return err
		}
		if problem := enforce.VerificationProblem(task, c.policy, fingerprint); problem != "" {
			return fmt.Errorf("%w: %s", workflow.ErrStaleVerification, problem)
		}
		if c.policy.RequireIndependentQA() {
			if problem := enforce.QAProblem(task, c.policy, fingerprint); problem != "" {
				return fmt.Errorf("%w: %s", workflow.ErrStaleVerification, problem)
			}
		}
		return c.machine.Transition(task, workflow.ActionCompleted, actor, nil)
	})
}

// Block parks a task, recording why.
//
// Unlike the plugin, this requires an authorized actor: any bound agent could
// previously park a workflow indefinitely.
func (c *Controller) Block(taskID, actor, reason string) (*workflow.Task, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("blocking requires a reason")
	}
	return c.store.Update(taskID, func(task *workflow.Task) error {
		if !c.mayBlock(actor, task.State) {
			return fmt.Errorf("%w: %s may not block a task in state %s",
				workflow.ErrWrongActor, actor, task.State)
		}
		return c.machine.Transition(task, workflow.ActionBlocked, actor,
			map[string]string{"reason": reason})
	})
}

// mayBlock reports whether an actor can park the task: the orchestrator
// always, or whichever role currently owns the stage.
func (c *Controller) mayBlock(actor string, state workflow.State) bool {
	if actor == "" {
		return false
	}
	if actor == c.policy.AgentFor(workflow.RoleOrchestrator) {
		return true
	}
	if owner := enforce.RoleForState(state); owner != "" {
		return actor == c.policy.AgentFor(owner)
	}
	return false
}

// Resume returns a blocked task to the stage it was parked from.
func (c *Controller) Resume(taskID, actor string) (*workflow.Task, error) {
	return c.store.Update(taskID, func(task *workflow.Task) error {
		if err := c.requireActor(actor, workflow.RoleOrchestrator, "resuming a task"); err != nil {
			return err
		}
		return c.machine.Transition(task, workflow.ActionResumed, actor, nil)
	})
}

// Cancel abandons a task.
func (c *Controller) Cancel(taskID, actor, reason string) (*workflow.Task, error) {
	return c.store.Update(taskID, func(task *workflow.Task) error {
		if err := c.requireActor(actor, workflow.RoleOrchestrator, "cancelling a task"); err != nil {
			return err
		}
		return c.machine.Transition(task, workflow.ActionCancelled, actor,
			map[string]string{"reason": reason})
	})
}

// Evidence renders the controller's gate receipts for a reviewer's brief, so
// QA reviews recorded evidence rather than an agent's account of it.
func Evidence(task *workflow.Task) string {
	if len(task.Receipts) == 0 {
		return "No gate evidence has been recorded."
	}
	var b strings.Builder
	b.WriteString("Controller gate evidence:\n")
	for _, receipt := range task.Receipts {
		status := "PASS"
		if !receipt.Success {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "\n- gate %q: %s\n", receipt.GateID, status)
		fmt.Fprintf(&b, "  command: %s\n", strings.Join(receipt.Command, " "))
		if receipt.ExitCode != nil {
			fmt.Fprintf(&b, "  exit code: %d\n", *receipt.ExitCode)
		}
		if receipt.FailureReason != "" {
			fmt.Fprintf(&b, "  failure: %s\n", receipt.FailureReason)
		}
		if receipt.TimedOut {
			b.WriteString("  timed out: yes\n")
		}
		if receipt.OutputTruncated {
			b.WriteString("  output truncated: yes\n")
		}
		if tail := lastLines(receipt.Stdout, 20); tail != "" {
			fmt.Fprintf(&b, "  stdout tail:\n%s\n", indent(tail))
		}
		if tail := lastLines(receipt.Stderr, 20); tail != "" {
			fmt.Fprintf(&b, "  stderr tail:\n%s\n", indent(tail))
		}
	}
	return b.String()
}

// lastLines returns the final n lines of s.
func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = "    " + line
	}
	return strings.Join(lines, "\n")
}
