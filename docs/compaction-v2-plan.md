# Compaction v2 — keep working when context is full (plan)

**Status:** planned (debated) · **Date:** 2026-06-09 · **Origin:** operator report — "compaction drops messages mid-turn and stops what I asked; 45 → 2 messages; model 400." Goal: Anthropic-style lossless-ish summarization that **caches what's dropped (recall on demand)** and **keeps the task going even when context is full.**

## TL;DR
Tenant already has an Anthropic-style compactor (`internal/memory/compress`: structured LLM handoff, verbatim tail, archive, reversible, goal header). It is **not** the load-bearing bug. The `model: invalid request: 400` that kills the task is an **orphaned tool_use/tool_result pair** in the assembled request, produced by **two** pairing-blind token-walk trimmers — and the one that actually fires is the **assembler's `truncateWorking`, mid-turn, every plan-loop iteration.**

## Confirmed root cause (file:line)
A tool round-trip = an assistant message with `ToolCalls:[{ID}]` then separate `Role:"tool"` messages with `ToolCallID` (agent.go:485, 911-922). The assembler copies both 1:1 to the wire (`buildMessages`, assemble.go:739-746); both backends serialize 1:1 with no repair (vllm.go:102-129, anthropic.go:218-262). A request with a `tool_result` lacking its `tool_use` (or vice-versa) → **400** → `planner.Generate` dies (agent.go:442) → `team: DONE — error: agent: planner generate`.

Two independent producers of the orphan, both pairing-blind token walks:
| Producer | Location | Fires |
|---|---|---|
| **Assembler `truncateWorking`** | assemble.go:561-571 (via agent.go:303→374→739) | **every turn, incl. mid-loop** when working overflows the slot ← **the screenshot** |
| Compaction tail split | archive_compact.go:58-75 | post-turn, when hysteresis trips |

`TailTokens=1500` (flat) also explains "45 → 2": with big tool turns, `keep=1` → `[summary]+[1 tail]`.

## Design decisions (debated forks, resolved)
- **Fix layer = the assembler boundary (authoritative) + the compactor (quality).** A `sanitizePairs` pass in `buildMessages` is the single choke point every trimmer/backend funnels through → request is valid **by construction**. Compactor boundary-snap is quality, not the load-bearing fix.
- **Fork A — keep recent user messages verbatim: YES (bounded).** Re-emit the last **K=4** user turns verbatim in the summary under `## User Requests (verbatim)`; older user intent folds into the summary. `goalHeader` demoted to redundancy. (Mirrors Anthropic "Primary Request and Intent.")
- **Fork B — tail unit = last-N-complete-exchanges (N=3 floor), token budget as a ceiling only.** Exchange = user → assistant[+tool_use] → tool_result… → assistant-final. Drop **whole oldest exchanges**, never split a pair. Subsumes pairing-safety + min-turns + scaled budget; kills 45→2.
- **Tail budget = 0.25–0.30 of the working-tier slot** (from the same profile budget the assembler uses), below `compactionLowFrac=0.40` so hysteresis disarms in one shot. Flat 1500 = fallback only. N=3 floor wins if it conflicts.
- **Fork C — mid-turn safety: validation valve, NOT a mid-turn LLM compactor.** `sanitizePairs` before every `Generate` + exchange-aligned clamp in `truncateWorking`. (Mid-turn LLM compaction rejected — re-entrancy risk while the loop holds a working view.) Original "P4 task-in-progress signal" rejected: continuity gap was never the cause; the 400 was.
- **Fork D — recall as a tool: YES, Phase 2.** `recall(call_id)` over `ExpandLatestCompaction`/`recall:<id>` stubs; **read-only (no reinsert), targeted, bounded (≤2/turn), gated.** Episodic-write of spans rejected (archive already durable).

## Phase 1 — MUST (fixes the operator bug; additive; ~155 LOC + tests)
Confined to `internal/memory/assemble` + `internal/memory/compress` + one prompt string + 4 sizing call-sites. No public signature changes → Windows build untouched, CGO-free.
1. **`sanitizePairs([]model.Message) []model.Message`** — new pure fn in assemble.go, called at the end of the working loop in `buildMessages` (~746). Drop any `Role:"tool"` whose `ToolCallID` has no earlier matching assistant `ToolCalls.ID` in the request; strip any `ToolCalls` entry with no following matching tool result. **Load-bearing.** (~40 LOC)
2. **Exchange-aligned `truncateWorking`** (assemble.go:553-573) — snap the drop boundary to an exchange start; shared `exchangeStarts` helper. (~25 LOC)
3. **Exchange-aligned tail + verbatim user block in the compactor** (archive_compact.go:58-75, content 128-131) — last-N-exchanges clamped by scaled budget; **recompute `tailStart` from the FINAL post-snap tail** before `collectSpan` (line 88); add `## User Requests (verbatim)` (K=4). (~50 LOC)
4. **Scaled tail budget** (compress.go:48-51) — `TailFrac` (0.25–0.30 of working slot) plumbed from the slot budget; wire at the 4 `&compress.Compressor{}` sites (commands.go:1453,1805; eval.go:190; research.go:1337). Flat 1500 = fallback. (~20 LOC + 4 edits)
5. **Strengthen summarizer prompt** (compress.go:34-50) — always emit `## Active Task` + explicit **Next Step**.
6. **Tests** (ship with Phase 1).

## Phase 2 — fast-follow (separate ticket)
`recall(call_id)` agent tool wrapping `ExpandLatestCompaction` — gated, read-only, targeted, bounded. Does not touch Phase 1 correctness.

## Test plan (deterministic, fake summarizer — no live LLM)
Shared predicate `validateToolPairing([]model.Message) error` used by BOTH `sanitizePairs` and tests.
- **A — assembler pairing integrity (the 400 regression):** orphaned working set + tiny slot forcing a mid-pair cut → assert zero orphaned `tool_call_id` / childless `tool_calls`. *(The test that would have caught the screenshot — compactor-only tests miss the `truncateWorking` path.)*
- **B — compactor pairing integrity:** `TailTokens:8` → assert `validateToolPairing(out)`; unpreservable-within-budget → `changed==false`, `out==input` (fail-safe).
- **C — latest user request survives byte-identical** even when the fake summary omits `## Active Task` (via the verbatim user block).
- **D — no collapse to 2 / floor honored:** `len(out) >= floorExchanges+1`.
- **E — mid-turn overflow completes:** turn whose tool outputs exceed the slot assembles to a valid request.

## Risks + rollback
- **`tailStart` recompute order** — recompute from the post-snap tail (Test B), else span/tail overlap.
- **Elided tool messages** (`KindToolElided`) are still `Role:"tool"` with a live `ToolCallID` — `sanitizePairs`/exchange logic must treat them like normal tool messages (don't drop). Test A includes an elided case.
- **Hysteresis re-fire** — tail fraction 0.25–0.30 < 0.40 keeps `summary+tail` under the disarm watermark; hysteresis.go unchanged.
- **Fail-safe everywhere** — every new branch falls back to "leave intact." Archive stays source of truth; no data loss.
- **Rollback:** confined to two packages + one prompt + 4 sizing sites; no persisted-artifact shape change (old summaries still expand). Revert the two packages → exact prior behavior.
