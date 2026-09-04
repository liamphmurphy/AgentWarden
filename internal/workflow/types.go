// Package workflow contains the pure workflow domain: states, transitions and
// policy. It performs no I/O — no exec, no network, no filesystem, no clock —
// so the entire governance model is testable without a repository or a model.
package workflow

import "time"

// State is a workflow state. The zero value is not a valid state.
type State string

const (
	StatePlanning         State = "planning"
	StateImplementing     State = "implementing"
	StateVerifying        State = "verifying"
	StateQAReview         State = "qa_review"
	StateChangesRequested State = "changes_requested"
	StateReadyToComplete  State = "ready_to_complete"
	StateComplete         State = "complete"
	StateBlocked          State = "blocked"
	StateCancelled        State = "cancelled"
)

// Action is an event that may advance a task from one state to another.
type Action string

const (
	ActionPlanSubmitted           Action = "plan_submitted"
	ActionImplementationSubmitted Action = "implementation_submitted"
	ActionGatesVerified           Action = "gates_verified"
	ActionGateFailed              Action = "gate_failed"
	ActionQAApproved              Action = "qa_approved"
	ActionQARejected              Action = "qa_rejected"
	ActionCodeChanged             Action = "code_changed"
	ActionCompleted               Action = "completed"
	ActionBlocked                 Action = "blocked"
	ActionResumed                 Action = "resumed"
	ActionCancelled               Action = "cancelled"
)

// Role names a participant in the workflow. Roles are mapped to concrete agent
// IDs by the policy, so the state machine never names an agent directly.
type Role string

const (
	RoleOrchestrator Role = "orchestrator"
	RolePlanner      Role = "planner"
	RoleImplementer  Role = "implementer"
	RoleReviewer     Role = "reviewer"
)

// Clock is injected so transition timestamps are deterministic in tests.
type Clock interface {
	Now() time.Time
}

// Event is one immutable entry in a task's audit history.
type Event struct {
	Sequence  int               `json:"sequence"`
	From      State             `json:"from"`
	To        State             `json:"to"`
	Action    Action            `json:"action"`
	Actor     string            `json:"actor"`
	Timestamp time.Time         `json:"timestamp"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Receipt is evidence that the runtime itself executed a gate. Only the
// enforcer creates receipts; an agent's claim that a command passed is never
// accepted as one.
type Receipt struct {
	GateID          string      `json:"gateID"`
	Command         []string    `json:"command"`
	ExitCode        *int        `json:"exitCode"`
	Success         bool        `json:"success"`
	FailureReason   string      `json:"failureReason,omitempty"`
	TimedOut        bool        `json:"timedOut"`
	OutputTruncated bool        `json:"outputTruncated"`
	Stdout          string      `json:"stdout"`
	Stderr          string      `json:"stderr"`
	DurationMS      int64       `json:"durationMS"`
	PolicyHash      string      `json:"policyHash"`
	Repository      Fingerprint `json:"repository"`
	RanAt           time.Time   `json:"ranAt"`
}

// Fingerprint identifies an exact repository working tree.
type Fingerprint struct {
	Head   string `json:"head"`
	Digest string `json:"digest"`
}

// IsEditingState reports whether a state is one where the model may change
// the work tree, and so one whose entry needs a baseline recorded.
func IsEditingState(s State) bool {
	return s == StateImplementing || s == StateChangesRequested
}

// Same reports whether two fingerprints describe the same working tree. Both
// components must match, and an empty fingerprint never equals another.
func (f Fingerprint) Same(other Fingerprint) bool {
	if f.Head == "" || f.Digest == "" {
		return false
	}
	return f.Head == other.Head && f.Digest == other.Digest
}

// QA is a reviewer's verdict, bound to the inputs it was formed against.
type QA struct {
	Verdict    string      `json:"verdict"` // "approved" | "rejected"
	Actor      string      `json:"actor"`
	Notes      string      `json:"notes,omitempty"`
	PolicyHash string      `json:"policyHash"`
	Repository Fingerprint `json:"repository"`
	DecidedAt  time.Time   `json:"decidedAt"`
}

// Task is the full persisted state of one governed unit of work.
type Task struct {
	ID          string `json:"id"`
	Objective   string `json:"objective"`
	State       State  `json:"state"`
	ResumeState State  `json:"resumeState,omitempty"`
	Plan        string `json:"plan,omitempty"`
	Handoff     string `json:"handoff,omitempty"`
	// Baseline is the work tree as it stood when the task entered an editing
	// stage. It is what makes "I changed nothing" detectable without
	// believing anything the model says: a submission whose fingerprint still
	// equals the baseline cannot be an implementation.
	Baseline   *Fingerprint       `json:"baseline,omitempty"`
	PolicyHash string             `json:"policyHash"`
	Receipts   map[string]Receipt `json:"receipts"`
	QA         *QA                `json:"qa,omitempty"`
	Events     []Event            `json:"events"`
	Revision   int                `json:"revision"`
	CreatedAt  time.Time          `json:"createdAt"`
	UpdatedAt  time.Time          `json:"updatedAt"`
}
