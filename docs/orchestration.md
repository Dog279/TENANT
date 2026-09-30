# Orchestration & the agent team

Tenant's orchestrator (the main loop, and the dedicated `tenant orchestrate`
command) can break a request into roles and run specialized sub-agents
**concurrently** over the team bus. `spawn_agent` is non-blocking —
`TeamRuntime.Spawn` starts the sub-agent in a goroutine and returns its id
immediately, so workers run in true parallel while the orchestrator keeps its
turn.

The orchestrator's behavior is steered by two pieces of system prompt:

- `orchestratorPrompt` (`cmd/tenant/team.go`) — used by `tenant orchestrate`.
- `renderAgentsForOrchestrator` (`cmd/tenant/agentprofile.go`) — appended in
  **both** the interactive TUI and `tenant orchestrate`, but **only when named
  agents are defined**. With no agents configured it returns an empty string,
  so operators who never define a team see the unchanged base prompt.

## The two delegation patterns

### 1. Fan-out → await → synthesize (the default)

For work that splits into independent parts whose results you then combine:

1. `spawn_agent(role, task)` for each independent part.
2. `team_await` **once** — it blocks until the whole team finishes and returns
   every result.
3. Synthesize one final answer from the collected results.

Guardrails (always apply):

- Spawn agents **only for INDEPENDENT, parallel work**. Don't spawn an agent
  whose job is to wait for or combine another agent's output — sub-agents run at
  the same time and can't reliably receive each other's results. **You** are the
  only synthesis layer.
- **Never poll `team_check` in a loop to wait.** It returns immediately; a poll
  loop just burns your step budget before the team finishes. Use `team_await`.

### 2. Delegate-and-keep-working (concurrent) — TEN-140

`spawn_agent` returns immediately, so you don't have to await right after
spawning. When you have your own independent work to do, overlap it with the
worker:

1. `spawn_agent` a long-running worker (e.g. a coder implementing a feature on a
   local/`<model-host>` model).
2. **Keep doing your own independent work** — research, draft, or spawn more
   independent workers — while the worker runs.
3. Call `team_await` **only when you actually need the worker's results**.
4. You **must** await before writing any final answer that depends on a worker's
   output (don't synthesize while a needed worker is still running).

> Example: spawn a coder to implement X, keep researching Y yourself, then
> `team_await` once and combine X + Y.

The same guardrails from pattern 1 apply: independent work only, and never poll
`team_check` in a loop.

## Delegating coding by default — TEN-139

When a team member's **description** marks them a coding/implementation
specialist (the built-in `Programmer`, or any custom agent whose description
says so), the orchestrator routes **substantial** coding to that member with
`spawn_agent` **by default** — rather than writing the code itself. This is
data-driven off the rendered agent descriptions, so a user's custom coding agent
works the same way; there is no hardcoded agent name.

The split is a deliberate **trivial/substantial threshold** (efficiency vs.
reliable delegation):

- **Substantial → delegate.** A new file or script, a new function, a multi-line
  change, debugging, or a refactor. This is where the specialist genuinely helps
  — disciplined diffs, root-cause fixes, a regression test, a clean build.
- **Trivial → handle directly.** A typo, a rename, a one-line tweak, where
  spawning a worker would only add round-trip latency for no real benefit.

Delegation is worthwhile **regardless of the specialist's model**: the
disciplined coding persona is the value even on the same model. It is *extra*
valuable when the specialist is pinned to a different model than the
orchestrator (e.g. orchestrator on a fast cloud model, `Programmer` pinned to a
local Qwen endpoint on `<model-host>` via `/agents model Programmer …`): the orchestrator
stays responsive while implementation runs on the model best suited for it. By
default a specialist with no pinned provider simply inherits the orchestrator's
model.

The steering only appears when named agents are configured; with no team it has
no effect.

> Note: this steering is *advisory* — the orchestrator still decides. A sharp
> threshold makes substantial delegation far more reliable, but it is not a hard
> runtime guarantee.

## Team budgets

Every team run (a turn that spawns sub-agents, `tenant orchestrate`, `/research`)
works inside a hard resource envelope. When it runs out, the run stops and the
feed and the log say which limit it hit. Set any of these under `budgets` in
`config.json`. A key you leave out keeps its default.

```json
"budgets": {
  "max_concurrent_subagents": 4,
  "max_agents_per_run": 16,
  "max_tool_calls_per_turn": 64,
  "max_tool_calls_per_run": 256,
  "max_tokens_per_run": 500000,
  "max_cost_usd": 25,
  "max_wall_time": "30m",
  "max_browser_instances": 4,
  "bus_history_cap": 1000,
  "input_cost_per_million_usd": 15,
  "output_cost_per_million_usd": 75
}
```

- `max_wall_time` is a duration: `"30m"`, `"2h"`. Older files that hold a number
  of nanoseconds still load.
- **Cost is an estimate.** `max_cost_usd` is checked against a figure built from
  the tokens the backend reports and the two per-million rates. Set the rates to
  your provider's prices; a negative rate means free (local models).
