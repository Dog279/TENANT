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

## model

Writes the `model` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [2001](#model-2001) | warning | Failover | `Model fallback: {from} {reason}, using {to}` |
| [2002](#model-2002) | warning | Health | `{profile} is slow ({average} average); skipped for {cooldown}` |
| [2003](#model-2003) | warning | Fallback chain | `Fallback {profile} skipped for role {role}: {err}` |
| [2004](#model-2004) | warning | Fallback chain | `Fallback provider {provider} skipped: {err}` |
| [2005](#model-2005) | warning | Configuration | `health_gating is set but no fallbacks are configured; it has nothing to move to` |
| [2006](#model-2006) | warning | Reasoning effort | `Reasoning effort {effort} unavailable for {profile}; using its default: {err}` |
| [2007](#model-2007) | warning | Swap | `Switched to {provider}, but it is not ready: {problem}` |
| [2008](#model-2008) | warning | Budget | `Ignoring {env}={value}: not a positive token count` |
| [2009](#model-2009) | warning | Budget | `The claudecode spend ledger was not saved: {err}` |
| [3001](#model-3001) | error | Call | `Model {profile} unavailable ({reason}): {err}` |
| [3002](#model-3002) | error | Usage | `Token usage for {model} was not recorded: {err}` |
| [5001](#model-5001) | info | Swap | `Primary model switched to {provider} ({model})` |
| [9001](#model-9001) | info | Call | `Model call to {profile}` |
| [9002](#model-9002) | info | Call | `{profile} used {prompt_tokens} prompt and {completion_tokens} completion tokens` |

### model 2001

`Model fallback: {from} {reason}, using {to}`

- **Level:** warning · **Channel:** operational · **Task:** Failover · **Log:** model
- **Fields:** `from`, `to`, `reason`, `err`
- **Find it:** `provider:model id:2001`

**What it means.** A model call failed on one link of its fallback chain and moved to the next.

**Why it happens.** The provider was rate limited, out of credits, unreachable or returned a server error.

**What to do.** None while the fallback serves. Out of credits: recharge the provider. Otherwise check the provider's status; it is retried after a short cooldown.

### model 2002

`{profile} is slow ({average} average); skipped for {cooldown}`

- **Level:** warning · **Channel:** operational · **Task:** Health · **Log:** model
- **Fields:** `profile`, `average`, `cooldown`
- **Find it:** `provider:model id:2002`

**What it means.** Health gating moved calls off a profile that answers, but too slowly.

**Why it happens.** Its average call time passed health_gating's threshold: an overloaded server, a cold model or a slow network.

**What to do.** None; it is tried again after the cooldown. If it recurs, check the server's load, or raise the threshold in config.json health_gating.

### model 2003

`Fallback {profile} skipped for role {role}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Fallback chain · **Log:** model
- **Fields:** `profile`, `role`, `err`
- **Find it:** `provider:model id:2003`

**What it means.** A fallback profile could not be added to a role's chain, so that role has one fallback fewer.

**Why it happens.** The profile is not registered, or its backend could not be built: a missing key or an unknown backend.

**What to do.** Check the provider with /model; fix its key or endpoint, then /model fallback to reinstall the chain.

### model 2004

`Fallback provider {provider} skipped: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Fallback chain · **Log:** model
- **Fields:** `provider`, `err`
- **Find it:** `provider:model id:2004`

**What it means.** A provider listed in config.json fallbacks was left out of the fallback chain.

**Why it happens.** It is not a configured provider, or its profiles could not be built or registered.

**What to do.** Check the name against /model, or remove it with /model fallback.

### model 2005

`health_gating is set but no fallbacks are configured; it has nothing to move to`

- **Level:** warning · **Channel:** operational · **Task:** Configuration · **Log:** model
- **Find it:** `provider:model id:2005`

**What it means.** Latency health gating is on, but there is no fallback to prefer over a slow model.

**Why it happens.** config.json sets health_gating without fallbacks.

**What to do.** Add a fallback with /model fallback, or remove health_gating.

### model 2006

`Reasoning effort {effort} unavailable for {profile}; using its default: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Reasoning effort · **Log:** model
- **Fields:** `profile`, `effort`, `err`
- **Find it:** `provider:model id:2006`

**What it means.** A turn asked for a different reasoning effort, but that variant of the model could not be built.

**Why it happens.** The backend refused the profile with the new effort.

**What to do.** None; the turn ran at the profile's own effort. Check the provider's reasoning setting with /model.

### model 2007

`Switched to {provider}, but it is not ready: {problem}`

- **Level:** warning · **Channel:** operational · **Task:** Swap · **Log:** model
- **Fields:** `provider`, `model`, `endpoint`, `problem`
- **Find it:** `provider:model id:2007`

**What it means.** The primary model was switched, but the check right after found it unreachable or not serving the model.

**Why it happens.** The server is down or still loading, or it serves a different model name.

**What to do.** Start the server, or fix the model name with /model; the next message retries.

### model 2008

`Ignoring {env}={value}: not a positive token count`

- **Level:** warning · **Channel:** operational · **Task:** Budget · **Log:** model
- **Fields:** `env`, `value`
- **Find it:** `provider:model id:2008`

**What it means.** The claudecode backend's weekly token budget is set but unreadable, so no budget is enforced.

**Why it happens.** The environment variable holds something other than a positive whole number.

**What to do.** Set it to a token count, or unset it.

### model 2009

`The claudecode spend ledger was not saved: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Budget · **Log:** model
- **Fields:** `path`, `err`
- **Find it:** `provider:model id:2009`

**What it means.** A claudecode call's tokens were not added to the weekly spend ledger, so the budget undercounts.

**Why it happens.** The ledger file in the config folder could not be written: permissions or a full disk.

**What to do.** Check that the file named by path is writable.

### model 3001

`Model {profile} unavailable ({reason}): {err}`

- **Level:** error · **Channel:** operational · **Task:** Call · **Log:** model
- **Fields:** `profile`, `backend`, `model`, `reason`, `err`
- **Find it:** `provider:model id:3001`

**What it means.** A model call failed because its provider could not serve it.

**Why it happens.** The server is down or unreachable, rate limited, out of credits, or returned a server error.

**What to do.** Check the provider (/model, doctor). With fallbacks configured the call moved on; otherwise the turn failed.

### model 3002

`Token usage for {model} was not recorded: {err}`

- **Level:** error · **Channel:** operational · **Task:** Usage · **Log:** model
- **Fields:** `model`, `err`
- **Find it:** `provider:model id:3002`

**What it means.** One model call's tokens are missing from the usage ledger (usage.db).

**Why it happens.** The ledger could not be written: a locked or damaged database, or a full disk.

**What to do.** Check usage.db in the data folder; the per-call tokens are also in model's diagnostic events (9002).

### model 5001

`Primary model switched to {provider} ({model})`

- **Level:** info · **Channel:** operational · **Task:** Swap · **Log:** model
- **Fields:** `provider`, `model`, `backend`, `endpoint`
- **Keywords:** `config`
- **Find it:** `provider:model id:5001`

**What it means.** The operator switched the primary model live; every agent and job uses it from the next call.

**Why it happens.** /model use, or a key reload that re-applies the active provider.

**What to do.** None.

### model 9001

`Model call to {profile}`

- **Level:** info · **Channel:** diagnostic · **Task:** Call · **Log:** model
- **Fields:** `profile`, `backend`, `model`
- **Find it:** `provider:model id:9001`

**What it means.** One call to a model, as an activity under its turn: start, then stop with its duration, or fail with the error.

**Why it happens.** The agent, a sub-agent or a job asked the model for a completion.

**What to do.** None. For slow or failing calls, compare durations across backend and profile.

### model 9002

`{profile} used {prompt_tokens} prompt and {completion_tokens} completion tokens`

- **Level:** info · **Channel:** diagnostic · **Task:** Call · **Log:** model
- **Fields:** `profile`, `backend`, `model`, `prompt_tokens`, `completion_tokens`
- **Find it:** `provider:model id:9002`

**What it means.** The tokens one model call used, as its backend reported them.

**Why it happens.** A call finished and its backend reported usage.

**What to do.** None. Sum them by model or trace to see what a turn or a job cost.

## route

Writes the `route` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1001](#route-1001) | info | Decision | `Route decision {decision_id}: {summary}` |
| [2001](#route-2001) | warning | Decision | `Route decision {decision_id} fell back to the planner: {err}` |
| [2002](#route-2002) | warning | Classifier | `The route classifier failed repeatedly; classification is skipped until it recovers` |
| [2003](#route-2003) | warning | Classifier | `Route classifier warm-up failed at {endpoint}: {err}` |
| [2004](#route-2004) | warning | Rules | `Route rule {rule} for role {role}: {problem}` |
| [2005](#route-2005) | warning | Decision | `Routed role {role} unavailable; using {fallback}: {err}` |
| [2006](#route-2006) | warning | Decision log | `Route decision log prune failed: {err}` |
| [2007](#route-2007) | warning | Configuration | `Planner-big provider {provider} unavailable while routing is off: {err}` |
| [3001](#route-3001) | error | Decision log | `Route decision {decision_id} was not saved: {err}` |
| [3002](#route-3002) | error | Decision log | `The operator's confirmation of route decision {decision_id} was not saved: {err}` |
| [3003](#route-3003) | error | Guard | `Route {stage} panicked: {panic}` |
| [4001](#route-4001) | warning | Privacy override | `Route forced to profile {profile}; never_cloud is pierced` |
| [4002](#route-4002) | warning | Privacy override | `Route decision {decision_id} used forced profile {profile}, piercing never_cloud` |

### route 1001

`Route decision {decision_id}: {summary}`

- **Level:** info · **Channel:** operational · **Task:** Decision · **Log:** route
- **Fields:** `decision_id`, `summary`, `mode`, `status`, `tier`, `role`, `profile`, `reason`, `agent`, `session`
- **Find it:** `provider:route id:1001`

**What it means.** The router classified a prompt and chose where it runs (or, in shadow mode, where it would have).

**Why it happens.** A turn started with routing on.

**What to do.** None. /route why shows the latest decisions in full; route-decisions.jsonl has every one by decision_id.

### route 2001

`Route decision {decision_id} fell back to the planner: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Decision · **Log:** route
- **Fields:** `decision_id`, `summary`, `mode`, `agent`, `session`, `err`
- **Find it:** `provider:route id:2001`

**What it means.** The router could not classify a prompt or apply its policy, so the turn ran on the planner.

**Why it happens.** The classifier is down, slow or returned something unreadable, or a bound profile changed mid-decision.

**What to do.** Check /route status. /route why shows the decision and the classifier's raw output.

### route 2002

`The route classifier failed repeatedly; classification is skipped until it recovers`

- **Level:** warning · **Channel:** operational · **Task:** Classifier · **Log:** route
- **Fields:** `decision_id`
- **Find it:** `provider:route id:2002`

**What it means.** The router's circuit breaker opened: turns skip the classifier and run on the planner for a while.

**Why it happens.** Several classifier calls in a row failed or timed out.

**What to do.** Check the classifier's server (/route status). The breaker retries by itself.

### route 2003

`Route classifier warm-up failed at {endpoint}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Classifier · **Log:** route
- **Fields:** `endpoint`, `model`, `err`
- **Find it:** `provider:route id:2003`

**What it means.** The classifier model could not be pre-loaded, so the first decisions may time out.

**Why it happens.** Its server is down or unreachable, or does not serve the configured model.

**What to do.** Start the classifier's server, or fix route.endpoint and route.model in config.json.

### route 2004

`Route rule {rule} for role {role}: {problem}`

- **Level:** warning · **Channel:** operational · **Task:** Rules · **Log:** route
- **Fields:** `rule`, `role`, `profile`, `problem`
- **Find it:** `provider:route id:2004`

**What it means.** A routing rule points at a role that can't serve it well, or at no model at all.

**Why it happens.** The rule's role has no bound profile, or its profile is tagged low quality for the rule's kind of prompt.

**What to do.** Bind the role to a suitable provider, or change the rule in config.json route.rules.

### route 2005

`Routed role {role} unavailable; using {fallback}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Decision · **Log:** route
- **Fields:** `decision_id`, `role`, `fallback`, `err`
- **Find it:** `provider:route id:2005`

**What it means.** The router chose a role, but it could not be used for this turn, so the turn ran elsewhere.

**Why it happens.** The role's binding changed after the decision, or never_cloud ruled out its profile.

**What to do.** Check the role's binding with /route status and /model.

### route 2006

`Route decision log prune failed: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Decision log · **Log:** route
- **Fields:** `err`
- **Find it:** `provider:route id:2006`

**What it means.** Old decisions were not removed from route-decisions.jsonl, so it keeps growing.

**Why it happens.** The file or its lock could not be read or rewritten: permissions, a full disk, or another process holding it.

**What to do.** Check the file in the data folder; the next daily prune retries.

### route 2007

`Planner-big provider {provider} unavailable while routing is off: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Configuration · **Log:** route
- **Fields:** `provider`, `err`
- **Find it:** `provider:route id:2007`

**What it means.** route.big_role names a provider that can't be built. Routing is off, so nothing uses it yet.

**Why it happens.** The provider was removed or renamed, or its key is missing.

**What to do.** Fix or clear route.big_role before turning routing on.

### route 3001

`Route decision {decision_id} was not saved: {err}`

- **Level:** error · **Channel:** operational · **Task:** Decision log · **Log:** route
- **Fields:** `decision_id`, `err`
- **Find it:** `provider:route id:3001`

**What it means.** A decision was not written to route-decisions.jsonl; /route why still shows it until restart.

**Why it happens.** The data folder or the file's lock could not be written: permissions or a full disk.

**What to do.** Check that route-decisions.jsonl in the data folder is writable.

### route 3002

`The operator's confirmation of route decision {decision_id} was not saved: {err}`

- **Level:** error · **Channel:** operational · **Task:** Decision log · **Log:** route
- **Fields:** `decision_id`, `err`
- **Find it:** `provider:route id:3002`

**What it means.** Accepting an assisted suggestion with !big was not recorded as a gold label.

**Why it happens.** route-decisions.jsonl could not be rewritten: permissions, a full disk, or its lock.

**What to do.** Check the file in the data folder; the turn itself ran as asked.

### route 3003

`Route {stage} panicked: {panic}`

- **Level:** error · **Channel:** operational · **Task:** Guard · **Log:** route
- **Fields:** `stage`, `panic`, `stack`
- **Find it:** `provider:route id:3003`

**What it means.** A bug in the router was caught; the turn ran on the planner, or without the decision's annotation.

**Why it happens.** A programming error in the classifier, the policy input or a decision annotator.

**What to do.** Report it with the stack; routing keeps working for other turns.

### route 4001

`Route forced to profile {profile}; never_cloud is pierced`

- **Level:** warning · **Channel:** operational · **Task:** Privacy override · **Log:** route
- **Fields:** `profile`, `binding`
- **Keywords:** `audit`
- **Find it:** `provider:route id:4001`

**What it means.** The operator forced routing to one profile, overriding never_cloud.

**Why it happens.** /route force, or route.force_profile in config.json at start.

**What to do.** None when intended. Any other /route mode (on, shadow, off, …) clears it and restores never_cloud.

### route 4002

`Route decision {decision_id} used forced profile {profile}, piercing never_cloud`

- **Level:** warning · **Channel:** operational · **Task:** Privacy override · **Log:** route
- **Fields:** `decision_id`, `agent`, `profile`, `role`
- **Keywords:** `audit`
- **Find it:** `provider:route id:4002`

**What it means.** A turn ran on the forced profile although never_cloud would have kept it local.

**Why it happens.** A privacy override (4001) is armed.

**What to do.** None when intended; otherwise switch /route to another mode, which clears the force.

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
