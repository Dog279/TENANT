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
├── approvals/                    ← every gated-action decision: pending / approved / denied / expired
├── discord/                      ← the relay: inbound messages, the relay agent's actions, relay logs
├── imessage/
├── cron/                         ← scheduled-job runner agents + the engine
├── eval/                         ← nightly/manual eval task agents
├── improve/                      ← every self-improvement job run (distill, skills, consolidate, reflect, soul, profile, eval), serve's and the TUI's
├── memory/                       ← facts added, superseded, removed (fact_id=); compaction; retrieval diagnostics
├── skills/                       ← skills saved, accepted, enabled/disabled, trusted, removed, imported, exported
├── model/                        ← router, backends (llama.cpp/vLLM, Anthropic, …), fallbacks
├── route/ mcp/ peering/ dashboard/
├── sandbox/                      ← the execution sandbox: every run's start and exit, refusals, escalations, dry-run would-be refusals
├── backup/                       ← each backup, verify and restore, with its counts and result
├── privacy/                      ← each retention sweep and deletion, with what it removed
├── system/                       ← start-up and shutdown (each shutdown stage an activity; was shutdown/), settings and credentials reloads, Tailscale, setup, health, the status feed lines (was tui.log), crash dumps
├── boots/ crash/                 ← records, not sources: see "Crashes and unexpected exits"
├── logbook.db                    ← the store: every event, indexed (see "The store: logbook.db")
├── archive/                      ← the store's archived records (security-<first>-<last>.ndjson.gz)
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
`key=value` fields. Values with spaces, quotes or newlines are quoted (Go
`strconv.Quote` escaping), so a multi-line tool result still occupies one line.
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
`eval` under serve and the TUI, and to stderr from `tenant eval`.

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
2026-09-27 14:05:10.412 INFO  [sandbox] process started	surface=discord backend=restricted command="curl -H 'Authorization: Bearer «secret:bearer»' https://api.example.com" limits="wall=1m0s"
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
command and cron shell job it runs, on every backend, the host included, as a
child process (see [Child processes](#child-processes)): a `process started`
line with `command` and `limits`, and a `process exited` line with `pid`,
`exit`, `duration`, `timed_out`, `violations` when there are any, and `tail`,
the last 512 bytes of the output (stdout and stderr together, as the model
sees them). Both carry `surface` and `backend`. What the boundary did gets a
line of its own:

| Level | Message | When |
|---|---|---|
| WARN | `refused by the sandbox` | the boundary refused a file or network access and the command was not re-run: enforce on, and the escalation was denied or the surface is unattended |
| WARN | `refused, then approved to re-run outside the sandbox` | an approved `os_exec_unsandboxed` re-ran it on the host |
| WARN | `dry-run: the sandbox would have refused this; it ran on the host` | `"enforce": false`: what enforcing would have refused |
| ERROR | `sandbox could not start the command` | the backend could not start the command (`err=`) |

Their fields are `surface`, `backend`, `exit`, `timed_out`, `command`,
`violations` when there are any, and `err` on a start failure. `command` holds
the first 300 characters of the command. For an escalation or a dry-run,
`exit` and `timed_out` describe the host re-run, and the exit line is the
host re-run's.

```
2026-09-22 14:05:10.412 WARN  [sandbox] refused by the sandbox	surface=discord backend=restricted exit=1 timed_out=false command="cat ~/.ssh/id_ed25519" violations="filesystem: sandbox blocked access outside the policy's read/write roots"
```

Some things are recorded elsewhere or not at all:
- A surface whose backend is unavailable fails closed before anything runs. That
  error goes back to the turn or cron job, not to this folder.
- On surfaces with an approval broker (TUI and dashboard, Discord, iMessage), the
  escalation prompt itself is an `approvals` line (`action=os_exec_unsandboxed`).
- `command` and `tail` are scrubbed (see [Secrets](#secrets)) before they are
  cut, so a secret typed inline does not reach the log.
- On Linux and Windows the backends do not attribute refusals yet. A denied
  access there shows only as a non-zero `exit`.
- `tenant sandbox run`, the operator's trial run, prints its result and files
  nothing.

## Child processes

A process Tenant starts is filed by Tenant, in the log of the subsystem that
started it (`internal/logs/capture`): a `process started` line with its `pid`,
a `process exited` line with `exit` (the code, when Tenant knows it),
`duration`, and the OS's words for a failure (`exit status 3`,
`signal: killed`) in the message. A failure is a warning, unless Tenant
stopped the child (`cancelled=true`: a shutdown, a cancelled turn).

| Child | Log | Tagged | Also filed |
|---|---|---|---|
| a local MCP server (`mcp_servers`) | `mcp` | `server=<name>` | `backend`, `limits`, `command` at start; its stderr, line by line; `killed="after the grace period"` when it ignored its stdin closing and was killed |
| `os_exec`, a cron shell job | `sandbox` | `surface`, `backend` | see [The `sandbox` source](#the-sandbox-source) |
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
keep two folders under the root. They hold records, not logs, so they never
become tabs, and backups leave them out:

- `boots/<boot_id>.lock` and `boots/<boot_id>.json`: the process's boot lock,
  held for its lifetime, and its record (pid, role, version, host, start time).
  An orderly stop marks the record clean.
- `crash/<boot_id>.txt`: the Go runtime's fatal output with every goroutine's
  stack (`debug.SetCrashOutput`, `SetTraceback("all")`). It still goes to
  stderr as well, but stderr is lost under the Windows task and drawn over the
  TUI's screen. An orderly stop deletes the file if it is empty.

At start, a record whose lock can be taken belongs to a process that has
exited. If it was not marked clean, it is filed once as a critical `system`
event and its files are cleared:

| Message | When |
|---|---|
| `the previous run crashed: <first line of the dump>` | the runtime left its fatal output. The dump moves to `system/crash-<boot_id>.txt` (listed with the tab's files). The event carries its path in `file` and its head in `stack` |
| `the previous run ended unexpectedly` | no dump: the process was killed, lost power or called `os.Exit` |

Both carry `boot`, `pid`, `role`, `version`, `host` and `started`. v1 files
have no CRITICAL level, so these lines read `ERROR` and carry
`severity=critical`. Only the lock decides, never how old a record is, so a
live process is never reported (for example after a laptop wakes). The check
runs at start only: a serve crash while the TUI is open is reported when either
starts next.

A panic that a guard contains is logged with its stack. A tool that panics
fails only its own call: the model gets an error result, and the log book a
critical `agent: tool panicked` record. The other guards (routing, the spawn
classifier) log at ERROR, and a shutdown step writes `system 3020`. The
`route` guards write `route 3003`. The `eval` guards log through slog's
default logger, which serve and the TUI point at the log book.

## The dashboard

The **Activity** tab reads the hub: every source is a tab (built-ins first,
custom ones after, `system` last), with a retained-line count, an
**⚠ errors only** toggle, and — when a source is selected — its files on disk
with a raw plain-text view / download. History from before the current run is
there too: at start the hub re-reads the tail of every folder's newest file.
Every open tab updates live. After Tenant restarts, an open tab redraws itself
from the new process's lines when it reconnects. A source's tab stays until
Tenant restarts, even if its folder is deleted.

`tenant attach [URL] --follow [--source discord] [--level error] [--trace ID]`
is the terminal equivalent, over `GET /api/activity`; `--trace` keeps one
turn's lines. Flags may come before or after the URL. After a Tenant restart
`--follow` says so and replays what the new process kept.

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
TUI's feed does not show it); when no hub is reachable it appends straight to
the day's file, and a hub that starts later picks the folder up. The token is
`dashboard.auth` when config.json sets one, else the generated
`<config>/dashboard-auth-token` a default install uses (`--token` overrides).
Tenant's own sources (the folders in the tree above: `turns`, `approvals`,
`system`, …) are refused, hub up or not.

**Anything that speaks HTTP** — the live logging API (same Bearer token as the
rest of the dashboard). It refuses Tenant's own sources with HTTP 403 and drops
an `ev` field: only Tenant files turn events, so an outside line can never pass
for an approval or a tool call.

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
`since` and `epoch` as `epoch` for a gap-free tail. Sequence numbers start at 1
again when Tenant restarts, under a new epoch; a `since` from another epoch
replays everything the hub kept, and a `since` with no `epoch` keeps its old
meaning. With `limit=N`, at most N events come back and `cursor` is the last
one's, so the next call continues after it.

**A process that only writes files** — drop a folder under the root and append
to a `.log` file in it (`<root>/mytool/2026-09-22.log`). The hub discovers new
folders within ~5 s, seeds the tab from the file's tail, and follows it live.
Lines in the format above keep their level and fields; any other text is kept
verbatim as an INFO line. The folder name is the source name
(`[a-z0-9][a-z0-9._-]{0,39}`).

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
defer act.End(&err)
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
  ([PLUGINS.md](PLUGINS.md) §5). The tool mux's own events (connectors and
  peers that did not come back at launch, unsaved tool stats) are the
  `toolmux` log's.

### The store: `logbook.db`

`serve` and the TUI also keep every event in `<logs root>/logbook.db`, one
SQLite database (WAL, `synchronous=FULL`, `fullfsync` on macOS). Each writes
through its own connection, so both can share a data directory.
`"logs": {"store": false}` turns it off (read at start); if it can't open, a
`logbook` warning says so and the text files carry on alone.

- **The write path.** An event joins one queue (64k events); one writer
  goroutine takes a batch every 100 ms or 512 events, scrubs it, caps its
  values, appends its operational events to the text files, then commits the
  batch, and only then feeds the Activity tab. The text files are written
  first, so a batch the store loses (a crash mid-commit, a failed write) is
  still in them; a failed commit leaves one `logbook` warning.
- **When the queue is full,** an operational event waits up to 1 ms and a
  diagnostic one not at all, then it is dropped and counted per log
  (`Hub.Dropped`). A `security` event is never queued: it forces its batch to
  commit.
- **Diagnostic events** (`.Diagnostic()` definitions) go to the store only.
- **Reading it.** `store.Since(cursor)` returns every event after a cursor,
  `<store ID>.<seq>`, in commit order, so a tail never misses a row. A cursor
  from another database (restored or rebuilt) starts again from the beginning.
- **Backups** (`tenant backup`) take `logbook.db` with `VACUUM INTO` on a
  fresh connection, never by opening the raw file (on POSIX that would drop
  the locks the backing-up process holds on it), plus `archive/`. The text
  files hold the same events and are left out unless `--include-mirror` is
  given. A restore brings `logbook.db` back with a new store ID, so live-tail cursors start over. A log book
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
- The file is owner-only. The v1 history import and the viewer arrive with
  the next logging v2 tickets.

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
  "security":   { "max_mb": 256, "max_age_days": 365 },
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
  (gzip, one JSON object per line, every column, `data` as its stored text);
  *keep* deletes nothing and writes one `logbook` warning each time the log
  passes its limit. The Security log is always written in full and never
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
  error is a `logbook` error line, never fatal.
- **Low disk.** Below 1 GB or 5% free on the data volume, diagnostic events
  are not written; below 256 MB, only warnings and worse. The Security log
  always writes. Shed events count as dropped (`Hub.Dropped`), and crossing
  either line writes a `logbook` warning.
- `runs.db` keeps its own retention (`runs.max_age_days`, `runs.max_count`).

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
