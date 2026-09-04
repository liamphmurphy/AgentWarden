package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/lmurphy/agentwarden/internal/enforce"
	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/tool"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

// maxSteps bounds one Run so a model that keeps calling tools cannot spin
// forever.
const maxSteps = 40

// maxIdenticalFailures aborts a run when a model repeats the same failing call
// with the same result. Small models get stuck in exactly this way, and
// failing fast with the real error is far more useful than burning every step.
const maxIdenticalFailures = 4

// Observer receives loop progress for rendering. All methods may be called
// from the loop goroutine.
type Observer interface {
	// TextDelta reports incremental assistant output.
	TextDelta(text string)
	// ToolStarted reports a call about to execute.
	ToolStarted(call tool.Call)
	// ToolFinished reports the outcome of a call.
	ToolFinished(call tool.Call, result tool.Result)
	// Blocked reports an enforcer refusal, so the UI can explain the block.
	Blocked(call tool.Call, decision enforce.Decision)
	// TurnFinished reports the end of one model turn.
	TurnFinished(usage *provider.Usage)
}

// NopObserver discards progress.
type NopObserver struct{}

func (NopObserver) TextDelta(string)                    {}
func (NopObserver) ToolStarted(tool.Call)               {}
func (NopObserver) ToolFinished(tool.Call, tool.Result) {}
func (NopObserver) Blocked(tool.Call, enforce.Decision) {}
func (NopObserver) TurnFinished(*provider.Usage)        {}

// Confirmer decides whether a call needing confirmation may proceed. In auto
// mode the permission layer never returns ask, so this is not consulted.
type Confirmer interface {
	Confirm(ctx context.Context, call tool.Call, action, resource string) (bool, error)
}

// DenyConfirmer refuses anything that would need confirmation, which is the
// safe default for non-interactive runs.
type DenyConfirmer struct{}

// Confirm always declines.
func (DenyConfirmer) Confirm(context.Context, tool.Call, string, string) (bool, error) {
	return false, nil
}

// AllowConfirmer approves everything, used by `agentwarden run` where there is no
// terminal to ask.
type AllowConfirmer struct{}

// Confirm always approves.
func (AllowConfirmer) Confirm(context.Context, tool.Call, string, string) (bool, error) {
	return true, nil
}

// Loop runs the model/tool cycle for one session.
type Loop struct {
	Provider    provider.Provider
	Model       string
	Tools       *tool.Registry
	Governor    enforce.Governor
	Permissions *enforce.Permissions
	Confirmer   Confirmer
	Observer    Observer

	// Task is the governed task. It is required when the Governor is active
	// and ignored otherwise, so an ungoverned session needs no task at all.
	Task *workflow.Task
	// Session carries the per-session counters the enforcer mutates.
	Session *enforce.Session

	// SystemPrompt is prepended once, before the conversation.
	SystemPrompt string

	// TaskRefresh, when set, reloads the authoritative task. A handoff tool
	// advances the state machine through the store, so without this the loop
	// would keep masking against the stage it has already left.
	TaskRefresh func() (*workflow.Task, error)
	// OnStateChange, when set, is called after the task's state advances, so
	// the caller can retarget the session's identity to the new stage owner.
	OnStateChange func(task *workflow.Task)
	// AdvanceStage, when set, asks the caller to advance a stage that belongs
	// to the runtime rather than the model — running the policy's gates for
	// `verifying`. It reports whether the workflow actually moved on.
	//
	// This exists so verification happens the moment that stage is entered.
	// Running it only after the whole run finished left the model taking turn
	// after turn in a stage that offered it no tool capable of advancing
	// anything, until it hit the step limit and narrated its confusion.
	AdvanceStage func(ctx context.Context) (bool, error)

	// messages is the running conversation.
	messages []provider.Message
	// lastFailure and failureStreak detect a model stuck repeating one call.
	lastFailure   string
	failureStreak int
	// advanced records that the runtime moved the workflow on, so the loop
	// takes another pass rather than counting it as a model step.
	advanced bool
}

// Messages returns the conversation so far.
func (l *Loop) Messages() []provider.Message {
	return append([]provider.Message(nil), l.messages...)
}

// Reset clears the conversation, keeping configuration.
func (l *Loop) Reset() { l.messages = nil }

// SetMessages restores a conversation checkpoint, keeping configuration.
func (l *Loop) SetMessages(messages []provider.Message) {
	l.messages = append([]provider.Message(nil), messages...)
}

// SetSystemPrompt replaces the system prompt, including on a conversation
// already under way.
//
// Assigning the field alone would not be enough: the prompt is copied into the
// message list before the first turn and never re-read, so a session that
// started under one identity would keep being instructed as that identity for
// the rest of its life. Switching governance off has to be able to retract the
// workflow instructions it switched on.
func (l *Loop) SetSystemPrompt(prompt string) {
	l.SystemPrompt = prompt
	if len(l.messages) > 0 && l.messages[0].Role == provider.RoleSystem {
		if prompt == "" {
			l.messages = l.messages[1:]
			return
		}
		l.messages[0].Text = prompt
		return
	}
	if prompt != "" {
		l.messages = append([]provider.Message{{
			Role: provider.RoleSystem,
			Text: prompt,
		}}, l.messages...)
	}
}

// Note appends an out-of-band message the model will read on its next turn.
//
// Rewriting the system prompt does not undo what the conversation already
// says: earlier turns may contain workflow tool calls and their results, and a
// small model reads that recent history as the current situation. A note is
// how a mid-session change of rules is stated where the model is actually
// looking.
func (l *Loop) Note(text string) {
	if text == "" {
		return
	}
	l.messages = append(l.messages, provider.Message{
		Role:     provider.RoleUser,
		Text:     text,
		Internal: true,
	})
}

// observer returns a non-nil Observer.
func (l *Loop) observer() Observer {
	if l.Observer == nil {
		return NopObserver{}
	}
	return l.Observer
}

// task returns a usable task even when ungoverned, so the Governor interface
// can be called unconditionally.
func (l *Loop) task() *workflow.Task {
	if l.Task != nil {
		return l.Task
	}
	return &workflow.Task{State: workflow.StatePlanning, Receipts: map[string]workflow.Receipt{}}
}

func (l *Loop) session() *enforce.Session {
	if l.Session == nil {
		l.Session = &enforce.Session{}
	}
	return l.Session
}

// Result summarizes one Run.
type Result struct {
	// Text is the assistant's final prose.
	Text string
	// Steps is how many model turns were taken.
	Steps int
	// Blocked counts enforcer refusals during the run.
	Blocked int
	// Usage accumulates token counts when the endpoint reports them.
	Usage provider.Usage
}

// Run sends a user prompt and drives the loop until the model stops calling
// tools.
func (l *Loop) Run(ctx context.Context, prompt string) (Result, error) {
	if l.Provider == nil {
		return Result{}, errors.New("loop has no provider")
	}
	if len(l.messages) == 0 && l.SystemPrompt != "" {
		l.messages = append(l.messages, provider.Message{
			Role: provider.RoleSystem,
			Text: l.SystemPrompt,
		})
	}
	if prompt != "" {
		l.messages = append(l.messages, provider.Message{Role: provider.RoleUser, Text: prompt})
	}

	var result Result
	for step := 0; step < maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		// Some stages are not the model's to act in. Rather than send a
		// request the model cannot usefully answer, hand the stage to the
		// runtime; if nobody can advance it, stop and say so.
		if stalled, err := l.stageBelongsToRuntime(ctx); err != nil {
			return result, err
		} else if stalled != "" {
			result.Text = stalled
			return result, nil
		} else if l.advanced {
			l.advanced = false
			continue
		}

		result.Steps++

		text, calls, usage, err := l.turn(ctx)
		if err != nil {
			return result, err
		}
		if usage != nil {
			result.Usage.PromptTokens += usage.PromptTokens
			result.Usage.CompletionTokens += usage.CompletionTokens
		}
		l.observer().TurnFinished(usage)

		// Record what the model produced before acting on it, so the
		// conversation stays coherent even if a tool is refused.
		assistant := provider.Message{Role: provider.RoleAssistant, Text: text}
		assistant.ToolCalls = calls
		l.messages = append(l.messages, assistant)

		if len(calls) == 0 {
			result.Text = text
			// A turn that ends without the required handoff is caught here;
			// the loop owns the turn boundary, so no external hook is needed.
			if decision := l.Governor.OnTurnEnd(l.task(), l.session(), nil); !decision.Allow {
				result.Blocked++
				l.session().ForcedTool = decision.ForceTool
				l.messages = append(l.messages, provider.Message{
					Role:     provider.RoleUser,
					Text:     decision.Correction,
					Internal: true,
				})
				continue
			}
			return result, nil
		}

		blocked, names := l.executeCalls(ctx, calls)
		result.Blocked += blocked

		if isStuck, reason := l.stuck(); isStuck {
			result.Text = text
			return result, fmt.Errorf(
				"the model repeated the same failing call %d times and made no progress: %s",
				l.failureStreak, reason)
		}

		if decision := l.Governor.OnTurnEnd(l.task(), l.session(), names); !decision.Allow {
			result.Blocked++
			l.session().ForcedTool = decision.ForceTool
			l.messages = append(l.messages, provider.Message{
				Role:     provider.RoleUser,
				Text:     decision.Correction,
				Internal: true,
			})
		}
	}
	return result, fmt.Errorf("gave up after %d steps without a final answer", maxSteps)
}

// stageBelongsToRuntime handles a stage the model cannot advance.
//
// It returns a message to finish the run with when the workflow is stuck
// there, "" when the loop should carry on, and sets l.advanced when the stage
// was handed to the runtime and moved.
func (l *Loop) stageBelongsToRuntime(ctx context.Context) (string, error) {
	if !l.Governor.Enabled() {
		return "", nil
	}
	visible := l.Governor.VisibleTools(l.task(), l.session(), l.Tools.Defs())
	if canAdvanceWorkflow(visible) {
		return "", nil
	}

	state := l.task().State
	if l.AdvanceStage != nil {
		advanced, err := l.AdvanceStage(ctx)
		if err != nil {
			return "", err
		}
		l.refreshTask()
		// A hook that claims progress without making any would spin here to
		// the step limit, so the state is checked rather than trusted.
		if advanced && l.task().State != state {
			l.advanced = true
			return "", nil
		}
	}
	return stageStall(l.task()), nil
}

// canAdvanceWorkflow reports whether any visible tool could move the workflow
// on. Status and history only report, so they do not count.
//
// This is decided from the masked tool list rather than from the state name,
// so a policy that declares its own states is judged by what it actually
// exposes.
func canAdvanceWorkflow(visible []provider.ToolDef) bool {
	for _, def := range visible {
		if !enforce.IsWorkflowTool(def.Name) {
			continue
		}
		if def.Name == enforce.ToolStatus || def.Name == enforce.ToolHistory {
			continue
		}
		return true
	}
	return false
}

// stageStall explains why the run ended without asking the model anything.
//
// The task ID is included where the way out is an operator command, since the
// ID otherwise appears only in the transcript of the session that made it.
func stageStall(task *workflow.Task) string {
	switch task.State {
	case workflow.StateComplete:
		return "The task is complete; there is nothing further to do."
	case workflow.StateCancelled:
		return "The task was cancelled."
	case workflow.StateBlocked:
		return fmt.Sprintf(
			"The task is blocked and nothing here can unblock it. "+
				"Resume it with `agentwarden -resume %s`, or drop it with `agentwarden -cancel %s`.",
			task.ID, task.ID)
	default:
		return fmt.Sprintf(
			"The workflow is in %s, which the runtime owns rather than the model, and it "+
				"did not advance. Check the gate output, or drop the task with "+
				"`agentwarden -cancel %s`.", task.State, task.ID)
	}
}

// turn issues one request and collects its output.
func (l *Loop) turn(ctx context.Context) (string, []provider.ToolCall, *provider.Usage, error) {
	req := l.buildRequest()

	stream, err := l.Provider.Stream(ctx, req)
	if err != nil {
		return "", nil, nil, err
	}
	defer stream.Close()

	var (
		text  strings.Builder
		calls []provider.ToolCall
		usage *provider.Usage
	)
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, nil, err
		}
		switch event.Kind {
		case provider.EventText:
			text.WriteString(event.Text)
			l.observer().TextDelta(event.Text)
		case provider.EventToolCall:
			if event.ToolCall != nil {
				calls = append(calls, *event.ToolCall)
			}
		case provider.EventDone:
			usage = event.Usage
		}
	}
	return text.String(), calls, usage, nil
}

// buildRequest assembles the next payload, applying masking, the tool_choice
// pin and the state banner. Everything the enforcer decides lands here, which
// is why these are inputs to the request rather than post-hoc corrections.
func (l *Loop) buildRequest() provider.Request {
	all := l.Tools.Defs()
	visible := l.Governor.VisibleTools(l.task(), l.session(), all)

	messages := append([]provider.Message(nil), l.messages...)
	// The banner goes next to the newest message rather than in the distant
	// system prompt, because small models lose early instructions.
	if banner := l.Governor.Banner(l.task(), l.session(), visible); banner != "" {
		messages = append(messages, provider.Message{Role: provider.RoleUser, Text: banner})
	}

	return provider.Request{
		Model:      l.Model,
		Messages:   messages,
		Tools:      visible,
		ToolChoice: l.Governor.ToolChoice(l.task(), l.session()),
	}
}

// executeCalls runs the model's requested calls, returning how many were
// refused and which tool names were attempted.
func (l *Loop) executeCalls(ctx context.Context, calls []provider.ToolCall) (blocked int, names []string) {
	for _, call := range calls {
		names = append(names, call.Name)
		toolCall := tool.Call{ID: call.ID, Name: call.Name, Args: call.Args}

		// Capture the handoff expected *before* the call runs: a successful
		// handoff advances the state, so checking afterwards would compare
		// against the next stage's tool.
		expectedHandoff := enforce.HandoffTool(l.task().State)

		// The pin is consumed once the model complies, so a single forced
		// turn does not lock the session into that tool forever.
		if l.session().ForcedTool == call.Name {
			l.session().ForcedTool = ""
		}

		if decision := l.Governor.Intercept(l.task(), l.session(), call); !decision.Allow {
			blocked++
			l.observer().Blocked(toolCall, decision)
			if decision.ForceTool != "" {
				l.session().ForcedTool = decision.ForceTool
			}
			l.appendToolResult(call, tool.Result{Content: decision.Correction, IsError: true})
			continue
		}

		result, err := l.runTool(ctx, toolCall)
		if err != nil {
			return blocked, names
		}
		if call.Name == expectedHandoff && !result.IsError {
			l.session().HandedOff = true
		}
		l.trackFailure(call.Name, result)
		l.appendToolResult(call, result)
		l.refreshTask()
	}
	return blocked, names
}

// refreshTask reloads the task and notifies the caller when the stage changed.
func (l *Loop) refreshTask() {
	if l.TaskRefresh == nil {
		return
	}
	latest, err := l.TaskRefresh()
	if err != nil || latest == nil {
		return
	}
	previous := workflow.State("")
	if l.Task != nil {
		previous = l.Task.State
	}
	l.Task = latest
	if latest.State != previous && l.OnStateChange != nil {
		l.OnStateChange(latest)
	}
}

// trackFailure counts consecutive identical failures so a stuck model can be
// stopped with a useful message.
func (l *Loop) trackFailure(name string, result tool.Result) {
	if !result.IsError {
		l.lastFailure = ""
		l.failureStreak = 0
		return
	}
	signature := name + "\x00" + result.Content
	if signature == l.lastFailure {
		l.failureStreak++
		return
	}
	l.lastFailure = signature
	l.failureStreak = 1
}

// stuck reports whether the model is repeating one failing call, and the error
// it keeps hitting.
func (l *Loop) stuck() (bool, string) {
	if l.failureStreak < maxIdenticalFailures {
		return false, ""
	}
	_, content, _ := strings.Cut(l.lastFailure, "\x00")
	return true, content
}

// runTool applies permissions and then executes the tool.
func (l *Loop) runTool(ctx context.Context, call tool.Call) (tool.Result, error) {
	impl, ok := l.Tools.Get(call.Name)
	if !ok {
		return tool.Result{
			Content: fmt.Sprintf("unknown tool %q", call.Name),
			IsError: true,
		}, nil
	}

	if l.Permissions != nil {
		action, resource := describeCall(call)
		switch l.Permissions.Evaluate(action, resource) {
		case enforce.EffectDeny:
			return tool.Result{
				Content: fmt.Sprintf("permission denied: %s", enforce.Describe(action, resource, enforce.EffectDeny)),
				IsError: true,
			}, nil
		case enforce.EffectAsk:
			confirmer := l.Confirmer
			if confirmer == nil {
				confirmer = DenyConfirmer{}
			}
			approved, err := confirmer.Confirm(ctx, call, action, resource)
			if err != nil {
				return tool.Result{}, err
			}
			if !approved {
				return tool.Result{Content: "the user declined this action", IsError: true}, nil
			}
		}
	}

	l.observer().ToolStarted(call)
	result, err := impl.Run(ctx, call)
	if err != nil {
		return tool.Result{}, err
	}
	l.observer().ToolFinished(call, result)
	return result, nil
}

// appendToolResult records a result as a tool message the model will see.
func (l *Loop) appendToolResult(call provider.ToolCall, result tool.Result) {
	l.messages = append(l.messages, provider.Message{
		Role:       provider.RoleTool,
		ToolCallID: call.ID,
		Name:       call.Name,
		Text:       result.Content,
	})
}

// describeCall maps a call onto the permission action and resource it needs.
func describeCall(call tool.Call) (action, resource string) {
	switch call.Name {
	case enforce.ToolBash:
		if argv, err := tool.Argv(call.Args); err == nil {
			return enforce.ActionShell, strings.Join(argv, " ")
		}
		return enforce.ActionShell, ""
	case enforce.ToolEdit, enforce.ToolWrite:
		return enforce.ActionEdit, jsonField(call.Args, "path", "file_path")
	case enforce.ToolTask:
		return enforce.ActionSubagent, jsonField(call.Args, "subagent")
	default:
		return "", ""
	}
}

// jsonField pulls the first present string field from a JSON object.
func jsonField(args string, keys ...string) string {
	var parsed map[string]any
	if err := unmarshalLoose(args, &parsed); err != nil {
		return ""
	}
	for _, key := range keys {
		if value, ok := parsed[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}
