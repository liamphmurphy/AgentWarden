// Command agentwarden is a terminal coding agent with a native workflow enforcer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lmurphy/agentwarden/internal/agent"
	"github.com/lmurphy/agentwarden/internal/config"
	"github.com/lmurphy/agentwarden/internal/controller"
	"github.com/lmurphy/agentwarden/internal/enforce"
	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/provider/openaicompat"
	"github.com/lmurphy/agentwarden/internal/session"
	"github.com/lmurphy/agentwarden/internal/skill"
	"github.com/lmurphy/agentwarden/internal/tool"
	"github.com/lmurphy/agentwarden/internal/tui"
	"github.com/lmurphy/agentwarden/internal/workflow"
	"gopkg.in/yaml.v3"
)

// realClock is the production Clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// options are the parsed command-line flags.
type options struct {
	model string
	// inspectOnly suppresses side effects such as starting a task, so
	// `-config` can report the setup without altering it.
	inspectOnly   bool
	agentName     string
	noWorkflow    bool
	auto          bool
	logRequests   string
	objective     string
	showConfig    bool
	listTasks     bool
	cancelTask    string
	resumeTask    string
	resumeSession string
	resumePicker  bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "agentwarden: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `agentwarden — a terminal coding agent with a native workflow enforcer

Usage:
  agentwarden [flags]                 start the interactive TUI
  agentwarden run [flags] "<prompt>"  run one prompt and print the answer
  agentwarden resume [session-id]     list or continue a previous session

Flags:
  -model string      provider/model to use (overrides config)
  -agent string      agent to run as (overrides config)
  -no-workflow       run ungoverned, skipping the state machine and gates
  -auto              auto-approve tool calls that would otherwise prompt
  -log-requests path  append every provider payload to a JSONL file
  -objective string  objective for a new governed task
  -config            print the resolved configuration and exit
  -tasks             list this project's governed tasks and exit
  -cancel id         cancel a governed task and exit
  -resume id         resume a blocked governed task and exit

Commands:
  resume              list previous governed sessions in this project
  resume <session-id> continue a session at its persisted workflow stage

Governance is on only when the config enables it and a policy file exists.
`)
}

func run() error {
	flag.Usage = usage

	// `agentwarden run "prompt"` is a subcommand; everything else is the TUI.
	args := os.Args[1:]
	oneShot := false
	resumeCommand := false
	if len(args) > 0 && args[0] == "run" {
		oneShot = true
		args = args[1:]
	} else if len(args) > 0 && args[0] == "resume" {
		resumeCommand = true
		args = args[1:]
	}

	var opts options
	fs := flag.NewFlagSet("agentwarden", flag.ContinueOnError)
	fs.Usage = usage
	fs.StringVar(&opts.model, "model", "", "provider/model to use")
	fs.StringVar(&opts.agentName, "agent", "", "agent to run as")
	fs.BoolVar(&opts.noWorkflow, "no-workflow", false, "run ungoverned")
	fs.BoolVar(&opts.auto, "auto", false, "auto-approve tool calls")
	fs.StringVar(&opts.logRequests, "log-requests", "", "append provider payloads to a JSONL file")
	fs.StringVar(&opts.objective, "objective", "", "objective for a new governed task")
	fs.BoolVar(&opts.showConfig, "config", false, "print the resolved configuration")
	fs.BoolVar(&opts.listTasks, "tasks", false, "list governed tasks")
	fs.StringVar(&opts.cancelTask, "cancel", "", "cancel a governed task by id")
	fs.StringVar(&opts.resumeTask, "resume", "", "resume a blocked governed task by id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if resumeCommand {
		if len(fs.Args()) > 1 {
			return errors.New("`agentwarden resume` accepts at most one session id")
		}
		if len(fs.Args()) == 1 {
			opts.resumeSession = fs.Args()[0]
		} else {
			opts.resumePicker = true
		}
	}
	prompt := strings.Join(fs.Args(), " ")
	if opts.resumePicker {
		project, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("locate working directory: %w", err)
		}
		opts.resumeSession, err = chooseSession(project)
		if err != nil {
			return err
		}
		if opts.resumeSession == "" {
			return nil
		}
	}

	// Every one of these inspects or repairs existing state, so none of them
	// may start a task of its own on the way in.
	opts.inspectOnly = opts.showConfig || opts.listTasks ||
		opts.cancelTask != "" || opts.resumeTask != "" || opts.resumeSession != ""

	app, err := build(opts)
	if err != nil {
		return err
	}
	defer app.close()

	if opts.showConfig {
		return app.printConfig()
	}
	if opts.listTasks {
		return app.printTasks()
	}
	if opts.cancelTask != "" {
		return app.cancelTask(opts.cancelTask)
	}
	if opts.resumeTask != "" {
		return app.resumeTask(opts.resumeTask)
	}
	if opts.resumeSession != "" {
		return app.runTUI()
	}
	if oneShot {
		if prompt == "" {
			return errors.New("`agentwarden run` needs a prompt")
		}
		return app.runOnce(prompt)
	}
	return app.runTUI()
}

// app holds everything a session needs.
type app struct {
	cfg          *config.Config
	project      string
	governed     bool
	policy       *workflow.Policy
	ctl          *controller.Controller
	tools        *tool.Registry
	governor     enforce.Governor
	perms        *enforce.Permissions
	provider     provider.Provider
	modelID      string
	modelRef     string
	agentDef     *agent.Definition
	task         *workflow.Task
	conversation []provider.Message
	gateRun      *enforce.GateRunner
	logFile      *os.File
	actor        *controller.Actor
	skills       *skill.Set
	agents       *agent.Registry
	// agentExplicit records that the user named the agent with -agent, which
	// is honoured in plain mode too; a merely configured default is not, since
	// defaultAgent is the identity a governed session starts from.
	agentExplicit bool

	// enforcer is the real governor, held separately from the active one so
	// governance can be engaged and disengaged without rebuilding it.
	enforcer *enforce.Enforcer
	machine  *workflow.Machine
	// policyPath is needed by the enforcer to refuse edits to the policy.
	policyPath string
	// policyAvailable reports whether governance can be switched on at all.
	policyAvailable bool
	// loop is retained so a mode switch can swap its governor in place.
	loop *agent.Loop
	// reportState pushes workflow transitions to the UI, so the status bar
	// does not keep showing the state the session started in.
	reportState func(workflow.State)
}

// announceState tells the UI the current workflow state, if anything is
// listening.
func (a *app) announceState() {
	if a.reportState != nil {
		a.reportState(a.WorkflowState())
	}
}

// GovernanceAvailable reports whether this session can be governed. It needs a
// policy that parses and a git work tree to fingerprint.
func (a *app) GovernanceAvailable() bool { return a.policyAvailable }

// WorkflowState reports the current state, or "" when ungoverned.
func (a *app) WorkflowState() workflow.State {
	if !a.governed || a.task == nil {
		return ""
	}
	if task, err := a.ctl.Get(a.task.ID); err == nil {
		return task.State
	}
	return a.task.State
}

// SetGoverned engages or disengages the enforcer on the live session.
//
// Swapping the loop's governor is what makes the switch real: the TUI's own
// flag only decides what the status bar says.
func (a *app) SetGoverned(on bool) error {
	if a.loop == nil {
		return errors.New("no active session")
	}
	if !on {
		a.governed = false
		a.governor = enforce.NewNop()
		a.loop.Governor = a.governor

		// Nothing about the workflow may follow the session into plain mode.
		// The task is what the loop masks and reports against; the session
		// carries the stage's role and its escalation counters; the prompt and
		// the permission rules are the stage owner's. Leaving any of them
		// behind is what makes a plain session still act as though a planning
		// stage were under way, and refuse the edit it was just asked for.
		a.loop.Task = nil
		a.loop.Session = &enforce.Session{}
		a.loop.TaskRefresh = nil
		a.loop.OnStateChange = nil
		a.applyPlainPermissions()
		a.loop.SetSystemPrompt(a.systemPrompt())
		// The conversation still contains the governed turns, so the change
		// is stated where the model is looking rather than only in the
		// system prompt it has already read.
		a.loop.Note(plainSwitchNote)
		a.announceState()
		return nil
	}
	if !a.policyAvailable {
		return errors.New("no workflow policy is loaded for this project")
	}

	// A session that started plain has no task yet.
	if a.task == nil {
		task, err := a.ctl.Start(context.Background(), "interactive session")
		if err != nil {
			return err
		}
		a.task = task
		a.actor = &controller.Actor{TaskID: task.ID}
		controller.Register(a.tools, a.ctl, a.actor)
	}

	if a.enforcer == nil {
		a.enforcer = enforce.New(a.policy, a.machine, a.policyPath)
	}
	a.governed = true
	a.governor = a.enforcer
	a.loop.Governor = a.governor
	a.attachGovernance(a.loop)
	a.syncActor()
	a.retargetSession(a.loop)
	a.loop.SetSystemPrompt(a.systemPrompt())
	a.loop.Note(governedSwitchNote)
	a.announceState()
	return nil
}

// applyPlainPermissions installs the permissions of an ungoverned session.
//
// A stage owner's rules exist to divide a workflow's duties — the orchestrator
// may not edit because the engineer does that — so with no workflow they
// describe nothing, and keeping them in force is what makes plain mode refuse
// an edit while advertising that every tool is available. An agent named with
// -agent keeps its own rules: that restriction was the user's choice, not the
// workflow's.
//
// The rules are stated rather than emptied because an unmatched edit or shell
// action defaults to asking, and the TUI has no confirmation prompt yet, so an
// empty rule set would refuse everything instead of allowing it.
func (a *app) applyPlainPermissions() {
	if a.perms == nil {
		return
	}
	if a.agentExplicit && a.agentDef != nil {
		a.perms.SetRules(a.agentDef.Permissions)
		return
	}
	a.perms.SetRules(plainRules())
}

// plainRules are the permissions an ungoverned session runs under.
func plainRules() []enforce.Rule {
	return []enforce.Rule{
		{Action: enforce.ActionEdit, Resource: "*", Effect: enforce.EffectAllow},
		{Action: enforce.ActionShell, Resource: "*", Effect: enforce.EffectAllow},
		{Action: enforce.ActionWebfetch, Resource: "*", Effect: enforce.EffectAllow},
	}
}

// attachGovernance wires the hooks a governed loop needs. Both the initial
// build and a mid-session switch go through here, so the two cannot drift.
func (a *app) attachGovernance(loop *agent.Loop) {
	loop.Task = a.task
	// A handoff advances the state machine through the store, so the loop has
	// to re-read the task or it would keep masking against the stage it has
	// already left.
	loop.TaskRefresh = func() (*workflow.Task, error) {
		return a.ctl.Get(a.task.ID)
	}
	loop.OnStateChange = func(task *workflow.Task) {
		a.task = task
		a.syncActor()
		a.retargetSession(loop)
		// The prompt follows the stage owner as well: a new stage is a new
		// identity, and its instructions are the ones that now apply.
		loop.SetSystemPrompt(a.systemPrompt())
		a.announceState()
	}
	loop.AdvanceStage = func(ctx context.Context) (bool, error) {
		return a.advanceVerification(ctx, loop)
	}
}

// advanceVerification runs the policy's gates when the workflow is waiting for
// them, reporting whether the task moved on.
//
// Verification is the one stage the runtime owns rather than the model: there
// is no tool it could call to advance it. Running the gates from inside the
// loop, the moment that stage is entered, is what stops the model taking turns
// in a stage where nothing it can do would help.
func (a *app) advanceVerification(ctx context.Context, loop *agent.Loop) (bool, error) {
	if a.ctl == nil || a.task == nil {
		return false, nil
	}
	task, err := a.ctl.Get(a.task.ID)
	if err != nil {
		return false, err
	}
	if task.State != workflow.StateVerifying {
		return false, nil
	}

	outcome := a.ctl.Verify(ctx, a.task.ID)
	if outcome.Error != nil {
		return false, outcome.Error
	}
	if outcome.Task == nil {
		return false, nil
	}
	a.task = outcome.Task
	loop.Task = outcome.Task
	a.syncActor()
	a.retargetSession(loop)
	// The next stage has a different owner, so it gets that owner's prompt.
	loop.SetSystemPrompt(a.systemPrompt())
	a.announceState()
	return outcome.Task.State != workflow.StateVerifying, nil
}

func (a *app) close() {
	if a.logFile != nil {
		a.logFile.Close()
	}
}

// requestLogger appends payloads to a JSONL file. Masking and tool_choice are
// only verifiable on the wire, so this is the way to confirm they took effect.
type requestLogger struct{ file *os.File }

func (l *requestLogger) LogRequest(providerID string, body []byte) {
	fmt.Fprintf(l.file, "{\"provider\":%q,\"at\":%q,\"request\":%s}\n",
		providerID, time.Now().Format(time.RFC3339Nano), body)
}

// build assembles the session from configuration and flags.
// build assembles the session and then settles the permissions its mode
// implies.
//
// Assembly ends in a plain session by several routes — governance off, no
// policy file, no git work tree — so the mode's rules are applied once here
// rather than at each of those returns.
func build(opts options) (*app, error) {
	a, err := assemble(opts)
	if err != nil {
		return nil, err
	}
	if !a.governed {
		a.applyPlainPermissions()
	}
	return a, nil
}

// assemble constructs the session from config, flags and the policy file.
func assemble(opts options) (*app, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home directory: %w", err)
	}
	project, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("locate working directory: %w", err)
	}

	cfg, err := config.Load(home, project)
	if err != nil && !errors.Is(err, config.ErrNoConfig) {
		return nil, err
	}
	if opts.auto {
		cfg.Auto = true
	}

	a := &app{cfg: cfg, project: project}

	// Resolve the model: flag, then agent override, then config default.
	a.modelRef = opts.model
	if a.modelRef == "" {
		a.modelRef = cfg.DefaultModel
	}

	agentDirs := config.ExpandDirs(cfg.AgentDirs, home, project)
	a.agents, err = agent.LoadRegistry(agentDirs)
	if err != nil {
		return nil, err
	}
	skillDirs := config.ExpandDirs(cfg.SkillDirs, home, project)
	a.skills, err = skill.Load(skillDirs)
	if err != nil {
		return nil, err
	}

	agentName := opts.agentName
	a.agentExplicit = agentName != ""
	if agentName == "" {
		agentName = cfg.DefaultAgent
	}
	if agentName != "" {
		def, ok := a.agents.Get(agentName)
		if !ok {
			return nil, fmt.Errorf("unknown agent %q (available: %s)",
				agentName, strings.Join(a.agents.Names(), ", "))
		}
		a.agentDef = def
		if a.modelRef == "" {
			a.modelRef = def.Model
		}
	}

	if a.modelRef == "" {
		// With nothing configured, pick the sole option if there is one.
		refs := cfg.ModelRefs()
		if len(refs) != 1 {
			return nil, errors.New("no model selected: pass -model provider/model or set defaultModel")
		}
		a.modelRef = refs[0]
	}

	providerID, model, err := cfg.ResolveModel(a.modelRef)
	if err != nil {
		return nil, err
	}
	a.modelID = model.ModelID

	if opts.logRequests != "" {
		f, err := os.OpenFile(opts.logRequests, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("open request log: %w", err)
		}
		a.logFile = f
	}

	a.provider = a.newProviderClient(providerID)

	// Whether governance *starts* on. Availability is decided separately,
	// below, by whether a policy loads: the session can be switched either
	// way at runtime, so the machinery is built even when starting plain.
	a.governed = cfg.Workflow.Enabled && !opts.noWorkflow
	a.tools = tool.NewRegistry()
	registerCoreTools(a.tools, project, a.agents)

	var rules []enforce.Rule
	if a.agentDef != nil {
		rules = a.agentDef.Permissions
	}
	a.perms = enforce.NewPermissions(rules, cfg.Auto)

	a.governor = enforce.NewNop()

	policyPath := cfg.PolicyPath(project)
	raw, err := os.Open(policyPath)
	if err != nil {
		// No policy: this session is plain-only. That is an error when
		// governance was explicitly requested, and merely a fact otherwise.
		if a.governed || opts.resumeSession != "" {
			if opts.resumeSession != "" {
				return nil, errors.New("cannot resume a session without its workflow policy")
			}
			return nil, fmt.Errorf("workflow is enabled but the policy could not be read: %w", err)
		}
		a.governed = false
		return a, nil
	}
	defer raw.Close()

	a.policy, err = workflow.ParsePolicy(raw, yaml.Unmarshal)
	if err != nil {
		// A malformed policy is always worth reporting: silently running
		// plain would hide a governance failure.
		return nil, err
	}

	clock := realClock{}
	machine := workflow.NewMachineWithTransitions(a.policy.Transitions(), clock)
	store := session.NewStore(config.StatePath(project))
	finger := enforce.NewGitFingerprinter(project)
	a.gateRun = enforce.NewGateRunner(enforce.ExecRunner{}, finger, project, clock, nil)
	a.ctl = controller.New(a.policy, machine, store, a.gateRun, finger, clock)
	a.machine = machine
	a.policyPath = policyPath

	// Fail closed: without a git work tree there is no way to detect a moving
	// tree, so gate evidence could not be trusted.
	if _, err := finger.Fingerprint(context.Background()); err != nil {
		if a.governed || opts.resumeSession != "" {
			return nil, fmt.Errorf("governed sessions require a git repository: %w", err)
		}
		// Plain-only: keep the session usable, but do not offer a switch that
		// could not produce trustworthy evidence.
		a.policy = nil
		a.ctl = nil
		return a, nil
	}
	a.policyAvailable = true

	// Starting plain: the enforcer is built and ready, but not engaged, and no
	// task is created until governance is actually switched on.
	if !a.governed {
		if opts.resumeSession != "" {
			return nil, errors.New("cannot resume a governed session while workflow is disabled")
		}
		return a, nil
	}
	a.enforcer = enforce.New(a.policy, machine, policyPath)
	a.governor = a.enforcer

	if opts.resumeSession != "" {
		if err := a.loadSession(opts.resumeSession); err != nil {
			return nil, err
		}
		return a, nil
	}

	// Reporting the configuration must not leave a task behind.
	if opts.inspectOnly {
		a.actor = &controller.Actor{}
		controller.Register(a.tools, a.ctl, a.actor)
		return a, nil
	}

	objective := opts.objective
	if objective == "" {
		objective = "interactive session"
	}
	a.task, err = a.ctl.Start(context.Background(), objective)
	if err != nil {
		return nil, err
	}

	a.actor = &controller.Actor{TaskID: a.task.ID}
	a.syncActor()
	controller.Register(a.tools, a.ctl, a.actor)
	return a, nil
}

// syncActor points the actor at the role that owns the current stage.
//
// A single interactive session performs every stage in turn, so its identity
// has to follow the workflow: otherwise the enforcer would show the session a
// handoff tool that the actor check then refuses, leaving the workflow
// unsatisfiable. Identity separation between roles needs genuinely separate
// sessions, which is why independent QA is only meaningful once delegation
// launches them.
func (a *app) syncActor() {
	if a.actor == nil || a.policy == nil || a.task == nil {
		return
	}
	role := enforce.RoleForState(a.task.State)
	if role == "" {
		role = workflow.RoleOrchestrator
	}
	if agentID := a.policy.AgentFor(role); agentID != "" {
		a.actor.AgentID = agentID
		a.syncPermissions(agentID)
		return
	}
	// With no role mapping configured, fall back to the selected agent.
	if a.agentDef != nil {
		a.actor.AgentID = a.agentDef.Name
		a.syncPermissions(a.agentDef.Name)
	}
}

// syncPermissions adopts the named agent's permission rules.
//
// Masking decides which tools the model can see; these rules decide whether a
// visible tool may actually run. Both must follow the stage, or the workflow
// becomes unsatisfiable: the implementing stage offers `edit`, and the
// orchestrator's rules deny it.
func (a *app) syncPermissions(agentID string) {
	if a.perms == nil || a.agents == nil {
		return
	}
	def, ok := a.agents.Get(agentID)
	if !ok {
		// No definition for the stage owner: leave the current rules rather
		// than silently widening or narrowing them.
		return
	}
	a.perms.SetRules(def.Permissions)
}

// newProviderClient builds the HTTP client for a configured provider. It is a
// method so a mid-session model switch can rebuild it with the same options,
// including the request log.
func (a *app) newProviderClient(providerID string) provider.Provider {
	p := a.cfg.Providers[providerID]
	opts := openaicompat.Options{
		ID:      providerID,
		Name:    p.Name,
		BaseURL: p.BaseURL,
		APIKey:  p.APIKey,
		Headers: p.Headers,
		Extra:   p.Extra,
	}
	if a.logFile != nil {
		opts.Logger = &requestLogger{file: a.logFile}
	}
	return openaicompat.New(opts)
}

// ModelRefs lists the selectable provider/model references.
func (a *app) ModelRefs() []string { return a.cfg.ModelRefs() }

// DescribeModel renders a reference for display.
func (a *app) DescribeModel(ref string) string { return a.cfg.DescribeModel(ref) }

// CurrentModel returns the active reference.
func (a *app) CurrentModel() string { return a.modelRef }

// ContextWindow reports the active model's declared context window, or 0 when
// the config does not declare one.
//
// It is only ever a declaration: an OpenAI-compatible endpoint reports how
// many tokens a request used but not how many it would accept, so the window
// has to come from config. Returning 0 rather than a guess keeps the panel
// honest about which of the two it is showing.
func (a *app) ContextWindow() int {
	_, model, err := a.cfg.ResolveModel(a.modelRef)
	if err != nil {
		return 0
	}
	return model.ContextWindow
}

// SetModel switches the live session to another provider and model.
//
// The client is rebuilt and swapped onto the loop, so the change takes effect
// on the next request rather than only relabelling the status bar. The
// conversation is deliberately kept: switching model mid-task is most useful
// for escalating a problem the current model is struggling with.
func (a *app) SetModel(ref string) error {
	if a.loop == nil {
		return errors.New("no active session")
	}
	providerID, model, err := a.cfg.ResolveModel(ref)
	if err != nil {
		return err
	}
	a.provider = a.newProviderClient(providerID)
	a.modelRef = ref
	a.modelID = model.ModelID
	a.loop.Provider = a.provider
	a.loop.Model = a.modelID
	return nil
}

// registerCoreTools adds the filesystem, search and shell tools.
func registerCoreTools(r *tool.Registry, project string, agents *agent.Registry) {
	r.Add(tool.Read{Root: project})
	r.Add(tool.Write{Root: project})
	r.Add(tool.Edit{Root: project})
	r.Add(tool.LS{Root: project})
	r.Add(tool.Glob{Root: project})
	r.Add(tool.Grep{Root: project})
	r.Add(tool.Bash{Root: project})
	if subagents := agents.Subagents(); len(subagents) > 0 {
		r.Add(tool.Task{Agents: subagents})
	}
}

// plainPrompt is the identity of an ungoverned session. It states the absence
// of a workflow rather than staying silent about it, because the conversation
// may still contain turns from when there was one.
const plainPrompt = "You are a careful software engineering assistant working in a terminal. " +
	"This session has no workflow, no stages and no gates: there is nothing to plan through, " +
	"delegate to or hand off. Carry out what is asked directly with the tools you have."

// Notes handed to the model when governance changes mid-session. They are
// short and declarative because their job is to contradict the earlier turns
// still in context, not to explain the feature.
const (
	plainSwitchNote = "Governance has been switched OFF for this session. " +
		"There is no workflow, no task and no stage, and the workflow_* tools are gone. " +
		"Disregard any earlier workflow state; carry out requests directly."
	governedSwitchNote = "Governance has been switched ON for this session. " +
		"Work now proceeds through the workflow state machine and its gates, " +
		"and only the tools listed for the current stage are available."
)

// promptAgent picks the identity whose instructions belong in the system
// prompt.
//
// Governed, that is the agent owning the current stage — the same identity
// masking and permissions already follow. Prompting as the orchestrator while
// acting as the planner is unsatisfiable: the orchestrator's own prompt says
// not to plan, while the planning stage offers only workflow_submit_plan.
//
// Plain, no stage owns the session and so no role prompt applies. A workflow
// agent's instructions ("call workflow_start, do not implement yourself")
// describe a workflow that is not running, which a model reads as a governed
// session already in progress. An agent named with -agent is kept in both
// modes, being a direct instruction from the user rather than a default.
func (a *app) promptAgent() *agent.Definition {
	if a.governed {
		if def := a.stageOwnerDef(); def != nil {
			return def
		}
		return a.agentDef
	}
	if a.agentExplicit {
		return a.agentDef
	}
	return nil
}

// stageOwnerDef returns the definition of the agent acting for the current
// stage, or nil when there is none.
func (a *app) stageOwnerDef() *agent.Definition {
	if a.agents == nil || a.actor == nil || a.actor.AgentID == "" {
		return nil
	}
	if def, ok := a.agents.Get(a.actor.AgentID); ok {
		return def
	}
	return nil
}

// systemPrompt assembles the prompt for the identity the session is currently
// running as, plus its skills.
func (a *app) systemPrompt() string {
	def := a.promptAgent()

	var parts []string
	if def != nil && def.Prompt != "" {
		parts = append(parts, def.Prompt)
	} else {
		parts = append(parts, plainPrompt)
	}

	// Skills are reference material rather than role instructions, so a plain
	// session keeps the configured agent's skills even when it drops that
	// agent's prompt.
	skillSource := def
	if skillSource == nil {
		skillSource = a.agentDef
	}
	if skillSource != nil && len(skillSource.Skills) > 0 {
		found, missing := a.skills.Resolve(skillSource.Skills)
		if prompt := skill.Prompt(found); prompt != "" {
			parts = append(parts, prompt)
		}
		// A referenced-but-absent skill is usually a typo, so say so rather
		// than silently dropping it.
		for _, name := range missing {
			fmt.Fprintf(os.Stderr, "agentwarden: agent %q references unknown skill %q\n",
				skillSource.Name, name)
		}
	}
	return strings.Join(parts, "\n\n")
}

// newLoop builds the agent loop for this session.
func (a *app) newLoop(observer agent.Observer, confirmer agent.Confirmer) *agent.Loop {
	role := workflow.Role("")
	if a.governed && a.task != nil {
		role = enforce.RoleForState(a.task.State)
		a.syncActor()
	}
	loop := &agent.Loop{
		Provider:    a.provider,
		Model:       a.modelID,
		Tools:       a.tools,
		Governor:    a.governor,
		Permissions: a.perms,
		Confirmer:   confirmer,
		Observer:    observer,
		Task:        a.task,
		Session:     &enforce.Session{Role: role, AgentID: a.actorAgentID()},
		// Assembled after syncActor above, so a governed session is prompted
		// as the agent that owns its current stage.
		SystemPrompt: a.systemPrompt(),
	}
	if len(a.conversation) > 0 {
		loop.SetMessages(a.conversation)
		loop.SetSystemPrompt(a.systemPrompt())
	}
	if a.governed {
		a.attachGovernance(loop)
	}
	return loop
}

// retargetSession updates the loop's session identity after a stage change and
// clears the per-stage counters, so budgets and escalation apply per stage.
func (a *app) retargetSession(loop *agent.Loop) {
	if loop.Session == nil || a.task == nil {
		return
	}
	newRole := enforce.RoleForState(a.task.State)
	if loop.Session.Role == newRole {
		return
	}
	loop.Session.Role = newRole
	loop.Session.AgentID = a.actorAgentID()
	loop.Session.HandedOff = false
	enforce.ResetStateCounters(loop.Session)
}

func (a *app) actorAgentID() string {
	if a.actor != nil {
		return a.actor.AgentID
	}
	if a.agentDef != nil {
		return a.agentDef.Name
	}
	return ""
}

// operatorActor is the identity a command-line action takes.
//
// The person at the terminal owns the workflow, so they act as the
// orchestrator: resuming and cancelling are the orchestrator's to perform, and
// there is no more authoritative actor than the operator.
func (a *app) operatorActor() string {
	if a.policy == nil {
		return ""
	}
	return a.policy.AgentFor(workflow.RoleOrchestrator)
}

// printTasks shows the persisted task checkpoints in newest-first order.
func (a *app) printTasks() error {
	return printTasksAt(a.project)
}

func printTasksAt(project string) error {
	tasks, err := loadTasks(project)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Println("no governed tasks recorded for this project")
		return nil
	}

	for _, task := range tasks {
		fmt.Printf("%s  %-18s %s  %s\n", task.ID, task.State,
			task.UpdatedAt.Format("2006-01-02 15:04"), truncateLine(task.Objective, 48))
	}
	return nil
}

func loadTasks(project string) ([]*workflow.Task, error) {
	store := session.NewStore(config.StatePath(project))
	ids, err := store.List()
	if err != nil {
		return nil, err
	}

	tasks := make([]*workflow.Task, 0, len(ids))
	for _, id := range ids {
		task, err := store.Load(id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agentwarden: could not read task %s: %v\n", id, err)
			continue
		}
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
	})
	return tasks, nil
}

// sessionPickerModel is the small pre-session UI used by `agentwarden resume`.
// It exits before the normal agent TUI is built, so selecting a row can feed
// the same persisted-session path as an explicit ID.
type sessionPickerModel struct {
	picker   *tui.Picker
	width    int
	selected string
}

func newSessionPicker(tasks []*workflow.Task) *sessionPickerModel {
	choices := make([]tui.Choice, 0, len(tasks))
	for _, task := range tasks {
		label := fmt.Sprintf("%-18s %s  %s", task.State,
			task.UpdatedAt.Format("2006-01-02 15:04"), truncateLine(task.Objective, 48))
		choices = append(choices, tui.Choice{Value: task.ID, Label: label})
	}
	picker := tui.NewPicker(12)
	picker.Open("Resume a session", choices, "")
	return &sessionPickerModel{picker: picker, width: 100}
}

func (m *sessionPickerModel) Init() tea.Cmd { return nil }

func (m *sessionPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			m.picker.Move(-1)
		case "down", "j":
			m.picker.Move(1)
		case "enter":
			if choice, ok := m.picker.Selected(); ok {
				m.selected = choice.Value
				return m, tea.Quit
			}
		case "esc", "ctrl+c":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *sessionPickerModel) View() string {
	return "\n" + m.picker.View(m.width) + "\n"
}

func chooseSession(project string) (string, error) {
	tasks, err := loadTasks(project)
	if err != nil {
		return "", err
	}
	if len(tasks) == 0 {
		fmt.Println("no governed tasks recorded for this project")
		return "", nil
	}

	model := newSessionPicker(tasks)
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := program.Run(); err != nil {
		return "", err
	}
	return model.selected, nil
}

// truncateLine shortens text to one readable column.
func truncateLine(text string, n int) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if len(text) <= n {
		return text
	}
	return text[:n-1] + "…"
}

// cancelTask closes a task the operator does not want finished.
func (a *app) cancelTask(taskID string) error {
	if a.ctl == nil {
		return errors.New("no workflow policy is loaded for this project")
	}
	task, err := a.ctl.Cancel(taskID, a.operatorActor(), "cancelled by the operator")
	if err != nil {
		return err
	}
	fmt.Printf("task %s is now %s\n", task.ID, task.State)
	return nil
}

// resumeTask reopens a blocked task.
//
// A blocked task has no tool the model could call to leave that state, so
// without this it would stay blocked for good.
func (a *app) resumeTask(taskID string) error {
	if a.ctl == nil {
		return errors.New("no workflow policy is loaded for this project")
	}
	task, err := a.ctl.Resume(taskID, a.operatorActor())
	if err != nil {
		return err
	}
	fmt.Printf("task %s is now %s\n", task.ID, task.State)
	return nil
}

// loadSession attaches a persisted task and conversation to the next session.
func (a *app) loadSession(taskID string) error {
	if a.ctl == nil || a.policy == nil {
		return errors.New("no workflow policy is loaded for this project")
	}
	task, err := a.ctl.Get(taskID)
	if err != nil {
		return err
	}
	if task.PolicyHash != a.policy.Hash() {
		return fmt.Errorf("session %s was created with a different workflow policy", task.ID)
	}
	if task.State == workflow.StateBlocked {
		task, err = a.ctl.Resume(task.ID, a.operatorActor())
		if err != nil {
			return err
		}
	}
	conversation, err := session.NewStore(config.StatePath(a.project)).LoadMessages(task.ID)
	if err != nil {
		return err
	}
	a.task = task
	a.conversation = conversation
	a.actor = &controller.Actor{TaskID: task.ID}
	a.syncActor()
	controller.Register(a.tools, a.ctl, a.actor)
	return nil
}

// printConfig reports the resolved configuration, for debugging setup.
func (a *app) printConfig() error {
	fmt.Printf("model:      %s\n", a.modelRef)
	fmt.Printf("governed:   %t\n", a.governed)
	fmt.Printf("auto:       %t\n", a.cfg.Auto)
	if a.agentDef != nil {
		fmt.Printf("agent:      %s (%s)\n", a.agentDef.Name, a.agentDef.Mode)
	}
	if sources := a.cfg.Sources(); len(sources) > 0 {
		fmt.Printf("config:     %s\n", strings.Join(sources, ", "))
	} else {
		fmt.Printf("config:     (defaults; no agentwarden.json found)\n")
	}
	fmt.Printf("agents:     %s\n", orNone(strings.Join(a.agents.Names(), ", ")))
	fmt.Printf("skills:     %s\n", orNone(strings.Join(a.skills.Names(), ", ")))
	fmt.Printf("tools:      %s\n", strings.Join(a.tools.Names(), ", "))
	if a.governed {
		fmt.Printf("policy:     %s\n", a.cfg.PolicyPath(a.project))
		fmt.Printf("policyHash: %s\n", a.policy.Hash()[:12])
		var gates []string
		for _, g := range a.policy.Gates {
			marker := ""
			if !g.IsRequired() {
				marker = " (optional)"
			}
			gates = append(gates, g.ID+marker)
		}
		fmt.Printf("gates:      %s\n", strings.Join(gates, ", "))
		if a.task != nil {
			fmt.Printf("task:       %s (%s)\n", a.task.ID, a.task.State)
		}
		if ids, err := session.NewStore(config.StatePath(a.project)).List(); err == nil {
			fmt.Printf("tasks:      %d recorded\n", len(ids))
		}
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// runOnce executes a single prompt without a TUI, for scripting.
//
// It is always ungoverned in spirit: there is no terminal to confirm with, so
// tool calls that would prompt are approved.
func (a *app) runOnce(prompt string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	loop := a.newLoop(&stdoutObserver{}, agent.AllowConfirmer{})
	runner := a.newRunner(loop)
	result, err := runner.Run(ctx, prompt)
	if err != nil {
		return err
	}
	if a.governed && a.task != nil {
		a.reportOutcome()
	}
	fmt.Println()
	if result.Blocked > 0 {
		fmt.Fprintf(os.Stderr, "agentwarden: %d action(s) were blocked by the workflow\n", result.Blocked)
	}
	return nil
}

// newRunner wraps a loop so verification runs after the model's turn.
func (a *app) newRunner(loop *agent.Loop) Runner {
	if !a.governed {
		return loop
	}
	return &governedRunner{app: a, loop: loop}
}

// Runner executes a prompt.
type Runner interface {
	Run(ctx context.Context, prompt string) (agent.Result, error)
}

// reportOutcome prints the final governed state and any gate evidence, so a
// scripted run can be judged from its output.
func (a *app) reportOutcome() {
	task, err := a.ctl.Get(a.task.ID)
	if err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "\nworkflow: %s\n", task.State)
	for _, gate := range a.policy.Gates {
		receipt, ok := task.Receipts[gate.ID]
		switch {
		case !ok:
			fmt.Fprintf(os.Stderr, "  · %s not run\n", gate.ID)
		case receipt.Success:
			fmt.Fprintf(os.Stderr, "  ✓ %s passed\n", gate.ID)
		default:
			fmt.Fprintf(os.Stderr, "  ✗ %s failed (%s)\n", gate.ID, receipt.FailureReason)
		}
	}
}

// stdoutObserver streams a one-shot run to the terminal.
type stdoutObserver struct{}

func (stdoutObserver) TextDelta(text string) { fmt.Print(text) }

func (stdoutObserver) ToolStarted(call tool.Call) {
	fmt.Fprintf(os.Stderr, "\n· %s\n", call.Name)
}

func (stdoutObserver) ToolFinished(tool.Call, tool.Result) {}

func (stdoutObserver) Blocked(call tool.Call, decision enforce.Decision) {
	fmt.Fprintf(os.Stderr, "\n⚠ blocked %s: %s\n", call.Name, decision.Reason)
}

func (stdoutObserver) TurnFinished(*provider.Usage) {}

// runTUI starts the interactive interface.
func (a *app) runTUI() error {
	// The gate list is needed whenever governance is *available*, not only
	// when it starts on: switching it on mid-session must show the gates.
	var gates []workflow.Gate
	if a.policyAvailable {
		gates = a.policy.GatesFor(workflow.StateVerifying)
	}

	// Resolve the terminal theme before the program starts. Detection writes
	// a query to the terminal and reads the reply; done later, Bubble Tea
	// would consume that reply as keyboard input.
	glamourStyle := tui.DetectTheme()

	model := tui.New(tui.Options{
		GlamourStyle:  glamourStyle,
		Switcher:      a,
		Models:        a,
		Gates:         gates,
		Governed:      a.governed,
		Auto:          a.cfg.Auto,
		ModelName:     a.modelRef,
		State:         a.WorkflowState(),
		Stages:        a.stages(),
		ContextWindow: a.ContextWindow(),
		Messages:      a.conversation,
	})

	// Rebuild the controller so gate progress reports into the UI: a long
	// suite should show live status rather than appearing to hang. Done for
	// any session that could become governed.
	if a.policyAvailable {
		clock := realClock{}
		finger := enforce.NewGitFingerprinter(a.project)
		a.gateRun = enforce.NewGateRunner(
			enforce.ExecRunner{}, finger, a.project, clock, model.GateProgress())
		a.machine = workflow.NewMachineWithTransitions(a.policy.Transitions(), clock)
		store := session.NewStore(config.StatePath(a.project))
		a.ctl = controller.New(a.policy, a.machine, store, a.gateRun, finger, clock)
		if a.actor != nil {
			controller.Register(a.tools, a.ctl, a.actor)
		}
	}

	a.reportState = model.StateReporter()

	loop := a.newLoop(model.Observer(), &tuiConfirmer{})
	a.loop = loop
	// Always the governed runner: it checks the live mode per run, so a switch
	// takes effect without rebuilding anything.
	model.SetRunner(&governedRunner{app: a, loop: loop})

	// Mouse cell motion enables click-to-toggle on the gate pane. Keyboard
	// equivalents exist too, so this is an addition rather than a dependency.
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := program.Run()
	return err
}

// stages resolves the workflow spine for the status panel, naming the agent
// that owns each stage.
//
// The spine is derived from the policy's own transition graph rather than
// hardcoded, so a project that declares custom states sees its own stages. It
// is empty when no policy loaded, which is what makes the panel drop the
// section instead of drawing a workflow that is not in force.
func (a *app) stages() []tui.Stage {
	if !a.policyAvailable || a.policy == nil {
		return nil
	}
	var out []tui.Stage
	for _, state := range workflow.Pipeline(a.policy.Transitions()) {
		stage := tui.Stage{State: state}
		if role := enforce.RoleForState(state); role != "" {
			stage.Agent = a.policy.AgentFor(role)
		}
		out = append(out, stage)
	}
	return out
}

// tuiConfirmer approves in the TUI. Interactive confirmation prompts are a
// follow-up; until then a call that would ask is refused unless --auto is set,
// which errs toward not acting without consent.
type tuiConfirmer struct{}

func (tuiConfirmer) Confirm(context.Context, tool.Call, string, string) (bool, error) {
	return false, nil
}

// governedRunner runs a prompt and then advances verification if the workflow
// reached that stage, so gates run outside the tool call that triggered them.
type governedRunner struct {
	app  *app
	loop *agent.Loop
}

func (r *governedRunner) Run(ctx context.Context, prompt string) (agent.Result, error) {
	// Mode is read per run, so ctrl+g takes effect on the next prompt without
	// rebuilding the runner.
	if !r.app.governed {
		return r.loop.Run(ctx, prompt)
	}
	r.app.syncActor()
	r.app.retargetSession(r.loop)
	result, err := r.loop.Run(ctx, prompt)
	if saveErr := r.app.saveConversation(r.loop); saveErr != nil {
		err = errors.Join(err, saveErr)
	}
	if err != nil || r.app.ctl == nil || r.app.task == nil {
		return result, err
	}

	// The loop runs the gates itself the moment verification is entered, so
	// this is only the safety net for a run that exited before reaching that
	// check — a step-limit exhaustion, or a cancelled turn. Verify no-ops
	// unless the task really is waiting to be verified.
	for {
		advanced, verifyErr := r.app.advanceVerification(ctx, r.loop)
		if verifyErr != nil {
			return result, verifyErr
		}
		if !advanced || !r.app.policy.AutoAdvance() {
			break
		}
	}
	return result, nil
}

func (a *app) saveConversation(loop *agent.Loop) error {
	if a.task == nil {
		return nil
	}
	messages := loop.Messages()
	if err := session.NewStore(config.StatePath(a.project)).SaveMessages(a.task.ID, messages); err != nil {
		return err
	}
	a.conversation = messages
	return nil
}
