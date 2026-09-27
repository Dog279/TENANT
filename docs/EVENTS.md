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

## backup

Writes the `backup` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1001](#backup-1001) | info | Backup | `Back up to {path}` |
| [1002](#backup-1002) | info | Backup | `Backup written to {path}: {files} files, {bytes} bytes` |
| [1003](#backup-1003) | info | Verify | `Verify backup {path}` |
| [1004](#backup-1004) | info | Verify | `Backup {path} checks out: {files} files, made {created} by Tenant {version}` |
| [1005](#backup-1005) | info | Restore | `Restore backup {path}` |
| [1006](#backup-1006) | info | Restore | `Restored backup {path}` |
| [2001](#backup-2001) | warning | Backup | `Backup {path}: {warning}` |
| [3001](#backup-3001) | error | Verify | `Backup {path} failed verification with {count} problems, the first: {problem}` |

### backup 1001

`Back up to {path}`

- **Level:** info · **Channel:** operational · **Task:** Backup · **Log:** backup
- **Fields:** `path`
- **Find it:** `provider:backup id:1001`

**What it means.** A backup of the config and data directories: its start, then its stop with the duration, or its failure with the error.

**Why it happens.** The operator ran tenant backup create.

**What to do.** None when it stops. On a failure, read the error; nothing is left at the path.

### backup 1002

`Backup written to {path}: {files} files, {bytes} bytes`

- **Level:** info · **Channel:** operational · **Task:** Backup · **Log:** backup
- **Fields:** `path`, `bytes`, `files`, `databases`, `logs`, `secrets`, `skipped`, `encrypted`, `version`
- **Find it:** `provider:backup id:1002`

**What it means.** A backup file was written. skipped counts what was left out, such as credentials in an unencrypted backup.

**Why it happens.** A backup finished.

**What to do.** Check it now and then with tenant backup verify, and keep it somewhere other than this machine.

### backup 1003

`Verify backup {path}`

- **Level:** info · **Channel:** operational · **Task:** Verify · **Log:** backup
- **Fields:** `path`
- **Find it:** `provider:backup id:1003`

**What it means.** A backup file was checked: its start, then its stop with the duration, or its failure with the error. Its findings follow as their own event.

**Why it happens.** The operator ran tenant backup verify.

**What to do.** None when it stops. A failure means the file could not be read at all: a wrong path, or a missing or wrong passphrase or key.

### backup 1004

`Backup {path} checks out: {files} files, made {created} by Tenant {version}`

- **Level:** info · **Channel:** operational · **Task:** Verify · **Log:** backup
- **Fields:** `path`, `files`, `created`, `version`, `encrypted`, `newer`, `notes`
- **Find it:** `provider:backup id:1004`

**What it means.** Every checksum in the backup matches, every database passes its integrity check and every log parses. newer counts schema from a newer Tenant, which a restore refuses unless forced.

**Why it happens.** A verify or a restore read the backup.

**What to do.** None.

### backup 1005

`Restore backup {path}`

- **Level:** info · **Channel:** operational · **Task:** Restore · **Log:** backup
- **Fields:** `path`, `dry_run`
- **Find it:** `provider:backup id:1005`

**What it means.** A restore of a backup over the config and data directories: its start, then its stop with the duration, or its failure with the error. A dry run only verifies.

**Why it happens.** The operator ran tenant backup restore.

**What to do.** None when it stops. On a failure nothing was changed; read the error.

### backup 1006

`Restored backup {path}`

- **Level:** info · **Channel:** operational · **Task:** Restore · **Log:** backup
- **Fields:** `path`, `aside`, `kept_secrets`
- **Find it:** `provider:backup id:1006`

**What it means.** The backup replaced the config and data directories. aside names where the previous ones are kept.

**Why it happens.** A restore finished.

**What to do.** Start Tenant and check it; delete the directories named in aside once you are happy. Don't run this install alongside the machine the backup came from.

### backup 2001

`Backup {path}: {warning}`

- **Level:** warning · **Channel:** operational · **Task:** Backup · **Log:** backup
- **Fields:** `path`, `warning`
- **Find it:** `provider:backup id:2001`

**What it means.** A backup was written, but something about it needs attention.

**Why it happens.** Usually the destination cannot restrict access to the file (a FAT or exFAT drive), and the backup is not encrypted.

**What to do.** Keep the file somewhere private, or back up again with --encrypt.

### backup 3001

`Backup {path} failed verification with {count} problems, the first: {problem}`

- **Level:** error · **Channel:** operational · **Task:** Verify · **Log:** backup
- **Fields:** `path`, `count`, `problem`, `problems`
- **Find it:** `provider:backup id:3001`

**What it means.** The backup is damaged: a checksum does not match, the file is cut short, a database fails its integrity check or a log does not parse. It cannot be restored.

**Why it happens.** The file was changed or cut short after it was written, or the disk or the copy damaged it.

**What to do.** Use another backup, and make a fresh one with tenant backup create.

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

## dashboard

Writes the `dashboard` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#dashboard-1) | info | Server | `Dashboard on {addr}` |
| [1001](#dashboard-1001) | info | Stream | `Live {stream} stream to {remote}` |
| [2001](#dashboard-2001) | warning | Server | `HTTP server: {err}` |
| [2002](#dashboard-2002) | warning | Stream | `Live {stream} stream to {remote} fell behind` |
| [2003](#dashboard-2003) | warning | Stream | `WebSocket handshake from {remote} failed: {err}` |
| [2101](#dashboard-2101) | warning | Access | `Request for unrecognized host {host} rejected: {method} {route}` |
| [2102](#dashboard-2102) | warning | Access | `Cross-origin request from {origin} rejected: {method} {route}` |
| [2103](#dashboard-2103) | warning | Access | `Request without a valid token rejected: {method} {route}` |
| [2104](#dashboard-2104) | warning | Access | `Browser change without a matching Origin rejected: {method} {route}` |
| [2105](#dashboard-2105) | warning | Access | `Sign-in with a wrong token from {remote}` |
| [2201](#dashboard-2201) | warning | Action | `Dashboard action {action} failed: {err}` |
| [2202](#dashboard-2202) | warning | Action | `API key {cred} was not {op}` |
| [2203](#dashboard-2203) | warning | Action | `API key {cred} was sent in the URL and refused` |
| [2204](#dashboard-2204) | warning | Action | `Integration {integration} test failed: {err}` |
| [2205](#dashboard-2205) | warning | Action | `MCP connector {url} was not added: {err}` |
| [2206](#dashboard-2206) | warning | Chat | `Chat turn failed: {err}` |
| [3001](#dashboard-3001) | error | Page | `Page {page} could not be rendered: {err}` |
| [9001](#dashboard-9001) | info | Request | `{method} {route}` |

### dashboard 1

`Dashboard on {addr}`

- **Level:** info · **Channel:** operational · **Task:** Server · **Log:** dashboard
- **Fields:** `addr`, `tls`
- **Find it:** `provider:dashboard id:1`

**What it means.** The dashboard's web server: start, then stop with its uptime, or fail with the error that stopped it.

**Why it happens.** dashboard.enabled is on, or /dashboard on started it; it stops at shutdown or on /dashboard off.

**What to do.** On a failure, read the error: usually the address is taken or the TLS certificate can't be read. /dashboard on starts it again.

### dashboard 1001

`Live {stream} stream to {remote}`

- **Level:** info · **Channel:** operational · **Task:** Stream · **Log:** dashboard
- **Fields:** `stream`, `remote`
- **Find it:** `provider:dashboard id:1001`

**What it means.** A browser page subscribed to a live feed (chat and activity events, or the log book): start when it connects, stop when it leaves.

**Why it happens.** A dashboard page with a live feed was opened, reloaded or closed.

**What to do.** None.

### dashboard 2001

`HTTP server: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Server · **Log:** dashboard
- **Fields:** `err`, `stack`
- **Find it:** `provider:dashboard id:2001`

**What it means.** Go's HTTP server reported a problem it handled on its own, such as a failed TLS handshake or a handler that panicked.

**Why it happens.** A client that doesn't speak TLS (or doesn't trust the certificate), a scanner, a dropped connection, or a bug in a page's handler.

**What to do.** A handshake error from an unknown address is usually noise. A panic is a bug: report it with its stack.

### dashboard 2002

`Live {stream} stream to {remote} fell behind`

- **Level:** warning · **Channel:** operational · **Task:** Stream · **Log:** dashboard
- **Fields:** `stream`, `remote`, `missed`
- **Find it:** `provider:dashboard id:2002`

**What it means.** A page read its live feed more slowly than events arrived, and its buffer filled, so events were dropped for it (missed counts them where known): the page no longer shows all of them.

**Why it happens.** A slow or suspended browser tab, a slow network, or a burst of events (a long tool run streams many).

**What to do.** Reload the page to redraw it. The events themselves are still in the log book.

### dashboard 2003

`WebSocket handshake from {remote} failed: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Stream · **Log:** dashboard
- **Fields:** `remote`, `err`
- **Find it:** `provider:dashboard id:2003`

**What it means.** A client asked for the chat WebSocket, but its handshake was malformed; it got an HTTP error instead.

**Why it happens.** A client or proxy that doesn't speak WebSocket, or a proxy that strips the Upgrade headers.

**What to do.** Use the dashboard in a browser directly, or make the proxy pass WebSocket upgrades through.

### dashboard 2101

`Request for unrecognized host {host} rejected: {method} {route}`

- **Level:** warning · **Channel:** operational · **Task:** Access · **Log:** dashboard
- **Fields:** `host`, `method`, `route`, `remote`
- **Find it:** `provider:dashboard id:2101`

**What it means.** A request named a Host the dashboard doesn't answer to, and got 403.

**Why it happens.** The dashboard was reached under a name or address it doesn't know (a reverse proxy, a LAN name, a new tailnet name), or a DNS-rebinding attempt.

**What to do.** If the name is yours, add it to dashboard.allowed_hosts. Otherwise nothing: the request was refused.

### dashboard 2102

`Cross-origin request from {origin} rejected: {method} {route}`

- **Level:** warning · **Channel:** operational · **Task:** Access · **Log:** dashboard
- **Fields:** `origin`, `method`, `route`, `remote`
- **Find it:** `provider:dashboard id:2102`

**What it means.** A browser request came from a page on another site, and got 403.

**Why it happens.** A web page on another origin tried to call the dashboard, or the dashboard is open under two different addresses.

**What to do.** Open the dashboard at one address. If you don't recognize the origin, a site you visited tried to reach it; it was refused.

### dashboard 2103

`Request without a valid token rejected: {method} {route}`

- **Level:** warning · **Channel:** operational · **Task:** Access · **Log:** dashboard
- **Fields:** `method`, `route`, `remote`, `credential`
- **Find it:** `provider:dashboard id:2103`

**What it means.** A request carried no dashboard token, or a wrong one (credential says which), and got 401.

**Why it happens.** A browser that hasn't signed in, a token that changed since sign-in, a script with an old token, or someone guessing.

**What to do.** Sign in at /settings with the token in <config>/dashboard-auth-token. Many wrong tokens from an address you don't know mean someone is guessing: keep the dashboard off public networks.

### dashboard 2104

`Browser change without a matching Origin rejected: {method} {route}`

- **Level:** warning · **Channel:** operational · **Task:** Access · **Log:** dashboard
- **Fields:** `method`, `route`, `remote`
- **Find it:** `provider:dashboard id:2104`

**What it means.** A change (a POST) authenticated by the sign-in cookie didn't prove it came from a dashboard page, and got 403.

**Why it happens.** A page on another site submitted a form to the dashboard, or an old browser or extension stripped the Origin header.

**What to do.** Make the change from the dashboard page itself, or call the API with an Authorization: Bearer token.

### dashboard 2105

`Sign-in with a wrong token from {remote}`

- **Level:** warning · **Channel:** operational · **Task:** Access · **Log:** dashboard
- **Fields:** `remote`
- **Find it:** `provider:dashboard id:2105`

**What it means.** The sign-in form was sent a token that doesn't match the dashboard's, and got 403.

**Why it happens.** A typo, a token from before it changed, or someone guessing.

**What to do.** Use the token in <config>/dashboard-auth-token (or dashboard.auth). Many failures from an address you don't know mean someone is guessing.

### dashboard 2201

`Dashboard action {action} failed: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Action · **Log:** dashboard
- **Fields:** `action`, `target`, `err`
- **Find it:** `provider:dashboard id:2201`

**What it means.** A change or command made on a dashboard page didn't apply. The page showed the operator the same error.

**Why it happens.** The value was refused (an unknown category or mode, a bad schedule, a job or fact that no longer exists), or the change couldn't be saved: config.json or a store is unwritable or locked, or the subsystem isn't running.

**What to do.** Read the error. Fix the value and try again; if saving failed, check the data directory's permissions and free space.

### dashboard 2202

`API key {cred} was not {op}`

- **Level:** warning · **Channel:** operational · **Task:** Action · **Log:** dashboard
- **Fields:** `cred`, `op`, `err` (sensitive, masked: `err`)
- **Find it:** `provider:dashboard id:2202`

**What it means.** Storing or removing an API key from the Keys page failed. The page said so without the reason, which can quote the key.

**Why it happens.** The credential store is unwritable or locked, or the key's ID is unknown.

**What to do.** Check the config directory's permissions, then try again. The reason is kept, masked, in this event.

### dashboard 2203

`API key {cred} was sent in the URL and refused`

- **Level:** warning · **Channel:** operational · **Task:** Action · **Log:** dashboard
- **Fields:** `cred`
- **Find it:** `provider:dashboard id:2203`

**What it means.** A request tried to store an API key through the URL's query string, where browser history and proxy logs keep it. It was refused.

**Why it happens.** A script or bookmark that puts value= in the URL instead of the form's body.

**What to do.** Rotate that key: it may now sit in a history or a log. Submit keys through the Keys page's form.

### dashboard 2204

`Integration {integration} test failed: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Action · **Log:** dashboard
- **Fields:** `integration`, `err`
- **Find it:** `provider:dashboard id:2204`

**What it means.** The Test button's check of an integration ran in the background and failed; the page shows the result on refresh.

**Why it happens.** The integration's credentials are missing or expired, its sign-in on the computer running Tenant wasn't finished, or its service is unreachable.

**What to do.** Finish any sign-in on the computer running Tenant, fix the integration's settings, and test again.

### dashboard 2205

`MCP connector {url} was not added: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Action · **Log:** dashboard
- **Fields:** `url`, `err`
- **Find it:** `provider:dashboard id:2205`

**What it means.** Connecting a remote MCP service from the MCP page ran in the background and failed.

**Why it happens.** The URL isn't an MCP endpoint, the service is unreachable, or its sign-in on the computer running Tenant wasn't approved.

**What to do.** Check the URL, approve the sign-in window on the computer running Tenant, and connect again. The mcp log has the details.

### dashboard 2206

`Chat turn failed: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Chat · **Log:** dashboard
- **Fields:** `err`
- **Find it:** `provider:dashboard id:2206`

**What it means.** A message sent from the Chat page didn't get an answer; the chat shows the error.

**Why it happens.** The turn gate refused it, the model is unreachable, or the agent failed before its loop started.

**What to do.** Read the error. For the model, see the model log; then send the message again.

### dashboard 3001

`Page {page} could not be rendered: {err}`

- **Level:** error · **Channel:** operational · **Task:** Page · **Log:** dashboard
- **Fields:** `page`, `err`
- **Find it:** `provider:dashboard id:3001`

**What it means.** A dashboard page's template failed, and the browser got a 500.

**Why it happens.** A bug: the page's data doesn't fit its template.

**What to do.** Report it with the error. Other pages keep working.

### dashboard 9001

`{method} {route}`

- **Level:** info · **Channel:** diagnostic · **Task:** Request · **Log:** dashboard
- **Fields:** `method`, `route`, `status`
- **Find it:** `provider:dashboard id:9001`

**What it means.** One request to the dashboard: start, then stop with its status and duration, or fail on a 5xx.

**Why it happens.** A browser page, the dashboard API or a live stream.

**What to do.** None. For slow or failing pages, compare durations and statuses by route.

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

## eval

Writes the `eval` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1001](#eval-1001) | info | Run | `Eval run of {subset}, {tasks} tasks` |
| [1002](#eval-1002) | info | Task | `Eval task {task}` |
| [1003](#eval-1003) | info | Trend | `Eval trend: {subset} scored {overall}, {passed}/{total} passed` |
| [2001](#eval-2001) | warning | Task | `Eval task {task} was not graded ({class}): {reason}` |
| [2002](#eval-2002) | warning | Regression | `No eval baseline at {path}: regressions are not checked` |
| [2003](#eval-2003) | warning | Regression | `Eval {subset} regressed against its baseline: Δ {delta}, 95% CI upper bound {ci_high}` |
| [2004](#eval-2004) | warning | Regression | `Eval baseline {path} could not be read: {err}` |
| [2005](#eval-2005) | warning | Run | `Eval {record} was not saved: {err}` |
| [2006](#eval-2006) | warning | Schedule | `Schedule {value} is malformed and ignored: {effect}` |
| [3001](#eval-3001) | error | Task | `The route classifier panicked in an eval task: {panic}` |

### eval 1001

`Eval run of {subset}, {tasks} tasks`

- **Level:** info · **Channel:** operational · **Task:** Run · **Log:** eval
- **Fields:** `subset`, `tasks`, `overall`, `passed`, `failed`, `ungraded`, `skipped`
- **Find it:** `provider:eval id:1001`

**What it means.** An eval run: start, then stop with its overall score and counts, or fail when it was cancelled.

**Why it happens.** The nightly schedule (improve.eval_every or eval_at) came due, someone ran it now (the dashboard, /eval), `tenant eval` ran, or the soul nudge scored a candidate.

**What to do.** None. Compare overall across runs in the trend (`tenant eval --trend`); a regression is reported on its own (eval/2003).

### eval 1002

`Eval task {task}`

- **Level:** info · **Channel:** operational · **Task:** Task · **Log:** eval
- **Fields:** `task`, `category`, `mode`, `result`, `score`
- **Find it:** `provider:eval id:1002`

**What it means.** One task of an eval run: start, then stop with its result (pass, fail, ungraded or skipped) and score.

**Why it happens.** Its run reached it.

**What to do.** For a failing task, open its report in <data>/eval-artifacts, or run `tenant eval --baseline-diff` for the movers.

### eval 1003

`Eval trend: {subset} scored {overall}, {passed}/{total} passed`

- **Level:** info · **Channel:** operational · **Task:** Trend · **Log:** eval
- **Fields:** `subset`, `overall`, `passed`, `total`, `regressed`, `delta`, `artifact`
- **Find it:** `provider:eval id:1003`

**What it means.** A finished run's line was added to the trend (trend.jsonl), with its verdict against the baseline.

**Why it happens.** A nightly run finished, or `tenant eval --append-trend` ran.

**What to do.** None. `tenant eval --trend` shows the series.

### eval 2001

`Eval task {task} was not graded ({class}): {reason}`

- **Level:** warning · **Channel:** operational · **Task:** Task · **Log:** eval
- **Fields:** `task`, `class`, `reason`
- **Find it:** `provider:eval id:2001`

**What it means.** A task was left out of the score because the harness, the model provider or the judge failed, not the agent.

**Why it happens.** class says which: harness (the task's agent couldn't be built), quota, endpoint, ratelimit or provider (the model), or judge (the grader was unusable after its retry).

**What to do.** Fix the named piece (a provider's quota or endpoint, the judge model) and run again. Many ungraded tasks make a run's score unreliable.

### eval 2002

`No eval baseline at {path}: regressions are not checked`

- **Level:** warning · **Channel:** operational · **Task:** Regression · **Log:** eval
- **Fields:** `path`
- **Find it:** `provider:eval id:2002`

**What it means.** A run finished with no baseline to compare against, so the regression gate is off.

**Why it happens.** No baseline was ever captured, or it lives somewhere the run doesn't look.

**What to do.** Capture one from a good run: `tenant eval --baseline-from <data>/eval-artifacts/eval-<time>.json`.

### eval 2003

`Eval {subset} regressed against its baseline: Δ {delta}, 95% CI upper bound {ci_high}`

- **Level:** warning · **Channel:** operational · **Task:** Regression · **Log:** eval
- **Fields:** `subset`, `delta`, `ci_high`, `overall`, `artifact`
- **Find it:** `provider:eval id:2003`

**What it means.** A run scored worse than its baseline, beyond chance (the paired bootstrap's 95% interval is below zero).

**Why it happens.** A change to the prompt, the model, routing, the tools or memory made the agent worse, or the environment lost something tasks need.

**What to do.** Run `tenant eval --baseline-diff` to see which tasks moved and why; fix the cause, or capture a new baseline if the change is intended.

### eval 2004

`Eval baseline {path} could not be read: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Regression · **Log:** eval
- **Fields:** `path`, `err`
- **Find it:** `provider:eval id:2004`

**What it means.** The baseline exists but doesn't parse, so this run has no regression verdict.

**Why it happens.** The file was edited by hand, truncated, or written by an incompatible version.

**What to do.** Capture it again: `tenant eval --baseline-from <a good artifact>`.

### eval 2005

`Eval {record} was not saved: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Run · **Log:** eval
- **Fields:** `record`, `path`, `err`
- **Find it:** `provider:eval id:2005`

**What it means.** A run finished, but its report artifact or its trend line didn't reach <data>/eval-artifacts.

**Why it happens.** The data directory is full, unwritable or locked.

**What to do.** Check the data directory's free space and permissions. The run's scores are still in this log (eval/1001).

### eval 2006

`Schedule {value} is malformed and ignored: {effect}`

- **Level:** warning · **Channel:** operational · **Task:** Schedule · **Log:** eval
- **Fields:** `value`, `effect`, `err`
- **Find it:** `provider:eval id:2006`

**What it means.** An eval cadence setting didn't parse, so it was ignored rather than guessed at.

**Why it happens.** improve.eval_at is not HH:MM, or an improve.*_every duration is not like 24h.

**What to do.** Set it again with /eval every <duration> or /eval at HH:MM (applies live), or fix it in config.json.

### eval 3001

`The route classifier panicked in an eval task: {panic}`

- **Level:** error · **Channel:** operational · **Task:** Task · **Log:** eval
- **Fields:** `panic`, `stack`
- **Find it:** `provider:eval id:3001`

**What it means.** A bug in the route classifier was caught while a route task measured it; that sample counts as a failed classification.

**Why it happens.** A programming error in the classifier.

**What to do.** Report it with the stack. The rest of the run goes on.

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

## improve

Writes the `improve` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1001](#improve-1001) | info | Job run | `Self-improvement job {job}` |
| [1002](#improve-1002) | info | Job run | `{job}: {summary}` |
| [2001](#improve-2001) | warning | Job run | `{job}: {step} failed; the run went on: {err}` |
| [2002](#improve-2002) | warning | Consolidation | `Consolidation could not tell which facts are protected, so it may merge any of {facts} facts: {err}` |
| [2003](#improve-2003) | warning | Reflection | `Reflection wrote nothing: the synthesis failed: {err}` |
| [2004](#improve-2004) | warning | Reflection | `Reflection cut SME section {section} to {cap} tokens; it was about {tokens}` |
| [2005](#improve-2005) | warning | Soul nudge | `Soul nudge proposed nothing: the candidate could not be scored: {err}` |
| [2006](#improve-2006) | warning | Configuration | `improve.profile {profile} is not usable, so the jobs stay on the main model: {reason}` |

### improve 1001

`Self-improvement job {job}`

- **Level:** info · **Channel:** operational · **Task:** Job run · **Log:** improve
- **Fields:** `job`
- **Find it:** `provider:improve id:1001`

**What it means.** A self-improvement job ran: its start, then its stop with the duration, or its failure with the error.

**Why it happens.** The scheduler found it due (serve, or the TUI with self-improvement on), or the operator asked for a run.

**What to do.** None when it stops. On a failure, read the error, and the job's warnings under the same activity.

### improve 1002

`{job}: {summary}`

- **Level:** info · **Channel:** operational · **Task:** Job run · **Log:** improve
- **Fields:** `job`, `summary`, `changed`, `source_runs`
- **Find it:** `provider:improve id:1002`

**What it means.** What one run of a self-improvement job did. source_runs are the turns (runs.db trace ids) it learned from.

**Why it happens.** Every run that does not fail reports its result, whether it changed anything or not.

**What to do.** None. To see what a run wrote, follow its activity to the memory and skills events it caused.

### improve 2001

`{job}: {step} failed; the run went on: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Job run · **Log:** improve
- **Fields:** `job`, `step`, `err`, `fact_id`, `section`, `size`
- **Find it:** `provider:improve id:2001`

**What it means.** A best-effort step of a self-improvement job failed, and the job skipped it.

**Why it happens.** The model, or a memory database the step writes to, returned an error.

**What to do.** None if it is rare: the next run tries again. If it repeats, check the model and the memory databases with tenant doctor.

### improve 2002

`Consolidation could not tell which facts are protected, so it may merge any of {facts} facts: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Consolidation · **Log:** improve
- **Fields:** `facts`, `err`
- **Find it:** `provider:improve id:2002`

**What it means.** Consolidation ran without leaving out pinned and protected facts, so one of them may be merged into a broader fact.

**Why it happens.** The fact signals table in facts.db could not be read.

**What to do.** Check facts.db with tenant doctor. A fact merged by mistake can be restored from its page in the dashboard.

### improve 2003

`Reflection wrote nothing: the synthesis failed: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Reflection · **Log:** improve
- **Fields:** `err`
- **Find it:** `provider:improve id:2003`

**What it means.** The reflection job did not update the project's SME document; the previous version stays in use.

**Why it happens.** The proposer model failed, or answered with something that is not the expected JSON.

**What to do.** Check the model set by improve.profile (or the main model). The next run tries again.

### improve 2004

`Reflection cut SME section {section} to {cap} tokens; it was about {tokens}`

- **Level:** warning · **Channel:** operational · **Task:** Reflection · **Log:** improve
- **Fields:** `section`, `tokens`, `cap`
- **Find it:** `provider:improve id:2004`

**What it means.** A section of the SME document was longer than its cap, so its end was cut off.

**Why it happens.** The model wrote more than the per-section limit allows.

**What to do.** None usually. If details go missing from the section, raise its token cap.

### improve 2005

`Soul nudge proposed nothing: the candidate could not be scored: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Soul nudge · **Log:** improve
- **Fields:** `err`
- **Find it:** `provider:improve id:2005`

**What it means.** The soul-nudge job had a candidate, but could not run the fitness check that must pass before it is proposed.

**Why it happens.** There is no fitness baseline yet, or the eval run failed.

**What to do.** Capture a fitness baseline (tenant eval) if there is none; otherwise read the error. Nothing is proposed until it passes.

### improve 2006

`improve.profile {profile} is not usable, so the jobs stay on the main model: {reason}`

- **Level:** warning · **Channel:** operational · **Task:** Configuration · **Log:** improve
- **Fields:** `profile`, `reason`, `err`
- **Find it:** `provider:improve id:2006`

**What it means.** The self-improvement jobs run on the main model instead of the profile improve.profile names.

**Why it happens.** The profile is not in config.json, or its model could not be set up.

**What to do.** Fix the profile's entry in config.json, or clear improve.profile.

## memory

Writes the `memory` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1001](#memory-1001) | info | Fact | `Fact {fact_id} added by {job}` |
| [1002](#memory-1002) | info | Fact | `Fact {fact_id} superseded by fact {by}` |
| [1003](#memory-1003) | info | Fact | `Fact {fact_id} removed` |
| [1004](#memory-1004) | info | Fact | `Fact {fact_id} restored` |
| [1101](#memory-1101) | info | Compaction | `Compacted a conversation from {before} messages to {after}` |
| [2001](#memory-2001) | warning | Distillation | `Distillation skipped {episodes} episodes from #{first_episode} at {step}: {err}` |
| [2002](#memory-2002) | warning | Distillation | `Distillation: {step} failed for fact {fact_id}: {err}` |
| [2003](#memory-2003) | warning | Distillation | `Could not tell whether a new fact repeats fact {fact_id}; added it anyway: {err}` |
| [9001](#memory-9001) | info | Retrieval | `Retrieved {facts} facts and {episodes} episodes for the turn` |
| [9002](#memory-9002) | info | Retrieval | `memory_recall found {facts} facts, {episodes} episodes and {archived} archive events` |

### memory 1001

`Fact {fact_id} added by {job}`

- **Level:** info · **Channel:** operational · **Task:** Fact · **Log:** memory
- **Fields:** `fact_id`, `job`, `agent`, `source_runs`, `fact` (sensitive, masked: `fact`)
- **Find it:** `provider:memory id:1001`

**What it means.** A fact was written to long-term memory (facts.db). source_runs are the turns it was learned from.

**Why it happens.** Distillation or consolidation learned it, the agent or the operator saved it, an import brought it, or an edit or restore made a new version.

**What to do.** None. To see where it came from, open `/memory/facts/<fact_id>` in the dashboard.

### memory 1002

`Fact {fact_id} superseded by fact {by}`

- **Level:** info · **Channel:** operational · **Task:** Fact · **Log:** memory
- **Fields:** `fact_id`, `by`
- **Find it:** `provider:memory id:1002`

**What it means.** A fact was replaced by a newer one. It stays in facts.db for its history, but recall no longer uses it.

**Why it happens.** Distillation learned something that contradicts it, consolidation merged it, or it was edited or resolved as a duplicate.

**What to do.** None. Its versions are on `/memory/facts/<fact_id>` in the dashboard, where an earlier one can be restored.

### memory 1003

`Fact {fact_id} removed`

- **Level:** info · **Channel:** operational · **Task:** Fact · **Log:** memory
- **Fields:** `fact_id`
- **Find it:** `provider:memory id:1003`

**What it means.** A fact was removed from memory: recall no longer uses it, but it can still be restored.

**Why it happens.** The operator or the agent forgot it.

**What to do.** None. To undo it, restore it from the dashboard's removed-facts page.

### memory 1004

`Fact {fact_id} restored`

- **Level:** info · **Channel:** operational · **Task:** Fact · **Log:** memory
- **Fields:** `fact_id`
- **Find it:** `provider:memory id:1004`

**What it means.** A removed fact was brought back, and recall uses it again.

**Why it happens.** The operator restored it.

**What to do.** None.

### memory 1101

`Compacted a conversation from {before} messages to {after}`

- **Level:** info · **Channel:** operational · **Task:** Compaction · **Log:** memory
- **Fields:** `session`, `before`, `after`, `origin`, `source_messages`
- **Find it:** `provider:memory id:1101`

**What it means.** The oldest turns of a conversation were summarized into one message, so it fits the model's context again. origin says whether the summary was made from the archive's raw events or from the working set.

**Why it happens.** The conversation filled most of its share of the context window, or the operator ran /compress.

**What to do.** None. If answers lose the thread after this, lower the model's context use or raise its context length.

### memory 2001

`Distillation skipped {episodes} episodes from #{first_episode} at {step}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Distillation · **Log:** memory
- **Fields:** `step`, `first_episode`, `episodes`, `agent`, `err`
- **Find it:** `provider:memory id:2001`

**What it means.** Distillation could not learn facts from a batch of turns. It moved past them, so their facts are not extracted.

**Why it happens.** The summarizer model failed or answered with something that is not the expected JSON (extract), or embedding or writing the facts failed (persist).

**What to do.** Check the summarizer model (tenant doctor). If it keeps happening, the batch size may be too large for the model.

### memory 2002

`Distillation: {step} failed for fact {fact_id}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Distillation · **Log:** memory
- **Fields:** `step`, `fact_id`, `by`, `err`
- **Find it:** `provider:memory id:2002`

**What it means.** A best-effort write after distilling a fact failed; the fact itself was kept.

**Why it happens.** facts.db refused the write: it is busy, full or damaged.

**What to do.** If it repeats, check facts.db with tenant doctor.

### memory 2003

`Could not tell whether a new fact repeats fact {fact_id}; added it anyway: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Distillation · **Log:** memory
- **Fields:** `fact_id`, `err`
- **Find it:** `provider:memory id:2003`

**What it means.** Distillation added a fact that may repeat or contradict an existing one, because the model could not judge them.

**Why it happens.** The summarizer model failed or gave an answer that could not be read.

**What to do.** None usually: consolidation merges duplicates later. If it repeats, check the summarizer model.

### memory 9001

`Retrieved {facts} facts and {episodes} episodes for the turn`

- **Level:** info · **Channel:** diagnostic · **Task:** Retrieval · **Log:** memory
- **Fields:** `facts`, `episodes`, `fact_ids`, `episode_ids`, `truncations`
- **Find it:** `provider:memory id:9001`

**What it means.** What long-term memory put into a turn's context, after its budget cut: the facts and past turns by id.

**Why it happens.** Every turn retrieves memory.

**What to do.** None. When an answer ignores something memory holds, check whether its fact was retrieved here.

### memory 9002

`memory_recall found {facts} facts, {episodes} episodes and {archived} archive events`

- **Level:** info · **Channel:** diagnostic · **Task:** Retrieval · **Log:** memory
- **Fields:** `query`, `scope`, `facts`, `episodes`, `archived`, `err` (sensitive, masked: `query`)
- **Find it:** `provider:memory id:9002`

**What it means.** The agent searched its memory with memory_recall. err holds a store's search error, which the agent never sees.

**Why it happens.** The agent decided it needed something from earlier conversations.

**What to do.** None; on an err, check the memory databases with tenant doctor.

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

## privacy

Writes the `privacy` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1001](#privacy-1001) | info | Retention | `Retention sweep` |
| [1002](#privacy-1002) | info | Deletion | `Privacy deletion ({reason})` |
| [1003](#privacy-1003) | info | Deletion | `{action} {count} {data}` |
| [2001](#privacy-2001) | warning | Deletion | `A privacy deletion did not fully finish: {warning}` |

### privacy 1001

`Retention sweep`

- **Level:** info · **Channel:** operational · **Task:** Retention · **Log:** privacy
- **Fields:** `classes`
- **Find it:** `provider:privacy id:1001`

**What it means.** The daily retention sweep of a running Tenant: its start, then its stop with the duration, or its failure with the error. classes counts the data classes that have a retention period; a sweep with none removes nothing.

**Why it happens.** Serve and the TUI sweep a few minutes after they start, then daily.

**What to do.** None when it stops. On a failure, read the error: usually config.json could not be read, and nothing was removed.

### privacy 1002

`Privacy deletion ({reason})`

- **Level:** info · **Channel:** operational · **Task:** Deletion · **Log:** privacy
- **Fields:** `reason`
- **Find it:** `provider:privacy id:1002`

**What it means.** Data was deleted for good: its start, the counts removed, then its stop with the duration, or its failure with the error. reason is forget (the operator asked) or retention (a class aged out).

**Why it happens.** tenant privacy forget or prune, or the retention sweep.

**What to do.** None when it stops. After a failure, the stores done before it stay deleted and the rest are unchanged; run the command again.

### privacy 1003

`{action} {count} {data}`

- **Level:** info · **Channel:** operational · **Task:** Deletion · **Log:** privacy
- **Fields:** `data`, `action`, `count`, `reason`
- **Find it:** `provider:privacy id:1003`

**What it means.** One kind of data a deletion changed: rows or files deleted, archive files rewritten, or records unlinked from what was removed.

**Why it happens.** A privacy deletion ran.

**What to do.** None.

### privacy 2001

`A privacy deletion did not fully finish: {warning}`

- **Level:** warning · **Channel:** operational · **Task:** Deletion · **Log:** privacy
- **Fields:** `warning`, `reason`
- **Find it:** `provider:privacy id:2001`

**What it means.** The data was deleted, but a clean-up step after it did not finish, so copies may linger until later.

**Why it happens.** A running Tenant was reading the store, so its WAL still holds old pages, or its search index could not be compacted.

**What to do.** None usually: the store's next checkpoint clears its WAL. A deletion run while Tenant is stopped leaves no copies.

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

## skills

Writes the `skills` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1001](#skills-1001) | info | Skill | `Skill {skill} saved ({source})` |
| [1002](#skills-1002) | info | Skill | `Skill {skill} is now {trust} (was {prior})` |
| [1003](#skills-1003) | info | Skill | `Skill {skill} accepted` |
| [1004](#skills-1004) | info | Skill | `Skill {skill} {state}` |
| [1005](#skills-1005) | info | Skill | `Skill {skill} removed` |
| [1006](#skills-1006) | info | Package | `Skill {skill} imported from {from}` |
| [1007](#skills-1007) | info | Package | `Skill {skill} exported to {dir}` |

### skills 1001

`Skill {skill} saved ({source})`

- **Level:** info · **Channel:** operational · **Task:** Skill · **Log:** skills
- **Fields:** `skill`, `skill_id`, `source`, `status`, `trust`, `source_runs`
- **Find it:** `provider:skills id:1001`

**What it means.** A skill was added or changed. source says how: operator, induction, auto-accept, import, seed or revert. Its earlier version is in its history (/skills history).

**Why it happens.** The operator or the agent saved it, the skill-induction job proposed it, a package was imported, or a version was restored.

**What to do.** None. /skills show names what it does; /skills diff compares it with its earlier version.

### skills 1002

`Skill {skill} is now {trust} (was {prior})`

- **Level:** info · **Channel:** operational · **Task:** Skill · **Log:** skills
- **Fields:** `skill`, `skill_id`, `trust`, `prior`, `source`
- **Find it:** `provider:skills id:1002`

**What it means.** A skill's trust changed. An untrusted skill is not offered to the agent.

**Why it happens.** The operator trusted or distrusted it, or a change widened the tools it expects to use, which makes it untrusted until reviewed.

**What to do.** If it became untrusted by a change, review it (/skills show) and trust it again if it is fine.

### skills 1003

`Skill {skill} accepted`

- **Level:** info · **Channel:** operational · **Task:** Skill · **Log:** skills
- **Fields:** `skill`, `skill_id`
- **Find it:** `provider:skills id:1003`

**What it means.** A proposed skill went live: it is enabled and offered to the agent.

**Why it happens.** The operator accepted a skill the skill-induction job proposed.

**What to do.** None.

### skills 1004

`Skill {skill} {state}`

- **Level:** info · **Channel:** operational · **Task:** Skill · **Log:** skills
- **Fields:** `skill`, `skill_id`, `state`
- **Find it:** `provider:skills id:1004`

**What it means.** A skill was enabled or disabled. A disabled skill is not offered to the agent.

**Why it happens.** The operator switched it on or off.

**What to do.** None.

### skills 1005

`Skill {skill} removed`

- **Level:** info · **Channel:** operational · **Task:** Skill · **Log:** skills
- **Fields:** `skill`, `skill_id`
- **Find it:** `provider:skills id:1005`

**What it means.** A skill was forgotten: it is disabled and hidden, and its row and history stay until a privacy forget deletes them.

**Why it happens.** The operator or the agent forgot it.

**What to do.** None. Saving a skill of the same name brings it back.

### skills 1006

`Skill {skill} imported from {from}`

- **Level:** info · **Channel:** operational · **Task:** Package · **Log:** skills
- **Fields:** `skill`, `from`, `digest`, `trusted`, `unchanged`
- **Find it:** `provider:skills id:1006`

**What it means.** An Agent Skills package (SKILL.md) was imported as a skill. It is untrusted unless the import said otherwise.

**Why it happens.** The operator ran tenant skills import or /skills import.

**What to do.** Review it (/skills show) and trust it (/skills trust) before the agent may use it.

### skills 1007

`Skill {skill} exported to {dir}`

- **Level:** info · **Channel:** operational · **Task:** Package · **Log:** skills
- **Fields:** `skill`, `dir`
- **Find it:** `provider:skills id:1007`

**What it means.** A skill was written out as an Agent Skills package (SKILL.md).

**Why it happens.** The operator ran tenant skills export or /skills export.

**What to do.** None.

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
| [10](#system-10) | info | Lifecycle | `Start-up of {role}` |
| [20](#system-20) | info | Shutdown | `Shutdown of {steps} steps` |
| [21](#system-21) | info | Shutdown | `Shutdown stage {stage}: {steps} steps` |
| [1301](#system-1301) | info | Tailscale | `Publish the dashboard to the tailnet` |
| [1302](#system-1302) | info | Tailscale | `Withdraw the dashboard from the tailnet` |
| [2020](#system-2020) | warning | Shutdown | `Shutdown step {stage}/{step} failed: {err}` |
| [2110](#system-2110) | warning | Component | `{component} unavailable ({effect}): {err}` |
| [2201](#system-2201) | warning | Storage | `{what} was not saved: {err}` |
| [3010](#system-3010) | error | Lifecycle | `{role} stopped with an error: {err}` |
| [3020](#system-3020) | error | Shutdown | `Shutdown step {stage}/{step} panicked: {panic}` |
| [3101](#system-3101) | error | Component | `{component} failed: {err}` |
| [5001](#system-5001) | info | Configuration | `{path} was edited; applied {changed}` |
| [5002](#system-5002) | warning | Configuration | `{path} did not load, so {fallback}: {err}` |
| [5003](#system-5003) | info | Configuration | `credentials.json changed; the active model provider's key was reloaded` |
| [5004](#system-5004) | warning | Configuration | `A changed key did not reload into the live model provider: {err}` |
| [5010](#system-5010) | warning | Agent profile | `Agent profile {profile}: model {configured} is served as {served}; using that` |
| [5011](#system-5011) | warning | Agent profile | `Agent profile {profile}: the endpoint does not serve model {configured}; its calls may fail` |

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

### system 10

`Start-up of {role}`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** system
- **Fields:** `role`
- **Keywords:** `lifecycle`
- **Find it:** `provider:system id:10`

**What it means.** serve or the TUI starting up: its start, then its stop with the duration once it is ready, or its failure with the error.

**Why it happens.** The operator or a service manager started Tenant.

**What to do.** None when it stops. On a failure Tenant did not start: read the error, fix its cause and start it again.

### system 20

`Shutdown of {steps} steps`

- **Level:** info · **Channel:** operational · **Task:** Shutdown · **Log:** system
- **Fields:** `steps`
- **Keywords:** `lifecycle`
- **Find it:** `provider:system id:20`

**What it means.** Tenant shutting down, stage by stage: its start, then its stop with the duration, or its failure naming every step that failed.

**Why it happens.** serve or the TUI was asked to stop (Ctrl-C, the service manager, an update), or it is leaving after an error.

**What to do.** None when it stops. On a failure, read the steps' events under it; the shutdown still ran every other step.

### system 21

`Shutdown stage {stage}: {steps} steps`

- **Level:** info · **Channel:** operational · **Task:** Shutdown · **Log:** system
- **Fields:** `stage`, `steps`
- **Keywords:** `lifecycle`
- **Find it:** `provider:system id:21`

**What it means.** One stage of the shutdown: unblock (deny pending approvals), intake (stop taking new work), drain (let running work finish) or release (close stores). Its start, then its stop with the duration, or its failure.

**Why it happens.** The shutdown reached the stage.

**What to do.** None when it stops. A slow drain is a job or turn still running; a failure names the steps that failed.

### system 1301

`Publish the dashboard to the tailnet`

- **Level:** info · **Channel:** operational · **Task:** Tailscale · **Log:** system
- **Find it:** `provider:system id:1301`

**What it means.** tailscale serve publishing the loopback dashboard to the operator's tailnet: its start, then its stop with the duration, or its failure with the error.

**Why it happens.** The operator ran /tailscale serve, or Tenant re-applied that choice at start.

**What to do.** None when it stops. On a failure, install Tailscale or run tailscale up, then try again.

### system 1302

`Withdraw the dashboard from the tailnet`

- **Level:** info · **Channel:** operational · **Task:** Tailscale · **Log:** system
- **Find it:** `provider:system id:1302`

**What it means.** tailscale serve reset withdrawing the dashboard from the tailnet: its start, then its stop with the duration, or its failure with the error.

**Why it happens.** The operator ran /tailscale unserve.

**What to do.** None when it stops. On a failure, check tailscale serve status and reset it by hand.

### system 2020

`Shutdown step {stage}/{step} failed: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Shutdown · **Log:** system
- **Fields:** `stage`, `step`, `err`, `took`
- **Find it:** `provider:system id:2020`

**What it means.** A component did not stop cleanly. The shutdown went on with the other steps.

**Why it happens.** Closing it returned an error: a store that could not flush, a listener already gone, a timeout.

**What to do.** Read the error. A store that failed to close may have lost its last writes; tenant doctor checks the databases.

### system 2110

`{component} unavailable ({effect}): {err}`

- **Level:** warning · **Channel:** operational · **Task:** Component · **Log:** system
- **Fields:** `component`, `effect`, `err`
- **Find it:** `provider:system id:2110`

**What it means.** A part of Tenant did not come up, or could not be refreshed, so Tenant runs without it. effect says what is missing.

**Why it happens.** Its store or file could not be opened (permissions, a full disk, a database another process holds), or what it depends on failed.

**What to do.** Read the error and fix its cause, then restart Tenant, or for the embedder switch the model again.

### system 2201

`{what} was not saved: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Storage · **Log:** system
- **Fields:** `what`, `path`, `err`
- **Find it:** `provider:system id:2201`

**What it means.** A change applies in the running process but was not written to disk, so a restart will undo it.

**Why it happens.** The file could not be written: permissions, a full disk, or a read-only config or data directory.

**What to do.** Fix the path's permissions or free space, then make the change again so it is saved.

### system 3010

`{role} stopped with an error: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** system
- **Fields:** `role`, `err`
- **Keywords:** `lifecycle`
- **Find it:** `provider:system id:3010`

**What it means.** Tenant ran, then stopped because of an error rather than being asked to. Its shutdown still ran.

**Why it happens.** The terminal UI failed: the terminal went away or could not be driven.

**What to do.** Read the error, then start Tenant again. If it repeats, run it in another terminal.

### system 3020

`Shutdown step {stage}/{step} panicked: {panic}`

- **Level:** error · **Channel:** operational · **Task:** Shutdown · **Log:** system
- **Fields:** `stage`, `step`, `panic`, `stack`, `took`
- **Find it:** `provider:system id:3020`

**What it means.** A component's stop crashed. The panic was contained and the shutdown went on with the other steps.

**Why it happens.** A bug in that component's stop.

**What to do.** Report it with the stack. The component may not have released what it held; check its store with tenant doctor.

### system 3101

`{component} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** Component · **Log:** system
- **Fields:** `component`, `err`
- **Find it:** `provider:system id:3101`

**What it means.** A subsystem stopped with an error.

**Why it happens.** Something it depends on failed: a connection, a file or its configuration.

**What to do.** Open the component's log; the event before this one usually names the cause.

### system 5001

`{path} was edited; applied {changed}`

- **Level:** info · **Channel:** operational · **Task:** Configuration · **Log:** system
- **Fields:** `path`, `changed`
- **Keywords:** `config`
- **Find it:** `provider:system id:5001`

**What it means.** The settings file changed on disk, and the running Tenant applied what differs, with no restart: tool rules, category approval modes or exec profiles.

**Why it happens.** Someone edited the file by hand or from another window, or a script pushed it.

**What to do.** None. Check that changed lists what you meant to change.

### system 5002

`{path} did not load, so {fallback}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Configuration · **Log:** system
- **Fields:** `path`, `fallback`, `err`
- **Keywords:** `config`
- **Find it:** `provider:system id:5002`

**What it means.** A configuration file could not be read or parsed, so Tenant goes on without it; fallback says what it uses instead.

**Why it happens.** The file has a JSON error, or it cannot be read.

**What to do.** Fix the file (tenant doctor points at a JSON error); an edit is picked up without a restart.

### system 5003

`credentials.json changed; the active model provider's key was reloaded`

- **Level:** info · **Channel:** operational · **Task:** Configuration · **Log:** system
- **Keywords:** `config`
- **Find it:** `provider:system id:5003`

**What it means.** A key was rotated outside the settings page, and the running Tenant now uses it, with no restart. The model log records the swap.

**Why it happens.** A secrets manager or a hand edit changed credentials.json.

**What to do.** None.

### system 5004

`A changed key did not reload into the live model provider: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Configuration · **Log:** system
- **Fields:** `provider`, `source`, `err`
- **Keywords:** `config`
- **Find it:** `provider:system id:5004`

**What it means.** A model provider's key changed, but the running Tenant still uses the old one. source says where the change came from.

**Why it happens.** The provider's configuration could not be read, or the provider could not be rebuilt with the new key.

**What to do.** Read the error, then switch to the provider again with /model use or the dashboard's model page.

### system 5010

`Agent profile {profile}: model {configured} is served as {served}; using that`

- **Level:** warning · **Channel:** operational · **Task:** Agent profile · **Log:** system
- **Fields:** `profile`, `configured`, `served`
- **Keywords:** `config`
- **Find it:** `provider:system id:5010`

**What it means.** An agent profile names its model in a different letter case than the endpoint serves it, so Tenant uses the served name.

**Why it happens.** A typo in the profile's model name. The endpoint's model lookup is case-sensitive.

**What to do.** Correct the model name in the agent profile.

### system 5011

`Agent profile {profile}: the endpoint does not serve model {configured}; its calls may fail`

- **Level:** warning · **Channel:** operational · **Task:** Agent profile · **Log:** system
- **Fields:** `profile`, `configured`, `served`
- **Keywords:** `config`
- **Find it:** `provider:system id:5011`

**What it means.** An agent profile names a model its endpoint does not list, so the sub-agents that use it may fail every call.

**Why it happens.** The model name is wrong, or the server loaded another model.

**What to do.** Set the profile's model to one of served, or load the model on the server.

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
