package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func parse(t *testing.T, doc string) (*Policy, error) {
	t.Helper()
	return ParsePolicy(strings.NewReader(doc), yaml.Unmarshal)
}

func mustParse(t *testing.T, doc string) *Policy {
	t.Helper()
	p, err := parse(t, doc)
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	return p
}

const validPolicy = `
version: 1
roles:
  orchestrator: orchestrator
  planner: tech-lead
  implementer: engineer
  reviewer: qa-engineer
gates:
  - id: unit
    command: ["go", "test", "./..."]
    required: true
`

func TestParseAppliesDefaults(t *testing.T) {
	p := mustParse(t, `
version: 1
roles:
  implementer: engineer
  reviewer: qa-engineer
gates:
  - id: unit
    command: ["go", "test", "./..."]
`)
	g := p.Gates[0]
	if g.TimeoutSeconds != DefaultTimeoutSeconds {
		t.Errorf("timeout default = %d, want %d", g.TimeoutSeconds, DefaultTimeoutSeconds)
	}
	if g.MaxOutputBytes != DefaultMaxOutputBytes {
		t.Errorf("output cap default = %d, want %d", g.MaxOutputBytes, DefaultMaxOutputBytes)
	}
	if !g.IsRequired() {
		t.Error("gates should default to required")
	}
	if !p.RequirePlan() || !p.RequireIndependentQA() {
		t.Error("workflow options should default to true")
	}
}

// TestHashIgnoresCosmeticChange is the property that makes receipts usable:
// reordering keys or adding comments must not invalidate passing evidence.
func TestHashIgnoresCosmeticChange(t *testing.T) {
	a := mustParse(t, validPolicy)
	b := mustParse(t, `
# a leading comment
version: 1
gates:
  - command: ["go", "test", "./..."]
    id: unit
    required: true
    timeout_seconds: 900
roles:
  reviewer: qa-engineer
  implementer: engineer
  planner: tech-lead
  orchestrator: orchestrator
`)
	if a.Hash() != b.Hash() {
		t.Errorf("cosmetically identical policies must hash alike:\n a=%s\n b=%s", a.Hash(), b.Hash())
	}
}

// TestHashDetectsSemanticChange is the other half: a real edit must invalidate.
func TestHashDetectsSemanticChange(t *testing.T) {
	base := mustParse(t, validPolicy)
	variants := map[string]string{
		"different command": strings.Replace(validPolicy, `["go", "test", "./..."]`, `["go", "test", "-race", "./..."]`, 1),
		"different timeout": validPolicy + "    timeout_seconds: 60\n",
		"now optional":      strings.Replace(validPolicy, "required: true", "required: false", 1) + "  - id: other\n    command: [\"true\"]\n    required: true\n",
		"different role":    strings.Replace(validPolicy, "implementer: engineer", "implementer: coder", 1),
		"small model mode":  validPolicy + "small_model_mode: true\n",
	}
	for name, doc := range variants {
		t.Run(name, func(t *testing.T) {
			p, err := parse(t, doc)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if p.Hash() == base.Hash() {
				t.Error("semantic change must alter the policy hash")
			}
		})
	}
}

func TestPolicyRejections(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{"wrong version", `
version: 2
gates:
  - id: unit
    command: ["true"]
`},
		{"no gates", `
version: 1
roles: {implementer: engineer, reviewer: qa}
gates: []
`},
		{"duplicate gate id", `
version: 1
gates:
  - id: unit
    command: ["true"]
  - id: unit
    command: ["false"]
`},
		{"gate without command", `
version: 1
gates:
  - id: unit
`},
		{"gate without id", `
version: 1
gates:
  - command: ["true"]
`},
		{"no required gate", `
version: 1
gates:
  - id: unit
    command: ["true"]
    required: false
`},
		{"reviewer equals implementer", `
version: 1
roles:
  implementer: engineer
  reviewer: engineer
gates:
  - id: unit
    command: ["true"]
`},
		{"custom state targets undeclared state", `
version: 1
roles: {implementer: engineer, reviewer: qa}
gates:
  - id: unit
    command: ["true"]
states:
  research:
    on: {plan_submitted: nowhere}
`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parse(t, tc.doc); err == nil {
				t.Fatal("expected rejection")
			} else if !errors.Is(err, ErrInvalidPolicy) {
				t.Fatalf("want ErrInvalidPolicy, got %v", err)
			}
		})
	}
}

// TestReviewerEqualsImplementerAllowedWithoutIndependentQA shows the check is
// conditional on the option, not absolute.
func TestReviewerEqualsImplementerAllowedWithoutIndependentQA(t *testing.T) {
	p := mustParse(t, `
version: 1
workflow:
  require_independent_qa: false
roles:
  implementer: engineer
  reviewer: engineer
gates:
  - id: unit
    command: ["true"]
`)
	if p.RequireIndependentQA() {
		t.Error("independent QA should be off")
	}
}

// TestOptionalGatesAreReturnedByGatesFor is fix #1: an optional gate is dead
// config in the plugin. It must still be scheduled to run.
func TestOptionalGatesAreReturnedByGatesFor(t *testing.T) {
	p := mustParse(t, `
version: 1
roles: {implementer: engineer, reviewer: qa}
gates:
  - id: unit
    command: ["true"]
    required: true
  - id: lint
    command: ["true"]
    required: false
`)
	scheduled := p.GatesFor(StateVerifying)
	if len(scheduled) != 2 {
		t.Fatalf("optional gates must still be scheduled, got %d of 2", len(scheduled))
	}
	if req := p.RequiredGates(); len(req) != 1 || req[0].ID != "unit" {
		t.Errorf("only unit should block completion, got %+v", req)
	}
}

func TestGatesForRespectsRunAt(t *testing.T) {
	p := mustParse(t, `
version: 1
roles: {implementer: engineer, reviewer: qa}
gates:
  - id: unit
    command: ["true"]
    run_at: [verifying, ready_to_complete]
  - id: smoke
    command: ["true"]
    run_at: [ready_to_complete]
  - id: implicit
    command: ["true"]
`)
	byState := map[State][]string{
		StateVerifying:       {"unit", "implicit"},
		StateReadyToComplete: {"unit", "smoke"},
		StatePlanning:        {},
	}
	for state, want := range byState {
		got := p.GatesFor(state)
		if len(got) != len(want) {
			t.Errorf("%s: got %d gates, want %d", state, len(got), len(want))
			continue
		}
		for i, id := range want {
			if got[i].ID != id {
				t.Errorf("%s[%d]: got %s, want %s", state, i, got[i].ID, id)
			}
		}
	}
}

func TestTransitionsUsesBuiltinWhenNoStatesDeclared(t *testing.T) {
	p := mustParse(t, validPolicy)
	if got := p.Transitions()[StatePlanning][ActionPlanSubmitted]; got != StateImplementing {
		t.Errorf("want builtin graph, got planning/plan_submitted -> %q", got)
	}
}

func TestTransitionsUsesCustomStatesWhenDeclared(t *testing.T) {
	p := mustParse(t, `
version: 1
roles: {implementer: engineer, reviewer: qa}
gates:
  - id: unit
    command: ["true"]
states:
  research:
    agent: researcher
    allow_tools: [read, grep]
    on: {plan_submitted: planning}
  planning:
    on: {cancelled: cancelled}
`)
	tr := p.Transitions()
	if got := tr["research"][ActionPlanSubmitted]; got != StatePlanning {
		t.Errorf("custom edge missing, got %q", got)
	}
	if _, leaked := tr[StatePlanning][ActionPlanSubmitted]; leaked {
		t.Error("builtin edges must not leak into a custom graph")
	}
	if rule := p.States["research"]; rule.Agent != "researcher" || len(rule.AllowTools) != 2 {
		t.Errorf("state rule not parsed: %+v", rule)
	}
}

func TestAgentForAndGateLookup(t *testing.T) {
	p := mustParse(t, validPolicy)
	if got := p.AgentFor(RoleImplementer); got != "engineer" {
		t.Errorf("AgentFor(implementer) = %q, want engineer", got)
	}
	if _, ok := p.Gate("unit"); !ok {
		t.Error("gate unit should be found")
	}
	if _, ok := p.Gate("nope"); ok {
		t.Error("unknown gate should not be found")
	}
}

// TestShippedExamplePolicyIsValid guards the example the README tells people
// to copy.
func TestShippedExamplePolicyIsValid(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "workflow.yml"))
	if err != nil {
		t.Fatalf("read example policy: %v", err)
	}
	p, err := ParsePolicy(strings.NewReader(string(raw)), yaml.Unmarshal)
	if err != nil {
		t.Fatalf("the shipped example policy must be valid: %v", err)
	}

	if !p.SmallModelMode {
		t.Error("the example should demonstrate small_model_mode")
	}
	for role, agent := range map[Role]string{
		RoleOrchestrator: "orchestrator",
		RolePlanner:      "tech-lead",
		RoleImplementer:  "engineer",
		RoleReviewer:     "qa-engineer",
	} {
		if got := p.AgentFor(role); got != agent {
			t.Errorf("role %s = %q, want %q", role, got, agent)
		}
	}
	// It should demonstrate both a blocking and a non-blocking gate.
	if len(p.RequiredGates()) == 0 {
		t.Error("the example needs a required gate")
	}
	if len(p.Gates) == len(p.RequiredGates()) {
		t.Error("the example should also show an optional gate")
	}
}
