# Change: `tenant privacy` — retention by data class, export, and deletion with a preview (OPS-003)

**Branch:** `feat/ten-309-privacy` (on top of `feat/ten-305-backup`) · **Status:** implemented and
tested on Windows, with the POSIX paths run as Linux binaries under WSL; not yet deployed.
**Ticket:** OPS-003 / TEN-309.

## What changed
```
tenant privacy [status]                      what Tenant keeps, how long, and how to remove it
tenant privacy retention set <class> <days|forever>
tenant privacy prune [--yes]                 remove everything past its retention now
tenant privacy forget [selectors] [--yes]    remove specific data, preview first
    --session ID  --episode N  --fact N  --skill NAME  --match "text"
    --before YYYY-MM-DD  --archive-month YYYY-MM  --connector NAME  --only-agent ID
tenant privacy sessions [--limit N]          recent sessions, to find an id to forget
tenant privacy export --out DIR [--no-archive] [--only-agent ID]
```

- **Retention by data class.** `privacy.retention_days` in `config.json` sets a period for
  each class:
  - `episodes`
  - `facts` (not reconfirmed within the period; pinned and protected facts are kept)
  - `archive`
  - `usage`
  - `research`
  - `screenshots`
  - `relay_inbox` (default still 30 days; now configurable, and `forever` turns the sweep off)
  - `eval_artifacts` (baselines and the trend log are never pruned)

  Every class other than `relay_inbox` is kept forever until you set a period, so
  nothing changes on upgrade. `tenant privacy status` shows every class next to
  `runs`, `route_log` and `logs`, which already had their own settings.
  `retention set` writes the right key for any of them. `tenant serve` and the TUI
  prune by the configured periods a few minutes after start and then daily, and
  `tenant privacy prune` does it now.
- **Forget, with a preview.** A forget selects what you name:
  - a session, an episode, a fact, a skill;
  - text wherever it appears;
  - everything before a date;
  - an archive month;
  - a connected service's stored credentials and data.

  It prints every kind of data that will go, with counts and samples, and deletes nothing
  until you confirm (`--yes` for scripts). What goes with the selection:
  - facts every source of which is forgotten;
  - the run records of forgotten turns;
  - SME section versions that cite only forgotten facts;
  - the turn's events in the archive: the user's message up to the next one, tool calls
    and results included;
  - for text, matching archive events, skills and their earlier versions, and lines of
    the synthesized user profile.

  The preview also notes what it cannot reach:
  - a soul that mentions the text (yours to edit);
  - research runs that mention it;
  - the log book, until its own retention;
  - backups made before now.
- **Deletion is real.**
  - Rows are deleted, not tombstoned, on a connection with `secure_delete` on, so freed
    pages are zeroed.
  - The full-text indexes are optimized, so the deleted words leave the index too.
  - The WAL is truncated.
  - Archive session files are rewritten without the removed events. If the live archive
    appends during the rewrite, the rewrite starts over, so no new event is lost.

  A test forgets a street address and then reads the raw database files: the text is
  gone.
- **Provenance stays consistent.** A fact that loses one of several sources keeps the rest
  and records the removed one (kind, id, when, why) in `fact_source_tombstones`, a new
  table in `facts.db` that `semantic.Store.RemovedSources` reads. Links are repaired
  across stores:
  - SME sections drop citations of deleted facts;
  - skills drop removed run ids;
  - episodes drop a removed run.

  A retention prune never deletes the facts distilled from what it prunes; they record
  that their source aged out.
- **A plan selects again when it is applied.** A turn that lands in the session between the
  preview and the confirmation is forgotten too.
- **Connectors.** `--connector` removes a service's `credentials.json` entries
  (`skill:<name>:*`) and its own files. The supported services and their files:
  - `gsuite`, `discord`, `imessage` and `web` (credentials only);
  - `x` (the token cache);
  - `github` and `atlassian` (their token files);
  - `simplefin` (the ledger);
  - `mcp` (every remote MCP sign-in).

  Status and export list the services with something stored, by name only.
- **Export.** `privacy export` writes, owner-only, readable without Tenant:
  - each store table as JSON Lines (episodes, facts, signals, removed sources, SME
    sections, skills and their history, runs, token usage), with embedding vectors left
    out;
  - the synthesized profiles, the souls and research runs;
  - the raw archive, unless `--no-archive`;
  - the connector list;
  - a README that says what each file holds.

  `--only-agent` limits it to one agent.

## Why
Tenant kept memory forever, with no way to say "not this" beyond hiding a fact or an episode
(a tombstone that keeps the text and its search-index entry). There was no per-class
retention past runs, routes and logs, no way to see what Tenant holds, and no deletion that
followed a fact back to its sources, or a turn forward into the archive.

## Evidence
- **`internal/privacy`**, on a fixture of real stores (four turns in three sessions, facts
  sharing sources, a pinned fact, an SME section, skills, runs, the archive, a profile, a
  soul, research, screenshots and eval files):
  - forgetting text removes it from every store, the search index, the archive, the profile
    and the raw database files;
  - a fact that loses one of two sources keeps the other and records the removed episode and
    run;
  - forgetting a session takes its derived facts (pinned too, as a forget should) and signal
    rows, its archive file and its runs, and leaves a source-less fact alone;
  - forgetting one episode cuts exactly its turn from a shared session file and unlinks
    skills and facts;
  - an agent-scoped forget leaves other agents and agent-less files alone;
  - a preview changes no file;
  - an apply catches a turn added after its preview;
  - retention prunes episodes, archive months, research, screenshots and eval reports, and
    keeps facts, baselines and the trend;
  - stale-fact retention spares pinned facts;
  - export contents, the agent filter, and its guards;
  - the sessions listing.
- **`cmd/tenant`:**
  - the status view;
  - `retention set` for every kind of key, and its rejections;
  - forget's confirmation and idempotence;
  - forgetting a connector (tokens and credential entries go, others stay, no value leaks
    into the summary);
  - export;
  - the daily prune honors only configured periods;
  - the relay sweep honors its configured period.
- **POSIX:** exports are `0600` / `0700` throughout, and a rewritten archive file stays
  `0600`. Run as Linux binaries under WSL with the rest of the suite.
- **Mutation testing:** 37 targeted mutations of the cascades, the deletion mechanics
  (`secure_delete`, FTS optimize, WAL truncate), archive rewrites, retention and the CLI;
  36 are caught. The survivor is equivalent: a whole-session fast path that the per-event
  scan reaches anyway.

## Operator notes
- Nothing is pruned until you set a period, except the relay inbox (30 days, as before).
- A forget cannot be undone. Take a backup first (`tenant backup --encrypt`) if you might
  want it back. Backups made before a forget still hold what it removed.
- Forgetting text matches ASCII letters regardless of case. Other characters must match
  exactly.
- A running agent keeps its current session's recent turns in memory. Forgetting the
  session in progress removes it from disk, but that agent can still see those turns
  until you start a new session (`/new`) or restart it.
