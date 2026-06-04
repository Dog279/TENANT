# Temporal Memory — Adoption Plan (Graphiti + Onyx lessons)

Status: **DRAFT for review** · Author: staff-eng design pass · Date: 2026-05-22

> Goal: give Tenant's memory **temporal context on top of semantics** — the
> ability to reason about *when a fact was true* and *when we learned it*, to
> retire contradicted facts without losing history, and (optionally) to model
> entities and relationships — while preserving Tenant's defining constraints:
> a single static CGO-free Go binary, SQLite-backed, surgical, enterprise-grade
> stability and throughput.

---

## 1. Recommendation (TL;DR)

**Adapt, don't replace.** Evolve the existing T3 semantic store into a
**bi-temporal fact store** and add a **lightweight SQLite-native entity/edge
layer** — adopting Graphiti's *ideas* (bi-temporality, non-destructive
invalidation, typed graph) without its *infrastructure* (a managed graph
database). A full replacement with Graphiti is **rejected** for a hard reason:
Graphiti mandates an external graph DB (Neo4j / FalkorDB / Kuzu / Neptune) and a
Python runtime — both fatal to Tenant's "one static binary, no external
services" product promise. We can capture ~90% of the value in SQLite.

Phasing:

| Phase | Scope | New deps | Risk | Value |
|------|-------|----------|------|-------|
| **1** | Bi-temporal facts + LLM contradiction-driven invalidation + point-in-time retrieval | none | low | high |
| **2** | SQLite-native entity + typed-edge graph (bi-temporal), entity resolution, recursive-CTE traversal | none | medium | high |
| **3** | Pluggable store interface + enterprise throughput hardening + optional external backends | optional | medium | strategic |

Phase 1 alone delivers the headline ask ("temporal context on top of
semantics") with zero new dependencies and closes an existing TODO in
`internal/memory/distill/distill.go`.

---

## 2. Where Tenant is today (T3 semantic store)

`internal/memory/semantic` already implements a surprising amount of what
Graphiti/Zep are known for:

- **Hybrid retrieval**: vector (brute-force cosine over `embedding BLOB`) + FTS5
  keyword, fused with **RRF** — the same pattern Graphiti uses (semantic + BM25 +
  RRF).
- **Time-decayed confidence**: `EffectiveConfidence(now)` decays a fact's score
  by `(now − last_confirmed)`; fully-decayed facts drop out of ranking. This is
  functionally Onyx's `DOC_TIME_DECAY` recency bias — *we already bake recency
  into ranking, not post-hoc.*
- **Non-destructive lifecycle**: `superseded_by` (pointer to the replacing fact),
  `tombstoned` (soft delete), `Reaffirm` (bumps `last_confirmed`).
- **Provenance**: `source_episodes` (JSON array of T2 episode IDs).
- **Scoping**: `visibility` (private/shared/public) per agent — analogous to
  Onyx's ACL-as-index-data (we enforce it as a query filter, which is the right
  posture).

What we **lack** versus Graphiti:

1. **Bi-temporality.** `first_seen`/`last_confirmed` are *transaction time*
   (when we learned/confirmed a fact). We have **no event/valid time** — when a
   fact was actually true in the world. We cannot answer *"what was true on
   2026-01-01"* vs *"what did we believe on 2026-01-01."*
2. **Contradiction-driven invalidation.** Supersession is manual / pointer-based.
   The distiller has an explicit TODO: *"Detecting that a new fact contradicts
   (vs merely covers similar territory) needs LLM judgment … contradictions
   accumulate."* Graphiti closes exactly this loop with an LLM contradiction
   check that invalidates (not deletes) the stale edge.
3. **Structure.** Facts are flat one-sentence claims. No entities, no typed
   relationships, no graph traversal ("what else is connected to X").

---

## 3. What the two systems teach us

### 3.1 Graphiti (getzep/graphiti, Apache-2.0, Python) — the temporal core

The borrowable ideas (verified against repo + the Zep paper, arXiv 2501.13956):

- **Bi-temporal edges.** Each fact/edge carries four timestamps:
  - `valid_at` — when the fact became true (event time, **T**)
  - `invalid_at` — when it stopped being true (event time, **T**)
  - `created_at` — when ingested (transaction time, **T′**)
  - `expired_at` — when marked invalidated (transaction time, **T′**)
  This is the crux: two independent timelines let you replay *world state* or
  *belief state* at any instant.
- **Non-destructive invalidation.** New info contradicting an old fact → an
  **LLM judges the contradiction** → the old edge's `invalid_at`/`expired_at`
  are set; it stays in the graph for history. "Consistently prioritize new
  information."
- **Episodes → entities → edges.** Raw episodes are extracted (by LLM) into
  entity *nodes* (with evolving summaries) and edge *facts* (NL relationship +
  embedding + provenance). Entity resolution merges new mentions into existing
  nodes via embedding similarity + FTS + an LLM resolution prompt.
- **Hybrid + graph retrieval.** semantic + BM25 + **breadth-first graph
  traversal**, reranked by RRF / MMR / node-distance / cross-encoder, with
  temporal-validity datetime filters.
- **Cost reality (the warning).** Ingestion is the bottleneck — *multiple
  sequential LLM calls per episode* (extract → resolve → edge-extract →
  contradiction-check). Retrieval is sub-second; **writes are slow and
  expensive.** This dictates our architecture (§6).

### 3.2 Onyx (onyx-dot-app/onyx, MIT core, Python) — the enterprise spine

Onyx is a different beast (document RAG at corporate scale), but its
production-engineering choices are the relevant lessons:

- **Pluggable index behind one interface** (`DocumentIndex`) — backend (Vespa /
  OpenSearch) swappable without touching pipeline code.
- **Hybrid in one engine** — co-locating BM25 + ANN avoids the cross-system
  score-normalization problem; if you must merge two stores, do explicit
  Min-Max/Z-Score normalization. (We use RRF in one store — already aligned.)
- **Recency in ranking** — tunable `DOC_TIME_DECAY` + `last_updated` range
  filters. (We have decay; we should add explicit time-range filters — Phase 1.)
- **ACLs are index data**, enforced via mandatory query-time filters. (We have
  `visibility`; the lesson is to keep it a hard filter, never app-layer-only.)
- **Durable distributed job queue** (Celery) for indexing with explicit
  refresh/prune cadences and orphan reconciliation — but it's a known stability
  surface (stuck tasks). For us: keep the extraction pipeline on the existing
  background scheduler, make it idempotent and bounded.
- **Tiered deployment** ("lite" mode drops the heavy tier). For us: temporal/
  graph features must degrade gracefully when the embedder/LLM is absent.

**Net:** Graphiti informs the *data model*; Onyx informs the *operational
posture*. Neither should be adopted wholesale.

---

## 4. Gap analysis

| Capability | Graphiti | Onyx | Tenant today | Action |
|---|---|---|---|---|
| Hybrid semantic+keyword | ✅ (RRF) | ✅ (Vespa) | ✅ (RRF over vec+FTS5) | keep |
| Recency / decay in ranking | via temporal filters | ✅ DOC_TIME_DECAY | ✅ confidence decay | keep, add range filters |
| **Event/valid time** | ✅ valid_at/invalid_at | partial (last_updated) | ❌ | **Phase 1** |
| **Transaction time** | ✅ created_at/expired_at | n/a | ✅ first_seen/last_confirmed | rename-align Phase 1 |
| **Point-in-time query** | ✅ | filters | ❌ | **Phase 1** |
| **LLM contradiction → invalidate** | ✅ | n/a | ❌ (TODO) | **Phase 1** |
| Non-destructive history | ✅ | versioning | ✅ supersede/tombstone | keep |
| **Entities (nodes)** | ✅ | KG in Postgres | ❌ | **Phase 2** |
| **Typed edges / relationships** | ✅ | KG | ❌ (flat facts) | **Phase 2** |
| **Graph traversal retrieval** | ✅ BFS | — | ❌ | **Phase 2** (recursive CTE) |
| Entity resolution / dedup | ✅ | — | partial (reaffirm) | **Phase 2** |
| Pluggable store backend | graph DBs | ✅ interface | ❌ (SQLite only) | **Phase 3** |
| ACL/permission scoping | group_id | ✅ enterprise | ✅ visibility | keep, harden |
| External infra required | **graph DB** | Vespa/PG/Redis/Minio | **none** | preserve "none" |

---

## 5. Proposed design

### Phase 1 — Bi-temporal facts (no new deps)

Extend the `facts` table with event-time fields and a typed invalidation reason,
keeping transaction-time as-is.

```sql
ALTER TABLE facts ADD COLUMN valid_at      INTEGER;  -- event time: became true (unix; NULL = unknown/always)
ALTER TABLE facts ADD COLUMN invalid_at    INTEGER;  -- event time: stopped being true (NULL = still true)
ALTER TABLE facts ADD COLUMN invalidated_at INTEGER; -- txn time: when WE marked it invalid (mirror of expired_at)
ALTER TABLE facts ADD COLUMN invalid_reason TEXT;    -- 'contradiction' | 'user' | 'superseded' | NULL
-- first_seen  → keep as created_at (txn: when learned)
-- last_confirmed → keep (txn: last re-validated; drives decay)
CREATE INDEX IF NOT EXISTS idx_facts_valid ON facts(valid_at, invalid_at);
```

Semantics (mirrors Graphiti's four-timestamp model, mapped onto our two
existing + two new columns):

- **Event timeline**: `valid_at` … `invalid_at`.
- **Transaction timeline**: `first_seen` (created) … `invalidated_at` (expired).
- A fact is **live** when `tombstoned=0 AND superseded_by IS NULL AND invalid_at
  IS NULL`.

New retrieval knobs (extend `semantic.Query`):

- `AsOfValid time.Time` — only facts where `valid_at ≤ AsOfValid < invalid_at`
  ("what was true then").
- `AsOfKnown time.Time` — only facts where `first_seen ≤ AsOfKnown <
  invalidated_at` ("what we believed then").
- Default (both zero) = current live facts (today's behavior, unchanged).

**Contradiction-driven invalidation** (closes the distill TODO): during the
background distillation pass, for each candidate new fact, retrieve the top-K
semantically-near live facts and run **one** LLM judgment call classifying each
as `same | refines | contradicts | unrelated`. On `contradicts`, set the old
fact's `invalid_at = new.valid_at` (or now), `invalidated_at = now`,
`invalid_reason='contradiction'`, and link `superseded_by = new.id`. **History
is preserved** (audit + point-in-time still see it).

Where event time comes from: the distiller's fact-extraction prompt gains an
optional `valid_at` output (the model extracts "since March", "as of last week"
→ a timestamp; absent → NULL = "treated as always/unknown"). This is the *only*
prompt change in Phase 1.

### Phase 1+ — typed fact-to-fact edges (the `related_to` patch)

**Question raised:** add a `related_to`-style temporal link to patch the cons of
pure semantic lookup? **Verdict: yes — but typed, and derived from the
contradiction LLM call we're already making, NOT from raw embedding
similarity** (a similarity-seeded `related_to` is redundant — the RRF hybrid
search already surfaces textually-similar facts; precomputing it adds
maintenance for ~no gain).

The genuine weakness a link fixes is **relational / chained recall**: surfacing
a fact's refinement or contradiction chain even when the linked fact isn't
similar to the query string. The Phase-1 contradiction pass already classifies
each new fact against its top-K neighbors as `same | refines | contradicts |
unrelated` — so we persist those judgments as typed edges essentially for free.

```sql
CREATE TABLE fact_edges (
  id         INTEGER PRIMARY KEY,
  agent_id   TEXT NOT NULL,
  src_id     INTEGER NOT NULL REFERENCES facts(id),
  dst_id     INTEGER NOT NULL REFERENCES facts(id),
  relation   TEXT NOT NULL,                    -- 'contradicts' | 'refines' | 'related'
  confidence REAL NOT NULL DEFAULT 1.0,
  valid_at   INTEGER, invalid_at INTEGER,      -- the LINK itself is temporal
  first_seen INTEGER NOT NULL,
  tombstoned INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_fact_edges_src ON fact_edges(src_id, relation);
```

- **Retrieval:** after the hybrid top-K, do a **1-hop expansion** along live
  `refines`/`related` edges (temporal-filtered), adding linked facts at a
  discounted (node-distance-penalized) score. This is Graphiti's BFS rerank in
  miniature, over the flat fact table.
- **`contradicts` edges** double as the invalidation audit trail and power
  "show me the history of this claim."
- **Cost: zero extra LLM calls** (reuses the contradiction-judgment output that
  Phase 1 already produces). One additive table.

This is the right "patch the semantic cons" move: it captures associative recall
*because the edges encode LLM-judged relationships*, which embeddings cannot —
whereas a cosine-seeded `related_to` would just re-encode similarity. If we
later want true entities, Phase 2 supersedes this without throwing it away
(fact_edges become a special case of entity edges).

### Phase 2 — SQLite-native entity + edge graph (no new deps)

Add two tables that give us Graphiti's structure without a graph DB. Traversal
is **recursive SQL CTE** over an adjacency table — SQLite handles 1–3 hop
expansion over tens of thousands of edges comfortably.

```sql
CREATE TABLE entities (
  id          INTEGER PRIMARY KEY,
  agent_id    TEXT NOT NULL,
  visibility  TEXT NOT NULL DEFAULT 'private',
  name        TEXT NOT NULL,
  type        TEXT,                 -- person | org | project | concept | ...
  summary     TEXT,                 -- evolving, like Graphiti's node summary
  embedding   BLOB NOT NULL,        -- name+summary embedding (resolution + search)
  first_seen  INTEGER NOT NULL,
  last_seen   INTEGER NOT NULL,
  tombstoned  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE edges (                 -- typed, bi-temporal relationship facts
  id          INTEGER PRIMARY KEY,
  agent_id    TEXT NOT NULL,
  visibility  TEXT NOT NULL DEFAULT 'private',
  src_id      INTEGER NOT NULL REFERENCES entities(id),
  dst_id      INTEGER NOT NULL REFERENCES entities(id),
  relation    TEXT NOT NULL,         -- "works_at", "prefers", "located_in"
  fact        TEXT NOT NULL,         -- NL form (kept for retrieval + display)
  fact_embedding BLOB NOT NULL,
  confidence  REAL NOT NULL DEFAULT 1.0,
  valid_at    INTEGER, invalid_at INTEGER,            -- event time
  first_seen  INTEGER NOT NULL, invalidated_at INTEGER, -- txn time
  superseded_by INTEGER, invalid_reason TEXT,
  source_episodes TEXT,
  tombstoned  INTEGER NOT NULL DEFAULT 0
);
-- + FTS5 on edges.fact and entities.summary, mirroring facts_fts.
```

- **Entity resolution**: candidate generation (cosine on `embedding` + FTS on
  `name`) → one LLM resolution call to merge-or-create. Identical to Graphiti's
  hybrid+LLM approach.
- **Traversal retrieval**: a `GraphSearch(query, centerEntity, hops)` that seeds
  with hybrid search, then expands `hops` via recursive CTE filtered by temporal
  validity, reranked by node-distance + decay.
- **Backward compatible**: the flat `facts` table stays; edges are *additive*.
  The assembler can blend both (facts for unstructured claims, edges for
  relational/temporal ones).

### Phase 3 — pluggability + enterprise throughput (strategic)

- Define a `MemoryStore` interface (the Onyx `DocumentIndex` lesson) so the
  default SQLite implementation can be swapped for an external backend
  (pgvector, or a graph DB) at enterprise scale **without** touching the agent
  or assembler. Default stays SQLite — the single-binary promise is intact.
- Throughput hardening: SQLite **WAL mode**, prepared-statement reuse, batched
  embed calls, and an ANN index if the brute-force cosine becomes a bottleneck
  (e.g., `sqlite-vec` extension — but that reintroduces CGO; evaluate against a
  pure-Go HNSW). Add per-tier metrics/observability hooks.

---

## 6. Throughput & stability (the non-negotiable)

Graphiti's lesson is a warning: **temporal extraction is write-path-expensive**
(multiple LLM calls per episode). Tenant's architecture must keep this **off the
interactive hot path**:

- **Extraction stays asynchronous**, on the existing background distillation
  cadence (currently ~10 min) — never blocking a chat turn. A user turn writes
  raw episodes (T2, cheap); entities/edges/invalidation are derived later.
- **Bounded LLM calls.** One contradiction-judgment call per candidate fact
  (batched top-K in a single prompt), one resolution call per new entity. No
  unbounded fan-out. Reuse the planner/summarizer role — runs on the same vLLM
  the agent already uses (DGX throughput is the design point).
- **Idempotent + resumable.** Re-running extraction over the same episodes must
  not double-insert (dedup via `source_episodes` + entity resolution). Survives
  restarts and model swaps (the router is already hot-swappable).
- **Graceful degradation.** No embedder/LLM → temporal + graph features
  silently fall back to the current flat-fact behavior (the "lite tier" lesson).
  Memory must never hard-fail a turn.
- **Single-writer discipline.** SQLite WAL + the existing store mutexes; the
  background job is the only writer of derived rows.

---

## 7. Risks & mitigations

| Risk | Mitigation |
|---|---|
| LLM cost/latency of extraction | Async background only; bounded/batched calls; reuse local model |
| Entity-resolution false merges | Conservative thresholds + LLM confirmation; merges are reversible (supersede, not delete) |
| Schema migration on live DBs | Additive `ALTER TABLE ... ADD COLUMN` (SQLite-safe); NULL-tolerant reads; version stamp |
| Scope creep into a graph DB | Hard line: SQLite + recursive CTE; revisit only at Phase 3 behind the interface |
| Model emits bad/no `valid_at` | NULL = "always/unknown"; never blocks ingestion; decay still applies |
| Retrieval regressions | Default query behavior unchanged; temporal filters are opt-in |

---

## 8. Migration & backward compatibility

- All schema changes are **additive columns** — existing DBs upgrade in place,
  old rows read with NULL temporal fields (= "current, unknown event time").
- Existing `Search`/`List`/`Supersede`/`Tombstone` APIs keep their signatures;
  temporal filters are new optional `Query` fields.
- `tenant memory` CLI/TUI gains read-only temporal views (`/memory as-of
  <date>`, `/memory history <fact-id>`) — no behavior change by default.

---

## 9. Decisions needed from you

1. **Phase 1 only, or commit to Phase 2 (entities/edges)?** Phase 1 delivers the
   stated ask (temporal-on-semantic) cheaply; Phase 2 is the bigger feature bet
   (true knowledge graph) and where we'd lead the market.
2. **Event-time source**: trust the LLM to extract `valid_at` from language, or
   start conservative (NULL unless an explicit date is stated)?
3. **Graph-DB escape hatch (Phase 3)**: do we want the pluggable interface now
   (design for it) or defer entirely until an enterprise customer needs >SQLite
   scale?
4. **Naming**: keep `first_seen`/`last_confirmed` or rename to Graphiti's
   `created_at`/`expired_at` vocabulary for industry familiarity?

---

## 10. Appendix — source research

- **Graphiti**: bi-temporal edges (`valid_at`/`invalid_at`/`created_at`/
  `expired_at`), LLM contradiction → non-destructive invalidation, episode→
  entity→edge extraction, hybrid+BFS retrieval (RRF/MMR/node-distance/
  cross-encoder), requires Neo4j/FalkorDB/Kuzu/Neptune, Apache-2.0, Python.
  Zep paper: 71.2% vs 60.2% accuracy, ~90% latency reduction vs full-context.
- **Onyx**: FastAPI + Vespa (pluggable `DocumentIndex`, OpenSearch backend),
  hybrid (weakAnd BM25 + HNSW), `DOC_TIME_DECAY`/`RECENCY_BIAS_MULTIPLIER`,
  Celery indexing, ACL-as-index-data, Helm/K8s, MIT core + enterprise edition.
- **Tenant today**: `internal/memory/semantic` — RRF hybrid, decayed confidence,
  supersede/tombstone/reaffirm, visibility, SQLite + FTS5; `distill.go` TODO for
  LLM contradiction detection (this plan closes it).
