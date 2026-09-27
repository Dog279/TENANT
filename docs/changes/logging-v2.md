# Change: Logging v2, an Event Viewer-class log book (`feat/logging-v2`)

**Branch:** `feat/logging-v2` (on top of `docs/logging-v2-plan`) · **Jira:** workstream TEN-359, tasks TEN-360 to TEN-400 · **Plan:** [`docs/design/logging-v2.md`](../design/logging-v2.md) · **Status:** implemented and tested on macOS. On the final commit: the full suite and the race detector pass, and so do `go vet` for Windows and Linux, the smoke eval and an end-to-end trace test. Windows and Linux compile and vet clean, and their tests run in CI when the branch is pushed. Not yet deployed.

## What changed

The v1 log book (a text folder per source) is now the readable mirror of a structured store. Every event has an ID, a level from critical to verbose, typed fields, and the IDs that tie it to its turn, operation and process. Every ID is documented. Anything can be found. Nothing is lost silently. The Security log is append-only and keyed.

### M0: v1's gaps closed (TEN-360 to TEN-368)
- **No secrets in the logs.** The dashboard token no longer reaches `pushSys`, the ring or stdout off a terminal (TEN-360). A scrubber runs before the value cap, the ring and the files: known values (raw, base64, URL-encoded, JSON-escaped) and credential patterns (TEN-361).
- **Every line links to its run.** Turn events and slog records carry `trace` (TEN-362). Route decisions, cron history, archive events, research events and usage rows carry the trace; a sub-agent's run names its parent; approval IDs are unique across processes and outcomes name their cause (TEN-368).
- **No stray output.** `slog.Default`, the `log` package and every `http.Server`'s error log point at the log book. The eval harness, chromedp and the OAuth prints are rerouted. One-shot commands file their warnings (TEN-363).
- **Child processes are captured** (MCP servers' stderr and exit, sandbox and host runs, `claude -p`, Chrome, helpers) through one wrapper, `internal/logs/capture` (TEN-364).
- **Crashes and unexpected exits are recorded.** The runtime's crash output goes to a file that is filed at the next start. Boot records and locks detect a run that died. Every `recover()` logs its stack, and a panicking tool fails only its own call (TEN-365).
- **No error is dropped silently** (38 paths). Unsaved operator choices are told to the operator (TEN-366).
- **v1 viewer and API fixes** (TEN-367): each subscriber gets its own wake-up, sequence numbers carry an epoch, `limit`, built-in sources are refused and `ev=` stripped for outside writers, `attach` flags after the URL, token-file reads, `logs.level` warn/error, retention applies live, tab order.

### M1: the store (TEN-369 to TEN-375)
- **The event model** is `internal/logs/event`, a leaf every package may import: providers, catalogued definitions, activities, and the emitter. `internal/logs/catalog` holds the built-in providers (TEN-369).
- **`<logs root>/logbook.db`** is one SQLite database (WAL, `synchronous=FULL`, `fullfsync` on macOS). Each process has one writer connection. A pipeline scrubs, caps, mirrors, then commits every 100 ms or 512 events. Cursors are `<store ID>.<seq>`, with no missed rows across processes. A Security record forces its batch to commit. Measured at 36–49k events/s (TEN-370).
- **Retention and budgets**: policies per log (size, age, overwrite, archive or keep, minimum level), a 2 GB budget for the whole book, chunked deletes, WAL checkpoints, low-disk shedding. Housekeeping runs under a lease, in one process at a time (TEN-371).
- **The mirror is safe to tail** (control characters escaped). Other programs' folders are filed as `ext.<name>`, there is a spool for writers when Tenant isn't running, and v1 history is imported once, scrubbed (TEN-372).
- **The log book watches itself**: `logbook` events, rate limits, retry and then the mirror alone, recovery import, quarantine of a corrupt database, boot records in the store, and it is released last (TEN-373).
- **Backup** copies `logbook.db` with `VACUUM INTO`. A restore gives it a new store ID. Privacy deletions reach the log book, including the mirror and archives; Security records are tombstoned (TEN-374).
- **`tenant doctor` and `tenant security audit`** cover the log book: integrity, size and budget, the lease holder, mirror-only processes, losses, a relocated `logs.dir`, file protection, the scrubber, a scan for known secrets, the chain, and the witness (TEN-375).

### M2: query (TEN-376 to TEN-378)
- **The filter language** (`internal/logs/query`, Appendix A): the page p95 is 12 ms on 1M events for indexed fields. It supports stats from rollups, top IDs, the timeline pivot, and per-ID statistics (TEN-376).
- **`tenant logs`**: `query`, `show`, `trace`, `stats`, `export`, `verify`, `config`, `clear`, `debug`, `catalog`, `components`, `recover`, and `alerts`. Every command runs locally or through the API, with the same features either way, in text, JSON or CSV (TEN-377).
- **API v2**: `/api/events…` and `/api/logbook/…`, with SSE resume. `POST /api/events` ingestion is marked `external`. The v1 routes are shims over the store. Reference client v2 uses the API or the spool (TEN-378).

### M3: the viewer (TEN-379 to TEN-381)
- **The dashboard's Logs page** (`/logs`) is laid out like Event Viewer:
  - a tree: Summary, Custom Views, Tenant, the subsystems by group, External, Components;
  - a grid that pages by cursor as you scroll, with live tail and a match count;
  - detail tabs: General (local and UTC time, pivot links), Details (sensitive fields masked; each reveal is audited), Correlated (the timeline and the run record), and Help (the catalog entry and "seen N times");
  - actions: filter, find, saved views, export, log properties with "debug for 30 min", clear, and a permalink `/logs/event/{seq}`.

  It is server-rendered with Datastar, with no new dependency. Discovery questions 1–4 pass as handler tests, and a chromedp check drives it in a real browser (TEN-379).
- **Summary and Components.** The Summary reads hourly rollups in 14–20 ms on 1M events. The component registry covers every subsystem serve starts and every child process, and each state change is a `system` event (TEN-380).
- **The TUI's `/logs`**: a filter, `follow`, `enter` for an event's detail and `c` for its correlated timeline, through `control.LogsControl`. The feed now scrolls from the keyboard (TEN-381).

### M4: catalogued (TEN-382 to TEN-395)
Every provider is catalogued, with a meaning, a cause and an action per event. Coverage per provider is 91–100%, held by a ratchet. `docs/EVENTS.md` is generated with a drift test. Each subsystem's main operations are activities (turns, sub-agent runs, connections, deliveries, job runs, model calls, sandbox runs, backups, sweeps, eval runs, requests). Each MCP server writes `mcp.<name>`, and each plugin declares its catalog in its TEN-321 descriptor.

### M5: the Security log (TEN-396 to TEN-398)
- **A keyed hash chain**: an HMAC on every link, salted data commitments, an anchor at `<config>/logbook.anchor`, a re-anchor record on restore, an optional witness (a file or the operator's Discord DM), chained archives, `verify`, and an offline-verifiable audit bundle (TEN-396).
- **Audit records** for approvals on every path (the host broker, Discord cards, iMessage, external links, the cron-exec approver, pairing, mode changes), for every exec, and for dashboard and peer access. `logs.security.on_failure` is `continue` or `deny` (TEN-397).
- **Audit records** for configuration diffs (secrets by name), secrets (by ID), scheduled jobs, privacy, backup, restore, and log-book administration. Discovery question 6 is answered (TEN-398).

### M6: alerts (TEN-399, TEN-400)
- **Alert rules**: a filter, an optional threshold, an action (dashboard, Discord, iMessage or cron), and a throttle. They are evaluated only by the lease holder, and there are five built-in rules. Alerts are events. Rules are managed in config, the API and `tenant logs alerts` (TEN-399).
- **Investigating an alert with the agent** (TEN-400; open question Q2 answered yes, since this goal included it):
  - It is an alert action. Each run needs the operator's approval.
  - It has a budget (tokens, tool calls, time, steps).
  - It runs with an allowlist of read-only tools, enforced in the tool surface: log queries and memory reads only.
  - The attached log text carries TEN-311's `untrusted` label.
  - The report goes to the status feed and the Security log. The rule's channel gets only the outcome and the trace, because what leaves the machine never carries log text (§11).

## After deploying: what the operator does
1. **Rotate the dashboard token** (TEN-360). Existing `logs/system/*.log` files and supervisor captures may already hold it.
2. **The Security log's key** is created at `<config>/logbook.key`, and the anchor at `<config>/logbook.anchor`. Encrypted backups carry the key; without it, a restored chain can't be verified.
3. Optional: set `logs.security.witness` (a path on another machine, or `discord`) so a rollback by someone holding your full privileges is still detected.
4. `logs.store: false` is now ignored, with a one-time warning: the store is the system of record.

## Decisions made during the build
- **Outside writers file under `ext.<name>`.** That covers `POST /api/events`, the v1 `POST /api/logs/{source}`, `tenant log`, the spool and other programs' folders: `deploy` becomes `ext.deploy`, in the viewer's External group. v1 feeders keep working, but a `tenant log deploy` line now lands in `ext.deploy`, not `deploy`.
- **The store is required.** `logs.store: false` is ignored, with one warning, since the v1 routes now read the store (§13).
- **A restore gives `logbook.db` a new store ID** (so every live cursor restarts), and the Security log records a re-anchor.
- **No chain key** refuses only the Security records (`ErrNoChainKey`). The rest of the batch commits, and the pipeline doesn't treat it as an outage.
- **`data.<key>` filters never match a sensitive field**, so a query can't read what only an audited reveal shows.
- **The Security log can't be cleared, disabled or level-filtered.** When full, it archives, never overwrites.
- **Alerts are evaluated only by the housekeeper-lease holder**, so serve and the TUI on one data directory never both alert.
- **One `model` log**, with a `backend` field, rather than a log per backend: a definition belongs to one log.
- **Event documentation** is `Doc(meaning, cause, action)`, three parts, all required.
- **Plugins share IDs** 1–3, 2001 and 3001–3003; each plugin's own events start at 1100, 2100 and 3100.
- **Low-disk shedding** is counted in each log's dropped total, not warned about every minute.
- **Actor and surface** are event attributes (`"actor"`/`"surface"` keys), not data.
- **Tool calls carry `tool_call`** through their approval, exec record and sandbox run.
- **One-shot commands** write their Security records to `logbook.db` (`logs.AuditEmitter`).

## Known limits and follow-ups
- **Only macOS has run the tests.** Windows and Linux compile and vet clean, and CI runs the tests on all three when the branch is pushed. The Windows-specific paths haven't run yet: boot locks, crash-file removal, DPAPI sealing of the key, the spool claim.
- **Low disk.** On a volume with less than 5% free, diagnostic events are shed, as the plan specifies (5% of this Mac's 460 GB disk is 23 GB).
- **Chrome's exit code** isn't known (chromedp drops it).
- **A `peer:` witness** isn't supported: no existing peer call stores a head. Files and the Discord DM work.
- **`Count` on 1M events** takes about 250 ms for indexed filters. The page query itself is 12 ms.
- **An outage still open when a process exits** isn't imported by the next start; its lines stay in the text files.
- **Under `on_failure: deny`,** a host prompt whose record fails is refused, but the dashboard event it already emitted says approved.
- **A budget-stopped investigation** still makes one tool-less wrap-up call (at most 30 s).
- **The v1 `approvals` folder** is still written through this release; remove it in the next.
- **Small follow-ups from the survey:**
  - nothing writes research `events.jsonl`;
  - the Discord relay manager still reports running after a fatal gateway stop;
  - `tenant privacy prune`'s message overstates what it prunes;
  - the web reader's audit event has no trace;
  - a dashboard page you're signed out of answers with JSON rather than the sign-in page (this predates logging v2).
