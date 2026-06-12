# TEN-35: distill.Supersede — Contradiction Detection + Semantic Dedup

> Debate + implementation plan for TEN-35. 2026-06-12.

## State of Play

The distill package (`internal/memory/distill/`) already handles the T2→T3 pipeline: extract facts from episodes, embed them, compare against existing facts, then Reaffirm or Insert. What it does NOT handle: **contradictions**. If a user says "I prefer Python" on Monday and "I prefer Go" on Tuesday, both facts accumulate. The semantic store grows inconsistent.

The building blocks exist:
- `persistFacts()` at `distill.go:196` has a 3-way branch: cosine ≥ 0.88 → Reaffirm, cosine ≥ 0.80 (borderline) → LLM adjudicates `same/distinct`, else → Insert.
- `isRestatement()` at `distill.go:270` calls the summarizer with a binary same/different prompt. Returns `bool`.
- `semantic.Supersede(ctx, oldID, newID)` at `semantic/insert.go:78` already works — sets `superseded_by` on old fact, `Search` filters it out.
- `semantic.Restore()` at `semantic/insert.go:97` only handles tombstones, NOT supersede. There is no undo for Supersede.

The gap: `isRestatement` only asks same/different. It can't detect "user prefers Go" contradicting "user prefers Python" — those are semantically close (high cosine) but logically incompatible. The LLM returns `same: false`, both facts insert, and the store has a contradiction.

## The Debate

### Programmer Position: Modify isRestatement → adjudicatePair (3-way verdict)

Replace the binary `isRestatement()` with `adjudicatePair()` returning a 3-way enum: `same | contradict | distinct`. One LLM call, wider prompt, zero additional cost.

- **Signature change**: `(bool, error)` → `(pairVerdict, error)` where `pairVerdict` is a typed string.
- **New prompt**: asks for `{"verdict": "same"|"contradict"|"distinct"}` instead of `{"same": bool}`.
- **persistFacts body**: borderline branch gains a `contradict` case → Insert new fact, call `semantic.Supersede(old, new)`, increment `FactsSuperseded`.
- **Backward compat**: if model returns old-format `{"same":true/false}`, map to new enum.
- **RunResult**: add `FactsSuperseded int`.
- **DistillJob**: report superseded count in summary + Details.

Files touched: `distill.go` (core change), `distilljob.go` (reporting), `borderline_test.go` (unit tests), `distill_test.go` (integration tests).

### Strategist Position: Ship it, but add Unsupersede as a safety net

The 3-way verdict is correct. But `Supersede` is **irreversible** — `Restore()` only clears tombstones, not `superseded_by`. A false "contradict" verdict silently destroys a valid fact with no recovery path.

**Required mitigations:**
1. **Add `Unsupersede(oldID)`** to `semantic.Store` — one-liner: `UPDATE facts SET superseded_by = NULL WHERE id = ?`. Ship in the same PR.
2. **Dual-gate Supersede** — require LLM verdict `contradict` AND cosine ≥ 0.75. Contradictions need topical proximity; a low-cosine pair is unlikely to be a real contradiction.
3. **Log verdicts** — log `adjudicatePair` results at Info level so you can audit false positives retroactively.

**Explicitly NOT in scope:**
- Extending contradiction detection below the 0.80 borderline band. Below 0.80, signal-to-noise is terrible and LLM call volume multiplies 3-5x. Violates the cost contract.
- Threshold tuning. The 0.80/0.88 bands are fine. Ship the verdict, instrument counts, tune later with data.
- Separate `isContradiction()` call. Doubles cost for zero information gain.
- User-facing undo UI. `Unsupersede` in the store is enough for v1.

### Adversarial Check

- **Q: What if the LLM says "contradict" for genuinely distinct facts?** (e.g. "user works on Tenant" vs "user lives in Colorado" — both mention the user but aren't contradictory.)
  - A: The dual-gate (cosine ≥ 0.75) prevents this. "Tenant" and "Colorado" won't hit cosine 0.75 with real embeddings. And `Unsupersede` is the recovery path for edge cases.

- **Q: What about temporal preferences? "user prefers dark mode" → "user prefers light mode" — is that a contradiction or a change?**
  - A: Functionally the same outcome: the old preference is stale, Supersede replaces it. Whether it's a "contradiction" or an "update" doesn't matter — the new fact is more recent and should win.

- **Q: Race condition — two concurrent distill runs both find the same contradiction?**
  - A: `Supersede` is idempotent in effect. If both run, the old fact gets `superseded_by` set to one of the new facts. The other new fact is redundant but harmless. `Search` already filters superseded facts, so only the winner surfaces.

- **Q: Why not skip Insert on contradict and just update the old fact's text?**
  - A: Audit trail. `Supersede` keeps both rows. You can see what changed and when. In-place mutation destroys provenance.

## Verdict

```
RECOMMENDATION: Ship the 3-way adjudicatePair with dual-gate Supersede
and Unsupersede safety net. ~200 LOC, 4 files, no new dependencies.
Option A: 3-way verdict + Unsupersede + dual-gate (Completeness: 9/10)
Option B: 3-way verdict without Unsupersede (Completeness: 6/10 — irreversible data mutation)
```

## Architecture: Code Changes

### Change 1 — New verdict type + RunResult field

`internal/memory/distill/distill.go`

```go
// pairVerdict is the 3-way result of adjudicatePair.
type pairVerdict string

const (
    verdictSame       pairVerdict = "same"
    verdictContradict pairVerdict = "contradict"
    verdictDistinct   pairVerdict = "distinct"
)
```

Add `FactsSuperseded int` to `RunResult`.

### Change 2 — persistFacts signature

**Before:** `func (d *Distiller) persistFacts(...) (int, int, error)`
**After:** `func (d *Distiller) persistFacts(...) (inserted, reaffirmed, superseded int, err error)`

The borderline block changes from:
```go
same, aerr := d.isRestatement(ctx, summarizer, f.Fact, existing.Fact.Fact)
if aerr != nil { /* warn */ } else if same { /* reaffirm */ }
```

To:
```go
verdict, aerr := d.adjudicatePair(ctx, summarizer, f.Fact, existing.Fact.Fact)
if aerr != nil { /* warn, fall through to insert */ } else {
    switch verdict {
    case verdictSame:       /* reaffirm + continue */
    case verdictContradict: /* insert new + supersede old + continue */
    case verdictDistinct:   /* fall through to normal insert */
    }
}
```

**Contradict branch detail:** Insert the new fact, get its ID, call `semantic.Supersede(ctx, existing.Fact.ID, newID)`. If Supersede fails, log warning (non-fatal — new fact is live, old just didn't get marked). Don't bump `inserted` — net fact count didn't grow.

### Change 3 — adjudicatePair replaces isRestatement

**Before:** `func (d *Distiller) isRestatement(ctx, llm, candidate, existing) (bool, error)`
**After:** `func (d *Distiller) adjudicatePair(ctx, llm, candidate, existing) (pairVerdict, error)`

New prompt:
```
You compare two memory facts about a user or project. Decide their relationship:

- "same": They express the same underlying claim (one may be reworded, more/less specific).
- "contradict": They make mutually exclusive claims about the same property
  (e.g. different preferred languages, different deadlines, changed preference).
- "distinct": They are about unrelated topics.

Respond with JSON only: {"verdict": "same"} or {"verdict": "contradict"} or {"verdict": "distinct"}
```

New schema:
```json
{"type":"object","properties":{"verdict":{"type":"string","enum":["same","contradict","distinct"]}},"required":["verdict"]}
```

Backward compat: response struct has both `Verdict string` and `Same *bool`. If `verdict` is present, use it. If absent, map `same:true` → `verdictSame`, `same:false` → `verdictDistinct`. If neither present, default to `verdictDistinct`.

Delete old: `restatementSystemPrompt`, `restatementJSONSchema`, `isRestatement`.

### Change 4 — Dual-gate Supersede

In the `verdictContradict` case, add a cosine floor check before calling Supersede:

```go
case verdictContradict:
    if existing.Score < 0.75 {
        // Cosine too low — LLM may be hallucinating topical overlap.
        // Treat as distinct and insert both.
        break
    }
    // Proceed with supersede...
```

This prevents false-positive Supersede on loosely related pairs.

### Change 5 — Unsupersede on semantic.Store

`internal/memory/semantic/insert.go` — add after `Restore`:

```go
// Unsupersede clears the superseded_by field on a fact, reversing a
// Supersede call. Used for correcting false-positive contradiction
// verdicts. Does NOT affect tombstone status.
func (s *Store) Unsupersede(ctx context.Context, id int64) error {
    res, err := s.db.ExecContext(ctx,
        `UPDATE facts SET superseded_by = NULL WHERE id = ?`, id)
    if err != nil {
        return fmt.Errorf("semantic: unsupersede: %w", err)
    }
    n, _ := res.RowsAffected()
    if n == 0 {
        return fmt.Errorf("%w: id=%d", ErrNotFound, id)
    }
    return nil
}
```

### Change 6 — DistillJob reporting

`internal/improve/distilljob.go` — add `facts_superseded` to summary + Details map. Add `res.FactsSuperseded > 0` to `changed` condition.

### Change 7 — Run caller update

`distill.go` Run method: `ins, reaff, err := d.persistFacts(...)` → `ins, reaff, sup, err := d.persistFacts(...)`. Add `result.FactsSuperseded += sup`.

## Files Changed

| File | Change | LOC |
|------|--------|-----|
| `internal/memory/distill/distill.go` | pairVerdict type, RunResult field, persistFacts 3-int return + contradict branch, adjudicatePair replaces isRestatement, Run caller update | ~80 |
| `internal/memory/semantic/insert.go` | Add Unsupersede method | ~15 |
| `internal/improve/distilljob.go` | Report facts_superseded in summary + Details | ~8 |
| `internal/memory/distill/borderline_test.go` | Rename TestIsRestatement → TestAdjudicatePair, add same/contradict/distinct/backward-compat cases | ~60 |
| `internal/memory/distill/distill_test.go` | Add TestRun_ContradictionTriggersSupersede, TestRun_BorderlineSameStillReaffirms, TestRun_BorderlineDistinctStillInserts | ~80 |

**Total: ~245 LOC. Estimated 30 min AI-assisted.**

## Test Cases

### Unit (borderline_test.go)

| Test | Input | Expected |
|------|-------|----------|
| TestAdjudicatePair_Same | `{"verdict":"same"}` | verdictSame |
| TestAdjudicatePair_Contradict | `{"verdict":"contradict"}` | verdictContradict |
| TestAdjudicatePair_Distinct | `{"verdict":"distinct"}` | verdictDistinct |
| TestAdjudicatePair_BackwardCompatSameTrue | `{"same":true}` | verdictSame |
| TestAdjudicatePair_BackwardCompatSameFalse | `{"same":false}` | verdictDistinct |
| TestAdjudicatePair_NoisyFencedOutput | `` ```json\n{"verdict":"contradict"}\n``` `` | verdictContradict |
| TestAdjudicatePair_InvalidVerdictFallsBack | `{"verdict":"potato"}` | verdictDistinct |
| TestAdjudicatePair_EmptyResponse | `""` | verdictDistinct + error |

### Integration (distill_test.go)

| Test | Setup | Expected |
|------|-------|----------|
| TestRun_ContradictionTriggersSupersede | Pre-seed "prefers Python" vecA; extract "prefers Go" vecA (high cosine); adjudicate returns contradict | FactsSuperseded=1, FactsInserted=0, old fact has SupersededBy set |
| TestRun_BorderlineSameStillReaffirms | Same setup, adjudicate returns same | FactsReaffirmed=1, FactsSuperseded=0 (regression) |
| TestRun_BorderlineDistinctStillInserts | Same setup, adjudicate returns distinct | FactsInserted=1, Count=2 (regression) |
| TestRun_ContradictBelowCosineFloorInserts | adjudicate returns contradict, but cosine < 0.75 | Falls through to insert, no Supersede |
| TestDistillJob_ReportsSuperseded | Wire DistillJob with contradiction data | Details["facts_superseded"] == 1 |

## Open Questions

1. **Log verdicts at Info or Debug?** Recommendation: Info during v1 rollout (visibility into false positive rate), downgrade to Debug once stable.
2. **Should `Unsupersede` also bump `last_confirmed`?** No — the old fact's decay clock is stale for a reason. Let it decay naturally or manually Reaffirm.
3. **Batch reporting for DistillJob:** should `FactsSuperseded` go in the summary line or just Details? Both — summary gets `(5 new, 3 reaffirmed, 1 superseded)`.
