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

## discord

Writes the `discord` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#discord-1) | info | Connection | `Discord gateway connection` |
| [2](#discord-2) | info | Connection | `Gateway session ready ({mode})` |
| [3](#discord-3) | info | Connection | `Discord asked the gateway to reconnect; {next}` |
| [1001](#discord-1001) | info | Message | `Message {message} from {author} in channel {channel}` |
| [1002](#discord-1002) | info | Relay turn | `Relay turn in channel {channel} (approvals: {approvals})` |
| [1003](#discord-1003) | info | Delivery | `Message to channel {channel}` |
| [1201](#discord-1201) | info | Approval card | `Approval card {message} posted in channel {channel} ({layout})` |
| [2001](#discord-2001) | warning | Connection | `Gateway session ended; {next}: {err}` |
| [2002](#discord-2002) | warning | Connection | `Gateway could not connect ({step}); retrying: {err}` |
| [2003](#discord-2003) | warning | Connection | `Gateway heartbeat failed: {err}` |
| [2004](#discord-2004) | warning | Connection | `Gateway heartbeat not acknowledged; dropping the connection` |
| [2005](#discord-2005) | warning | Connection | `Undecodable {payload} from the gateway; {effect}: {err}` |
| [2101](#discord-2101) | warning | Message | `Messages from {author} are coming too fast; told them to slow down` |
| [2102](#discord-2102) | warning | Delivery | `Discord rate-limited a {method} on {resource}; retry after {retry_after}` |
| [2103](#discord-2103) | warning | Message | `An attachment could not be downloaded: {err}` |
| [2104](#discord-2104) | warning | Relay inbox | `The relay inbox retention sweep is incomplete: {err}` |
| [2201](#discord-2201) | warning | Approval card | `Approval card fell back to {fallback} in channel {channel}: {err}` |
| [2202](#discord-2202) | warning | Approval card | `Approval for {action} denied: no operator channel to post the card in` |
| [2203](#discord-2203) | warning | Approval card | `Approval card {message} could not be updated to {verdict}: {err}` |
| [2204](#discord-2204) | warning | Approval card | `A button click ({click}) could not be acknowledged: {err}` |

### discord 1

`Discord gateway connection`

- **Level:** info · **Channel:** operational · **Task:** Connection · **Log:** discord
- **Find it:** `provider:discord id:1`

**What it means.** The relay's connection to the Discord gateway, from the relay starting to it stopping. Drops in between are resumed or re-identified inside it; it fails only when Discord refuses the bot for good.

**Why it happens.** Start: the relay was turned on. Stop: it was turned off, its token was swapped, or Tenant stopped. Fail: Discord closed with a fatal code (4004 bad token, 4013/4014 intents the bot may not use).

**What to do.** On a fail, check the bot token (/skill configure discord) and that the Message Content intent is enabled in the Discord Developer Portal, then turn the relay on again (/relay on).

### discord 2

`Gateway session ready ({mode})`

- **Level:** info · **Channel:** operational · **Task:** Connection · **Log:** discord
- **Fields:** `mode`, `bot`
- **Find it:** `provider:discord id:2`

**What it means.** The gateway accepted the bot: a new session (identified) or the previous one picked up where it left off (resumed).

**Why it happens.** The relay connected, or reconnected after a drop.

**What to do.** None. After a drop, a resumed session missed no messages; an identified one may have.

### discord 3

`Discord asked the gateway to reconnect; {next}`

- **Level:** info · **Channel:** operational · **Task:** Connection · **Log:** discord
- **Fields:** `next`
- **Find it:** `provider:discord id:3`

**What it means.** Discord asked the relay to reconnect (opcode 7), and it did.

**Why it happens.** Routine: Discord moves sessions between servers.

**What to do.** None.

### discord 1001

`Message {message} from {author} in channel {channel}`

- **Level:** info · **Channel:** operational · **Task:** Message · **Log:** discord
- **Fields:** `message`, `channel`, `guild`, `author`, `bytes`, `attachments`, `text` (sensitive, masked: `text`)
- **Find it:** `provider:discord id:1001`

**What it means.** The relay accepted a message: a DM from the operator, or a server message that mentions the bot.

**Why it happens.** Someone wrote to the bot.

**What to do.** None. Messages from anyone else, and server messages that don't mention the bot, are ignored without an event.

### discord 1002

`Relay turn in channel {channel} (approvals: {approvals})`

- **Level:** info · **Channel:** operational · **Task:** Relay turn · **Log:** discord
- **Fields:** `channel`, `approvals`
- **Find it:** `provider:discord id:1002`

**What it means.** A turn of the Discord agent, from the message that started it until its answer is ready. Approvals says where its gated actions ask: the broker (relay.permissions), the approver (buttons) or none.

**Why it happens.** The operator or a server member asked the bot something. A fail is a turn that ended with an error, which the relay also replies in Discord; a stopped turn is not a fail.

**What to do.** On a fail, read the error and the turns log for the same trace.

### discord 1003

`Message to channel {channel}`

- **Level:** info · **Channel:** operational · **Task:** Delivery · **Log:** discord
- **Fields:** `channel`, `bytes`
- **Find it:** `provider:discord id:1003`

**What it means.** The bot posting one message to a channel or DM: a relay answer, a notice, an operator DM or an agent's discord_send.

**Why it happens.** A fail is a message Discord did not accept: the bot lacks access to the channel (50001), the message is empty or too long, Discord rate-limited it, or the network failed.

**What to do.** On a fail, check the error: grant the bot access to the channel, or check the host's network. The message was not sent and is not retried.

### discord 1201

`Approval card {message} posted in channel {channel} ({layout})`

- **Level:** info · **Channel:** operational · **Task:** Approval card · **Log:** discord
- **Fields:** `message`, `channel`, `layout`, `action`
- **Find it:** `provider:discord id:1201`

**What it means.** An approval card for a gated action reached Discord, in the v2 (Components) or legacy layout.

**Why it happens.** A Discord-driven turn reached an action whose category asks for approval.

**What to do.** Approve or deny it on the card.

### discord 2001

`Gateway session ended; {next}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Connection · **Log:** discord
- **Fields:** `next`, `err`
- **Find it:** `provider:discord id:2001`

**What it means.** A gateway session ended unexpectedly, and the relay is reconnecting: resuming the same session, or identifying a new one.

**Why it happens.** The network dropped, Discord closed the connection, a RESUME or IDENTIFY could not be sent, Discord invalidated the session, or a heartbeat went unanswered.

**What to do.** None when it is rare: the relay reconnects by itself with backoff. If it repeats, check the host's network; a new session (re-identifying) may have missed messages sent meanwhile.

### discord 2002

`Gateway could not connect ({step}); retrying: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Connection · **Log:** discord
- **Fields:** `step`, `err`
- **Find it:** `provider:discord id:2002`

**What it means.** The relay could not reach the Discord gateway, and retries with backoff (up to a minute apart).

**Why it happens.** The host is offline, DNS or a proxy fails, or Discord is down; fetching the gateway URL also fails with a revoked bot token.

**What to do.** Check the host's network and discordstatus.com. If the error is 401, the bot token was revoked: configure a new one (/skill configure discord).

### discord 2003

`Gateway heartbeat failed: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Connection · **Log:** discord
- **Fields:** `err`
- **Find it:** `provider:discord id:2003`

**What it means.** A heartbeat could not be sent to the gateway. When the timed heartbeat fails, the relay drops the connection and reconnects.

**Why it happens.** The connection broke under the relay: a network drop or a closed socket.

**What to do.** None when it is rare; the session-ended event that follows says how the relay reconnects.

### discord 2004

`Gateway heartbeat not acknowledged; dropping the connection`

- **Level:** warning · **Channel:** operational · **Task:** Connection · **Log:** discord
- **Find it:** `provider:discord id:2004`

**What it means.** Discord did not acknowledge the last heartbeat before the next was due, so the connection is a zombie: the relay closes it and reconnects.

**Why it happens.** The connection stalled: a half-open socket, a sleeping host, or network loss that the socket has not noticed yet.

**What to do.** None when it is rare. If it repeats, check for the host sleeping or a flaky network.

### discord 2005

`Undecodable {payload} from the gateway; {effect}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Connection · **Log:** discord
- **Fields:** `payload`, `effect`, `err`
- **Find it:** `provider:discord id:2005`

**What it means.** The gateway sent a payload the relay could not decode. The effect says what happened instead: a message or click dropped, a session that cannot resume.

**Why it happens.** Discord changed a payload's shape, or the frame was corrupted.

**What to do.** If it repeats, report it with the error: Tenant's gateway client needs updating.

### discord 2101

`Messages from {author} are coming too fast; told them to slow down`

- **Level:** warning · **Channel:** operational · **Task:** Message · **Log:** discord
- **Fields:** `author`, `channel`
- **Find it:** `provider:discord id:2101`

**What it means.** The relay dropped a message because its sender is over the relay's rate limit (about 6 messages in 30 seconds).

**Why it happens.** Someone is sending messages faster than the limit: a burst, a paste split into many messages, or a flood in a server.

**What to do.** Wait and send again. If it is a flood from a server member, remove the bot from that server or restrict who can mention it.

### discord 2102

`Discord rate-limited a {method} on {resource}; retry after {retry_after}`

- **Level:** warning · **Channel:** operational · **Task:** Delivery · **Log:** discord
- **Fields:** `method`, `resource`, `retry_after`
- **Find it:** `provider:discord id:2102`

**What it means.** Discord refused a request with 429 Too Many Requests. The request is not retried; the event after it says what failed.

**Why it happens.** The bot sent too many requests in a short time: a long answer in many chunks, a burst of approval cards, or another program using the same bot token.

**What to do.** None when it is rare. If it repeats, make sure no other program uses this bot's token.

### discord 2103

`An attachment could not be downloaded: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Message · **Log:** discord
- **Fields:** `file`, `err` (sensitive, masked: `file`)
- **Find it:** `provider:discord id:2103`

**What it means.** A file the operator sent could not be saved to the relay inbox; the turn goes ahead without it and the operator is told.

**Why it happens.** Discord's CDN link expired or failed, the file is bigger than it claimed, or the inbox folder could not be written.

**What to do.** Send the file again. If it repeats, check free disk space and the permissions of <data>/relay-inbox.

### discord 2104

`The relay inbox retention sweep is incomplete: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Relay inbox · **Log:** discord
- **Fields:** `dir`, `failures`, `err`
- **Find it:** `provider:discord id:2104`

**What it means.** Files in the relay inbox past their retention (privacy.retention_days.relay_inbox) could not all be deleted, so they are kept longer than configured.

**Why it happens.** The inbox folder or a file in it could not be read or deleted: permissions, a file held open, or a disk error.

**What to do.** Check the permissions of <data>/relay-inbox and delete the old files by hand if needed. The sweep runs again with the first attachment after the relay restarts.

### discord 2201

`Approval card fell back to {fallback} in channel {channel}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Approval card · **Log:** discord
- **Fields:** `fallback`, `channel`, `err`
- **Find it:** `provider:discord id:2201`

**What it means.** Discord rejected an approval card, so the relay posted it in a simpler form: legacy buttons, or a text prompt the operator answers with APPROVE-<nonce> or deny.

**Why it happens.** Discord did not accept the card's layout (an older API surface, a missing permission), or could not be reached.

**What to do.** None when it is rare; the prompt still reached the operator. If every card falls back, set relay.card_style to legacy; if the text prompt fails too, the action is denied when the card times out.

### discord 2202

`Approval for {action} denied: no operator channel to post the card in`

- **Level:** warning · **Channel:** operational · **Task:** Approval card · **Log:** discord
- **Fields:** `action`
- **Find it:** `provider:discord id:2202`

**What it means.** A gated action was denied without asking, because the relay had no channel to ask the operator in.

**Why it happens.** The action came from a Discord-driven turn after the relay lost its channel, or the relay is not fully configured.

**What to do.** Turn the relay on (/relay on) with the operator set (/relay allow <id>), and ask again from Discord.

### discord 2203

`Approval card {message} could not be updated to {verdict}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Approval card · **Log:** discord
- **Fields:** `message`, `verdict`, `err`
- **Find it:** `provider:discord id:2203`

**What it means.** An approval card was resolved, but its message in Discord still shows live buttons. The verdict applies anyway; a later click only gets a "resolved" reply.

**Why it happens.** Discord refused the edit: the message was deleted, the bot lost access to the channel, or the network failed.

**What to do.** None: the decision stands. Delete the stale card if it confuses.

### discord 2204

`A button click ({click}) could not be acknowledged: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Approval card · **Log:** discord
- **Fields:** `click`, `err`
- **Find it:** `provider:discord id:2204`

**What it means.** Discord was not told that a button click was handled, so the clicker sees "This interaction failed". For a verdict click, the verdict still applies.

**Why it happens.** The acknowledgement missed Discord's 3-second window (a slow network or a busy host), or Discord rejected it.

**What to do.** None when it is rare. Check the card or the relay's reply to see what was decided.

## imessage

Writes the `imessage` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#imessage-1) | info | Responder | `iMessage responder, polling every {interval}` |
| [1001](#imessage-1001) | info | Message | `Message {row} received ({bytes} bytes)` |
| [1002](#imessage-1002) | info | Turn | `iMessage turn` |
| [1003](#imessage-1003) | info | Delivery | `iMessage of {bytes} bytes` |
| [2001](#imessage-2001) | warning | Poll | `The responder cannot read new messages: {err}` |
| [9001](#imessage-9001) | verbose | Poll | `Poll for new iMessages` |

### imessage 1

`iMessage responder, polling every {interval}`

- **Level:** info · **Channel:** operational · **Task:** Responder · **Log:** imessage
- **Fields:** `interval`
- **Find it:** `provider:imessage id:1`

**What it means.** The iMessage responder: it polls Messages for new texts and answers them with the iMessage agent.

**Why it happens.** Start: /imessage on, or Tenant started with it on. Stop: /imessage off, or Tenant stopped.

**What to do.** None.

### imessage 1001

`Message {row} received ({bytes} bytes)`

- **Level:** info · **Channel:** operational · **Task:** Message · **Log:** imessage
- **Fields:** `row`, `bytes`, `from`, `chat`, `text` (sensitive, masked: `from`, `chat`, `text`)
- **Find it:** `provider:imessage id:1001`

**What it means.** The responder picked up a text from an allowed handle: an approval reply, or a message it will answer.

**Why it happens.** Someone on the allowlist (/imessage allow) texted this Mac.

**What to do.** None. Texts from handles not on the allowlist, and this Mac's own, are skipped without an event.

### imessage 1002

`iMessage turn`

- **Level:** info · **Channel:** operational · **Task:** Turn · **Log:** imessage
- **Fields:** `chat` (sensitive, masked: `chat`)
- **Find it:** `provider:imessage id:1002`

**What it means.** A turn of the iMessage agent, from the text that started it until its answer is ready. A fail is a turn that ended with an error; the texter is told to try again. A stopped responder's turn is not a fail.

**Why it happens.** An allowed handle texted a message that is not an approval reply.

**What to do.** On a fail, read the error and the turns log for the same trace.

### imessage 1003

`iMessage of {bytes} bytes`

- **Level:** info · **Channel:** operational · **Task:** Delivery · **Log:** imessage
- **Fields:** `bytes`, `chat` (sensitive, masked: `chat`)
- **Find it:** `provider:imessage id:1003`

**What it means.** The responder sending one iMessage: an answer, a busy or unavailable notice, or an approval prompt.

**Why it happens.** A fail is a text Messages did not send: osascript failed, Messages is not signed in, or Tenant lacks the Automation permission for Messages.

**What to do.** On a fail, open Messages and check it is signed in, and allow Tenant (or its terminal) under System Settings > Privacy & Security > Automation. The text is not retried.

### imessage 2001

`The responder cannot read new messages: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Poll · **Log:** imessage
- **Fields:** `err`
- **Find it:** `provider:imessage id:2001`

**What it means.** Polling Messages for new texts failed, so the responder is not answering. It keeps polling every few seconds and records this again only when the error changes.

**Why it happens.** Tenant lacks Full Disk Access (needed to read ~/Library/Messages/chat.db), the database is locked or moved, or the cursor could not be saved.

**What to do.** Grant Full Disk Access to Tenant (or its terminal) under System Settings > Privacy & Security, then /imessage off and on.

### imessage 9001

`Poll for new iMessages`

- **Level:** verbose · **Channel:** diagnostic · **Task:** Poll · **Log:** imessage
- **Find it:** `provider:imessage id:9001`

**What it means.** One poll of Messages for new texts, with how long it took.

**Why it happens.** The responder polls every few seconds while it is on.

**What to do.** None. A failed poll also records the responder's poll warning, once per error.

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
