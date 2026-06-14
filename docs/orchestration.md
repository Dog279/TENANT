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
   local/DGX model).
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
says so), the orchestrator delegates coding, implementation, and debugging work
to that member with `spawn_agent` **by default** — rather than writing the code
itself. This is data-driven off the rendered agent descriptions, so a user's
custom coding agent works the same way; there is no hardcoded agent name.

This is most valuable when the coding specialist is pinned to a different model
than the orchestrator (e.g. orchestrator on a fast cloud model, `Programmer`
pinned to a local Qwen/DGX endpoint): the orchestrator stays responsive while
implementation runs on the model best suited for it. When the specialist simply
inherits the orchestrator's model, delegation still buys the specialist's
persona and rules.

The steering only appears when named agents are configured; with no team it has
no effect.
