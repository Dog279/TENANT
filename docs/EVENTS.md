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
