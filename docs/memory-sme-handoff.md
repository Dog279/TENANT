# Long-term project SME memory (TEN-255) — operator handoff

All four phases of the long-term-memory epic are shipped on `main`. This is the
"turn it on and what to watch" guide. Full design + rationale:
[memory-sme-plan.md](memory-sme-plan.md).

## What shipped

| Phase | Commit | What it gives you |
|---|---|---|
| 1 | `f8f03c4` | fact **importance/heat/pin** signals, **importance-stretched decay** (a 9/10 fact lasts ~2.6yr, not 365d), **consolidation protection** (the holistic merge can't eat load-bearing facts), heat-reset |
| 2 | `335ceba` | **supersession-as-transition** (contradictions become recorded transitions, not orphans) + **`memory_history`** "as of date X" recall over bi-temporal validity |
| 3 | `4ee31eb` | the **per-project SME doc** — a `ReflectionJob` synthesizes a sectioned, versioned project understanding from your load-bearing facts and injects it into the system reserve every turn |
| 4 | _(this)_ | **feedback-driven protection** (ACKed turns promote their facts to protected) + a **`doctor` SME check** |

The store never hard-deletes; everything is soft (superseded / decayed / tombstoned), all reversible.

## How to turn it on

The background **ReflectionJob is OFF by default**. To synthesize + inject the per-project SME, set in `config.json`:

```json
{ "improve": { "reflect_every": "6h" } }
```

Everything else is already live once self-improve is on (`--self-improve` / serve):
- **Importance + decay + consolidation protection** (Phase 1) — automatic; the distiller scores importance, the consolidation job protects load-bearing facts.
- **Supersession + `memory_history`** (Phase 2) — `memory_history` is an enabled tool; ask the agent "what did we know as of <date>".
- **Feedback-driven protection** (Phase 4) — run `tenant ack` (or `/ack` in the TUI) on a good turn; the facts distilled from it get promoted to `protected` on the next distill cycle. Promote-only by design — `undo` does **not** auto-unprotect.

## What to watch

`tenant doctor` now includes a **`project SME (memory)`** check that reports:
- whether the SME doc exists / is stale (when `reflect_every` is on),
- the **merge-protected fraction** — if it warns that a majority of facts are protected, consolidation is being starved; raise `protectImportance` or review pins.

## The knobs (all default to safe)

| Knob | Default | Effect |
|---|---|---|
| `improve.reflect_every` | off | SME synthesis cadence (e.g. `"6h"`) |
| `ProtectImportance` (`semantic`) | 0.9 | importance threshold for merge-protection (+ must be actually-used) |
| `decayHorizonK` (`semantic`) | 4 | how much importance stretches the decay horizon |
| `importanceWeight` / `heatWeight` (`search`) | 0.4 / 0.2 | ranking modulation (inside the `[0.6,1.0]` envelope) |
| `pinned_max` | 5 | always-include budget cap (a stopgap, not the longevity mechanism) |
| consolidation `Holistic` / interval | true / 6h | unchanged — protection, not a default flip, defuses over-distillation |

## Deferred (optional polish — not blocking)

The design (§10 Phase 4) lists these as polish; they're cleanly scoped follow-ups:

1. **Interactive dashboard memory page** — pin/unpin buttons, SME section + version history, temporal timeline viz. Needs a `MemoryControl.ProjectSME()` interface method + REST handler + SSR + threading an SME source into `memControl` (2 sites) + updating the dashboard test fakes. ~½ day. (Operator visibility today is via the new `doctor` check + `memory_history`/`memory_search` tools.)
2. **`/pin` TUI command** + `memory_remember` setting pin/high-importance — surface the pin/importance controls to the operator/agent directly (the engine + `SetPinned`/`SetProtected` store API already exist; just no UI/tool affordance yet).

## Open operator decisions (design §12)

- **Multi-project**: shipped single-project (Tenant itself); the `projects` registry + cwd→project mapping is deferred until a second heavy project exists. `project_id` is reserved in the schema.
- **ReflectionJob auto-trust**: it ships off; consider an eval gate (like `SoulNudgeJob`) before any unattended run. Dogfood manually first.
- **`protectImportance` = 0.9**: start strict, tune *down* from the doctor protected-fraction telemetry — never up blindly.
- **Pin authority**: operator-only initially recommended (vs. agent-pinning via a gated tool).
