# team_await returns on events, not on a three-minute timer

Status: plan, written 2026-10-27 from the code on `main` f3558be. Ticket TEN-529 under TEN-497 (multi-agent).
Not started.

## The problem

`team_await` is the orchestrator's one way to wait for its helpers. Today (`cmd/tenant/team.go`,
`report` → `wait`) it blocks until *every* running helper of the conversation has finished, or a
timer fires, whichever comes first. The timer is three minutes: the `orchestrate` CLI's
`--await-timeout` default, and the hard-coded fallback when the TUI and serve paths wire
`spawnTool{rt: rt}` with no timeout. On the timer it returns "(timed out waiting; reporting
current state)" with nothing new, and the model calls it again.

Every return is a planner request with the whole context, and consumes one step of the plan
ceiling. From this Mac's turns log:

| | |
|---|---|
| `team_await` calls, 2026-10-07 to 10-27 | 127 |
| returned at exactly 3m0s with helpers still running | 101 |
| returned early because the team finished | 14 |
| one turn on 2026-10-27, trace 639699b3 | 5 consecutive 3-minute timeouts, then a 27 s return: 15 minutes of waiting cost 5 planner calls |

A long QA helper on Claude Code is routinely 10 to 20 minutes. The orchestrator spends that time
re-asking the same question at full context price, and the plan ceiling counts each ask as work.

What already exists and is not used for waking: each `member` has a `done` channel that `wait`
selects on, but only in spawn order and only to learn that *all* are done; the bus has a
coalescing `Notify()` channel, and every helper posts `DONE — …` to the team on the bus before it
closes `done`, so the two signals always arrive together.

## The target

Like the desktop app's CI monitor for a pull request: the orchestrator sleeps until something
happened, then wakes with that something. `team_await` returns when a helper finishes (done or
error), or a helper writes to the orchestrator on the bus, with the news in the return. It does not
return on a clock unless a long safety ceiling passes. Waiting costs no tokens; only news does.

## Design

### 1. `wait` wakes on any event

`wait` selects over three things at once instead of walking members in order:

- a `done` channel of any running member of the conversation (one `select` over all of them via a
  fan-in goroutine per member, or a single `chan struct{}` the runtime closes-and-replaces on every
  finish; the latter is one field and no goroutines);
- the bus `Notify()` channel, acted on only when `Bus.Since(cursor)` shows a message addressed to
  the orchestrator (`To == orchID`) or broadcast to the team, from someone other than the
  orchestrator, since the await started. A helper's own `DONE` broadcast coincides with its `done`,
  so one finish is one wake, not two;
- the ceiling timer and the caller's context, as today.

It returns a reason: `finished` (which members), `message` (how many, from whom), `ceiling`,
`cancelled`.

### 2. `report` returns the news

Unchanged shape, new content: the finished members' results (dropped afterwards, as today, so each
result is returned once), then the bus messages that woke it (drained from the orchestrator's
inbox so `team_check` doesn't repeat them), then one line per helper still running with its role
and elapsed time, so the model knows whether to await again or do its own work. Nothing running
and nothing new keeps today's "no sub-agents to report". A ceiling return keeps today's "(timed
out waiting; reporting current state)" wording, since that is exactly what it is.

### 3. The ceiling

Three minutes was the only thing keeping a stuck helper from holding the orchestrator forever.
That job already belongs to the run's budgets (`budgets` in config.json: `max_wall_time`,
`max_tokens_per_run`, `max_tool_calls_per_run`), which cancel a helper that overruns, and to
`CancelStuck`. So the ceiling becomes **30 minutes** by default, or the run's remaining
`max_wall_time` when that is shorter. `--await-timeout`
on `tenant orchestrate` keeps its name and now means that ceiling; `tenant research`'s per-wave
`--await-timeout` (10 m) is unchanged, since a wave is a different thing. No new config key.

### 4. The orchestrator's instructions

The prompt and the tool description stop saying "call team_await ONCE, it blocks until the whole
team finishes". They say: it returns as soon as a helper finishes or messages you; use what came
back, then await again while helpers are still running; it never returns empty-handed except at
the ceiling. "Don't poll team_check in a loop" stays.

### 5. What the operator sees

During a long wait the activity rail already shows every helper's tool calls and bus lines, so a
blocked `team_await` is visible work, not a hang. The `Tool team_await finished in …` log line
gains the wake reason (`finished main-qa-3`, `2 messages`, `ceiling`), which is the measurement
for whether this worked.

## Not in scope

Streaming helper progress into the orchestrator's context (it would cost what this saves);
changing `team_check`, `team_send` or `team_history`; research waves; a per-helper timeout.

## Tests

- Two helpers, one finishes at 100 ms: `team_await` returns at 100 ms with that result and
  "1 still running", not at the ceiling; a second await returns the other.
- A helper posts to the orchestrator mid-task: the await returns with the message and the helper
  still running; the message does not reappear in `team_check`.
- A helper's `DONE` broadcast plus its `done` close produce one return, with the result once.
- Nothing happens: the await returns at a short test ceiling with the timed-out wording and the
  run still alive (today's `TestTeamAwait_TimeoutKeepsRunAndWorkersAlive` becomes the ceiling case).
- The existing suite (`TestTeamAwait_*`, `TestTeamRuntime_SpawnAndAwait`, the info-flow and
  conversation-scoping tests) keeps its assertions; only "all at once" expectations change to
  "each as it finishes".
- A measurement, not a test: after a week on main, `team_await` log lines should show `finished`
  and `message` reasons far outnumbering `ceiling`.

## Documentation (findtime-net, same PR)

- `models/teams.md`: how the orchestrator waits (returns on a helper finishing or messaging it, the
  30-minute ceiling), and `--await-timeout`'s new meaning.
- `reference/cli.md`: the `--await-timeout` row for `tenant orchestrate`.
- `models/tuning.md`, "Team budgets": the ceiling defers to the wall budget.
- `CHANGELOG.md` under Changed.

## Work breakdown (one PR)

1. `wait` with the fan-in and the bus cursor; `report` with the news and the still-running lines;
   the wake reason in the tool's log line.
2. The ceiling: 30 m default, wall-budget cap, `--await-timeout` as the ceiling, the three wiring
   sites.
3. Prompt and tool description.
4. Tests, docs, changelog, docfacts.

Size: M. The behaviour change is confined to `team.go`'s wait/report and two strings.
