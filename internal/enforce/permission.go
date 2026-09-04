package enforce

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Effect is the outcome of a permission check.
type Effect string

const (
	// EffectAllow runs the call without asking.
	EffectAllow Effect = "allow"
	// EffectAsk requires human confirmation.
	EffectAsk Effect = "ask"
	// EffectDeny refuses the call outright.
	EffectDeny Effect = "deny"
)

// Rule is one ordered permission rule. Later rules win, so a broad deny can be
// narrowed by a specific allow placed after it — the same ordering your
// existing qa-engineer agent relies on.
type Rule struct {
	Action   string `yaml:"action" json:"action"`
	Resource string `yaml:"resource" json:"resource"`
	Effect   Effect `yaml:"effect" json:"effect"`
}

// Actions a rule can name.
const (
	ActionEdit     = "edit"
	ActionShell    = "shell"
	ActionSubagent = "subagent"
	ActionWebfetch = "webfetch"
)

// Permissions evaluates rules for a session. Mode auto short-circuits the ask
// effect, but never overrides an explicit deny.
type Permissions struct {
	rules []Rule
	// auto turns every EffectAsk into EffectAllow, which is what --auto does.
	auto bool
}

// NewPermissions returns a Permissions with the given ordered rules.
func NewPermissions(rules []Rule, auto bool) *Permissions {
	return &Permissions{rules: rules, auto: auto}
}

// SetAuto toggles auto-approval at runtime, backing the /auto TUI command.
func (p *Permissions) SetAuto(auto bool) { p.auto = auto }

// Auto reports whether auto-approval is on.
func (p *Permissions) Auto() bool { return p.auto }

// actionForTool maps a tool name onto the permission action it falls under.
func actionForTool(toolName string) string {
	switch toolName {
	case ToolEdit, ToolWrite:
		return ActionEdit
	case ToolBash:
		return ActionShell
	case ToolTask:
		return ActionSubagent
	default:
		// Read-only tools need no permission.
		return ""
	}
}

// Evaluate resolves the effect for an action against a resource. The default
// for an unmatched edit or shell action is EffectAsk, so a missing rule errs
// toward asking rather than silently permitting.
func (p *Permissions) Evaluate(action, resource string) Effect {
	if action == "" {
		return EffectAllow
	}

	effect := EffectAsk
	for _, rule := range p.rules {
		if rule.Action != action {
			continue
		}
		if matchResource(rule.Resource, resource) {
			effect = rule.Effect
		}
	}

	// Auto-approval upgrades an ask, but an explicit deny still stands: a
	// rule the user wrote outranks a convenience flag.
	if effect == EffectAsk && p.auto {
		return EffectAllow
	}
	return effect
}

// EvaluateTool resolves the effect for a tool call.
func (p *Permissions) EvaluateTool(toolName, resource string) Effect {
	return p.Evaluate(actionForTool(toolName), resource)
}

// matchResource matches a rule pattern against a concrete resource. "*" matches
// everything; a trailing "*" is a prefix match; otherwise filepath.Match
// semantics apply, falling back to equality for patterns it rejects.
func matchResource(pattern, resource string) bool {
	switch {
	case pattern == "" || pattern == "*":
		return true
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(resource, strings.TrimSuffix(pattern, "*"))
	default:
		if ok, err := filepath.Match(pattern, resource); err == nil && ok {
			return true
		}
		return pattern == resource
	}
}

// Describe renders a permission outcome for the UI.
func Describe(action, resource string, effect Effect) string {
	return fmt.Sprintf("%s %s -> %s", action, resource, effect)
}
