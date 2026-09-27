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
├── subagents/                    ← orchestrated sub-agents (Programmer, Researcher, …) + the bus
├── approvals/                    ← every gated-action decision: pending / approved / denied / expired
├── discord/                      ← the relay: inbound messages, the relay agent's actions, relay logs
├── imessage/
├── cron/                         ← scheduled-job runner agents + the engine
├── eval/                         ← nightly/manual eval task agents
├── improve/                      ← distill / reflect / consolidate / soul jobs
├── model/                        ← router, backends (llama.cpp/vLLM, Anthropic, …), fallbacks
├── route/ memory/ mcp/ peering/ dashboard/
├── sandbox/                      ← the execution sandbox: confined runs, refusals, escalations, dry-run would-be refusals
├── privacy/ shutdown/            ← the daily retention sweep; the staged shutdown's steps
├── system/                       ← setup, health, settings, the status feed lines (was tui.log), crash dumps
├── boots/ crash/                 ← records, not sources: see "Crashes and unexpected exits"
└── <anything-you-add>/           ← a script, a sidecar, a new Go package — see "Adding a source"
```

Override the root with `--log-dir DIR` or `"logs": {"dir": "…"}` in config.json.

## The line format

One entry is exactly one line, readable in `tail -f` and exact under `grep`:

```
2026-09-22 14:03:11.123 INFO  [turns] tool call web_search	ev=tool_call iter=2 args="{\"q\":\"hn top\"}" trace=9c1e6f0a2b7d4e35a8f1c0d2e3b4a5c6
2026-09-22 14:03:12.001 ERROR [turns] tool result web_search failed	ev=tool_result iter=2 result="timeout after 30s" trace=9c1e6f0a2b7d4e35a8f1c0d2e3b4a5c6
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
  restart. A day's file rolls to `<day>.1.log` past 64 MB.
- **Level floor:** INFO by default. `"logs": {"level": "warn"}` (or `"error"`)
  raises it for the files and the feed; `"debug"` or `TENANT_LOG=debug` lowers
  it (the files then hold the backend internals too).
- **Value cap:** one value (a tool result, an argument blob) is cut at 4 KB with
  a `…(+N bytes)` marker (`"logs": {"max_field_bytes": N}`; `-1` = unlimited).

Where lines land: a turn-event's source is decided by the emitter (`cron`,
`discord`, `eval`, …) or derived (`subagents` when a sub-agent is attributed,
`approvals`, the channel for an inbound message, else `turns`). A plain slog
record goes to the logger's fixed source (`logs.For("x")` / `hub.Logger("x")`),
else a `source` attribute, else its `"tag: …"` message prefix (`mcp:` → `mcp`,
`vllm:` → `model`, `reflect:` → `improve`, …), else `system`.

`serve` still writes its TENANT_LOG-filtered stream to stderr for the
supervisor (launchd/systemd) — the hub tees to it. The TUI writes nothing to
stderr; `<data>/tui.log` no longer exists.

## The `sandbox` source

The execution sandbox (`docs/changes/sandbox-seam.md`) writes one line per
`os_exec` command or cron shell job it runs on a confining backend, and one per
refusal:

| Level | Message | When |
|---|---|---|
| INFO | `ran in the sandbox` | ran on a confining backend and nothing was refused. On macOS, a timeout or a reaped background process still shows in `violations` |
| WARN | `refused by the sandbox` | the boundary refused a file or network access and the command was not re-run: enforce on, and the escalation was denied or the surface is unattended |
| WARN | `refused, then approved to re-run outside the sandbox` | an approved `os_exec_unsandboxed` re-ran it on the host |
| WARN | `dry-run: the sandbox would have refused this; it ran on the host` | `"enforce": false`: what enforcing would have refused |
| ERROR | `sandbox could not start the command` | the backend could not start the command (`err=`) |

The fields are `surface`, `backend`, `exit`, `timed_out`, `command`,
`violations` when there are any, and `err` on a start failure. `command` holds
the first 300 characters of the command. For an escalation or a dry-run,
`exit` and `timed_out` describe the host re-run.

```
2026-09-22 14:05:10.412 WARN  [sandbox] refused by the sandbox	surface=discord backend=restricted exit=1 timed_out=false command="cat ~/.ssh/id_ed25519" violations="filesystem: sandbox blocked access outside the policy's read/write roots"
```

Some things are recorded elsewhere or not at all:
- A command on the `host` backend with nothing refused is the pre-seam path and
  is **not logged** here (a `host` command that cannot even start still is). The
  folder and its tab appear once a surface runs on a confining backend.
- A surface whose backend is unavailable fails closed before anything runs. That
  error goes back to the turn or cron job, not to this folder.
- On surfaces with an approval broker (TUI and dashboard, Discord, iMessage), the
  escalation prompt itself is an `approvals` line (`action=os_exec_unsandboxed`).
- The `command` field is the literal command text, so a secret typed inline into
  a command ends up in this log.
- On Linux and Windows the backends do not attribute refusals yet. A denied
  access there shows as `ran in the sandbox` with a non-zero `exit`.

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
- **The Discord gateway** logs how each session ended and how it reconnects: a
  drop, a failed RESUME or IDENTIFY, an invalidated session or a missed
  heartbeat at WARN, a reconnect Discord asked for at INFO. Undecodable
  messages and a fatal stop are logged too.
- **Stores and files:** tool stats, the nightly eval artifact, rotated OAuth
  tokens, the relay-inbox retention sweep and `peers.json` reads are logged; a
  research run record that could not be written is reported on the research
  progress. A failure that repeats on a timer (tool stats every minute, peer
  probes every 30 s) is logged once per streak.

Packages that may not import the log book (plugins, toolmux) log through
`slog.Default()` with a `source` attribute; those lines reach stderr until the
default logger is pointed at the hub (LOG-002).

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
classifier, shutdown steps) log at ERROR. The `route` and `eval` guards log
through slog's default logger, which reaches stderr until serve and the TUI
point it at the log book.

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
and works in tests (records go to stderr at WARN until a hub is installed). A
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
- **Until the store lands,** events are written to the same folders as v1
  lines. The log is the folder. The event's ID, task, opcode and correlation
  IDs come first among the fields: `event=cron/3001 task="Job run" trace=… activity=…`.
  A critical event is written as `ERROR` with `severity=critical`, and a
  sensitive field as `«sensitive»`, never its value.
- `Audit` is for records that must not be lost. For now it returns once the
  line is written; the Security log's store will make it wait for its commit.
- **Plugins** each write their own log, `plugin.<id>`, under Integrations ›
  Plugins: started or could not start, the operator's enable and disable,
  token refreshes, and their API's rate limits, refused credentials and
  failed calls. The catalog comes with the plugin's descriptor
  ([PLUGINS.md](PLUGINS.md) §5). The tool mux's own events (connectors and
  peers that did not come back at launch, unsaved tool stats) are the
  `toolmux` log's.

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
