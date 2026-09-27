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
