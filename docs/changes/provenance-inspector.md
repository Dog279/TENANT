# Change: the memory provenance inspector: one fact, fully explained (PRD-001)

**Branch:** `feat/ten-297-provenance-inspector` (on top of
`refactor/ten-320-303-command-registry`) · **Status:** implemented and tested on Windows; not
yet deployed.
**Ticket:** PRD-001 / TEN-297.

## What changed
```
/memory/facts/{id}                        everything about one fact
    ?turns=1                              its source conversations, rebuilt from the archive
    ?compare=A,B                          a word diff of two versions
    ?edit=1                               the editor
POST /memory/facts/{id}/edit              save new text as a new version
POST /memory/facts/{id}/invalidate        no longer true (kept for as-of recall)
POST /memory/facts/{id}/revalidate        true again
POST /memory/facts/{id}/restore-version   make an earlier version current again (version=N)

GET  /api/memory/facts/{id}               the same data as JSON
GET  /api/memory/facts/{id}/turns
POST /api/memory/facts/{id}/edit                  {"text": "..."}
POST /api/memory/facts/{id}/invalidate | /revalidate
POST /api/memory/facts/{id}/restore-version       {"version": N}
```

- **The fact page.** Each row of the Facts list links to `Everything about it`. The page
  shows:
  - **where the fact came from:** its source turns (a forgotten one is marked), its durable
    run ids, the sources a forget or retention removed (TEN-309), and what made it. That
    is the job (learned from a conversation, merged during memory upkeep, imported, saved
    by the agent or by an MCP client, or your own edit or restore) and the model, when a
    model did the work;
  - **how sure Tenant is:** base and decayed confidence, importance, pin and protection;
  - **when it held:** learned, last confirmed, true since, no longer true since;
  - **what it replaced and what replaced it,** with every version, oldest first;
  - **where it is used:** the expert-notes (SME) sections that cite it, and how often recall
    pulled it into a turn.

  Statuses and jobs appear in operator words (`current`, `replaced`, `removed`), and
  every time is shown in local time; the JSON keeps the stored values.
- **Archive reconstruction.** `Show the full conversations from the archive` rebuilds each
  source turn from the raw archive: the user's message, every tool call and result, and
  the reply. When a turn can't be rebuilt, the page says why:
  - the episode was forgotten;
  - the archive no longer has the turn (pruned, or older than the archive);
  - the archive isn't reachable from this server.
- **Edit, invalidate, restore, compare.**
  - **Edit** saves the new text as a new version, embedded with the live embedder, which
    replaces the current one. The old version stays, so you can compare or restore it.
    Pins, protection and importance carry over; the retrieval count starts again. Like the
    memory tool, Tenant refuses an edit it couldn't make findable (no embedder).
  - **`No longer true`** sets the fact's end of validity. The fact stays in memory for as-of
    recall, and `It's true again` clears it.
  - **`Make this current again`** restores an earlier version as a new version, so history
    only grows.
  - **`Compare`** shows a word diff of two versions.
  - **`It contradicts another fact`** opens the existing conflict picker with this fact.
- **Origins.** A new table in `facts.db`, `fact_origin` (fact id, job, model, when), is
  written for every new fact from now on. The writers are:
  - distillation, with the summarizer model;
  - consolidation, with its model;
  - the MCP memory server;
  - `tenant memory import`;
  - the `memory_remember` tool;
  - eval seeding;
  - dashboard edits and restores.

  Facts from before this change show `not recorded`.
- **Store guarantees.**
  - `Insert` writes a fact and its origin in one transaction.
  - `Edit` reads the current version, inserts the new one, carries signals over and
    supersedes the old one, all in one transaction. So two edits of one version cannot
    both become current: the second fails and leaves nothing behind.

## Why
Tenant could show a fact's source turns (TEN-89), but not what made the fact, how its
confidence was reached, what it replaced, or what uses it. A wrong fact could only be
removed, not corrected. PRD-001 asks for one place that answers "why does Tenant believe
this?" and lets the operator fix the fact without losing its history.

## Evidence
- **`internal/memory/semantic`:**
  - origins are recorded and read back, and `Insert` sets the fact's id;
  - an edit makes a new version and keeps the old one; sources, confidence and the pin
    carry over, and the use count starts again;
  - an edit refuses a removed fact, a replaced version and unchanged text, each with its
    own message;
  - versions come back oldest first from any member of the chain; a missing fact is
    `ErrNotFound`;
  - a restore adds a version; restoring the current version is refused; restoring over a
    removed current version says to restore that first;
  - **eight concurrent edits of one version:** exactly one wins, and no extra fact is
    left. Without the transaction, three or four facts stay current (5 of 5 runs);
  - an insert whose origin can't be written leaves no fact.
- **`internal/memory/archive`:** `Reader.Turn` returns one turn of its session:
  - from the user's message to the next, not across sessions;
  - the latest sending of repeated words before the episode;
  - it is not stopped by a malformed line;
  - nothing for text never said, a message after the episode, or no session.
- **`internal/memory/sme`:** `CitingFact` finds only the active sections that cite a fact.
- **Origin writers:**
  - distillation records its job and summarizer model;
  - consolidation, MCP, import, the remember tool and eval seeding each record their job;
  - consolidation also records its model.
- **`internal/dashboard`:**
  - the JSON routes: detail, turns, edit (trimmed, empty and unknown fields refused),
    invalidate, revalidate, restore-version (defaulting to the fact in the URL);
  - 501 without an inspector;
  - the page shows every section in operator words (no "superseded" or "tombstoned")
    and one instant as the same local time everywhere, plus the archive view, the
    compare view and the editor;
  - an older version can't be edited, doesn't offer to make the current version current,
    and compares oldest first; a version isn't compared with itself;
  - the forms and their redirects and error texts;
  - the Facts list links each row;
  - the word diff.
- **`cmd/tenant`, the adapter over real stores:**
  - the detail of an edited fact and of its old version (origin, signals, sources, a
    removed source, versions, replacements, SME citations, retrievals, and the new
    version's embedding);
  - a fact from before origins;
  - an edit without an embedder is refused and leaves nothing;
  - a restore uses the fact's own embedder when none is configured;
  - invalidate and revalidate;
  - rebuilding turns from a real archive, with the tool call and result; a turn the
    archive lacks, a forgotten episode, and no archive;
  - how archived events read.
- **Mutation testing:** 68 targeted mutations across the store, archive, SME, the
  dashboard page and API, the adapter, and each origin writer; all are caught. Two more
  that remove the `Edit` and `Insert` transactions are also caught.

  The first sweep's 17 survivors led to the tests above, and to dropping a redundant guard
  from `Edit`: a conditional supersede that the transaction already guaranteed.

## Operator notes
- Origins start with this version; older facts show `not recorded`.
- An edit or restore needs the embedder, like the memory tool.
- The archive view reads the raw archive, so a turn pruned by retention or removed by a
  forget can't be rebuilt; the page says which.
- The JSON routes sit under `/api/memory` with the rest of the memory API and use the
  same dashboard auth.
