# agentwarden

A terminal coding agent for OpenAI-compatible endpoints, with a **workflow
enforcer built into the agent loop** rather than bolted on beside it.

Point it at Ollama, vLLM, llama.cpp, LM Studio, or a gateway. Configure it with
JSON. Write agents and skills as markdown. The distinguishing feature is that
you can declare a delivery workflow — who does what, in what order, and which
commands must pass — and the runtime makes it structurally impossible for the
model to skip a step.

```
┌ Gates ───────────────┐
│ ✓ unit        2.4s   │
│ ◐ integration 0:47   │
│   │ ok  pkg/api      │
│ · lint       pending │
└──────────────────────┘
```

<img width="1805" height="1075" alt="image" src="https://github.com/user-attachments/assets/588005c9-93a1-46a5-ba37-f495232ce2a5" />


## Why the enforcer lives in the loop

The usual way to enforce an agent workflow is to inspect what the model did and
complain afterwards. That fails on small models, which do not reliably follow
instructions, and it fails on large ones whenever the instruction is
inconvenient.

This runtime instead **removes the illegal option before the model sees it**.
Tool masking is an input to the request, not a correction after it:

```jsonc
// during the planning stage, this is the entire tool list on the wire
"tools": ["read", "ls", "glob", "grep", "workflow_submit_plan", ...]
// edit, write and bash are absent — there is no instruction to disobey
```

Three levers, strongest first:

| Lever | What it does |
|---|---|
| **Per-state tool masking** | The tool array is computed from `(state, role)`. A masked tool is absent from the payload entirely. |
| **Forced `tool_choice`** | When the only legal next move is a handoff, the request pins `tool_choice` to that function, so the next output *is* that call. |
| **Runtime-executed gates** | Agentwarden runs your gate commands itself and reads the exit codes. The model is never asked whether the tests passed. |

Backing them up: a corrective `tool_result` containing a filled-in argument
skeleton, an escalation ladder (`warn → force → auto`), a re-injected state
banner each turn, and git-worktree fingerprinting so evidence expires the
moment a file changes.

## Install

Requires Go 1.25 or newer. Nothing else — no cgo, no system libraries.

### Put it on your PATH

```sh
git clone <this repo> agentwarden && cd agentwarden
make install
```

That builds and installs to `go env GOPATH`/bin (or `GOBIN` if you have set
it), which is on most Go developers' PATH already. The target tells you the
destination and warns if it is *not* on your PATH, since installing somewhere
the shell cannot see it is the usual reason a fresh install "works" but the
command is missing:

```
$ make install
installed /Users/you/go/bin/agentwarden
/Users/you/go/bin is on PATH; run: agentwarden
```

Check where it would go, and whether it is visible, without installing:

```sh
make where
```
```
install dir:  /Users/you/go/bin
on PATH:      yes
binary there: /Users/you/go/bin/agentwarden
PATH resolves: /Users/you/go/bin/agentwarden
```

The last two lines are reported separately because they can disagree — PATH
may resolve an older copy from a different directory than the one `make`
would install to.

`go install ./cmd/agentwarden` does the same thing if you prefer the plain Go
toolchain.

### Installing somewhere else

```sh
make install PREFIX=/usr/local        # -> /usr/local/bin/agentwarden (may need sudo)
make install PREFIX=~/.local          # -> ~/.local/bin/agentwarden
```

If the chosen directory is not on your PATH, add it and restart the shell:

```sh
echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.zshrc && exec zsh
```

### Build without installing

```sh
make build      # -> ./agentwarden in the repo
./agentwarden
```

### Other targets

| Target | What it does |
|---|---|
| `make build` | Build `./agentwarden` in the repo |
| `make install` | Build and install onto PATH |
| `make uninstall` | Remove the installed binary |
| `make where` | Show the install dir and whether it is on PATH |
| `make check` | `gofmt`, `go vet`, `go test -race` |
| `make clean` | Remove the binary and the local build cache |

`make help` lists them too.

### After installing

`agentwarden` reads `~/.config/agentwarden/agentwarden.json` wherever you run it, so it
works from any directory:

```sh
cd ~/some/other/project
agentwarden --config      # confirm the resolved configuration
agentwarden               # start a session
```

A governed session additionally requires a git repository, because
fingerprinting the work tree is what makes gate evidence trustworthy — outside
git it fails closed rather than pretending. In a directory with no workflow
policy the session simply runs plain, and the status bar says so.

## Quick start

Nothing is governed by default. Start with a plain session:

```sh
agentwarden                                        # interactive TUI
agentwarden run "what does internal/enforce do?"   # one-shot, prints and exits
agentwarden --config                               # show the resolved configuration
```

`~/.config/agentwarden/agentwarden.json`, overlaid by `./.agentwarden/agentwarden.json`:

```jsonc
{
  "providers": {
    "ollama": {
      "baseUrl": "http://127.0.0.1:11434/v1",
      // contextWindow is optional; set it to see context pressure as a
      // percentage in the session panel.
      "models": { "qwen3.5:latest": { "name": "qwen3.5", "contextWindow": 65536 } }
    },
    "gateway": {
      "baseUrl": "https://your-gateway.example.com/v1",
      // Secrets are interpolated from the environment, never stored here.
      "headers": { "x-api-key": "${GATEWAY_API_KEY}" },
      "models": { "sonnet": {}, "nemotron": {} }
    }
  },
  "defaultModel": "ollama/qwen3.5:latest",
  "workflow": { "enabled": false }
}
```

Comments and trailing commas are fine — the loader is JSONC-tolerant.
`${VAR}` and `${VAR:-default}` are interpolated, and an unset variable with no
default is a startup error rather than a puzzling 401 later. Point `envFile` at
a `.env` to supply values without exporting them.

## Turning on governance

Enable it and write a policy:

```jsonc
{ "workflow": { "enabled": true, "policy": ".agentwarden/workflow.yml" } }
```

`.agentwarden/workflow.yml`:

```yaml
version: 1
roles:
  orchestrator: orchestrator
  planner: tech-lead
  implementer: engineer
  reviewer: qa-engineer

gates:
  - id: unit
    command: ["go", "test", "./..."]
    required: true          # must pass before the task can be called done
    timeout_seconds: 900
  - id: lint
    command: ["golangci-lint", "run"]
    required: false         # runs and reports, but does not block
```

Commands are **argv arrays executed without a shell**, so `&&`, pipes and
redirects are inert. If a gate genuinely needs shell syntax, say so:
`["sh", "-lc", "..."]`.

Omit `states:` and you get the built-in graph:

```
planning → implementing → verifying → qa_review → ready_to_complete → complete
              ↑              │            │
              └──────────────┴────────────┴→ changes_requested

any active state → blocked → back to where it was parked
any non-terminal state → cancelled
```

Every `(state, action)` pair not in the table is denied, so adding a state can
never widen what is reachable by accident.

### What a handoff has to prove

`workflow_submit_implementation` is refused unless the work tree actually
moved. The tree is fingerprinted when the task enters an editing stage
(`implementing`, `changes_requested`, and again on every return to one), and a
submission whose fingerprint still equals that baseline cannot be an
implementation — whatever it says about itself.

This is not belt-and-braces; it is the only thing standing between the
enforcer and a fabricated handoff, because **gates cannot detect work that was
never done**. On an unchanged tree `go test`, `go vet` and the integration
suite all pass, so a placeholder submission sails through verification and
arrives at review with a full set of green receipts. Four tasks in one
repository reached `qa_review` exactly that way, one of them with a handoff
reading *"No changes yet — this is a placeholder submission to unblock the
state machine"*, and all three of its gates green. Passing gates prove the
tree is healthy; only the fingerprint proves the tree changed.

`files_changed` is required for the same reason, and is the cheap version of
the check — it catches the honest case before any git call.

### Who owns each stage

`verifying` belongs to the runtime, not the model. There is no tool it could
call to advance from there, so it is never given a turn: the gates run the
moment the stage is entered, from inside the agent loop, and the workflow moves
on to review or back to implementation without the model being consulted.

That matters more than it sounds. When verification ran only *after* a run had
finished, a model that reached `verifying` kept being sent requests offering it
four read-only tools and no way forward — thirteen turns of it in one observed
session — until it hit the step limit and reported a blocker that was not
actually true. The rule now is general: **if nothing in the visible tool list
could advance the workflow, the model is not asked.** The run ends with the
reason instead, so a stage a policy has made unsatisfiable is reported rather
than silently consuming the whole step budget.

### Custom states

Declaring `states:` replaces the built-in graph and unlocks per-stage
enforcement settings — including adding a role, which is a config change here
rather than a code change:

```yaml
states:
  research:
    agent: researcher
    allow_tools: [read, grep, glob, task]
    delegate_to: [researcher]
    max_direct_tool_calls: 3        # then the handoff is pinned
    on_violation: [warn, force, auto]
    on: { plan_submitted: planning }
  planning:
    on: { plan_submitted: implementing }
```

### Tuning for small models

A 7–30B model does not reliably follow instructions, and the usual response —
writing firmer instructions — does not help. These settings exist because
*removing an option works where instructing against it does not*.

```yaml
small_model_mode: true
workflow:
  auto_advance: true
routes:
  - when: { path_glob: "**/*.tf" }
    agent: infra
```

**`small_model_mode: true`** turns on three behaviours that are wasted on a
frontier model and load-bearing on a small one:

- *Tool masking* — the tool array is rebuilt from `(state, role)` every turn, so
  during `planning` the model receives a payload containing `read`, `grep`,
  `glob` and `workflow_submit_plan` and nothing else. It cannot call `edit`
  because `edit` is not in the request. No prompt asks it not to.
- *`tool_choice` pinning* — when the only legal move left is a handoff, the
  request sets `tool_choice` to that specific function, so the next thing the
  model emits *is* that call. This is the single highest-leverage lever, and
  the one thing a plugin sitting outside the agent loop cannot do.
- *Banner re-injection* — a compact block naming the state, the available
  tools, the pending gates and the required next action is appended next to the
  newest message, not buried in the system prompt. Small models lose early
  context within a handful of turns; the banner restores it for a few dozen
  tokens.

**`auto_advance: true`** lets the runtime drive stage handoffs itself: launch
the planner, then the implementer, run the gates, then the reviewer, without a
human saying "continue". Off by default because a stage boundary is a natural
place to look at what happened. Turn it on for unattended work, and when the
model is too weak to be trusted to ask for the next stage.

**`routes`** skips the model's judgement altogether. A rule matching a path
glob picks the subagent before the model is consulted, which is strictly more
reliable than hoping a 9B model routes correctly. Use it wherever the right
answer is already known.

#### What this looks like in practice

A real run against `qwen3.5:latest` (9.7B), taken from `--log-requests`:

| Requests | Stage | Tools actually on the wire |
|---|---|---|
| 0–1 | `planning` | `read, ls, glob, grep, workflow_submit_plan, …` |
| 2–3 | `implementing` | `read, write, edit, ls, glob, grep, bash, …` |
| 4+ | `verifying` | `read, ls, glob, grep, …` |

Write access appears for exactly one stage and is withdrawn again while the
gates run. The model narrated the effect itself, unprompted: *"bash isn't
listed as an available tool."* That is the whole design in one sentence —
there was no instruction for it to disobey.

#### Two adjacent settings worth knowing

Neither is strictly about model size, but both matter most with a weak model:

- **The escalation ladder** (`on_violation: [warn, force, auto]`, per state).
  A first violation returns a corrective tool result containing a *filled-in
  argument skeleton* rather than an explanation — small models copy templates
  and argue with prose. A second flips on `tool_choice` pinning. A third stops
  with a focused diagnostic when the handoff still did not happen; the runtime
  will not fabricate a plan, implementation summary or QA verdict merely to
  move the state machine.
- **The direct-work budget** (`max_direct_tool_calls`, per state). After N
  non-handoff calls in a stage, the handoff is pinned. This is the guard for a
  model that reads the same three files in a loop instead of committing to a
  plan.

#### Diagnosing a model that will not cooperate

Read the payloads, not the code: a gateway can silently drop `tool_choice`, and
the only way to know is to look.

```sh
agentwarden run --log-requests req.jsonl "fix the failing test and finish"
jq -r '.request | "\(.tool_choice.function.name // "-")  \([.tools[]?.function.name] | join(","))"' req.jsonl
```

If the same call is rejected repeatedly the run aborts early and reports the
underlying error, rather than burning every step and reporting a step limit —
a real 9B failure mode, and the error you want to see is the one the model kept
hitting, not "gave up after 40 steps".

## Agents and skills

Agent markdown, discovered from `agentDirs`. The frontmatter is compatible with
existing OpenCode agent files, so they port over as-is:

```markdown
---
description: Coordinates governed development work without editing code
mode: primary            # primary | subagent | all
model: ollama/qwen3.5:latest
skills: [go-style, patterns]
permissions:
  - { action: edit,  resource: "*",         effect: deny }
  - { action: shell, resource: "*",         effect: deny }
  - { action: shell, resource: "git diff*", effect: allow }
---

You coordinate work. You do not edit files yourself.
```

Rules are **ordered and later rules win**, so the pattern above — deny broadly,
then re-allow specific inspection commands — works as written. The older
`tools: {edit: false, ...}` boolean map is also honored.

Skills are `SKILL.md` files with `name` and `description` frontmatter,
discovered from `skillDirs` as either `<dir>/SKILL.md` or loose `*.md`. A skill
an agent references but which does not exist is reported on stderr rather than
silently dropped.

## The session panel

The right-hand column reports the three things a session accumulates silently:
where the workflow is, how full the context is, and what it has spent.

```
╭ …transcript……………………………………………………………╮ ╭────────────────────────╮
│                                    │ │ Stage                  │
│  Let me read the failing test      │ │ ✓ planning             │
│  first.                            │ │ ▸ implementing         │
│                                    │ │   ↳ engineer           │
│  ✓ read                            │ │ · verifying            │
│                                    │ │ · qa_review            │
│                                    │ │ · ready_to_complete    │
│                                    │ │ · complete             │
│                                    │ │                        │
│                                    │ │ Context            38% │
│                                    │ │ █████░░░░░░░           │
│                                    │ │ 12.4k of 32k           │
│                                    │ │                        │
│                                    │ │ Tokens                 │
│                                    │ │ sent             45.1k │
│                                    │ │ recv              2.3k │
│                                    │ │ turns                7 │
╰────────────────────────────────────╯ ╰────────────────────────╯
```

`ctrl+t` (or `/stats`) hides it and gives the column back to the transcript,
which is what you want when reading a wide diff. It drops itself automatically
below 72 columns.

**Stage** is the pipeline walked from the policy's own transition graph, so a
project with custom `states:` sees its own stages rather than the builtin ones.
Passed stages are ticked, the current one is pointed at and names the agent
that owns it. Recovery stages are deliberately off the spine — `changes_requested`
is reachable only by rejection, so drawing it inline would claim every task
passes through it; when you are in one it is named underneath instead. The
whole section disappears in plain mode, because then there is no stage to be in.

**Context** is the newest turn's prompt against the model's window. It is the
newest prompt rather than a running total on purpose: every turn resends the
conversation, so summing prompts would report a window many times overflowed.
The meter turns amber at 70% and red at 90%.

**Tokens** are cumulative for the session: `sent` and `recv` answer "what has
this cost", `turns` is how many model round-trips it took.

Two honest gaps worth knowing about:

- **`contextWindow` has to be configured.** An OpenAI-compatible endpoint
  reports how many tokens a request *used*, never how many it would *accept*,
  so the window cannot be discovered. Without it the panel shows the prompt
  size and says `set contextWindow` rather than inventing a denominator.
- **For Ollama, use the served context, not the model's maximum.** `ollama
  show` may report a 262144-token architecture limit while the server truncates
  at its own default (`OLLAMA_CONTEXT_LENGTH`, or `num_ctx` in a Modelfile).
  Putting the architecture number in the config would draw a meter reading 5%
  as the request was being silently cut.

  Agent prompts and tool schemas can consume 4K before the first model turn,
  so start Ollama with enough served context for agentic work and put the same
  value in AgentWarden's `contextWindow` display setting:

  ```sh
  OLLAMA_CONTEXT_LENGTH=65536 ollama serve
  ollama ps # verify the loaded model's CONTEXT column
  ```

If an endpoint reports no usage at all, both counters say `not reported`
instead of showing zeros — an unmeasured session and a free one are not the
same thing. Usage is requested explicitly with `stream_options.include_usage`,
since a streamed response omits it otherwise; an endpoint that rejects the
field can drop it with `"stream_options": null` under that provider's `extra`.

## Switching between governed and plain

Governance is worth it for delivery work and in the way for a quick question,
so switching is a single keystroke:

| | |
|---|---|
| **`ctrl+g`** | Toggle governed ⇄ plain, mid-session |
| `/mode` | The same toggle, as a command |
| `/govern` | Enforce the workflow: stages and gates |
| `/plain` | Drop governance |

The status bar always says which mode you are in and which key leaves it, so
the shortcut does not have to be remembered:

```
ollama/qwen3.5:latest · ⛨ governed:planning        ctrl+g plain · ctrl+w gates · ctrl+d quit
ollama/qwen3.5:latest · ○ plain                                  ctrl+g govern · ctrl+d quit
```

The hint names the *action the key performs*, not the state you are in, so it
reads as an offer rather than a label. The gate-pane hint only appears when
there are gates to show.

This is a real swap of the enforcer inside the running agent loop, not a
relabelling, and **nothing about the workflow follows the session into plain
mode**. Four things change together, because leaving any one of them behind
produces a session that still behaves as though a stage were under way:

| | Governed | Plain |
|---|---|---|
| Tool list | masked to the stage | everything except `workflow_*` |
| System prompt | the stage owner's agent | a plain assistant, stated as such |
| Permissions | the stage owner's rules | edit and shell allowed |
| Task and stage | tracked and reported | none; the panel drops the section |

The `workflow_*` tools are withheld in plain mode for a reason beyond tidiness:
they are not inert. They advance stored task state through the controller, so an
ungoverned session holding them could submit a plan or complete a task with no
gate checked — governance would be off while its state advanced. Their presence
also misleads: a model handed `workflow_start` and `workflow_status` infers it
is inside a workflow and narrates one.

The system prompt matters just as much. A workflow role agent's instructions
("call `workflow_start`, do not implement yourself") describe a workflow that
is not running, and a model reads that as a governed session in progress. Plain
mode therefore drops the role prompt and keeps the agent's *skills*, which are
reference material rather than role instructions. An agent named explicitly
with `-agent` is kept in both modes — that is your instruction, not the
workflow's.

Permissions follow the same logic. A stage owner's rules exist to divide the
workflow's duties: the orchestrator may not edit because the engineer does that.
With no workflow those divisions describe nothing, so plain mode allows edit and
shell rather than enforcing a separation of duties that no longer exists. An
agent you named with `-agent` keeps its own rules, deny rules included.

Confirm all of it with `--log-requests`:

```
governed, planning:  read, ls, glob, grep, workflow_submit_plan, …     (7 tools)
governed, implementing:  + write, edit, bash, workflow_submit_implementation
plain:               read, write, edit, ls, glob, grep, bash, task      (8 tools)
```

Because the switch happens mid-conversation, the earlier turns still describe
whichever mode you left. The system message is rewritten in place — assigning
the field alone would not do it, since the prompt is copied into the message
list before the first turn — and a short note is appended saying the rules
changed, because that is where a small model is actually looking.

If a project has no workflow policy the mode cannot be switched on, and the bar
says why — `○ plain (no policy)` — rather than offering a key that could only
fail. A switch is also refused mid-turn; cancel with `ctrl+c` first.

Starting mode comes from config and flags:

```sh
agentwarden                        # governed if the project enables it
agentwarden --no-workflow          # start plain; ctrl+g is still available
agentwarden run "quick question"   # one-shot, for scripting
```

### Managing tasks from outside a session

A governed task outlives the session that created it, and two of its states —
`blocked` and any stage a policy has made unsatisfiable — have no tool the
model could call to leave them. Those are the operator's to resolve:

```sh
agentwarden -tasks               # every task in this project, newest first
agentwarden -resume <task-id>    # reopen a blocked task
agentwarden -cancel <task-id>    # take a task out of play
agentwarden resume               # list previous governed sessions
agentwarden resume <task-id>     # continue one at its saved workflow stage
```

With no ID, the command opens a small session picker. Use the up/down arrows
to highlight a task, press Enter to continue it, or Esc to leave without
starting a session.

The `resume` command uses the same `.agentwarden/state/` task records as the
operator flags. It restores the governed workflow checkpoint — objective,
plan, handoff, receipts, current stage, and saved conversation. Sessions
created before conversation persistence was added have no transcript to
restore.

```
e317147f7150  qa_review          2026-09-04 12:39  interactive session
74ceaf8a4a92  planning           2026-09-04 12:16  interactive session
```

Both actions run as the orchestrator, because the person at the terminal owns
the workflow and there is no more authoritative actor than the operator. The
state machine still applies: cancelling an already-cancelled task is refused
rather than quietly accepted.

Cancelling matters for a task holding evidence you do not trust. A task parked
in `qa_review` is one approval away from `complete`, and nothing expires it.

`--auto` pre-approves tool calls that would otherwise prompt. It upgrades
"ask" to "allow" but **never overrides an explicit `deny` rule** — a rule you
wrote outranks a convenience flag. Toggle it in-session with `/auto`.

Without `--auto`, an `ask` decision pauses the tool call and opens a permission
pane showing the tool, action and target. `enter` or `y` allows it once, `a`
allows that exact action and target for the rest of the session, and `esc` or
`n` denies it. `ctrl+c` denies the pending action and cancels the run. The
confirmation pane owns the keyboard while open, so a key cannot also submit or
edit the prompt behind it.

## Switching provider and model

`ctrl+p` opens a picker listing every provider/model pair in your config:

```
┌ Switch model   ↑↓ select · enter confirm · esc cancel ─────────────────────┐
│   ollama/gemma4:latest        Ollama — Gemma 4 (8B)                        │
│ › ollama/qwen3.5:latest       Ollama — Qwen 3.5 (9.7B)                     │
│   ollama/qwen3.8:27b-mlx      Ollama — Qwen 3.8 (27B MLX)                  │
│   gateway/sonnet              Gateway — Claude Sonnet                      │
└────────────────────────────────────────────────────────────────────────────┘
```

The cursor starts on the model you are already using, so confirming by accident
changes nothing. `esc` cancels.

| | |
|---|---|
| **`ctrl+p`** | Open the model picker |
| `/model` | The same picker, as a command |
| `/model gateway/sonnet` | Switch directly, skipping the picker |
| `-model gateway/sonnet` | Choose the model at startup |

This rebuilds the provider client and swaps it onto the running agent loop, so
the next request genuinely goes to the new endpoint. From a request log across
one switch:

```
request 0: provider=ollama         model=qwen3.5:latest
request 1: provider=rp-switchyard  model=sonnet
```

**The conversation is kept.** Switching mid-task is most useful for escalating
a problem the current model is struggling with — drop from a gateway to a local
model for cheap iteration, or climb the other way when a 9B model stalls.

The picker is only offered when there is more than one model configured; the
`ctrl+p` hint disappears otherwise rather than opening a one-item list. A
switch is refused mid-turn, as with mode; cancel with `ctrl+c` first.

Models must be declared in config to be selectable — except on a provider that
declares none at all, which accepts any name, since a local endpoint often
serves whatever has been pulled.

## Keys and commands

| Key | Action |
|---|---|
| `↑` / `↓` | Walk back and forward through prompt history |
| `ctrl+g` | Toggle governed / plain |
| `ctrl+p` | Switch provider / model |
| `ctrl+w` | Toggle the gate pane |
| `ctrl+t` | Toggle the session panel (stage, context, tokens) |
| `ctrl+c` | Cancel the running turn (or quit when idle) |
| `ctrl+d` | Quit |

### Prompt history

`↑` recalls the previous prompt, most recent first; `↓` walks back towards the
present. A recalled prompt lands with the cursor at the end, so it can be
edited and resubmitted.

Two details that make it behave the way a shell does:

- **Your draft is not lost.** Whatever is typed when you first press `↑` is
  stashed, and arrowing forward past the newest entry restores it. Glancing
  back through history never discards a half-written prompt.
- **The arrow keys still move the cursor.** History only engages when the
  cursor is on the first line (for `↑`) or the last (for `↓`), so a multi-line
  draft navigates normally. While the model picker is open, the arrows drive
  the picker instead.

Blank prompts are skipped and consecutive duplicates collapse, so re-running
the same command twice does not mean pressing `↑` twice to get past it. Slash
commands are recorded too — a mistyped `/modle` is exactly what you want back.
History is per session and holds the last 200 prompts.

`/help` · `/model` · `/mode` · `/govern` · `/plain` · `/auto` · `/gates` · `/stats` · `/copy` · `/mouse` · `/clear` · `/quit`

## Verifying it actually works

Masking and `tool_choice` only matter if they reach the wire, and a gateway can
silently drop either. So check:

```sh
agentwarden run --log-requests req.jsonl "fix the failing test and finish"
jq -r '.request | "\(.tool_choice // "-")  \([.tools[]?.function.name] | join(","))"' req.jsonl
```

During the planning stage you should see no `edit`, no `write`, no `bash`, and
`tool_choice` pinned to `workflow_submit_plan` after a violation.

```sh
go test ./... -race     # the full suite
go test ./... -short    # skips the tests that shell out to go test
make check              # fmt, vet and test
```

## How it is put together

```
cmd/agentwarden          CLI, flag parsing, wiring
internal/
  workflow           states, transitions, policy, hashing — pure, no I/O
  enforce            masking, gates, receipts, fingerprints, permissions
  controller         task lifecycle; the only producer of gate evidence
  provider           the model interface
    openaicompat     the one implementation
    fake             scripted provider for deterministic tests
  agent              the model/tool loop; agent markdown
  tool               read, write, edit, bash, glob, grep, ls, task
  skill              SKILL.md discovery
  session            task records and an append-only audit log
  tui                Bubble Tea interface, 30fps
```

`internal/workflow` has no dependencies, no clock and no I/O, so the whole
governance model is testable without a repository or a model. `internal/enforce`
is where policy meets the operating system.

State lives in `.agentwarden/state/` as JSON and JSONL — inspect it with `cat`.
It is excluded from the work-tree fingerprint, because otherwise writing a
receipt would change the tree that receipt was bound to.

## What this is not

It is an **enforcement mechanism, not a security boundary.** Anyone who can
edit the config, disable the tool, or run git directly can bypass it. Keep the
same commands as required CI checks and protect the policy file with normal
review. What this buys you is that the agent cannot quietly skip a step —
not that a determined human cannot.

Independent QA is likewise only as real as the identities behind it. A single
interactive session performs each stage in turn and adopts the stage's role as
it goes, so the *sequence* and the *gates* are enforced, but one session
reviewing its own work is not genuine independence.
