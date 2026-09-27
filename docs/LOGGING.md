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
├── system/                       ← setup, health, settings, the status feed lines (was tui.log)
└── <anything-you-add>/           ← a script, a sidecar, a new Go package — see "Adding a source"
```

Override the root with `--log-dir DIR` or `"logs": {"dir": "…"}` in config.json.

## The line format

One entry is exactly one line, readable in `tail -f` and exact under `grep`:

```
2026-09-22 14:03:11.123 INFO  [turns] tool call web_search	ev=tool_call iter=2 args="{\"q\":\"hn top\"}"
2026-09-22 14:03:12.001 ERROR [turns] tool result web_search failed	ev=tool_result iter=2 result="timeout after 30s"
2026-09-22 14:03:40.250 WARN  [approvals] approval denied ap-3	ev=approval_resolved category=exec action=os_exec origin=discord outcome=denied
2026-09-22 14:04:02.000 INFO  [discord] relay attachment saved	chat=general path=/…/relay-inbox/img.png
```

`<local date> <time.ms> <LEVEL> [<source>] <message>` then a **tab** and
`key=value` fields. Values with spaces, quotes or newlines are quoted (Go
`strconv.Quote` escaping), so a multi-line tool result still occupies one line.
Turn events carry their kind in `ev=`; plain records have no `ev`.

- **Retention:** files older than 30 days are deleted (`"logs": {"retention_days": N}`;
  `-1` keeps forever). A day's file rolls to `<day>.1.log` past 64 MB.
- **Level floor:** INFO by default. `"logs": {"level": "debug"}` or
  `TENANT_LOG=debug` lowers it (the files then hold the backend internals too).
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

## The dashboard

The **Activity** tab reads the hub: every source is a tab (built-ins first,
custom ones after, `system` last), with a retained-line count, an
**⚠ errors only** toggle, and — when a source is selected — its files on disk
with a raw plain-text view / download. History from before the current run is
there too: at start the hub re-reads the tail of every folder's newest file.

`tenant attach --follow [--source discord] [--level error]` is the terminal
equivalent, over `GET /api/activity`.

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

It POSTs to the running hub (the line shows up live in open dashboards and
TUI feeds); when no hub is reachable it appends straight to the day's file, and
a hub that starts later picks the folder up.

**Anything that speaks HTTP** — the live logging API (same Bearer token as the
rest of the dashboard):

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
| `GET` | `/api/activity?since=&source=&level=` | the retained lines as JSON (cursor replay) |
| `POST` | `/api/addon/event` | legacy add-on line (`{"source","text","level"}`) — files under the add-on's name |

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

## Reading it back programmatically

`logs.ParseLine` inverts the format, so another tool (or an AI pulling logs)
can load any file losslessly. The dashboard's JSON projection
(`/api/activity`) adds `source`, `level`, `msg` and `fields` to each event and
keeps the older `kind / agent / iter / tool / is_err / text` fields.
