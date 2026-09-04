package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lmurphy/agentwarden/internal/enforce"
	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/tool"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

// Actor identifies the agent calling a workflow tool. It is supplied by the
// runtime, never by the model, so a model cannot claim to be another role.
type Actor struct {
	AgentID string
	TaskID  string
}

// errResult renders a failure the model can act on.
func errResult(format string, args ...any) (tool.Result, error) {
	return tool.Result{Content: fmt.Sprintf(format, args...), IsError: true}, nil
}

func objectSchema(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// SubmitPlanTool records a plan and advances the workflow.
type SubmitPlanTool struct {
	Ctl   *Controller
	Actor *Actor
}

func (SubmitPlanTool) Def() provider.ToolDef {
	return provider.ToolDef{
		Name:        enforce.ToolSubmitPlan,
		Description: "Submit the plan for this task and hand off to implementation.",
		Parameters: objectSchema(map[string]any{
			"plan": map[string]any{"type": "string", "description": "The plan, in enough detail to implement."},
			"acceptance_criteria": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Observable criteria that decide whether the work is done.",
			},
		}, "plan", "acceptance_criteria"),
	}
}

func (t SubmitPlanTool) Run(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args struct {
		Plan               string   `json:"plan"`
		AcceptanceCriteria []string `json:"acceptance_criteria"`
	}
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		return errResult("invalid arguments: %v", err)
	}
	if len(args.AcceptanceCriteria) == 0 {
		return errResult("a plan must state at least one observable acceptance criterion")
	}
	task, err := t.Ctl.SubmitPlan(ctx, t.Actor.TaskID, t.Actor.AgentID, args.Plan, args.AcceptanceCriteria)
	if err != nil {
		return errResult("%v", err)
	}
	return tool.Result{Content: fmt.Sprintf("plan accepted; the task is now %s", task.State)}, nil
}

// SubmitImplementationTool records a handoff and moves to verification.
type SubmitImplementationTool struct {
	Ctl   *Controller
	Actor *Actor
}

func (SubmitImplementationTool) Def() provider.ToolDef {
	return provider.ToolDef{
		Name: enforce.ToolSubmitImplementation,
		Description: "Submit the completed implementation. " +
			"The runtime will then run the required verification gates itself.",
		Parameters: objectSchema(map[string]any{
			"summary": map[string]any{"type": "string", "description": "What changed and why."},
			"files_changed": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Every file actually written. The runtime checks the work tree changed.",
			},
		}, "summary", "files_changed"),
	}
}

func (t SubmitImplementationTool) Run(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args struct {
		Summary      string   `json:"summary"`
		FilesChanged []string `json:"files_changed"`
	}
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		return errResult("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Summary) == "" {
		return errResult("a summary of the change is required")
	}
	task, err := t.Ctl.SubmitImplementation(ctx, t.Actor.TaskID, t.Actor.AgentID, args.Summary, args.FilesChanged)
	if err != nil {
		return errResult("%v", err)
	}
	// The gates run outside this call so a long suite cannot block a single
	// tool result for its whole timeout.
	return tool.Result{Content: fmt.Sprintf(
		"implementation recorded; the task is now %s and the runtime will run the required gates",
		task.State)}, nil
}

// SubmitQATool records a reviewer's verdict.
type SubmitQATool struct {
	Ctl   *Controller
	Actor *Actor
}

func (SubmitQATool) Def() provider.ToolDef {
	return provider.ToolDef{
		Name:        enforce.ToolSubmitQA,
		Description: "Record your review verdict for this task.",
		Parameters: objectSchema(map[string]any{
			"verdict": map[string]any{
				"type": "string",
				"enum": []string{enforce.VerdictApproved, enforce.VerdictRejected},
			},
			"notes": map[string]any{"type": "string", "description": "What you checked and what you found."},
		}, "verdict", "notes"),
	}
}

func (t SubmitQATool) Run(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args struct {
		Verdict string `json:"verdict"`
		Notes   string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		return errResult("invalid arguments: %v", err)
	}
	task, err := t.Ctl.SubmitQA(ctx, t.Actor.TaskID, t.Actor.AgentID, args.Verdict, args.Notes)
	if err != nil {
		return errResult("%v", err)
	}
	return tool.Result{Content: fmt.Sprintf("verdict recorded; the task is now %s", task.State)}, nil
}

// CompleteTool finishes a task, subject to gate and QA evidence.
type CompleteTool struct {
	Ctl   *Controller
	Actor *Actor
}

func (CompleteTool) Def() provider.ToolDef {
	return provider.ToolDef{
		Name: enforce.ToolComplete,
		Description: "Mark the task complete. " +
			"This is refused unless every required gate has passed against the current working tree.",
		Parameters: objectSchema(map[string]any{}),
	}
}

func (t CompleteTool) Run(ctx context.Context, _ tool.Call) (tool.Result, error) {
	task, err := t.Ctl.Complete(ctx, t.Actor.TaskID, t.Actor.AgentID)
	if err != nil {
		return errResult("%v", err)
	}
	return tool.Result{Content: fmt.Sprintf("task %s is complete", task.ID)}, nil
}

// StatusTool reports the authoritative task state.
type StatusTool struct {
	Ctl   *Controller
	Actor *Actor
}

func (StatusTool) Def() provider.ToolDef {
	return provider.ToolDef{
		Name:        enforce.ToolStatus,
		Description: "Show the authoritative state of the current task, including gate evidence.",
		Parameters:  objectSchema(map[string]any{}),
	}
}

func (t StatusTool) Run(_ context.Context, _ tool.Call) (tool.Result, error) {
	task, err := t.Ctl.Get(t.Actor.TaskID)
	if err != nil {
		return errResult("%v", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "task: %s\nstate: %s\nobjective: %s\n", task.ID, task.State, task.Objective)
	if task.Plan != "" {
		fmt.Fprintf(&b, "\nplan:\n%s\n", task.Plan)
	}
	if task.Handoff != "" {
		fmt.Fprintf(&b, "\nimplementation handoff:\n%s\n", task.Handoff)
	}
	b.WriteString("\n" + Evidence(task))
	if task.QA != nil {
		fmt.Fprintf(&b, "\nQA verdict: %s by %s\n%s\n", task.QA.Verdict, task.QA.Actor, task.QA.Notes)
	}
	return tool.Result{Content: b.String()}, nil
}

// HistoryTool reports the audit trail.
type HistoryTool struct {
	Ctl   *Controller
	Actor *Actor
}

func (HistoryTool) Def() provider.ToolDef {
	return provider.ToolDef{
		Name:        enforce.ToolHistory,
		Description: "Show the transition history of the current task.",
		Parameters:  objectSchema(map[string]any{}),
	}
}

func (t HistoryTool) Run(_ context.Context, _ tool.Call) (tool.Result, error) {
	task, err := t.Ctl.Get(t.Actor.TaskID)
	if err != nil {
		return errResult("%v", err)
	}
	if len(task.Events) == 0 {
		return tool.Result{Content: "no transitions recorded yet"}, nil
	}
	var b strings.Builder
	for _, event := range task.Events {
		fmt.Fprintf(&b, "%d. %s: %s -> %s (%s)\n",
			event.Sequence, event.Action, event.From, event.To, event.Actor)
		if reason := event.Metadata["reason"]; reason != "" {
			fmt.Fprintf(&b, "   reason: %s\n", reason)
		}
	}
	return tool.Result{Content: b.String()}, nil
}

// BlockTool parks a task with a reason.
type BlockTool struct {
	Ctl   *Controller
	Actor *Actor
}

func (BlockTool) Def() provider.ToolDef {
	return provider.ToolDef{
		Name:        enforce.ToolBlock,
		Description: "Park this task because it cannot proceed, stating why.",
		Parameters: objectSchema(map[string]any{
			"reason": map[string]any{"type": "string"},
		}, "reason"),
	}
}

func (t BlockTool) Run(_ context.Context, call tool.Call) (tool.Result, error) {
	var args struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		return errResult("invalid arguments: %v", err)
	}
	task, err := t.Ctl.Block(t.Actor.TaskID, t.Actor.AgentID, args.Reason)
	if err != nil {
		return errResult("%v", err)
	}
	return tool.Result{Content: fmt.Sprintf("task %s is blocked", task.ID)}, nil
}

// Register adds every workflow tool to a registry.
func Register(r *tool.Registry, ctl *Controller, actor *Actor) {
	r.Add(SubmitPlanTool{Ctl: ctl, Actor: actor})
	r.Add(SubmitImplementationTool{Ctl: ctl, Actor: actor})
	r.Add(SubmitQATool{Ctl: ctl, Actor: actor})
	r.Add(CompleteTool{Ctl: ctl, Actor: actor})
	r.Add(StatusTool{Ctl: ctl, Actor: actor})
	r.Add(HistoryTool{Ctl: ctl, Actor: actor})
	r.Add(BlockTool{Ctl: ctl, Actor: actor})
}

var (
	_ tool.Tool = SubmitPlanTool{}
	_ tool.Tool = SubmitImplementationTool{}
	_ tool.Tool = SubmitQATool{}
	_ tool.Tool = CompleteTool{}
	_ tool.Tool = StatusTool{}
	_ tool.Tool = HistoryTool{}
	_ tool.Tool = BlockTool{}
	_           = workflow.StatePlanning
)
