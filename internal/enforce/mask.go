package enforce

import (
	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

// Tool names the enforcer reasons about by identity rather than by string
// matching at call sites.
const (
	ToolTask  = "task"
	ToolRead  = "read"
	ToolWrite = "write"
	ToolEdit  = "edit"
	ToolBash  = "bash"
	ToolGlob  = "glob"
	ToolGrep  = "grep"
	ToolLS    = "ls"

	ToolSubmitPlan           = "workflow_submit_plan"
	ToolSubmitImplementation = "workflow_submit_implementation"
	ToolSubmitQA             = "workflow_submit_qa"
	ToolStatus               = "workflow_status"
	ToolHistory              = "workflow_history"
	ToolBlock                = "workflow_block"
	ToolComplete             = "workflow_complete"
)

// readOnlyTools are safe in any state.
var readOnlyTools = []string{ToolRead, ToolGrep, ToolGlob, ToolLS}

// alwaysVisible are the workflow tools every governed role may call.
var alwaysVisible = []string{ToolStatus, ToolHistory}

// defaultStateTools is the builtin masking table: for each state, which tools
// a session may even see. Absence from this list means the tool is omitted
// from the request payload entirely, so the model cannot call it regardless of
// what it was told.
var defaultStateTools = map[workflow.State][]string{
	// Planning is read-only and must hand off; no edit, write or bash.
	workflow.StatePlanning: append(append([]string{}, readOnlyTools...), ToolSubmitPlan),
	// Implementing is the only state with write access.
	workflow.StateImplementing: append(append([]string{}, readOnlyTools...),
		ToolWrite, ToolEdit, ToolBash, ToolSubmitImplementation),
	workflow.StateChangesRequested: append(append([]string{}, readOnlyTools...),
		ToolWrite, ToolEdit, ToolBash, ToolSubmitImplementation),
	// Verifying is the runtime's own stage; the model gets no tools that
	// could disturb the tree while gates run.
	workflow.StateVerifying: append([]string{}, readOnlyTools...),
	// QA reviews evidence; it can read and run inspection commands only.
	workflow.StateQAReview:        append(append([]string{}, readOnlyTools...), ToolBash, ToolSubmitQA),
	workflow.StateReadyToComplete: append(append([]string{}, readOnlyTools...), ToolComplete),
	workflow.StateBlocked:         append([]string{}, readOnlyTools...),
	workflow.StateComplete:        readOnlyTools,
	workflow.StateCancelled:       readOnlyTools,
}

// Session is the governance-relevant slice of a live session. It lives here
// rather than in package session so the enforcer has no dependency on session
// state management, keeping the dependency edge one-way.
type Session struct {
	TaskID  string
	AgentID string
	// Role is the workflow role this session was launched as.
	Role workflow.Role
	// DirectToolCalls counts non-delegating tool calls made in the current
	// state, driving the direct-work budget.
	DirectToolCalls int
	// Violations counts blocked attempts in the current state, driving the
	// escalation ladder.
	Violations int
	// ForcedTool is set when the previous turn's decision pinned the next
	// call to a specific tool.
	ForcedTool string
	// HandedOff records that this session completed its stage handoff. Once
	// it has, the session is finished and may end its turn freely: the next
	// stage belongs to a different role's session.
	HandedOff bool
}

// allowedToolNames returns the tool names visible for a state, honoring a
// custom state rule's allow_tools when one is declared.
func allowedToolNames(policy *workflow.Policy, state workflow.State) []string {
	if rule, ok := policy.States[state]; ok && len(rule.AllowTools) > 0 {
		return append(append([]string{}, rule.AllowTools...), alwaysVisible...)
	}
	base, ok := defaultStateTools[state]
	if !ok {
		// An unknown state gets read-only access. Failing closed means adding
		// a state cannot accidentally grant write access.
		base = readOnlyTools
	}
	return append(append([]string{}, base...), alwaysVisible...)
}

// delegationAllowed reports whether the state expects work to be handed to a
// subagent, in which case the task tool becomes visible.
func delegationAllowed(policy *workflow.Policy, state workflow.State) bool {
	if rule, ok := policy.States[state]; ok {
		return len(rule.DelegateTo) > 0
	}
	// In the builtin graph the orchestrator drives handoffs, so no role-level
	// session is granted the task tool by default.
	return false
}

// MaskTools filters defs down to what the state permits. The returned slice
// preserves input order so request logs stay diffable.
func MaskTools(policy *workflow.Policy, state workflow.State, defs []provider.ToolDef) []provider.ToolDef {
	allowed := make(map[string]bool)
	for _, name := range allowedToolNames(policy, state) {
		allowed[name] = true
	}
	if delegationAllowed(policy, state) {
		allowed[ToolTask] = true
	}

	out := make([]provider.ToolDef, 0, len(defs))
	for _, def := range defs {
		if allowed[def.Name] {
			out = append(out, def)
		}
	}
	return out
}

// ToolVisible reports whether a single tool is permitted in a state.
func ToolVisible(policy *workflow.Policy, state workflow.State, name string) bool {
	for _, allowed := range allowedToolNames(policy, state) {
		if allowed == name {
			return true
		}
	}
	return delegationAllowed(policy, state) && name == ToolTask
}

// RoleForState returns the role responsible for a state, so a session can tell
// whether the current stage is still its own.
func RoleForState(state workflow.State) workflow.Role {
	switch state {
	case workflow.StatePlanning:
		return workflow.RolePlanner
	case workflow.StateImplementing, workflow.StateChangesRequested:
		return workflow.RoleImplementer
	case workflow.StateQAReview:
		return workflow.RoleReviewer
	case workflow.StateReadyToComplete:
		return workflow.RoleOrchestrator
	default:
		return ""
	}
}

// HandoffTool returns the tool a state expects before the turn may end, or ""
// when the state has no mandatory handoff.
func HandoffTool(state workflow.State) string {
	switch state {
	case workflow.StatePlanning:
		return ToolSubmitPlan
	case workflow.StateImplementing, workflow.StateChangesRequested:
		return ToolSubmitImplementation
	case workflow.StateQAReview:
		return ToolSubmitQA
	case workflow.StateReadyToComplete:
		return ToolComplete
	default:
		return ""
	}
}
