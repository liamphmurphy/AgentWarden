// Command agentwarden is a terminal coding agent with a native workflow enforcer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
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
	inspectOnly bool
	agentName   string
	noWorkflow  bool
	auto        bool
	logRequests string
	objective   string
	showConfig  bool
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

Flags:
  -model string      provider/model to use (overrides config)
  -agent string      agent to run as (overrides config)
  -no-workflow       run ungoverned, skipping the state machine and gates
  -auto              auto-approve tool calls that would otherwise prompt
  -log-requests path  append every provider payload to a JSONL file
  -objective string  objective for a new governed task
  -config            print the resolved configuration and exit

Governance is on only when the config enables it and a policy file exists.
`)
}

func run() error {
	flag.Usage = usage

	// `agentwarden run "prompt"` is a subcommand; everything else is the TUI.
	args := os.Args[1:]
	oneShot := false
	if len(args) > 0 && args[0] == "run" {
		oneShot = true
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	prompt := strings.Join(fs.Args(), " ")

	opts.inspectOnly = opts.showConfig

	app, err := build(opts, oneShot)
	if err != nil {
		return err
	}
	defer app.close()

	if opts.showConfig {
		return app.printConfig()
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
	cfg      *config.Config
	project  string
	governed bool
	policy   *workflow.Policy
	ctl      *controller.Controller
	tools    *tool.Registry
	governor enforce.Governor
	perms    *enforce.Permissions
	provider provider.Provider
	modelID  string
	modelRef string
	agentDef *agent.Definition
	task     *workflow.Task
	gateRun  *enforce.GateRunner
	logFile  *os.File
	actor    *controller.Actor
	skills   *skill.Set
	agents   *agent.Registry

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
		// Without a governor there is nothing to re-target on a state change.
		a.loop.TaskRefresh = nil
		a.loop.OnStateChange = nil
		return nil
	}
	if !a.policyAvailable {
		return errors.New("no workflow policy is loaded for this project")
	}

	// A session that started plain has no task yet.
	if a.task == nil {
		task, err := a.ctl.Start("interactive session")
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
	a.loop.Task = a.task
	a.loop.TaskRefresh = func() (*workflow.Task, error) { return a.ctl.Get(a.task.ID) }
	a.loop.OnStateChange = func(task *workflow.Task) {
		a.task = task
		a.syncActor()
		a.retargetSession(a.loop)
	}
	a.syncActor()
	a.retargetSession(a.loop)
	return nil
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
func build(opts options, oneShot bool) (*app, error) {
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
		if a.governed {
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
		if a.governed {
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
		return a, nil
	}
	a.enforcer = enforce.New(a.policy, machine, policyPath)
	a.governor = a.enforcer

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
	a.task, err = a.ctl.Start(objective)
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
		return
	}
	// With no role mapping configured, fall back to the selected agent.
	if a.agentDef != nil {
		a.actor.AgentID = a.agentDef.Name
	}
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

// systemPrompt assembles the agent prompt and its skills.
func (a *app) systemPrompt() string {
	var parts []string
	if a.agentDef != nil && a.agentDef.Prompt != "" {
		parts = append(parts, a.agentDef.Prompt)
	} else {
		parts = append(parts, "You are a careful software engineering assistant working in a terminal.")
	}
	if a.agentDef != nil && len(a.agentDef.Skills) > 0 {
		found, missing := a.skills.Resolve(a.agentDef.Skills)
		if prompt := skill.Prompt(found); prompt != "" {
			parts = append(parts, prompt)
		}
		// A referenced-but-absent skill is usually a typo, so say so rather
		// than silently dropping it.
		for _, name := range missing {
			fmt.Fprintf(os.Stderr, "agentwarden: agent %q references unknown skill %q\n",
				a.agentDef.Name, name)
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
		Provider:     a.provider,
		Model:        a.modelID,
		Tools:        a.tools,
		Governor:     a.governor,
		Permissions:  a.perms,
		Confirmer:    confirmer,
		Observer:     observer,
		Task:         a.task,
		Session:      &enforce.Session{Role: role, AgentID: a.actorAgentID()},
		SystemPrompt: a.systemPrompt(),
	}
	if a.governed {
		// A handoff advances the state machine through the store, so the loop
		// has to re-read the task or it would keep masking against the stage
		// it has already left.
		loop.TaskRefresh = func() (*workflow.Task, error) {
			return a.ctl.Get(a.task.ID)
		}
		loop.OnStateChange = func(task *workflow.Task) {
			a.task = task
			a.syncActor()
			a.retargetSession(loop)
		}
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
		GlamourStyle: glamourStyle,
		Switcher:     a,
		Models:       a,
		Gates:        gates,
		Governed:     a.governed,
		Auto:         a.cfg.Auto,
		ModelName:    a.modelRef,
		State:        a.WorkflowState(),
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
	if err != nil || r.app.ctl == nil || r.app.task == nil {
		return result, err
	}

	// Drive verification whenever the workflow is waiting for it. Running it
	// here rather than inside the handoff tool keeps a long suite from
	// blocking a single tool result for its whole timeout.
	for {
		task, loadErr := r.app.ctl.Get(r.app.task.ID)
		if loadErr != nil || task.State != workflow.StateVerifying {
			break
		}
		outcome := r.app.ctl.Verify(ctx, r.app.task.ID)
		if outcome.Error != nil {
			return result, outcome.Error
		}
		if outcome.Task != nil {
			r.app.task = outcome.Task
			r.loop.Task = outcome.Task
			r.app.syncActor()
			r.app.retargetSession(r.loop)
		}
		if !r.app.policy.AutoAdvance() {
			break
		}
		if outcome.Task == nil || outcome.Task.State != workflow.StateVerifying {
			break
		}
	}
	return result, nil
}
