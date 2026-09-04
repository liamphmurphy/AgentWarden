package enforce

import (
	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

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
	// OnTurnEnd judges a turn that is about to end.
	OnTurnEnd(task *workflow.Task, sess *Session, calledTools []string) Decision
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

// VisibleTools returns every tool unchanged.
func (Nop) VisibleTools(_ *workflow.Task, _ *Session, all []provider.ToolDef) []provider.ToolDef {
	return all
}

// ToolChoice never constrains the model.
func (Nop) ToolChoice(*workflow.Task, *Session) *provider.ToolChoice { return nil }

// Intercept allows every call.
func (Nop) Intercept(*workflow.Task, *Session, provider.ToolCall) Decision { return allow() }

// OnTurnEnd allows the turn to end.
func (Nop) OnTurnEnd(*workflow.Task, *Session, []string) Decision { return allow() }

// OnComplete allows completion.
func (Nop) OnComplete(*workflow.Task, workflow.Fingerprint) Decision { return allow() }

// Banner injects nothing.
func (Nop) Banner(*workflow.Task, *Session, []provider.ToolDef) string { return "" }

// Enabled reports that the session is ungoverned.
func (Nop) Enabled() bool { return false }
