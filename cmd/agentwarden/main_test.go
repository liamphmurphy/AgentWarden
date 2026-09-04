package main

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lmurphy/agentwarden/internal/agent"
	"github.com/lmurphy/agentwarden/internal/config"
	"github.com/lmurphy/agentwarden/internal/controller"
	"github.com/lmurphy/agentwarden/internal/enforce"
	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/session"
	"github.com/lmurphy/agentwarden/internal/skill"
	"github.com/lmurphy/agentwarden/internal/tool"
	"github.com/lmurphy/agentwarden/internal/workflow"
	"gopkg.in/yaml.v3"
)

// The four role agents mirror the shape of a real config: an orchestrator that
// coordinates a workflow and may not touch code, and role agents that may.
const (
	orchestratorPrompt = "You coordinate repository changes through the governed workflow. " +
		"Call workflow_start once with the objective. Do not plan, implement or review yourself."
	plannerPrompt     = "You produce an implementation plan and submit it."
	implementerPrompt = "You implement the approved plan exactly."
)

func testPolicy(t *testing.T) *workflow.Policy {
	t.Helper()
	const src = `
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
	policy, err := workflow.ParsePolicy(strings.NewReader(src), yaml.Unmarshal)
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	return policy
}

func testAgents() *agent.Registry {
	r := agent.NewRegistry()
	r.Add(&agent.Definition{
		Name: "orchestrator", Mode: agent.ModePrimary, Prompt: orchestratorPrompt,
		Skills: []string{"go-style"},
		Permissions: []enforce.Rule{
			{Action: enforce.ActionEdit, Resource: "*", Effect: enforce.EffectDeny},
			{Action: enforce.ActionShell, Resource: "*", Effect: enforce.EffectDeny},
		},
	})
	r.Add(&agent.Definition{
		Name: "tech-lead", Mode: agent.ModeAll, Prompt: plannerPrompt,
		Skills: []string{"go-style"},
		Permissions: []enforce.Rule{
			{Action: enforce.ActionEdit, Resource: "*", Effect: enforce.EffectDeny},
		},
	})
	r.Add(&agent.Definition{
		Name: "engineer", Mode: agent.ModeAll, Prompt: implementerPrompt,
		Permissions: []enforce.Rule{
			{Action: enforce.ActionEdit, Resource: "*", Effect: enforce.EffectAllow},
			{Action: enforce.ActionShell, Resource: "*", Effect: enforce.EffectAllow},
		},
	})
	return r
}

func testSkills() *skill.Set {
	s := skill.NewSet()
	s.Add(&skill.Skill{Name: "go-style", Description: "House Go style", Body: "Prefer clarity."})
	return s
}

// staticFingerprinter reports one unchanging tree, which is all the lifecycle
// actions under test here need.
type staticFingerprinter struct{}

func (staticFingerprinter) Fingerprint(context.Context) (workflow.Fingerprint, error) {
	return workflow.Fingerprint{Head: "h1", Digest: "d1"}, nil
}

// newTestApp builds a governed session over a real store in a temp directory,
// so the task lifecycle actions operate on genuine persisted state.
func newTestApp(t *testing.T) *app {
	t.Helper()
	policy := testPolicy(t)
	agents := testAgents()
	orchestrator, _ := agents.Get("orchestrator")

	clock := realClock{}
	machine := workflow.NewMachineWithTransitions(policy.Transitions(), clock)
	finger := staticFingerprinter{}
	store := session.NewStore(t.TempDir())
	gates := enforce.NewGateRunner(enforce.ExecRunner{}, finger, ".", clock, nil)

	a := &app{
		cfg:             &config.Config{},
		project:         t.TempDir(),
		governed:        true,
		policy:          policy,
		policyAvailable: true,
		agents:          agents,
		skills:          testSkills(),
		agentDef:        orchestrator,
		tools:           tool.NewRegistry(),
		machine:         machine,
		ctl:             controller.New(policy, machine, store, gates, finger, clock),
		task:            &workflow.Task{ID: "t1", State: workflow.StatePlanning},
	}
	a.perms = enforce.NewPermissions(orchestrator.Permissions, false)
	a.actor = &controller.Actor{TaskID: a.task.ID}
	a.enforcer = enforce.New(a.policy, a.machine, "workflow.yml")
	a.governor = a.enforcer
	a.syncActor()
	a.loop = a.newLoop(nil, nil)
	return a
}

// A governed session is masked and permissioned as the stage owner, so it must
// be prompted as the stage owner too. Prompting as the orchestrator — whose
// own instructions forbid planning — while the planning stage offers only
// workflow_submit_plan leaves the workflow unsatisfiable.
func TestGovernedPromptFollowsStageOwner(t *testing.T) {
	a := newTestApp(t)

	if got := a.systemPrompt(); !strings.Contains(got, plannerPrompt) {
		t.Errorf("planning stage prompt = %q, want the planner's", got)
	}
	if strings.Contains(a.systemPrompt(), orchestratorPrompt) {
		t.Error("the planning stage is prompted as the orchestrator")
	}

	a.task = &workflow.Task{ID: "t1", State: workflow.StateImplementing}
	a.syncActor()
	if got := a.systemPrompt(); !strings.Contains(got, implementerPrompt) {
		t.Errorf("implementing stage prompt = %q, want the implementer's", got)
	}
}

// The stage owner's skills come with the stage's prompt.
func TestGovernedPromptIncludesStageSkills(t *testing.T) {
	a := newTestApp(t)
	if got := a.systemPrompt(); !strings.Contains(got, "go-style") {
		t.Errorf("the planner's skill is missing from its prompt:\n%s", got)
	}
}

func TestPromptIncludesProjectInstructions(t *testing.T) {
	a := newTestApp(t)
	a.projectInstructions = "# Project rules\n\nRun the focused tests."

	got := a.systemPrompt()
	if !strings.Contains(got, a.projectInstructions) {
		t.Errorf("project instructions missing from prompt:\n%s", got)
	}
}

// Plain mode has no stage, so no role prompt applies. A workflow agent's
// instructions describe a workflow that is not running, which is what makes an
// ungoverned session narrate a planning stage that does not exist.
func TestPlainPromptDropsWorkflowRole(t *testing.T) {
	a := newTestApp(t)
	if err := a.SetGoverned(false); err != nil {
		t.Fatalf("SetGoverned(false): %v", err)
	}

	got := a.systemPrompt()
	for _, unwanted := range []string{orchestratorPrompt, plannerPrompt, "workflow_start"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("plain prompt still contains %q:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "no workflow") {
		t.Errorf("plain prompt should state that there is no workflow:\n%s", got)
	}
	// Skills are reference material rather than role instructions, so the
	// configured agent's skills stay attached even though its prompt does not.
	if !strings.Contains(got, "go-style") {
		t.Errorf("plain prompt lost the configured agent's skills:\n%s", got)
	}
}

// An agent named on the command line is the user's own instruction, so it
// survives into plain mode.
func TestPlainPromptKeepsExplicitAgent(t *testing.T) {
	a := newTestApp(t)
	engineer, _ := a.agents.Get("engineer")
	a.agentDef = engineer
	a.agentExplicit = true

	if err := a.SetGoverned(false); err != nil {
		t.Fatalf("SetGoverned(false): %v", err)
	}
	if got := a.systemPrompt(); !strings.Contains(got, implementerPrompt) {
		t.Errorf("an explicitly chosen agent was dropped in plain mode:\n%s", got)
	}
}

// Rewriting the field is not enough: the prompt is copied into the message
// list before the first turn, so a session that has already spoken would keep
// being instructed as the identity it started as.
func TestModeSwitchRewritesLivePrompt(t *testing.T) {
	a := newTestApp(t)
	a.loop.SetSystemPrompt(a.systemPrompt())
	// Simulate a conversation already under way.
	a.loop.Note("earlier user message")

	if err := a.SetGoverned(false); err != nil {
		t.Fatalf("SetGoverned(false): %v", err)
	}

	messages := a.loop.Messages()
	if len(messages) == 0 || messages[0].Role != provider.RoleSystem {
		t.Fatalf("expected a system message first, got %+v", messages)
	}
	if strings.Contains(messages[0].Text, plannerPrompt) {
		t.Error("the live conversation still carries the planner's instructions")
	}
	if !strings.Contains(messages[0].Text, "no workflow") {
		t.Errorf("the live system message was not rewritten: %q", messages[0].Text)
	}

	// The earlier turns still describe a workflow, so the change is also
	// stated where the model is actually looking.
	last := messages[len(messages)-1]
	if !strings.Contains(last.Text, "OFF") {
		t.Errorf("no note told the model governance was switched off: %q", last.Text)
	}
}

// Nothing about the workflow may follow the session into plain mode.
func TestPlainModeDropsWorkflowState(t *testing.T) {
	a := newTestApp(t)
	if err := a.SetGoverned(false); err != nil {
		t.Fatalf("SetGoverned(false): %v", err)
	}

	if a.governed {
		t.Error("the session still reports itself governed")
	}
	if a.governor.Enabled() {
		t.Error("the enforcer is still the active governor")
	}
	if a.loop.Task != nil {
		t.Error("the loop still holds the governed task")
	}
	if a.loop.Session == nil || a.loop.Session.Role != "" {
		t.Errorf("the loop still acts as a workflow role: %+v", a.loop.Session)
	}
	if a.loop.TaskRefresh != nil || a.loop.OnStateChange != nil {
		t.Error("the loop still tracks state changes")
	}
	if got := a.WorkflowState(); got != "" {
		t.Errorf("WorkflowState() = %q, want empty in plain mode", got)
	}
}

// Plain mode advertises that every tool is available, so the stage owner's
// division of duties must not still be denying edits.
func TestPlainModePermitsWork(t *testing.T) {
	a := newTestApp(t)
	if got := a.perms.Evaluate(enforce.ActionEdit, "README.md"); got != enforce.EffectDeny {
		t.Fatalf("fixture is wrong: the planning stage should deny edits, got %q", got)
	}

	if err := a.SetGoverned(false); err != nil {
		t.Fatalf("SetGoverned(false): %v", err)
	}
	for _, action := range []string{enforce.ActionEdit, enforce.ActionShell} {
		if got := a.perms.Evaluate(action, "README.md"); got != enforce.EffectAllow {
			t.Errorf("plain mode %s = %q, want allow", action, got)
		}
	}
}

// A restriction the user chose with -agent is theirs, not the workflow's, so
// plain mode keeps it.
func TestPlainModeKeepsExplicitAgentPermissions(t *testing.T) {
	a := newTestApp(t)
	orchestrator, _ := a.agents.Get("orchestrator")
	a.agentDef = orchestrator
	a.agentExplicit = true

	if err := a.SetGoverned(false); err != nil {
		t.Fatalf("SetGoverned(false): %v", err)
	}
	if got := a.perms.Evaluate(enforce.ActionEdit, "README.md"); got != enforce.EffectDeny {
		t.Errorf("an explicit agent's deny was widened to %q", got)
	}
}

// Switching back on must restore the stage's identity, or the session would
// stay a plain assistant while the enforcer masked it as a role.
func TestSwitchingBackRestoresGovernance(t *testing.T) {
	a := newTestApp(t)
	if err := a.SetGoverned(false); err != nil {
		t.Fatalf("SetGoverned(false): %v", err)
	}
	if err := a.SetGoverned(true); err != nil {
		t.Fatalf("SetGoverned(true): %v", err)
	}

	if !a.governed || !a.governor.Enabled() {
		t.Error("governance was not re-engaged")
	}
	if a.loop.Task == nil || a.loop.Task.ID != "t1" {
		t.Errorf("the task was not restored to the loop: %+v", a.loop.Task)
	}
	if a.loop.OnStateChange == nil || a.loop.TaskRefresh == nil {
		t.Error("state tracking was not restored")
	}
	if got := a.systemPrompt(); !strings.Contains(got, plannerPrompt) {
		t.Errorf("the stage owner's prompt was not restored:\n%s", got)
	}
	if got := a.perms.Evaluate(enforce.ActionEdit, "README.md"); got != enforce.EffectDeny {
		t.Errorf("the planning stage's edit denial was not restored, got %q", got)
	}

	messages := a.loop.Messages()
	if last := messages[len(messages)-1]; !strings.Contains(last.Text, "ON") {
		t.Errorf("no note told the model governance was switched on: %q", last.Text)
	}
}

// A session with no policy cannot be governed, and saying so is better than
// leaving the caller to guess from an unchanged mode.
func TestSetGovernedWithoutPolicyFails(t *testing.T) {
	a := newTestApp(t)
	a.policyAvailable = false
	if err := a.SetGoverned(false); err != nil {
		t.Fatalf("SetGoverned(false): %v", err)
	}
	if err := a.SetGoverned(true); err == nil {
		t.Error("engaging governance without a policy should fail")
	}
	if a.governed {
		t.Error("a failed switch left the session claiming governance")
	}
}

func TestPlainRulesCoverTheAskingActions(t *testing.T) {
	perms := enforce.NewPermissions(plainRules(), false)
	// Without an explicit allow these default to asking, and the TUI has no
	// confirmation prompt yet, so an empty rule set would refuse the work.
	for _, action := range []string{enforce.ActionEdit, enforce.ActionShell} {
		if got := perms.Evaluate(action, "anything"); got != enforce.EffectAllow {
			t.Errorf("plain %s = %q, want allow", action, got)
		}
	}
}

// A blocked task has no tool the model could call to leave that state, so
// without an operator-side resume it would stay blocked for good.
func TestResumeTaskReopensABlockedTask(t *testing.T) {
	a := newTestApp(t)
	task, err := a.ctl.Start(context.Background(), "add request timeouts")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := a.ctl.Block(task.ID, "tech-lead", "waiting on an answer"); err != nil {
		t.Fatalf("Block: %v", err)
	}

	if err := a.resumeTask(task.ID); err != nil {
		t.Fatalf("resumeTask: %v", err)
	}
	reopened, err := a.ctl.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.State != workflow.StatePlanning {
		t.Errorf("state = %s, want planning", reopened.State)
	}
}

func TestLoadSessionRestoresCheckpointAndReopensBlockedTask(t *testing.T) {
	a := newTestApp(t)
	task, err := a.ctl.Start(context.Background(), "add request timeouts")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := a.ctl.Block(task.ID, "tech-lead", "waiting on an answer"); err != nil {
		t.Fatalf("Block: %v", err)
	}

	if err := a.loadSession(task.ID); err != nil {
		t.Fatalf("loadSession(%q): %v", task.ID, err)
	}
	if a.task.State != workflow.StatePlanning {
		t.Errorf("loadSession(%q) state = %s, want planning", task.ID, a.task.State)
	}
	if a.actor.TaskID != task.ID {
		t.Errorf("loadSession(%q) actor task id = %q, want %q", task.ID, a.actor.TaskID, task.ID)
	}
	if a.actor.AgentID != "tech-lead" {
		t.Errorf("loadSession(%q) actor agent = %q, want tech-lead", task.ID, a.actor.AgentID)
	}
}

func TestSessionPickerNavigatesAndConfirmsWithEnter(t *testing.T) {
	tasks := []*workflow.Task{
		{ID: "newest", State: workflow.StateImplementing, Objective: "new task"},
		{ID: "older", State: workflow.StatePlanning, Objective: "old task"},
	}
	model := newSessionPicker(tasks)

	model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if model.selected != "older" {
		t.Errorf("sessionPicker selected = %q, want older", model.selected)
	}
}

// Cancelling is how a task carrying false evidence is taken out of play: four
// of them had reached qa_review with three green receipts and handoffs saying
// no work had been done.
func TestCancelTaskClosesATask(t *testing.T) {
	a := newTestApp(t)
	task, err := a.ctl.Start(context.Background(), "something to abandon")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := a.cancelTask(task.ID); err != nil {
		t.Fatalf("cancelTask: %v", err)
	}
	cancelled, err := a.ctl.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != workflow.StateCancelled {
		t.Errorf("state = %s, want cancelled", cancelled.State)
	}

	// Cancelled is terminal, so a second attempt must be refused rather than
	// silently accepted.
	if err := a.cancelTask(task.ID); err == nil {
		t.Error("cancelling a cancelled task should fail")
	}
}

// The operator acts as the orchestrator: resume and cancel are that role's to
// perform, and the person at the terminal owns the workflow.
func TestOperatorActsAsOrchestrator(t *testing.T) {
	a := newTestApp(t)
	if got := a.operatorActor(); got != "orchestrator" {
		t.Errorf("operatorActor() = %q, want orchestrator", got)
	}

	// With no policy there is no role mapping to speak of.
	a.policy = nil
	if got := a.operatorActor(); got != "" {
		t.Errorf("operatorActor() without a policy = %q, want empty", got)
	}
}

// These act on stored state, so they must say so rather than panic when the
// project has no governance at all.
func TestTaskActionsNeedAPolicy(t *testing.T) {
	a := newTestApp(t)
	a.ctl = nil
	if err := a.cancelTask("whatever"); err == nil {
		t.Error("cancelling without a controller should fail")
	}
	if err := a.resumeTask("whatever"); err == nil {
		t.Error("resuming without a controller should fail")
	}
}

func TestUnknownTaskIsReported(t *testing.T) {
	a := newTestApp(t)
	if err := a.cancelTask("nosuchtask"); err == nil {
		t.Error("cancelling an unknown task should fail")
	}
}

// Verification is the runtime's stage, and advanceVerification is what the
// loop calls the moment it is entered.
func TestAdvanceVerificationRunsGatesAndReportsMovement(t *testing.T) {
	a := newTestApp(t)
	task, err := a.ctl.Start(context.Background(), "add request timeouts")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	a.task = task
	a.actor.TaskID = task.ID
	a.loop.Task = task

	// Nothing to verify yet: the task is still planning.
	if advanced, err := a.advanceVerification(context.Background(), a.loop); err != nil {
		t.Fatalf("advanceVerification: %v", err)
	} else if advanced {
		t.Error("a task that is not verifying should not report movement")
	}
}
