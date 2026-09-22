# Change: the unified log book (`feat/logs-hub`)

**Branch:** `feat/logs-hub` (on top of `feat/discord-approval-buttons`) · **Status:** implemented, tested, not yet deployed.

## What changed
Every subsystem now logs to the same place — `<data>/logs/<source>/YYYY-MM-DD.log`, one
folder per source, plain text, one entry per line — and the dashboard's Activity tab grows a
tab per folder automatically. A new `internal/logs` package (the hub) is the single sink:

- **Turn events** (the agent's tool calls/results, lifecycle, errors; sub-agents; the bus;
  approvals; inbound Discord/iMessage messages; cron/eval runner agents; add-ons) are rendered
  into readable lines (`tool call web_search`, `approval denied ap-3 …`) with the kind kept in an
  `ev=` field. `agent.EntryFor` does the mapping; `agent.Event` gained a `Source` field the
  cron / Discord / eval observers set.
- **slog records** route through a `slog.Handler` on the hub: by the logger's fixed source
  (`logs.For("x")`), else a `source` attr, else the message's `tag:` prefix table
  (`mcp:`→`mcp`, `vllm:`→`model`, `reflect:`→`improve` …), else `system`. `serve` still tees
  to stderr at the TENANT_LOG level; the TUI's `<data>/tui.log` is gone (it was WARN-only and
  unreadable next to the feed).
- **Status feed lines** (`pushSys`) are filed under `system` in both surfaces.
- **The dashboard** reads the hub instead of the old in-memory `agent.EventLog` (deleted):
  dynamic source tabs with counts, an errors-only toggle, per-source file list + raw view
  (`/activity/files/{source}/{name}`), the same gap-free SSE replay. History survives a
  restart: the hub re-reads the tail of every folder's newest file at start.
- **The live logging API** (no rebuild): `POST /api/logs/{source}` (JSON or text/plain),
  `GET /api/logs`, `GET /api/logs/{source}[/{file}]`, `/api/activity?source=&level=`;
  `POST /api/addon/event` still works and files under the add-on's own name. `tenant log
  <source> "<msg>" k=v … [--level]` for shells (POSTs to the hub, falls back to the file);
  `tenant attach --source x --level error`. Folders dropped under the root by any process
  are discovered (~5 s) and tailed live.
- **Config:** `"logs": {"dir", "retention_days" (30; -1 never), "level" (info|debug),
  "max_field_bytes" (4096; -1 unlimited)}`; `--log-dir`. Daily files roll at 64 MB.

## Why
The activity ring lived only in memory (a restart wiped it), slog went to stderr/tui.log at
WARN, and the two streams never met — auditing without an AI pulling logs meant grepping
journal output. One folder per subsystem, in text, is what a person reads; and making a
source self-registering means a new subsystem or a custom script joins the record by copying
three lines, not by touching the dashboard.

## Evidence
`internal/logs` (line round-trip, daily files + size roll, ring cursors, folder discovery +
foreign tailing + promotion, backfill order, retention, path safety, slog routing/tee/groups),
`internal/agent` (event mapping), `internal/dashboard` (tabs follow sources, source/level
filters on page + SSE + REST, write-creates-source, validation, add-on compat, file view +
traversal), `cmd/tenant` (`tenant log` post / fallback / interspersed flags). Full
`internal/...` and `cmd/tenant` suites green.

## Operator notes
- Nothing to configure: the root is `<data>/logs` on first launch.
- launchd/systemd stderr capture is unchanged for `serve`; the folders are additive.
- `tenant log --list` shows the sources; `tenant attach --follow --source approvals` tails one.
- See `docs/LOGGING.md` for the format, the API and the copy-paste snippets.
