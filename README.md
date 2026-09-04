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
      "models": { "qwen3.5:latest": { "name": "qwen3.5" } }
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
  and argue with prose. A second flips on `tool_choice` pinning. A third has
  the runtime perform the action itself.
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
relabelling. After `ctrl+g` to plain the next request carries every tool and no
`tool_choice`; after `ctrl+g` back, the tool list is masked to the current
stage again. Confirm it with `--log-requests`:

```
governed, planning:  read, ls, glob, grep, workflow_submit_plan, …      (7 tools)
plain:               read, write, edit, ls, glob, grep, bash, task, …  (15 tools)
```

If a project has no workflow policy the mode cannot be switched on, and the bar
says why — `○ plain (no policy)` — rather than offering a key that could only
fail. A switch is also refused mid-turn; cancel with `ctrl+c` first.

Starting mode comes from config and flags:

```sh
agentwarden                        # governed if the project enables it
agentwarden --no-workflow          # start plain; ctrl+g is still available
agentwarden run "quick question"   # one-shot, for scripting
```

`--auto` pre-approves tool calls that would otherwise prompt. It upgrades
"ask" to "allow" but **never overrides an explicit `deny` rule** — a rule you
wrote outranks a convenience flag. Toggle it in-session with `/auto`.

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
| `ctrl+g` | Toggle governed / plain |
| `ctrl+p` | Switch provider / model |
| `ctrl+w` | Toggle the gate pane |
| `ctrl+c` | Cancel the running turn (or quit when idle) |
| `ctrl+d` | Quit |

`/help` · `/model` · `/mode` · `/govern` · `/plain` · `/auto` · `/gates` · `/clear` · `/quit`

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
