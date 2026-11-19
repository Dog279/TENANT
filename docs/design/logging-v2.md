# Tenant Logs v2: an Event Viewer–class log book

**Status:** Implemented on `feat/logging-v2` (TEN-360 to TEN-400), 2026-09-27; see [the change note](../changes/logging-v2.md). Planned 2026-09-27.
**Builds on:** the log book v1 (TEN-352, [LOGGING.md](../LOGGING.md)) and durable run records (TEN-300, `internal/runtrace`)
**Platforms:** Windows, Linux and macOS, with one design and no per-OS parts in the core
**Jira:** workstream [TEN-359](https://findtime.atlassian.net/browse/TEN-359), tasks TEN-360 to TEN-400 (the mapping is at the end of §13)

## 1. Summary

Tenant already sends every subsystem to one log book: a plain-text folder per source, a dashboard tab per folder, a REST API and a CLI. That makes a good diary. It is not yet an instrument:

- A line has no event ID, no link to the run that caused it, and no time zone.
- The viewer filters by source and "errors only", and by nothing else.
- Nothing is catalogued, so nobody can say what a given line means or what to do about it.
- Child processes, crashes and parts of some subsystems never reach the log book at all.
- Nothing protects security-relevant records from being edited or silently lost.

The survey behind this plan (§4 and §8) found:

- **Missing output.** No child process's output reaches the log book as a source of its own. None of the iMessage agent's turns reach it either.
- **Silent errors.** 22 background error paths drop errors silently. One of them is the save of an "approve always" rule.
- **Panics.** Nine `recover()` sites keep no stack, and a panicking plugin takes the whole process down.
- **Output that bypasses the log book.** Six HTTP servers send their errors to stderr. The nightly eval harness logs only there.
- **Approvals.** Decisions have no durable record of their own.
- **A leaked credential.** The dashboard's auth token is written into the `system` log. This is filed as a separate fix.

The quality bar is Windows Event Viewer, applied to Tenant's own logs:

- **Every running thing has a log.** The viewer shows the logs as a tree that fills itself in as subsystems, MCP servers, plugins and scripts appear. A Components view lists what is running now and links each item to its log.
- **Every event is structured:** log, provider, event ID, level, task, opcode, keywords, typed data, and correlation IDs.
- **Every event ID is documented:** what it means, why it happens and what to do. The documentation is generated from the same definitions the code uses.
- **Everything can be found.** You can filter on any field, search full text, take a time slice across all logs, and save custom views. One click goes from an event to everything correlated with it.
- **Nothing is lost silently.** Dropped events, rate limiting, full logs, crashes and unexpected exits are themselves events.
- **The Security log can be checked.** It is append-only, and every record is chained with a keyed hash. It is anchored outside its own database, and never deleted to free space.

**It all stays inside Tenant.** Nothing goes to the Windows Event Log, journald, os_log or syslog. The operating system's log facilities are not a dependency, and the same code, storage format and viewer run on all three OSes.

The work comes in seven phases (§13). Phase 0 fixes things in v1 that pay off right away:
- trace IDs in lines and in every record store;
- stray stderr;
- child-process output;
- crash capture;
- secret scrubbing;
- the dropped errors.

Phase 6 adds alert rules.

## 2. Goals, non-goals and principles

### Goals

1. **Complete coverage.** Every subsystem, component, child process, crash and unexpected exit of a running Tenant lands in the log book. The viewer can answer "what is running right now?"
2. **Event Viewer–class records and viewer.** See §3 for the mapping.
3. **Correlation.** From any event you can reach the turn, the approval, the operation and every other event they caused.
4. **Enterprise durability and trust:**
   - loss is measured, and it is zero for the Security log;
   - the Security log is tamper-evident;
   - retention works by both age and size;
   - backups and privacy deletion cover the logs.
5. **Discovery.** A new source shows up by itself, with no registration step and no rebuild. It stays findable afterwards.
6. **One portable design,** behaving the same on Windows, Linux and macOS and tested on all three in CI (`.github/workflows/ci.yml` already runs that matrix).
7. **Compatibility.** Everything that feeds v1 keeps working: `logs.For`, `tenant log`, `tenant attach`, the `/api/logs` and `/api/activity` routes, foreign folders and the reference client.

### Non-goals

- **OS log facilities.** No writing to the Windows Event Log, journald, os_log or syslog. Tenant's logs live in Tenant. (One line has been written there since TEN-535, outside this design: the reason serve or the TUI refused to start over a missing process protection, which a Tenant that is not running cannot show in its own viewer. It is also system 3030 in the log book. See `cmd/tenant/protectiongate.go`.)
- **Other kinds of system.** Not a SIEM, not a metrics database, not a distributed log cluster. The roadmap defers multi-tenancy and Kubernetes.
- **Replacing domain stores.** Memory provenance, skills history, eval trends and run records keep their own stores. The log book records that something happened and links to the domain record.
- **Localized messages.** Messages are in English only.

### Principles

- **Emitting never blocks the agent loop.** The one exception is a Security record, whose write is synchronous and bounded (§10.3).
- **Structured first, readable always.** The store is structured. Every view (the grid, the text mirror, the CLI) is rendered from it.
- **The catalog is the contract.** An event ID means one thing forever, and its documentation and its code come from the same definition.
- **Loss is data.** Anything dropped, sampled, suppressed or truncated is counted and reported as an event.
- **Secrets never reach disk.** Scrubbing happens before the value cap, the store and the mirror, not when the log is viewed.
- **Discovery over configuration.** A subsystem, MCP server, plugin or script appears in the tree the first time it writes.

## 3. Event Viewer concepts, mapped to Tenant

| Event Viewer | Tenant Logs v2 |
|---|---|
| **Windows Logs:** Application, Security, System | **Tenant logs:** `system` (processes, components, configuration, storage, crashes), `security` (the audit trail), `turns`, `subagents` |
| **Applications and Services Logs,** one per provider | **Subsystem logs:** one per subsystem (`discord`, `cron`, `sandbox`, `model`, …), plus one per MCP server (`mcp.<name>`), plugin (`plugin.<id>`) and external source (`ext.<name>`). The tree fills itself in. |
| **Channel types:** Admin, Operational, Analytic, Debug | Two channels, not four, because one operator doesn't need more: <ul><li>**operational** is always on;</li><li>**diagnostic** takes high-volume detail (tool payloads, per-call model metrics, requests) and is kept for a short time. Its verbose (debug) records are written only while a timed switch is on for that provider.</li></ul>**Administrative Events** is a built-in view (§9), not a channel. |
| **Provider** (publisher) with a manifest | **Provider,** declared in the **catalog** with its name, log, version, events and field schemas |
| **Event ID,** version and message template | **Event ID** within its provider (for example `discord 2001`), plus a version, a message template with named fields, and documentation (meaning, cause, action) |
| **Levels:** Critical, Error, Warning, Information, Verbose | The same five: critical, error, warning, info, verbose. Debug output is verbose. |
| **Task category and opcode** (Start, Stop, Info) | **Task** (such as "Connection" or "Job run") and **opcode** (start, stop, fail, info). A start/stop pair gives a duration. |
| **Keywords** (Audit Success, Audit Failure, …) | **Keywords:** `audit.success`, `audit.failure`, `config`, `lifecycle`, `network`, `child`, `external`, `imported`, … |
| **EventRecordID** | <ul><li>A **global sequence**, paired with a **store ID**, is the live-tail cursor. The store ID means a cursor can't be mistaken across a restore or a rebuilt database.</li><li>`security` also gets a **record number** that has no gaps.</li></ul> |
| **Correlation:** ActivityID, RelatedActivityID | `trace_id` (a turn, from runtrace), `activity_id` (any other operation), `related_id` (the parent), plus `session_id`, `approval_id` and `tool_call_id` |
| **Execution** (ProcessID, ThreadID), Computer, UserID | `pid`; process role (serve, tui, cli, child); `boot_id`; host; version. **Actor** is operator, agent, sub-agent, peer, external or system. **Surface** is tui, dashboard, discord, imessage, cron, cli, api or peer. |
| **EventData / UserData** | **Data:** typed fields (string, int, float, bool, time, duration, JSON), each with a sensitivity flag |
| **Overview and Summary** | A **Summary** page showing: <ul><li>counts by level per log over 1 hour, 24 hours and 7 days;</li><li>top event IDs;</li><li>recent critical events and errors;</li><li>log sizes against their limits;</li><li>loss counters;</li><li>unexpected exits.</li></ul> |
| **Custom Views;** Administrative Events | **Saved views.** Built in: Administrative Events (warning and above, across all logs), Security failures, This run, and Since last start |
| **Filter Current Log, Find, XPath** | A **filter language** plus full-text search, for example `level>=warning log:discord id:2001-2999 since:24h "reset"`. See Appendix A. |
| **Event Properties:** General, Details (XML) | **Event detail tabs:** <ul><li>General;</li><li>Details: the fields plus the raw JSON;</li><li>Correlated: a timeline across logs, plus the run record;</li><li>Help: the catalog entry.</li></ul> |
| **Log Properties:** size, maximum size, overwrite / archive / do not overwrite | **Log properties:** <ul><li>maximum size and maximum age;</li><li>what to do when full: overwrite, archive, or keep and alert;</li><li>minimum level;</li><li>enabled or not.</li></ul> |
| **Clear Log** (Security event 1102) | **Clear**, offering an export first. Clearing any log writes a `security` event. `security` itself can't be cleared, only archived. |
| **Save Events As** (.evtx, .xml, .csv, .txt) | **Export:** NDJSON (lossless), CSV or text. A Security export carries its hash chain for offline verification. |
| **Attach Task To This Event** | **Alert rules:** a match plus a threshold triggers an action: notify (dashboard, Discord, iMessage), run a job, or ask the agent to investigate |
| **System event 6008,** "The previous system shutdown was unexpected" | **Unexpected-exit detection** from start-up records and per-process OS locks. The Go runtime's crash output is saved to a file and read in at the next start. |
| **services.msc** | A **Components** view: every subsystem and child process with its state, uptime and last error, linked to its log |
| **Security log protected by the OS** | A **Security log** that is append-only, chained with a keyed hash (HMAC) on every record, and anchored outside its database. An outside witness is optional. `tenant logs verify` checks it, and §10.2 states plainly what that does and doesn't prove. |

## 4. Where v1 stands

This is a survey of `bfa09ad`. Each gap names the code it was found in.

### 4.1 What v1 gets right, and v2 keeps

- **One sink.** `internal/logs.Hub` takes slog records, turn events and status lines.
- **Files.** One folder per source, daily files that roll at 64 MB, 30-day retention, and a 4 KB cap on each value.
- **Sources register themselves:** through `logs.For`, an HTTP POST, `tenant log`, or a folder another process writes (discovered within about 5 seconds and tailed every second).
- **The line format** is readable by people and parses back almost losslessly. The reference client's tests pin it byte for byte. The exceptions: tabs and line breaks in the message are flattened to spaces, and field keys aren't escaped (`entry.go:139, 149-155`).
- **The dashboard's Activity tab** has a tab per source, an errors-only toggle, raw file views, and an SSE replay by cursor.
- **Run records.** `runs.db` (TEN-300) keeps each turn's trace ID, model, tokens, tools, approvals and outcome, and can export them as JSONL.

### 4.2 Gaps against the bar

**Records**
- **Almost no structure.** There are no event IDs, tasks, opcodes or keywords. The only structure is `ev=` on turn events, so nothing says what a line means or what to do about it.
- **Levels.** There are four, with no critical. `logs.level` honors only `debug` and ignores any other value, so the floor can never be raised (`cmd/tenant/wiring.go:316-348`).
- **Time.** Lines carry local wall-clock time with no offset (`entry.go` `timeLayout`). For an hour each autumn the times are ambiguous, and they can't be compared across machines. `/api/activity` also renders `at` to the second, losing the milliseconds that are on disk.
- **Sequence numbers restart at 1** on every start, with no epoch (`hub.go` `Open`).
  - The dashboard's SSE re-syncs to the new head, so it drops or repeats only the start-up lines (`hub.go:331-332`; `ssr_activity.go:157-176`).
  - `tenant attach --follow` won't move its cursor backwards (`attach.go:189-191`), so it goes silent until the new head passes the old cursor.

**Correlation**
- **Log lines can't be joined to `runs.db`.** `agent.Event.TraceID` is set only on `turn_start` (`agent.go:561`), and `agent.EntryFor` drops it anyway (`logentry.go`).
- **The slog handler ignores its `ctx`** (`slog.go` `Handle`), so even `…Context` calls lose the trace that `runtrace.WithSpan` put on the context.

**Coverage** (§8 goes subsystem by subsystem)
- **Local MCP servers.** Their stderr is kept only as an 8 KiB in-memory tail, and is shown only when start-up fails (`sandbox/process.go`, `mcpstdio.go`).
- **Only two processes open the hub:** serve (`serve.go:87`) and the TUI (`commands.go:1805`).
  - One-shot commands and `tenant mcp-memory` log to stderr only.
  - `slog.SetDefault` is never called, so a call on the default logger bypasses the hub.
  - In `internal/plugins/web/web.go`, chromedp errors print straight to stderr (lines 822-827), and a warning goes to `slog.Default()` (line 435).
- **Some TUI status lines skip the log book.** Self-improve results, research progress and the reconnect monitor go straight to the feed channel (`commands.go:2805-2827, 3066-3071`; `reconnect.go:105-109`).
- **`os_exec` on the host backend** is not logged at all (LOGGING.md).
- **Crashes** leave no stack in the log book. Serve's stderr goes to whatever supervises it:
  - launchd writes files that are never rotated;
  - the Windows task that `scripts/migrate-to-server.ps1` sets up has no redirect at all.

**Viewing**
- **The Activity page** renders the whole filtered ring, up to 10,000 rows, on the server, with no paging. Rows can't be expanded, and messages are cut at 200 characters (400 for errors).
- **Filters.** Only one source, and errors-or-warnings. There is no text search, time range, kind, agent, tool, trace, or choice of several sources. The same is true of the page, the SSE stream and `/api/activity`, and `/api/activity` has no limit either.
- **A second open tab lags.** Every open Activity tab waits on the same one-slot wake-up channel (`hub.go` `notify`), so each wake-up reaches only one tab. The other tab catches up only at the next write.
- **Two processes, one folder.** Serve and the TUI can share a data directory (`datalock` allows it), and then both write the same folders. Each tails only the folders it doesn't write itself, so for any source both write, each process shows only its own lines.
- **The TUI has no log view.** Its feed is capped at 500 lines and scrolls only with the mouse.
- **The command line:**
  - there is no `tenant logs` or `tenant trace`;
  - `tenant runs export` dumps all of `runs.db`, with no filter;
  - `tenant attach URL --follow` ignores flags placed after the URL, because the standard flag parser stops at the first positional argument.
- **Token lookup.** `tenant log`, doctor's serve check and the reference client read the token only from `dashboard.auth`. A default install keeps it in the token file, so they get 401s.

**Trust and safety**
- **The dashboard's auth token is written into the log book** (`serve.go:693` and `commands.go:2981`, through `hub.Logf("system", …)`). It is in `logs/system/*.log`, the ring and `/api/activity`. `serve.go:749` also prints it to stdout, which launchd captures to a file. *This is filed as a separate fix.*
- **Forged records.** `POST /api/logs/{source}` can write into any built-in source (`approvals`, `turns`), forged `ev=` fields included. So an external script can forge an approval record.
- **Secrets in logs.**
  - Commands are logged literally, so a secret typed inline lands in the `sandbox` folder (LOGGING.md).
  - `runs.redact_tool_bodies` doesn't reach tool arguments and results in the `turns` folder.
- **Terminal injection.** Only CR, LF and TAB are escaped. ESC and other control characters are written raw, so a hostile string can drive the terminal of anyone running `tail -f`.
- **No integrity protection.** Editing or deleting a file leaves no trace.
- **Permissions.** The reference client creates folders 0755 and files 0644, where the hub uses 0700 and 0600.

**Operations**
- **Retention is by age only.** There is no size budget beyond the 64 MB daily roll. The ring size, backfill and scan intervals are hard-coded.
- **Privacy settings.**
  - `tenant privacy retention set logs` says a running Tenant applies it at the next prune, but the hub fixes its retention when it opens (`privacyctl.go:312`).
  - `privacy status` ignores `logs.dir`.
- **No health checks.** Doctor has no log-book check, and `security audit` says nothing about logs.
- **Docs that don't match the code.** The tutorials say lines appear in TUI feeds (the TUI drops them) and that deleting a folder removes its tab (it doesn't).

### 4.3 Records kept outside the log book

`runs.db` is the only structured, correlated event store. The rest falls into four groups:

- memory state that carries its own provenance;
- JSONL telemetry;
- small JSON rings;
- nothing at all.

The only correlation ID carried in `context.Context` is `runtrace`'s span. Its trace ID reaches:
- `runs.db`;
- episodes;
- fact and skill lineage.

It reaches no other store in the table below.

| Record | Where it lives | Correlation | Retention | Gaps |
|---|---|---|---|---|
| **Run traces** (TEN-300) | `runs.db` | `trace_id` per turn. A sub-agent starts a new trace with no parent link (`team.go:596-605`). | 90 days / 10,000 rows | <ul><li>Pruning happens only when the store opens (`store.go:56`), so a long-running serve or TUI never prunes.</li><li>Eval turns land in the operator's `runs.db` (`eval.go:634`).</li><li>`cost_estimate_usd` is never written.</li><li>The only viewer is `tenant runs export`.</li></ul> |
| **Approval decisions** | No store of their own. They reach `runs.approvals` only when a turn span is present, plus the log book's lines. | `ap-N` restarts in each process. `policy:<cat>:<action>` merges two same-action prompts in one turn (`broker.go:459`). | as their hosts' | <ul><li>A shutdown `DenyAll` is recorded exactly like an operator's deny.</li><li>An expired Discord/iMessage prompt is recorded as denied.</li><li>A Discord verdict lives only in the Discord message; its nonce is never stored.</li><li>iMessage decisions are not persisted.</li><li>External approval marker files name no clicker and are world-readable.</li><li>The cron-exec auto-approver bypasses the broker entirely (`cronmgr.go:209-234`).</li><li>Changes to category modes are recorded nowhere.</li></ul> |
| **Route decisions** (TEN-289) | `route-decisions.jsonl` | decision ID, session; no trace | 90 days, by rewriting the whole file | <ul><li>Two processes can lose each other's rows during a rewrite.</li><li>Privacy doesn't cover it.</li></ul> |
| **Cron history** | `cron-history.json`, a ring of 64 | job ID; no trace | 64 entries | <ul><li>Also a whole-file rewrite.</li><li>Nothing shows it except each job's last run.</li><li>Privacy doesn't cover it.</li></ul> |
| **Improve job runs** | an in-memory ring of 64 | — | the process's lifetime | Lost on restart. |
| **Eval** | `eval-artifacts/trend.jsonl`, plus report files | task and artifact IDs | the trend log is kept forever | — |
| **Research** | `research/<run>/events.jsonl` | agent IDs; no trace | a privacy class | — |
| **Memory provenance and skills history** | side tables in facts.db; `skill_history` | `source_run_ids`, `change_source` | none | <ul><li>Consolidated facts drop `source_run_ids`.</li><li>Accepting, enabling or tombstoning a skill isn't recorded.</li></ul> |
| **Usage, tool statistics** | `usage.db`; `toolstats-<role>-<pid>.json` | — | none | <ul><li>Only the TUI records usage; serve opens `usage.db` but never writes to it.</li><li>Neither has a viewer.</li></ul> |

**No record at all:**
- dashboard access, sign-in and sign-out, and 401/403 rejections (`auth.go:34-55`);
- configuration changes (the file is overwritten atomically, with no history);
- reads and writes of secrets;
- results of `doctor`, `security audit` and `permissions effective` (stdout only);
- backup and verify results (printed only);
- privacy deletions (only counts are printed, plus one summary line from the daily sweep);
- peering heartbeats and pairing refusals;
- the orchestra bus.

## 5. Architecture

```
 emitters                          pipeline (in each Tenant process)                  store and views
 --------                          ---------------------------------                  ---------------
 Go code: logs.For, event.Def --+
 agent.Event via EntryFor     --+   enrich from ctx (trace, activity, process,   +-> text mirror (the v1 folders)
 child stdout/stderr capture  --+-> boot) -> rate-limit -> queue ->              |
 recover() and crash files    --+   writer goroutine: scrub, then mirror, then --+-> logbook.db (SQLite, WAL)
 POST /api/events, tenant log --+   commit the batch. A Security record forces    |     events, full-text index,
 foreign folders, spool files --+   the commit, and its caller waits for it.      |     rollups, names, leases, boots
                                                                                  +-> per-subscriber wake-ups
                                                                                        |
                             viewers: dashboard Logs, TUI /logs, tenant logs (local, or through the API), SSE
```

### Packages

Only four packages import `internal/logs` today: `cmd/tenant`, `internal/agent`, `internal/approval` and `internal/dashboard`. archtest forbids it for:
- plugins and the sandbox;
- cron, peering, config and secrets;
- privacy, backup, memory and model (`rules.go:31-68`).

So v2 splits the types that emitters need into a leaf that anything may import:

| Package | Holds |
|---|---|
| `internal/logs/event` | **A leaf that imports nothing internal.** It holds `Event`, `Level`, `Provider`, `Def`, activities, and an `Emitter` interface that is installed at start-up. Every package may import it, so an event can be written where the action happens without widening archtest for the heavy packages. |
| `internal/logs` | The pipeline: the queue, the writer goroutine and scrubbing. Also the slog handler, and v1's hub and ring, kept for compatibility. It installs itself as the `event` emitter. |
| `internal/logs/catalog` | The built-in providers and their event definitions, made with `event.NewProvider(...).Event(...)`, and the generator for `docs/EVENTS.md`. Plugins declare their own definitions through their TEN-321 descriptors. |
| `internal/logs/store` | The SQLite store: schema and migrations, the writer connection, retention and archives, leases, integrity. |
| `internal/logs/query` | The filter language, planning a filter into SQL and FTS5, and aggregates. |
| `internal/logs/capture` | Child-process stream capture, crash-file intake, and intake of foreign folders and spool files. |

The viewers live where the viewers already are: `internal/dashboard` (a Logs page), `internal/tui` (`/logs`) and `cmd/tenant` (`tenant logs …`).

### Core API (sketch)

```go
package event // internal/logs/event: imports nothing internal

type Level int8 // 1 Critical … 5 Verbose; slog: Critical = LevelError+4, Verbose = LevelDebug

type Event struct {
	Seq            uint64      // assigned by the store; Rec too, for security only
	Log, Provider  string
	ID             uint32      // 0 = not yet catalogued
	Version        uint8
	Time           time.Time   // UTC
	Level          Level
	Channel        Channel     // Operational | Diagnostic
	Task, Opcode   string
	Keywords       []string
	Msg            string      // rendered from non-sensitive fields only
	Data           []Field     // typed values, each with a Sensitive flag
	Corr           Correlation // TraceID, ActivityID, RelatedID, SessionID, ApprovalID, ToolCallID
	Actor, Surface string
	Proc           Process     // PID, Role, BootID, Host, Version
}

func NewProvider(name, log string) *Provider
func (p *Provider) Event(id uint32, lvl Level, task, tmpl string, fields ...string) *Def // chain .Sensitive(), .Doc(meaning, cause, action), .Keywords(), .Diagnostic(), .Retired()
func (d *Def) Log(ctx context.Context, kv ...any)          // queued; never blocks
func (d *Def) Audit(ctx context.Context, kv ...any) error  // security: returns after the commit
func Start(ctx context.Context, d *Def, kv ...any) (context.Context, *Activity)
func (a *Activity) End(errp *error)                        // stop, or fail with the error class
func Install(e Emitter)                                    // cmd/tenant installs internal/logs at start-up

// internal/logs keeps v1's entry point unchanged:
func For(provider string) *slog.Logger                     // uncatalogued events (ID 0)

// internal/logs/store: one writer connection per process
type Store interface {
	Commit(ctx context.Context, batch []event.Event) error // the writer goroutine only; chains security records
	Query(ctx context.Context, f query.Filter, from Cursor, limit int) ([]event.Event, Cursor, error)
	Stats(ctx context.Context, f query.Filter, bucket time.Duration) ([]Bucket, error) // read from rollups
	Subscribe(f query.Filter, from Cursor) (<-chan event.Event, func())               // no missed rows
	Logs() []LogInfo
	SetPolicy(log string, p Policy) error
	Clear(ctx context.Context, log string) error
	Verify(ctx context.Context, log string) (Report, error)
}

type Cursor struct {
	StoreID [16]byte // a new database, restore or rebuild gets a new ID, so old cursors restart
	Seq     uint64
}
```

### Dependency rules

These are the changes to `internal/archtest/rules.go`, and they land in LOG-010:

- **`internal/logs/event` imports nothing internal,** and one new row lets every package import it.
- **`internal/logs` and its sub-packages** may import:
  - `event`;
  - `fsperm`;
  - `runtrace`, a leaf, so the handler can read the trace from `ctx`;
  - `datalock`, for the store's locks.
- **The logs packages don't import `internal/secrets`.** The composition root reads the Security log's key and hands it to the store.
- **Secret changes are audited where the operator's action is handled** (`cmd/tenant/keysmgr.go` and the dashboard's key handlers), which know the surface and the actor. `internal/secrets` stays a leaf.
- **The TUI still doesn't import `internal/logs`** (rules.go:67). `/logs` reads through an `internal/control` interface, as the TUI already does for skills and approvals.
- **The dashboard may import `internal/logs`** (rules.go:68), as it already does.

### Process topology

- **Every Tenant process writes its own events.** That includes serve, the TUI and a one-shot CLI command. Each opens `logbook.db` with one writer connection, and SQLite serializes them.
  - The DSN sets `_txlock=immediate`, which modernc honours. A transaction then takes the write lock when it begins, instead of failing midway with an `SQLITE_BUSY` that `busy_timeout` doesn't retry. (`runs.db` lacks this today, `runtrace/store.go:42`.)
- **Commit order is sequence order.** A row committed later always has a higher `seq`. Retention deletes leave holes only at the old end, so tests assert "no missed rows", not "no gaps".
- **Cursors carry the store ID.** A quarantined, recovered or restored database gets a new ID. A client holding a cursor from the old one re-syncs instead of silently skipping, which is v1's restart bug.
- **Housekeeping takes a lease.** Inserts need no leader, but some tasks must run in exactly one process:
  - importing v1 history, and reading in spool and crash files;
  - retention and archiving;
  - vacuum and WAL checkpoints;
  - dead-boot checks.

  These run in whichever process holds the `housekeeper` lease. The lease is a row taken under `BEGIN IMMEDIATE` and renewed every 30 seconds. If the holder dies, another process takes over within a minute, so a serve crash while the TUI is open is reported by the TUI. A holder that could not renew starts no further task once its lease has run out, since another process may hold it by then; a task already running is not interrupted.
- **Long-running roles only.** Serve and the TUI keep boot records and can hold the lease. A one-shot command writes its events but keeps no boot record, so a Ctrl-C never reads as a crash.
- **Non-Tenant writers** post to the API. When no Tenant process is up, they write to the spool with the protocol in §6.4.
- **Child processes** never open the store. Their parent captures their output and files it (§8.4).
- **Another OS account.** The documented Windows server runs serve as SYSTEM (`scripts/migrate-to-server.ps1:210`). An operator's shell can't open that account's `logbook.db`, its `-shm` file or its key. There, `tenant logs` and the TUI read through the dashboard API (§9.4), the same path they use for a remote machine.
- **The data directory stays local,** as it must for every SQLite store Tenant already has. WAL does not work on network filesystems.

## 6. Storage

### 6.1 One SQLite database is the system of record

`<data>/logs/logbook.db` uses the same pure-Go driver as `runs.db` (`modernc.org/sqlite`). FTS5 is already proven in the repository, in episodic memory search.

```sql
-- Names are interned: a log, a provider or a task is stored once and referenced by number.
CREATE TABLE names (id INTEGER PRIMARY KEY, kind TEXT NOT NULL, name TEXT NOT NULL, UNIQUE (kind, name));

CREATE TABLE events (
  seq          INTEGER PRIMARY KEY AUTOINCREMENT, -- global commit order: the live-tail cursor
  log_id       INTEGER NOT NULL,                  -- names.id: system, security, discord, mcp.github, ext.deploy …
  provider_id  INTEGER NOT NULL,                  -- names.id
  event_id     INTEGER NOT NULL DEFAULT 0,        -- 0 = not yet catalogued
  version      INTEGER NOT NULL DEFAULT 0,
  time_ns      INTEGER NOT NULL,                  -- UTC, nanoseconds since the epoch
  level        INTEGER NOT NULL,                  -- 1 critical, 2 error, 3 warning, 4 info, 5 verbose
  channel      INTEGER NOT NULL DEFAULT 0,        -- 0 operational, 1 diagnostic
  task_id      INTEGER, opcode INTEGER, keywords TEXT,
  msg          TEXT    NOT NULL,                  -- rendered from non-sensitive fields only; scrubbed
  data         TEXT,                              -- JSON object of typed fields; scrubbed; sensitive ones flagged
  trace_id     BLOB, activity_id BLOB, related_id BLOB,   -- 16 bytes each
  session_id   TEXT, approval_id TEXT, tool_call_id TEXT,
  actor        TEXT, surface TEXT,
  pid          INTEGER, role INTEGER, boot_id BLOB,
  bytes        INTEGER NOT NULL,                  -- payload size: each log's share of the budget
  rec          INTEGER,                           -- security only: record number, no gaps
  salt         BLOB,                              -- security only: deleted by a privacy tombstone (§10.2)
  data_hash    BLOB,                              -- security only: SHA-256(salt ‖ msg ‖ data)
  hash         BLOB                               -- security only: HMAC-SHA256(key, previous hash ‖ header ‖ data_hash)
);
CREATE UNIQUE INDEX events_rec ON events(rec) WHERE rec IS NOT NULL;
CREATE INDEX events_time     ON events(time_ns);
CREATE INDEX events_log_time ON events(log_id, time_ns);
CREATE INDEX events_admin    ON events(time_ns) WHERE level <= 3;  -- the planner writes `level <= 3` literally
CREATE INDEX events_id       ON events(provider_id, event_id, time_ns);
CREATE INDEX events_trace    ON events(trace_id)    WHERE trace_id    IS NOT NULL;
CREATE INDEX events_activity ON events(activity_id) WHERE activity_id IS NOT NULL;
CREATE INDEX events_related  ON events(related_id)  WHERE related_id  IS NOT NULL;
CREATE VIRTUAL TABLE events_fts USING fts5(msg, content='', contentless_delete=1);

CREATE TABLE rollup (minute INTEGER, log_id INTEGER, level INTEGER, n INTEGER,
                     PRIMARY KEY (minute, log_id, level)) WITHOUT ROWID;
CREATE TABLE logs   (log_id INTEGER PRIMARY KEY, display TEXT, grp TEXT, kind TEXT, -- tenant|subsystem|mcp|plugin|external
                     max_bytes INTEGER, max_age_s INTEGER, on_full TEXT, min_level INTEGER, enabled INTEGER,
                     debug_until_ns INTEGER, bytes INTEGER, dropped INTEGER, suppressed INTEGER, cleared_ns INTEGER);
CREATE TABLE boots  (boot_id BLOB PRIMARY KEY, pid INTEGER, role INTEGER, version TEXT, host TEXT,
                     started_ns INTEGER, heartbeat_ns INTEGER, stopped_ns INTEGER, clean INTEGER, reported INTEGER);
CREATE TABLE leases (name TEXT PRIMARY KEY, boot_id BLOB, expires_ns INTEGER);
CREATE TABLE views  (name TEXT PRIMARY KEY, filter TEXT NOT NULL, builtin INTEGER, created_ns INTEGER);
CREATE TABLE meta   (key TEXT PRIMARY KEY, value TEXT); -- store ID, schema version, import cursors, chain head
```

- **The measured baseline.** The plan's review benchmarked the first draft of this schema on the development machine (modernc v1.50.1, SQLite 3.53.1, SSD). It measured:
  - 3.9k events/s;
  - 972 bytes per event;
  - the secondary indexes alone cutting a bare table from 97k/s to 8.2k/s.
- **What this shape changes from that draft:**
  - names are interned;
  - IDs are 16-byte BLOBs;
  - full-text search covers the message only;
  - record numbers and hashes exist only for `security`.

  Each change cuts row size and index work, and LOG-011 re-measures. Expected load is hundreds of events a second at peak, so even the draft had about ten times headroom.
- **Pragmas:**
  - WAL;
  - `synchronous=FULL` on the one writer connection, whose cost the batch commit spreads out. On macOS it also needs `fullfsync=ON` and `checkpoint_fullfsync=ON`, because FULL alone there doesn't flush the drive's cache;
  - `_txlock=immediate` and `busy_timeout=5000`;
  - `secure_delete=ON`, for privacy;
  - `auto_vacuum=INCREMENTAL`, set when the database is created.
- **Full-text search** uses a contentless-delete FTS5 table over `msg`. The writer maintains it in the same transaction, inserting and deleting by rowid.
  - The catalog allows a template to name only non-sensitive fields, so `msg` never holds a sensitive value.
  - Search therefore can't get around a masked field's audited reveal.
- **Rollups.** The writer adds to `rollup(minute, log, level)` in the same transaction. The Summary page and the tree's counts read rollups, not events. The review measured a GROUP BY over events at 320 ms for 200k rows.
- **Record numbers.** Only `security` has them. The committing transaction assigns each one from the chain head in `meta`.
- **Space.**
  - Incremental vacuum follows retention.
  - The housekeeper runs `wal_checkpoint(TRUNCATE)` every 5 minutes, after each retention pass, and whenever the WAL passes 64 MB.
  - Viewers keep their read transactions short, so checkpoints can finish. Without such a policy, the review saw the WAL reach 183–612 MB.
  - The WAL counts against the budget.
- **Migrations** are forward-only and recorded in `meta`. Each released schema version has an upgrade test.

### 6.2 Retention and disk budgets

Every log has a policy. The defaults below apply by class, and any log can override them from its Properties dialog, from `tenant logs config` or in `config.json`.

| Class | Max age | Max size | When full |
|---|---|---|---|
| `security` | 365 days | 256 MB | **Archive**: the oldest records move to a compressed, anchored NDJSON file in `<data>/logs/archive/` (§10.2) |
| `system` | 90 days | 128 MB | Overwrite the oldest |
| Every other log, operational channel | 30 days (the v1 default) | 64 MB each | Overwrite the oldest |
| Diagnostic channels, all logs together | 7 days | 256 MB | Overwrite the oldest. Verbose records are written only while a provider's timed debug switch is on. |
| **Whole log book, WAL included** | — | **2 GB** | Shed diagnostic records first, then the oldest operational ones. Never touch `security`; raise an alert instead. |

**How retention runs:**
- It deletes in chunks of at most 500 rows per transaction. The review measured a single 10,000-row delete holding the write lock for 0.7–1.0 s.
- Only the housekeeper runs it.
- The budget is checked against the real file sizes, database plus WAL. Each log's share is apportioned by its summed `bytes`.

**Low disk.** When the data volume has less than 1 GB free, or less than 5% free:
- diagnostic records are shed, and a warning is written;
- below 256 MB free, operational channels keep only warning and above;
- `security` keeps writing whatever the free space.

`runs.db` keeps its own retention (`runs.max_age_days`, `runs.max_count`).

### 6.3 The text mirror keeps v1 alive

The operational channel of every Tenant log is also written, in the v1 line format, to `<data>/logs/<log>/YYYY-MM-DD.log`. So `tail -f`, `grep` and the reference client's byte-for-byte tests keep working.

- **Fed from the pipeline.** The writer goroutine writes each batch to the mirror *before* committing it to the store. The mirror therefore keeps working when the store can't (§12.1).
- **Who writes it.** Each process mirrors the events it commits, appending just as v1's writers do. Appends are atomic per line on all three OSes: `O_APPEND` on POSIX, and `FILE_APPEND_DATA` on Windows.
- **Two format changes, both safe for v1 parsers:**
  - Control characters in the message are escaped. v1 flattens only CR, LF and TAB, so today ESC can reach a terminal running `tail -f`.
  - Sensitive fields are written as `«sensitive»`.
- **Critical** events are written as `ERROR` with `severity=critical`.
- **Folder names** are the log names, and v1's source names are kept as log names.
  - The one move is `approvals`, which goes into `security`. Its folder keeps being written for one release.
  - Every log name must also be a valid folder name on all three OSes, so Windows device names (`con`, `prn`, `aux`, `nul`, `com1`–`com9`, `lpt1`–`lpt9`) and names ending in a dot are refused.
- **External logs** (`ext.*`) are not mirrored. They already live in their own files.
- **Time.** Mirror timestamps stay in local time, as v1's are. `logbook.db` stores UTC and is the authority for time.
- **Retention.** Mirror files follow their log's maximum age. On Windows, a file that another process has open can't be deleted. Retention then skips it, counts it, and retries at the next prune.
- **Turning it off.** `logs.text_mirror: false` stops the mirror.

### 6.4 Intake from v1 and from outside

- **Foreign folders.** A folder under `<data>/logs/` counts as foreign only if no log of that name exists in the `logs` table.
  - Tenant never reads its own mirror folders back. Without this rule, one process would re-file another's mirror lines as external events and loop.
  - A foreign folder's lines become events in `ext.<folder>`, with keyword `external`, and are never mirrored.
  - A folder can't claim a reserved name: a Tenant log, or any provider under `tenant.*`.
- **The spool,** for when no Tenant process is running:
  1. A writer creates `<name>.tmp`, writes it, closes it, and renames it to `<name>.ndjson`.
  2. The housekeeper claims a file by renaming it to `<name>.ingesting`.
  3. It reads the file in, then deletes it.

  Go opens files without `FILE_SHARE_DELETE`, so on Windows a rename fails while another process holds the file open; the claim is simply retried later. On POSIX, nothing is read from a file that is still being written.
- **History.** The housekeeper imports v1's files once, idempotently, recording its progress in `meta`. Lines are scrubbed on the way in, because v1's `system` files already hold the dashboard token.
- **Crash files** are read in only when their process's boot lock is free (§12.2).

### 6.5 Backup and privacy

- **Backup** (TEN-305). `logbook.db` is copied with SQLite's own backup (`VACUUM INTO` on a fresh connection), never by opening and closing the raw file.
  - On POSIX, closing any handle to a database file drops *all* of that process's locks on it (`internal/backup/sqlite.go:22-35` does exactly this today).
  - In v2, the `tenant backup` process itself has `logbook.db` open, so a raw open and close would drop its own locks.
  - Archives are included. The mirror is left out by default.
- **Privacy** (TEN-309). Logs become a data class with their own retention.
  - **Deleting a session** removes it from every log except `security`: every event carrying the session's `session_id`, and every event carrying the `trace_id`s of its runs.
  - **No copies stay behind.** The deletion follows the house recipe (`internal/privacy/privacy.go:9-11`): `secure_delete` is on, and afterwards come FTS `optimize` and `wal_checkpoint(TRUNCATE)`. So deleted text doesn't survive in free pages, FTS segments or the WAL.
  - **Mirror files** holding those events are rewritten.
  - **In `security`,** a tombstone replaces the message, the data and the salt, and keeps `data_hash`. The chain still verifies (§10.2), and with the salt gone, nobody can confirm a guessed value.
  - **Archives** holding deleted events are rewritten: decompressed, tombstoned and recompressed. The commitments keep their chain valid.
  - **Older backups** keep deleted data until they are rotated out. `privacy status` says so, and counts the backups that predate the deletion.
  - A deletion preview lists what will go. A privacy export includes events flagged sensitive.

## 7. Correlation

- **IDs.** They are 128-bit random values, like `runtrace.NewID`'s. The store keeps them as 16-byte BLOBs, and viewers show them as hex:
  - `trace_id` is minted for each turn (TEN-300);
  - `activity_id` is minted for each non-turn operation;
  - `boot_id` is minted for each process start.
- **The context carries them.** The slog handler reads the trace from `runtrace.IDFromContext(ctx)` and the activity from `event.ActivityFromContext(ctx)`, so any `log.InfoContext(ctx, …)` line is correlated with no extra arguments. A lint rule prefers the `…Context` calls in any function that has a `ctx`.
- **Activities** wrap operations that are not turns: a cron job run, a Discord delivery, an MCP server's lifetime, a backup, a privacy sweep, a dashboard request or a peer RPC. One line of code starts one:

  ```go
  ctx, act := event.Start(ctx, cronlog.JobRun, "job", name) // start event; links related_id to the trace or activity already in ctx
  defer act.End(&err)                                         // stop event with duration, or fail event with the error class
  ```
- **Across processes.** A child process gets `TENANT_TRACE_ID` and `TENANT_ACTIVITY_ID` in its environment. Its captured output is filed under the parent's activity. The reference client v2 reads the same variables, so a script started by a cron job logs under that job.
- **Across HTTP.** An external writer passes the IDs as fields of `POST /api/events`.
  - W3C `traceparent` stays out of the core until something consumes it: federation (TEN-189) or an exporter (D10).
  - When one does, it maps onto these IDs with no schema change.
- **The pivot.** A `trace:<id>` or `activity:<id>` query returns one timeline across all logs. It follows `related_id` down a bounded number of levels, so it reaches tool call → sandbox run → child-process output. The viewer shows the run record from `runs.db` beside that timeline.

## 8. Coverage: every subsystem, component and process

This section is the survey of `bfa09ad` that phase 0 works from. Each line names the code it is about.

### 8.1 Subsystems

| Subsystem | Today | Gaps | v2 log |
|---|---|---|---|
| **Status feed** (`pushSys`) | `system` | <ul><li>In serve, lines are dropped when its 64-slot channel is full (`serve.go:109-114`).</li><li>In the TUI, improve results, research progress and the reconnect monitor write to the feed channel directly and never reach the log book.</li></ul> | `system` |
| **Shutdown** (`lifecycle`) | `shutdown`: failures at WARN, successes at DEBUG | The iMessage responder, watchers, peer heartbeat and turn gate are not registered. | `system` (component events) |
| **Model router and backends** | `model` | <ul><li>Per-call lines are DEBUG only.</li><li>Failover notices go to `system`.</li><li>`/model` swap warnings go to stderr (`wiring.go:186`).</li></ul> | `model`, with per-call latency and tokens in its diagnostic channel |
| **Approval brokers** | `approvals`, plus events | Decisions aren't durable (§4.3). | `security` |
| **Tool mux and first-party plugins** | Routed by message prefix | The plugin packages have no logger. Their failures show up only as tool results. | `plugin.<id>`, declared in each TEN-321 descriptor |
| **Browser (Chrome) and egress proxy** | none | <ul><li>chromedp errors go to stderr (`internal/plugins/web/web.go:822-827`).</li><li>The NoSandbox warning goes to `slog.Default` (`web.go:435`, same file).</li><li>Refused proxy dials are not logged.</li></ul> | `browser` |
| **Remote MCP connectors** | `mcp` | The OAuth URL is printed to stderr (`mcpremote/fetcher.go:90`). | `mcp.<name>` |
| **Local MCP servers** (TEN-318) | `mcp`: started / did not start / skipped | Stderr and exit are never observed after a successful start. | `mcp.<name>` |
| **Peering:** client, listener, health monitor | `peering` | <ul><li>The listener logs everything at INFO (`peerctl.go:583-587`).</li><li>The health monitor's logger is never used.</li><li>Rejected tokens are not logged (`listener.go:180-182`).</li></ul> | `peering`; authentication goes to `security` |
| **Team runtime, orchestra bus, sub-agents** | `subagents` | <ul><li>The sub-agent logger has no source, so its warnings land in `turns` (`team.go:612`).</li><li>A sub-agent's run has no parent trace.</li></ul> | `subagents` |
| **Main agent and turn gate** | `turns` | <ul><li>The gate logs nothing.</li><li>Dashboard chat throws away `Turn` errors (§8.3).</li></ul> | `turns` |
| **Route runtime** | `route`, plus `route-decisions.jsonl` | Write, prune and warm-up errors are dropped. | `route` |
| **Cron** | `cron`: failures at WARN, successes at DEBUG. Run summaries go to `system`. | A cron agent's tool events reach `cron` only under serve. The TUI passes no event sink. | `cron` |
| **Self-improve scheduler** | `improve` | <ul><li>The TUI's job results never reach the log book.</li><li>Some TUI lines land in `system`.</li><li>An eval result is logged only at DEBUG.</li></ul> | `improve` |
| **Nightly eval harness** | `eval` (serve only) | <ul><li>It builds a stderr-only logger (`eval.go:566`) and writes directly to stderr (`eval.go:588, 755, 770, 799`).</li><li>It builds its own router and tool mux.</li></ul> | `eval` |
| **Dashboard HTTP** | `dashboard` | <ul><li>`http.Server` has no `ErrorLog` (`server.go:157-161`).</li><li>There is no access log, and sign-in and auth failures aren't logged.</li></ul> | `dashboard`; authentication goes to `security` |
| **Discord relay** | `discord` | <ul><li>Gateway session drops, resume/identify, heartbeat failures and undecodable messages aren't logged (`gateway.go:294-316, 365, 431, 504-512`).</li><li>A relay turn's failure is only replied to Discord (`discordrelay.go:331-333`).</li><li>Relay-agent events reach `discord` only under serve.</li></ul> | `discord` |
| **iMessage responder** | `imessage` | The iMessage agent has no observer (`imessagerelay.go:348-374`), so none of its turns reach the log book, under serve or the TUI. | `imessage` |
| **Sandbox and exec** | `sandbox` | Runs on the host backend are not logged (`execprofiles.go:492-494`). | `sandbox`; exec audit goes to `security` |
| **Research** (TUI) | `subagents` | Progress lines never reach the log book, and store write errors are dropped. | `research` |
| **Tailscale, credentials watcher, settings watcher** | `system`, through `pushSys` | <ul><li>The Tailscale logger is never used.</li><li>Key-reload errors are dropped (`keysmgr.go:143`).</li></ul> | `system` |
| **Usage ledger** | none | Serve never records usage, and the TUI ignores record errors. | `model` (diagnostic) |
| **Privacy retention** | `privacy` | A config-load error skips the day silently (`privacyctl.go:556-559`). | `privacy` |
| **The log hub itself** | none | Write and prune errors only set `SourceInfo.Err` (`hub.go:417-420, 855`). | `logbook` |
| **The TUI program** | none | Fatal errors go to stderr only (`main.go:47`). | `system` |

Three loggers are created and never used: the reconnect monitor's, the peer health monitor's and the memory compressor's.

The packages that run under serve or the TUI with no logging at all:
- peering, orchestra, route, research, sandbox, runtrace;
- `memory/*`, except distill;
- the plugins: github, atlassian, gsuite, sql, wiki, osys, crm, simplefin, imessage, cron and x.

### 8.2 Output that bypasses the log book (LOG-002)

`slog.SetDefault` and `log.SetOutput` are never called. So `slog.Default()` and every `net/http` server without an `ErrorLog` fall through to stderr. In the TUI, that output draws over the alternate screen.

1. **The nightly eval harness:** its stderr-only logger and direct writes (above).
2. **The web plugin:** chromedp errors and the NoSandbox warning.
3. **Interactive OAuth:**
   - `mcpremote/fetcher.go:90` and `atlassian/oauth.go:264` print to stderr;
   - `gsuite/oauth_flow.go:211` prints to stdout.
4. **HTTP servers with no `ErrorLog`,** so TLS handshake errors and handler panics go to stderr:
   - the dashboard (`server.go:158`);
   - the peer listener (`listener.go:409`);
   - the web proxy (`web/proxy.go:33`);
   - three OAuth callback servers.

   **The fix.** Three servers already go through `httplimits.Configure`. It can set:

   ```go
   ErrorLog = slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn)
   ```

   That reaches the hub once serve and the TUI set the default logger. The remaining servers are changed to call `Configure` too.
5. **When the hub fails to open,** serve and the TUI fall back to a memory hub that is never installed, so every `logs.For` logger writes to stderr (`serve.go:88-91`, `commands.go:1805-1808`).
6. **Fatal errors in serve and the TUI** reach stderr only (`main.go:47`). Serve's banner and status lines go to stdout (`serve.go:124, 746-754`).
7. **The Windows `TenantServe` task** (`scripts/migrate-to-server.ps1:209-211`) redirects neither stdout nor stderr, so everything above is lost there.

### 8.3 Errors dropped silently (LOG-005)

These are the most important of the 22 paths the survey found:

1. **Dashboard chat turns:** `_, _ = co.runner.Turn(...)` (`dashboard/session.go:74, 149`). This loses turn-gate rejections and failures before the loop starts.
2. **The Discord gateway:** see the Discord row in §8.1. `discordmgr.go:256` uses `_ = gw.Run`.
3. **"Approve always" on a prompt a tool rule forced:** a failure to save the `allow` rule is dropped (`_ = b.rules.Set(pat, "allow")`, `approval/broker.go:293`). The operator believes the rule was saved.
4. **Operator choices:** failures of `persistLaunchConfig` are dropped at ten call sites, among them `commands.go:2535`, `serve_subsystems.go:81`, `discordmgr.go:289` and `mcptuictl.go:162`.
5. **Discord interaction ACKs and operator DMs** are dropped (`discordapprove.go:333, 358`; `mcptuictl.go:32, 40`).
6. **Peering:**
   - a failure to load `peers.json` silently disables federation (`mux.go:636-638`);
   - heartbeat load errors are dropped (`peerheartbeat.go:231-235`);
   - `_ = MarkAuthenticated` and `_ = ConfirmRotation` (`listener.go:187, 189`).
7. **Persistence:**
   - the route decision log write and prune (`route/log.go:786, 793`);
   - the tool-stats flush (`toolmux/stats.go:254-264`);
   - the nightly eval artifact (`eval_job.go:162`);
   - research store writes (`research/store.go:291, 359`).
8. **Refreshed OAuth tokens** that are not persisted (`gsuite/auth.go:334`; `mcpremote/persist.go:295`), and the relay-inbox retention sweep (`discordingest.go:244-281`).
9. **After a `/model` swap,** errors from re-resolving the embedder are dropped (`serve.go:590-594`, `commands.go:2481-2485`).

### 8.4 Child processes (LOG-003)

No child's output reaches the log book as a source of its own. Today it arrives, if at all, as clipped error text or as tool-result fields.

| Child | Output today | v2 |
|---|---|---|
| **Local MCP servers** | Stderr goes to an 8 KiB tail, read only when start-up fails, and then clipped to its first 300 bytes (`mcpstdio.go:135-138`; `execprofiles.go:543-547`). Exit is never observed. | A start event (pid, backend, limits); stderr lines, rate-limited; an exit event (code, duration, killed after grace) |
| **`os_exec` and cron shell jobs** | Output is returned to the model. It reaches the log book only as a 4 KiB `result` field. Confined runs log metadata to `sandbox`. | Start and exit events in `sandbox`; output lines in `sandbox`'s diagnostic channel; an exec record in `security` |
| **`claude -p`** (the claudecode backend) | Stderr is used to classify failures, and discarded on success (`claudecode.go:153-170`). | Stderr lines and the exit event in `model` |
| **Chrome** (chromedp) | Read until "DevTools listening", then closed. It appears only in a start failure. | Start and exit events in `browser`; the crash text if it dies |
| **tailscale, crm, osascript, gcloud, `ps`/`tasklist`** | Used only in returned errors or status. | Start and exit events in the log of the subsystem that started it, with `child=<name>` |

**Not started by Tenant:** vLLM, Ollama and llama.cpp run separately. They are observed through the `model` provider: connection errors, latency, fallbacks.

A single `capture.Run` / `capture.Start` wrapper handles every child the same way, whichever sandbox backend runs it. Each child also gets:
- an activity linked to its parent;
- a Components entry while it runs;
- `TENANT_TRACE_ID` and `TENANT_ACTIVITY_ID` in its environment.

### 8.5 Panics and crashes (LOG-004)

- **Stacks.** None of the nine `recover()` sites captures a stack, and nothing in the code calls `debug.Stack`.
  - Two sites recover silently: `route/log.go:424` and `team.go:265`.
  - Three log only the panic value: `agent.go:1848, 1861, 1976`.
- **Goroutines with no protection.** A panic in any of these kills the process:
  - tool dispatch (`agent/tools.go:157`): a panicking plugin crashes Tenant;
  - sub-agents, the turn gate, dashboard turn goroutines;
  - cron, improve, Discord and iMessage;
  - MCP and peer goroutines, the hub and the watchers.
- **Where crash output goes.** With no `SetCrashOutput`, the runtime's crash output goes only to file descriptor 2:
  - under launchd, to `tenant.serve.err.log`;
  - under the Windows task, nowhere, so it is lost;
  - in the TUI, into the alternate screen, and the terminal is left unrestored.
- **Plan (LOG-004):**
  - `SetCrashOutput` and heartbeats (§12.2);
  - a stack for every `recover()`;
  - tool dispatch recovers a plugin's panic, logs it as critical with the stack, and returns an error result to the model instead of taking the process down.

### 8.6 Record stores: how v2 links them (LOG-008)

The domain stores in §4.3 keep their data. v2 gives each one the trace, and one event per record, so everything can be reached from the log book:

- **The trace goes everywhere:**
  - route decisions;
  - cron history entries;
  - archive events;
  - research events;
  - usage rows.

  A sub-agent's run records its parent's trace, which becomes `related_id` in events.
- **Fixes to run records:**
  - `runs.db` prunes on a timer, not only when it opens;
  - eval turns are kept out of the operator's `runs.db`, or tagged so the viewer can hide them.
- **Approval fidelity:**
  - approval IDs are unique across processes: the boot ID plus a counter, in place of `ap-N`;
  - an outcome says which it was: an operator's deny, a shutdown's deny, or expiry;
  - `policy:<cat>:<action>` gets a per-prompt suffix, so repeated prompts in one turn stay separate records.
- **One event per record, linking back:**
  - `route` events carry `decision_id` and link to `/route why`;
  - memory events carry `fact_id` and link to the provenance inspector (TEN-297);
  - cron events carry `job_id`.
- **Whole-file rewrites:** `route-decisions.jsonl` and `cron-history.json` are rewritten whole under a lock held in only one process. Serve and the TUI can therefore lose each other's rows. LOG-008 moves both to append-and-compact under `datalock`.

### 8.7 Components: what is running now

The Components view (§9.2, LOG-031) is fed from three places:
- `lifecycle` registrations, which gain a start hook alongside the stop;
- explicit registration for what `lifecycle` doesn't know about today: the iMessage responder, the watchers, the peer heartbeat and the turn gate;
- child-process capture (§8.4).

Every component has a name, a kind (subsystem, connector, server, child, job), a log and a state. Each state change is a `system` event with the component's activity, so "when did Discord last reconnect, and why?" is a single query.

## 9. Viewers

### 9.1 Discovery: the questions the viewer must answer

The acceptance test for discovery (the "discovery drill") scripts each question below and requires it to be answered in no more than three actions. The drill grows with the milestones, because questions 5–7 need the catalog and the Security log.

| # | Question | How it is answered | Passes from |
|---|---|---|---|
| 1 | What is running right now, and is it healthy? | The Components view | M3 |
| 2 | What has gone wrong recently? | The Summary page, then Administrative Events | M3 |
| 3 | What happened at 14:03? | A time slice across all logs, for example `since:14:00 until:14:10` | M3 |
| 4 | Why did this fail, and what else did that run do? | The event's Correlated tab: the run, its approvals, its tool calls and its child output | M3 |
| 5 | What does this event mean, and what should I do? | The event's Help tab, from the catalog entry | M4 |
| 6 | Who did this, from where, and with whose approval? | Security records: actor, surface, `approval_id` | M5 |
| 7 | Has this happened before? | Statistics for the event ID: count, first seen, trend | M4 |

### 9.2 Dashboard: the Logs page

```
+ Logs ------------------------------------------------------------------------------------------------+
| v Custom Views         | discord   level>=warning since:24h          [live]   231 of 12,345 | Actions   |
|   Administrative (12)  | Lvl  Time          Provider  ID    Task        Message              | Filter... |
|   Security failures    | WRN  14:03:11.123  discord   2001  Connection  Relay disconnected...| Save view |
|   This run             | ERR  14:02:58.990  discord   3004  Delivery    Message to #ops fa...| Find...   |
| v Tenant               | ...                                                                  | Export... |
|   System  Security     +----------------------------------------------------------------------+ Properties|
|   Turns   Sub-agents   | [General] Details  Correlated  Help                                  | Clear log |
| v Subsystems           | Relay disconnected: gateway reset; retrying in 5s                    | Debug 30m |
|   Discord (3 warnings) | discord 2001 / Warning / task Connection / opcode fail              |           |
|   Cron  Sandbox  ...   | 14:03:11.123 local (21:03:11.123 UTC) / serve pid 4412 / boot 7c1e.. |           |
| v Integrations         | activity 5d2a.. (Discord connection) -> 4 related events    [open]   |           |
|   MCP > github         | seen 14 times in 24 h, first 09:12                                   |           |
| > External             |                                                                      |           |
| > Components           |                                                                      |           |
+------------------------------------------------------------------------------------------------------+
```

The layout follows Event Viewer:

- **Tree** (left):
  - Custom Views: the built-ins and your own;
  - Tenant: System, Security, Turns, Sub-agents;
  - Subsystems, grouped: Channels (Discord, iMessage), Scheduler (Cron), Integrations (MCP ▸ each server, Plugins ▸ each plugin), Model (router and backends), Memory (Memory, Improve), Sandbox, Dashboard, Peering, Backup and Privacy, Eval;
  - External: one entry per foreign source;
  - Components.

  A log appears the first time it is written to.
- **Grid** (center):
  - columns: level, time, log, provider, event ID, task, message, with a column chooser;
  - sorting, and cursor paging over `seq` that loads more rows as you scroll;
  - a live-tail switch;
  - a count ("12,345 events; 231 match").
- **Detail** (bottom):
  - **General:** the message and key facts. Time is shown in local time and UTC, and trace and activity links pivot straight away.
  - **Details:** every field, with sensitive fields masked until revealed. Revealing a field writes a `security` event. The raw JSON is shown too.
  - **Correlated:** the timeline from §7, with the run summary.
  - **Help:** the catalog entry, plus "seen 14 times in 24 h; first seen …".
- **Actions** (right):
  - Filter this log; Clear filter; Save as custom view; Find (full text);
  - Export (NDJSON, CSV or text);
  - Properties: size, limits, when full, minimum level, "debug for 30 minutes";
  - Clear log; live tail; copy a link to the event (`/logs/event/{seq}`).
- **Summary** (the tree's root):
  - counts by level per log over 1 hour, 24 hours and 7 days, with sparklines;
  - top event IDs and the latest critical events and errors;
  - a table of logs showing size, limit, retention, last write, and dropped and suppressed counts;
  - unexpected exits.
- **Components:** every subsystem and child process with its state (starting, running, degraded, stopped, failed), uptime, last error and a link to its log, pre-filtered.

The existing Activity tab stays, as the live, friendly feed. The Logs page is the place for investigating.

**How it is built.** It follows the dashboard's current stack:
- pages rendered on the server, from `internal/dashboard/templates`;
- Datastar SSE patches for live rows, as `/activity/events` already does;
- the grid fetched a page at a time by cursor, and the detail pane fetched on selection.

There is no single-page app and no new front-end framework.

### 9.3 TUI: `/logs`

For one operator, the dashboard and the CLI are enough viewers. So the TUI gets a light view first:

- `/logs <filter>` shows the matching events in a scrollable pane, using the same filter syntax;
- `/logs follow <filter>` tails live;
- `enter` opens an event's detail, and `c` its correlated timeline.

A full-screen tree, list and detail viewer is deferred until the lighter view shows it's needed.

### 9.4 CLI: `tenant logs`

These are the journalctl and wevtutil equivalents.

- All of them are registered in `internal/cmdreg`, so help and completion come free.
- Every read command takes `--format text|json|csv`.
- **Local or remote.** Every command runs locally, reading `logbook.db`, when this account can open it. Otherwise it goes through the dashboard API. `--url` forces a server, which covers a remote machine or a service running as another account. The API holds no feature the local path lacks, and the reverse is true too.

```
tenant logs                                 list logs: size, limit, retention, counts, last write
tenant logs query [FILTER] [--follow] [--limit N] [--reverse]
tenant logs show <seq>                      one event in full, with its catalog help
tenant logs trace <trace|activity>          the correlated timeline plus the run record
tenant logs stats [FILTER]                  counts by log, level and event ID over time
tenant logs export [FILTER] --out FILE [--include-sensitive]
tenant logs verify [security]               integrity check; for security, the chain and archives too
tenant logs config <log> [--max-size] [--max-age] [--on-full] [--min-level]
tenant logs clear <log> [--export FILE]
tenant logs debug <provider> --for 30m      write a provider's debug records for a set time
tenant logs catalog [provider] [--id N]     the event reference
tenant logs components                      what is running now
```

- `tenant log <source> "<msg>" k=v …` (the writing command) stays as it is.
- `tenant attach` keeps its URL form for remote machines. Run locally with no URL, it behaves like `tenant logs query --follow`.
- **Exports describe themselves.** An export starts with a header record naming the schema version, the catalog version, the filter, the export time and the host, so a file still makes sense on another machine a year later. Exporting is recorded in `security`.

### 9.5 API

The API sits behind the dashboard's existing bearer authentication and `internal/httplimits`.

| Method | Path | What it does |
|---|---|---|
| `GET` | `/api/events?q=&cursor=&limit=` | Query. Returns events plus the next cursor, which is `<store ID>.<seq>`. A cursor from another store starts over and says so. |
| `GET` | `/api/events/{seq}` | one event, with its catalog entry |
| `GET` | `/api/events/stream?q=` | SSE live tail. It resumes from `Last-Event-ID`, a cursor as above, with no missed rows. |
| `GET` | `/api/events/stats?q=&bucket=` | aggregates for the Summary page |
| `POST` | `/api/events` | ingest one event (JSON) or a batch (NDJSON). Reserved providers are refused (§10.4). |
| `GET`, `PATCH` | `/api/logbook/logs[/{log}]` | list logs; read or change a log's properties |
| `POST` | `/api/logbook/logs/{log}/clear` | clear a log; writes a `security` event |
| `GET`, `POST`, `DELETE` | `/api/logbook/views[/{name}]` | saved views |
| `GET` | `/api/logbook/catalog[/{provider}]` | the event reference |
| `GET` | `/api/logbook/components` | what is running |

The v1 routes (`/api/logs…`, `/api/activity`, `/api/addon/event`) become thin shims over the store and keep passing their current tests.

## 10. The Security log

### 10.1 What it records

Each item below is written by the code that performs the action, synchronously, before the action returns:

- **Approvals.** Requested, granted (once, for the session, or always), denied by the operator, denied by shutdown, expired. Each record carries its category, action, detail, surface, actor, `approval_id` and `trace_id`. This includes the paths that leave no record today:
  - Discord card verdicts, with the nonce and the Discord user who clicked;
  - iMessage replies;
  - clicks on external approval links;
  - the cron-exec auto-approver, which today bypasses the broker;
  - peer pairing;
  - changes to category modes.
- **Execution.** Every `os_exec` and cron shell command, with the scrubbed command, sandbox backend, exit code, approval and trace. Also sandbox refusals and escalations (today these are only in the `sandbox` folder), and each change to the relay's exec mode.
- **Permissions and trust.** Changes to tool rules and saved modes; connector, MCP-server and skill trust changes; plugins enabled or disabled.
- **Configuration.** Each change, with a diff where secret values are replaced by their names.
- **Secrets.** Stored, changed or deleted, by ID only, never by value.
- **Access.** Dashboard sign-ins, failed tokens, CSRF and rate-limit rejections; peer pairing and unpairing.
- **Scheduled jobs.** Jobs added, changed or removed, whatever the surface. The separate audit item D4 is reviewing how `cron_add` is gated.
- **Data lifecycle.** Privacy deletions and exports; backups and restores.
- **The log book itself.** Logs cleared or reconfigured; exports; sensitive fields revealed; integrity checks passed or failed.

### 10.2 Integrity

- **Every link is keyed.** Each record's `hash` is `HMAC-SHA256(key, previous hash ‖ canonical header ‖ data_hash)`.
  - So nobody without the key can forge, edit, reorder or drop a record anywhere in the chain. A plain SHA-256 chain with occasional signed checkpoints would have left up to 1,000 records open to rewriting.
  - The HMAC costs microseconds per record.
- **The data is committed to, not contained.** `data_hash = SHA-256(salt ‖ msg ‖ data)`, with a random 16-byte salt per record.
  - A privacy tombstone deletes the message, the data and the salt, and keeps `data_hash`. The chain still verifies.
  - With the salt gone, nobody can confirm a guessed value.
  - The message is covered too, because templates render fields into it.
- **Anchoring the head outside the database.** A chain can't reveal its own truncation, or a whole older copy restored in its place, because both are valid prefixes. So:
  - every 1,000 records, or every hour, the head `(rec, hash)` is also written to `<config>/logbook.anchor`, outside the data directory;
  - `verify` compares the chain with the anchor.
  - A legitimate restore (`tenant backup restore`) writes a **re-anchor** record: the restored head, the head it replaced, and the backup it came from. So a restore is visible, not mistaken for tampering.
- **An outside witness (optional, recommended for the enterprise case).** At each anchor, the head is also sent to one witness the operator picks: a federated peer, the operator's Discord DM, or a file on another machine. Only a witness makes truncation and rollback detectable by someone holding the operator's full privileges.
- **The key.**
  - It is created once, in the config directory, with `secrets.CreateExclusive`. That gives a file only the owner can read: owner-only permissions on POSIX, and a current-user ACL on Windows.
  - On Windows it is also sealed with the user protector (DPAPI) when one is available (`secrets.NewUserProtector`). POSIX has no equivalent.
  - The key file and the anchor are added to the sandbox's deny roots, so no sandboxed command, MCP server or plugin child can read them.
- **Archive, never overwrite.** When `security` reaches its limit, its oldest records move to `archive/security-<first>-<last>.ndjson.gz`. Each file ends with its last `(rec, hash)`, so verification continues across files.
- **Verification.** `tenant logs verify security` walks the archives, then the database, then checks the anchor and the witness when there is one. It reports the first break by record number.
- **What this proves, and what it doesn't:**

  | Tampering by | Detected? |
  |---|---|
  | Anything without the key: a plugin, an MCP server, a sandboxed command, an external script, a bug, a disk error; including edits, deletions, reordering and forged records | Yes |
  | Anything without the key, truncating or rolling back the database | Yes, against the anchor |
  | Something running with the operator's full privileges, such as the agent's `os_exec` on the host backend | Only with a witness. Without one, it can read the key and rewrite chain and anchor together. |

### 10.3 When an audit write fails

A Security record doesn't wait in the queue. It joins the batch the writer is building and forces that batch to commit, and its `Audit` call returns only after the commit. The measured p99 is 18 ms idle and 110 ms while a full batch commits (§12.3).

If the write fails, the failure itself becomes a critical `system` event and a dashboard banner. Then `logs.security.on_failure` decides what happens:

- `continue` (the default): the action goes ahead;
- `deny`: gated actions are refused until writes succeed again. This is for operators who want the strict behavior.

### 10.4 Who can write it

- Only code inside the binary can write `security` or any provider under `tenant.*`.
- Anything arriving through `POST /api/events`, `tenant log`, a spool file or a foreign folder is filed with keyword `external` and origin `external`. A script can therefore never forge an audit record.

## 11. Privacy and safety of the log itself

- **Secret scrubbing, before anything is stored.** It covers the message and every string field. The order is: scrub, then truncate, then mirror and store. Scrubbing after the 4 KB cut would let a secret that straddles it escape; v1 does exactly that today (`hub.go:236-246`).
  - **Where it runs.** In v2 it runs in the writer goroutine, off the emit path, so it costs callers nothing. In phase 0 it runs in v1's `Hub.Log`.
  - **Known values.** These are the stored credentials, the dashboard token, MCP `env_set` values and anything else the secrets layer holds, each at least 8 characters long.
    - They are matched exactly, and also as base64, URL-encoded and JSON-escaped text, and replaced with `«secret:<name>»`.
    - The set is refreshed whenever a credential or an OAuth token changes.
  - **Pattern detectors** catch:
    - bearer tokens and JWTs;
    - `sk-`, `sk_live_`/`rk_live_`, `ghp_`/`github_pat_`, `xox?-`, `AIza…`, `tskey-` and AWS keys;
    - Discord bot tokens and webhook URLs;
    - PEM private keys and `user:pass@` in URLs;
    - `password=` / `token=` parameters, and JSON `"password"`, `"secret"`, `"token"` and `"api_key"` fields.

  This closes the gap LOGGING.md admits: today a secret typed into a command is written to the log. A test corpus guards every form above.
- **Sensitive fields.** The catalog marks fields that hold personal content or payloads: message text, prompts, tool arguments and results. By default (decision D5), such a field:
  - is stored, so investigations still work;
  - is masked in viewers until revealed, and each reveal is a `security` event;
  - never appears in the message (a template may name only non-sensitive fields), in the full-text index or in the mirror;
  - is left out of exports unless `--include-sensitive` is given;
  - is deletable by privacy requests.

  `logs.store_sensitive: false` stops storing these fields at all. A tool result that the information-flow labels (TEN-311) mark sensitive is treated the same way.
- **Truncation.** Values are still cut at `max_field_bytes` (4 KB by default). A cut value now also records its original size and its SHA-256, so two truncated payloads can still be compared. The full body stays in `runs.db` unless `runs.redact_tool_bodies` is set.
- **Injection.** Protection depends on where the text is shown:
  - in the mirror, every control character in the message is escaped (a change from v1; §6.3);
  - in the TUI, ANSI escape sequences are stripped before display;
  - in the dashboard, text is HTML-escaped.

  A child process's output is filed by its parent under the child's log, so a child cannot write into another log.
- **Log text is untrusted input.** Two features feed log content onward:
  - **Alerts sent off the machine** (LOG-070) carry only the message, the level, the IDs and a dashboard link. They never carry data fields, and they are scrubbed again on the way out.
  - **An agent investigation** (LOG-071) receives log text marked untrusted with TEN-311's labels, and runs with read-only tools. A crafted log line then can't steer it.
- **File protection.**
  - **POSIX:** `logbook.db`, the mirror, the archives, the spool and crash files are created through `fsperm`, owner-only.
  - **Windows:** `fsperm` is a no-op today (`internal/fsperm/restrict_windows.go`; TEN-325), so these files inherit the data directory's ACL until TEN-325 lands.
  - **The Security key** gets its own current-user ACL from `internal/secrets` on every OS (§10.2).

## 12. Reliability, performance and self-monitoring

### 12.1 The write path

- **Queue and writer.** Each process has one bounded queue (64k events) and one writer goroutine, holding the process's one writer connection. The writer takes a batch every 100 ms or every 512 events, whichever comes first, and for each batch:
  1. scrubs it;
  2. truncates values;
  3. appends it to the mirror;
  4. commits events, FTS rows, rollups and chain links in one transaction;
  5. wakes subscribers.
- **Security records** don't wait for the next batch. Each joins the batch being built and forces that batch to commit, and its `Audit` call returns after the commit (§10.3).
- **Viewers read the store, not a ring.** A live subscriber reads `seq > cursor` from `logbook.db`.
  - After each local commit, it is woken through its own channel. There is no shared one-slot channel, which is v1's bug.
  - It also polls every 500 ms, to pick up commits from other Tenant processes.
  - Read transactions stay short, so WAL checkpoints can finish.

  The live view is therefore exactly what is on disk: at most about 100 ms behind for this process's events, and about 500 ms behind for another process's.
- **Queue-full policy by channel:**

  | Channel | When the queue is full |
  |---|---|
  | `security` | Never queued: it forces a commit |
  | operational | Wait up to 1 ms, then drop and count |
  | diagnostic | Drop the newest and count |
- **Rate limiting.** Each (provider, event ID) pair gets a token bucket: 100 per second, with bursts up to 500.
  - Uncatalogued events (ID 0) are bucketed by provider *and* message template: the message with its numbers and quoted values stripped. So one chatty line can't silence the rest of its provider.
  - `security` is never rate-limited.
  - Once a minute, a `logbook` event reports what was suppressed, for example "suppressed 1,203 × mcp.github 3001 in the last 60 s".
- **Write failures** cover a full disk, a lock held past the timeout, and I/O errors.
  - The writer retries with backoff.
  - After three failures, the process runs on the mirror alone. That works because the mirror is written before the commit. A critical event and a banner are raised.
  - It keeps retrying. On recovery, the housekeeper imports the mirror's lines from the outage (keyword `recovered`), so the store has no hole.
- **Quarantine.** Every process holds `<data>/logs/logbook.lock` shared. Quarantine, recovery and restore take it exclusively.
  - A corrupt database found at open is moved aside **together with** its `-wal` and `-shm` files to `logbook.db.corrupt-<time>`, and a fresh store starts with a new store ID. A critical event is written, and `tenant logs recover` salvages what it can.
  - A process that can't get the exclusive lock, because others are running, doesn't move files they have open. That would fail on Windows and corrupt the database on POSIX. It runs on the mirror alone and reports the corruption.
- **Clock.** Events carry wall time, and their order comes from `seq`. A jump of more than 2 seconds between wall and monotonic time writes a `system` warning.

### 12.2 Start, stop and crashes

- **Opening first, closing last.** The log book is the first thing opened and the last thing released. It is registered first in the `lifecycle` Release stage, and that stage runs last-registered-first. Its final flush is bounded at 2 seconds.
- **Boot records,** for serve and the TUI only. A one-shot command keeps none, so its Ctrl-C never reads as a crash.
  - **Each process** writes a `boots` row when it starts, and holds an exclusive lock on `<data>/logs/boots/<boot_id>.lock` for its lifetime. The lock uses `datalock`'s mechanism, which already works on all three OSes. It marks the row `clean` when it stops.
  - **Detection.** A row with no clean stop whose lock can be taken belongs to a dead process, because the OS drops a lock when its process exits. The row then becomes a critical `system` event, "the previous run ended unexpectedly", carrying the time of its last event, and is marked `reported`.
  - **When it's checked:** at start, and every minute by the housekeeper. So a serve crash while the TUI is open is reported within a minute.
  - **Heartbeats.** `heartbeat_ns` is refreshed every 30 seconds, only to estimate when the process died. Age alone never decides it: after a laptop wakes, a live process would look dead.
- **Crash output.**
  - At start, serve and the TUI call `debug.SetTraceback("all")`, so an unrecovered panic dumps every goroutine, not only its own.
  - They also call `debug.SetCrashOutput`, which sends the runtime's fatal output to `<data>/logs/crash/<boot_id>.txt`.
  - **A clean stop** calls `SetCrashOutput(nil)` first, then deletes the file if it is empty. Go opens files without `FILE_SHARE_DELETE`, so on Windows the runtime's copy of the handle must be released before the file can be deleted.
  - **Reading it in.** A crash file is read in only once its process's boot lock is free, so a crash file of a process still running is never read. It is filed as a critical event, with the stacks attached.
- **Recover sites.** Every `recover()` in a long-running goroutine logs a critical event with the stack, instead of swallowing the panic.

### 12.3 Performance budgets

**Why these numbers.** They are based on the review's measurements of the first draft's schema (§6.1), and on Tenant's expected peak of a few hundred events a second.

**How CI enforces them.** The benchmarks run on all three OSes, and fail on a regression of more than 20% against that OS's stored baseline. The absolute targets are checked on a reference machine, not on shared CI runners.

| Path | Budget | Measured on the draft schema |
|---|---|---|
| A record below the level floor | no allocations, under 50 ns | — |
| An enabled emit, up to the moment it is queued | p99 under 20 µs; no scrubbing on this path | — |
| Writer throughput | at least 3,000 events/s sustained, about ten times the expected peak | 3.9k/s (4.4k with a 64 MB cache) |
| A `security` write | p99 up to 25 ms idle; up to 150 ms while a batch commits | 18 ms; 110 ms |
| Retention | never holds the write lock over 50 ms (chunks of 500 rows or fewer) | 0.7–1.0 s for one 10,000-row delete |
| The Summary page | under 50 ms at any size (it reads rollups) | 320 ms at 200k rows, grouping over events |
| A filtered query over 1M events, on indexed fields | p95 under 200 ms | — |
| `data.<key>` filters and full text | p95 under 1 s over 1M events. A key used often can get a generated, indexed column. | 205 ms for a `json_extract` filter at 200k rows |
| Live latency, from emit to the dashboard | under 1 s | — |
| Disk | at most 1 KB per event, indexes and FTS included; budget arithmetic uses 1 KB | 972 bytes |

### 12.4 Self-monitoring: the `logbook` provider

The log book writes events about itself for:
- the store opening (size, schema version, store ID);
- a write failure, entering and leaving mirror-only mode, and the recovery import;
- the queue reaching its high-water mark;
- events dropped or suppressed, per provider;
- a log reaching 80% or 95% of its limit, and what was done when it filled (overwrite or archive);
- the WAL passing its checkpoint threshold;
- low disk;
- the housekeeper lease changing hands;
- a dead process reported;
- a crash file read in;
- the v1 import finishing;
- a mirror error;
- a clock jump;
- an integrity check passing or failing.

The Summary page and `tenant logs` show the counters.

## 13. Delivery plan

There are seven phases.

- **Phase 0** is a set of small fixes. Each improves v1 immediately and is needed by v2 anyway.
- **From phase 1 on, v2 runs beside v1.** The store is written in addition to the v1 files (the mirror *is* the v1 files), and the new viewers appear beside the Activity tab. There is no cut-over day.
- **Rolling back** means setting `logs.store: false`, and works until LOG-022. After that, the v1 routes read from the store, so the store is required; the mirror stays as a readable copy.

| Milestone | Phase | What the operator gets |
|---|---|---|
| **M0: v1 gaps closed** | 0 | <ul><li>no secrets in the logs</li><li>lines and every record store linked to their runs</li><li>no stray output</li><li>child-process output visible</li><li>crashes recorded</li><li>no forged records</li></ul> |
| **M1: the store** | 1 | `logbook.db`, retention budgets, self-monitoring, v1 history imported, and doctor coverage |
| **M2: query** | 2 | `tenant logs …`, local or remote, and API v2 with SSE |
| **M3: the viewer** | 3 | the dashboard's Logs page, Summary, Components, the TUI's `/logs`, and discovery questions 1–4 |
| **M4: catalogued** | 4 | event IDs and documentation for every subsystem, and discovery questions 5 and 7 |
| **M5: trusted** | 5 | the Security log, with its keyed chain, anchor, verify and audit export, and discovery question 6 |
| **M6: alerting** | 6 | alert rules and notifications |

**Sizes.** S is up to 2 days, M is 3–5 days, and L is 1–2 weeks.

**Totals.** Done one after another, the tickets add up to about 170 engineer-days (roughly 34 weeks):

| Phase | Engineer-days |
|---|---|
| 0: v1 gaps | ~37 |
| 1: the store | ~33 |
| 2: query and CLI | ~19 |
| 3: the viewer | ~13 |
| 4: catalog | ~40 |
| 5: Security log | ~16 |
| 6: alerts | ~12 |

M3, the usable viewer, is about 100 of those days.

**Parallel work.** Most of phase 0 is independent, and so are phase 4's provider tickets. Phases 4 and 5 can overlap phase 3. With the one-branch-per-ticket workflow used for the July audit, calendar time is a fraction of the serial total.

The IDs (LOG-001 …) are the plan's own. They were filed in Jira on 2026-09-27; see *Jira* at the end of this section.

### Phase 0: close the v1 gaps (M0)

LOG-006 goes first. LOG-002 and LOG-003 start persisting new output (OAuth prints, host commands, MCP stderr), and must not do so before the scrubber exists.

| ID | Ticket | Acceptance criteria | Size |
|---|---|---|---|
| LOG-006 | Scrub secrets on the log path | <ul><li>The §11 scrubber runs in `Hub.Log`, before the value cap, the ring and the files.</li><li>It catches known values in raw, base64, URL-encoded and JSON-escaped form, plus the §11 patterns.</li><li>A test proves no configured secret appears in any source. It also covers the separate dashboard-token fix.</li><li>The `sandbox` folder's command field is scrubbed, and so are tool arguments and results in `turns`.</li><li>Corpus tests.</li></ul> | M |
| LOG-001 | Link lines to runs | <ul><li>Every turn-scoped event carries `trace`, not just `turn_start`.</li><li>`EntryFor` writes it.</li><li>The slog handler adds `trace` from `runtrace.IDFromContext(ctx)`.</li><li>`/api/activity` and `tenant attach` accept a `trace` filter.</li><li>Tests.</li></ul> | S |
| LOG-002 | Send stray output to the log book (needs LOG-006) | <ul><li>Serve and the TUI call `slog.SetDefault` and `log.SetOutput` pointing at the hub.</li><li>`httplimits.Configure` sets every `http.Server`'s `ErrorLog`, and all servers call it (§8.2).</li><li>The §8.2 list is rerouted: the eval harness logger, chromedp, OAuth prints.</li><li>When the hub fails to open, the memory hub is installed and the failure is shown.</li><li>Serve's `pushSys` never drops a line before it reaches the hub.</li><li>These now reach the log book: the TUI-only status lines; the iMessage agent's turns (it gets an observer); relay and cron agent events under the TUI; the sub-agent logger's source; the three unused loggers.</li><li>An archtest ratchet forbids new `fmt.Fprint*(os.Stderr` or `log.Print*` calls in long-running packages.</li></ul> | L |
| LOG-003 | Capture child-process output (needs LOG-006) | <ul><li>Each local MCP server's stderr is filed under `mcp` with `server=`, rate-limited. The 8 KiB tail stays for start errors.</li><li>Sandbox runs file their exit and stderr tail.</li><li>`os_exec` on the host backend is logged, with the command scrubbed.</li><li>Start and exit lines carry pid, backend, limits, exit code and duration.</li><li>Tests use the `mcpecho` fixture.</li></ul> | M |
| LOG-004 | Crashes and unexpected exits | <ul><li>Serve and the TUI call `debug.SetTraceback("all")` and `debug.SetCrashOutput` into `<data>/logs/crash/`. The next start files the output as critical, with the stacks.</li><li>A clean stop calls `SetCrashOutput(nil)` before removing an empty file.</li><li>A boot record, a lock held for the process's lifetime and a clean-stop marker detect unexpected exits. A test proves a live process is never reported dead.</li><li>All nine `recover()` sites log a stack (§8.5).</li><li>Tool dispatch recovers a plugin's panic and returns an error result, instead of crashing Tenant.</li><li>Tests on all three OSes, using a helper process that panics and one that is killed.</li></ul> | M |
| LOG-005 | Swallowed errors | <ul><li>Each of the 22 paths in §8.3 either logs with context or is documented as intentional.</li><li>"Approve always" and `persistLaunchConfig` failures reach the operator, not only the log.</li><li>Tests for representative cases.</li></ul> | M |
| LOG-007 | v1 viewer and API fixes | <ul><li>Each subscriber gets its own wake-up.</li><li>Sequence numbers get an epoch, so reconnecting clients re-sync.</li><li>`/api/activity` gets a `limit`.</li><li>`POST /api/logs/{source}` and `/api/addon/event` refuse the built-in sources and strip `ev=`.</li><li>`tenant attach` accepts flags after the URL.</li><li>`tenant log`, doctor and the reference client read the token file.</li><li>`logs.level` accepts `warn` and `error`.</li><li>Privacy retention applies to a running Tenant.</li><li>The tab order learns `sandbox`, `privacy` and `shutdown`.</li><li>The tutorials are corrected.</li></ul> | M |
| LOG-008 | Link the record stores | The §8.6 changes: <ul><li>the trace goes onto route decisions, cron history, archive events, research events and usage rows;</li><li>a sub-agent's run records its parent trace;</li><li>`runs.db` prunes on a timer, and its DSN gains `_txlock=immediate`;</li><li>eval turns are kept out of the operator's `runs.db`;</li><li>approval IDs are unique across processes, and an outcome records its cause;</li><li>the route log and cron history no longer lose rows when serve and the TUI both write.</li></ul> | L |

### Phase 1: the store (M1)

| ID | Ticket | Acceptance criteria | Size |
|---|---|---|---|
| LOG-010 | Event model, the `event` leaf, and the catalog skeleton | <ul><li>The §5 packages: `internal/logs/event` imports nothing internal.</li><li>The archtest rule changes from §5.</li><li>Five levels; operational and diagnostic channels; typed fields with a sensitivity flag.</li><li>Definitions are validated when they register and again in a test: IDs are unique, and templates name only declared, non-sensitive fields.</li><li>v1's `Entry`, slog and `EntryFor` map onto v2 with no change for their callers.</li></ul> | M |
| LOG-011 | The `logbook.db` writer | <ul><li>The §6.1 schema and pragmas: `_txlock=immediate`; `synchronous=FULL`, plus `fullfsync` on macOS.</li><li>One writer connection per process, with the §12.1 pipeline: scrub, truncate, mirror, commit, wake.</li><li>A Security record forces its batch to commit.</li><li>Cursors carry the store ID.</li><li>**No missed rows:** with three writer processes and 1M events, a reader tailing from a cursor sees every committed row exactly once.</li><li>Killing a writer in the middle of a batch loses at most that uncommitted batch, which is still in the mirror, and `PRAGMA integrity_check` passes afterwards.</li><li>Benchmarks meet §12.3.</li></ul> | L |
| LOG-012 | Retention and budgets | <ul><li>Each log has a maximum size, a maximum age and a when-full action.</li><li>Deletes run in chunks of 500 rows or fewer.</li><li>The WAL checkpoint policy and incremental vacuum.</li><li>Low-disk watermarks.</li><li>The measured file sizes (database plus WAL) stay within 10% of the budget.</li><li>A test for each when-full action.</li></ul> | M |
| LOG-013 | Mirror and intake | <ul><li>The mirror is written from the pipeline, before each commit. It escapes control characters and writes `«sensitive»` for sensitive fields, and the reference client's byte-for-byte tests still pass.</li><li>Foreign folders skip every Tenant log and file into `ext.<name>`; reserved names are refused.</li><li>The spool uses the temp-then-rename protocol.</li><li>The housekeeper imports v1 history once, scrubbed.</li><li>After an outage, the mirror's lines from the outage are imported.</li><li>A test proves one process's mirror is never re-read as another's external log.</li></ul> | M |
| LOG-014 | Self-monitoring and resilience | <ul><li>The `logbook` provider's events (§12.4), and the dropped and suppressed counters.</li><li>Rate limits: uncatalogued events are bucketed by template, and `security` is exempt.</li><li>Mirror-only fallback, then recovery.</li><li>Quarantine takes the store lock exclusively and moves the database with its `-wal` and `-shm`.</li><li>The housekeeper lease.</li><li>Boot records move into `boots`, with the periodic dead-process check.</li><li>The log book registers with `lifecycle`.</li><li>Fault-injection tests.</li></ul> | L |
| LOG-015 | Backup and privacy | <ul><li>`logbook.db` is backed up with `VACUUM INTO`, never a raw file copy.</li><li>A `logs` privacy data class.</li><li>Deletion follows the house recipe and rewrites the affected mirror files and archives.</li><li>A preview of what will go.</li><li>A privacy export includes sensitive events.</li><li>`privacy status` reports the backups that still hold deleted data.</li></ul> | M |
| LOG-016 | Doctor and `security audit` know the log book | <ul><li>**Doctor** checks: integrity; WAL size; budget use; the lease holder; mirror-only mode; dropped counts; a relocated `logs.dir`.</li><li>**`security audit`** reports: file protection (and TEN-325's state on Windows); whether the scrubber is on; a scan for known secrets in the logs; the Security log's last `verify`; whether a witness is set.</li></ul> | S |

### Phase 2: query and command line (M2)

| ID | Ticket | Acceptance criteria | Size |
|---|---|---|---|
| LOG-020 | Filter language and query engine | <ul><li>The Appendix A grammar, with a property test that printing and re-parsing a filter round-trips.</li><li>The planner writes `level <= 3` literally where the administrative index applies, and answers statistics from rollups.</li><li>p95 targets met on a fixture of 1M events.</li></ul> | L |
| LOG-021 | `tenant logs` | <ul><li>The §9.4 commands, registered in `cmdreg`.</li><li>They run locally, or through the API when the store isn't readable or `--url` is given.</li><li>`tenant attach` keeps its URL form.</li><li>Golden tests for the text, JSON and CSV output.</li></ul> | M |
| LOG-022 | API v2 and ingestion | <ul><li>The §9.5 routes, with auth and `httplimits`, and cursors that carry the store ID.</li><li>SSE that resumes from `Last-Event-ID`.</li><li>`POST /api/events` takes one event or an NDJSON batch, refuses reserved providers, and marks what it takes as external.</li><li>The v1 routes become shims and keep their tests. **This ends the rollback window.**</li><li>Reference client v2: the spool protocol, and correlation from the environment.</li></ul> | L |

### Phase 3: the viewer (M3)

| ID | Ticket | Acceptance criteria | Size |
|---|---|---|---|
| LOG-030 | The dashboard's Logs page | <ul><li>The §9.2 tree, grid, detail pane and actions.</li><li>Cursor paging.</li><li>Saved views, export, properties, clear, and permalinks.</li><li>Discovery questions 1–4 pass as `httptest` handler tests.</li><li>A chromedp check, skipped when Chrome is absent, confirms the page works in a real browser.</li></ul> | L |
| LOG-031 | Summary and Components | <ul><li>A component registry, fed by `lifecycle` and by child-process capture, with state changes as `system` events.</li><li>The Summary page reads rollups.</li><li>Every subsystem serve starts appears with its state and a link to its log.</li></ul> | M |
| LOG-032 | The TUI's `/logs` | <ul><li>The §9.3 light view, through a `LogsControl` interface.</li><li>Tests use a fake control, in the style of `cron_tui_test.go`.</li><li>The feed can be scrolled from the keyboard.</li></ul> | S |

### Phase 4: catalog coverage (M4)

| ID | Ticket | Acceptance criteria | Size |
|---|---|---|---|
| LOG-040 | Catalog tooling | <ul><li>The definition API (in place since LOG-010): every definition documented with `Doc(meaning, cause, action)`, all three required.</li><li>A generator for `docs/EVENTS.md`, with a drift test.</li><li>A coverage figure per provider: the share of *static* warning-and-above call sites that carry an ID, counted by a test that parses each package.</li><li>An archtest ratchet.</li><li>Discovery questions 5 and 7 pass.</li></ul> | M |
| LOG-041 … LOG-053 | One ticket per provider group:<ul><li>system and lifecycle;</li><li>turns and sub-agents, including the bus;</li><li>Discord and iMessage;</li><li>cron;</li><li>MCP, remote and local;</li><li>sandbox and exec;</li><li>model and router;</li><li>memory and improve;</li><li>dashboard requests and auth;</li><li>peering;</li><li>plugins, through an `Events` field on the TEN-321 descriptor;</li><li>backup, privacy and skills;</li><li>eval.</li></ul> | For each provider: <ul><li>its main operations are wrapped in activities;</li><li>at least 90% of its static warning-and-above call sites carry a catalogued ID, each with a meaning, a cause and an action;</li><li>its log shows up in the right tree group.</li></ul> | S–M each |

### Phase 5: the Security log (M5)

The chain comes first, so no Security record is ever written without it.

| ID | Ticket | Acceptance criteria | Size |
|---|---|---|---|
| LOG-060 | The Security log store (needs LOG-011, LOG-012) | <ul><li>The HMAC chain and salted commitments.</li><li>The anchor file, and a re-anchor record on each restore.</li><li>Archive when full, and the optional witness.</li><li>`tenant logs verify` and an audit export bundle.</li><li>The key is created through `secrets`, sealed with DPAPI on Windows, and on the sandbox deny list.</li><li>Tamper tests: an edit, a deletion, a reordering and a forged record are detected without the key; truncation and rollback are detected against the anchor; a legitimate restore reads as a re-anchor.</li></ul> | M |
| LOG-061 | Security events: approvals, execution, access (needs LOG-060, LOG-040) | <ul><li>Every approval path in §10.1.</li><li>Exec records, sandbox refusals and escalations, and the relay's exec mode.</li><li>Dashboard sign-ins and rejections; peer pairing.</li><li>The `on_failure` policy.</li><li>A test for each audited action, asserting its record.</li></ul> | L |
| LOG-062 | Security events: configuration, secrets, jobs, data, the log book | <ul><li>Config diffs, with secrets shown by name only.</li><li>Secret changes, recorded at their call sites.</li><li>Scheduled jobs; privacy actions; backup and restore.</li><li>Log administration and field reveals.</li><li>The `approvals` folder is kept for one release.</li><li>Discovery question 6 passes.</li></ul> | M |

### Phase 6: alert rules (M6)

| ID | Ticket | Acceptance criteria | Size |
|---|---|---|---|
| LOG-070 | Alert rules (needs LOG-012, LOG-014, LOG-020, LOG-022, LOG-060) | <ul><li>A rule is a filter, an optional threshold (N events in a window), an action and a throttle.</li><li>Actions: a dashboard notification; a Discord or iMessage message carrying only the message, level, IDs and a link, scrubbed; a cron prompt.</li><li>Built-in rules: any critical event; an unexpected exit; a burst of security failures; an unattended sandbox violation (TEN-347); a budget at 95%.</li><li>Alerts are themselves events.</li><li>A rule fires once per throttle window, and a test proves it.</li></ul> | L |
| LOG-071 | Investigate with the agent (optional; Q2) | <ul><li>An alert action that starts an agent turn with the correlated events attached.</li><li>The turn has a budget and needs approval.</li><li>The log text is labelled untrusted (TEN-311), and the turn runs with read-only tools.</li></ul> | M |

### Dependencies

```
LOG-006 -> LOG-002, LOG-003                         scrub before persisting anything new
LOG-001, LOG-004, LOG-005, LOG-007, LOG-008         independent
LOG-010 -> LOG-011 -> LOG-012, LOG-013, LOG-014, LOG-015, LOG-016
LOG-011 -> LOG-020 -> LOG-021, LOG-022 -> LOG-030, LOG-031, LOG-032
LOG-010 -> LOG-040 -> LOG-041 … LOG-053
LOG-011 + LOG-012 -> LOG-060 -> LOG-061, LOG-062      (LOG-061 and LOG-062 also need LOG-040)
LOG-012 + LOG-014 + LOG-020 + LOG-022 + LOG-060 -> LOG-070 -> LOG-071
```

### Where to start

The first sprint is the dashboard-token fix, plus the four phase 0 tickets with the most value per day:

| Order | Ticket | Why |
|---|---|---|
| 1 | **The dashboard-token fix** | Filed separately. It is a live credential leak. |
| 2 | **LOG-006** (scrubbing) | It closes the same class of problem for good, and everything that persists new output needs it. |
| 3 | **LOG-001** (trace IDs) | Every later feature pivots on correlation. |
| 4 | **LOG-004** (crashes and unexpected exits) | Today a crash under the Windows task leaves nothing behind. |
| 5 | **LOG-002** (stray output) | The log book can't be the one place to look while output still bypasses it. |

Phase 1 can start alongside, because LOG-010 depends on none of them.

### How this meets the open tickets

| Ticket | How it connects |
|---|---|
| **TEN-313** (OPS-004, Windows service) | <ul><li>A service should rely on the log book, not on whatever captures its stdout. LOG-002 and LOG-004 make that true on every OS.</li><li>A service running as another account is read through the API (§5).</li></ul> |
| **TEN-325** (Windows file ACLs) | Until it lands, log files on Windows inherit the data directory's ACL (§11). |
| **TEN-340** (router-classifier requests in the activity feed) | They land as events in the `route` log. No conflict. |
| **TEN-347** (alerts for unattended sandbox violations) | It uses LOG-070's rules. |
| **TEN-189/190** (the federation envelope and bridge) | When the envelope gains a trace field, it maps onto `trace_id` and `activity_id` (§7). |
| **TEN-300's OTLP TODO** | It stays deferred (D10). |
| **Audit item D4** (how `cron_add` is gated; a separate task) | Whatever that task decides, adding, changing or removing a job is on LOG-062's list, so the record exists either way. |

### Rollout

- **Phase 1:** `logs.store` switches the store on. It becomes on by default after a week's soak on the operator's machine. Until LOG-022, turning it off gives v1's behavior.
- **Phase 3:** the Activity tab stays and the Logs page is added beside it. Nothing is removed before M3.
- **After M5:** the `approvals` folder keeps being written for one more release.

### Jira

Filed 2026-09-27 under the workstream [TEN-359](https://findtime.atlassian.net/browse/TEN-359). Each ticket carries its own problem statement and acceptance criteria, labelled `logging-v2`. The critical-path dependencies are "blocks" links.

| Phase | Plan ID → Jira |
|---|---|
| First sprint | LOG-000 (the dashboard-token fix) → TEN-360 |
| 0 | LOG-006 → TEN-361 · LOG-001 → TEN-362 · LOG-002 → TEN-363 · LOG-003 → TEN-364 · LOG-004 → TEN-365 · LOG-005 → TEN-366 · LOG-007 → TEN-367 · LOG-008 → TEN-368 |
| 1 | LOG-010 → TEN-369 · LOG-011 → TEN-370 · LOG-012 → TEN-371 · LOG-013 → TEN-372 · LOG-014 → TEN-373 · LOG-015 → TEN-374 · LOG-016 → TEN-375 |
| 2 | LOG-020 → TEN-376 · LOG-021 → TEN-377 · LOG-022 → TEN-378 |
| 3 | LOG-030 → TEN-379 · LOG-031 → TEN-380 · LOG-032 → TEN-381 |
| 4 | LOG-040 → TEN-382 · LOG-041 … LOG-053 → TEN-383 … TEN-395, in order |
| 5 | LOG-060 → TEN-396 · LOG-061 → TEN-397 · LOG-062 → TEN-398 |
| 6 | LOG-070 → TEN-399 · LOG-071 → TEN-400 |

## 14. Testing and verification

- **Unit, golden and property tests:**
  - encoding and scrubbing;
  - the hash chain;
  - filter parsing, where a filter printed and parsed back must round-trip;
  - planning a filter into SQL;
  - CLI output (golden files);
  - the v1 line format, whose existing byte-for-byte tests stay as they are.
- **Fault injection.** The store's file-system and SQL layer can be swapped for one that fails on demand. Tests cover a full disk, a busy lock, a corrupt file, a killed process in the middle of a batch, and clock jumps.
- **Several writer processes.** A test starts helper processes (the `TestMain` helper pattern from `internal/sandbox`) that write at the same time. It then asserts:
  - **no missed rows:** a reader tailing from a cursor sees every committed row exactly once;
  - `security`'s record numbers have no gaps;
  - the chain verifies;
  - exactly one process holds the housekeeper lease at a time.
- **Portability tests,** run on all three OSes:
  - one process's mirror is never re-read as another's external log;
  - the spool claim works while a writer still holds the file (Windows), and never reads a half-written file (POSIX);
  - quarantine refuses to move a database other processes have open;
  - a crash file can be removed after `SetCrashOutput(nil)`;
  - Windows device names are refused as log names;
  - `fullfsync` is set on macOS.
- **Benchmarks** run on all three OSes in CI and fail on a regression of more than 20% against that OS's stored baseline. The absolute targets in §12.3 are checked on a reference machine.
- **archtest rules:**
  - `internal/logs/...` stays at the bottom of the dependency graph;
  - long-running packages may not write to `fmt.Print*`, the standard `log` package or `os.Stderr` (a ratchet that allows only a shrinking list);
  - packages marked catalogued may not add warning or error records without an ID (also a ratchet).
- **Catalog drift.** `docs/EVENTS.md` is generated, and a test fails when it is out of date. The docfacts pattern already does this.
- **End to end.** Start serve on the echo backend and run a turn that makes a tool call and needs an approval. Then check `tenant logs trace` returns turn → tool call → approval → Security record → sandbox run.
- **The discovery drill** (§9.1) runs three ways: as `httptest` handler tests against the dashboard; as a CLI script; and once in a real browser, through a chromedp test that skips when Chrome is absent.
- **The house workflow** applies to every ticket: `go vet`, the full suite, the smoke eval, and a mutation sweep over the new logic.

## 15. Decisions (recommended defaults) and open questions

The Recommended column holds the default this plan proceeds on, unless you decide otherwise.

| # | Decision | Recommended | Alternatives, and why not |
|---|---|---|---|
| D1 | The system of record | **One SQLite database, plus the v1 text mirror** | *An NDJSON journal with a rebuildable index:* two stores to keep consistent, plus index-lag and rebuild logic, for no gain. *Text only:* no indexed query, which is the point of the exercise. |
| D2 | One database or one per log | **One database, with per-log budgets** | *One database per log:* correlation and time slices across logs are the core of discovery, and they need one database. |
| D3 | Several writer processes | **Inserts: each process writes directly, and SQLite serializes them. Housekeeping: one process at a time, by lease.** | *A single owner that the others forward to:* more moving parts than the problem needs, because `datalock` already allows shared processes and SQLite already serializes writes. |
| D4 | Event IDs | **Numbers within each provider, stable forever, allocated in blocks by task** (Appendix B) | *One global number space:* needs central allocation, and plugins would collide. |
| D5 | Sensitive content | **Store it, mask it in viewers, leave it out of exports by default** | *Never store it:* investigations lose tool payloads and message text. Available as `logs.store_sensitive: false`. |
| D6 | Security log integrity | **An HMAC on every link, salted commitments, and an anchor outside the database. An outside witness is optional (Q5).** | *A plain hash chain with signed checkpoints* (the first draft): up to 1,000 records could be rewritten without the key, and truncation was invisible. *Nothing:* not enterprise-class. |
| D7 | When an audit write fails | **`continue`, plus a critical event and a banner** | *`deny`:* available for strict operators. |
| D8 | The text mirror | **On for operational channels** | *Off:* this breaks `tail -f`, grep and v1 scripts. It can be turned off per install. |
| D9 | v1 source names | **Keep them as log names.** `approvals` moves into `security`, and its folder keeps being written for one release. | *Rename everything:* breaks folders, habits and scripts. |
| D10 | External forwarding (OTLP, syslog, webhooks) | **Out of scope.** The logs stay in Tenant. | The TEN-300 OTLP TODO stays deferred. If it ever comes back, it goes on as an opt-in exporter reading from the store, with no change to the design. |
| D11 | Channels | **Two: operational and diagnostic, with a timed debug switch per provider** | *Event Viewer's four* (admin, operational, analytic, debug): more budgets and switches than one operator needs. Administrative Events works as a view instead. |
| D12 | The TUI viewer | **A light `/logs` view first; a full-screen viewer only if it's needed** | *A full viewer in phase 3:* a fourth viewer for one operator, next to the dashboard and the CLI. |

**Open questions** (none of them blocks phase 0 or phase 1):

- **Q1.** Should the Components view also show host resource snapshots (the process's memory, goroutines and open files; free space on the data volume)? The suggestion: a low-rate diagnostic event every 60 seconds, left out of phase 3.
- **Q2.** Should an alert be allowed to start an agent investigation that is given the correlated events? It would stand out among log viewers, but it needs a budget and an approval story (phase 6). **Decided yes** by the operator (TEN-400): each investigation is approved, budgeted and read-only, its input labelled untrusted (docs/LOGGING.md, "Investigating an alert with the agent").
- **Q3.** Should you be able to view a federated peer's logs through the Logs page? The suggestion: after TEN-189/190, as a read-only remote provider.
- **Q4.** When serve runs as another account (the Windows server runs it as SYSTEM), is reading through the API enough? The alternative is for the service to keep its log book where the operator's account can read it. The suggestion: the API, which is already the remote path.
- **Q5.** Which witness should the Security log use, if any: a federated peer, the operator's Discord DM, or a file on another machine? The suggestion: the Discord DM, because the relay already exists. It's opt-in, because without one the log is tamper-evident only against things that can't read the key (§10.2).

## Appendix A: Filter language

```
filter  := term*                         terms are ANDed; OR within a list: id:1,2,5-9
term    := ['-'] field op value          '-' negates:  -log:eval
         | '"quoted text"' | word        full text (FTS5) over msg and data
field   := log | provider | id | level | channel | task | opcode | tag | trace | activity
         | related | session | approval | tool_call | actor | surface | pid | boot
         | since | until | data.<key>
op      := ':' | '=' | '!=' | '>=' | '<=' | '>' | '<'
value   := word | "quoted" | list (a,b) | range (1000-1999) | glob (mcp.*)
         | duration (15m, 2h, 7d; relative to now) | time (RFC 3339, 14:03, today, yesterday)
level   := critical > error > warning > info > verbose      (level>=warning = warning, error, critical)
```

| Filter | What it finds |
|---|---|
| `level>=warning since:24h` | Administrative Events for the last day |
| `log:discord id:2001-2999 -id:2005` | a band of Discord events, minus one |
| `provider:mcp.* level>=error` | every MCP server's errors |
| `trace:3f2a9c…` | one turn, across every log |
| `tag:audit.failure since:7d` | a week of denied or failed audited actions |
| `data.tool:os_exec "permission denied"` | exec results containing that phrase |
| `boot:current level>=warning` | this process run's problems |

## Appendix B: Event ID conventions

IDs are unique within their provider and never reused. A retired ID stays in the catalog, marked `retired`. Blocks are assigned by task:

| Block | Use |
|---|---|
| 1–999 | Lifecycle: starting, ready, stopping, stopped, degraded, recovered |
| 1000–1999 | Normal operations (the provider's main tasks) |
| 2000–2999 | Warnings, retries and degradation |
| 3000–3999 | Errors and failures |
| 4000–4999 | Security and audit (in `security`, and in providers that write audit events) |
| 5000–5999 | Configuration changes |
| 9000–9999 | Diagnostics (the diagnostic channel) |

Sample entries, in the shape the catalog would give them:

```go
var (
    Started     = System.Event(1,    event.Info,     "Lifecycle", "Tenant {version} started as {role}", "version", "role")
    StoppedOK   = System.Event(2,    event.Info,     "Lifecycle", "Tenant stopped cleanly after {uptime}", "uptime")
    Unexpected  = System.Event(3,    event.Critical, "Lifecycle", "The previous run ended unexpectedly; its last event was at {last}", "last", "boot")
    CompFailed  = System.Event(3101, event.Error,   "Component", "{component} failed: {err}", "component", "err").
        Doc("A subsystem stopped with an error.",                                           // what it means
            "Something it depends on failed: a connection, a file or its configuration.",   // why it happens
            "Open the component's log; the event before this one usually names the cause.") // what to do

    ApprovalAsk = Security.Event(4101, event.Info,    "Approval", "Approval {approval} requested: {action}", "approval", "category", "action", "surface")
    ApprovalNo  = Security.Event(4103, event.Warning, "Approval", "Approval {approval} denied: {action}", "approval", "action", "surface", "actor").
        Keywords("audit.failure")
)
```

`docs/EVENTS.md`, the dashboard's Help tab and `tenant logs catalog` all render from these definitions.

## Appendix C: Configuration

`config.json` keeps v1's `logs` block and adds to it:

```json
"logs": {
  "dir": "", "level": "info", "retention_days": 30, "max_field_bytes": 4096,
  "store": true,
  "text_mirror": true,
  "budget_mb": 2048,
  "store_sensitive": true,
  "diagnostic": { "max_mb": 256, "max_age_days": 7 },
  "security": { "max_mb": 256, "max_age_days": 365, "on_failure": "continue",
                "witness": "" },
  "per_log": { "mcp.github": { "max_mb": 32, "min_level": "warning" } },
  "rate_limit": { "per_second": 100, "burst": 500 }
}
```

- `budget_mb` covers the database and its WAL (§6.2).
- `security.witness` is empty (none) by default. Otherwise it is `discord`, `peer:<name>` or a path on another machine (§10.2, Q5).

`TENANT_LOG` and `--log-dir` keep their v1 meaning. New keys are added to `internal/config/testdata/keys.golden` and appear on the dashboard's configuration page.
