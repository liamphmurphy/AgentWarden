# AGENTS.md

Guidance for AI agents working on this repository. Read this before making
changes; it records the invariants that are easy to break and expensive to
break silently.

## What this project is

A Go terminal coding agent whose defining feature is a **workflow enforcer
inside the agent loop**. The enforcer's job is to make illegal actions
impossible rather than discouraged. If a change makes enforcement advisory,
best-effort, or dependent on the model choosing to cooperate, it has removed
the reason this project exists.

## Build and test

```sh
make check                  # gofmt, go vet, go test -race
go test ./... -race         # everything
go test ./... -short        # skips tests that shell out to a real `go test`
go build -o agentwarden ./cmd/agentwarden
```

`GOCACHE` is set to `./.gocache` by the Makefile. If you run `go` directly in a
restricted sandbox, set it yourself: `GOCACHE=$PWD/.gocache go test ./...`.

Some tests bind a local HTTP listener (`httptest`) or run `git`; those need
network/exec permission even though they touch nothing external.

## Architecture, and the one rule about it

```
internal/workflow    pure domain: states, transitions, policy, hashing
internal/enforce     policy meets the OS: masking, gates, fingerprints
internal/controller  task lifecycle; the ONLY producer of gate evidence
internal/provider    model interface + openaicompat + fake
internal/agent       the model/tool loop; agent markdown
internal/tool        read, write, edit, bash, glob, grep, ls, task
internal/skill       SKILL.md discovery
internal/session     task records + append-only audit log
internal/tui         Bubble Tea interface
```

**`internal/workflow` must stay pure.** No I/O, no `os`, no `exec`, no
`time.Now()` — take a `workflow.Clock`. The entire governance model is testable
without a repository or a model because of this, and that property is worth
more than the convenience of reaching for a syscall.

Dependencies point one way: `workflow` ← `enforce` ← `controller` ← `agent` ←
`tui`. The enforcer defines its own `enforce.Session` rather than importing
`session`, specifically to keep that edge from becoming a cycle. Don't
"simplify" it by importing the other way.

## Invariants you must not break

These are the load-bearing guarantees. Each has tests; if you find yourself
editing a test to make a change pass, stop and reconsider the change.

1. **Masking is subtraction from the request, not a post-hoc refusal.** A
   masked tool is absent from `request.tools`. `TestMaskingRemovesWriteToolsOutsideImplementation`
   and `TestEnforcementEndToEnd` assert absence, not denial. Never "fix"
   masking by allowing the tool and rejecting the call.

2. **Only the controller creates receipts and QA verdicts.** An agent's claim
   that a command passed is never evidence. If you add a path where the model
   can report a gate result, you have broken the feature.

3. **An implementation handoff must prove the tree moved.** The work tree is
   fingerprinted on entry to every editing stage, and
   `SubmitImplementation` refuses a submission whose fingerprint still equals
   that baseline. Gates cannot cover this: on an unchanged tree every suite
   passes, so a placeholder submission reaches review with a full set of green
   receipts — which is exactly what happened four times in one repository
   before the check existed. Never relax it to a warning, and never let a
   transition into an editing stage skip `recordBaseline`. See
   `TestSubmitImplementationRefusesAnUnchangedTree` and
   `TestRealGateBlocksThenPasses`.

4. **A stage the model cannot advance is never given a turn.** If nothing in
   the masked tool list could move the workflow on, the loop hands the stage
   to the runtime (`AdvanceStage`, which runs the gates for `verifying`) or
   ends the run with the reason. Verification therefore happens the moment
   that stage is entered, not after the run. Running it later left a model
   taking thirteen turns in a stage offering it four read-only tools before it
   gave up and reported a blocker that was not true. `AdvanceStage` must also
   report *actual* movement, or the loop spins to the step limit: the state is
   compared, not trusted. See
   `TestVerifyingStageRunsGatesInsteadOfAskingTheModel` and
   `TestAdvanceStageMustActuallyMoveTheState`.

5. **Gate commands are argv, executed without a shell.** `ExecRunner` must
   never grow a `sh -c`. The agent-facing `bash` tool is different and
   deliberately does use a shell.

6. **Evidence expires when its inputs move.** `VerificationProblem` is the
   single staleness predicate: policy hash, passing receipt, receipt's policy
   hash, work-tree fingerprint. Adding a caller that skips it reintroduces
   "the tests passed ten edits ago".

7. **Fingerprinting fails closed.** Outside a git work tree, a governed
   session refuses to start. Do not add a fallback that guesses.

8. **`.agentwarden/state` is excluded from the fingerprint.** This is a fixed bug,
   not an optimization: the store writes receipts *inside* the repository it
   fingerprints, so without the exclusion saving a receipt invalidates that
   receipt. See `TestFingerprintExcludesAgentwardenState`.

9. **Everything about identity follows the stage owner.** A single session
   performs each stage in turn, so the actor, the permission rules *and* the
   system prompt must all track `enforce.RoleForState`. Each was a separate
   fixed bug. When the session ran as `orchestrator` throughout, masking
   offered `workflow_submit_plan` while the actor check refused it, leaving
   the workflow unsatisfiable. When the rules stayed the planner's, the
   implementing stage was denied the edit it exists to make. When the prompt
   stayed the orchestrator's, the model was told "do not plan yourself" by a
   stage whose only handoff is `workflow_submit_plan`. See `syncActor`,
   `syncPermissions`, `promptAgent` and `TestGovernedPromptFollowsStageOwner`.

10. **Plain mode carries no workflow state.** Switching governance off must
   drop all of it together: the loop's task, the session role and its
   counters, the state-change hooks, the stage owner's prompt and the stage
   owner's permissions — and `Nop` withholds the `workflow_*` tools. Those
   tools are not inert; they advance stored state through the controller, so an
   ungoverned session holding them could complete a task with no gate checked.
   A leftover of any kind produces a plain session that still narrates a
   planning stage and refuses the edit it was just asked for. Because the
   switch happens mid-conversation, `Loop.SetSystemPrompt` rewrites the stored
   system message rather than only the field — the prompt is copied into the
   message list before the first turn and never re-read. See
   `TestPlainModeDropsWorkflowState`, `TestModeSwitchRewritesLivePrompt` and
   `TestNopPermitsEverythingButTheStateMachine`.

11. **The panel never invents a number it was not given.** Token counts show
   `not reported` until an endpoint actually returns usage, and context is a
   percentage only when `contextWindow` is configured — an OpenAI-compatible
   endpoint reports tokens *used*, never tokens *accepted*. Context pressure is
   the newest prompt, not the sum of prompts: every turn resends the
   conversation. See `TestStatusPaneUnreportedUsage` and
   `TestStatusPaneWithoutContextWindow`.

12. **Deny by default.** Unknown `(state, action)` pairs are rejected; unknown
   states get read-only tools. Adding a state must never widen access by
   accident.

13. **`--auto` never overrides an explicit `deny`.** It upgrades `ask` to
   `allow` and nothing else.

14. **A live switch must change the thing, never just the label.** This
    applies to both switchers. `tui.Model.Governed` and `tui.Model.ModelName`
    only decide what the status bar says; enforcement lives in
    `agent.Loop.Governor` and the endpoint in `agent.Loop.Provider`. Changes go
    through `ModeSwitcher` / `ModelSwitcher`, and the UI updates its label only
    after the switcher returns without error. See
    `TestSwitchDelegatesRatherThanRelabelling` and
    `TestFailedModelSwitchDoesNotRelabel`.

15. **An open picker owns the keyboard.** `handleKey` checks
    `picker.IsOpen()` before the normal bindings, so `enter` selects a row
    rather than submitting the half-typed prompt behind the overlay, and the
    arrow keys drive the picker rather than the prompt history.
    See `TestPickerOwnsEnterWhileOpen` and `TestPickerKeepsArrowKeysWhileOpen`.

    Key precedence in `handleKey` is: permission confirmation, picker, history,
    then the textarea. History only engages at the edge line (`input.Line() ==
    0` for up, the last line for down) so a multi-line draft still navigates
    normally.

16. **Terminal capability detection happens before the program starts.**
    Querying the background colour writes an escape sequence and reads the
    reply; once Bubble Tea owns stdin, that reply is read as keystrokes and
    appears as junk in the prompt. `tui.DetectTheme` is the only place allowed
    to do it, enforced by an AST check in `TestNoTerminalQueryingAPIsInModel`.

17. **Filesystem tools stay confined to the project root.** `tool.resolve`
    handles the awkward cases (a file that does not exist yet, a symlinked
    root such as macOS `/tmp` → `/private/tmp`). Don't simplify it back into a
    prefix comparison.

18. **An `ask` decision in the TUI must actually ask.** The model loop waits on
    the confirmation response or its run context; it must never silently turn
    `ask` into deny, leak the waiting goroutine on cancellation, or let modal
    keystrokes reach the prompt behind it. Session approval is scoped to the
    exact `(action, resource)` pair. See `TestConfirmationWaitsForExplicitApproval`,
    `TestConfirmationOwnsKeyboardAndCanDeny` and
    `TestConfirmationCancellationReleasesWaitingCall`.

## Conventions

- **Comments explain why, not what.** The existing code comments the
  non-obvious decision and the reason a subtle thing is that way. Match that
  density; don't narrate the mechanics.
- **Table-driven tests**, with the case name saying what property is being
  checked. Test names in this repo are statements (`TestCompletionRefusedUntilGatePasses`).
- **Errors wrap sentinels** from `internal/workflow/errors.go` so callers can
  use `errors.Is`.
- **A tool failure is a `tool.Result{IsError: true}`, not a Go error.** The
  model needs to see it; a Go error aborts the run. Reserve Go errors for
  runtime failures.
- **No new dependencies without a reason.** Current set: `bubbletea`,
  `bubbles`, `lipgloss`, `glamour`, `yaml.v3`.

## Writing tests for enforcement

Use the scripted `provider/fake`, not a live model:

```go
model := fake.New(
    fake.CallTurn("c1", enforce.ToolEdit, `{"path":"main.go", ...}`), // illegal
    fake.CallTurn("c2", enforce.ToolSubmitPlan, `{"plan":"..."}`),    // complies
    fake.TextTurn("done"),
)
// then assert on model.Requests(): fake.OffersTool(req, ...), req.ToolChoice
```

`fake.Provider` records every request, which is how you assert masking and
`tool_choice` actually reached the payload. Handoff tools in tests must
advance the state machine, as the real ones do — see `withHandoffs` in
`internal/agent/loop_test.go`. A stub that doesn't transition will make the
enforcer correctly demand a handoff forever, which looks like a loop bug and
isn't one.

## When working on small-model behaviour

The point of this system is models that don't follow instructions well. When
adding a mechanism, prefer in this order:

1. Make the bad action impossible (masking, `tool_choice`, schema constraints).
2. Feed back a **filled-in argument skeleton**, not prose. See
   `enforce.ArgumentSkeleton`. Small models copy templates; they argue with
   explanations.
3. Re-inject state near the newest message, not in the system prompt — early
   context gets lost within a few turns.

Verify on the wire, not by reading code: `agentwarden run --log-requests req.jsonl`
then inspect the payloads. A gateway can silently drop `tool_choice`, and the
only way to know is to look.

## Things deliberately not done

Don't add these without discussion; each was considered.

- **Multi-session role delegation with distinct identities.** The controller
  is designed for it, but it isn't wired up. Until it is, independent QA is
  sequence enforcement, not identity separation — say so honestly rather than
  implying otherwise.
- **A second provider adapter.** The `Provider` interface exists so one can be
  added; nothing needs it yet.
- **Treating this as a security boundary.** It isn't, the README says so, and
  claims to the contrary should not creep in.
