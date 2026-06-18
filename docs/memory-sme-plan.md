# T3 → Per-Project SME: Long-Term Semantic Memory Design

**Status:** Final design (post-debate synthesis + adversarial-review pass). Epic-ready, phaseable.
**Constraints honored:** pure-Go / CGO-free, additive-only (Windows build stays green), echo-offline-safe, single-binary personal box.
**Scope:** evolve the T3 `facts` system into a **single-project** subject-matter expert (Tenant itself) that retains nuance + importance over 1 year+ without hard-deleting anything. Multi-project scoping is designed-in but deferred (see §7/§12).

> **Revision note (post-review).** A 7-finding adversarial review corrected the first synthesis. The material changes folded in below: (1) the SME doc rides the **system reserve** with a per-section token cap and is counted against `WritableBudget` — it is **not** routed through `truncateFacts` (that path is for the ranked active block only); (2) **decay-immunity is broadened** — high-importance facts get a stretched decay horizon, not just the ≤5 pinned/explicit facts, so the majority of load-bearing knowledge actually survives a year; (3) merge-protection is **gated and calibrated** (strict 0.9 threshold + must be actually-used) so it can't swallow the whole store and starve consolidation; (4) importance gets **downward pressure** (agreement-averaged, not one-way ratchet) and heat-reset moves into the always-on cadence so both work in Phase 1; (5) the supersession verdict is a **return-type change** (bool→3-valued) that catches only near-paraphrase contradictions — honestly scoped.

---

## 1. Problem & goal

Tenant's T3 semantic layer has the right bones — atomic facts, cosine-band dedup, soft supersede, confidence decay, hybrid retrieval — but four structural gaps make it *lose* knowledge as a project ages, which is the opposite of what an SME should do:

1. **Over-distillation.** `ConsolidationJob` (Holistic, every 6h) hands up to 80 facts to an LLM that groups "same underlying claim" and merges each group into ONE sentence, calling `Supersede` on the originals (`consolidatejob.go:113`, `:181`). This is the dominant force in the store and it flattens specifics — versions, file paths, ticket IDs (`TEN-234`), gotchas like the "OAuth option-order" bug — into abstractions. The store trends toward *fewer, blurrier* facts.
2. **No importance signal.** Every fact competes flat. "User prefers tabs" ranks identically to "merge_pr is hard-gated regardless of Confirm." There is nothing that says *this one is load-bearing*.
3. **Over-supersession + silent decay.** `EffectiveConfidence` (`semantic.go:69`) decays linearly to 0 at 365 days. A correct, load-bearing fact that simply isn't re-mentioned for a year **silently drops out of `Search`** (the `ec <= 0` continue at `search.go:105`). Nothing protects it.
4. **No long-project relevance.** There is no per-project dimension and no always-present nuance carrier. Retrieval has no recency or priority bias (`search.go:113` is `relevance * (0.6 + 0.4*ec)` only). A year-old design rationale is recoverable only by retrieval luck.

**Goal:** a per-project SME that (a) *guarantees* load-bearing knowledge is present every turn, (b) ranks by predicted + revealed importance, (c) stops the blunt holistic merge from eating nuance, (d) records *why/when* things changed, and (e) carries a synthesized project understanding that survives a year — all additively, in pure Go, working with the echo backend.

---

## 2. What the field does

- **Generative Agents (Park et al., 2023)** — the canonical retrieval score: `recency + importance + relevance`, each min-max normalized, all weights 1. Importance is an LLM **poignancy 1–10** scored *at write time* ("brushing teeth"=1, "asking your crush out"=8). **Reflection**: when accumulated importance crosses a threshold, synthesize higher-level inferences and store them *as new memories* (consolidation by **addition**, not replacement). This is our importance + reflection blueprint.
- **Zep / Graphiti (Rasmussen et al., 2025)** — bi-temporal knowledge graph: every edge carries event-time (`valid_from`/`valid_to`: when true in the world) and transaction-time (`t_created`/`t_expired`: when the system learned/unlearned it). Supersession is **edge invalidation**: a contradicting fact sets the old edge's `valid_to = new.valid_from` and keeps both — never deletes. We adopt the **model** (two scalar columns), not the Neo4j engine.
- **Mem0 (Chhikara et al., 2025)** — ADD-only by default; DELETE only on explicit contradiction. Its own 2026 report names two hazards we must solve: **nuance loss from aggressive consolidation** (our exact failure) and **staleness** ("a memory about a user's employer is accurate until they change jobs, then confidently wrong"). Validates: add importance + supersession-as-transition rather than trusting the holistic merge.
- **MemGPT / Letta** — **core memory blocks** that are always in context = the "pin" primitive. The **sleep-time / split-writer** principle: a *separate* background agent (not the live loop) edits durable memory. Maps onto Tenant's improve scheduler + the pinned proposer router (TEN-195).
- **MemoryOS (EMNLP 2025)** — formula-driven **heat**: `Heat = α·N_visit + β·L_interaction + γ·R_recency`. Frequency + engagement + access-recency in one number = *revealed* importance, complementary to the LLM's *predicted* importance. (We take the heat formula but **not** its FIFO hard-eviction — Tenant never hard-deletes.)

**Convergent consensus across all five:** importance + recency + supersession-as-transition + an always-present durable carrier are now table-stakes, and a graph engine is *not* required to get them on a personal box.

---

## 3. Design overview (the chosen architecture, in prose)

The winning approach is **Stance A's risk-managed spine, with C's SME document as the payoff and B's storage shape + bi-temporal model grafted in** — not any one stance whole. The judges tied A and C at the top (both pure-Go-perfect, additive-safe) and mined B for its side-table storage form and scalar temporal model while rejecting its entity-relation engine as over-engineered for a one-person, single-binary project.

Concretely, the system gains **five interacting layers, all additive**:

1. **A `fact_signals` side table** (B's form) holds `importance`, `access_count`, `last_accessed`, `pinned`, `protected`, `valid_from`, `valid_to`. Joined `LEFT` so a fact with no signals row behaves **exactly** as today. The hot `facts` table, its FTS5 triggers, and the positional `scanFact` are **never touched** — this is the single cleanest additive move available and sidesteps the lockstep `scanFact`/`SELECT` churn that adding columns to `facts` would force.

2. **Importance at write** — `DistillJob` scores each fact 1–10 (Generative-Agents poignancy) as one extra JSON field on the LLM call it already makes; `memory_remember` writes high; reaffirmation **agreement-averages** it (a later lower score corrects a spurious early high) and it **drifts slowly toward neutral** if never re-scored — importance can rise *and* fall (§5). A complementary **heat** signal (`access_count` + `len(source_episodes)` + access-recency) captures revealed value.

3. **Longevity is two distinct mechanisms** (the review's central correction): (a) an **importance-stretched decay horizon** so the *broad* high-importance population survives years, not just a handful (§7) — this is the real 1-year guarantee; and (b) a small **pinned always-include sub-tier** (Letta core block, `retrieveFacts` `assemble.go:439`, ≤5, bounded) that guarantees a few canon sentences appear every turn regardless of embedding luck — a budget-safety stopgap, *not* the retention mechanism. Pinned facts are hard decay-immune; everything else decays on a horizon scaled by its importance.

4. **Consolidation protection + supersession-as-transition** — `ConsolidationJob` excludes pinned/explicit/`protected` facts, and high-importance facts that are **actually used**, from both cosine clusters and the holistic candidate list (strictly safer; only *removes* facts from merge candidacy; calibrated at `protectImportance=0.9` + a protected-fraction telemetry guard so it can't starve consolidation). The distiller's existing 0.80–0.88 borderline `isRestatement` call is upgraded from a boolean to a 3-valued **SUPERSEDES** verdict: a near-paraphrase that contradicts + temporally overlaps an old fact sets `old.valid_to = new.valid_from` and supersedes it — a recorded transition, not an orphan (near-paraphrase contradictions only; see §6).

5. **The per-project SME document** (C) — generalized directly from the already-shipped `userprofile.Synthesizer`, which already folds T3 facts into an always-on sectioned markdown injected at `assemble.go:242`. A `ReflectionJob` (off by default, Paused-gated, proposer-routed — the proven `souljob` discipline) reads protected/high-importance/recent facts for the active project and rewrites a sectioned SME doc with `source_fact_ids` provenance + versioned rollback. This is the durable nuance carrier that survives a year *by construction*, decoupled from per-fact decay and retrieval luck.

The throughline, verified in code and enforced every phase: **relevance stays multiplicative in `search.go` (importance/heat MODULATE, never reorder); every new job is off-by-default + Paused-gated + proposer-routed; the `facts` hot table + FTS triggers are never altered.**

---

## 4. Schema changes (all additive)

Two design rules:
- **No `ALTER TABLE` on `facts`.** Signals live in a sibling `fact_signals` table joined `LEFT`. Absent row == today's behavior, bit-for-bit. This avoids touching `scanFact`'s positional scan and the FTS5 triggers entirely.
- **New tables use `CREATE TABLE IF NOT EXISTS`** in a new embedded `*.sql`, run once at `Open()`. The store comment at `semantic.go:96` already guarantees "sibling tables are not disturbed."

```sql
-- internal/memory/semantic/signals_schema.sql  (new, embedded, IF NOT EXISTS)

-- Per-fact importance / heat / temporal validity. One row per fact that
-- has any non-default signal; a fact with no row reads as importance=0.5,
-- pinned=0, protected=0, valid_to=NULL (currently true) — i.e. today.
CREATE TABLE IF NOT EXISTS fact_signals (
    fact_id       INTEGER PRIMARY KEY REFERENCES facts(id) ON DELETE CASCADE,
    importance    REAL    NOT NULL DEFAULT 0.5,   -- GA poignancy, 1-10 mapped to 0..1
    access_count  INTEGER NOT NULL DEFAULT 0,     -- MemoryOS N_visit (revealed value)
    last_accessed INTEGER,                         -- unix epoch of last retrieval hit; NULL→last_confirmed
    pinned        INTEGER NOT NULL DEFAULT 0,      -- Letta core-block: always-include + decay-immune + merge-immune
    protected     INTEGER NOT NULL DEFAULT 0,      -- merge-immune but NOT always-included (protection != pin)
    valid_from    INTEGER,                          -- Zep event-time: when the claim became true (NULL=unknown)
    valid_to      INTEGER,                          -- Zep event-time: when it stopped (NULL=currently true)
    project_id    TEXT                              -- NULL = global bucket (always co-retrieved)
);

CREATE INDEX IF NOT EXISTS idx_signals_pinned
    ON fact_signals(pinned, importance);
CREATE INDEX IF NOT EXISTS idx_signals_project
    ON fact_signals(project_id);
```

```sql
-- internal/memory/sme/sme_schema.sql  (new, embedded, IF NOT EXISTS) — Phase 3

-- Per-project living SME documents. One row per (project, section) so a
-- section re-synthesizes independently and is budgeted independently.
CREATE TABLE IF NOT EXISTS sme_docs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id      TEXT NOT NULL,
    agent_id        TEXT NOT NULL,
    section         TEXT NOT NULL,        -- e.g. Architecture, Conventions, Gotchas, Open Threads, Glossary, History
    body            TEXT NOT NULL,
    source_fact_ids TEXT,                 -- JSON array; provenance bounds hallucination
    version         INTEGER NOT NULL DEFAULT 1,
    updated_at      INTEGER NOT NULL,
    token_estimate  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sme_active ON sme_docs(project_id, section, version);

-- Optional project registry (deferred until a SECOND real project exists; see Open Decisions).
CREATE TABLE IF NOT EXISTS projects (
    id           TEXT PRIMARY KEY,
    label        TEXT,
    root_path    TEXT,
    created_at   INTEGER,
    last_active_at INTEGER
);
```

**Go-side plumbing:** a new `semantic.Signals` struct + `GetSignals(id)` / `UpsertSignals` / `BumpAccess(ids)` methods. `scanFact` is unchanged. A new `FactWithSignals` view (a struct embedding `*Fact` + `Signals`) is returned only by the new ranking path — existing callers of `Get`/`Search` are untouched. `Search` gains a `LEFT JOIN fact_signals` and reads signals into the new fields; with no signals row the COALESCE'd defaults reproduce today's math.

---

## 5. Importance model

Importance is **two complementary signals**, both pure-Go:

**Predicted (write-time poignancy).** `DistillJob.extractBatch` adds one field to its existing JSON schema and prompt:

```jsonc
// factsJSONSchema gains:  "importance": {"type":"integer","minimum":1,"maximum":10}
```
System-prompt rubric (Generative Agents, baked in): *"1 = transient/mundane (small talk, one-off). 10 = load-bearing project architecture, identity, a hard constraint, a deadline, or a decision rationale."* `persistFacts` maps 1–10 → 0..1 into `fact_signals.importance`. Defaults: legacy/unscored = 0.5 (neutral); `memory_remember` = 0.95 + may set `pinned=1`; `mcp_fact_add` = 0.7. On the **echo backend** the summarizer returns no usable score → fall back to 0.5 (no crash).

**Reinforced (agreement-averaged, with slow drift — NOT a one-way ratchet).** The first synthesis made re-confirmation `max(old, new)`, which is a permanent ratchet: one spurious `9` from a small model is canon forever, and combined with auto-protection (§6) it's a one-way slide into the protected set. Corrected: on the Reaffirm path (`distill.go:323` clear-match and `:340` borderline-same) importance becomes a **running average** weighted by confirmation count (`imp ← (imp·n + new)/(n+1)`, `n` = times confirmed, stored in signals), so a later *lower* score pulls a spurious early high back down, while genuinely-repeated high scores stay high. `source_episodes` unions as before. Independently, importance is given a **slow drift toward the 0.5 neutral** in the same pure-function style as decay (a fact no extraction has re-scored in months relaxes toward neutral) — revealed heat then re-raises whatever the agent actually keeps using. Net: importance can rise *and* fall, so a noisy early over-score is self-correcting even in Phase 1 (before the Phase-3 `ReflectionJob` exists).

**Revealed (heat).** After `Search` returns its top-K, a single batched async `UPDATE fact_signals SET access_count = access_count + 1, last_accessed = ?` fires off the turn hot path (best-effort, ignored on echo/degraded). Heat is computed at rank time:

```
heat = a·log1p(access_count) + b·log1p(len(source_episodes)) + c·exp(-Δt_lastAccessed / μ)
```
`source_episodes` length is the MemoryOS engagement term (`L_interaction`) — already in the JSON, just counted, no new column. `μ` ≈ a few weeks. **Heat-reset (periodic `access_count` halving of the top-heat facts, MemoryOS promotion-reset) runs in the always-on `ConsolidationJob` cadence, NOT inside the optional Phase-3 `ReflectionJob`** — so the rich-get-richer guard is live from Phase 1, not gated behind a feature that ships off by default.

**How importance is *used*** (never to delete — Tenant's no-hard-delete invariant is absolute):
- **Ranking** (§9): a small additive `importance` term + a small `heat` boost, both modulating.
- **Protection from merge** (§6): (`importance >= protectImportance`, default **0.9**, **AND actually-used**) or pinned/explicit/`protected` → excluded from `ConsolidationJob`.
- **Decay resistance** (§7): `pinned` → hard-immune; otherwise importance **stretches the decay horizon** (a 9/10 fact lasts years, not the flat 365d).
- **SME promotion** (§8): high-importance + protected facts are the input set the `ReflectionJob` synthesizes from.

Predicted = what the LLM guesses matters; revealed = what the agent actually keeps using. They cross-check each other.

---

## 6. Temporal / supersession model

**Bi-temporal as two scalar columns, no graph engine** (Zep's model, B's realization on the fact row). Transaction-time already exists: `first_seen` (learned), `last_confirmed` (last re-validated). We add **event-time** `valid_from` / `valid_to` in `fact_signals` (when the claim is true *in the world*). `NULL valid_to` = currently true. Legacy facts and echo are unaffected (both NULL).

**Supersession becomes a transition, not a deletion.** The distiller already pays for an LLM call in the 0.80–0.88 borderline band (`isRestatement`, `distill.go:332`). **Contract change, stated honestly:** the real `isRestatement` returns `(bool, error)` against a boolean schema `{"same": bool}` (`distill.go:369`), consumed at `:332-346` as a binary *same→Reaffirm / else→Insert* branch. This is **not** a free "add an enum field" — it is a **return-type change** (`bool` → a 3-valued `same | distinct | supersedes`) **plus a new write path inside `persistFacts`** (set `valid_to` + `Supersede`), threading the closest existing fact's id (`findClosest`, `:411`, already returns it). Feasible and additive, but real surface — budget for it accordingly.

```jsonc
// restatementJSONSchema becomes 3-valued: {"verdict": "same" | "distinct" | "supersedes"}
// "supersedes" = NEW contradicts OLD and refers to the same subject at a later time
//                (employer changed, deadline moved, endpoint switched).
```
On `supersedes`: set `old.valid_to = new.valid_from` (event-time close), then `Supersede(old, new)` (the existing soft FK, `insert.go:94`). The old fact stays — queryable "as of date X" via the audit path (`Get` + the new `valid_*`), filtered from default `Search` exactly as today (`superseded_by IS NULL`).

**Honest scope:** this fires **only in the 0.80–0.88 cosine band**, so it catches *near-paraphrase* contradictions ("endpoint is X" vs "endpoint is now Y", reworded). Semantically-distant contradictions that embed far apart (e.g. a constraint stated in one project context contradicted by a decision phrased entirely differently elsewhere) never reach this adjudicator and still both `Insert` — today's accumulate-orphans behavior. So this **mitigates** the Mem0 staleness hazard and the `distill.go:22` TODO; it does **not** close them. Closing them fully needs a separate contradiction pass over *topically*-related (not just cosine-near) facts — explicitly out of scope here.

**What changes in `ConsolidationJob`:** the blunt holistic merge is the main over-distill force, so it is **restricted to the low-value tail** — but the protection gate is **calibrated so it can't swallow the store** (see finding-3 fix in §7). Before grouping (both `clusterFacts` and `holisticGroups`), exclude a fact only when it is `pinned=1` OR `protected=1` OR (`importance >= protectImportance` **AND** it has been actually used — `access_count > 0` or pinned/explicit-remember origin). A high LLM score on a fact the agent never retrieves does **not** earn merge-immunity, so dead over-scored facts still consolidate. Protected facts can still be a merge *target's* source but are never superseded away. When a merge does happen, the merged fact inherits `importance = max(members)`, unions `source_episodes` (heat carries forward), and `valid_from = min(members.valid_from)`. Recency in *ranking* remains access-recency (heat); event-time is for correctness/audit only.

---

## 7. Forgetting & protection

**No hard delete, ever.** "Forgetting" stays a ranking/visibility phenomenon; we layer explicit protection tiers on top. The store **bifurcates** instead of uniformly shrinking: a protected nuanced head + a compressed noisy tail + a synthesized abstraction layer (the SME).

Two separate concerns the first synthesis conflated — kept distinct here:
- **Always-INCLUDE** (does it appear in *every* prompt regardless of relevance) — a tight budget-safety cap, the ≤5 pinned slots. This is a *budget* number, not a *sufficiency* number.
- **Decay-IMMUNITY / longevity** (does it survive a year in `Search` at all) — must be **broad**, covering the whole high-importance population, or the 1-year goal fails for everything the distiller auto-scores high but never re-mentions.

| Tier | Rule | Effect |
|---|---|---|
| **PINNED** | `fact_signals.pinned=1` (bounded, `pinned_max=5`) | always-included **and** decay-immune **and** merge-immune |
| **DECAY-RESISTANT** | `importance` high → **stretched decay horizon** (see guard) | `fullDecay` scales with importance: a 9/10 fact decays over ~2–3 yr, not 1 — survives long after the flat 365d→0 curve would have dropped it |
| **PROTECTED (merge)** | (`pinned` OR explicit-remember OR `protected=1`) OR (`importance ≥ protectImportance` **AND** actually-used) | excluded from `ConsolidationJob` grouping; **not** forced into every prompt |
| **MERGED (unchanged)** | everything else, incl. high-LLM-score-but-never-retrieved | low/medium-importance paraphrases still consolidate every 6h — correct; noise *should* compress |
| **SUPERSEDED-AS-TRANSITION** | near-paraphrase contradiction (0.80–0.88 band) | closed `valid_to` + supersede link; kept for audit |

**Decay guard — importance stretches the horizon (not a binary pin-only flag).** The first synthesis only made the ≤5 pinned + explicit-remember facts decay-immune, leaving the large auto-scored-high population to silently hit `ec ≤ 0` and exit `Search` at exactly 365 days — defeating Goal (a). Corrected: importance **scales the full-decay horizon** so longevity tracks importance smoothly, kept pure by passing signals in:

```go
// EffectiveConfidenceWithSignals: new sibling of EffectiveConfidence;
// the original stays for callers that don't have signals.
func (f *Fact) EffectiveConfidenceWithSignals(now time.Time, s Signals) float64 {
    if s.Pinned { // hard pin: never decays
        return f.Confidence
    }
    // importance (0..1) stretches the 365d horizon: imp=0.5 → 365d (today);
    // imp=0.9 → ~365d*(1+k*0.9). A high-importance fact decays over years, a
    // mundane one keeps today's curve. k≈2 ⇒ 9/10 fact ≈ 2.8yr horizon.
    return f.effectiveConfidenceWithHorizon(now, scaledFullDecay(s.Importance))
}
```

This is still pure, additive (the original `EffectiveConfidence` is untouched for callers without signals), and keeps genuine noise decaying on today's curve. **Backstop, closing the loop:** when the Phase-3 `ReflectionJob` cites a fact into an SME section it `Reaffirm()`s that `source_fact_id` — being load-bearing enough to enter the SME resets its decay clock, so the carrier and its evidence stay in sync.

**The knobs** (all tunable, all default to safe/today's behavior):
- `DefaultConsolidateInterval` — unchanged (6h).
- `Holistic` — **stays `true`** (we do NOT flip it; the calibrated protection filter, not a default flip, defuses over-distillation — strictly additive, no behavioral-default change, unlike Stance B).
- `DefaultClusterThreshold` (0.83) — unchanged.
- decay `grace` (30d) / base `fullDecay` (365d) — unchanged for mundane facts; importance *stretches* the horizon above this floor rather than retuning it.
- New: `protectImportance` (**0.9** = strict 9–10 only, deliberately conservative to avoid starving consolidation — tune *down* from telemetry, never up blindly), `importanceDecayHalfLife` (slow drift toward 0.5, §5), `decayHorizonK` (≈2), `pinned_max` (5), heat weights, `reflect_every` (empty = off).
- **Telemetry guard (Phase 1):** a `doctor` check + dashboard metric reports the **fraction of live facts that are merge-protected**. If that climbs toward a majority, consolidation is being starved (finding-3 risk) — raise `protectImportance` or tighten the actually-used gate. Protection is calibrated by evidence, not guessed once.

---

## 8. The SME layer

The per-project SME is the **durable nuance carrier** and the headline deliverable. It is generalized *directly* from `userprofile.Synthesizer` (`userprofile.go:168`), which already proves the entire mechanism in-tree: it folds T3 facts into a sectioned markdown via the summarizer and that markdown is injected always-on at `assemble.go:242`. Where the user-profile answers "who is the user" globally, the SME answers "everything load-bearing about THIS project."

**`ReflectionJob`** (`internal/improve/reflectjob.go`, new) — Generative-Agents reflection + Letta split-writer:
- **Trigger:** off by default via `improve.reflect_every` (empty = disabled), mirroring `soul_nudge_every` and using the same `resolveEvalCadence` fail-closed pattern (`eval_job.go:20`). Suppressed while degraded via the scheduler's `Paused` gate (`scheduler.go:104`). Runs on the **pinned proposer router** (TEN-195), never the live turn model.
- **Input:** protected + high-importance + recently-confirmed facts for the active project (`List` filtered by `project_id`) plus a window of recent episodes.
- **Action:** the proposer LLM rewrites SME *sections* (Architecture & Decisions, Conventions & Gotchas, Open Threads, Glossary/Entities, History) **preserving every specific** — paths, versions, rationale, dead-ends — and **citing `source_fact_ids`**. Writes `sme_docs` rows; the prior version row is kept (`version` bump) for rollback. This is consolidation by **addition** — it never supersedes the source facts. **On cite, it `Reaffirm()`s each `source_fact_id`** (resets that fact's decay clock — the §7 longevity backstop).
- **Hard per-section token cap, enforced at synthesis time.** Each section has a max `token_estimate`; a section the LLM writes over budget is rejected and re-summarized tighter *before* it is stored. The growth ceiling lives in the writer, not the reader — so the always-on doc can never silently balloon.

**Always-available retrieval — corrected budgeting.** The active project's `sme_docs` sections are injected into the **system reserve** every turn (not retrieved, not ranked) — exactly like `UserProfile` at `assemble.go:242`. The first synthesis contradictorily also said "budgeted via `truncateFacts`": that is **wrong and now removed**. `truncateFacts` (`assemble.go:632`) only trims the ranked `facts` slice that flows into the fenced `<memory-context>` *active* block; the **system block is never auto-truncated** (`assemble.go:258-265` — an oversized system block silently eats working-set/retrieval budget, and the TEN-132/TEN-214 guards only *warn*). So the SME must be governed at the source:
- carried via a new optional `Request.ProjectSME string` (empty = no change) into `buildSystemBlock` next to `UserProfile`;
- its tokens **folded into `SystemTokens` at count time** so `effectiveWritableBudget` (`assemble.go:339`, which already subtracts `measuredStatic` = Soul+System+Tools) accounts for it and the existing over-reserve **warning fires** instead of the doc quietly overcommitting the window;
- bounded by the synthesis-time per-section cap above, so total SME size is known and stable, not a per-turn truncation gamble (which would non-deterministically drop sections).

**Why this survives a year:** individual facts decay and noise compresses, but the SME re-states the durable nuance as added abstraction over *protected* source facts, is always-present (no retrieval miss can drop it), is decoupled from per-fact decay, and is written by a separate model from the live loop (so the live agent can't corrupt durable project memory). A year of episodes compresses into a stable, specific, queryable project expert that recalls *why* a decision was made — exactly what `MEMORY.md` is full of and current facts can't reconstruct.

---

## 9. Retrieval changes

**Single ranking line** (`search.go:113`), preserving the load-bearing MODULATE-never-reorder invariant the comment at `:109-113` documents:

```go
// today:  score := relevance * (0.6 + 0.4*ec)
// new (signals via LEFT JOIN; absent row → importance=0.5, heat=0 → identical to today):
ec := fact.EffectiveConfidenceWithSignals(now, sig)
if ec <= 0 { continue }                       // decay-immune facts never hit this
score := relevance*(0.55 + 0.30*ec + 0.15*sig.Importance) + wHeat*normHeat(sig)
```
`relevance` stays the **multiplicative spine** — importance/heat can break ties and gently favor, but a clearly-more-relevant fact can never be buried (the exact bug real embeddings caught at `:62`). `wHeat` is small (~0.05). Weights live in config (`importance_weight`, `heat_weight`), default-conservative.

**Pinned always-include sub-tier** (`retrieveFacts`, `assemble.go:439`) — a **budget-safety guarantee for a *handful* of canon facts, explicitly a Phase-1 stopgap, not the longevity mechanism.** The ≤5 cap is a *budget* number: it caps how much rides in *every* prompt, not how much survives a year. Longevity for the broad load-bearing population is the §7 importance-stretched decay horizon; the durable always-present carrier of `MEMORY.md`-style standing constraints (additive-only, Windows-green, pure-Go, key paths — far more than 5) is the **SME doc** (§8), which is this design's actual thesis. Pins are for the rare "this exact sentence must never be absent."

```go
// BEFORE Search: fetch up to pinned_max pinned facts via idx_signals_pinned,
// ALWAYS prepend them, dedupe against Search hits with the existing
// dedupeFacts(threshold=0.90) at assemble.go:478. Bounded so they can't
// blow the 0.15 fact budget; if over budget, lowest-importance pinned drops
// first and a truncation note is recorded.
pinned := req.SemanticStore.PinnedFacts(ctx, req.AgentID, pinnedMax)
hits := req.SemanticStore.Search(ctx, q)
out := dedupeFacts(append(pinned, factsOf(hits)...), factDedupeThreshold)
```

**Per-project scoping** — designed-in, **inert until the multi-project registry ships** (deferred, §12.1). `Query` gains optional `ProjectIDs []string` (boost, not hard filter, so global `project_id IS NULL` facts are always co-retrieved and general user prefs aren't siloed). But until cwd→project mapping and the `projects` registry land, **no write path populates `project_id`** (`DistillJob`/`memory_remember`/`ConsolidationJob`/`mcp_fact_add` don't know one), so every row is the global bucket and `ProjectIDs` is unused. Phases 1–3 therefore deliver a **single-project SME (Tenant itself)**; the column is kept now (cheap, additive, future-proof) but the scoping logic is not built/tested against data that doesn't exist yet. Empty `ProjectIDs` = today's behavior.

**Optional later:** a small entity/keyword-overlap boost in `fuseRelevance` (Mem0-2026 multi-signal) for named entities that embed poorly (file paths, `TEN-####`). Low priority, additive.

---

## 10. Phased rollout

Each phase is independently green, independently valuable, and ships like the existing TEN-### cadence. **Lowest-risk highest-value first.**

**Phase 1 — Signals side table + importance + heat + pin + protection (S–M, ~4–5 days). The acute-pain wave.**
This is the consensus minimum across all three stances and closes the table-stakes gaps at near-zero risk:
- `fact_signals` table (`signals_schema.sql`) + `Signals` struct + `GetSignals`/`UpsertSignals`/`BumpAccess` + idempotent `IF NOT EXISTS` at `Open()`.
- Importance field in the `DistillJob` extract schema + **agreement-averaged reaffirm with slow drift toward 0.5** (not a one-way `max` ratchet — finding 4).
- Fold importance + heat into the `search.go:113` line (LEFT JOIN; conservative weights; invariant tests against real-ish embeddings).
- **Importance-stretched decay horizon** (`EffectiveConfidenceWithSignals` scales `fullDecay` by importance — broad longevity, not just ≤5 pins — finding 2).
- Pinned always-include tier in `retrieveFacts` + bounded budget + dashboard/tool pin-unpin (the budget-safety stopgap, not the longevity mechanism — finding 6).
- `ConsolidationJob` **calibrated** protection filter (exclude pinned/explicit/protected, or high-importance **AND actually-used**; default `protectImportance=0.9` — finding 3) + **heat-reset moved into the always-on consolidation cadence** (so the rich-get-richer guard runs in Phase 1, not gated behind Phase-3 — finding 4).
- Async batched `access_count` bump off the hot path.
- **Telemetry/doctor check:** report the live merge-protected fraction so protection is tuned from evidence (finding 3).
- **Gate:** `baselines/fitness.json` eval shows no retrieval regression (the heat write touches the retrieval path — measure it).

**Phase 2 — Bi-temporal supersession-as-transition (S–M, ~2 days).**
- `valid_from`/`valid_to` already exist in `fact_signals` from Phase 1; wire them.
- Extend the borderline `isRestatement` verdict with `supersedes`; on it, set `old.valid_to` + `Supersede`.
- Audit-path "as of date X" reader (host/MCP `memory_history` tool, off the default assembler path).
- Cheap (an LLM call already paid for in the band) and all three stances want it.

**Phase 3 — Single-project SME document (M, ~3 days). The real SME, gated.**
- `sme_docs` table + `ReflectionJob` (off by default via `improve.reflect_every`, Paused-gated, proposer-routed) with `source_fact_ids` provenance + versioned rollback + **`Reaffirm()` of cited source facts** (longevity backstop, finding 2).
- **Per-section hard token cap enforced at synthesis time**; SME injected into the **system reserve** and **counted into `SystemTokens`/`WritableBudget`** so the over-reserve warning fires — **not** routed through `truncateFacts` (finding 1).
- **Defer the multi-project `projects` registry + cwd→project mapping** until a second heavily-used project exists (§12.1). Ships as a **single-project SME (Tenant itself)**; `project_id` stays NULL/global and `Query.ProjectIDs` is inert until the registry lands (finding 7) — no speculative scoping machinery built against absent data.
- **Gate:** mirror `SoulNudgeJob` — turn on for dogfooding first; consider an eval gate before any auto-trust, because an always-present synthesized doc is the sharpest hallucination surface.

**Phase 4 (optional) — Feedback-driven protection + dashboard polish (S, ~1.5 days).**
- Wire the existing ack/undo signal (TEN-151) to promote/demote `protected` — load-bearing-ness *learned* from real use.
- Dashboard memory page: pin/unpin, importance, SME sections + version history, temporal timeline; doctor check for SME staleness.

---

## 11. Risks & mitigations

- **Noisy small-model importance scores.** Agreement-averaged reaffirm (a later lower score pulls a spurious high back down) + slow drift toward 0.5 + conservative ~0.15 ranking weight + heat (revealed value) as an independent corrective. Importance is a boost, never a delete gate.
- **Over-protection starving consolidation → unbounded store growth** (finding 3). A small model over-scores, a large fraction becomes merge-immune, the holistic 80-fact window fills with skipped facts, the store grows toward the no-max-count ceiling. Mitigations: strict `protectImportance=0.9`; protection also requires the fact to be *actually used* (`access_count>0`/pinned/explicit); a doctor/dashboard metric on the merge-protected fraction so it's tuned down from evidence, not trusted blind.
- **Pin sprawl** blowing the 15% fact budget. Hard `pinned_max` (5), lowest-importance pinned drops first, truncation note surfaced; pinning is a deliberate gated act, not automatic.
- **Stale pin kept alive forever** (Mem0 "employer changed" hazard). Pins still participate in the SUPERSEDES transition — a contradicting newer fact closes the old pin's `valid_to` and the supersede filter hides it; pins are operator-visible/reversible in the dashboard.
- **`ReflectionJob` hallucinating a wrong abstraction into an always-present doc** (the single sharpest risk). Off by default, proposer-routed, fails closed; `source_fact_ids` citation + the prompt forbids unsupported claims; versioned rows allow rollback; rendered as *background, not instructions* (like `UserProfile`); SoulNudgeJob-style eval gate before any auto-trust.
- **SUPERSEDES false-positive** wrongly closing a still-true `valid_to`. Fires only in the narrow 0.80–0.88 band already adjudicated; soft + reversible (Restore clears it); strictly better than today's accumulate-orphans. *Conversely, it only catches near-paraphrase contradictions — distant ones still both Insert (§6); the staleness hazard is mitigated, not closed.*
- **Heat ossifying a popular-but-wrong fact** (rich-get-richer). MemoryOS promotion-reset (periodic `access_count` halving) runs in the **always-on consolidation cadence from Phase 1** (not gated behind Phase-3) + small heat weight.
- **Migration on a large DB.** `CREATE TABLE IF NOT EXISTS` + a side table means zero rewrite of `facts` and no backfill; at personal scale (thousands of facts) it's instant; guarded idempotent at `Open()`.
- **Echo backend.** Importance degrades to 0.5; heat/pins/decay are pure-SQL + embedder-agnostic; reflections suppressed while degraded. No crash, no echo-derived garbage.

---

## 12. Open decisions for the operator

1. **Multi-project now, or single-project first?** The design ships the SME doc against one project (Tenant itself) and **defers** `projects` + cwd→project mapping. Confirm: are you about to run a *second* heavily-used project soon? If not, the multi-project registry is speculative generality (YAGNI) and should wait. *(Recommended: defer.)*
2. **`ReflectionJob` auto-trust.** It ships off by default. Do you want it gated behind a fitness-eval check (like `SoulNudgeJob`) before it's allowed to write SME docs unattended, or is dashboard-visible + versioned-rollback enough for dogfooding? *(Recommended: eval gate before any unattended run; manual dogfood first.)*
3. **`protectImportance` threshold (default 0.9 ≈ strict 9–10).** Too low over-protects junk and starves consolidation (the finding-3 risk); too high lets nuance get merged. The review's lesson: start *strict* (0.9, plus the actually-used gate) and tune **down** only when the merge-protected-fraction telemetry shows the SME is starved for input — never up blindly. *(Recommended: 0.9 + actually-used gate + watch the live protected-fraction metric.)*
4. **Pin authority + cap sizing.** Two coupled questions. (a) Should the *agent* pin via a gated tool, or operator-only via dashboard/CLI? Agent-pinning is more autonomous but a sprawl vector. (b) `pinned_max=5` is a deliberate *budget* cap, not a sufficiency cap — longevity for the broad load-bearing set comes from the importance-stretched decay horizon (§7) and the always-present SME doc (§8), not from pins. Confirm you're comfortable with pins being a tiny "never-absent" set rather than the retention mechanism. *(Recommended: operator-only pins initially, keep `pinned_max` small, let the SME be the real carrier.)*
5. **Graph-expansion recall (Stance B's connected-nuance channel) — in or out?** It's the one mechanism that recalls supporting detail the query doesn't embed-match, but it adds a second always-on entity-extraction LLM call, entity-resolution drift, dual-write drift, and LEFT JOINs on the hot path. The judges rejected it for the critical path. Confirm it stays **deferred behind a default-off flag, gated on the eval baseline**, only if flat scoring + the SME doc prove insufficient. *(Recommended: defer; revisit only with evidence.)*
