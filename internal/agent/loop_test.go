package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lmurphy/agentwarden/internal/enforce"
	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/provider/fake"
	"github.com/lmurphy/agentwarden/internal/tool"
	"github.com/lmurphy/agentwarden/internal/workflow"
	"gopkg.in/yaml.v3"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time {
	c.t = c.t.Add(time.Second)
	return c.t
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

const testPolicy = `
version: 1
roles:
  orchestrator: orchestrator
  planner: tech-lead
  implementer: engineer
  reviewer: qa-engineer
gates:
  - id: unit
    command: ["true"]
    required: true
`

func mustPolicy(t *testing.T, doc string) *workflow.Policy {
	t.Helper()
	p, err := workflow.ParsePolicy(strings.NewReader(doc), yaml.Unmarshal)
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	return p
}

// testTools registers the full surface so masking has something to subtract.
func testTools(t *testing.T) *tool.Registry {
	t.Helper()
	root := t.TempDir()
	r := tool.NewRegistry()
	r.Add(tool.Read{Root: root})
	r.Add(tool.Write{Root: root})
	r.Add(tool.Edit{Root: root})
	r.Add(tool.Bash{Root: root})
	r.Add(tool.Grep{Root: root})
	r.Add(tool.Glob{Root: root})
	r.Add(tool.LS{Root: root})
	r.Add(stubTool{name: enforce.ToolStatus})
	r.Add(stubTool{name: enforce.ToolHistory})
	return r
}

// withHandoffs adds handoff tools that advance the task, mirroring the real
// workflow tools. Without the transition the enforcer would correctly keep
// demanding a handoff that had in fact already happened.
func withHandoffs(r *tool.Registry, task *workflow.Task, machine *workflow.Machine) *tool.Registry {
	r.Add(handoffTool{
		name: enforce.ToolSubmitPlan, action: workflow.ActionPlanSubmitted,
		task: task, machine: machine,
	})
	r.Add(handoffTool{
		name: enforce.ToolSubmitImplementation, action: workflow.ActionImplementationSubmitted,
		task: task, machine: machine,
	})
	// The later stages need theirs too: a stage whose advancing tool is not
	// registered is one the model gets no turn in, so an incomplete fixture
	// would look like a stalled workflow.
	r.Add(handoffTool{
		name: enforce.ToolSubmitQA, action: workflow.ActionQAApproved,
		task: task, machine: machine,
	})
	r.Add(handoffTool{
		name: enforce.ToolComplete, action: workflow.ActionCompleted,
		task: task, machine: machine,
	})
	return r
}

// handoffTool advances the workflow when called, as the real handoff tools do.
type handoffTool struct {
	name    string
	action  workflow.Action
	task    *workflow.Task
	machine *workflow.Machine
}

func (h handoffTool) Def() provider.ToolDef {
	return provider.ToolDef{
		Name:        h.name,
		Description: "submit " + h.name,
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (h handoffTool) Run(context.Context, tool.Call) (tool.Result, error) {
	if err := h.machine.Transition(h.task, h.action, "actor", nil); err != nil {
		return tool.Result{Content: err.Error(), IsError: true}, nil
	}
	return tool.Result{Content: "accepted, now " + string(h.task.State)}, nil
}

// stubTool stands in for a workflow handoff tool.
type stubTool struct {
	name  string
	reply string
}

func (s stubTool) Def() provider.ToolDef {
	return provider.ToolDef{
		Name:        s.name,
		Description: "stub " + s.name,
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (s stubTool) Run(context.Context, tool.Call) (tool.Result, error) {
	reply := s.reply
	if reply == "" {
		reply = "ok"
	}
	return tool.Result{Content: reply}, nil
}

// recordingObserver captures what the UI would have been told.
type recordingObserver struct {
	NopObserver
	blocked []enforce.Decision
	ran     []string
}

func (o *recordingObserver) Blocked(_ tool.Call, d enforce.Decision) {
	o.blocked = append(o.blocked, d)
}

func (o *recordingObserver) ToolStarted(call tool.Call) {
	o.ran = append(o.ran, call.Name)
}

// TestEnforcementEndToEnd is the headline scenario, fully deterministic with
// no network and no model:
//
//  1. the model tries to edit while planning and is hard-blocked,
//  2. the model complies with the required handoff,
//  3. a masked tool never appears in the payload at all.
func TestEnforcementEndToEnd(t *testing.T) {
	policy := mustPolicy(t, testPolicy)
	machine := workflow.NewMachineWithTransitions(policy.Transitions(), newClock())
	governor := enforce.New(policy, machine, ".agentwarden/workflow.yml")

	task := &workflow.Task{
		ID: "t1", State: workflow.StatePlanning,
		PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
	}

	// Turn 1: the model reaches for edit, which planning does not permit.
	// Turn 2: it complies and submits a plan.
	// Turn 3: it finishes.
	model := fake.New(
		fake.CallTurn("c1", enforce.ToolEdit, `{"path":"main.go","old_string":"a","new_string":"b"}`),
		fake.CallTurn("c2", enforce.ToolSubmitPlan, `{"plan":"do the work"}`),
		fake.TextTurn("Plan submitted."),
	)

	observer := &recordingObserver{}
	loop := &Loop{
		Provider: model,
		Model:    "test",
		Tools:    withHandoffs(testTools(t), task, machine),
		Governor: governor,
		Task:     task,
		Session:  &enforce.Session{Role: workflow.RolePlanner, AgentID: "tech-lead"},
		Observer: observer,
	}

	result, err := loop.Run(context.Background(), "write the plan")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The illegal edit was refused.
	if result.Blocked == 0 {
		t.Fatal("the edit attempt should have been blocked")
	}
	if len(observer.blocked) == 0 {
		t.Fatal("the UI should have been told about the block")
	}
	if !strings.Contains(observer.blocked[0].Correction, enforce.ToolSubmitPlan) {
		t.Errorf("the correction should name the required action: %q", observer.blocked[0].Correction)
	}
	// The edit must never have executed.
	for _, name := range observer.ran {
		if name == enforce.ToolEdit {
			t.Error("a blocked tool must not execute")
		}
	}

	requests := model.Requests()
	if len(requests) < 2 {
		t.Fatalf("want at least 2 requests, got %d", len(requests))
	}

	// Masking is structural: while the task is planning, edit is absent from
	// the payload rather than merely denied after the fact. The first two
	// requests are the planning turns; the plan is accepted during the
	// second, after which edit becomes legitimately available.
	for i, req := range requests[:2] {
		if fake.OffersTool(req, enforce.ToolEdit) {
			t.Errorf("planning request %d should not offer edit at all: %v", i, fake.ToolNames(req))
		}
		if fake.OffersTool(req, enforce.ToolWrite) {
			t.Errorf("planning request %d should not offer write: %v", i, fake.ToolNames(req))
		}
		if !fake.OffersTool(req, enforce.ToolRead) {
			t.Errorf("planning request %d should offer read: %v", i, fake.ToolNames(req))
		}
		if !fake.OffersTool(req, enforce.ToolSubmitPlan) {
			t.Errorf("planning request %d should offer the handoff: %v", i, fake.ToolNames(req))
		}
	}

	// Once the plan is accepted the state advances and write access opens up,
	// which is the same masking table working in the other direction.
	if len(requests) > 2 {
		if !fake.OffersTool(requests[2], enforce.ToolEdit) {
			t.Errorf("after the handoff, implementing should offer edit: %v", fake.ToolNames(requests[2]))
		}
	}

	// A single rejected call is the warning rung. It must not be counted a
	// second time as a final turn merely because the provider paused for the
	// tool result.
	second := requests[1]
	if second.ToolChoice != nil {
		t.Errorf("one violation should warn rather than pin tool_choice: %+v", second.ToolChoice)
	}

	// The banner restates the authoritative state every turn.
	banner := lastUserText(second)
	for _, want := range []string{"WORKFLOW STATE", "planning", enforce.ToolSubmitPlan} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner should mention %q:\n%s", want, banner)
		}
	}
	if model.Remaining() != 0 {
		t.Errorf("the scenario should consume every scripted turn, %d left", model.Remaining())
	}
}

// lastUserText returns the final user message of a request, which is where the
// banner is injected.
func lastUserText(req provider.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == provider.RoleUser {
			return req.Messages[i].Text
		}
	}
	return ""
}

// TestBlockedCallFeedsCorrectionBackToModel checks the refusal arrives in-band
// as a tool result, so the model can act on it rather than losing the turn.
func TestBlockedCallFeedsCorrectionBackToModel(t *testing.T) {
	policy := mustPolicy(t, testPolicy)
	governor := enforce.New(policy, workflow.NewMachine(newClock()), "")

	// The model attempts a write, is refused, then complies with the handoff.
	model := fake.New(
		fake.CallTurn("c1", enforce.ToolWrite, `{"path":"x.go","content":"y"}`),
		fake.CallTurn("c2", enforce.ToolSubmitPlan, `{"plan":"understood"}`),
		fake.TextTurn("understood"),
	)
	task := &workflow.Task{
		ID: "t1", State: workflow.StatePlanning,
		PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
	}
	machine := workflow.NewMachine(newClock())
	loop := &Loop{
		Provider: model,
		Model:    "test",
		Tools:    withHandoffs(testTools(t), task, machine),
		Governor: governor,
		Task:     task,
		Session:  &enforce.Session{},
	}

	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var toolResults []provider.Message
	for _, m := range loop.Messages() {
		if m.Role == provider.RoleTool {
			toolResults = append(toolResults, m)
		}
	}
	if len(toolResults) != 2 {
		t.Fatalf("want 2 tool results, got %d", len(toolResults))
	}
	if toolResults[0].ToolCallID != "c1" {
		t.Errorf("the result must answer the call it refused: %+v", toolResults[0])
	}
	if !strings.Contains(toolResults[0].Text, "BLOCKED") {
		t.Errorf("the model should see the refusal: %q", toolResults[0].Text)
	}
}

// TestCompletionRefusedUntilGatePasses is the guarantee the whole design
// exists for.
func TestCompletionRefusedUntilGatePasses(t *testing.T) {
	policy := mustPolicy(t, testPolicy)
	governor := enforce.New(policy, workflow.NewMachine(newClock()), "")
	current := workflow.Fingerprint{Head: "h1", Digest: "d1"}

	task := &workflow.Task{
		ID: "t1", State: workflow.StateReadyToComplete,
		PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
	}

	// No evidence: refused, and the refusal names the pending gate.
	decision := governor.OnComplete(task, current)
	if decision.Allow {
		t.Fatal("completion must be refused without gate evidence")
	}
	if !strings.Contains(decision.Correction, "unit") {
		t.Errorf("the refusal should name the gate: %q", decision.Correction)
	}

	// A failing receipt is still not evidence.
	code := 1
	task.Receipts["unit"] = workflow.Receipt{
		GateID: "unit", Success: false, ExitCode: &code,
		PolicyHash: policy.Hash(), Repository: current,
	}
	if governor.OnComplete(task, current).Allow {
		t.Error("a failing gate must not permit completion")
	}

	// Passing gate plus a fresh approval lets it through.
	zero := 0
	task.Receipts["unit"] = workflow.Receipt{
		GateID: "unit", Success: true, ExitCode: &zero,
		PolicyHash: policy.Hash(), Repository: current,
	}
	task.QA = &workflow.QA{
		Verdict: enforce.VerdictApproved, PolicyHash: policy.Hash(), Repository: current,
	}
	if d := governor.OnComplete(task, current); !d.Allow {
		t.Errorf("completion should be permitted: %s", d.Reason)
	}

	// An edit after approval reopens verification.
	moved := workflow.Fingerprint{Head: "h1", Digest: "d2"}
	if governor.OnComplete(task, moved).Allow {
		t.Error("an edit after approval must reopen verification")
	}
}

// TestTurnEndWithoutHandoffIsCorrected covers the case the plugin could not
// see at all, because OpenCode exposed no session-end hook.
func TestTurnEndWithoutHandoffIsCorrected(t *testing.T) {
	policy := mustPolicy(t, testPolicy)
	governor := enforce.New(policy, workflow.NewMachine(newClock()), "")

	// The model reads a file, then simply stops without submitting a plan.
	model := fake.New(
		fake.CallTurn("c1", enforce.ToolLS, `{"path":"."}`),
		fake.TextTurn("I had a look around."),
		fake.CallTurn("c2", enforce.ToolSubmitPlan, `{"plan":"now the plan"}`),
		fake.TextTurn("done"),
	)
	task := &workflow.Task{
		ID: "t1", State: workflow.StatePlanning,
		PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
	}
	loop := &Loop{
		Provider: model,
		Model:    "test",
		Tools:    withHandoffs(testTools(t), task, workflow.NewMachine(newClock())),
		Governor: governor,
		Task:     task,
		Session:  &enforce.Session{},
	}

	result, err := loop.Run(context.Background(), "look around")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Blocked == 0 {
		t.Error("stopping without a handoff should be caught")
	}

	// The correction is delivered and the model gets another turn.
	var corrected bool
	for _, m := range loop.Messages() {
		if m.Role == provider.RoleUser && strings.Contains(m.Text, enforce.ToolSubmitPlan) {
			corrected = true
			if !m.Internal {
				t.Error("the workflow correction should be marked internal")
			}
		}
	}
	if !corrected {
		t.Error("the model should be told which handoff is required")
	}
}

// Tool calls are intermediate model-loop rounds, not final turns. Treating
// each one as a missing handoff makes an implementer exhaust the three-rung
// violation ladder while it is still inspecting and editing the repository.
func TestIntermediateToolCallsDoNotConsumeHandoffViolations(t *testing.T) {
	policy := mustPolicy(t, testPolicy)
	machine := workflow.NewMachineWithTransitions(policy.Transitions(), newClock())
	task := &workflow.Task{
		ID: "t1", State: workflow.StateImplementing,
		PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
	}
	model := fake.New(
		fake.CallTurn("c1", enforce.ToolStatus, `{}`),
		fake.CallTurn("c2", enforce.ToolHistory, `{}`),
		fake.CallTurn("c3", enforce.ToolLS, `{"path":"."}`),
		fake.CallTurn("c4", enforce.ToolSubmitImplementation, `{"summary":"done"}`),
	)
	sess := &enforce.Session{Role: workflow.RoleImplementer, AgentID: "engineer"}
	loop := &Loop{
		Provider: model,
		Model:    "test",
		Tools:    withHandoffs(testTools(t), task, machine),
		Governor: enforce.New(policy, machine, ""),
		Task:     task,
		Session:  sess,
	}

	result, err := loop.Run(context.Background(), "implement it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Blocked != 0 {
		t.Errorf("intermediate tool work should not be blocked, got %d", result.Blocked)
	}
	if sess.Violations != 0 {
		t.Errorf("intermediate tool work consumed %d handoff violations", sess.Violations)
	}
	if task.State != workflow.StateVerifying {
		t.Errorf("handoff did not advance the task: %s", task.State)
	}
	if model.Remaining() != 0 {
		t.Errorf("the model was stopped before its handoff, %d turns remain", model.Remaining())
	}
}

// An OpenAI-compatible endpoint may emit only hidden reasoning when its
// served context is exhausted. Such a turn has no assistant artifact worth
// restoring and must not be retried until the generic 40-step limit.
func TestEmptyTurnsStopAtWorkflowEscalationWithoutGrowingHistory(t *testing.T) {
	policy := mustPolicy(t, testPolicy)
	model := fake.New(
		fake.TextTurn(""),
		fake.TextTurn(""),
		fake.TextTurn(""),
		fake.TextTurn("never reached"),
	)
	task := &workflow.Task{
		ID: "t1", State: workflow.StatePlanning,
		PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
	}
	loop := &Loop{
		Provider: model,
		Model:    "test",
		Tools:    withHandoffs(testTools(t), task, workflow.NewMachine(newClock())),
		Governor: enforce.New(policy, workflow.NewMachine(newClock()), ""),
		Task:     task,
		Session:  &enforce.Session{},
	}

	result, err := loop.Run(context.Background(), "make a plan")
	if err == nil {
		t.Fatal("empty governed turns should stop with a diagnostic")
	}
	for _, want := range []string{enforce.ToolSubmitPlan, "served context window", "neither assistant text nor a tool call"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q: %v", want, err)
		}
	}
	if result.Steps != 3 {
		t.Errorf("steps = %d, want the three-rung escalation", result.Steps)
	}
	if model.Remaining() != 1 {
		t.Errorf("the loop should stop before the step limit, %d turns remain", model.Remaining())
	}

	var assistants, corrections int
	for _, message := range loop.Messages() {
		switch {
		case message.Role == provider.RoleAssistant:
			assistants++
		case message.Role == provider.RoleUser && message.Internal:
			corrections++
		}
	}
	if assistants != 0 {
		t.Errorf("empty assistant turns should not be persisted, got %d", assistants)
	}
	if corrections != 1 {
		t.Errorf("identical corrections should be deduplicated, got %d", corrections)
	}
}

// TestUngovernedLoopImposesNothing backs --no-workflow, `agentwarden run` and
// /plain: a quick question should not pay for the state machine.
func TestUngovernedLoopImposesNothing(t *testing.T) {
	model := fake.New(
		fake.CallTurn("c1", enforce.ToolWrite, `{"path":"free.txt","content":"anything"}`),
		fake.TextTurn("wrote it"),
	)
	loop := &Loop{
		Provider: model,
		Model:    "test",
		Tools:    testTools(t),
		Governor: enforce.NewNop(),
		Session:  &enforce.Session{},
	}

	result, err := loop.Run(context.Background(), "write a file")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Blocked != 0 {
		t.Errorf("an ungoverned run should block nothing, got %d", result.Blocked)
	}
	if result.Text != "wrote it" {
		t.Errorf("text = %q", result.Text)
	}

	req := model.Requests()[0]
	// Every tool is offered, and no banner is injected.
	if !fake.OffersTool(req, enforce.ToolWrite) || !fake.OffersTool(req, enforce.ToolEdit) {
		t.Errorf("all tools should be offered: %v", fake.ToolNames(req))
	}
	if req.ToolChoice != nil {
		t.Error("the model should not be constrained")
	}
	if strings.Contains(lastUserText(req), "WORKFLOW STATE") {
		t.Error("no banner should be injected when ungoverned")
	}
}

// TestPermissionsDenyStopsExecution checks the permission layer is consulted
// separately from workflow masking.
func TestPermissionsDenyStopsExecution(t *testing.T) {
	model := fake.New(
		fake.CallTurn("c1", enforce.ToolBash, `{"command":"rm -rf /"}`),
		fake.TextTurn("ok"),
	)
	observer := &recordingObserver{}
	loop := &Loop{
		Provider: model,
		Model:    "test",
		Tools:    testTools(t),
		Governor: enforce.NewNop(),
		Permissions: enforce.NewPermissions([]enforce.Rule{
			{Action: enforce.ActionShell, Resource: "rm *", Effect: enforce.EffectDeny},
		}, false),
		Observer: observer,
		Session:  &enforce.Session{},
	}

	if _, err := loop.Run(context.Background(), "clean up"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, name := range observer.ran {
		if name == enforce.ToolBash {
			t.Error("a denied command must not execute")
		}
	}
	var sawDenial bool
	for _, m := range loop.Messages() {
		if m.Role == provider.RoleTool && strings.Contains(m.Text, "permission denied") {
			sawDenial = true
		}
	}
	if !sawDenial {
		t.Error("the model should be told the call was denied")
	}
}

// TestAutoModeApprovesWithoutPrompting is the --auto behavior: an unmatched
// action runs instead of asking.
func TestAutoModeApprovesWithoutPrompting(t *testing.T) {
	for _, auto := range []bool{false, true} {
		name := "ask"
		if auto {
			name = "auto"
		}
		t.Run(name, func(t *testing.T) {
			model := fake.New(
				fake.CallTurn("c1", enforce.ToolBash, `{"command":"echo hi"}`),
				fake.TextTurn("done"),
			)
			observer := &recordingObserver{}
			loop := &Loop{
				Provider: model,
				Model:    "test",
				Tools:    testTools(t),
				Governor: enforce.NewNop(),
				// No rule matches, so the effect is ask unless auto is on.
				Permissions: enforce.NewPermissions(nil, auto),
				Confirmer:   DenyConfirmer{},
				Observer:    observer,
				Session:     &enforce.Session{},
			}

			if _, err := loop.Run(context.Background(), "say hi"); err != nil {
				t.Fatalf("Run: %v", err)
			}

			ran := false
			for _, n := range observer.ran {
				if n == enforce.ToolBash {
					ran = true
				}
			}
			if ran != auto {
				t.Errorf("bash executed = %v, want %v (auto=%v)", ran, auto, auto)
			}
		})
	}
}

func TestUnknownToolIsReportedNotFatal(t *testing.T) {
	model := fake.New(
		fake.CallTurn("c1", "nonexistent_tool", `{}`),
		fake.TextTurn("oh"),
	)
	loop := &Loop{
		Provider: model,
		Model:    "test",
		Tools:    testTools(t),
		Governor: enforce.NewNop(),
		Session:  &enforce.Session{},
	}

	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatalf("an unknown tool should not fail the run: %v", err)
	}
	var reported bool
	for _, m := range loop.Messages() {
		if m.Role == provider.RoleTool && strings.Contains(m.Text, "unknown tool") {
			reported = true
		}
	}
	if !reported {
		t.Error("the model should be told the tool does not exist")
	}
}

func TestSystemPromptSentOnce(t *testing.T) {
	model := fake.New(
		fake.CallTurn("c1", enforce.ToolLS, `{}`),
		fake.TextTurn("done"),
	)
	loop := &Loop{
		Provider:     model,
		Model:        "test",
		Tools:        testTools(t),
		Governor:     enforce.NewNop(),
		SystemPrompt: "you are a test agent",
		Session:      &enforce.Session{},
	}

	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, req := range model.Requests() {
		count := 0
		for _, m := range req.Messages {
			if m.Role == provider.RoleSystem {
				count++
			}
		}
		if count != 1 {
			t.Errorf("request %d has %d system messages, want exactly 1", i, count)
		}
	}
}

func TestRunPropagatesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	loop := &Loop{
		Provider: fake.New(fake.TextTurn("never reached")),
		Model:    "test",
		Tools:    testTools(t),
		Governor: enforce.NewNop(),
		Session:  &enforce.Session{},
	}
	if _, err := loop.Run(ctx, "go"); err == nil {
		t.Error("a cancelled context should stop the loop")
	}
}

func TestRunRequiresProvider(t *testing.T) {
	loop := &Loop{Tools: testTools(t), Governor: enforce.NewNop()}
	if _, err := loop.Run(context.Background(), "go"); err == nil {
		t.Error("a loop without a provider should error")
	}
}

func TestResetClearsConversation(t *testing.T) {
	loop := &Loop{
		Provider:     fake.New(fake.TextTurn("hi")),
		Model:        "test",
		Tools:        testTools(t),
		Governor:     enforce.NewNop(),
		SystemPrompt: "sys",
		Session:      &enforce.Session{},
	}
	if _, err := loop.Run(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if len(loop.Messages()) == 0 {
		t.Fatal("expected a conversation")
	}
	loop.Reset()
	if len(loop.Messages()) != 0 {
		t.Error("Reset should clear the conversation")
	}
}

func TestDescribeCall(t *testing.T) {
	tests := []struct {
		name         string
		call         tool.Call
		wantAction   string
		wantResource string
	}{
		{"bash", tool.Call{Name: enforce.ToolBash, Args: `{"command":"git push"}`}, enforce.ActionShell, "git push"},
		{"edit path", tool.Call{Name: enforce.ToolEdit, Args: `{"path":"a.go"}`}, enforce.ActionEdit, "a.go"},
		{"write file_path", tool.Call{Name: enforce.ToolWrite, Args: `{"file_path":"b.go"}`}, enforce.ActionEdit, "b.go"},
		{"task", tool.Call{Name: enforce.ToolTask, Args: `{"subagent":"engineer"}`}, enforce.ActionSubagent, "engineer"},
		{"read needs no permission", tool.Call{Name: enforce.ToolRead, Args: `{"path":"a"}`}, "", ""},
		{"malformed args", tool.Call{Name: enforce.ToolEdit, Args: `{bad`}, enforce.ActionEdit, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action, resource := describeCall(tc.call)
			if action != tc.wantAction || resource != tc.wantResource {
				t.Errorf("describeCall = (%q, %q), want (%q, %q)",
					action, resource, tc.wantAction, tc.wantResource)
			}
		})
	}
}

// TestStuckModelIsStoppedWithTheRealError covers a failure mode seen with a
// real 9B model: it repeated one rejected call indefinitely. Burning every
// step and reporting "gave up" hides the actual cause, so the loop stops early
// and surfaces the error the model kept hitting.
func TestStuckModelIsStoppedWithTheRealError(t *testing.T) {
	registry := testTools(t)
	// A handoff tool that always refuses, as a validation failure would.
	registry.Add(rejectingTool{
		name:   enforce.ToolSubmitPlan,
		reason: "a plan must state at least one observable acceptance criterion",
	})

	turns := make([]fake.Turn, 0, 12)
	for i := 0; i < 12; i++ {
		turns = append(turns, fake.CallTurn("c", enforce.ToolSubmitPlan, `{"plan":"x"}`))
	}
	model := fake.New(turns...)

	policy := mustPolicy(t, testPolicy)
	loop := &Loop{
		Provider: model,
		Model:    "test",
		Tools:    registry,
		Governor: enforce.New(policy, workflow.NewMachine(newClock()), ""),
		Task: &workflow.Task{
			ID: "t1", State: workflow.StatePlanning,
			PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
		},
		Session: &enforce.Session{},
	}

	_, err := loop.Run(context.Background(), "plan it")
	if err == nil {
		t.Fatal("a stuck model should stop the run")
	}
	// The message must carry the real cause, not a generic step-limit note.
	if !strings.Contains(err.Error(), "acceptance criterion") {
		t.Errorf("error should surface the underlying failure: %v", err)
	}
	// It must give up well before the step limit.
	if used := len(model.Requests()); used >= maxSteps {
		t.Errorf("should stop early, used %d requests", used)
	}
}

// rejectingTool always fails, standing in for a validation rejection.
type rejectingTool struct {
	name   string
	reason string
}

func (r rejectingTool) Def() provider.ToolDef {
	return provider.ToolDef{
		Name:        r.name,
		Description: "always refuses",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (r rejectingTool) Run(context.Context, tool.Call) (tool.Result, error) {
	return tool.Result{Content: r.reason, IsError: true}, nil
}

// TestVariedFailuresDoNotTripTheStuckGuard: a model making different mistakes
// is still making progress, so the guard must not fire on it.
func TestVariedFailuresDoNotTripTheStuckGuard(t *testing.T) {
	loop := &Loop{
		Provider: fake.New(fake.TextTurn("done")),
		Model:    "test",
		Tools:    testTools(t),
		Governor: enforce.NewNop(),
		Session:  &enforce.Session{},
	}
	for i := 0; i < 10; i++ {
		loop.trackFailure("read", tool.Result{
			Content: fmt.Sprintf("cannot read file-%d.go", i),
			IsError: true,
		})
	}
	if isStuck, _ := loop.stuck(); isStuck {
		t.Error("distinct failures are progress, not a loop")
	}

	// A success resets the streak.
	for i := 0; i < maxIdenticalFailures; i++ {
		loop.trackFailure("read", tool.Result{Content: "same error", IsError: true})
	}
	if isStuck, _ := loop.stuck(); !isStuck {
		t.Fatal("identical repeats should trip the guard")
	}
	loop.trackFailure("read", tool.Result{Content: "file contents"})
	if isStuck, _ := loop.stuck(); isStuck {
		t.Error("a success should reset the streak")
	}
}

// TestTaskRefreshKeepsMaskingCurrent is the fix for a stale-task bug: a handoff
// advances the state machine through the store, so without re-reading the task
// the loop would keep masking against the stage it had already left.
func TestTaskRefreshKeepsMaskingCurrent(t *testing.T) {
	policy := mustPolicy(t, testPolicy)
	task := &workflow.Task{
		ID: "t1", State: workflow.StatePlanning,
		PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
	}
	// The authoritative copy the store would return, already advanced.
	advanced := &workflow.Task{
		ID: "t1", State: workflow.StateImplementing,
		PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
	}

	model := fake.New(
		fake.CallTurn("c1", enforce.ToolLS, `{"path":"."}`),
		fake.TextTurn("done"),
	)
	var notified workflow.State
	loop := &Loop{
		Provider: model,
		Model:    "test",
		// The handoff tools have to be registered even though this test never
		// calls one: a stage that exposes no tool capable of advancing the
		// workflow is not a stage the model is given a turn in.
		Tools:    withHandoffs(testTools(t), task, workflow.NewMachine(newClock())),
		Governor: enforce.New(policy, workflow.NewMachine(newClock()), ""),
		Task:     task,
		// Already handed off: the stage this session owned is finished, which
		// is precisely when a refresh matters.
		Session:     &enforce.Session{HandedOff: true},
		TaskRefresh: func() (*workflow.Task, error) { return advanced, nil },
		OnStateChange: func(t *workflow.Task) {
			notified = t.State
		},
	}

	if _, err := loop.Run(context.Background(), "look"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if loop.Task.State != workflow.StateImplementing {
		t.Errorf("the loop should adopt the refreshed task, got %s", loop.Task.State)
	}
	if notified != workflow.StateImplementing {
		t.Errorf("the caller should be notified of the stage change, got %q", notified)
	}
	// The second request must reflect the new stage, where edit is legal.
	requests := model.Requests()
	if len(requests) < 2 {
		t.Fatalf("want 2 requests, got %d", len(requests))
	}
	if !fake.OffersTool(requests[1], enforce.ToolEdit) {
		t.Errorf("after the refresh, implementing should offer edit: %v",
			fake.ToolNames(requests[1]))
	}
}

// The verifying stage belongs to the runtime: nothing the model can see would
// advance it. Sending it a request anyway is what produced a session that took
// turn after turn narrating its own confusion until it hit the step limit.
func TestVerifyingStageRunsGatesInsteadOfAskingTheModel(t *testing.T) {
	policy := mustPolicy(t, testPolicy)
	task := &workflow.Task{
		ID: "t1", State: workflow.StateVerifying,
		PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
	}
	// What the runtime's gate run leaves behind.
	reviewed := &workflow.Task{
		ID: "t1", State: workflow.StateQAReview,
		PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
	}

	model := fake.New(fake.TextTurn("the review looks fine"))
	advances := 0
	loop := &Loop{
		Provider: model,
		Model:    "test",
		Tools:    withHandoffs(testTools(t), task, workflow.NewMachine(newClock())),
		Governor: enforce.New(policy, workflow.NewMachine(newClock()), ""),
		Task:     task,
		// Handing off is how the session reached verifying in the first place,
		// so the stage it owned is already finished.
		Session: &enforce.Session{HandedOff: true},
		AdvanceStage: func(context.Context) (bool, error) {
			advances++
			return true, nil
		},
		TaskRefresh: func() (*workflow.Task, error) { return reviewed, nil },
	}

	if _, err := loop.Run(context.Background(), "finish the task"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advances != 1 {
		t.Errorf("the runtime should have been asked to advance once, got %d", advances)
	}
	if loop.Task.State != workflow.StateQAReview {
		t.Errorf("state = %s, want qa_review", loop.Task.State)
	}
	// One request, for the qa_review stage — none for verifying.
	requests := model.Requests()
	if len(requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(requests))
	}
	if fake.OffersTool(requests[0], enforce.ToolSubmitImplementation) {
		t.Errorf("the request was built for the wrong stage: %v", fake.ToolNames(requests[0]))
	}
}

// When nobody can advance the stage the run has to end with an explanation
// rather than spending every remaining step on it.
func TestStageNobodyCanAdvanceEndsTheRun(t *testing.T) {
	policy := mustPolicy(t, testPolicy)
	task := &workflow.Task{
		ID: "t1", State: workflow.StateVerifying,
		PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
	}

	model := fake.New(fake.TextTurn("never asked"))
	loop := &Loop{
		Provider: model,
		Model:    "test",
		Tools:    withHandoffs(testTools(t), task, workflow.NewMachine(newClock())),
		Governor: enforce.New(policy, workflow.NewMachine(newClock()), ""),
		Task:     task,
		Session:  &enforce.Session{},
		// The gates did not move it on.
		AdvanceStage: func(context.Context) (bool, error) { return false, nil },
	}

	result, err := loop.Run(context.Background(), "finish the task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(model.Requests()) != 0 {
		t.Errorf("the model should not have been asked anything, got %d requests",
			len(model.Requests()))
	}
	if result.Steps != 0 {
		t.Errorf("steps = %d, want 0", result.Steps)
	}
	if !strings.Contains(result.Text, "verifying") {
		t.Errorf("the reason should name the stage: %q", result.Text)
	}
}

// A terminal or blocked task must not be handed to the model either, and the
// message should say what to do about it.
func TestTerminalStagesEndTheRunWithAReason(t *testing.T) {
	tests := []struct {
		state workflow.State
		want  string
	}{
		{workflow.StateComplete, "complete"},
		{workflow.StateCancelled, "cancelled"},
		{workflow.StateBlocked, "blocked"},
	}
	for _, tt := range tests {
		policy := mustPolicy(t, testPolicy)
		task := &workflow.Task{
			ID: "t1", State: tt.state,
			PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
		}
		model := fake.New(fake.TextTurn("never asked"))
		loop := &Loop{
			Provider: model,
			Model:    "test",
			Tools:    withHandoffs(testTools(t), task, workflow.NewMachine(newClock())),
			Governor: enforce.New(policy, workflow.NewMachine(newClock()), ""),
			Task:     task,
			Session:  &enforce.Session{},
		}
		result, err := loop.Run(context.Background(), "carry on")
		if err != nil {
			t.Fatalf("%s: Run: %v", tt.state, err)
		}
		if len(model.Requests()) != 0 {
			t.Errorf("%s: the model was asked anyway", tt.state)
		}
		if !strings.Contains(result.Text, tt.want) {
			t.Errorf("%s: text = %q, want it to mention %q", tt.state, result.Text, tt.want)
		}
	}
}

// An ungoverned session has no stages, so the check must never fire there.
func TestRuntimeStageCheckIgnoresPlainSessions(t *testing.T) {
	model := fake.New(fake.TextTurn("hello"))
	loop := &Loop{
		Provider: model,
		Model:    "test",
		Tools:    testTools(t),
		Governor: enforce.NewNop(),
		Session:  &enforce.Session{},
	}
	result, err := loop.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Text != "hello" {
		t.Errorf("text = %q, want the model's reply", result.Text)
	}
	if len(model.Requests()) != 1 {
		t.Errorf("want 1 request, got %d", len(model.Requests()))
	}
}

// A hook that claims progress without making any would otherwise spin the loop
// to the step limit.
func TestAdvanceStageMustActuallyMoveTheState(t *testing.T) {
	policy := mustPolicy(t, testPolicy)
	task := &workflow.Task{
		ID: "t1", State: workflow.StateVerifying,
		PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
	}
	model := fake.New(fake.TextTurn("never asked"))
	advances := 0
	loop := &Loop{
		Provider: model,
		Model:    "test",
		Tools:    withHandoffs(testTools(t), task, workflow.NewMachine(newClock())),
		Governor: enforce.New(policy, workflow.NewMachine(newClock()), ""),
		Task:     task,
		Session:  &enforce.Session{},
		AdvanceStage: func(context.Context) (bool, error) {
			advances++
			return true, nil // lies: the state never changes
		},
		TaskRefresh: func() (*workflow.Task, error) { return task, nil },
	}
	if _, err := loop.Run(context.Background(), "finish"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advances != 1 {
		t.Errorf("the loop should stop asking after the first no-op, got %d attempts", advances)
	}
}

// A failing gate run is an error the user needs to see, not something to
// retry silently.
func TestAdvanceStageErrorSurfaces(t *testing.T) {
	policy := mustPolicy(t, testPolicy)
	task := &workflow.Task{
		ID: "t1", State: workflow.StateVerifying,
		PolicyHash: policy.Hash(), Receipts: map[string]workflow.Receipt{},
	}
	loop := &Loop{
		Provider:     fake.New(fake.TextTurn("never asked")),
		Model:        "test",
		Tools:        withHandoffs(testTools(t), task, workflow.NewMachine(newClock())),
		Governor:     enforce.New(policy, workflow.NewMachine(newClock()), ""),
		Task:         task,
		Session:      &enforce.Session{},
		AdvanceStage: func(context.Context) (bool, error) { return false, errors.New("gate runner exploded") },
	}
	if _, err := loop.Run(context.Background(), "finish"); err == nil ||
		!strings.Contains(err.Error(), "exploded") {
		t.Errorf("err = %v, want the gate failure", err)
	}
}
