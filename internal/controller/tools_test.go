package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/lmurphy/agentwarden/internal/enforce"
	"github.com/lmurphy/agentwarden/internal/tool"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

// toolHarness pairs a controller with a registry of its workflow tools.
type toolHarness struct {
	*harness
	registry *tool.Registry
	actor    *Actor
	taskID   string
}

func newToolHarness(t *testing.T) *toolHarness {
	t.Helper()
	h := newHarness(t, policyDoc)
	task, err := h.ctl.Start(context.Background(), "fix the thing")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	actor := &Actor{AgentID: "tech-lead", TaskID: task.ID}
	registry := tool.NewRegistry()
	Register(registry, h.ctl, actor)
	return &toolHarness{harness: h, registry: registry, actor: actor, taskID: task.ID}
}

// editTree simulates the implementing stage actually writing something, which
// the baseline check requires before an implementation can be submitted.
func (h *toolHarness) editTree() {
	h.finger.current = workflow.Fingerprint{
		Head:   h.finger.current.Head,
		Digest: h.finger.current.Digest + "+",
	}
}

// call invokes a registered tool by name.
func (h *toolHarness) call(t *testing.T, name, args string) tool.Result {
	t.Helper()
	impl, ok := h.registry.Get(name)
	if !ok {
		t.Fatalf("tool %q is not registered", name)
	}
	result, err := impl.Run(context.Background(), tool.Call{ID: "c1", Name: name, Args: args})
	if err != nil {
		t.Fatalf("%s returned a harness error: %v", name, err)
	}
	return result
}

func TestRegisterExposesEveryWorkflowTool(t *testing.T) {
	h := newToolHarness(t)
	want := []string{
		enforce.ToolSubmitPlan,
		enforce.ToolSubmitImplementation,
		enforce.ToolSubmitQA,
		enforce.ToolComplete,
		enforce.ToolStatus,
		enforce.ToolHistory,
		enforce.ToolBlock,
	}
	for _, name := range want {
		if _, ok := h.registry.Get(name); !ok {
			t.Errorf("%s should be registered", name)
		}
	}
}

func TestWorkflowToolSchemasAreValid(t *testing.T) {
	h := newToolHarness(t)
	for _, name := range h.registry.Names() {
		impl, _ := h.registry.Get(name)
		def := impl.Def()
		t.Run(def.Name, func(t *testing.T) {
			if def.Description == "" {
				t.Error("a tool needs a description")
			}
			if def.Parameters["type"] != "object" {
				t.Errorf("parameters should be an object schema: %#v", def.Parameters)
			}
		})
	}
}

func TestSubmitPlanToolAdvancesWorkflow(t *testing.T) {
	h := newToolHarness(t)

	result := h.call(t, enforce.ToolSubmitPlan,
		`{"plan":"change the operator","acceptance_criteria":["go test passes"]}`)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	if !strings.Contains(result.Content, string(workflow.StateImplementing)) {
		t.Errorf("the result should report the new state: %q", result.Content)
	}

	task, err := h.ctl.Get(h.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != workflow.StateImplementing {
		t.Errorf("state = %s", task.State)
	}
	if !strings.Contains(task.Plan, "go test passes") {
		t.Errorf("the acceptance criteria should be recorded: %q", task.Plan)
	}
}

// TestSubmitPlanToolRequiresCriteria: the schema is what forces a weak model
// to state how success will be judged.
func TestSubmitPlanToolRequiresCriteria(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantWord string
	}{
		{"no criteria", `{"plan":"just do it"}`, "acceptance criterion"},
		{"empty criteria", `{"plan":"just do it","acceptance_criteria":[]}`, "acceptance criterion"},
		{"empty plan", `{"plan":"","acceptance_criteria":["x"]}`, "empty"},
		{"malformed", `{"plan":`, "invalid arguments"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newToolHarness(t)
			result := h.call(t, enforce.ToolSubmitPlan, tc.args)
			if !result.IsError {
				t.Fatal("expected a rejection")
			}
			if !strings.Contains(result.Content, tc.wantWord) {
				t.Errorf("content = %q, want it to mention %q", result.Content, tc.wantWord)
			}
		})
	}
}

// TestWrongActorSurfacesAsToolError: the refusal must reach the model as a
// result it can act on, not as a crash.
func TestWrongActorSurfacesAsToolError(t *testing.T) {
	h := newToolHarness(t)
	h.actor.AgentID = "engineer" // not the configured planner

	result := h.call(t, enforce.ToolSubmitPlan,
		`{"plan":"a plan","acceptance_criteria":["x"]}`)
	if !result.IsError {
		t.Fatal("the wrong actor should be refused")
	}
	if !strings.Contains(result.Content, "tech-lead") {
		t.Errorf("the refusal should name the authorized role: %q", result.Content)
	}
}

func TestSubmitImplementationTool(t *testing.T) {
	h := newToolHarness(t)
	h.call(t, enforce.ToolSubmitPlan, `{"plan":"p","acceptance_criteria":["x"]}`)
	h.actor.AgentID = "engineer"
	h.editTree()

	result := h.call(t, enforce.ToolSubmitImplementation,
		`{"summary":"changed the operator","files_changed":["add.go"]}`)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	// The message must tell the model the runtime will verify, so it does not
	// try to claim the gates passed itself.
	if !strings.Contains(result.Content, "gates") {
		t.Errorf("the result should say the runtime will run the gates: %q", result.Content)
	}

	task, _ := h.ctl.Get(h.taskID)
	if task.State != workflow.StateVerifying {
		t.Errorf("state = %s, want verifying", task.State)
	}
}

func TestSubmitImplementationToolRequiresSummary(t *testing.T) {
	h := newToolHarness(t)
	h.call(t, enforce.ToolSubmitPlan, `{"plan":"p","acceptance_criteria":["x"]}`)
	h.actor.AgentID = "engineer"

	if result := h.call(t, enforce.ToolSubmitImplementation, `{"summary":"  "}`); !result.IsError {
		t.Error("an empty summary should be refused")
	}
}

func TestSubmitQAToolRecordsVerdict(t *testing.T) {
	h := newToolHarness(t)
	h.call(t, enforce.ToolSubmitPlan, `{"plan":"p","acceptance_criteria":["x"]}`)
	h.actor.AgentID = "engineer"
	h.editTree()
	h.call(t, enforce.ToolSubmitImplementation, `{"summary":"done","files_changed":["add.go"]}`)
	if outcome := h.ctl.Verify(context.Background(), h.taskID); outcome.Error != nil {
		t.Fatalf("Verify: %v", outcome.Error)
	}
	h.actor.AgentID = "qa-engineer"

	result := h.call(t, enforce.ToolSubmitQA, `{"verdict":"approved","notes":"checked the diff"}`)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	task, _ := h.ctl.Get(h.taskID)
	if task.State != workflow.StateReadyToComplete {
		t.Errorf("state = %s", task.State)
	}
	if task.QA == nil || task.QA.Notes != "checked the diff" {
		t.Errorf("the verdict should be recorded: %+v", task.QA)
	}
}

func TestSubmitQAToolRejectsUnknownVerdict(t *testing.T) {
	h := newToolHarness(t)
	if result := h.call(t, enforce.ToolSubmitQA, `{"verdict":"probably","notes":"n"}`); !result.IsError {
		t.Error("an unknown verdict should be refused")
	}
}

// TestCompleteToolRefusedWithoutEvidence is the guarantee, seen from the
// model's side.
func TestCompleteToolRefusedWithoutEvidence(t *testing.T) {
	h := newToolHarness(t)
	h.actor.AgentID = "orchestrator"

	result := h.call(t, enforce.ToolComplete, `{}`)
	if !result.IsError {
		t.Fatal("completion without evidence must be refused")
	}
	if !strings.Contains(result.Content, "gate") {
		t.Errorf("the refusal should mention the missing gate evidence: %q", result.Content)
	}
}

func TestCompleteToolSucceedsWithEvidence(t *testing.T) {
	h := newToolHarness(t)
	h.call(t, enforce.ToolSubmitPlan, `{"plan":"p","acceptance_criteria":["x"]}`)
	h.actor.AgentID = "engineer"
	h.editTree()
	h.call(t, enforce.ToolSubmitImplementation, `{"summary":"done","files_changed":["add.go"]}`)
	h.ctl.Verify(context.Background(), h.taskID)
	h.actor.AgentID = "qa-engineer"
	h.call(t, enforce.ToolSubmitQA, `{"verdict":"approved","notes":"ok"}`)
	h.actor.AgentID = "orchestrator"

	result := h.call(t, enforce.ToolComplete, `{}`)
	if result.IsError {
		t.Fatalf("completion should be permitted: %s", result.Content)
	}
	task, _ := h.ctl.Get(h.taskID)
	if task.State != workflow.StateComplete {
		t.Errorf("state = %s", task.State)
	}
}

func TestStatusToolReportsAuthoritativeState(t *testing.T) {
	h := newToolHarness(t)
	h.call(t, enforce.ToolSubmitPlan, `{"plan":"the plan text","acceptance_criteria":["criterion one"]}`)

	result := h.call(t, enforce.ToolStatus, `{}`)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	for _, want := range []string{"implementing", "fix the thing", "the plan text", "criterion one"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("status should include %q:\n%s", want, result.Content)
		}
	}
}

func TestHistoryToolReportsTransitions(t *testing.T) {
	h := newToolHarness(t)

	if result := h.call(t, enforce.ToolHistory, `{}`); !strings.Contains(result.Content, "no transitions") {
		t.Errorf("a fresh task should report no history: %q", result.Content)
	}

	h.call(t, enforce.ToolSubmitPlan, `{"plan":"p","acceptance_criteria":["x"]}`)
	result := h.call(t, enforce.ToolHistory, `{}`)
	for _, want := range []string{"plan_submitted", "planning", "implementing", "tech-lead"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("history should include %q:\n%s", want, result.Content)
		}
	}
}

func TestBlockToolRequiresReason(t *testing.T) {
	h := newToolHarness(t)
	if result := h.call(t, enforce.ToolBlock, `{"reason":""}`); !result.IsError {
		t.Error("blocking without a reason should be refused")
	}

	result := h.call(t, enforce.ToolBlock, `{"reason":"the API spec is ambiguous"}`)
	if result.IsError {
		t.Fatalf("the planner owns the planning stage: %s", result.Content)
	}
	task, _ := h.ctl.Get(h.taskID)
	if task.State != workflow.StateBlocked {
		t.Errorf("state = %s", task.State)
	}
	if task.ResumeState != workflow.StatePlanning {
		t.Errorf("the stage should be remembered for resume, got %q", task.ResumeState)
	}
}

func TestToolsReportMalformedArgumentsAsErrors(t *testing.T) {
	h := newToolHarness(t)
	for _, name := range []string{
		enforce.ToolSubmitPlan,
		enforce.ToolSubmitImplementation,
		enforce.ToolSubmitQA,
		enforce.ToolBlock,
	} {
		t.Run(name, func(t *testing.T) {
			result := h.call(t, name, `{not valid json`)
			if !result.IsError {
				t.Error("malformed arguments should be an error result")
			}
			if !strings.Contains(result.Content, "invalid arguments") {
				t.Errorf("content = %q", result.Content)
			}
		})
	}
}
