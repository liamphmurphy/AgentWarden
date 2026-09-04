package enforce

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

// Escalation steps for repeated violations in a state.
const (
	// EscalateWarn returns a corrective message and lets the model retry.
	EscalateWarn = "warn"
	// EscalateForce pins the next call to the required tool via tool_choice.
	EscalateForce = "force"
	// EscalateAuto has the runtime perform the required action itself.
	EscalateAuto = "auto"
)

// defaultLadder is used when a state declares no on_violation sequence.
var defaultLadder = []string{EscalateWarn, EscalateForce, EscalateAuto}

// Decision is the enforcer's verdict on a proposed action.
type Decision struct {
	// Allow reports whether the action may proceed.
	Allow bool
	// Reason states why an action was refused, for logs and the UI.
	Reason string
	// Correction is the synthetic tool result handed back to the model. It
	// names the required next step and includes a filled-in argument
	// skeleton, because small models recover from templates far better than
	// from prose telling them what they did wrong.
	Correction string
	// ForceTool pins the next turn's tool_choice to this tool.
	ForceTool string
	// AutoPerform asks the caller to carry out the required action itself,
	// the last rung of the ladder.
	AutoPerform string
}

// allow is the permissive decision.
func allow() Decision { return Decision{Allow: true} }

// Enforcer applies workflow policy to a live session. The zero value is not
// usable; construct one with New.
type Enforcer struct {
	policy     *workflow.Policy
	machine    *workflow.Machine
	policyPath string
}

// New returns an Enforcer for a policy.
func New(policy *workflow.Policy, machine *workflow.Machine, policyPath string) *Enforcer {
	return &Enforcer{policy: policy, machine: machine, policyPath: policyPath}
}

// Policy exposes the active policy.
func (e *Enforcer) Policy() *workflow.Policy { return e.policy }

// VisibleTools returns the tools that may appear in the next request. This is
// the primary structural lever: a masked tool is absent from the payload, so
// there is no instruction for the model to disobey.
func (e *Enforcer) VisibleTools(task *workflow.Task, _ *Session, all []provider.ToolDef) []provider.ToolDef {
	return MaskTools(e.policy, task.State, all)
}

// ToolChoice constrains the next turn. It pins a specific tool when the
// session is under a forced-escalation, or when the state's only remaining
// legal move is its handoff.
func (e *Enforcer) ToolChoice(task *workflow.Task, sess *Session) *provider.ToolChoice {
	if sess.ForcedTool != "" {
		return &provider.ToolChoice{Mode: provider.ToolChoiceFunction, Name: sess.ForcedTool}
	}
	// Once the direct-work budget is spent, the handoff is the only way out.
	if budget := e.budget(task.State); budget > 0 && sess.DirectToolCalls >= budget {
		if handoff := HandoffTool(task.State); handoff != "" {
			return &provider.ToolChoice{Mode: provider.ToolChoiceFunction, Name: handoff}
		}
	}
	return nil
}

// budget returns the direct-tool-call allowance for a state, 0 meaning
// unlimited.
func (e *Enforcer) budget(state workflow.State) int {
	if rule, ok := e.policy.States[state]; ok {
		return rule.MaxDirectToolCalls
	}
	return 0
}

// ladder returns the escalation sequence for a state.
func (e *Enforcer) ladder(state workflow.State) []string {
	if rule, ok := e.policy.States[state]; ok && len(rule.OnViolation) > 0 {
		return rule.OnViolation
	}
	return defaultLadder
}

// escalate picks the rung for the violation count and builds the decision.
func (e *Enforcer) escalate(task *workflow.Task, sess *Session, reason string) Decision {
	ladder := e.ladder(task.State)
	// Violations is incremented by the caller before this runs, so the first
	// violation reads rung 0.
	idx := sess.Violations - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(ladder) {
		idx = len(ladder) - 1
	}

	handoff := HandoffTool(task.State)
	decision := Decision{
		Allow:      false,
		Reason:     reason,
		Correction: e.correction(task, reason, handoff),
	}
	switch ladder[idx] {
	case EscalateForce:
		decision.ForceTool = handoff
	case EscalateAuto:
		decision.ForceTool = handoff
		decision.AutoPerform = handoff
	}
	return decision
}

// correction renders the message the model sees in place of a tool result.
func (e *Enforcer) correction(task *workflow.Task, reason, handoff string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "BLOCKED: %s\n\n", reason)
	fmt.Fprintf(&b, "Workflow state: %s\n", task.State)

	if problem := e.pendingGateSummary(task); problem != "" {
		fmt.Fprintf(&b, "Pending verification: %s\n", problem)
	}
	if handoff != "" {
		fmt.Fprintf(&b, "\nRequired next action: call %s\n", handoff)
		if skeleton := ArgumentSkeleton(handoff); skeleton != "" {
			fmt.Fprintf(&b, "\nUse exactly this shape, filling in the values:\n%s\n", skeleton)
		}
	}
	return b.String()
}

// pendingGateSummary lists required gates still lacking a passing receipt.
func (e *Enforcer) pendingGateSummary(task *workflow.Task) string {
	var pending []string
	for _, gate := range e.policy.RequiredGates() {
		if receipt, ok := task.Receipts[gate.ID]; !ok || !receipt.Success {
			pending = append(pending, gate.ID)
		}
	}
	sort.Strings(pending)
	if len(pending) == 0 {
		return ""
	}
	return strings.Join(pending, ", ") + " (not yet passing)"
}

// ArgumentSkeleton returns a pre-filled JSON template for a tool, so a
// blocked model is shown the shape of the correct call rather than being told
// to work it out.
func ArgumentSkeleton(toolName string) string {
	var skeleton any
	switch toolName {
	case ToolTask:
		skeleton = map[string]any{
			"subagent":            "<agent id>",
			"objective":           "<what this subagent must accomplish>",
			"acceptance_criteria": []string{"<observable criterion>"},
			"files_in_scope":      []string{"<path>"},
		}
	case ToolSubmitPlan:
		skeleton = map[string]any{
			"plan":                "<the plan>",
			"acceptance_criteria": []string{"<observable criterion>"},
		}
	case ToolSubmitImplementation:
		skeleton = map[string]any{
			"summary":       "<what changed>",
			"files_changed": []string{"<path>"},
		}
	case ToolSubmitQA:
		skeleton = map[string]any{
			"verdict": "approved | rejected",
			"notes":   "<what you checked>",
		}
	default:
		return ""
	}
	out, err := json.MarshalIndent(skeleton, "", "  ")
	if err != nil {
		return ""
	}
	return string(out)
}

// Intercept judges a tool call before it executes. Every refusal path
// increments the session's violation count so repeated attempts escalate.
func (e *Enforcer) Intercept(task *workflow.Task, sess *Session, call provider.ToolCall) Decision {
	// A masked tool should never have been offered, so reaching here means
	// the model invented the name or a stale schema was cached.
	if !ToolVisible(e.policy, task.State, call.Name) {
		sess.Violations++
		return e.escalate(task, sess,
			fmt.Sprintf("tool %q is not available in state %s", call.Name, task.State))
	}

	// Publication stays blocked until the task is genuinely complete.
	if call.Name == ToolBash && task.State != workflow.StateComplete {
		if argv, err := bashArgv(call.Args); err == nil && IsPublishCommand(argv) {
			sess.Violations++
			return Decision{
				Allow:  false,
				Reason: fmt.Sprintf("publication is blocked while the task is %s", task.State),
				Correction: fmt.Sprintf(
					"BLOCKED: %q publishes work, and workflow state is %s.\n\n"+
						"Publication is only permitted once the workflow is complete.",
					strings.Join(argv, " "), task.State),
			}
		}
	}

	// The active policy cannot be rewritten by a governed session.
	if call.Name == ToolEdit || call.Name == ToolWrite {
		if path, err := editPath(call.Args); err == nil && IsPolicyEdit(e.policyPath, path) {
			sess.Violations++
			return Decision{
				Allow:      false,
				Reason:     "the active workflow policy cannot be edited by a governed session",
				Correction: "BLOCKED: the workflow policy file is the governance input and cannot be edited from inside a governed session.",
			}
		}
	}

	// A delegation must actually decompose the work.
	if call.Name == ToolTask {
		if problem := validateDelegation(call.Args); problem != "" {
			sess.Violations++
			return Decision{
				Allow:      false,
				Reason:     problem,
				Correction: fmt.Sprintf("BLOCKED: %s\n\nUse exactly this shape:\n%s", problem, ArgumentSkeleton(ToolTask)),
				ForceTool:  ToolTask,
			}
		}
	}

	if call.Name != HandoffTool(task.State) {
		sess.DirectToolCalls++
	}
	return allow()
}

// OnTurnEnd fires when the model ends a turn. Because the loop owns the turn
// lifecycle, a role that stops without handing off is detectable here — the
// gap that had no equivalent hook in the plugin and needed a human to notice.
func (e *Enforcer) OnTurnEnd(task *workflow.Task, sess *Session, calledTools []string) Decision {
	// A session that already handed off is finished; the next stage belongs
	// to a different role's session, so it must not be asked for that stage's
	// handoff too.
	if sess.HandedOff {
		return allow()
	}
	// Likewise if the workflow has moved past the stage this session owns.
	if sess.Role != "" && RoleForState(task.State) != sess.Role {
		return allow()
	}

	handoff := HandoffTool(task.State)
	if handoff == "" {
		return allow()
	}
	for _, name := range calledTools {
		if name == handoff {
			return allow()
		}
	}
	sess.Violations++
	return e.escalate(task, sess,
		fmt.Sprintf("the turn ended in state %s without calling %s", task.State, handoff))
}

// OnComplete judges an attempt to finish. It reports what is missing; the
// caller runs the gates, because the enforcer never trusts a claim that they
// already ran.
func (e *Enforcer) OnComplete(task *workflow.Task, current workflow.Fingerprint) Decision {
	if problem := VerificationProblem(task, e.policy, current); problem != "" {
		return Decision{
			Allow:  false,
			Reason: problem,
			Correction: fmt.Sprintf(
				"BLOCKED: cannot complete — %s\n\nRequired gates must pass against the current working tree.\nPending: %s",
				problem, e.pendingGateSummary(task)),
		}
	}
	if problem := QAProblem(task, e.policy, current); problem != "" {
		return Decision{
			Allow:      false,
			Reason:     problem,
			Correction: fmt.Sprintf("BLOCKED: cannot complete — %s", problem),
		}
	}
	return allow()
}

// Banner renders the state block injected each turn near the newest message.
// Small models lose instructions placed only in a distant system prompt, so
// the authoritative state is repeated every turn.
func (e *Enforcer) Banner(task *workflow.Task, sess *Session, visible []provider.ToolDef) string {
	names := make([]string, 0, len(visible))
	for _, def := range visible {
		names = append(names, def.Name)
	}

	var b strings.Builder
	b.WriteString("=== WORKFLOW STATE (authoritative) ===\n")
	fmt.Fprintf(&b, "State: %s\n", task.State)
	if sess.Role != "" {
		fmt.Fprintf(&b, "Your role: %s (%s)\n", sess.Role, sess.AgentID)
	}
	fmt.Fprintf(&b, "Available tools: %s\n", strings.Join(names, ", "))

	if pending := e.pendingGateSummary(task); pending != "" {
		fmt.Fprintf(&b, "Gates not yet passing: %s\n", pending)
	} else if len(e.policy.RequiredGates()) > 0 {
		b.WriteString("Gates: all required gates passing\n")
	}

	if handoff := HandoffTool(task.State); handoff != "" {
		fmt.Fprintf(&b, "Required next action: call %s before ending your turn\n", handoff)
	}
	if sess.ForcedTool != "" {
		fmt.Fprintf(&b, "You must call %s now.\n", sess.ForcedTool)
	}
	b.WriteString("Tools not listed above are unavailable and cannot be called.\n")
	b.WriteString("======================================")
	return b.String()
}

// ResetStateCounters clears the per-state counters after a transition, so
// budgets and escalation apply per state rather than per session.
func ResetStateCounters(sess *Session) {
	sess.DirectToolCalls = 0
	sess.Violations = 0
	sess.ForcedTool = ""
}

// --- argument helpers -------------------------------------------------------

// bashArgv extracts the argv from a bash tool call. The bash tool accepts
// either a command string or an argv array.
func bashArgv(args string) ([]string, error) {
	var parsed struct {
		Command any `json:"command"`
	}
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return nil, err
	}
	switch v := parsed.Command.(type) {
	case string:
		return strings.Fields(v), nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("command array must contain strings")
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("command missing")
	}
}

// editPath extracts the target path from an edit or write call.
func editPath(args string) (string, error) {
	var parsed struct {
		Path     string `json:"path"`
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return "", err
	}
	if parsed.Path != "" {
		return parsed.Path, nil
	}
	if parsed.FilePath != "" {
		return parsed.FilePath, nil
	}
	return "", fmt.Errorf("no path in arguments")
}

// minObjectiveLength is the shortest delegation objective accepted. A weak
// model will otherwise delegate with "fix it", which defeats the point.
const minObjectiveLength = 20

// validateDelegation rejects a delegation that has not decomposed the work,
// returning "" when the call is acceptable.
func validateDelegation(args string) string {
	var parsed struct {
		Subagent           string   `json:"subagent"`
		Objective          string   `json:"objective"`
		AcceptanceCriteria []string `json:"acceptance_criteria"`
	}
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return "delegation arguments are not valid JSON"
	}
	if strings.TrimSpace(parsed.Subagent) == "" {
		return "delegation must name a subagent"
	}
	if len(strings.TrimSpace(parsed.Objective)) < minObjectiveLength {
		return fmt.Sprintf("delegation objective must be at least %d characters and describe the work concretely", minObjectiveLength)
	}
	if len(parsed.AcceptanceCriteria) == 0 {
		return "delegation must state at least one observable acceptance criterion"
	}
	return ""
}
