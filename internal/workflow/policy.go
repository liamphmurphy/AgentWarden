package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Default gate limits, applied when a gate omits them.
const (
	DefaultTimeoutSeconds = 900
	DefaultMaxOutputBytes = 1_000_000
)

// Gate is a verification command declared by the repository. Command is an
// argv array executed without a shell, so it cannot smuggle pipes or
// redirection; a gate that genuinely needs shell syntax must say so explicitly
// with ["sh", "-lc", "..."].
type Gate struct {
	ID             string   `yaml:"id" json:"id"`
	Command        []string `yaml:"command" json:"command"`
	TimeoutSeconds int      `yaml:"timeout_seconds" json:"timeoutSeconds"`
	MaxOutputBytes int      `yaml:"max_output_bytes" json:"maxOutputBytes"`
	Required       *bool    `yaml:"required" json:"required"`
	WorkDir        string   `yaml:"work_dir,omitempty" json:"workDir,omitempty"`
	RunAt          []State  `yaml:"run_at,omitempty" json:"runAt,omitempty"`
}

// IsRequired reports whether the gate blocks completion. Gates default to
// required; optional gates still execute and still report, they just do not
// block. (The plugin validated `required: false` but never ran such gates.)
func (g Gate) IsRequired() bool {
	return g.Required == nil || *g.Required
}

// StateRule describes one state in a custom machine, including the tool
// masking and escalation behavior the enforcer applies while in it.
type StateRule struct {
	Agent              string           `yaml:"agent,omitempty" json:"agent,omitempty"`
	AllowTools         []string         `yaml:"allow_tools,omitempty" json:"allowTools,omitempty"`
	DelegateTo         []string         `yaml:"delegate_to,omitempty" json:"delegateTo,omitempty"`
	MaxDirectToolCalls int              `yaml:"max_direct_tool_calls,omitempty" json:"maxDirectToolCalls,omitempty"`
	OnViolation        []string         `yaml:"on_violation,omitempty" json:"onViolation,omitempty"`
	On                 map[Action]State `yaml:"on,omitempty" json:"on,omitempty"`
}

// Route deterministically selects an agent before the model is consulted.
type Route struct {
	When  RouteMatch `yaml:"when" json:"when"`
	Agent string     `yaml:"agent" json:"agent"`
}

// RouteMatch is the condition half of a Route.
type RouteMatch struct {
	PathGlob string `yaml:"path_glob,omitempty" json:"pathGlob,omitempty"`
}

// Options are the coarse workflow switches.
type Options struct {
	RequirePlan          *bool `yaml:"require_plan" json:"requirePlan"`
	RequireIndependentQA *bool `yaml:"require_independent_qa" json:"requireIndependentQA"`
	// AutoAdvance lets the runtime drive stage handoffs unattended: it
	// launches the next role, runs gates and continues without waiting for a
	// human to ask it to. Off by default, so a stage boundary is a natural
	// place to inspect progress.
	AutoAdvance bool `yaml:"auto_advance" json:"autoAdvance"`
}

// Policy is the immutable governance input. It declares roles, gates and
// optionally states; it never stores live task state.
type Policy struct {
	Version        int                 `yaml:"version" json:"version"`
	Workflow       Options             `yaml:"workflow" json:"workflow"`
	SmallModelMode bool                `yaml:"small_model_mode" json:"smallModelMode"`
	Roles          map[Role]string     `yaml:"roles" json:"roles"`
	States         map[State]StateRule `yaml:"states,omitempty" json:"states,omitempty"`
	Gates          []Gate              `yaml:"gates" json:"gates"`
	Routes         []Route             `yaml:"routes,omitempty" json:"routes,omitempty"`

	// hash is computed at load time over the normalized policy.
	hash string
}

// Hash returns the policy fingerprint. Receipts are bound to it, so a
// semantically meaningful policy edit invalidates existing evidence while
// comment or key-order churn does not.
func (p *Policy) Hash() string { return p.hash }

// RequirePlan reports whether a plan stage is mandatory (default true).
func (p *Policy) RequirePlan() bool {
	return p.Workflow.RequirePlan == nil || *p.Workflow.RequirePlan
}

// RequireIndependentQA reports whether reviewer and implementer must differ
// (default true).
func (p *Policy) RequireIndependentQA() bool {
	return p.Workflow.RequireIndependentQA == nil || *p.Workflow.RequireIndependentQA
}

// AutoAdvance reports whether the runtime should drive stage handoffs itself.
func (p *Policy) AutoAdvance() bool { return p.Workflow.AutoAdvance }

// Gate returns the gate with the given ID.
func (p *Policy) Gate(id string) (Gate, bool) {
	for _, g := range p.Gates {
		if g.ID == id {
			return g, true
		}
	}
	return Gate{}, false
}

// RequiredGates returns only the gates that block completion.
func (p *Policy) RequiredGates() []Gate {
	out := make([]Gate, 0, len(p.Gates))
	for _, g := range p.Gates {
		if g.IsRequired() {
			out = append(out, g)
		}
	}
	return out
}

// GatesFor returns the gates that should run in a state. A gate with no
// run_at runs in the verifying state only.
func (p *Policy) GatesFor(s State) []Gate {
	out := make([]Gate, 0, len(p.Gates))
	for _, g := range p.Gates {
		if len(g.RunAt) == 0 {
			if s == StateVerifying {
				out = append(out, g)
			}
			continue
		}
		for _, at := range g.RunAt {
			if at == s {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

// AgentFor resolves a role to its configured agent ID.
func (p *Policy) AgentFor(r Role) string { return p.Roles[r] }

// Transitions builds the transition graph this policy implies: the custom
// graph when states are declared, otherwise the builtin one.
func (p *Policy) Transitions() map[State]map[Action]State {
	if len(p.States) == 0 {
		return builtinTransitions
	}
	out := make(map[State]map[Action]State, len(p.States))
	for state, rule := range p.States {
		byAction := make(map[Action]State, len(rule.On))
		for action, target := range rule.On {
			byAction[action] = target
		}
		out[state] = byAction
	}
	return out
}

// ParsePolicy decodes, normalizes, validates and hashes a policy document.
// Decoding is delegated to the caller-supplied unmarshal func so this package
// stays free of a YAML dependency and remains pure.
func ParsePolicy(r io.Reader, unmarshal func([]byte, any) error) (*Policy, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read policy: %w", err)
	}
	var p Policy
	if err := unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPolicy, err)
	}
	p.normalize()
	if err := p.validate(); err != nil {
		return nil, err
	}
	p.hash = p.computeHash()
	return &p, nil
}

// normalize applies defaults so the hash is computed over fully-resolved
// values and two documents differing only in omitted defaults hash alike.
func (p *Policy) normalize() {
	for i := range p.Gates {
		if p.Gates[i].TimeoutSeconds <= 0 {
			p.Gates[i].TimeoutSeconds = DefaultTimeoutSeconds
		}
		if p.Gates[i].MaxOutputBytes <= 0 {
			p.Gates[i].MaxOutputBytes = DefaultMaxOutputBytes
		}
		if p.Gates[i].Required == nil {
			required := true
			p.Gates[i].Required = &required
		}
	}
	if p.Workflow.RequirePlan == nil {
		v := true
		p.Workflow.RequirePlan = &v
	}
	if p.Workflow.RequireIndependentQA == nil {
		v := true
		p.Workflow.RequireIndependentQA = &v
	}
}

func (p *Policy) validate() error {
	if p.Version != 1 {
		return fmt.Errorf("%w: version must be 1, got %d", ErrInvalidPolicy, p.Version)
	}
	if len(p.Gates) == 0 {
		return fmt.Errorf("%w: at least one gate is required", ErrInvalidPolicy)
	}
	seen := make(map[string]bool, len(p.Gates))
	for _, g := range p.Gates {
		if g.ID == "" {
			return fmt.Errorf("%w: every gate needs an id", ErrInvalidPolicy)
		}
		if seen[g.ID] {
			return fmt.Errorf("%w: duplicate gate id %q", ErrInvalidPolicy, g.ID)
		}
		seen[g.ID] = true
		if len(g.Command) == 0 {
			return fmt.Errorf("%w: gate %q needs a command", ErrInvalidPolicy, g.ID)
		}
	}
	if len(p.RequiredGates()) == 0 {
		return fmt.Errorf("%w: at least one gate must be required", ErrInvalidPolicy)
	}

	// Custom states replace the builtin graph entirely, so they must be
	// self-consistent: every transition target has to exist.
	for state, rule := range p.States {
		for action, target := range rule.On {
			if _, ok := p.States[target]; !ok && !isBuiltinState(target) {
				return fmt.Errorf("%w: state %q action %q targets undeclared state %q",
					ErrInvalidPolicy, state, action, target)
			}
		}
	}

	// Independent QA is only meaningful if the two roles really differ.
	if p.RequireIndependentQA() {
		impl, rev := p.Roles[RoleImplementer], p.Roles[RoleReviewer]
		if impl != "" && impl == rev {
			return fmt.Errorf("%w: independent QA requires implementer (%q) and reviewer to differ",
				ErrInvalidPolicy, impl)
		}
	}
	return nil
}

func isBuiltinState(s State) bool {
	_, ok := builtinTransitions[s]
	return ok || s == StateComplete || s == StateCancelled
}

// computeHash hashes a deterministic rendering of the normalized policy.
// Rendering is explicit rather than reflective so a new field cannot silently
// drop out of the hash without a compile-time reminder here.
func (p *Policy) computeHash() string {
	var b strings.Builder
	b.WriteString("version=" + strconv.Itoa(p.Version) + "\n")
	b.WriteString("require_plan=" + strconv.FormatBool(p.RequirePlan()) + "\n")
	b.WriteString("require_independent_qa=" + strconv.FormatBool(p.RequireIndependentQA()) + "\n")
	b.WriteString("small_model_mode=" + strconv.FormatBool(p.SmallModelMode) + "\n")
	b.WriteString("auto_advance=" + strconv.FormatBool(p.Workflow.AutoAdvance) + "\n")

	roles := make([]string, 0, len(p.Roles))
	for role, agent := range p.Roles {
		roles = append(roles, string(role)+"="+agent)
	}
	sort.Strings(roles)
	b.WriteString("roles:" + strings.Join(roles, ",") + "\n")

	gates := make([]string, 0, len(p.Gates))
	for _, g := range p.Gates {
		gates = append(gates, fmt.Sprintf("%s|%s|%d|%d|%t|%s|%v",
			g.ID, strings.Join(g.Command, "\x00"), g.TimeoutSeconds,
			g.MaxOutputBytes, g.IsRequired(), g.WorkDir, g.RunAt))
	}
	sort.Strings(gates)
	b.WriteString("gates:" + strings.Join(gates, ",") + "\n")

	states := make([]string, 0, len(p.States))
	for state, rule := range p.States {
		transitions := make([]string, 0, len(rule.On))
		for action, target := range rule.On {
			transitions = append(transitions, string(action)+">"+string(target))
		}
		sort.Strings(transitions)
		allow := append([]string(nil), rule.AllowTools...)
		sort.Strings(allow)
		delegate := append([]string(nil), rule.DelegateTo...)
		sort.Strings(delegate)
		states = append(states, fmt.Sprintf("%s|%s|%s|%s|%d|%s|%s",
			state, rule.Agent, strings.Join(allow, "+"), strings.Join(delegate, "+"),
			rule.MaxDirectToolCalls, strings.Join(rule.OnViolation, "+"),
			strings.Join(transitions, "+")))
	}
	sort.Strings(states)
	b.WriteString("states:" + strings.Join(states, ",") + "\n")

	routes := make([]string, 0, len(p.Routes))
	for _, r := range p.Routes {
		routes = append(routes, r.When.PathGlob+">"+r.Agent)
	}
	sort.Strings(routes)
	b.WriteString("routes:" + strings.Join(routes, ",") + "\n")

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
