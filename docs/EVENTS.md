# Event reference

<!-- Generated from the event catalog: go test ./cmd/tenant -run TestEventsDoc -update. Do not edit by hand. -->

Every catalogued event Tenant writes: what it means, why it happens and what to
do about it. The definitions live in the code, in `internal/logs/catalog` and in
each package that declares a provider. This file, the dashboard's Help tab and
`tenant logs catalog` are all rendered from them. To declare an event, see
[LOGGING.md](LOGGING.md#event-ids-the-reference-and-coverage).

IDs are unique within their provider and never reused: a retired ID stays
here, marked retired. They are assigned in blocks: 1–999 lifecycle, 1000s
operations, 2000s warnings, 3000s errors, 4000s security and audit, 5000s
configuration, 9000s diagnostics.

## research

Writes the `research` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#research-1) | info | Research run | `Research run {run}` |
| [2001](#research-2001) | warning | Record | `Research record not saved: could not {op}: {err}` |
| [2002](#research-2002) | warning | Wiki | `Research report not saved to the wiki: could not {op}: {err}` |

### research 1

`Research run {run}`

- **Level:** info · **Channel:** operational · **Task:** Research run · **Log:** research
- **Fields:** `run`, `question` (sensitive, masked: `question`)
- **Find it:** `provider:research id:1`

**What it means.** A research pass started, or ended: a stop or a fail, with its duration. Its sub-agents' activities name it as related.

**Why it happens.** The operator ran /research or tenant research, or replayed a run. It fails when the planner can't be reached or no sub-agent found anything usable.

**What to do.** None when it stops. When it fails, read the error; tenant research show <run> has each sub-agent's findings.

### research 2001

`Research record not saved: could not {op}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Record · **Log:** research
- **Fields:** `run`, `op`, `err`
- **Find it:** `provider:research id:2001`

**What it means.** The research ran, but its record in the research store is missing or incomplete, so tenant research list/show and replay won't have all of it.

**Why it happens.** The research directory under the data directory could not be written: the disk is full, or its permissions are wrong.

**What to do.** Check free space and the data directory's permissions.

### research 2002

`Research report not saved to the wiki: could not {op}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Wiki · **Log:** research
- **Fields:** `op`, `err`
- **Find it:** `provider:research id:2002`

**What it means.** The report was written, but not deposited in the wiki, or not indexed there, so wiki_search won't find it yet.

**Why it happens.** The wiki directory could not be written, or its index could not be rebuilt.

**What to do.** Check the wiki directory (--wiki or the wiki plugin's configuration); an unindexed report is picked up at the next wiki search.

## security

Writes the `security` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [4101](#security-4101) | info | Approval | `Approval {approval} requested: {action}` |
| [4103](#security-4103) | warning | Approval | `Approval {approval} denied: {action}` |

### security 4101

`Approval {approval} requested: {action}`

- **Level:** info · **Channel:** operational · **Task:** Approval · **Log:** security
- **Fields:** `approval`, `category`, `action`, `surface`
- **Find it:** `provider:security id:4101`

**What it means.** A gated action is waiting for the operator's decision.

**Why it happens.** The agent, a job or a plugin asked for an action whose category needs approval.

**What to do.** Approve or deny it where it was asked; it expires when nobody answers.

### security 4103

`Approval {approval} denied: {action}`

- **Level:** warning · **Channel:** operational · **Task:** Approval · **Log:** security
- **Fields:** `approval`, `action`, `surface`, `actor`
- **Keywords:** `audit.failure`
- **Find it:** `provider:security id:4103`

**What it means.** A gated action was denied, so it did not run.

**Why it happens.** The operator denied it, or Tenant denied it while shutting down.

**What to do.** None when intended; otherwise check the category's approval mode.

## subagents

Writes the `subagents` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#subagents-1) | info | Sub-agent | `Sub-agent {agent} ({role})` |
| [1101](#subagents-1101) | info | Team budget | `team budget` |
| [1201](#subagents-1201) | info | Bus | `bus message` |
| [2001](#subagents-2001) | warning | Spawn | `No router for agent profile {profile}; the sub-agent uses the shared router: {err}` |
| [2002](#subagents-2002) | warning | Spawn | `Spawn rule role {role} is not bound on the spawn router; using the planner` |
| [2003](#subagents-2003) | warning | Spawn | `Sub-agent {role} not spawned: {err}` |
| [2101](#subagents-2101) | warning | Team budget | `team run terminated: {reason}` |
| [3001](#subagents-3001) | error | Spawn | `Spawn classifier panicked; using the planner role: {panic}` |

### subagents 1

`Sub-agent {agent} ({role})`

- **Level:** info · **Channel:** operational · **Task:** Sub-agent · **Log:** subagents
- **Fields:** `agent`, `role`, `run`, `prompt` (sensitive, masked: `prompt`)
- **Find it:** `provider:subagents id:1`

**What it means.** A sub-agent started work on its task (prompt), or finished: a stop or a fail, with its duration. Its run field is the trace of its own turn.

**Why it happens.** The orchestrator spawned it (the spawn_agent tool), or /research did. It fails when its turn fails, or the team run's budget stops it.

**What to do.** None when it stops. When it fails, read the error, then its turn (filter trace:<run>).

### subagents 1101

`team budget`

- **Level:** info · **Channel:** operational · **Task:** Team budget · **Log:** subagents
- **Fields:** `agent`, `text`, `team_budget`
- **Find it:** `provider:subagents id:1101`

**What it means.** The team run's usage against its budget (text; team_budget holds it as JSON): sub-agents, tool calls, tokens, cost, browsers and wall time.

**Why it happens.** Written when the number of sub-agents or browsers changes, and every 10 seconds while a team run is active.

**What to do.** None. The limits are set in config.json (budgets).

### subagents 1201

`bus message`

- **Level:** info · **Channel:** operational · **Task:** Bus · **Log:** subagents
- **Fields:** `agent`, `text` (sensitive, masked: `text`)
- **Find it:** `provider:subagents id:1201`

**What it means.** A message between agents on the orchestra bus: agent is the sender; text is the recipient and the message.

**Why it happens.** A sub-agent or the orchestrator used a team tool (team_send, team_broadcast), or a sub-agent reported that it was done.

**What to do.** None.

### subagents 2001

`No router for agent profile {profile}; the sub-agent uses the shared router: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Spawn · **Log:** subagents
- **Fields:** `profile`, `err`
- **Find it:** `provider:subagents id:2001`

**What it means.** A sub-agent's profile pins a model, but its router could not be built, so the sub-agent runs on the orchestrator's model instead.

**Why it happens.** The profile names a provider or model that isn't configured, its key is missing, or config.json could not be read.

**What to do.** Check the profile (/agents) and its provider's configuration.

### subagents 2002

`Spawn rule role {role} is not bound on the spawn router; using the planner`

- **Level:** warning · **Channel:** operational · **Task:** Spawn · **Log:** subagents
- **Fields:** `agent`, `role`
- **Find it:** `provider:subagents id:2002`

**What it means.** A spawn routing rule picked a role for a sub-agent that its router has no model for, so it runs on the planner role.

**Why it happens.** The routing rule names a role that no configured model is bound to.

**What to do.** Bind a model to the role, or change the rule (route in config.json).

### subagents 2003

`Sub-agent {role} not spawned: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Spawn · **Log:** subagents
- **Fields:** `role`, `err`
- **Find it:** `provider:subagents id:2003`

**What it means.** The orchestrator asked for a sub-agent, and the team runtime refused. The orchestrator is told why.

**Why it happens.** Too many sub-agents are running at once, or the team run's budget (agents, tokens, cost or wall time) is spent.

**What to do.** None when the orchestrator waits for the running ones. Raise the limits in config.json (budgets) if they are too tight.

### subagents 2101

`team run terminated: {reason}`

- **Level:** warning · **Channel:** operational · **Task:** Team budget · **Log:** subagents
- **Fields:** `agent`, `reason`, `text`, `team_budget`
- **Find it:** `provider:subagents id:2101`

**What it means.** The team run hit a budget limit and was stopped: its running sub-agents were cancelled.

**Why it happens.** The run used up its tool calls, tokens, cost or wall time (reason).

**What to do.** Raise the limit in config.json (budgets) if the task needs it, or split the task.

### subagents 3001

`Spawn classifier panicked; using the planner role: {panic}`

- **Level:** error · **Channel:** operational · **Task:** Spawn · **Log:** subagents
- **Fields:** `panic`, `stack`
- **Find it:** `provider:subagents id:3001`

**What it means.** The spawn routing classifier's code panicked; the sub-agent was spawned on the planner role.

**Why it happens.** A bug in the spawn classifier or its rules.

**What to do.** Report it with the stack.

## system

Writes the `system` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#system-1) | info | Lifecycle | `Tenant {version} started as {role}` |
| [2](#system-2) | info | Lifecycle | `Tenant stopped cleanly after {uptime}` |
| [3](#system-3) | critical | Lifecycle | `The previous run ended unexpectedly; its last event was at {last}` |
| [3101](#system-3101) | error | Component | `{component} failed: {err}` |

### system 1

`Tenant {version} started as {role}`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** system
- **Fields:** `version`, `role`
- **Find it:** `provider:system id:1`

**What it means.** Tenant started.

**Why it happens.** The operator or a service manager started serve or the TUI.

**What to do.** None.

### system 2

`Tenant stopped cleanly after {uptime}`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** system
- **Fields:** `uptime`
- **Find it:** `provider:system id:2`

**What it means.** Tenant finished its shutdown.

**Why it happens.** It was asked to stop: Ctrl-C, the service manager, or an update.

**What to do.** None.

### system 3

`The previous run ended unexpectedly; its last event was at {last}`

- **Level:** critical · **Channel:** operational · **Task:** Lifecycle · **Log:** system
- **Fields:** `last`, `boot`
- **Find it:** `provider:system id:3`

**What it means.** The previous run of Tenant never stopped cleanly.

**Why it happens.** It crashed or was killed, or the machine lost power or restarted.

**What to do.** Read the events before its last one, and any crash file filed at this start.

### system 3101

`{component} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** Component · **Log:** system
- **Fields:** `component`, `err`
- **Find it:** `provider:system id:3101`

**What it means.** A subsystem stopped with an error.

**Why it happens.** Something it depends on failed: a connection, a file or its configuration.

**What to do.** Open the component's log; the event before this one usually names the cause.

## turns

Writes the `turns` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#turns-1) | info | Turn | `Turn` |
| [1001](#turns-1001) | info | Routing | `route observation` |
| [1002](#turns-1002) | info | Skills | `skills retrieved` |
| [1003](#turns-1003) | info | Tools | `tool surface ranked` |
| [1004](#turns-1004) | info | Turn | `operator interjected` |
| [1005](#turns-1005) | info | Turn | `final response` |
| [1101](#turns-1101) | info | Tool call | `Tool {tool} finished in {duration}` |
| [1201](#turns-1201) | info | Compaction | `working set compacted` |
| [2001](#turns-2001) | warning | Validation | `tool call rejected {tool}` |
| [2002](#turns-2002) | warning | Retry | `tool retry {tool}` |
| [2003](#turns-2003) | warning | Turn | `loop ceiling hit — forced synthesis` |
| [2101](#turns-2101) | warning | Run record | `Could not {op} the run record of turn {trace_id}: {err}` |
| [2102](#turns-2102) | warning | Retrieval | `Query embedding failed; the turn goes on without retrieval: {err}` |
| [2103](#turns-2103) | warning | Routing | `Routed role {role} unavailable; using the planner: {err}` |
| [2104](#turns-2104) | warning | Turn | `The model gave an empty answer after {tools_called} tool calls; forcing a synthesis` |
| [2105](#turns-2105) | warning | Turn | `The same tool calls repeated {identical_runs} times; forcing a synthesis` |
| [2106](#turns-2106) | warning | Compaction | `Working-set compaction failed at {stage}: {err}` |
| [2107](#turns-2107) | warning | Tools | `Tool {tool} hidden: the active profile has no capability entry for gate {gate}` |
| [2108](#turns-2108) | warning | Retry | `Planner stream dropped; retrying on a fresh connection (attempt {attempt}): {err}` |
| [2109](#turns-2109) | warning | Memory | `The turn's {record} was not saved: {err}` |
| [2201](#turns-2201) | warning | Turn gate | `Turn from {source} refused: {err}` |
| [3001](#turns-3001) | error | Turn | `turn error` |
| [3101](#turns-3101) | error | Tool call | `Tool {tool} failed after {duration}` |
| [3102](#turns-3102) | error | Routing | `Route {hook} panicked; {fallback}` |
| [3103](#turns-3103) | critical | Tool call | `Tool {tool} panicked: {panic}` |
| [9001](#turns-9001) | info | Tool call | `tool call {tool}` |
| [9002](#turns-9002) | info | Tool call | `tool result {tool}` |
| [9003](#turns-9003) | error | Tool call | `tool result {tool} failed` |

### turns 1

`Turn`

- **Level:** info · **Channel:** operational · **Task:** Turn · **Log:** turns
- **Fields:** `agent`, `text`, `duration`, `error` (sensitive, masked: `text`)
- **Find it:** `provider:turns id:1`

**What it means.** A turn started (text is the message), or ended: a stop or a fail, with its duration. A sub-agent's turn names the turn that spawned it as related.

**Why it happens.** A message reached the agent: typed in the TUI or the dashboard, relayed from a channel, or sent by a job or a peer. A turn fails when its model can't be reached, its context can't be assembled, or its tool calls keep failing validation.

**What to do.** None when it stops. When it fails, read its error, then the turn's other events (filter trace:<id>).

### turns 1001

`route observation`

- **Level:** info · **Channel:** operational · **Task:** Routing · **Log:** turns
- **Fields:** `text`
- **Find it:** `provider:turns id:1001`

**What it means.** The route classifier's view of the turn's message: the role it would pick, and whether it applied.

**Why it happens.** Routing is configured (route in config.json); every turn is observed, in shadow or active mode.

**What to do.** None. The route log has the decision's detail.

### turns 1002

`skills retrieved`

- **Level:** info · **Channel:** operational · **Task:** Skills · **Log:** turns
- **Fields:** `text`
- **Find it:** `provider:turns id:1002`

**What it means.** Saved skills matched the message and were added to the turn's prompt (text names them).

**Why it happens.** The skill library holds recipes close to what was asked.

**What to do.** None. A skill that keeps matching the wrong requests can be edited or deleted with /skills.

### turns 1003

`tool surface ranked`

- **Level:** info · **Channel:** operational · **Task:** Tools · **Log:** turns
- **Fields:** `text`
- **Find it:** `provider:turns id:1003`

**What it means.** Which tools the turn offers the model: all of them, or the top ranked by the message, or a tool hidden by its capability gate.

**Why it happens.** The tool catalog is large enough for ranking, or a profile gate dropped a tool.

**What to do.** None. If a tool the task needs is missing, check /tools and the profile's capabilities.

### turns 1004

`operator interjected`

- **Level:** info · **Channel:** operational · **Task:** Turn · **Log:** turns
- **Fields:** `iter`, `text` (sensitive, masked: `text`)
- **Find it:** `provider:turns id:1004`

**What it means.** The operator sent a message while the turn was running; the agent folded it in.

**Why it happens.** A message typed mid-turn in the TUI or the dashboard.

**What to do.** None.

### turns 1005

`final response`

- **Level:** info · **Channel:** operational · **Task:** Turn · **Log:** turns
- **Fields:** `agent`, `iter`, `text` (sensitive, masked: `text`)
- **Find it:** `provider:turns id:1005`

**What it means.** The turn's answer is ready.

**Why it happens.** The model answered without calling another tool.

**What to do.** None.

### turns 1101

`Tool {tool} finished in {duration}`

- **Level:** info · **Channel:** operational · **Task:** Tool call · **Log:** turns
- **Fields:** `agent`, `iter`, `tool`, `duration`
- **Find it:** `provider:turns id:1101`

**What it means.** A tool call returned. Its arguments and result are in the diagnostic channel (turns 9001 and 9002).

**Why it happens.** The model called a tool.

**What to do.** None. A tool that is often slow shows here first.

### turns 1201

`working set compacted`

- **Level:** info · **Channel:** operational · **Task:** Compaction · **Log:** turns
- **Fields:** `text`
- **Find it:** `provider:turns id:1201`

**What it means.** The conversation's working set was shortened (text: messages before and after): old tool results elided and older turns summarized.

**Why it happens.** The working set passed its budget's high-water mark, the context overflowed mid-turn, or the operator ran /compress.

**What to do.** None. Compaction that repeats every turn means the context budget is small for the conversation.

### turns 2001

`tool call rejected {tool}`

- **Level:** warning · **Channel:** operational · **Task:** Validation · **Log:** turns
- **Fields:** `agent`, `iter`, `tool`, `text`
- **Find it:** `provider:turns id:2001`

**What it means.** A tool call failed validation before it ran: an unknown tool, arguments that aren't JSON, or a missing or mistyped argument (text). The model gets the error and may retry.

**Why it happens.** The model called the tool wrongly: common with small local models, or after a tool's schema changed.

**What to do.** None when it recovers. If the turn stops with too many validation failures, try a stronger model for the planner role.

### turns 2002

`tool retry {tool}`

- **Level:** warning · **Channel:** operational · **Task:** Retry · **Log:** turns
- **Fields:** `agent`, `iter`, `tool`, `text`
- **Find it:** `provider:turns id:2002`

**What it means.** A tool call failed with a transient error and was retried (text: the attempt and the error).

**Why it happens.** The tool's service was briefly unreachable, timed out, or rate-limited the request.

**What to do.** None when a retry succeeds. Frequent retries point at the tool's service or the network.

### turns 2003

`loop ceiling hit — forced synthesis`

- **Level:** warning · **Channel:** operational · **Task:** Turn · **Log:** turns
- **Fields:** `agent`, `text` (sensitive, masked: `text`)
- **Find it:** `provider:turns id:2003`

**What it means.** The turn used all its planner iterations, or kept repeating one tool call, so the agent wrote an answer from what it had gathered (text).

**Why it happens.** The task needed more steps than plan_loop_ceiling allows, or the model was stuck.

**What to do.** Raise plan_loop_ceiling in config.json for long tasks, or use /goal; a model stuck in a loop may need a stronger planner.

### turns 2101

`Could not {op} the run record of turn {trace_id}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Run record · **Log:** turns
- **Fields:** `op`, `trace_id`, `err`
- **Find it:** `provider:turns id:2101`

**What it means.** The turn ran, but its record in runs.db was not written, so /runs and the dashboard won't show it (or show it unfinished).

**Why it happens.** runs.db could not be written: the disk is full, the file is locked or damaged.

**What to do.** Check free space and the data directory's permissions; tenant doctor checks runs.db.

### turns 2102

`Query embedding failed; the turn goes on without retrieval: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Retrieval · **Log:** turns
- **Fields:** `agent`, `err`
- **Find it:** `provider:turns id:2102`

**What it means.** The message could not be embedded, so the turn runs without recalled memories, episodes or skills.

**Why it happens.** The embedder role's model (often Ollama) is down, unreachable or not loaded.

**What to do.** Check the embedder's backend (tenant doctor, the model log).

### turns 2103

`Routed role {role} unavailable; using the planner: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Routing · **Log:** turns
- **Fields:** `agent`, `role`, `err`
- **Find it:** `provider:turns id:2103`

**What it means.** Active routing picked a role for the turn, but that role's model could not be resolved in time, so the turn runs on the planner.

**Why it happens.** The role's backend is down or slow, or the route timeout (route.timeout_ms) is shorter than its warm-up.

**What to do.** Check the role's backend in the model log, or raise route.timeout_ms.

### turns 2104

`The model gave an empty answer after {tools_called} tool calls; forcing a synthesis`

- **Level:** warning · **Channel:** operational · **Task:** Turn · **Log:** turns
- **Fields:** `agent`, `iter`, `tools_called`
- **Find it:** `provider:turns id:2104`

**What it means.** The model stopped calling tools but wrote nothing, so the agent asked it again, with tools off, to answer from what it gathered.

**Why it happens.** A thinking model's whole reply was a reasoning block, or the model gave up.

**What to do.** None when the synthesis answers. If it happens often, try another planner model.

### turns 2105

`The same tool calls repeated {identical_runs} times; forcing a synthesis`

- **Level:** warning · **Channel:** operational · **Task:** Turn · **Log:** turns
- **Fields:** `agent`, `iter`, `identical_runs`
- **Find it:** `provider:turns id:2105`

**What it means.** The model kept making the same tool calls, so the turn stopped looping and answered from what it had.

**Why it happens.** A weak model locked onto one call, or a tool keeps returning something the model doesn't accept.

**What to do.** Look at the repeated call's result in the turn's tool events; a stronger planner usually avoids it.

### turns 2106

`Working-set compaction failed at {stage}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Compaction · **Log:** turns
- **Fields:** `agent`, `stage`, `err`
- **Find it:** `provider:turns id:2106`

**What it means.** The working set could not be compacted, so the conversation stays long and the next turns may overflow the context.

**Why it happens.** The compressor's model failed or timed out (stage summary), or eliding old tool results failed (stage microcompaction).

**What to do.** Check the compressor's backend in the model log; /clear starts a fresh working set.

### turns 2107

`Tool {tool} hidden: the active profile has no capability entry for gate {gate}`

- **Level:** warning · **Channel:** operational · **Task:** Tools · **Log:** turns
- **Fields:** `tool`, `gate`, `profile`
- **Find it:** `provider:turns id:2107`

**What it means.** A tool was not offered to the model because its capability gate is unknown to the active profile. Written once per gate.

**Why it happens.** The profile has no capabilities entry for the gate, and no built-in rule covers it.

**What to do.** Add the gate to the profile's capabilities (allow or deny) so the choice is deliberate.

### turns 2108

`Planner stream dropped; retrying on a fresh connection (attempt {attempt}): {err}`

- **Level:** warning · **Channel:** operational · **Task:** Retry · **Log:** turns
- **Fields:** `agent`, `iter`, `attempt`, `err`
- **Find it:** `provider:turns id:2108`

**What it means.** The planner's streamed reply broke off mid-way, so the call was sent again on a new connection.

**Why it happens.** The backend recycled the connection (a GOAWAY or a reset), restarted, or the network dropped.

**What to do.** None when the retry succeeds. Repeated drops point at the backend or a proxy in front of it.

### turns 2109

`The turn's {record} was not saved: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Memory · **Log:** turns
- **Fields:** `agent`, `record`, `episode`, `err`
- **Find it:** `provider:turns id:2109`

**What it means.** Part of the turn's memory was not written: an archive event (the transcript), its episode, or the episode's route tags.

**Why it happens.** The memory store's database could not be written: the disk is full, or the file is locked or damaged.

**What to do.** Check free space and the data directory; tenant doctor checks the stores.

### turns 2201

`Turn from {source} refused: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Turn gate · **Log:** turns
- **Fields:** `source`, `err`
- **Find it:** `provider:turns id:2201`

**What it means.** A message was not run because the shared agent's turn queue had no room for it.

**Why it happens.** The queue is full, or this surface has used its share of it: turns are arriving faster than the agent finishes them.

**What to do.** Wait for the running turns to finish. A queue that stays full means a stuck turn (see the dashboard's status) or too many jobs at once.

### turns 3001

`turn error`

- **Level:** error · **Channel:** operational · **Task:** Turn · **Log:** turns
- **Fields:** `agent`, `iter`, `text`
- **Find it:** `provider:turns id:3001`

**What it means.** The turn hit an error (text): a planner call failed, the context overflowed, or its forced synthesis failed. Some are recovered from: an overflow is compacted and retried.

**Why it happens.** The model's backend failed or refused the request, or the conversation outgrew the model's context.

**What to do.** Read the error; the model log usually has the backend's side. A turn that ends on it also has a turn fail (turns 1).

### turns 3101

`Tool {tool} failed after {duration}`

- **Level:** error · **Channel:** operational · **Task:** Tool call · **Log:** turns
- **Fields:** `agent`, `iter`, `tool`, `duration`, `error`
- **Find it:** `provider:turns id:3101`

**What it means.** A tool call returned an error. The model gets the error and usually tries something else; the result is in the diagnostic channel (turns 9003), and error holds a failure outside the tool itself.

**Why it happens.** The tool refused its arguments, its service failed, or the tool is broken.

**What to do.** None when the turn recovers. A tool that fails every time needs its configuration or its service checked.

### turns 3102

`Route {hook} panicked; {fallback}`

- **Level:** error · **Channel:** operational · **Task:** Routing · **Log:** turns
- **Fields:** `agent`, `hook`, `fallback`, `panic`, `stack`
- **Find it:** `provider:turns id:3102`

**What it means.** The route classifier's code panicked. The turn went on without it.

**Why it happens.** A bug in the route classifier or its policy.

**What to do.** Report it with the stack; turn routing off (route in config.json) until it is fixed.

### turns 3103

`Tool {tool} panicked: {panic}`

- **Level:** critical · **Channel:** operational · **Task:** Tool call · **Log:** turns
- **Fields:** `tool`, `panic`, `stack`
- **Find it:** `provider:turns id:3103`

**What it means.** A tool's code panicked. Only that call failed: the model got an error result and the turn went on.

**Why it happens.** A bug in the tool or its plugin.

**What to do.** Report it with the stack, and disable the tool (/disable) if it keeps happening.

### turns 9001

`tool call {tool}`

- **Level:** info · **Channel:** diagnostic · **Task:** Tool call · **Log:** turns
- **Fields:** `agent`, `iter`, `tool`, `args` (sensitive, masked: `args`)
- **Find it:** `provider:turns id:9001`

**What it means.** A tool call about to run, with its arguments.

**Why it happens.** The model called a tool.

**What to do.** None.

### turns 9002

`tool result {tool}`

- **Level:** info · **Channel:** diagnostic · **Task:** Tool call · **Log:** turns
- **Fields:** `agent`, `iter`, `tool`, `duration`, `result` (sensitive, masked: `result`)
- **Find it:** `provider:turns id:9002`

**What it means.** A tool call's result, as the model got it (before the result cap).

**Why it happens.** A tool call returned.

**What to do.** None.

### turns 9003

`tool result {tool} failed`

- **Level:** error · **Channel:** diagnostic · **Task:** Tool call · **Log:** turns
- **Fields:** `agent`, `iter`, `tool`, `duration`, `result` (sensitive, masked: `result`)
- **Find it:** `provider:turns id:9003`

**What it means.** A failed tool call's result: the error the model got.

**Why it happens.** A tool call returned an error.

**What to do.** See turns 3101.
