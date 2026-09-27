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

## browser

Writes the `browser` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [2001](#browser-2001) | warning | Sandbox | `Chrome's process sandbox is disabled ({outer})` |
| [3001](#browser-3001) | error | DevTools | `chromedp: {err}` |

### browser 2001

`Chrome's process sandbox is disabled ({outer})`

- **Level:** warning · **Channel:** operational · **Task:** Sandbox · **Log:** browser
- **Fields:** `outer`
- **Keywords:** `security`
- **Find it:** `provider:browser id:2001`

**What it means.** Chrome runs without its own process sandbox, because browser_no_sandbox is set.

**Why it happens.** The operator turned it off for an environment where Chrome's sandbox can't start; Tenant detected the named outer sandbox.

**What to do.** Keep it only inside that outer sandbox; turn browser_no_sandbox off anywhere else.

### browser 3001

`chromedp: {err}`

- **Level:** error · **Channel:** operational · **Task:** DevTools · **Log:** browser
- **Fields:** `err`
- **Find it:** `provider:browser id:3001`

**What it means.** The browser automation library reported an error talking to Chrome.

**Why it happens.** Chrome sent something chromedp could not handle, or the tab went away mid-command.

**What to do.** Usually harmless once; if it repeats, check the browser plugin's recent tool results and restart the browser with /disable web then /enable web.

## cron

Writes the `cron` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#cron-1) | info | Scheduler | `Scheduler started with {jobs} jobs, {enabled} enabled` |
| [1001](#cron-1001) | info | Job run | `Run of job {job}` |
| [1002](#cron-1002) | info | Schedule | `Job {job} missed its run at {due}; it runs once now` |
| [2001](#cron-2001) | warning | Schedule | `Job {job} can't be scheduled and stays disabled: {err}` |
| [2002](#cron-2002) | warning | Schedule | `Job {job} missed its run at {due}: {reason}` |
| [2003](#cron-2003) | warning | Run history | `The run of job {job} was not saved to the run history: {err}` |
| [2004](#cron-2004) | warning | Run history | `The run history could not be read, so missed runs aren't caught up: {err}` |
| [5001](#cron-5001) | info | Job | `Job {job} added: {spec}` |
| [5002](#cron-5002) | info | Job | `Job {job} {state}` |
| [5003](#cron-5003) | info | Job | `Job {job} removed` |
| [5004](#cron-5004) | info | Job | `cron-exec turned {state}` |
| [9001](#cron-9001) | verbose | Job run | `Job {job} printed {bytes} bytes` |

### cron 1

`Scheduler started with {jobs} jobs, {enabled} enabled`

- **Level:** info · **Channel:** operational · **Task:** Scheduler · **Log:** cron
- **Fields:** `jobs`, `enabled`, `tz`, `catchup`
- **Find it:** `provider:cron id:1`

**What it means.** The cron scheduler loaded its jobs and started checking for due runs.

**Why it happens.** Serve or the TUI started with cron available.

**What to do.** None. A job that can't be scheduled is reported on its own (cron/2001).

### cron 1001

`Run of job {job}`

- **Level:** info · **Channel:** operational · **Task:** Job run · **Log:** cron
- **Fields:** `job`, `id`, `kind`, `exec`
- **Find it:** `provider:cron id:1001`

**What it means.** A scheduled job ran: start, then stop with its duration, or fail with its error.

**Why it happens.** Its schedule came due, catch-up fired it, or someone ran it now (cron_run, /cron run, the dashboard).

**What to do.** On a failure, read the error; for a prompt job, open its turn (the trace) to see what it did.

### cron 1002

`Job {job} missed its run at {due}; it runs once now`

- **Level:** info · **Channel:** operational · **Task:** Schedule · **Log:** cron
- **Fields:** `job`, `id`, `due`
- **Find it:** `provider:cron id:1002`

**What it means.** A job's run came due while Tenant was down or the job was disabled, and catch-up runs it once.

**Why it happens.** cron.catchup is on, and the job is a prompt job without exec.

**What to do.** None. Turn cron.catchup off to skip missed runs instead.

### cron 2001

`Job {job} can't be scheduled and stays disabled: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Schedule · **Log:** cron
- **Fields:** `job`, `id`, `err`
- **Find it:** `provider:cron id:2001`

**What it means.** A saved job has a schedule or timezone that no longer parses, so it is kept but never runs.

**Why it happens.** config.json was edited by hand, or the timezone database changed.

**What to do.** Fix the job's spec or tz (/cron, the dashboard), or remove it.

### cron 2002

`Job {job} missed its run at {due}: {reason}`

- **Level:** warning · **Channel:** operational · **Task:** Schedule · **Log:** cron
- **Fields:** `job`, `id`, `due`, `reason`
- **Find it:** `provider:cron id:2002`

**What it means.** A job's run came due while Tenant was down or the job was disabled, and it was skipped.

**Why it happens.** Catch-up is off (it is held off while the model is degraded at start-up), or the job is a shell or exec job, which never catch up so a destructive job can't fire unexpectedly.

**What to do.** Run it now if the missed run matters (/cron run). Turn cron.catchup on to catch up prompt jobs.

### cron 2003

`The run of job {job} was not saved to the run history: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Run history · **Log:** cron
- **Fields:** `job`, `id`, `err`
- **Find it:** `provider:cron id:2003`

**What it means.** A job ran, but its record did not reach cron-history.json.

**Why it happens.** The data directory is full, unwritable or locked.

**What to do.** Check the data directory's disk space and permissions. The run's events in this log still hold its outcome.

### cron 2004

`The run history could not be read, so missed runs aren't caught up: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Run history · **Log:** cron
- **Fields:** `err`
- **Find it:** `provider:cron id:2004`

**What it means.** cron-history.json could not be read at start-up, so no job knows when it last ran.

**Why it happens.** The file is unreadable or locked by another process.

**What to do.** Check the file's permissions in the data directory; the next run writes a new record.

### cron 5001

`Job {job} added: {spec}`

- **Level:** info · **Channel:** operational · **Task:** Job · **Log:** cron
- **Fields:** `job`, `id`, `spec`, `kind`, `exec`, `tz`, `enabled`
- **Find it:** `provider:cron id:5001`

**What it means.** A cron job was created.

**Why it happens.** The agent (cron_add), /cron add or the dashboard added it.

**What to do.** None.

### cron 5002

`Job {job} {state}`

- **Level:** info · **Channel:** operational · **Task:** Job · **Log:** cron
- **Fields:** `job`, `id`, `state`
- **Find it:** `provider:cron id:5002`

**What it means.** A cron job was enabled or disabled.

**Why it happens.** The agent, /cron or the dashboard changed it.

**What to do.** None.

### cron 5003

`Job {job} removed`

- **Level:** info · **Channel:** operational · **Task:** Job · **Log:** cron
- **Fields:** `job`, `id`
- **Find it:** `provider:cron id:5003`

**What it means.** A cron job was deleted.

**Why it happens.** The agent, /cron remove or the dashboard removed it.

**What to do.** None.

### cron 5004

`cron-exec turned {state}`

- **Level:** info · **Channel:** operational · **Task:** Job · **Log:** cron
- **Fields:** `state`
- **Find it:** `provider:cron id:5004`

**What it means.** The global cron-exec switch changed: while off, shell jobs and exec prompt jobs fail instead of running.

**Why it happens.** /cron exec or the dashboard flipped it.

**What to do.** None when intended.

### cron 9001

`Job {job} printed {bytes} bytes`

- **Level:** verbose · **Channel:** diagnostic · **Task:** Job run · **Log:** cron
- **Fields:** `job`, `bytes`, `output` (sensitive, masked: `output`)
- **Find it:** `provider:cron id:9001`

**What it means.** The output of a shell job's command.

**Why it happens.** A shell job ran.

**What to do.** None. The output is masked in viewers; it can hold anything the command printed.

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

## peering

Writes the `peering` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#peering-1) | info | Listener | `Peer listener on {addr}` |
| [1101](#peering-1101) | info | Pairing | `Paired with {name} at {remote}, filed as {peer}` |
| [1102](#peering-1102) | info | Pairing | `Pairing request from {name} at {remote} was denied` |
| [1103](#peering-1103) | info | Pairing | `Pairing request to {url}` |
| [1201](#peering-1201) | info | Heartbeat | `Peer {peer} is reachable ({latency})` |
| [1301](#peering-1301) | verbose | Connection | `Session from peer {peer}` |
| [1302](#peering-1302) | verbose | RPC | `Peer {peer} called {tool}` |
| [2001](#peering-2001) | warning | Authentication | `A request from {remote} was refused: its token matches no paired peer` |
| [2002](#peering-2002) | warning | Authentication | `peers.json could not be reloaded; peers are checked against the copy in memory: {err}` |
| [2003](#peering-2003) | warning | Authentication | `Peer {peer} authenticated, but peers.json was not updated ({change}): {err}` |
| [2101](#peering-2101) | warning | Pairing | `Pairing request from {remote} refused: too many are pending` |
| [2201](#peering-2201) | warning | Heartbeat | `Peer {peer} is unreachable: {err}` |
| [2202](#peering-2202) | warning | Heartbeat | `Heartbeats stopped: peers.json can't be read: {err}` |
| [2301](#peering-2301) | warning | RPC | `Peer {peer}'s {tool} could not search {store}: {err}` |
| [3001](#peering-3001) | error | Listener | `Peer listener on {addr} did not start: {err}` |
| [3101](#peering-3101) | error | Pairing | `Pairing with {name} failed after approval: {err}` |

### peering 1

`Peer listener on {addr}`

- **Level:** info · **Channel:** operational · **Task:** Listener · **Log:** peering
- **Fields:** `addr`, `tls`
- **Find it:** `provider:peering id:1`

**What it means.** The federation listener that paired peers dial: start, then stop with its uptime, or fail with the error that stopped it.

**Why it happens.** peer.listen is set, or /peer serve started it; it stops at shutdown or when /peer serve moves it.

**What to do.** On a failure, read the error: usually the address was taken or the network went away. /peer serve restarts it.

### peering 1101

`Paired with {name} at {remote}, filed as {peer}`

- **Level:** info · **Channel:** operational · **Task:** Pairing · **Log:** peering
- **Fields:** `name`, `instance`, `remote`, `peer`
- **Find it:** `provider:peering id:1101`

**What it means.** The operator approved a pairing request, and the inviter is now a peer that may dial this instance (sharing nothing yet).

**Why it happens.** Another instance ran tenant peer invite --to (or /peer invite) at this one.

**What to do.** Choose what it may read: /configure peer, or tenant peer share.

### peering 1102

`Pairing request from {name} at {remote} was denied`

- **Level:** info · **Channel:** operational · **Task:** Pairing · **Log:** peering
- **Fields:** `name`, `instance`, `remote`
- **Find it:** `provider:peering id:1102`

**What it means.** A pairing request was refused, so nothing was stored.

**Why it happens.** The operator denied it, or nobody answered within 90 seconds.

**What to do.** None when intended. If it was expected, ask the inviter to try again and approve it when the PINs match.

### peering 1103

`Pairing request to {url}`

- **Level:** info · **Channel:** operational · **Task:** Pairing · **Log:** peering
- **Fields:** `url`
- **Find it:** `provider:peering id:1103`

**What it means.** This instance asked a peer to pair: start, then stop when the peer approved, or fail with why not.

**Why it happens.** The operator ran tenant peer invite --to or /peer invite.

**What to do.** On a failure, read the error: the peer was unreachable, its operator denied it, or its certificate did not match.

### peering 1201

`Peer {peer} is reachable ({latency})`

- **Level:** info · **Channel:** operational · **Task:** Heartbeat · **Log:** peering
- **Fields:** `peer`, `url`, `latency`
- **Find it:** `provider:peering id:1201`

**What it means.** A peer this instance dials answered its heartbeat, for the first time or after being unreachable.

**Why it happens.** The peer started, or its network came back.

**What to do.** None.

### peering 1301

`Session from peer {peer}`

- **Level:** verbose · **Channel:** operational · **Task:** Connection · **Log:** peering
- **Fields:** `peer`
- **Find it:** `provider:peering id:1301`

**What it means.** A paired peer opened a session with the listener: start, then stop when it closes.

**Why it happens.** The peer's heartbeat, a federated search or tenant peer query.

**What to do.** None.

### peering 1302

`Peer {peer} called {tool}`

- **Level:** verbose · **Channel:** operational · **Task:** RPC · **Log:** peering
- **Fields:** `peer`, `tool`
- **Find it:** `provider:peering id:1302`

**What it means.** A paired peer called a tool on this instance: start, then stop with its duration, or fail with its error.

**Why it happens.** The peer's heartbeat (peer_hello), or its agent searching this instance's shared wiki or memory.

**What to do.** On a denial, the peer asked for something this instance doesn't share: share it (/configure peer) or ask its operator. Otherwise, read the error.

### peering 2001

`A request from {remote} was refused: its token matches no paired peer`

- **Level:** warning · **Channel:** operational · **Task:** Authentication · **Log:** peering
- **Fields:** `remote`
- **Find it:** `provider:peering id:2001`

**What it means.** A request reached the listener with a bearer token that is not a current peer's.

**Why it happens.** A removed or revoked peer is still dialing, a peer kept its old token after a rotation, or someone is probing the port.

**What to do.** If the address is a peer you trust, re-pair it or hand it its new token; otherwise block the address.

### peering 2002

`peers.json could not be reloaded; peers are checked against the copy in memory: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Authentication · **Log:** peering
- **Fields:** `err`
- **Find it:** `provider:peering id:2002`

**What it means.** The listener could not re-read peers.json, so a revoke, rotation or new pairing made elsewhere hasn't landed.

**Why it happens.** The file is unreadable or malformed, or the disk failed.

**What to do.** Check peers.json in the config directory. Requests authenticate against the last good copy until it reads again.

### peering 2003

`Peer {peer} authenticated, but peers.json was not updated ({change}): {err}`

- **Level:** warning · **Channel:** operational · **Task:** Authentication · **Log:** peering
- **Fields:** `peer`, `change`, `err`
- **Find it:** `provider:peering id:2003`

**What it means.** A peer's first use of its invite, or of its rotated token, could not be saved.

**Why it happens.** peers.json could not be written.

**What to do.** None at once: the request still authenticates, and the next one retries. If it repeats, check the config directory's permissions.

### peering 2101

`Pairing request from {remote} refused: too many are pending`

- **Level:** warning · **Channel:** operational · **Task:** Pairing · **Log:** peering
- **Fields:** `remote`
- **Find it:** `provider:peering id:2101`

**What it means.** A pairing request was turned away before reaching the operator: each host may have one pending request, and three may be pending in all.

**Why it happens.** The same host asked again before its first request was answered, or many hosts are asking at once.

**What to do.** Answer the pending request. If the requests are unexpected, block the host: the pairing endpoint is open to anyone who can reach the listener.

### peering 2201

`Peer {peer} is unreachable: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Heartbeat · **Log:** peering
- **Fields:** `peer`, `url`, `err`
- **Find it:** `provider:peering id:2201`

**What it means.** A peer this instance dials failed its heartbeat, so its shared knowledge is out of reach.

**Why it happens.** The peer is off or asleep, its network or address changed, or it revoked this instance's token.

**What to do.** Check the peer is running and reachable at its URL. It is reported again when it answers.

### peering 2202

`Heartbeats stopped: peers.json can't be read: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Heartbeat · **Log:** peering
- **Fields:** `err`
- **Find it:** `provider:peering id:2202`

**What it means.** The heartbeat could not read the paired peers, so no peer's liveness is checked.

**Why it happens.** peers.json is unreadable or malformed.

**What to do.** Check peers.json in the config directory; heartbeats resume on the next round after it reads.

### peering 2301

`Peer {peer}'s {tool} could not search {store}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** RPC · **Log:** peering
- **Fields:** `peer`, `tool`, `store`, `err`
- **Find it:** `provider:peering id:2301`

**What it means.** A peer's search of this instance's shared knowledge failed here, so it got partial results or none.

**Why it happens.** The wiki index or a memory store failed: a locked or damaged database, or a disk error.

**What to do.** Check the memory log around this time. The peer's next search tries again.

### peering 3001

`Peer listener on {addr} did not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Listener · **Log:** peering
- **Fields:** `addr`, `err`
- **Find it:** `provider:peering id:3001`

**What it means.** The federation listener could not start, so no peer can reach this instance.

**Why it happens.** The address is in use, a non-loopback address has neither TLS nor peer.transport: overlay, or the peer cert or peers.json could not be read.

**What to do.** Fix the cause the error names, then /peer serve <addr> or restart.

### peering 3101

`Pairing with {name} failed after approval: {err}`

- **Level:** error · **Channel:** operational · **Task:** Pairing · **Log:** peering
- **Fields:** `name`, `remote`, `err`
- **Find it:** `provider:peering id:3101`

**What it means.** The operator approved a pairing, but it could not be completed.

**Why it happens.** A token could not be minted, or peers.json could not be written.

**What to do.** Check the config directory's disk space and permissions, then ask the inviter to try again.

## plugin.atlassian

Writes the `plugin.atlassian` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#pluginatlassian-1) | info | Lifecycle | `Jira started` |
| [2](#pluginatlassian-2) | info | Lifecycle | `{target} enabled` |
| [3](#pluginatlassian-3) | info | Lifecycle | `{target} disabled` |
| [1101](#pluginatlassian-1101) | info | Auth | `Jira OAuth token refreshed` |
| [2001](#pluginatlassian-2001) | warning | API | `Jira rate limit reached on {op}: HTTP {status}` |
| [2101](#pluginatlassian-2101) | warning | Auth | `Jira's rotated refresh token not saved: {err}` |
| [2102](#pluginatlassian-2102) | warning | Auth | `Open the sign-in URL to authorize Tenant's Jira access` |
| [3001](#pluginatlassian-3001) | error | Lifecycle | `Jira could not start: {err}` |
| [3002](#pluginatlassian-3002) | error | API | `Jira refused the credentials on {op}: HTTP {status}` |
| [3003](#pluginatlassian-3003) | error | API | `Jira API call {op} failed: {err}` |
| [3101](#pluginatlassian-3101) | error | Auth | `Jira OAuth token refresh failed: {err}` |

### plugin.atlassian 1

`Jira started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.atlassian
- **Find it:** `provider:plugin.atlassian id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.atlassian 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.atlassian
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.atlassian id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.atlassian 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.atlassian
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.atlassian id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.atlassian 1101

`Jira OAuth token refreshed`

- **Level:** info · **Channel:** operational · **Task:** Auth · **Log:** plugin.atlassian
- **Find it:** `provider:plugin.atlassian id:1101`

**What it means.** The OAuth access token expired and was renewed; the rotated refresh token was saved.

**Why it happens.** Atlassian access tokens last an hour.

**What to do.** None.

### plugin.atlassian 2001

`Jira rate limit reached on {op}: HTTP {status}`

- **Level:** warning · **Channel:** operational · **Task:** API · **Log:** plugin.atlassian
- **Fields:** `op`, `status`, `retry`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.atlassian id:2001`

**What it means.** The service refused a call because the account used up its request quota.

**Why it happens.** The agent made many calls in a short time, or something else shares the account's quota.

**What to do.** Wait until the quota resets (retry holds the service's Retry-After, when it sent one). If it keeps happening, use the tool less or raise the account's quota.

### plugin.atlassian 2101

`Jira's rotated refresh token not saved: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Auth · **Log:** plugin.atlassian
- **Fields:** `err`
- **Find it:** `provider:plugin.atlassian id:2101`

**What it means.** Atlassian rotated the refresh token, and the new one could not be written, so the call that refreshed it failed and the saved token may no longer work.

**Why it happens.** The token cache or its directory can't be written: permissions or a full disk.

**What to do.** Fix what the error names. If Jira calls then fail to refresh, sign in again with `tenant atlassian login`.

### plugin.atlassian 2102

`Open the sign-in URL to authorize Tenant's Jira access`

- **Level:** warning · **Channel:** operational · **Task:** Auth · **Log:** plugin.atlassian
- **Fields:** `url`
- **Find it:** `provider:plugin.atlassian id:2102`

**What it means.** Signing in to Jira needs the operator's browser, and it could not be opened for them.

**Why it happens.** No browser opener in this process (a service, a remote shell), or opening it failed.

**What to do.** Open the logged url in a browser on this machine and approve access; the sign-in completes by itself.

### plugin.atlassian 3001

`Jira could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.atlassian
- **Fields:** `err`
- **Find it:** `provider:plugin.atlassian id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.atlassian 3002

`Jira refused the credentials on {op}: HTTP {status}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.atlassian
- **Fields:** `op`, `status`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.atlassian id:3002`

**What it means.** The service refused a call as unauthorized, so the tool failed.

**Why it happens.** The token or key expired or was revoked, or it lacks the scope or access this call needs.

**What to do.** Check the plugin's credentials: /configure it again, or paste a new key on the dashboard's Keys page.

### plugin.atlassian 3003

`Jira API call {op} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.atlassian
- **Fields:** `op`, `status`, `err`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.atlassian id:3003`

**What it means.** A call to the service failed on its side or on the way, so the tool failed. status is 0 when no answer came.

**Why it happens.** The service had an outage or an error, or the network, DNS or a proxy failed.

**What to do.** Retry later. If it persists, check the service's status page and this machine's connection.

### plugin.atlassian 3101

`Jira OAuth token refresh failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** Auth · **Log:** plugin.atlassian
- **Fields:** `err`
- **Find it:** `provider:plugin.atlassian id:3101`

**What it means.** The access token could not be renewed, so Jira calls fail.

**Why it happens.** The refresh token was revoked, expired after 90 days unused, or the OAuth app's secret changed.

**What to do.** Sign in again with `tenant atlassian login`, then /enable atlassian.

## plugin.crm

Writes the `plugin.crm` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#plugincrm-1) | info | Lifecycle | `CRM started` |
| [2](#plugincrm-2) | info | Lifecycle | `{target} enabled` |
| [3](#plugincrm-3) | info | Lifecycle | `{target} disabled` |
| [3001](#plugincrm-3001) | error | Lifecycle | `CRM could not start: {err}` |
| [3101](#plugincrm-3101) | error | Tool | `crm-tool {subcommand} could not run: {err}` |

### plugin.crm 1

`CRM started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.crm
- **Find it:** `provider:plugin.crm id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.crm 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.crm
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.crm id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.crm 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.crm
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.crm id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.crm 3001

`CRM could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.crm
- **Fields:** `err`
- **Find it:** `provider:plugin.crm id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.crm 3101

`crm-tool {subcommand} could not run: {err}`

- **Level:** error · **Channel:** operational · **Task:** Tool · **Log:** plugin.crm
- **Fields:** `subcommand`, `err`
- **Find it:** `provider:plugin.crm id:3101`

**What it means.** The external crm-tool did not run to completion, so the CRM tool failed. A run that exits with an error is the tool's answer, and is not logged.

**Why it happens.** The binary is missing or not executable, or it ran past its timeout.

**What to do.** Check --crm-tool-path (or $CRM_TOOL_PATH), and run the tool by hand to see where it hangs.

## plugin.github

Writes the `plugin.github` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#plugingithub-1) | info | Lifecycle | `GitHub started` |
| [2](#plugingithub-2) | info | Lifecycle | `{target} enabled` |
| [3](#plugingithub-3) | info | Lifecycle | `{target} disabled` |
| [2001](#plugingithub-2001) | warning | API | `GitHub rate limit reached on {op}: HTTP {status}` |
| [3001](#plugingithub-3001) | error | Lifecycle | `GitHub could not start: {err}` |
| [3002](#plugingithub-3002) | error | API | `GitHub refused the credentials on {op}: HTTP {status}` |
| [3003](#plugingithub-3003) | error | API | `GitHub API call {op} failed: {err}` |

### plugin.github 1

`GitHub started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.github
- **Find it:** `provider:plugin.github id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.github 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.github
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.github id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.github 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.github
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.github id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.github 2001

`GitHub rate limit reached on {op}: HTTP {status}`

- **Level:** warning · **Channel:** operational · **Task:** API · **Log:** plugin.github
- **Fields:** `op`, `status`, `retry`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.github id:2001`

**What it means.** The service refused a call because the account used up its request quota.

**Why it happens.** The agent made many calls in a short time, or something else shares the account's quota.

**What to do.** Wait until the quota resets (retry holds the service's Retry-After, when it sent one). If it keeps happening, use the tool less or raise the account's quota.

### plugin.github 3001

`GitHub could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.github
- **Fields:** `err`
- **Find it:** `provider:plugin.github id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.github 3002

`GitHub refused the credentials on {op}: HTTP {status}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.github
- **Fields:** `op`, `status`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.github id:3002`

**What it means.** The service refused a call as unauthorized, so the tool failed.

**Why it happens.** The token or key expired or was revoked, or it lacks the scope or access this call needs.

**What to do.** Check the plugin's credentials: /configure it again, or paste a new key on the dashboard's Keys page.

### plugin.github 3003

`GitHub API call {op} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.github
- **Fields:** `op`, `status`, `err`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.github id:3003`

**What it means.** A call to the service failed on its side or on the way, so the tool failed. status is 0 when no answer came.

**Why it happens.** The service had an outage or an error, or the network, DNS or a proxy failed.

**What to do.** Retry later. If it persists, check the service's status page and this machine's connection.

## plugin.gsuite

Writes the `plugin.gsuite` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#plugingsuite-1) | info | Lifecycle | `Google Workspace started` |
| [2](#plugingsuite-2) | info | Lifecycle | `{target} enabled` |
| [3](#plugingsuite-3) | info | Lifecycle | `{target} disabled` |
| [1101](#plugingsuite-1101) | info | Auth | `Google access token refreshed; valid until {expires}` |
| [2001](#plugingsuite-2001) | warning | API | `Google Workspace rate limit reached on {op}: HTTP {status}` |
| [2101](#plugingsuite-2101) | warning | Auth | `Rotated Google OAuth token not saved; a restart may need a new sign-in: {err}` |
| [2102](#plugingsuite-2102) | warning | Auth | `Could not open a browser; open the sign-in URL to authorize Google: {err}` |
| [3001](#plugingsuite-3001) | error | Lifecycle | `Google Workspace could not start: {err}` |
| [3002](#plugingsuite-3002) | error | API | `Google Workspace refused the credentials on {op}: HTTP {status}` |
| [3003](#plugingsuite-3003) | error | API | `Google Workspace API call {op} failed: {err}` |
| [3101](#plugingsuite-3101) | error | Auth | `Google access token refresh failed: {err}` |

### plugin.gsuite 1

`Google Workspace started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.gsuite
- **Find it:** `provider:plugin.gsuite id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.gsuite 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.gsuite
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.gsuite id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.gsuite 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.gsuite
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.gsuite id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.gsuite 1101

`Google access token refreshed; valid until {expires}`

- **Level:** info · **Channel:** operational · **Task:** Auth · **Log:** plugin.gsuite
- **Fields:** `expires`
- **Find it:** `provider:plugin.gsuite id:1101`

**What it means.** A new access token was minted for the Gmail, Calendar and Drive calls.

**Why it happens.** The previous one was about to expire, or this is the first call since the plugin started.

**What to do.** None.

### plugin.gsuite 2001

`Google Workspace rate limit reached on {op}: HTTP {status}`

- **Level:** warning · **Channel:** operational · **Task:** API · **Log:** plugin.gsuite
- **Fields:** `op`, `status`, `retry`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.gsuite id:2001`

**What it means.** The service refused a call because the account used up its request quota.

**Why it happens.** The agent made many calls in a short time, or something else shares the account's quota.

**What to do.** Wait until the quota resets (retry holds the service's Retry-After, when it sent one). If it keeps happening, use the tool less or raise the account's quota.

### plugin.gsuite 2101

`Rotated Google OAuth token not saved; a restart may need a new sign-in: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Auth · **Log:** plugin.gsuite
- **Fields:** `err`
- **Find it:** `provider:plugin.gsuite id:2101`

**What it means.** Google rotated the refresh token, and the new one could not be written to the credentials. The plugin keeps working until Tenant restarts.

**Why it happens.** The credentials file or its directory can't be written: permissions, a full disk, or a lock another process holds.

**What to do.** Fix what the error names; the save is retried on the next refresh. After a restart that finds the old token refused, run /configure gsuite again.

### plugin.gsuite 2102

`Could not open a browser; open the sign-in URL to authorize Google: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Auth · **Log:** plugin.gsuite
- **Fields:** `url`, `err`
- **Find it:** `provider:plugin.gsuite id:2102`

**What it means.** Signing in to Google needs the operator's browser, and it could not be opened for them.

**Why it happens.** No browser opener in this process (a service, a remote shell), or opening it failed.

**What to do.** Open the logged url in a browser on this machine and approve access; the sign-in completes by itself.

### plugin.gsuite 3001

`Google Workspace could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.gsuite
- **Fields:** `err`
- **Find it:** `provider:plugin.gsuite id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.gsuite 3002

`Google Workspace refused the credentials on {op}: HTTP {status}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.gsuite
- **Fields:** `op`, `status`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.gsuite id:3002`

**What it means.** The service refused a call as unauthorized, so the tool failed.

**Why it happens.** The token or key expired or was revoked, or it lacks the scope or access this call needs.

**What to do.** Check the plugin's credentials: /configure it again, or paste a new key on the dashboard's Keys page.

### plugin.gsuite 3003

`Google Workspace API call {op} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.gsuite
- **Fields:** `op`, `status`, `err`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.gsuite id:3003`

**What it means.** A call to the service failed on its side or on the way, so the tool failed. status is 0 when no answer came.

**Why it happens.** The service had an outage or an error, or the network, DNS or a proxy failed.

**What to do.** Retry later. If it persists, check the service's status page and this machine's connection.

### plugin.gsuite 3101

`Google access token refresh failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** Auth · **Log:** plugin.gsuite
- **Fields:** `err`
- **Find it:** `provider:plugin.gsuite id:3101`

**What it means.** No access token could be minted, so every Gmail, Calendar and Drive call fails.

**Why it happens.** The OAuth refresh token was revoked or expired, the service account's key or delegation changed, or gcloud is signed out.

**What to do.** Sign in again with /configure gsuite (for gcloud mode, `gcloud auth login`); `tenant doctor` checks the setup.

## plugin.os

Writes the `plugin.os` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#pluginos-1) | info | Lifecycle | `OS started` |
| [2](#pluginos-2) | info | Lifecycle | `{target} enabled` |
| [3](#pluginos-3) | info | Lifecycle | `{target} disabled` |
| [3001](#pluginos-3001) | error | Lifecycle | `OS could not start: {err}` |

### plugin.os 1

`OS started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.os
- **Find it:** `provider:plugin.os id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.os 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.os
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.os id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.os 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.os
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.os id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.os 3001

`OS could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.os
- **Fields:** `err`
- **Find it:** `provider:plugin.os id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

## plugin.simplefin

Writes the `plugin.simplefin` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#pluginsimplefin-1) | info | Lifecycle | `SimpleFIN started` |
| [2](#pluginsimplefin-2) | info | Lifecycle | `{target} enabled` |
| [3](#pluginsimplefin-3) | info | Lifecycle | `{target} disabled` |
| [2001](#pluginsimplefin-2001) | warning | API | `SimpleFIN rate limit reached on {op}: HTTP {status}` |
| [3001](#pluginsimplefin-3001) | error | Lifecycle | `SimpleFIN could not start: {err}` |
| [3002](#pluginsimplefin-3002) | error | API | `SimpleFIN refused the credentials on {op}: HTTP {status}` |
| [3003](#pluginsimplefin-3003) | error | API | `SimpleFIN API call {op} failed: {err}` |

### plugin.simplefin 1

`SimpleFIN started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.simplefin
- **Find it:** `provider:plugin.simplefin id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.simplefin 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.simplefin
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.simplefin id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.simplefin 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.simplefin
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.simplefin id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.simplefin 2001

`SimpleFIN rate limit reached on {op}: HTTP {status}`

- **Level:** warning · **Channel:** operational · **Task:** API · **Log:** plugin.simplefin
- **Fields:** `op`, `status`, `retry`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.simplefin id:2001`

**What it means.** The service refused a call because the account used up its request quota.

**Why it happens.** The agent made many calls in a short time, or something else shares the account's quota.

**What to do.** Wait until the quota resets (retry holds the service's Retry-After, when it sent one). If it keeps happening, use the tool less or raise the account's quota.

### plugin.simplefin 3001

`SimpleFIN could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.simplefin
- **Fields:** `err`
- **Find it:** `provider:plugin.simplefin id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.simplefin 3002

`SimpleFIN refused the credentials on {op}: HTTP {status}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.simplefin
- **Fields:** `op`, `status`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.simplefin id:3002`

**What it means.** The service refused a call as unauthorized, so the tool failed.

**Why it happens.** The token or key expired or was revoked, or it lacks the scope or access this call needs.

**What to do.** Check the plugin's credentials: /configure it again, or paste a new key on the dashboard's Keys page.

### plugin.simplefin 3003

`SimpleFIN API call {op} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.simplefin
- **Fields:** `op`, `status`, `err`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.simplefin id:3003`

**What it means.** A call to the service failed on its side or on the way, so the tool failed. status is 0 when no answer came.

**Why it happens.** The service had an outage or an error, or the network, DNS or a proxy failed.

**What to do.** Retry later. If it persists, check the service's status page and this machine's connection.

## plugin.sql

Writes the `plugin.sql` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#pluginsql-1) | info | Lifecycle | `SQL started` |
| [2](#pluginsql-2) | info | Lifecycle | `{target} enabled` |
| [3](#pluginsql-3) | info | Lifecycle | `{target} disabled` |
| [3001](#pluginsql-3001) | error | Lifecycle | `SQL could not start: {err}` |
| [3101](#pluginsql-3101) | error | Database | `SQL database schema unreadable: {err}` |

### plugin.sql 1

`SQL started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.sql
- **Find it:** `provider:plugin.sql id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.sql 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.sql
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.sql id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.sql 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.sql
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.sql id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.sql 3001

`SQL could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.sql
- **Fields:** `err`
- **Find it:** `provider:plugin.sql id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.sql 3101

`SQL database schema unreadable: {err}`

- **Level:** error · **Channel:** operational · **Task:** Database · **Log:** plugin.sql
- **Fields:** `err`
- **Find it:** `provider:plugin.sql id:3101`

**What it means.** The database's schema could not be read, so the agent can't see its tables.

**Why it happens.** The file was moved, deleted, locked by another program, or is corrupt.

**What to do.** Check the database file named by --sql-db or /configure sql; `sqlite3 <file> 'PRAGMA integrity_check'` tests it.

## plugin.web

Writes the `plugin.web` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#pluginweb-1) | info | Lifecycle | `Web started` |
| [2](#pluginweb-2) | info | Lifecycle | `{target} enabled` |
| [3](#pluginweb-3) | info | Lifecycle | `{target} disabled` |
| [2001](#pluginweb-2001) | warning | API | `Web rate limit reached on {op}: HTTP {status}` |
| [3001](#pluginweb-3001) | error | Lifecycle | `Web could not start: {err}` |
| [3002](#pluginweb-3002) | error | API | `Web refused the credentials on {op}: HTTP {status}` |
| [3003](#pluginweb-3003) | error | API | `Web API call {op} failed: {err}` |

### plugin.web 1

`Web started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.web
- **Find it:** `provider:plugin.web id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.web 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.web
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.web id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.web 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.web
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.web id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.web 2001

`Web rate limit reached on {op}: HTTP {status}`

- **Level:** warning · **Channel:** operational · **Task:** API · **Log:** plugin.web
- **Fields:** `op`, `status`, `retry`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.web id:2001`

**What it means.** The service refused a call because the account used up its request quota.

**Why it happens.** The agent made many calls in a short time, or something else shares the account's quota.

**What to do.** Wait until the quota resets (retry holds the service's Retry-After, when it sent one). If it keeps happening, use the tool less or raise the account's quota.

### plugin.web 3001

`Web could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.web
- **Fields:** `err`
- **Find it:** `provider:plugin.web id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.web 3002

`Web refused the credentials on {op}: HTTP {status}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.web
- **Fields:** `op`, `status`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.web id:3002`

**What it means.** The service refused a call as unauthorized, so the tool failed.

**Why it happens.** The token or key expired or was revoked, or it lacks the scope or access this call needs.

**What to do.** Check the plugin's credentials: /configure it again, or paste a new key on the dashboard's Keys page.

### plugin.web 3003

`Web API call {op} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.web
- **Fields:** `op`, `status`, `err`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.web id:3003`

**What it means.** A call to the service failed on its side or on the way, so the tool failed. status is 0 when no answer came.

**Why it happens.** The service had an outage or an error, or the network, DNS or a proxy failed.

**What to do.** Retry later. If it persists, check the service's status page and this machine's connection.

## plugin.wiki

Writes the `plugin.wiki` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#pluginwiki-1) | info | Lifecycle | `Wiki started` |
| [2](#pluginwiki-2) | info | Lifecycle | `{target} enabled` |
| [3](#pluginwiki-3) | info | Lifecycle | `{target} disabled` |
| [2101](#pluginwiki-2101) | warning | Index | `Wiki embedder unreachable at start; using the declared dimension {declared_dim}: {err}` |
| [2102](#pluginwiki-2102) | warning | Index | `Wiki initial reindex failed; it reindexes on the first search: {err}` |
| [3001](#pluginwiki-3001) | error | Lifecycle | `Wiki could not start: {err}` |

### plugin.wiki 1

`Wiki started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.wiki
- **Find it:** `provider:plugin.wiki id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.wiki 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.wiki
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.wiki id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.wiki 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.wiki
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.wiki id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.wiki 2101

`Wiki embedder unreachable at start; using the declared dimension {declared_dim}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Index · **Log:** plugin.wiki
- **Fields:** `declared_dim`, `err`
- **Find it:** `provider:plugin.wiki id:2101`

**What it means.** The embedding model did not answer when the wiki started, so the index uses the dimension its profile declares and reindexes on the first search.

**Why it happens.** The embedder's server is down, still loading, or unreachable.

**What to do.** Check the embedder role's backend (`tenant doctor`). The wiki heals on its own once it answers.

### plugin.wiki 2102

`Wiki initial reindex failed; it reindexes on the first search: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Index · **Log:** plugin.wiki
- **Fields:** `err`
- **Find it:** `provider:plugin.wiki id:2102`

**What it means.** The notes changed since the index was saved, and they could not be embedded at start, so searches may miss them until the first search reindexes.

**Why it happens.** The embedder is down, or a note could not be read.

**What to do.** Check the embedder role's backend; or run `tenant wiki reindex` once it answers.

### plugin.wiki 3001

`Wiki could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.wiki
- **Fields:** `err`
- **Find it:** `provider:plugin.wiki id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

## plugin.x

Writes the `plugin.x` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#pluginx-1) | info | Lifecycle | `X started` |
| [2](#pluginx-2) | info | Lifecycle | `{target} enabled` |
| [3](#pluginx-3) | info | Lifecycle | `{target} disabled` |
| [1101](#pluginx-1101) | info | Auth | `X user token refreshed` |
| [2001](#pluginx-2001) | warning | API | `X rate limit reached on {op}: HTTP {status}` |
| [3001](#pluginx-3001) | error | Lifecycle | `X could not start: {err}` |
| [3002](#pluginx-3002) | error | API | `X refused the credentials on {op}: HTTP {status}` |
| [3003](#pluginx-3003) | error | API | `X API call {op} failed: {err}` |
| [3101](#pluginx-3101) | error | Auth | `X user token refresh failed: {err}` |

### plugin.x 1

`X started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.x
- **Find it:** `provider:plugin.x id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.x 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.x
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.x id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.x 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.x
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.x id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.x 1101

`X user token refreshed`

- **Level:** info · **Channel:** operational · **Task:** Auth · **Log:** plugin.x
- **Find it:** `provider:plugin.x id:1101`

**What it means.** The user-context access token expired and was renewed with the refresh token, which X rotates; both were saved.

**Why it happens.** X access tokens last about two hours.

**What to do.** None.

### plugin.x 2001

`X rate limit reached on {op}: HTTP {status}`

- **Level:** warning · **Channel:** operational · **Task:** API · **Log:** plugin.x
- **Fields:** `op`, `status`, `retry`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.x id:2001`

**What it means.** The service refused a call because the account used up its request quota.

**Why it happens.** The agent made many calls in a short time, or something else shares the account's quota.

**What to do.** Wait until the quota resets (retry holds the service's Retry-After, when it sent one). If it keeps happening, use the tool less or raise the account's quota.

### plugin.x 3001

`X could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.x
- **Fields:** `err`
- **Find it:** `provider:plugin.x id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.x 3002

`X refused the credentials on {op}: HTTP {status}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.x
- **Fields:** `op`, `status`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.x id:3002`

**What it means.** The service refused a call as unauthorized, so the tool failed.

**Why it happens.** The token or key expired or was revoked, or it lacks the scope or access this call needs.

**What to do.** Check the plugin's credentials: /configure it again, or paste a new key on the dashboard's Keys page.

### plugin.x 3003

`X API call {op} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.x
- **Fields:** `op`, `status`, `err`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.x id:3003`

**What it means.** A call to the service failed on its side or on the way, so the tool failed. status is 0 when no answer came.

**Why it happens.** The service had an outage or an error, or the network, DNS or a proxy failed.

**What to do.** Retry later. If it persists, check the service's status page and this machine's connection.

### plugin.x 3101

`X user token refresh failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** Auth · **Log:** plugin.x
- **Fields:** `err`
- **Find it:** `provider:plugin.x id:3101`

**What it means.** The user-context token could not be renewed or saved, so posting and user reads fail.

**Why it happens.** The refresh token was revoked or already used, the app's client id changed, X is unreachable, or the token file can't be written.

**What to do.** Sign in again with `tenant x --login`.

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

## toolmux

Writes the `toolmux` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1001](#toolmux-1001) | info | Connectors | `MCP connector {server} re-authorized` |
| [2001](#toolmux-2001) | warning | Connectors | `Local MCP server {connector} did not start: {err}` |
| [2002](#toolmux-2002) | warning | Connectors | `Remote MCP connector {server} did not reconnect: {err}` |
| [2003](#toolmux-2003) | warning | Connectors | `MCP connector {server} needs re-authorization: {err}` |
| [2004](#toolmux-2004) | warning | Peers | `peers.json unreadable; no peer is reconnected: {err}` |
| [2005](#toolmux-2005) | warning | Peers | `Peer {peer} did not reconnect: {err}` |
| [2006](#toolmux-2006) | warning | Stats | `Tool stats not saved to {path}; retrying every flush: {err}` |

### toolmux 1001

`MCP connector {server} re-authorized`

- **Level:** info · **Channel:** operational · **Task:** Connectors · **Log:** toolmux
- **Fields:** `server`
- **Find it:** `provider:toolmux id:1001`

**What it means.** A connector that had lost its sign-in has a working one again, and its tools run.

**Why it happens.** The operator reconnected it, or its token refreshed after all.

**What to do.** None.

### toolmux 2001

`Local MCP server {connector} did not start: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Connectors · **Log:** toolmux
- **Fields:** `connector`, `err`
- **Find it:** `provider:toolmux id:2001`

**What it means.** A local MCP server registered for this machine failed to start at launch, so its tools stay a stub.

**Why it happens.** Its command is missing or fails, or the sandbox refused its profile.

**What to do.** Fix what the error names, then /enable the connector to start it again.

### toolmux 2002

`Remote MCP connector {server} did not reconnect: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Connectors · **Log:** toolmux
- **Fields:** `server`, `err`
- **Find it:** `provider:toolmux id:2002`

**What it means.** A remote connector could not come back at launch from its cached sign-in, so its tools stay a stub.

**Why it happens.** The server is down or unreachable, or refused the cached token.

**What to do.** /enable the connector to sign in again, or check the server's status.

### toolmux 2003

`MCP connector {server} needs re-authorization: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Connectors · **Log:** toolmux
- **Fields:** `server`, `url`, `err`, `fix`
- **Find it:** `provider:toolmux id:2003`

**What it means.** A connector's sign-in expired and could not be refreshed, so its tools fail until it is reconnected.

**Why it happens.** The server revoked the refresh token, or it expired unused.

**What to do.** Run the command in fix (`tenant mcp connect <url>`) at the host, or use the dashboard's Remote services page.

### toolmux 2004

`peers.json unreadable; no peer is reconnected: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Peers · **Log:** toolmux
- **Fields:** `err`
- **Find it:** `provider:toolmux id:2004`

**What it means.** The paired peers could not be read at launch, so federated search runs on this machine alone.

**Why it happens.** peers.json is corrupt, or can't be read.

**What to do.** Fix or restore peers.json in the config directory (`tenant peer list` reads it), then restart.

### toolmux 2005

`Peer {peer} did not reconnect: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Peers · **Log:** toolmux
- **Fields:** `peer`, `err`
- **Find it:** `provider:toolmux id:2005`

**What it means.** A paired peer could not be reached at launch, so searches leave it out.

**Why it happens.** The peer is offline, its address changed, or it revoked this machine's token.

**What to do.** Check the peer is up (`tenant peer show <name>`); pair again if its token was revoked.

### toolmux 2006

`Tool stats not saved to {path}; retrying every flush: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Stats · **Log:** toolmux
- **Fields:** `path`, `err`
- **Find it:** `provider:toolmux id:2006`

**What it means.** The per-tool usage counts could not be written. They are kept in memory and retried, and logged again only after a success.

**Why it happens.** The data directory can't be written: permissions or a full disk.

**What to do.** Fix what the error names; nothing else is affected.
