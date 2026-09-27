# Logging — the unified log book

Every subsystem in Tenant writes through **one log book** (`internal/logs`).
It files each line under a per-source folder of plain-text daily files and
keeps the same lines in memory for the dashboard's Activity tab. One source is
one folder is one tab.

```
<data>/logs/                      ← the root (macOS: ~/Library/Application Support/tenant/logs)
├── turns/                        ← the primary agent's own turns: lifecycle, tool calls/results, errors
│   ├── 2026-09-21.log
│   └── 2026-09-22.log
├── subagents/                    ← orchestrated sub-agents (Programmer, Researcher, …), their own warnings, the bus, the team budget
├── research/                     ← /research passes and their record failures
├── approvals/                    ← every gated-action decision: pending / approved / denied / expired (kept this release; they move into security)
├── discord/                      ← the relay: inbound messages, the relay agent's actions, relay logs
├── imessage/
├── cron/                         ← scheduled-job runner agents + the engine
├── eval/                         ← each eval run and its tasks, regressions, trend updates, harness errors; the task agents' turns
├── improve/                      ← every self-improvement job run (distill, skills, consolidate, reflect, soul, profile, eval), serve's and the TUI's
├── memory/                       ← facts added, superseded, removed (fact_id=); compaction; retrieval diagnostics
├── skills/                       ← skills saved, accepted, enabled/disabled, trusted, removed, imported, exported
├── model/                        ← router, backends (llama.cpp/vLLM, Anthropic, …), fallbacks
├── route/ peering/
├── dashboard/                    ← the web server's start/stop and errors, live streams, rejected requests
├── mcp.<name>/                   ← one per MCP connector and local server: its connections, sign-in, failed calls, a server's process and stderr
├── mcp/                          ← MCP lines that belong to no one server (`tenant mcp-memory`)
├── sandbox/                      ← the execution sandbox: every run's start and exit, refusals, escalations, dry-run would-be refusals
├── backup/                       ← each backup, verify and restore, with its counts and result
├── privacy/                      ← each retention sweep and deletion, with what it removed
├── system/                       ← start-up and shutdown (each shutdown stage an activity; was shutdown/), settings and credentials reloads, Tailscale, setup, health, the status feed lines (was tui.log), crash dumps
├── boots/ crash/                 ← records, not sources: see "Crashes and unexpected exits"
├── logbook.db                    ← the store: every event, indexed (see "The store: logbook.db")
├── logbook.lock                  ← held shared by every process with the store open
├── logbook/                      ← the log book's own events: its store, write failures, what it dropped and suppressed
├── archive/                      ← the store's archived records (security-<first>-<last>.ndjson.gz)
├── spool/                        ← events left by writers while no Tenant process was up (see "Intake")
└── <anything-you-add>/           ← a script, a sidecar, a new Go package — see "Adding a source"
```

Override the root with `--log-dir DIR` or `"logs": {"dir": "…"}` in config.json.

## The line format

One entry is exactly one line, readable in `tail -f` and exact under `grep`:

```
2026-09-22 14:03:11.123 INFO  [turns] tool call web_search	event=turns/9001 task="Tool call" ev=tool_call iter=2 tool=web_search args="{\"q\":\"hn top\"}" trace=9c1e6f0a2b7d4e35a8f1c0d2e3b4a5c6
2026-09-22 14:03:12.001 ERROR [turns] tool result web_search failed	event=turns/9003 task="Tool call" ev=tool_result iter=2 tool=web_search duration=30.001s result="timeout after 30s" trace=9c1e6f0a2b7d4e35a8f1c0d2e3b4a5c6
2026-09-22 14:03:40.250 WARN  [approvals] approval denied ap-3f9c2a1b-3	ev=approval_resolved category=exec action=os_exec origin=local outcome=denied trace=9c1e6f0a2b7d4e35a8f1c0d2e3b4a5c6
2026-09-22 14:04:02.000 INFO  [discord] relay attachment saved	chat=general path=/…/relay-inbox/img.png
```

`<local date> <time.ms> <LEVEL> [<source>] <message>` then a **tab** and
`key=value` fields. Values with spaces, quotes, newlines or other control
characters are quoted (Go `strconv.Quote` escaping), so a multi-line tool
result still occupies one line. In the message, newlines and tabs become
spaces and every other control character (ESC, DEL, the C1 set, a byte that
isn't UTF-8) is escaped the same way (`\x1b`), so a line can't drive the
terminal of a `tail -f`.
Turn events carry their kind in `ev=`; plain records have no `ev`. Every
event a turn raises, and the approvals it asks for, carries `trace=`: the
turn's trace ID, the `trace_id` of its record in `runs.db`. A slog line logged
with a `…Context` call inside a turn carries it too.

- **Retention:** files older than 30 days are deleted (`"logs": {"retention_days": N}`;
  `-1` keeps forever, or `tenant privacy retention set logs N|forever`). A
  running Tenant applies a change at its next daily privacy sweep, with no
  restart. With the store on, each folder keeps its log's max age instead
  (see [Retention and disk budgets](#retention-and-disk-budgets)). A file
  another process has open (Windows) is skipped, counted in a `logbook` line,
  and retried at the next hourly prune. A day's file rolls to `<day>.1.log`
  past 64 MB.
- **Level floor:** INFO by default. `"logs": {"level": "warn"}` (or `"error"`)
  raises it for the files and the feed; `"debug"` or `TENANT_LOG=debug` lowers
  it (the files then hold the backend internals too).
- **Value cap:** one value (a tool result, an argument blob) is cut at 4 KB with
  a `…(+N bytes)` marker (`"logs": {"max_field_bytes": N}`; `-1` = unlimited),
  after secrets are scrubbed from it (see [Secrets](#secrets)).

Where lines land: a turn-event's source is decided by the emitter (`cron`,
`discord`, `eval`, …) or derived (`subagents` when a sub-agent is attributed,
`approvals`, the channel for an inbound message, else `turns`). A plain slog
record goes to the logger's fixed source (`logs.For("x")` / `hub.Logger("x")`),
else a `source` attribute, else its `"tag: …"` message prefix (`mcp:` → `mcp`,
`vllm:` → `model`, `reflect:` → `improve`, …), else `system`.

`tenant backup` and `tenant privacy prune|forget` are one-shot commands, yet
their events land here too: held in memory while the command runs, appended
when it ends (a restore's go into the restored log book).

`serve` still writes its TENANT_LOG-filtered stream to stderr for the
supervisor (launchd/systemd) — the hub tees to it. The TUI writes nothing to
stderr; `<data>/tui.log` no longer exists.

Nothing in serve or the TUI writes around the log book (TEN-363):

- `slog.Default()` and the `log` package point at the hub, so a library's line
  (chromedp, a plugin's `slog.Default().Warn`) is filed, never drawn over the
  TUI. So does every `http.Server`'s error log (TLS handshake failures, handler
  panics): `httplimits.Configure` gives it one at WARN, and an archtest check
  fails a server built without it.
- Status-feed lines are filed before the feed gets them, so a full feed drops
  only its own copy: serve's setup and health lines under `system`, the TUI's
  self-improvement results under `improve`, research progress under `research`
  and the model reconnect monitor under `model`.
- The Discord relay, iMessage and cron agents' tool calls, results and finals
  are filed under `discord`, `imessage` and `cron` under the TUI as under
  serve. Sub-agents' own lines go to `subagents`, not `turns`. A peer that stops
  answering health probes, or comes back, is logged under `peering`.
- A sign-in URL (remote MCP, Atlassian, Google) is shown in the status feed and
  filed under `system`; a one-shot command prints it on stderr.
- When the log book cannot be opened, a memory-only hub stands in (the lines
  still reach the dashboard feed) and the failure is shown: in the TUI's feed,
  on serve's stderr.

A one-shot command (and `tenant mcp-memory`) files its warnings and errors in
the log book too, under the same sources, while still printing them on stderr
at the TENANT_LOG level. Its hub runs no background loops and opens a day
file only when the first warning comes. The nightly eval harness's notes go to
`eval` under serve and the TUI, and to stderr from `tenant eval`, whose table
and summary stay the command's output.

A new `fmt.Fprint*(os.Stderr, …)` or `log.Print*` call in `cmd/tenant` or an
internal package fails `go test ./internal/archtest`: the files that had one
(mostly one-shot commands' terminal output) are listed with their counts in
`internal/archtest/stray.go`, and a count only goes down. Log through
`logs.For` or `slog.Default()` instead.

## Secrets

Every entry is scrubbed before the value cap, the Activity feed and the files:
the message and every field, so the `sandbox` folder's `command` and the tool
arguments and results in `turns` too.

- **Credentials Tenant holds** become `«secret:<name>»` wherever they appear:
  raw, JSON-escaped, URL-encoded or base64-encoded (also inside a longer value,
  such as a Basic credential). They are the values in Tenant's secret files
  (credentials.json, the dashboard token, OAuth token caches, peer tokens and
  keys), `dashboard.auth`, MCP `env_set` values, and API keys given with
  `--api-key` or in the environment. Values under 8 characters are ignored.
  The set is re-read whenever Tenant writes a secret file, and every 20 seconds
  for changes made by hand or by another Tenant process.
- **Text shaped like a credential** loses the secret and keeps its label:
  bearer tokens and JWTs; `sk-`, `sk_live_`/`rk_live_`, `ghp_`/`github_pat_`,
  `xox?-`, `AIza…`, `tskey-` and AWS keys; Discord bot tokens and webhook URLs;
  PEM private keys; the password in a `user:pass@` URL; `password=` and
  `token=` style parameters; JSON `"password"`, `"secret"`, `"token"` and
  `"api_key"` fields; and any field whose key ends in one of those words.

```
2026-09-27 14:05:10.412 INFO  [sandbox] Command on restricted for discord	event=sandbox/1 task=Run opcode=start surface=discord backend=restricted command="curl -H 'Authorization: Bearer «secret:bearer»' https://api.example.com" limits="wall=1m0s"
```

Lines read back from disk into the feed (the tail re-read at start, folders
other processes write) are scrubbed on the way in; the files themselves are
not rewritten. The copies of log lines `serve` prints to stdout and stderr
are scrubbed too.

The dashboard's auth token never goes to the log book. The `system` line
written when the dashboard starts says where the token lives
(`<config>/dashboard-auth-token`, or `dashboard.auth` in config.json) and how
to show it. `serve` prints the token itself only when stdout is a terminal.

Builds before TEN-360 wrote the token into `system/*.log`, the Activity feed
and serve's stdout. If you ran one, rotate the token: stop Tenant, delete
`<config>/dashboard-auth-token` (or change `dashboard.auth`), and start it
again. The old files then hold a dead token.

## The `sandbox` source

The execution sandbox (`docs/changes/sandbox-seam.md`) files every `os_exec`
command and cron shell job it runs, on every backend, the host included. It
is catalogued ([EVENTS.md](EVENTS.md#sandbox)) and sits in the viewer's
Sandbox group. Each run is an activity (`sandbox 1`): its start carries
`command` and `limits` and is related to the turn or cron job that ran it
(`related`); its stop has the `duration`, and it fails when the backend could
not start the command (`error`). The command's environment gets the run's
activity (`TENANT_ACTIVITY_ID`). In between:

| Event | Level | When |
|---|---|---|
| `sandbox 1001` | INFO | the command exited: `pid`, `exit`, `status`, `timed_out`, `violations` when there are any, and `tail`, the last 512 bytes of the output (stdout and stderr together, as the model sees them) |
| `sandbox 2001` | WARN | the boundary refused a file or network access and the command was not re-run: enforce on, and the escalation was denied or the surface is unattended |
| `sandbox 2002` | WARN | `"enforce": false` (dry run): what enforcing would have refused; the command ran on the host |
| `sandbox 2003` | WARN | an approved `os_exec_unsandboxed` re-ran the refused command on the host |
| `sandbox 3001` | ERROR | the surface's backend is unavailable, so nothing ran (the sandbox fails closed) |

Every event carries `surface` and `backend`; a refusal also `command`,
`exit` and `violations`, what the boundary refused as the backend attributes
it (`filesystem: …`, `network: …`). `command` holds the first 300 characters
of the command. For an escalation or a dry run, `exit` and the exit event
describe the host re-run.

```
2026-09-22 14:05:10.412 WARN  [sandbox] The sandbox refused a command: filesystem: sandbox blocked access outside the policy's read/write roots	event=sandbox/2001 task=Refusal trace=… activity=… surface=discord backend=restricted command="cat ~/.ssh/id_ed25519" exit=1 violations="filesystem: sandbox blocked access outside the policy's read/write roots"
```

- On surfaces with an approval broker (TUI and dashboard, Discord, iMessage), the
  escalation prompt itself is an `approvals` line (`action=os_exec_unsandboxed`).
- `command` and `tail` are scrubbed (see [Secrets](#secrets)) before they are
  cut, so a secret typed inline does not reach the log.
- On Linux and Windows the backends attribute refusals only as far as the
  command's output shows them (a read-only file system, an unreachable
  network). An unattributed denial shows only as a non-zero `exit`.
- `tenant sandbox run`, the operator's trial run, prints its result and files
  nothing.

## The MCP logs

Each MCP connector and local server writes its own log, `mcp.<name>`, named
after its label (`mcp:github` → `mcp.github`, `mcp:mcp.atlassian.com` →
`mcp.mcp.atlassian.com`: lowercased, other characters as `-`, cut to fit).
The viewer groups them under Integrations › MCP. They are catalogued
([EVENTS.md](EVENTS.md#mcp)):

- **A connection** is an activity (`mcp 1`): it fails when the server can't be
  reached, refuses the sign-in or the MCP handshake, and stops when Tenant
  closes the session. A connector reconnecting at launch has
  `interactive=false`; one with no saved sign-in stops with `mcp 1003`, not a
  failure. `mcp 1001` lists the tools it came up with; `mcp 1002` is a server
  announcing that its tools changed. A call that could not reach the server
  is `mcp 3001`.
- **Sign-in:** a refresh (`mcp 1102`), an early refresh that failed while the
  token still works (`mcp 2101`), the sign-in dying, once, with the fix
  (`mcp 3101`), and coming back (`mcp 1103`). A URL no browser opened is
  `mcp 2001`. The feed line and operator DM for a dead sign-in still come;
  a DM that failed is `mcp 2103`.
- **A local server's process** is an activity too (`mcp 2`): its start names
  the sandbox backend and limits, and its connection, its own lines
  (`process started` with the pid, its stderr, `process exited`) and its
  environment (`TENANT_ACTIVITY_ID`) belong to it. It fails when the server
  could not start or exited on its own; one killed after ignoring its stdin
  closing is `mcp 2201`. An `mcp_servers` entry that was skipped is `mcp 2202`.
- **Changes:** a connector added (`mcp 5001`), removed (`mcp 5002`) or given a
  new trust (`mcp 5003`; `mcp 5004`, a warning, for `allow`). The audit
  records of these are the Security log's.

`tenant mcp-memory`, Tenant serving its own memory over MCP, writes the `mcp`
log (`mcpserver/…`).

## Child processes

A process Tenant starts is filed by Tenant, in the log of the subsystem that
started it (`internal/logs/capture`): a `process started` line with its `pid`,
a `process exited` line with `exit` (the code, when Tenant knows it),
`duration`, and the OS's words for a failure (`exit status 3`,
`signal: killed`) in the message. A failure is a warning, unless Tenant
stopped the child (`cancelled=true`: a shutdown, a cancelled turn).

| Child | Log | Tagged | Also filed |
|---|---|---|---|
| a local MCP server (`mcp_servers`) | `mcp.<name>` | `server=mcp:<name>` | `backend`, `limits`, `command` at start; its stderr, line by line; `killed="after the grace period"` when it ignored its stdin closing and was killed. See [The MCP logs](#the-mcp-logs) |
| `os_exec`, a cron shell job | `sandbox` | `surface`, `backend` | catalogued events instead of these lines: see [The `sandbox` source](#the-sandbox-source) |
| `claude -p` (the claudecode backend) | `model` | `child=claude` | its stderr, line by line, whether the call failed or not |
| Chrome (the web plugin) | `browser` | `child=chrome` | its output, line by line, so its crash text survives it. chromedp keeps Chrome's exit status, so the exit line says when it ended, not with what code |
| `tailscale` | `system` | `child=tailscale`, `args` | |
| the crm tool, `gcloud`, `ps`/`tasklist` | `plugin.crm`, `plugin.gsuite`, `plugin.os` | `child=<name>` | |
| `osascript` | `imessage` | `child=osascript` | |

**Output lines are data.** Each one is the message of a line whose log and
tags Tenant chose, marked `stream=stderr`, so a child can't write into another
log, or pass for one of Tenant's own lines, by printing something shaped like
one. A child's lines are rate-limited: a burst of 200, then 2 a second. What
the limit drops is filed as `suppressed N lines of stderr (rate limit)` when
lines flow again, and at exit. A line longer than 16 KiB is cut there and the
rest of it dropped.

**A local MCP server** also keeps the last 8 KiB of its stderr, which explains
a server that fails to start in the error `/enable` shows.

**Correlation.** A child's environment gets `TENANT_TRACE_ID` and
`TENANT_ACTIVITY_ID` when the work that started it has them (a turn's
`os_exec`, say), and its lines carry the same trace and activity. A script
that logs through `POST /api/logs` or `tenant log` joins them by passing them
on as its `trace` and `activity` fields
(`tenant log deploy "done" trace=$TENANT_TRACE_ID`).

**While it runs, a child is a component** (see
[What is running](#what-is-running-components-and-the-summary)), named by its
first tag (`server`, `child` or the sandbox's `surface`); it leaves the list
when it exits.

## The `model` and `route` logs

Both are catalogued ([EVENTS.md](EVENTS.md#model)) and sit in the viewer's
Model group.

- **`model`** is one log for every backend. Each event names its `profile`,
  `backend` and `model`, so `data.backend:vllm` picks one out. Every model
  call made through the router is an activity under its turn (`model 9001`:
  start, then stop with its duration, or fail), with its tokens in
  `model 9002`; both are diagnostic events. Operational events cover the rest:
  - a provider that can't serve a call (3001);
  - a failover (2001), which also stays in the status feed;
  - a slow model skipped by health gating (2002);
  - a `/model use` switch (5001), and a switched model that isn't ready (2007).
- **`route`** writes one event per routing decision, carrying the
  `decision_id` of its row in `route-decisions.jsonl` (1001, or 2001 when the
  turn fell back to the planner). `/route why` shows the recent ones in full.
  The privacy override is `route 4001` (armed) and `4002` (used).
- **Token usage.** Serve records the main agent's calls in `usage.db`, as the
  TUI does. A failed write is `model 3002`.

## Errors are not dropped

A background failure is logged where it happens (TEN-366). The ones that change
what the operator believes reach the operator too:

- **A choice that was not saved.** A permission change, an "approve always", a
  tool switched on or off, a relay, iMessage, peer, Tailscale or connector
  setting applies at once. When `settings.<agent>.json` or `config.json` cannot
  be written, the failure is logged (`config: save failed`, `settings save
  failed`) and the operator is told "… applies now but was not saved, so a
  restart will undo it": in the status feed, or as the command's own error
  where it has one.
- **A dashboard chat turn that failed** (a full turn queue, a model that will
  not resolve) is logged under `dashboard` and shown in the chat as
  `⚠ turn failed: …`. A turn you stopped is not a failure.
- **Discord and iMessage** write catalogued events (`discord/…`,
  `imessage/…` in [EVENTS.md](EVENTS.md)). The gateway records how each
  session ended and how it reconnects: a drop, a failed RESUME or IDENTIFY,
  an invalidated session or a missed heartbeat at WARN, a reconnect Discord
  asked for at INFO. Undecodable payloads are recorded, and a fatal stop fails
  the relay's connection activity. Every message the bot or the iMessage
  responder sends is a delivery activity, so a failed send is recorded
  whichever path sent it. A failing iMessage poll (every 3 s) is recorded
  once per error.
- **Stores and files:** tool stats, the nightly eval artifact, rotated OAuth
  tokens, the relay-inbox retention sweep and `peers.json` reads are logged; a
  research run record that could not be written is reported on the research
  progress. A failure that repeats on a timer (tool stats every minute, peer
  probes every 30 s) is logged once per streak.

Packages that may not import the log book (plugins, toolmux) log through
`slog.Default()` with a `source` attribute, which serve and the TUI point at
the hub.

## Crashes and unexpected exits

Serve and the TUI (never a one-shot command, so a Ctrl-C is never a crash)
keep a boot record in `logbook.db` and two folders under the root. The
folders hold records, not logs, so they never become tabs, and backups leave
them out:

- a row in `logbook.db`'s `boots` table: pid, role, version, host, start time,
  and a heartbeat refreshed every 30 seconds. An orderly stop marks it clean.
- `boots/<boot_id>.lock`: the process's boot lock, held for its lifetime.
- `crash/<boot_id>.txt`: the Go runtime's fatal output with every goroutine's
  stack (`debug.SetCrashOutput`, `SetTraceback("all")`). It still goes to
  stderr as well, but stderr is lost under the Windows task and drawn over the
  TUI's screen. An orderly stop deletes the file if it is empty.

A row with no clean stop whose lock can be taken belongs to a process that
has died. It is filed once as a critical `system` event, carrying the time of
the process's last event, and its files are cleared:

| Event | When |
|---|---|
| `system/4` `The previous run crashed: <first line of the dump>` | the runtime left its fatal output. The dump moves to `system/crash-<boot_id>.txt` (listed with the tab's files). The event carries its path in `file` and its head in `stack` |
| `system/3` `The previous run ended unexpectedly; its last event was at <time>` | no dump: the process was killed, lost power or called `os.Exit` |

Both carry `boot`, `pid`, `role`, `version`, `host` and `started`. v1 files
have no CRITICAL level, so these lines read `ERROR` and carry
`severity=critical`. Only the lock decides, never the heartbeat's age, so a
live process is never reported (for example after a laptop wakes), and a
crash file is read only once its process's lock is free. The check runs at
start and every minute in whichever process does the housekeeping, so a serve
crash while the TUI is open is reported within a minute. With the store off
(`"store": false`) or unavailable there are no boot records: an unexpected
exit is not reported, and a crash dump goes to stderr only.

A panic that a guard contains is logged with its stack. A tool that panics
fails only its own call: the model gets an error result, and the log book a
critical `agent: tool panicked` record. The other guards (routing, the spawn
classifier) log at ERROR, and a shutdown step writes `system 3020`. The
`route` guards write `route 3003`, and the `eval` route-task guard `eval/3001`.

## The dashboard

The **Activity** tab reads the hub: every source is a tab (built-ins first,
custom ones after, `system` last), with a retained-line count, an
**⚠ errors only** toggle, and — when a source is selected — its files on disk
with a raw plain-text view / download. History from before the current run is
there too: at start the hub re-reads the tail of every folder's newest file.
Every open tab updates live. After Tenant restarts, an open tab redraws itself
from the new process's lines when it reconnects. A source's tab stays until
Tenant restarts, even if its folder is deleted.

The **Logs** page (`/logs`) is where to investigate: the log book itself,
laid out like Event Viewer ([below](#the-logs-page)). The Activity tab links to
it, filtered to the source it shows.

`tenant attach [URL] --follow [--source discord] [--level error] [--trace ID]`
is the terminal equivalent, over `GET /api/activity`; `--trace` keeps one
turn's lines. Flags may come before or after the URL. After a Tenant restart
`--follow` says so and replays what the new process kept. With no URL, when
this account can open `logbook.db`, it reads the log book itself, as
`tenant logs query --follow` does ([below](#the-command-line-tenant-logs)).

## Adding a source (no rebuild)

Anything that names a new source creates its folder and its tab the first time
it writes. Pick whichever fits:

**A Go package inside the binary** — one line, no plumbing:

```go
var log = logs.For("myfeature") // tenant/internal/logs

log.Info("started", "port", 8080)          // → <data>/logs/myfeature/<day>.log + a "myfeature" tab
log.Error("sync failed", "err", err)
```

`logs.For` resolves the process-wide hub lazily, so it is safe at package init
and works in tests (records go to stderr at WARN until a hub is installed; a
one-shot command with a data directory files them as well). A
subsystem that already has a `*slog.Logger` can also just be handed
`log.With("source", "myfeature")`.

**A Go program outside the binary** (a sidecar, a watcher, a custom automation) —
copy the one stdlib-only file in [`reference/logging/tenantlog.go`](../reference/logging/tenantlog.go):

```go
log := tenantlog.New("deploy")                        // finds the hub via Tenant's config.json
log.Info("build started", "host", "build-box")        // live via the API, or the day's file when no hub is up
slog.SetDefault(slog.New(log.Handler()))              // optional: the program's whole slog output too
```

Tutorials on adapting it — defaults, slog, dashboard-styled events, file-only
mode, the hot-patch checklist for a live box — are beside it in
[`reference/logging/`](../reference/logging/).

**A script, cron job or sidecar on the box** — the CLI:

```bash
tenant log deploy "build started" host=build-box commit=abc123
tenant log deploy "build failed" --level error step=link
tenant log --list
```

It POSTs to the running hub (the line shows up live in open dashboards; the
TUI's feed does not show it); when no hub is reachable (or with `--file`) it
leaves the line in the spool, which the next Tenant process to run files into
the log book, tagged `external` (see [Intake](#intake-v1-history-foreign-folders-and-the-spool)).
With `"logs": {"store": false}` it appends to the day's file instead. The token is
`dashboard.auth` when config.json sets one, else the generated
`<config>/dashboard-auth-token` a default install uses (`--token` overrides).
Tenant's own sources (the folders in the tree above: `turns`, `approvals`,
`system`, …) are refused, hub up or not.

**Anything that speaks HTTP** — the live logging API (same Bearer token as the
rest of the dashboard). It refuses Tenant's own sources with HTTP 403 and drops
`ev` and `event` fields: only Tenant files turn events and catalogued events,
so an outside line can never pass for an approval, a tool call or a
`security` record.

```bash
curl -s -X POST http://127.0.0.1:8770/api/logs/watcher \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"level":"warn","msg":"disk at 91%","fields":{"host":"build-box","pct":"91"}}'

# or a bare text body:
curl -s -X POST 'http://127.0.0.1:8770/api/logs/watcher?level=error' \
  -H "Authorization: Bearer $TOKEN" --data-binary 'backup failed'
```

```powershell
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8770/api/logs/watcher" `
  -Headers @{Authorization="Bearer $env:TENANT_TOKEN"} -ContentType 'application/json' `
  -Body (@{level='info'; msg='snapshot ok'; fields=@{host=$env:COMPUTERNAME}} | ConvertTo-Json)
```

| Method | Path | What |
|---|---|---|
| `GET` | `/api/logs` | `{"root", "sources": [{name, native, count, last_at, err}]}` |
| `GET` | `/api/logs/{source}` | `{"source", "files": [{name, size, mod_time}]}` |
| `GET` | `/api/logs/{source}/{file}` | the raw file (`text/plain`) |
| `POST` | `/api/logs/{source}` | `{"level", "msg", "fields"}` — or a `text/plain` body (`?level=`) |
| `GET` | `/api/activity?since=&epoch=&limit=&source=&level=&trace=` | the retained lines as JSON (cursor replay), see below |
| `POST` | `/api/addon/event` | legacy add-on line (`{"source","text","level"}`) — files under the add-on's name (not one of Tenant's own) |

`/api/activity` returns `{"events", "cursor", "epoch"}`. Pass `cursor` back as
`since` and `epoch` as `epoch` for a gap-free tail. With `logbook.db` open,
the cursor is the store's seq and the epoch its ID, so a cursor survives a
restart; a `since` from another epoch (or none) replays the newest 10,000
events. Without the store, sequence numbers start at 1 again when Tenant
restarts, under a new epoch, and a `since` from another epoch replays
everything the hub kept. A `since` with no `epoch` keeps its old meaning.
With `limit=N`, at most N events come back and `cursor` is the last one's, so
the next call continues after it. The log book's own API is in
[The API](#the-api).

**A process that only writes files** — drop a folder under the root and append
to a `.log` file in it (`<root>/mytool/2026-09-22.log`). The hub discovers new
folders within ~5 s, seeds the tab from the file's tail, and follows it live.
Lines in the format above keep their level and fields; any other text is kept
verbatim as an INFO line. The folder name is the source name
(`[a-z0-9][a-z0-9._-]{0,39}`, not ending in a dot and not a Windows device
name such as `con`, `nul`, `com1` or `lpt1.log`, since it is a folder on every
OS). With the store on, the housekeeper also files the folder's lines into
`logbook.db` as the log `ext.<folder>` (see [Intake](#intake-v1-history-foreign-folders-and-the-spool)).

**A turn event from a new agent surface** — set `Source` on the events you
mirror into the feed, as the cron / Discord / eval observers do:

```go
obs := func(e agent.Event) {
    e.Agent, e.Source = "tenant-myrelay", "myrelay"
    emit(e)
}
```

## Catalogued events (logging v2, in progress)

Logging v2 (`docs/design/logging-v2.md`) gives every event an ID, documented
once in a catalog. The event model, `internal/logs/event`, imports nothing
else from Tenant, so any internal package can emit where the action happens:

```go
var Cron = event.NewProvider("cron", "cron") // a provider writes one log
var JobFailed = Cron.Event(3001, event.Error, "Job run", "Job {job} failed: {err}", "job", "err").
    Doc("A scheduled job failed.", "Its command or prompt returned an error.", "Open the job's run in the cron log.")

JobFailed.Log(ctx, "job", name, "err", err)       // never blocks
ctx, act := event.Start(ctx, JobRun, "job", name) // start … stop (with duration) or fail
defer act.End(&err)                               // End(&err, "status", code) adds what only the end knows
```

- **Definitions** are checked when they register (at package init): an ID is
  unique in its provider, and the template names only declared fields that
  aren't sensitive. The built-in ones are in `internal/logs/catalog`.
- **In the text files,** an event is a v1 line in its log's folder. The
  event's ID, task, opcode and correlation IDs come first among the fields:
  `event=cron/3001 task="Job run" trace=… activity=…`. A critical event is
  written as `ERROR` with `severity=critical`, and a sensitive field as
  `«sensitive»`, never its value. The store keeps the typed values.
- `Audit` is for records that must not be lost: it returns once the record is
  committed to `logbook.db`, with the commit's error. With the store off it
  returns once the line is written.
- **Plugins** each write their own log, `plugin.<id>`, under Integrations ›
  Plugins: started or could not start, the operator's enable and disable,
  token refreshes, and their API's rate limits, refused credentials and
  failed calls. The catalog comes with the plugin's descriptor
  ([PLUGINS.md](PLUGINS.md) §5). The tool mux's own events (peers that did
  not come back at launch, unsaved tool stats) are the `toolmux` log's; an
  MCP connector's are its own log's ([The MCP logs](#the-mcp-logs)).

### The store: `logbook.db`

`serve` and the TUI also keep every event in `<logs root>/logbook.db`, one
SQLite database (WAL, `synchronous=FULL`, `fullfsync` on macOS). Each writes
through its own connection, so both can share a data directory.
The store is always on: `"logs": {"store": false}`, v1's rollback switch, is
ignored since TEN-378 and draws one `logbook` warning at start. The log book
is the first thing a process opens and the last it releases: its final flush
(bounded at 2 s) is the last shutdown step, and the store closes after the
shutdown's own last events.

- **The write path.** An event joins one queue (64k events); one writer
  goroutine takes a batch every 100 ms or 512 events, scrubs it, caps its
  values, appends its operational events to the text files, then commits the
  batch, and only then feeds the Activity tab. The text files are written
  first, so a batch the store loses (a crash mid-commit, a failed write) is
  still in them.
- **When a commit fails** (a full disk, a lock held past its timeout, an I/O
  error) it is retried after 100 ms and 200 ms, each failure a `logbook/3001`
  error. After the third the process runs on the text files alone: a critical
  `logbook/2` and a `⚠ log book:` line in the status feed (the TUI's feed,
  serve's stdout, the `system` log; `Hub.OnAlert`, `Hub.Degraded`). It keeps
  trying, backing off to every 30 seconds; the first commit that succeeds
  ends the outage (`logbook/3`, `✅`) and imports the text files' lines from it
  with keyword `recovered` (`logbook/1001`), so the store has no hole. A
  `security` record's `Audit` returns the error meanwhile.
- **A corrupt database** found at open is moved aside, with its `-wal` and
  `-shm`, to `logbook.db.corrupt-<time>`, and an empty store with a new ID
  starts (critical `logbook/4`). Every process holds `logbook.lock` shared
  while it has the store open; the move takes it exclusively, so a process
  that finds the file corrupt while others have it open moves nothing (that
  fails on Windows and corrupts the file on POSIX): it runs on the text files
  alone and says so (`logbook/5`). A restore's re-identify takes the lock
  exclusively too. `store.Salvage` (for `tenant logs recover`) copies what
  can still be read from the moved file into the store, keyword `recovered`.
  The housekeeper runs `quick_check` once a day (`logbook/1002`, or critical
  `logbook/3004`).
- **When the queue is full,** an operational event waits up to 1 ms and a
  diagnostic one not at all, then it is dropped and counted per log
  (`Hub.Summary`). A `security` event is never queued: it forces its batch to
  commit.
- **Rate limits.** Each provider and event ID may write 100 events a second,
  in bursts of 500; the rest are suppressed. An uncatalogued line (ID 0) is
  limited by its provider and its template, the message with numbers,
  words holding a digit and quoted values taken out, so one chatty line
  can't silence the rest of its provider. `security` is never limited.
  `"logs": {"rate_limit": {"per_second": 100, "burst": 500}}` changes it
  live (`per_second: -1` lifts it).
- **Self-monitoring.** Once a minute each process reports what it dropped
  and suppressed, per provider (`Suppressed 1203 × mcp.github 3001 in the
  last 1m0s`, `logbook/2002` and `2003`), adds them to each log's `dropped`
  and `suppressed` counters in the store (`store.Logs`), and reports its
  queue's high-water mark when it passed half the queue (`logbook/2001`). A
  failed text-file write is `logbook/3002`, once until writes succeed again.
  A jump of more than 2 seconds between the wall clock and the monotonic
  clock is a `system/2001` warning. The full list is the `logbook` provider in
  [EVENTS.md](EVENTS.md).
- **Diagnostic events** (`.Diagnostic()` definitions) go to the store only.
- **Reading it.** `store.Since(cursor)` returns every event after a cursor,
  `<store ID>.<seq>`, in commit order, so a tail never misses a row. A cursor
  from another database (restored or rebuilt) starts again from the beginning.
- **Backups** (`tenant backup`) take `logbook.db` with `VACUUM INTO` on a
  fresh connection, never by opening the raw file (on POSIX that would drop
  the locks the backing-up process holds on it), plus `archive/`. The text
  files hold the same events and are left out unless `--include-mirror` is
  given. A restore brings `logbook.db` back with a new store ID, so live-tail
  cursors start over, and writes a re-anchor record into its Security log (see
  "The Security log's chain"). A log book
  moved outside the data dir by `logs.dir` is not in the backup, and `create`
  says so.
- **Privacy** (`tenant privacy forget --session …`, or any forget that removes
  runs) removes the session's events and the events of its runs' trace IDs
  from every log but `security`, where the record is tombstoned instead: its
  message becomes `«deleted by a privacy request»` and its data and salt are
  cleared, while its record number and hashes stay so the chain verifies.
  The deletion zeroes freed pages (`secure_delete`), optimizes the full-text
  index and truncates the WAL. The matching lines of the text files are
  blanked in place (overwritten with spaces), so a process appending to the
  file loses nothing; archives holding the events are rewritten. The preview
  lists what goes per log, with samples. `tenant privacy export` writes every
  event to `logbook.jsonl`, sensitive fields included. `tenant privacy status`
  counts the backups made before the last forget that still hold what it
  removed. A running Tenant's Activity tab keeps what it already read until
  it scrolls past.
- The file is owner-only. The viewer arrives with the next logging v2
  tickets.

#### Intake: v1 history, foreign folders and the spool

The housekeeper (whichever process holds its lease) files, every tick, what
no Tenant process wrote through its pipeline. Nothing it files is mirrored.
It never reads a folder into `security` or a log under `tenant.*`.

- **v1 history, once.** The lines of Tenant's own folders written before the
  store's first event are imported as events of their log, tagged `imported`
  and scrubbed on the way in (v1's `system` files hold the dashboard token).
  An `event=` claim is dropped. Progress is kept in the store's `meta`
  table, so a run cut short resumes and files nothing twice; `logbook/1101`
  says when it is done.
- **Foreign folders.** A folder is foreign only when no Tenant log has its
  name: not in the store, not a built-in or registered provider, not written
  by the running process. Its lines become events of `ext.<folder>`, tagged
  `external`, with any `event=` dropped. Each file's offset is kept in `meta`.
  A byte is read only once it was there at the previous tick: a process
  mirrors a line before it commits it, so another process's new log could
  look foreign for that moment, and is never filed as external.
- **The spool,** `<logs root>/spool/`, for writers when no Tenant process is
  up. A writer creates `<name>.tmp`, writes one event per line in the event's
  JSON form (`{"time":…,"log":"deploy","level":"warning","msg":"…","data":{…}}`;
  `logs.Spool` in Go), closes it and renames it `<name>.ndjson`. The
  housekeeper claims a file by renaming it `<name>.ingesting`, files it and
  deletes it. A rename that fails (on Windows, while another process has the
  file open) is retried at the next tick, and a `.tmp` is never read. What it
  files is tagged `external` and never catalogued (its `id` is dropped). An
  event for `security`, a log or provider under `tenant.*`, a built-in or a
  registered provider is refused, and so is a malformed line; `logbook/1102`
  counts them per file.

#### Retention and disk budgets

Every log has a policy: a maximum size, a maximum age, what happens when it is
full, a minimum level and an enabled switch. The defaults, by class:

| Log | Max age | Max size | When full |
|---|---|---|---|
| `security` | 365 days | 256 MB | archive |
| `system` | 90 days | 128 MB | overwrite |
| any other log | `retention_days` (30) | 64 MB | overwrite |
| diagnostic events, all logs together | 7 days | 256 MB | overwrite |
| the whole book, `logbook.db` plus its WAL | — | 2 GB | shed diagnostic, then the oldest events of overwrite logs |

```json
"logs": {
  "retention_days": 30, "budget_mb": 2048,
  "diagnostic": { "max_mb": 256, "max_age_days": 7 },
  "security":   { "max_mb": 256, "max_age_days": 365, "witness": "", "on_failure": "continue" },
  "per_log": { "mcp.github": { "max_mb": 32, "max_age_days": 14, "min_level": "warning", "on_full": "overwrite" } }
}
```

- `0` or a missing key keeps the default; `-1` lifts a limit. `min_level` is
  `critical`, `error`, `warning`, `info` or `verbose`; `on_full` is
  `overwrite`, `archive` or `keep`. Edits apply with no restart: to writes
  within 30 seconds, to retention at its next pass. A policy set on the log
  itself (`store.SetPolicy`, for `tenant logs config` and the dashboard) wins
  over `config.json`.
- **Size** is measured on disk, database plus WAL, and a log's share is
  apportioned by its events' summed payload.
- **When full:** *overwrite* deletes the oldest events; *archive* first moves
  them to `<logs root>/archive/<log>-<first seq>-<last seq>.ndjson.gz`
  (gzip, one JSON object per line, every column, `data` and `keywords` as
  their stored text;
  the Security log's are named by record number, taken oldest record first,
  and end with a `{"rec":…,"hash":…}` line);
  *keep* deletes nothing and writes one `logbook` warning each time the log
  passes its limit. A log reaching 80% and 95% of its limit is a warning,
  once per crossing, and a full one says what was done: its oldest events
  overwritten, with how many, or archived, with the file. The Security log is always written in full and never
  overwritten: its records are archived before any is deleted, the whole-book
  budget never sheds it, and a book left over budget by records it may not
  delete raises one critical `logbook` event.
- **The housekeeper.** Serve and the TUI each run a loop every 30 seconds.
  Whichever holds the `housekeeper` lease in `logbook.db` (renewed every 30
  seconds, taken over within a minute of its holder dying) runs retention every
  5 minutes: it deletes in transactions of at most 500 events (each holds the
  write lock for well under 50 ms), then runs `incremental_vacuum` and
  `wal_checkpoint(TRUNCATE)`. It also checkpoints whenever the WAL passes
  64 MB. Rollups older than the longest max age of any log go too. Other
  tickets add their tasks with `Hub.Housekeep(name, every, fn)`; a task's
  error is a `logbook/3005` error, never fatal. The lease changing hands is
  `logbook/1007` and `1008`.
- **Low disk.** Below 1 GB or 5% free on the data volume, diagnostic events
  are not written; below 256 MB, only warnings and worse. The Security log
  always writes. Shed events count as dropped (`Hub.Summary`), and crossing
  either line writes a `logbook` warning.
- **The timed debug switch.** Each log has one (`logs.debug_until_ns`, set
  by `tenant logs debug` or the API's `debug_for`, at most a day). While it is
  on, the log's verbose events are written past the `logs.level` floor and
  past the log's own minimum level; when it ends they stop again. Every
  process reads it at its next housekeeper tick (the one that set it, at
  once). With `logs.level: debug` every log's verbose events are written
  anyway.
- `runs.db` keeps its own retention (`runs.max_age_days`, `runs.max_count`).

#### The Security log's chain

Every `security` record is linked, in the transaction that commits it, into a
keyed hash chain (design §10.2):

- **The columns.** `rec` is the record number, the chain head's (in `meta`)
  plus one, with no gaps; `salt` is 16 random bytes; `data_hash` is
  `SHA-256(salt ‖ str(msg) ‖ str(data) ‖ str(dtypes))`; `hash` is
  `HMAC-SHA256(key, previous hash ‖ header ‖ data_hash)`, where record 1's
  previous hash is 32 zero bytes. `str(s)` is a 4-byte big-endian length and
  the bytes, a number is 8 bytes big-endian, and the header is `rec, time_ns,
  log, provider, event_id, version, level, channel, task, opcode, keywords,
  trace_id, activity_id, related_id, session_id, approval_id, tool_call_id,
  actor, surface, pid, role, boot_id, host, proc_version`, in that order, as
  stored (the `internal/logs/store` package doc has it exactly). Only `seq`
  and `bytes` are left out.
- **The key** is `<config>/logbook.key`: 32 random bytes, created once
  (owner-only; a current-user ACL on Windows, where it is also sealed with
  DPAPI). It is a managed secret: doctor audits it, the file tools won't read
  it, and only an encrypted backup carries it. Without the key a batch holding
  a security record is refused (`store.ErrNoChainKey`), and `Audit` returns
  the error; a key that can't be read or created is a critical `logbook`
  event at start.
- **The anchor.** Every 1,000 records, or every hour when the head has moved,
  the housekeeper writes the head to `<config>/logbook.anchor`
  (`{"rec":…,"hash":…,"time":…}`, atomic, owner-only), outside the data dir a
  rollback would replace. It moves the anchor on only from a head the chain
  still holds; if the chain has lost it (truncated, or an older copy put
  back), the anchor stays and one critical `logbook` event says so. The key
  and the anchor are deny roots of every sandbox.
- **The witness** (`logs.security.witness`) also gets the head at each
  anchor: `"discord"` DMs it to the operator, and an absolute path gets the
  anchor's JSON (best a file on another machine). `""` is none, the default;
  `peer:<name>` is not supported yet. Only a witness catches a rewrite by
  something holding the operator's own privileges.
- **A restore** (`tenant backup restore`) writes a re-anchor record
  (`logbook/4001`: the restored head, the anchor it replaced, the backup's
  name) and moves the anchor and the witness to it, so it verifies as a
  restore, not as tampering.
- **Verification** (`store.Verify`, for `tenant logs verify security`) walks
  the archives, oldest first, then the database, then checks the anchor and a
  file witness, and reports the first break by record number: an edited,
  deleted, reordered or forged record, a record never chained, an archive cut
  short, or a chain that ends before (or forks from) the anchor. A privacy
  tombstone verifies (its `data_hash` stays; its message can't change), and
  a witness still naming the head a re-anchor record replaced is a restore
  (the anchor itself moves with the restore). The whole walk is one read
  snapshot.
- **An audit export bundle** (`store.ExportSecurity`) is the log with its
  chain, optionally a time window: a header line (format, schema, Tenant
  version, time, host, store ID, filter, the head before the first record,
  the anchor), the records as archive lines (sensitive fields included: the
  data hash commits to them), and a closing `(rec, hash)` line.
  `store.VerifyBundle` checks it offline with the key.

#### What the Security log records: approvals, execution and access

Each record is written where the action happens, synchronously, before the
action returns (design §10.1; `internal/logs/catalog/security_access.go`, IDs
in [EVENTS.md](EVENTS.md#security)). Every Security record carries who acted
and where as the event's own `actor` and `surface`: a call passes them as
`"actor", …, "surface", …`, which set those attributes rather than data
fields (a template may still name them). The approval is the event's
`approval`, from the context (`event.WithApproval`), and the turn's `trace` is
filled in as for any event. Keywords are `audit.success` or `audit.failure`.

- **Approvals** (`internal/approval`): requested (`4101`), granted once, for
  the session or always (`4102`), denied (`4103`), denied at shutdown
  (`4104`), expired (`4105`), each with `category`, `action`, `detail`
  (scrubbed) and `origin` (the surface that raised it). `via` says what
  decided: `operator` (the actor names who: `operator` on the TUI or the
  dashboard, `discord:<user id>` on a Discord card, with the card's `nonce`;
  the operator on an iMessage reply, with its nonce), or a standing setting
  (`mode`, `session`, `rule`, `lock`, `cron`), or `no_surface`. Every broker
  path writes them, prompts and policy verdicts alike; so do the cron-exec
  auto-approver (origin `cron`), peer pairing (category `pairing`, asked by
  the `peer`) and a click on an external approval link (category `external`,
  approval `ext:<ns>:<id>`; its marker line names the clicker too). A
  category's mode changed from `/permissions` (and the relay's and iMessage's)
  or the dashboard's Access page is `4106`.
- **Execution** (`cmd/tenant/securitylog.go`): every command run through the
  exec seam, `os_exec` and cron shell jobs (`4201`: the scrubbed command, the
  backend, the exit code; its approval is the one that let it run: `osys`
  opens a slot the approval gate fills). A sandbox refusal is `4202`, a
  command run outside the sandbox after one `4203` (`why`: `approved` or
  `dry_run`), and the Discord relay's exec mode turned on or off `4204`
  (cron-exec's is `4704`, below).
- **Access:** dashboard sign-in and sign-out (`4301`, `4302`); a request
  refused (`4303`, `reason`: `token` for a wrong dashboard or peer token,
  `sign_in`, `csrf`, `host`, `origin`; a request with no token at all is only
  in the dashboard log); a peer record gaining a live token (`4311`: a
  pairing either way, or an invite code minted) or losing it (`4312`, `how`:
  `remove` or `revoke`).
- **When a record can't be written** (design §10.3), the log book files a
  critical `system/4001` for each lost record, raises one banner on the
  status feed (`Hub.OnAlert`) until a record commits again (then
  `system/4002`, and a second line), and applies `logs.security.on_failure`, read at each failure so an
  edit applies at once: `continue` (the default) lets the action go ahead;
  `deny` refuses every gated action (an approval, prompted or not, the
  cron-exec approver's, a pairing, an external approval) until records are
  written again. Code that gates an action writes its record with
  `Def.Gate`, which returns an error only then (`event.ErrRefused`).

#### What the Security log records: changes

Each record below is written through `Def.Audit` by the code that makes the
change, before it returns (design §10.1; `docs/EVENTS.md` has each ID). It
names who made the change, `actor` (`operator`, `agent`, or `tenant` for a
scheduled sweep), and from where, `surface` (`tui`, `dashboard`, `cli`, or,
for the agent, its turn's surface: `local`, `discord`, `imessage`,
`cron-exec`); an agent's change also carries its turn's `trace_id`.

- **Configuration** (`security/4501`): every save of `config.json` or
  `settings.<agent>.json` that changes something, whoever saves it (the
  record is filed by `internal/config`'s `Save`). `diff` has one line per
  changed setting, `path: old → new`; a secret value (`dashboard.auth`, a key
  ending in password, secret, token or api_key, every `env_set` value) is
  shown as `«secret:<path>»`.
- **Secrets** (`4601` stored, `4602` changed, `4603` deleted), by ID, never
  the value, where the operator's action is handled: the Keys page,
  `/configure`, `/model add-cloud` and `remove` (and the Model page),
  `/skill configure` and `clear`, `tenant setup`, `tenant model add|remove`,
  `tenant privacy forget --connector`. `internal/secrets` doesn't log; token
  refreshes a plugin makes on its own aren't recorded.
- **Scheduled jobs** (`4701` added, `4702` enabled or disabled, `4703`
  removed, `4704` the cron-exec switch): from `/cron`, the dashboard, and the
  agent's `cron_*` tools, which ask nobody (audit item D4).
- **Data lifecycle**: `4801` a privacy deletion (forget, prune, and the
  retention sweep as `tenant`), with what it removed; `4802` a privacy
  export; `4803` a backup written; `4804` a backup restored, after the
  re-anchor record.
- **The log book**: `4901` cleared, `4902` reconfigured, `4903` exported,
  `4904` a sensitive field revealed, `4905`/`4906` a verify passed or failed.
  Verifying the Security log and its audit export bundle write theirs;
  `catalog.AuditLogAdmin` is the call for the rest.

A one-shot command (`tenant backup`, `tenant privacy`, `tenant setup`,
`tenant model`) files its events into `logbook.db` when it ends, so its
records join the chain. Every other one-shot command (`tenant route`,
`tenant peer`, …) opens `logbook.db` when it writes its first Security
record (`logs.AuditEmitter`), so its changes are recorded and chained too.

The v1 `approvals` folder keeps being written through this release while
approvals move into `security`; it goes in the next.

#### Health checks: `tenant doctor` and `tenant security audit`

Both open `logbook.db` read-only (`store.Options.ReadOnly`): they create,
tighten and migrate nothing there, and read beside a running serve without
holding it up.

- **`tenant doctor`** adds six `log book …` checks: its **location** (a
  `logs.dir` outside the data dir is left out of `tenant backup`), its
  **integrity** (`quick_check` and the schema version), its **size** (the
  database and WAL against `logs.budget_mb`, and the fullest logs against
  their limits; WARN over the budget, or a `keep` log past its limit), who
  holds the **housekeeper** lease (role, pid, host and boot ID from `boots`;
  WARN when it has expired while a process that keeps the log book runs),
  **mirror-only** processes, and the **losses** (each log's dropped and
  suppressed counters since the store was created). A process running on its
  text files alone leaves `<logs root>/boots/<boot id>.mirror-only` (why and
  since), removed once `logbook.db` is written again or it exits; its boot
  lock says whether it still runs (one whose store never opened holds the
  lock itself while marked). A marker whose process died is a WARN, and
  `--fix` clears it. The private-data permission check, and its `--fix`, now
  cover a relocated `logs.dir` too, unless its root holds the home, data or
  config directory. `logbook.key` and `logbook.anchor` join the config
  files `--fix` tightens.
- **`tenant security audit`** adds a `logbook` section and the log book's
  files to `file_protection`: `logbook.db`, `-wal`, `-shm`, every folder of
  the root (the text files, `archive`, `spool`, `crash`, `boots`) and the
  anchor (the key is checked with the managed secrets). On Windows they are
  `inherited`: `fsperm` is a no-op until TEN-325, so they carry only the data
  directory's ACL, a warning. The section reports the **scrubber** (on, how
  many known values it holds and how many are under its 8-character floor,
  never the values), a **secret scan** (every text file and the newest
  10,000 events of `logbook.db`, searched for the known values in every
  encoding the scrubber matches, reported by the credential's name with line
  numbers or seqs, never the value; a warning), the Security log's
  **verify** result with the time it ran (`ok`, `broken`, a critical
  finding, `unverified`, `no_key` or `no_store`; the audit files no event of
  its own), and the **witness** (none, `discord`, a file, or a setting that
  reaches no witness, a warning).

### Turns, sub-agents and research

- **Turn-event lines** (the ones with `ev=`) name their catalogued event:
  `event=turns/<id> task=…`; bus messages and the team budget are
  `subagents/…`. Approvals, inbound messages and add-on lines carry none yet.
- **A turn is an activity:** its `turn started` line has `opcode=start` (and
  `related=` the spawning turn, for a sub-agent's); a `turn finished` or
  `turn failed` line (`ev=turn_end`, `opcode=stop` or `fail`, `duration=`)
  ends it. Its correlation is its `trace`.
- **Tool calls:** the `tool call` and `tool result` lines are the payload
  records (diagnostic channel, `args` and `result` sensitive; until the store
  lands, these files and the Activity tab still show them in full). Each call
  also writes an operational `Tool <name> finished in <d>` or `failed after <d>`.
- **Where an agent's events land:** in its surface's folder, as its logger
  names it (`logs.SourceOf`): a sub-agent's in `subagents`, a cron job's in
  `cron`, the main agent's in `turns`. `event.WithLog(ctx, provider, log)` is
  what re-files one provider's events under a context.
- **Sub-agents and research passes** are activities too: `subagents/1`
  (related to the spawning turn, naming its own turn's trace as `run`) and
  `research/1` (its sub-agents are related to it). The turn gate logs each
  turn it refuses (`turns/2201`).

### Finding events: the filter language

Every viewer takes the same filter (`internal/logs/query`). Terms are
separated by spaces and must all hold; values in a list are alternatives;
a leading `-` negates a term:

```
level>=warning log:discord id:2001-2999 -id:2005 since:24h "reset"
provider:mcp.* level>=error          every MCP server's errors
data.tool:os_exec "permission denied"
since:14:00 until:14:10              a time slice across every log
boot:current level>=warning          this run's problems
```

| Field | Values |
|---|---|
| `log` `provider` `task` `opcode` `tag` `session` `approval` `tool_call` `actor` `surface` | a name, or a glob with `*` and `?` |
| `trace` `activity` `related` `boot` | a whole ID; `boot:current` is the run of the process reading the store (serve, for the dashboard) |
| `id` `pid` | numbers, ranges (`2001-2999`) and comparisons (`id>=3000`) |
| `level` | `critical` > `error` > `warning` > `info` > `verbose`: `level>=warning` is warning, error and critical |
| `channel` | `operational` or `diagnostic` |
| `since` `until` | a duration back from now (`15m`, `2h`, `7d`, `2w`) or a local time (`14:03`, `today`, `yesterday`, `2026-09-27`) or RFC 3339. `until` is exclusive. |
| `data.<key>` | a field of the event's data: text, a glob, or a number, which also compares (`data.iter>=3`). A dotted key is a flat field (`data.req.method`) or a path into embedded JSON. A sensitive field never matches, so a filter can't read a masked value. |

Operators are `:` `=` `!=` `>=` `<=` `>` `<` (`:` and `=` are the same;
comparisons are for `id`, `pid`, `level` and `data.<key>`). A bare word or a
`"quoted phrase"` searches the message (full text, case-insensitive; the data
isn't searched, so a masked field stays masked). A word shaped like `name:`
must be quoted. Inside quotes, `\"` is a quote and `\\` a backslash. A filter
that doesn't parse says where and what was expected (`*query.SyntaxError`).

The store runs a filter: `Query` returns a page and the cursor for the next,
oldest first or newest first; `Count`; `Stats` counts by time, log and level,
from the per-minute rollups when the filter is only log, level and time
(a bound inside a minute counts that whole minute); `TopIDs`; `Timeline`
returns everything of one trace or activity across all logs, following the
activities it started (`related`) down a few levels; `EventStats` answers
"has this happened before?" with the count, first and last seen and a 7-day
trend. On 1M events (`TENANT_LOGBOOK_PERF=1 go test ./internal/logs/store -run
TestQueryBudgets -v`) a page takes well under the budgets: 200 ms on indexed
fields, 1 s for `data.<key>` and full text. `Count` over a field without an
index reads every row it can't rule out, so it can take longer.

### What is running: components and the Summary

**Components** (`internal/lifecycle`) are what a serve or TUI process runs:
each with a name, a kind (`subsystem`, `connector`, `server`, `child`, `job`),
its log, a state (`starting`, `running`, `degraded`, `stopped`, `failed`),
since when, when this run started (its uptime) and its last error. They are
tracked where they start: `Group.Start` registers a stop that also tracks
(cron, the improve scheduler); `lifecycle.Track` covers the rest (the
dashboard, the Discord relay, the iMessage responder, degraded while chat.db
can't be polled; the peer listener; the peer heartbeat, degraded while
`peers.json` can't be read; the settings and credentials watchers; the turn
gate); and `internal/logs/capture` tracks each child while it runs.
`lifecycle.Components()` lists them, JSON-ready, for the Components view and
`GET /api/logbook/components`.

Every state change is a `system` event: `system/101` starting, `102`
running, `103` stopped, `2101` degraded, `3101` failed, and `2102` for a
child that failed (a warning, as its exit line is: a command's non-zero exit
is often its answer). Each run of a component is one activity, so
`activity:<id>` is its history, and
`provider:system task:Component data.component:"discord relay"` every run's.

**The Summary** (`store.Summary`; `Hub.Summary` adds the retention settings
in force and the events this process's queue dropped) is, per log, the
counts by level over the last hour, 24 hours and 7 days, exact to the minute,
with a sparkline of the week's 168 hours; its size against its limit, its
retention and what happens when it is full, its dropped and suppressed
counts and its newest event. Then the 10 event IDs most frequent among
warnings and worse over the last 24 hours (in whole hours), and the latest 10
critical events, errors and unexpected exits (the `system` log's critical
events) of the week. It reads per-hour rollups: `rollup_hour` (log and
level) and `rollup_id` (event ID, warnings and worse), which each commit keeps
beside the per-minute ones, retention prunes with them and a privacy forget
takes its events out of. On 1M events it takes 14–20 ms (p95; the budget is 50;
`TENANT_LOGBOOK_PERF=1 go test ./internal/logs/store -run TestSummaryBudget -v`),
where a week from the per-minute rollups alone took 135 ms.

### The Logs page

The dashboard's **Logs** page (`internal/dashboard/ssr_logs.go`) reads the
store through the functions the API's handlers call, and renders on the
server like every page; the grid's next pages, an event's detail, the match
count and the live tail arrive as Datastar patches.

- **The tree:** Summary at its root; Custom Views (the four built-ins and the
  saved ones); Tenant's logs (System, Security, Turns, Sub-agents, …); the
  subsystems where `catalog.Group` puts them (Channels, Scheduler,
  Integrations › MCP and Plugins, one entry per server or plugin, Model,
  Memory, Sandbox, …); External; and Components. A log appears once it is
  written to, with its warnings and worse of the last 24 hours.
- **The grid:** level, time, log, provider, event ID, task and message, newest
  first, 100 rows a page; scrolling to the bottom fetches the next page by
  cursor. The count ("12,345 events; 231 match") is fetched after the page.
  **live** tails the filter: each new match is added at the top, from the
  page's own subscription to the store, and a reconnect resumes from the last
  row it got.
- **An event's detail** (click a row; `/logs/event/{seq}` is its permalink,
  and opens the page with it selected in its log): **General**, the message,
  the time in local time and UTC, its facts, and every correlation ID as a
  link to the events that share it; **Details**, every field and the raw JSON,
  sensitive values masked until **Reveal**, which files a Security record
  (`security/4904`); **Correlated**, the timeline of its turn (or activity)
  across every log, with the turn's run record from `runs.db` (its tool calls
  and approvals); **Help**, the catalog entry and how often the ID was seen.
- **Actions:** the filter (a parse error shows beside it, with its column);
  Find, which adds quoted text to the filter; save the filter as a view;
  export what it matches (NDJSON, CSV or text, through
  `/api/events/export`, without sensitive values); and, when the filter names
  one log, its Properties (maximum size and age, when full, minimum level,
  enabled, reset, debug for 30 minutes) and Clear, which offers the export
  first. Each change and each clear files its Security record; the Security
  log is never cleared.
- **Summary** and **Components** show what the two sections above describe.

The discovery questions of the design (§9.1: what is running, what went
wrong recently, what happened at 14:03, why did this fail and what else did
that run do) are handler tests, each answered in at most three requests
(`TestLogsDiscoveryDrill`), and `TestLogsPageInChrome` drives the page in
Chrome when it is installed.

### The API

The dashboard serves the log book over HTTP (`internal/logs/logapi`), behind
its bearer token and request limits. `tenant logs` runs the same handlers in
its own process when it can open `logbook.db`, so the two have the same
features.

| Method | Path | What |
|---|---|---|
| `GET` | `/api/events?q=&cursor=&limit=&reverse=&count=` | `{"events", "next", "restarted", "count"}`: up to `limit` (100, at most 1000) matches after `cursor`, oldest first, or before it newest first with `reverse=1`; `count=1` adds how many match in all |
| `GET` | `/api/events/{seq}` | `{"event", "spec", "help", "stats"}`: the event, its catalog entry and Help (Markdown), and `{count, first, last, trend}` |
| `GET` | `/api/events/stream?q=` | Server-sent events: each message's `id` is the event's cursor and its data the event. It resumes after `Last-Event-ID` (or `?cursor=`) with no missed row; with neither it starts at the newest event. A cursor from another store sends `event: restart` and starts over. A comment line every 15 s keeps it open. |
| `GET` | `/api/events/stats?q=&bucket=` | `{"buckets": [{start, log, level, n}]}`; `bucket` is a duration (`15m`, `1h`, `1d`), none counts all time |
| `GET` | `/api/events/top?q=&n=` | `{"top": [{provider, id, n}]}`: the most frequent event IDs |
| `GET` | `/api/events/timeline/{id}?depth=` | `{"id", "events", "run", "run_note"}`: a trace's or activity's timeline across every log, with the trace's `runs.db` record, or why there is none |
| `GET` | `/api/events/export?q=&format=ndjson\|csv\|text` | every match, oldest first, after a header record (below) |
| `POST` | `/api/events` | one event (JSON) or a batch (NDJSON, `Content-Type: application/x-ndjson`) from outside: `{"accepted": n}` |
| `GET` | `/api/logbook/logs[/{log}]` | `{"logs": [{name, group, events, bytes, policy, last_write, dropped, suppressed, debug_until, cleared}]}`, or one log |
| `PATCH` | `/api/logbook/logs/{log}` | `{max_bytes, max_age_s, on_full, min_level, enabled, debug_for, reset}`, each optional: the log's own policy (it wins over `config.json`; `reset` returns to it) and its debug switch (`"30m"`; `"0"` turns it off). `{log}` may name a provider. Answers the log. |
| `POST` | `/api/logbook/logs/{log}/clear` | `{"log", "cleared": n}`; the Security log is never cleared (403) |
| `GET` `POST` `DELETE` | `/api/logbook/views[/{name}]` | `{"views": [{name, filter, builtin}]}`: the built-ins (Administrative Events `level>=warning`, Security failures `log:security tag:audit.failure`, This run `boot:current`, Since last start) and the saved ones; `POST {name, filter}` saves one |
| `GET` | `/api/logbook/catalog[/{provider}][?id=N]` | `{"version", "specs", "markdown"}`: the event reference |
| `GET` | `/api/logbook/components` | `{"components": [{name, kind, log, state, since, started, error, activity}]}`: what the answering process runs (`lifecycle.Components`) |
| `GET` | `/api/logbook/verify[?log=security]` | `{"ok", "problems", "chain", "note"}`: SQLite's quick check, and with `log=security` the chain (`{records, head, archives, tombstones, restores, notes, break}`), checked against the key, the anchor and a file witness |
| `GET` | `/api/logbook/audit?since=&until=` | the Security log's audit export bundle (NDJSON), verifiable offline with the key |
| `POST` | `/api/logbook/recover` | `{"file", "recovered", "problems"}`: copies what can still be read of a quarantined `logbook.db.corrupt-<time>` (the newest, or `{"file"}`) into the store, each event marked `recovered` |
| `GET` `POST` `PATCH` `DELETE` | `/api/logbook/alerts[/{name}]` | `{"rules": [{name, filter, threshold, action, throttle, disabled, builtin, error}]}`: the alert rules in force; `POST` a `logs.alerts` entry adds or replaces one (400 if it doesn't compile), `PATCH {"disabled": bool}`, `DELETE` removes the entry (404 for none). Every answer is the rules. |

- **Cursors** are `<store ID>.<seq>`. A restored or rebuilt `logbook.db` has
  a new store ID, so a cursor from before it starts over, and `restarted`
  (or the stream's `restart` message) says so.
- **Sensitive values** are `«sensitive»` unless the request adds
  `sensitive=1`. A reveal, an export, a clear, a change of a log's settings
  and a check each file a Security record (`security/4901`–`4906`), naming the
  operator and the surface: `dashboard`, or `cli` for `tenant logs` reading
  locally.
- **Events from outside** (`POST /api/events`, the v1 `POST /api/logs/{source}`
  and `/api/addon/event`, `tenant log`, the spool, other programs' folders)
  are marked with keyword `external` and filed under `ext.`: `deploy` is
  `ext.deploy`, in the viewer's External group and the `ext.deploy/` folder,
  whichever way it arrived. They keep their message, level, time and data,
  and the `trace` and `activity` a program Tenant started finds in
  `TENANT_TRACE_ID` and `TENANT_ACTIVITY_ID`; an event ID, task, session,
  approval, tool call, actor or surface is dropped, and so is a data field
  named like one of them. `POST /api/events` refuses (403) a log or provider
  that is Tenant's own:
  `security`, anything under `tenant.`, a v1 source and every registered
  provider and its log. A batch is taken or refused whole.
- **Exports** start with a header record, the first NDJSON line
  `{"export": {schema, catalog, tenant, filter, exported, host, sensitive}}`
  (a `# tenant logbook export: …` line in CSV and text), so a file still makes
  sense on another machine a year later.
- **v1 routes** stay: `GET /api/activity` reads `logbook.db` when it is
  open (its cursor is the store's seq and its epoch the store ID, so it
  survives a restart; foreign folders reach it once they are filed into the
  store), and the `/api/logs` routes list and serve the text files.

### The command line: `tenant logs`

```
tenant logs                                 the logs: events, size, limit, how long they are kept, last write
tenant logs query [FILTER] [--follow] [--limit N] [--reverse]
tenant logs show <seq>                      one event in full, with its catalog help and statistics
tenant logs trace <trace|activity>          the timeline across every log, with the run record
tenant logs stats [FILTER] [--bucket 1h]    counts by log and level, over time, and the top event IDs
tenant logs export [FILTER] --out FILE [--include-sensitive] [--format csv|text]
tenant logs export --audit --out FILE [--since 30d] [--until …]   the Security log's audit bundle
tenant logs verify [security] [--bundle FILE]
tenant logs config <log> [--max-size 64MB] [--max-age 30d] [--on-full keep] [--min-level warning] [--enabled false] [--reset]
tenant logs clear <log> [--export FILE]
tenant logs debug <provider|log> --for 30m
tenant logs catalog [provider] [--id N]
tenant logs components
tenant logs recover [FILE]                  salvage a quarantined logbook.db.corrupt-<time>
tenant logs alerts [list|enable|disable|remove <rule>]
tenant logs alerts add <rule> FILTER [--count N --window 10m] [--action discord|imessage|cron] [--target PROMPT] [--throttle 1h]
```

- **Local or through the dashboard.** Each command is a client of the API
  above. When this account can open `<logs root>/logbook.db`, the API's own
  handlers run in the command's process over it; otherwise, or with
  `--url URL`, it goes through the dashboard (the token as `tenant log`
  finds it, or `--token`): a serve running as another account, such as the
  Windows server's SYSTEM service, or on another machine. Both paths answer
  the same way. `components` asks the running serve's dashboard even when
  reading locally, since only that process knows what it runs; `verify
  --bundle` checks an audit bundle offline with this install's key.
- **`boot:current`** names the run of the process that answers: through the
  dashboard, serve's own; read locally, the running serve's (else the
  running TUI's), found from the boots table and the boot locks, never the
  command's own.
- **Output.** Reads take `--format text|json|csv`. Text is the text mirror's
  line after each event's seq (local time); `json` is one event per line for
  events (NDJSON, UTC) and one document otherwise; `csv` has a column row.
  `query` shows the newest `--limit` (100) oldest first; `--follow` then
  prints each new event, resuming after the last one it printed if the
  stream drops.
- **Exports** are NDJSON unless `--format` says otherwise, start with the
  header record, are owner-only files, and file a Security record. `clear --export`
  exports the log first.

### The terminal UI: `/logs`

`/logs [FILTER]` shows the newest 200 events the filter matches in the chat
pane's place, newest at the bottom; `/logs follow [FILTER]` then adds each new
one as it is written. A filter that doesn't parse says why in the chat.
`↑`/`↓`, `PgUp`/`PgDn` and `g`/`G` move; `enter` opens the selected event's
detail (its fields with sensitive values masked, how often it has occurred,
its catalog Help); `c` opens its correlated timeline (its trace's, else its
activity's, with the run record in brief), where `enter` and `c` work again;
`esc` or `q` goes back, and from the list closes the pane. It reads through
the API above, run in the TUI's process (`control.LogsControl`,
`cmd/tenant/logstui.go`), and strips escape sequences from log text before
showing it (any other control character is shown as a space). The
full-screen viewer is the dashboard's (decision D12). The activity feed
scrolls from the keyboard too, with or without mouse capture: `Alt+PgUp`/
`Alt+PgDn` (or `Ctrl+PgUp`/`Ctrl+PgDn`) a page, `Alt+↑`/`Alt+↓` a line.

### Alert rules

An alert rule (`internal/logs/alerts`) is a filter, an optional threshold
(N events within a window), an action and a throttle. The rules are
evaluated every housekeeper tick (30 s) in the process that holds the
housekeeper lease, so two processes sharing the log book never both alert.
It reads the events committed since its cursor, which it keeps with each
rule's last firing in `logbook.db`'s meta table: a lease handover neither
fires twice nor misses an event. A new or restored log book starts with its
last 5 minutes. A rule fires on a new match, once its threshold holds (the
count within the window ending now), at most once per throttle (15 minutes
unless set; 1 minute at least); when two rules would send one event to the
same place, the earlier one does. The `alerts` log is never evaluated, so an
alert can't set off another.

Every firing is an event, `alerts/1001` (warning), so `log:alerts` finds
them; `alerts/3001` is an action that failed and `alerts/3002` a rule that
doesn't compile. A firing also puts a banner, `⚠ alert <rule>: <message>`,
on the evaluating process's status feed (the TUI's, or serve's terminal and
`system` log), and the dashboard's Overview turns its Agent card from
healthy to `⚠ N alerts` while any fired in the last 24 hours, whichever
process evaluated them. The actions:

| `action.kind` | Does |
|---|---|
| `dashboard` (default) | the alert event and the banner only |
| `discord` | a DM to the operator through the relay (as the MCP re-auth notice) |
| `imessage` | a text to `imessage.operator` |
| `cron` | a one-off prompt job through the scheduler (the safe runner, no exec); `target` is the prompt |
| `investigate` | once the operator approves, a read-only agent turn over the event (below); `target` is empty, `discord` or `imessage`: where a notice that it finished also goes |

What leaves the log book is only the rule, the event's level, log,
provider and ID, its number and time, its message (scrubbed again, on one
line, cut at 300 characters, in a code span so it can't pass for Tenant's
own text) and a link to it on the dashboard (`/logs/event/<seq>`). Never a
data field. A cron prompt carries no log text at all: the operator's prompt,
then those facts without the message.

The built-in rules run unless an entry of the same name changes or
disables them:

| Rule | Filter |
|---|---|
| `unexpected-exit` | `provider:system id:3,4`: a crash or a kill, found at the next start |
| `sandbox-unattended` | `provider:sandbox id:2001,2002 data.surface:cron-*`: a refusal, or in a dry run an unconfined run, on a cron surface, where nobody could be asked (TEN-347) |
| `audit-failures` | `tag:audit.failure`, 5 within 10 minutes |
| `logbook-full` | `provider:logbook id:2004 data.percent>=95` |
| `critical` | `level:critical` |

`logs.alerts` in `config.json` is read at every evaluation, so an edit
applies without a restart. An entry named like a built-in rule overrides
the fields it sets. `tenant logs alerts` lists the rules in force and
changes them (`add <rule> FILTER [--count N --window 10m] [--action …]
[--target PROMPT] [--throttle 1h]`, `enable`, `disable`, `remove`, which
puts a built-in rule back to its defaults), here or through the dashboard
like every `tenant logs` command: `GET/POST /api/logbook/alerts`,
`PATCH` (`{"disabled": true}`) and `DELETE /api/logbook/alerts/{name}`.
Each change is a Security record (`security/4902`, log `alerts`), besides
config.json's own (`4501`):

```json
"logs": { "alerts": [
  { "name": "critical", "action": { "kind": "discord" } },
  { "name": "audit-failures", "disabled": true },
  { "name": "cron-errors", "filter": "log:cron level>=error",
    "threshold": { "count": 3, "window": "1h" },
    "action": { "kind": "cron", "target": "Find out why the cron jobs fail." },
    "throttle": "2h" }
] }
```

#### Investigating an alert with the agent (TEN-400)

The `investigate` action asks the agent to explain an alert. Open question
Q2 of the design (§15) was decided yes by the operator, on the condition
that log text is treated as what it is: input anyone can write. So each
limit is enforced by code, not asked of the model (`cmd/tenant/loginvestigate.go`):

- **Approval, every time.** A firing asks the operator through the approval
  broker (the TUI and the dashboard's approvals, category `investigate`),
  showing the alert's facts but no log text. No mode, rule or session grant
  answers it. Denied, unanswered within 15 minutes, or with no surface to
  ask, no turn runs, and `investigate/4002` says why. One investigation runs
  (or waits) at a time; a firing meanwhile fails as `alerts/3001`.
- **Read-only tools.** The turn's whole tool surface is `logs_query` (the
  log book, in the filter language, sensitive fields never shown) and the
  memory reads `memory_recall`, `recall` and `memory_history`, each only if
  it is neither gated nor outbound (`memory_search` is outbound: peers see
  the query). The model is offered nothing else; a call to any other tool
  fails the agent's validation and is refused again at dispatch. It has no
  exec, no file access, no network, no messaging, no config, cron or
  memory writes, and no sub-agents.
- **A budget.** The top-level `budgets` apply, held under an investigation's
  ceiling: 200,000 tokens, 24 tool calls, $5 at the budgets' rates, 10
  minutes, and 8 steps. The first one reached stops the turn.
- **Untrusted input.** The attachment is the alert's event and the up to 40
  nearest events of its turn or activity (`trace:`/`activity:` timeline),
  each line only the time, level, number, provider/ID, log, correlation IDs,
  message and non-sensitive fields, scrubbed again and cut (16 KB in all).
  It and every `logs_query` result carry TEN-311's label
  `{untrusted, log}`, so the flow rules follow them: an outbound call
  quoting them would ask first.

The turn's activity is filed in the `alerts` log, which no rule reads, and
shows in the dashboard's activity feed as `tenant-investigate`. Its result
goes to the status feed and to the Security log: `investigate/4001` when it
starts and `4003` when it ends (outcome, steps, tool calls, tokens, and the
result as a sensitive field), both with the turn's trace, so `trace:<id>`
finds the whole run and `runs.db` its record. A result that quotes a
sensitive read (memory) stays behind the Security record's reveal: the feed
gets a pointer to the trace instead. The rule's channel, when `target` names
one, gets only the outcome and the trace to read it by: the report is written
from log text, and nothing sent off the machine carries log text (§11).

### Event IDs, the reference and coverage

**Declaring an event.** Declare it as a package-level `var`, next to the
provider it belongs to (the built-in ones in `internal/logs/catalog`), and
give it `.Doc(meaning, cause, action)`. All three are required. The ID goes in
its block:

| Block | Use |
|---|---|
| 1–999 | lifecycle: starting, ready, stopping, stopped, degraded, recovered |
| 1000s | normal operations |
| 2000s | warnings, retries and degradation (level warning) |
| 3000s | errors and failures (level error or critical) |
| 4000s | security and audit |
| 5000s | configuration changes |
| 9000s | diagnostics: `.Diagnostic()`, and only these |

An ID means one thing forever. When an event stops being written, keep its
definition and mark it `.Retired()`; never delete it or reuse its number.

**The reference.** [`docs/EVENTS.md`](EVENTS.md) is generated from every
provider linked into the binary. After adding or changing a definition, run:

```bash
go test ./cmd/tenant -run TestEventsDoc -update
```

The same test fails when the file is stale, when a definition breaks the
conventions above, or when an ID the file lists is no longer declared. The
event Help text (`catalog.Help`, looked up with `catalog.Lookup(provider, id)`)
is the same entry, so the dashboard and `tenant logs` show what the file says.

**Coverage.** `go test ./internal/archtest -run Coverage -v` prints, per
provider, how many static warning-and-above call sites carry a catalogued ID
(a warning-or-worse `Def`'s `Log` or `Audit`, and `event.Start`), against
those that don't (`Warn`, `Error`, `Log` at a constant warning level or worse,
and `hub.Logf` at one). The `providers` table in
`internal/archtest/coverage.go` maps each provider to its packages and files.

**The ratchet.** A package listed in `catalogued` (same file) may have at most
its recorded number of warning or error call sites without an ID. Adding one
fails the test: log it through a catalogued event instead. When the number
drops, the test fails too, and says what to lower it to.

**Catalogued so far:**

- `cron` (`internal/cron/events.go`): each job run is an activity, `cron/1001`:
  start, then stop with its duration or fail with its error. A prompt job's run
  carries its turn's `trace` and links to it (`related` on the start). A shell
  job's output is `cron/9001`, diagnostic and masked. Loading, missed runs
  (caught up, or skipped and why) and jobs added, switched and removed are
  logged too. The one-line run summary still goes to the status feed.
- `peering` (`internal/peering/events.go`): the listener's lifetime is an
  activity (`peering/1`), and a start that fails is `peering/3001`. Pairing,
  both ways, is logged; so are rejected tokens, and peers going up or down
  (once per change, not every heartbeat). Each peer session and each tool a
  peer calls is a verbose activity, so only a failure shows by default.
- `dashboard` (`internal/dashboard/events.go`): the web server is an activity
  (`dashboard/1`), and its HTTP error log (TLS handshake failures, handler
  panics) is `dashboard/2001`. Each live stream (SSE or WebSocket) is an
  activity from connect to disconnect (`dashboard/1001`); one that fell behind
  and missed events is `dashboard/2002`. Rejected requests (unknown host,
  cross-origin, no or wrong token, CSRF, a wrong sign-in token) are
  `dashboard/2101`–`2105`: the operational side, while the Security log keeps
  the audit records. Every request is a diagnostic activity (`dashboard/9001`)
  with its method, route pattern, status and duration, never its path, query,
  body or token.
- `eval` (`internal/eval/events.go`): each eval run, nightly or manual, is an
  activity (`eval/1001`) that stops with its overall score and counts, and each
  task is a child activity (`eval/1002`) that stops with its result. A task
  left ungraded by a harness, provider or judge failure is `eval/2001`, with
  its class. A missing baseline (`2002`), a regression against it (`2003`), an
  unreadable one (`2004`), a report or trend line not saved (`2005`) and a
  trend update (`1003`) are logged too.
- `mcp` (`internal/plugins/mcpremote/events.go`) and `sandbox`
  (`internal/sandbox/events.go`): see [The MCP logs](#the-mcp-logs) and
  [The `sandbox` source](#the-sandbox-source).

## Records that lead back to a run

A turn's trace ID (`trace_id` in `runs.db`) is also written on the records the
other stores keep, so each of them leads back to the run that made it:

| Record | Where | Field |
|---|---|---|
| Run traces | `runs.db` | `trace_id`; a sub-agent's run also has `parent_trace_id`, the turn that spawned it |
| Route decisions | `route-decisions.jsonl` | `trace_id` (a spawn-time decision carries the spawning turn's) |
| Cron history | `cron-history.json` | `trace_id` of the job's turn (none for a shell job) |
| Archive | `archive/<month>/<session>.jsonl` | `trace_id` |
| Token usage | `usage.db`, table `token_usage` | `trace_id` |

- `runs.db` re-applies its age and count bounds every hour while serve or the
  TUI runs, not only when it opens. Eval turns keep no run records.
- Approval IDs are `ap-<boot>-<n>`: the first 8 hex digits of the process's
  boot ID and a counter, so serve and the TUI never mint the same one. An
  approval the broker decides alone (a category mode, a session grant, a
  Discord or iMessage prompt) is `policy:<category>:<action>:<boot>-<n>`, one
  record per prompt.
- An outcome names its cause: `denied` is the operator's, `denied_shutdown`
  means the process stopped while it waited, and `expired` means nobody
  answered in time (a Discord or iMessage prompt's timeout, or the turn ended).
- `route-decisions.jsonl` and `cron-history.json` hold one record per line.
  Appends take a shared lock on a sibling `<file>.lock`; a rewrite (route
  retention, confirming an assisted suggestion, trimming cron history to its
  newest 64) takes it exclusively. Serve and the TUI therefore never lose each
  other's rows. A `cron-history.json` in the old format (one JSON array)
  becomes lines on its first load.

## Reading it back programmatically

`logs.ParseLine` inverts the format, so another tool (or an AI pulling logs)
can load any file losslessly. The dashboard's JSON projection
(`/api/activity`) adds `source`, `level`, `msg` and `fields` to each event and
keeps the older `kind / agent / iter / tool / is_err / text` fields.
