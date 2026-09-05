package enforce

import (
	"strings"

	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

// WorkflowToolPrefix marks the tools that drive the state machine. They are
// recognised by prefix rather than by an explicit list, so a policy or a
// future tool cannot escape the check by being added in one place and
// forgotten in another — the failure the plugin had, where delegation was
// suppressed by deleting two hardcoded key names.
const WorkflowToolPrefix = "workflow_"

// IsWorkflowTool reports whether a tool name belongs to the state machine.
func IsWorkflowTool(name string) bool {
	return strings.HasPrefix(name, WorkflowToolPrefix)
}

// Governor is the seam the agent loop talks to. Both the real Enforcer and the
// no-op implementation satisfy it, so the loop has one code path whether or
// not governance is active.
type Governor interface {
	// VisibleTools filters the tool set for the next request.
	VisibleTools(task *workflow.Task, sess *Session, all []provider.ToolDef) []provider.ToolDef
	// ToolChoice optionally constrains the next turn.
	ToolChoice(task *workflow.Task, sess *Session) *provider.ToolChoice
	// Intercept judges a tool call before it executes.
	Intercept(task *workflow.Task, sess *Session, call provider.ToolCall) Decision
	// OnTurnEnd judges a turn that is about to end without another tool call.
	OnTurnEnd(task *workflow.Task, sess *Session) Decision
	// OnComplete judges an attempt to finish the task.
	OnComplete(task *workflow.Task, current workflow.Fingerprint) Decision
	// Banner renders the per-turn state block, or "" when not governed.
	Banner(task *workflow.Task, sess *Session, visible []provider.ToolDef) string
	// Enabled reports whether governance is active, for the status bar.
	Enabled() bool
}

// Enabled reports that the real enforcer governs the session.
func (e *Enforcer) Enabled() bool { return true }

// Nop is an ungoverned Governor: every tool is visible, nothing is blocked and
// no banner is injected. This is what backs `--no-workflow`, `agentwarden run` and
// the /plain toggle, so a quick question does not pay for the state machine.
type Nop struct{}

// NewNop returns the ungoverned Governor.
func NewNop() Nop { return Nop{} }

// VisibleTools returns every tool except the ones that drive the state
// machine.
//
// Those are withheld for two reasons. They would be live: the workflow tools
// mutate stored task state through the controller, so an ungoverned session
// could submit a plan or complete a task with nothing checking gates —
// governance would be off while its state advanced. And their mere presence
// misleads: a model handed workflow_start and workflow_status infers it is in
// a governed workflow and narrates one, which is exactly what plain mode is
// for avoiding.
func (Nop) VisibleTools(_ *workflow.Task, _ *Session, all []provider.ToolDef) []provider.ToolDef {
	out := make([]provider.ToolDef, 0, len(all))
	for _, def := range all {
		if IsWorkflowTool(def.Name) {
			continue
		}
		out = append(out, def)
	}
	return out
}

// ToolChoice never constrains the model.
func (Nop) ToolChoice(*workflow.Task, *Session) *provider.ToolChoice { return nil }

// Intercept allows every call except one into the state machine.
//
// Masking already hides those tools, so reaching here means the model asked
// for one anyway — most often by repeating a call it made before governance
// was switched off, which is still in its context.
func (Nop) Intercept(_ *workflow.Task, _ *Session, call provider.ToolCall) Decision {
	if IsWorkflowTool(call.Name) {
		return Decision{
			Reason: call.Name + " is unavailable: this session is not governed",
			Correction: "There is no workflow in this session. " + call.Name +
				" does not apply; carry out the request directly with the tools you have.",
		}
	}
	return allow()
}

// OnTurnEnd allows the turn to end.
func (Nop) OnTurnEnd(*workflow.Task, *Session) Decision { return allow() }

// OnComplete allows completion.
func (Nop) OnComplete(*workflow.Task, workflow.Fingerprint) Decision { return allow() }

// Banner injects nothing.
func (Nop) Banner(*workflow.Task, *Session, []provider.ToolDef) string { return "" }

// Enabled reports that the session is ungoverned.
func (Nop) Enabled() bool { return false }
