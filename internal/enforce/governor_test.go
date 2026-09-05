package enforce

import (
	"testing"

	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

// Both implementations must satisfy the seam the loop depends on.
var (
	_ Governor = (*Enforcer)(nil)
	_ Governor = Nop{}
)

// TestNopPermitsEverythingButTheStateMachine backs the quick path:
// --no-workflow, agentwarden run and /plain impose nothing on ordinary work.
// The workflow tools are the exception, because they are not inert — they
// advance stored task state through the controller, so an ungoverned session
// holding them could submit a plan or complete a task with no gate checked.
func TestNopPermitsEverythingButTheStateMachine(t *testing.T) {
	nop := NewNop()
	task := &workflow.Task{State: workflow.StatePlanning, Receipts: map[string]workflow.Receipt{}}
	sess := &Session{}

	visible := nop.VisibleTools(task, sess, allTools())
	for _, def := range visible {
		if IsWorkflowTool(def.Name) {
			t.Errorf("%s should not be offered to an ungoverned session", def.Name)
		}
	}
	// Everything that is not the state machine survives untouched.
	var wantVisible int
	for _, def := range allTools() {
		if !IsWorkflowTool(def.Name) {
			wantVisible++
		}
	}
	if len(visible) != wantVisible {
		t.Errorf("visible tools = %d, want %d", len(visible), wantVisible)
	}
	if wantVisible == len(allTools()) {
		t.Fatal("the fixture has no workflow tools, so this proves nothing")
	}

	if tc := nop.ToolChoice(task, sess); tc != nil {
		t.Error("the model must not be constrained")
	}
	// Even the calls the real enforcer refuses outright go through here.
	for _, c := range []provider.ToolCall{
		{Name: ToolEdit, Args: `{"path":"a.go"}`},
		{Name: ToolBash, Args: `{"command":"git push"}`},
		{Name: ToolTask, Args: `{}`},
	} {
		if d := nop.Intercept(task, sess, c); !d.Allow {
			t.Errorf("%s should be allowed when ungoverned", c.Name)
		}
	}
	if d := nop.OnTurnEnd(task, sess); !d.Allow {
		t.Error("a turn should be free to end")
	}
	if d := nop.OnComplete(task, workflow.Fingerprint{}); !d.Allow {
		t.Error("completion should not require gate evidence")
	}
	if nop.Banner(task, sess, nil) != "" {
		t.Error("no banner should be injected")
	}
	if nop.Enabled() {
		t.Error("Enabled() should report ungoverned")
	}
}

// Masking hides the workflow tools, so a call for one means the model asked
// anyway — usually by repeating a call from before governance was switched
// off, which is still in its context.
func TestNopRefusesWorkflowCalls(t *testing.T) {
	nop := NewNop()
	task := &workflow.Task{State: workflow.StatePlanning, Receipts: map[string]workflow.Receipt{}}

	for _, name := range []string{
		ToolSubmitPlan, ToolSubmitImplementation, ToolSubmitQA,
		ToolComplete, ToolStatus, ToolHistory, ToolBlock,
		"workflow_start", "workflow_anything_later",
	} {
		d := nop.Intercept(task, &Session{}, provider.ToolCall{Name: name})
		if d.Allow {
			t.Errorf("%s advanced the state machine in an ungoverned session", name)
		}
		if d.Reason == "" || d.Correction == "" {
			t.Errorf("%s: refusal must say why and what to do instead: %+v", name, d)
		}
	}
}

func TestIsWorkflowTool(t *testing.T) {
	for _, name := range []string{ToolSubmitPlan, ToolStatus, "workflow_"} {
		if !IsWorkflowTool(name) {
			t.Errorf("%q should be recognised as a workflow tool", name)
		}
	}
	for _, name := range []string{ToolRead, ToolEdit, ToolBash, ToolTask, "", "my_workflow_tool"} {
		if IsWorkflowTool(name) {
			t.Errorf("%q should not be recognised as a workflow tool", name)
		}
	}
}

func TestEnforcerReportsEnabled(t *testing.T) {
	e, _ := newEnforcer(t, twoGatePolicy)
	if !e.Enabled() {
		t.Error("the real enforcer should report governed")
	}
}

func TestPermissionsOrderedRules(t *testing.T) {
	// The same shape as the existing qa-engineer agent: deny broadly, then
	// re-allow specific inspection commands.
	rules := []Rule{
		{Action: ActionEdit, Resource: "*", Effect: EffectDeny},
		{Action: ActionShell, Resource: "*", Effect: EffectDeny},
		{Action: ActionShell, Resource: "git status*", Effect: EffectAllow},
		{Action: ActionShell, Resource: "git diff*", Effect: EffectAllow},
	}
	p := NewPermissions(rules, false)

	tests := []struct {
		action, resource string
		want             Effect
	}{
		{ActionEdit, "src/main.go", EffectDeny},
		{ActionShell, "rm -rf /", EffectDeny},
		{ActionShell, "git status --short", EffectAllow},
		{ActionShell, "git diff HEAD", EffectAllow},
		{ActionShell, "git push", EffectDeny},
		// No rule mentions subagent, so it falls through to ask.
		{ActionSubagent, "engineer", EffectAsk},
		// Read-only tools need no permission at all.
		{"", "anything", EffectAllow},
	}
	for _, tc := range tests {
		t.Run(tc.action+" "+tc.resource, func(t *testing.T) {
			if got := p.Evaluate(tc.action, tc.resource); got != tc.want {
				t.Errorf("Evaluate(%q, %q) = %s, want %s", tc.action, tc.resource, got, tc.want)
			}
		})
	}
}

// TestAutoModeUpgradesAskButNotDeny is the safety property of --auto: it
// removes confirmation prompts without overriding a rule the user wrote.
func TestAutoModeUpgradesAskButNotDeny(t *testing.T) {
	rules := []Rule{{Action: ActionShell, Resource: "rm *", Effect: EffectDeny}}
	p := NewPermissions(rules, true)

	if got := p.Evaluate(ActionEdit, "src/main.go"); got != EffectAllow {
		t.Errorf("auto mode should approve an unmatched edit, got %s", got)
	}
	if got := p.Evaluate(ActionShell, "rm -rf /"); got != EffectDeny {
		t.Errorf("auto mode must not override an explicit deny, got %s", got)
	}

	p.SetAuto(false)
	if got := p.Evaluate(ActionEdit, "src/main.go"); got != EffectAsk {
		t.Errorf("turning auto off should restore asking, got %s", got)
	}
}

func TestEvaluateToolMapsActions(t *testing.T) {
	p := NewPermissions([]Rule{
		{Action: ActionEdit, Resource: "*", Effect: EffectDeny},
		{Action: ActionShell, Resource: "*", Effect: EffectAllow},
	}, false)

	tests := map[string]Effect{
		ToolEdit:  EffectDeny,
		ToolWrite: EffectDeny,
		ToolBash:  EffectAllow,
		ToolRead:  EffectAllow, // read-only, no permission needed
		ToolGrep:  EffectAllow,
	}
	for tool, want := range tests {
		if got := p.EvaluateTool(tool, "x"); got != want {
			t.Errorf("EvaluateTool(%s) = %s, want %s", tool, got, want)
		}
	}
}

func TestMatchResource(t *testing.T) {
	tests := []struct {
		pattern, resource string
		want              bool
	}{
		{"*", "anything", true},
		{"", "anything", true},
		{"git status*", "git status --short", true},
		{"git status*", "git push", false},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "src/main.ts", false},
		{"exact", "exact", true},
		{"exact", "other", false},
	}
	for _, tc := range tests {
		t.Run(tc.pattern+"|"+tc.resource, func(t *testing.T) {
			if got := matchResource(tc.pattern, tc.resource); got != tc.want {
				t.Errorf("matchResource(%q, %q) = %v, want %v", tc.pattern, tc.resource, got, tc.want)
			}
		})
	}
}
