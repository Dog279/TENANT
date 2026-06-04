# Deep Research — Go rebuild (Onyx-equivalent)

Status: **Phases A + B SHIPPED** · CLI `tenant research` + TUI `/research` ·
Phase C outlined · Date: 2026-05-22

> **Phase A (shipped)** — `cmd/tenant/research.go`: plan → waves of ≤3 concurrent
> researchers (reusing `TeamRuntime`) → `collapseCitations` → tools-off
> `synthesizeReport` → cited markdown report. Available as **CLI**
> `tenant research "<q>" [--agents 5 --parallel 3 --out report.md]` and **TUI**
> `/research <q>` (streams progress to the feed, report to chat, Esc-interruptible).
>
> **Phase B (shipped)** — iterative deepening: a depth-bounded reflective loop
> (`reflect` gap-analysis call → follow-up sub-questions → repeat) with a
> wall-clock cap (`--max-time`) and cross-cycle dedup (`normalizeQuestion`).
> `--depth 1` reduces to a single Phase-A pass; default `--depth 2`. The
> orchestration stays procedural Go (no in-agent `think_tool`) for deterministic,
> bounded cost (≤ `depth × agents` researchers). Unit-tested: plan/source parse,
> citation collapse + URL dedup, unmapped-marker safety, question normalization.
>
> **Remaining: Phase C** (sources + UX) — see §6c.

> Rebuild Onyx's "Deep Research" agent in Go, on top of Tenant's existing
> orchestration. Onyx Deep Research is a **2-level orchestrator-worker loop**:
> a lead orchestrator plans, dispatches ≤3 stateless research sub-agents in
> parallel, reflects across cycles, then synthesizes one cited report. That
> shape is ~70% already built in Tenant.

---

## 1. What Onyx Deep Research actually is (verified)

Confirmed from the live `onyx-dot-app/onyx` repo (`backend/onyx/deep_research/`):

- A hand-rolled orchestrator loop (`run_deep_research_llm_loop`, **not**
  LangGraph). Each cycle the orchestrator LLM gets 3 coordination tools:
  `research_agent` (spawn a sub-agent), `think_tool` (force reflection),
  `generate_report` (terminate + synthesize).
- **Plan** first: `RESEARCH_PLAN_PROMPT` → ≤6 independent, standalone
  sub-questions (a guide, not a fixed DAG). Optional clarification step before.
- **Parallel, stateless sub-agents**: orchestrator may emit multiple
  `research_agent` calls per cycle, run concurrently, **hard cap 3 parallel**.
  Each sub-agent runs its OWN bounded ReAct loop (`MAX_RESEARCH_CYCLES = 8`) with
  real search/web/open-url tools and writes an **intermediate report**. Sub-agents
  share no context — the orchestrator embeds everything they need in the call.
- **Reflect & deepen**: orchestrator `think`s between cycles, spawns follow-up
  sub-agents on gaps. Cycle cap `MAX_ORCHESTRATOR_CYCLES = 8` (4 for reasoning
  models); also terminates on diminishing returns or a 30-min wall-clock cap.
- **Synthesize**: `FINAL_REPORT_PROMPT` with **tools disabled**, ~20k-token
  budget, over all intermediate reports.
- **Citations**: each sub-agent keeps local `[1],[2]` markers + a source map;
  after the parallel join, `collapse_citations()` renumbers them into one global
  bibliography.

Closest open pattern: orchestrator-worker / plan-then-iterative-deepening with
reflection (≈ Anthropic's multi-agent research + GPT-Researcher's
plan→parallel-search→aggregate). NOT STORM, NOT static plan-and-execute.

---

## 2. What Tenant already has (the substrate)

| Onyx piece | Tenant equivalent (today) |
|---|---|
| Orchestrator loop | orchestrator agent + bounded planner loop (`internal/agent`) |
| `research_agent` (spawn) | `spawn_agent` tool → `TeamRuntime.Spawn` (`cmd/tenant/team.go`) |
| ≤3 parallel, concurrent | sub-agents already run concurrently on the bus |
| Join / collect results | `team_await` → `TeamRuntime.Await` returns each sub-agent's result |
| Sub-agent bounded ReAct + web | sub-agents get the composite toolset incl. the web plugin (`web_search/web_navigate/web_read`) + per-agent browser |
| Cycle cap | `PlanLoopCeiling` (now live-tunable via `/ceiling`) |
| Streaming intermediate steps | the TUI activity feed already streams sub-agent events (`TeamEvent`) |
| Stateless sub-agents w/ embedded task | `spawn_agent(role, task)` — task is self-contained |

**So the orchestrator-worker skeleton exists.** What's missing is the
*research-specific structure* on top of it.

---

## 3. The gaps to build

1. **A research mode/entry point** with a research-tuned orchestrator prompt
   (plan → dispatch → reflect → synthesize), distinct from the general team
   orchestrator. Surfaced as `tenant research "<q>"` + TUI `/research <q>`.
2. **Plan step** — decompose the query into ≤6 independent sub-questions before
   spawning (one LLM call). Today the orchestrator free-forms its spawns; we make
   the decomposition explicit and visible.
3. **Citation contract + collapse** — the genuinely new, fiddly piece:
   - Sub-agents capture the **URLs** they actually read (from `web_navigate`/
     `web_read` results, available in their working set) and end their
     intermediate report with a `## Sources` list, using local `[n]` markers
     inline.
   - The orchestrator **collapses** all sub-reports' local markers into one
     global, deduplicated bibliography (port of `collapse_citations`).
4. **Synthesis step** — after the join, a final **tools-off** generation with a
   large token budget that weaves the intermediate reports into one structured,
   cited report (vs today's free-form orchestrator answer).
5. **Reflection + deepening (Phase B)** — let the orchestrator inspect returned
   reports, identify gaps, and spawn a second wave; terminate on
   diminishing-returns / cycle cap / **wall-clock cap** (new).
6. **`think` step (optional)** — Onyx forces reflection with a no-op tool. Our
   planner already reasons in prose between iterations, so this is optional; we
   can add a `research_note` tool if we want explicit, streamed reflection.

---

## 4. Proposed Go design

### Entry points
- CLI: `tenant research "<question>" [--depth N] [--max-agents 3] [--web] [--out report.md]`
- TUI: `/research <question>` — streams plan, sub-agent branches, and the final
  report into the panes; offers to write the report to the wiki via `os_write_file`.

Both build on the existing `TeamRuntime` (so sub-agents get the full toolset,
the bus, per-agent browser, and the live model — including hot-swaps).

### Flow (Phase A — MVP, single wave)
```
research(question):
  1. plan      := LLM(RESEARCH_PLAN_PROMPT, question)      // ≤6 sub-questions, 1 call
     emit ResearchPlan event (streamed)
  2. for each sub-question (bounded to maxAgents, in waves of ≤3):
        id := team.Spawn(role="researcher", task=subQuestionPrompt)
     reports := team.Await(timeout)                        // concurrent, existing
  3. global := collapseCitations(reports)                  // renumber [n] → global biblio
  4. report := LLM(FINAL_REPORT_PROMPT, question, reports, global,
                   toolChoice=NONE, maxTokens=large)        // synthesis, tools off
  5. return report + bibliography
```

### Sub-agent contract (the research role prompt)
A spawned researcher is instructed to:
- Use `web_search`/`web_navigate`/`web_read` (and internal `wiki_search`,
  `memory_search`) to investigate ITS sub-question only.
- Track every source URL it reads.
- End with a markdown intermediate report: findings with inline `[n]` markers and
  a trailing `## Sources` list mapping `[n] → URL (title)`.
- Stay within its `PlanLoopCeiling` (the per-agent budget).

### Citation collapse (`collapseCitations`)
```
input:  []subReport{ text string, sources []Source{localN int, url, title} }
build:  globalMap[url] -> globalN   (dedup identical URLs across sub-agents)
for each subReport: rewrite inline [localN] -> [globalN] using its source list
output: combined text per sub-report (renumbered) + one global ## References
```
Pure string/map work in Go — no new deps. Robust to a sub-agent emitting no
sources (its `[n]` left as-is or dropped).

### Termination & budgets
- Per-agent: `PlanLoopCeiling` (live-tunable).
- Orchestrator waves: `--depth` (Phase A = 1 wave; Phase B = up to N).
- Parallelism: `--max-agents` waves of ≤3 (mirror Onyx's hard cap of 3 parallel).
- **Wall-clock cap** (new, Phase B): force synthesis after T minutes so a run
  always returns a report (Onyx's 30-min cap).

### Reuse, don't fork
- Spawning, the bus, `team_await`, per-agent browser, the composite toolset,
  model hot-swap, and event streaming are **reused as-is** from `TeamRuntime`.
- New code is concentrated in: a `research` orchestration wrapper, three prompts
  (plan / researcher / final-report), `collapseCitations`, and the CLI/TUI
  entry points.

---

## 5. Phasing

| Phase | Scope | Notes |
|---|---|---|
| **A — MVP** | plan → 1 wave of ≤3 concurrent researchers → citation collapse → tools-off synthesis; CLI + `/research` | Reuses TeamRuntime; the headline capability end-to-end |
| **B — Iterative deepening** | orchestrator reflects on wave-1 reports, spawns follow-up waves on gaps; `think`/`research_note` tool; diminishing-returns + wall-clock termination | This is the "deep" in deep research |
| **C — Sources + UX** | internal memory/wiki as first-class research sources alongside web; clarification step; richer streamed research timeline; auto-write report to wiki | Polish + enterprise feel |

Phase A is a real, shippable Deep Research. B is what makes it *deep*. C is
product polish.

---

## 6. Throughput & stability

- Deep Research is **expensive** (Onyx: 20–60+ LLM calls, minutes). It runs as an
  explicit mode the user invokes — never on a normal chat turn.
- All generation runs on the same local vLLM via the hot-swappable router
  (so a `/model use` mid-research applies to the next sub-agent — already true).
- Hard caps everywhere: ≤3 parallel (KV-cache pressure), per-agent ceiling, wave
  depth, wall-clock cap → a run is always bounded and always returns a report
  (forced synthesis on cap, exactly like Onyx).
- Graceful degradation: no web plugin → research falls back to internal
  memory/wiki sources; no embedder → keyword-only retrieval. Never hard-fail.
- Citations make output **auditable** — an enterprise requirement.

---

## 6b. Phase B — iterative deepening (detailed plan)

Phase A is a single linear pass (plan → one batch of waves → synthesize). B
turns that procedural pipeline into a **reflective loop**, reusing A's
`collapseCitations` + `synthesizeReport` **unchanged**. Only `deepResearch`
grows a loop:

```
plan(question) → subqs
cycle := 0
deadline := now + maxTime
for cycle < depth:
    runWaves(subqs, parallel)              // A's existing wave logic
    cycle++
    if cycle >= depth || now > deadline: break
    gaps := reflect(question, rt.Results())  // 1 tools-off LLM call
    if gaps is empty (model said DONE): break // diminishing returns
    subqs = dedupAgainstAsked(gaps)           // only NEW angles
collapse(rt.Results()) → synthesize          // always returns a report
```

- **`reflect(question, reportsSoFar) []string`** — one tools-off `Generate`:
  *"Given the question and findings so far, list up to K NEW sub-questions for
  important gaps or unresolved contradictions. If the findings sufficiently
  answer the question, output only DONE."* Parsed with the existing
  `parseNumberedList`; `DONE`/empty ⇒ terminate.
- **Termination (any of)** — `cycle == depth`; reflect returns DONE/no new
  questions; **wall-clock cap** (`maxTime`). On any exit we still
  collapse+synthesize whatever we have (Onyx's forced report — a run *always*
  returns).
- **New flags** — `--depth N` (default 2), `--max-time 20m`.
- **Dedup across cycles** — keep a normalized set of already-asked
  sub-questions (so reflect can't re-ask) and the set of already-cited URLs (so
  later waves add net-new sources). Cheap string-set bookkeeping.
- **No new agent tools** — our orchestrator is **procedural Go**, so `reflect`
  is a direct LLM call. We deliberately do NOT mirror Onyx's in-agent
  `think_tool`/`generate_report` "fake tools" (those exist because their
  orchestrator is itself an LLM tool-loop). Keeping orchestration in Go gives us
  deterministic control flow and a hard, bounded cost — better for enterprise
  stability.
- **Cost guard** — total researchers ≤ `depth × agents` (e.g. 2 × 5 = 10),
  plus `depth` reflection calls + 1 synthesis. Bounded and predictable.

### Bringing Deep Research into the TUI (`/research`) — fold into B

Phase A ships as the `tenant research` CLI (the clean, testable engine). To put
it where you actually work:

- Add a `ResearchControl` interface to `tui.Config` (`Research(question string)`),
  mirroring the `ModelControl` pattern.
- cmd-side impl holds the planner + the TUI's existing `TeamRuntime`. On
  `/research <q>` it runs `deepResearch` in a goroutine (off the UI loop),
  streams `say(...)` progress + sub-agent events into the feed (already wired via
  `observe → TeamEvents`), and posts the final cited report to the chat pane —
  with an offer to `os_write_file` it to the wiki.
- Small delta (the engine exists); this is the highest-leverage B item since the
  product is TUI-first.

## 6c. Phase C — sources + UX (later)

- **Internal sources as first-class citations** — `wiki_search` / `memory_search`
  results cited via `wiki:<file>` / `memory:<fact-id>` pseudo-URLs flowing through
  the same `collapseCitations` (extend `sourceLine` to accept non-http schemes).
- **Clarification step** — when the query is vague, ask 1–2 questions before
  planning (Onyx's `CLARIFICATION_PROMPT`). Skipped for detailed queries.
- **Persisted research runs** — store the report + per-finding provenance in a
  `research` archive (replay/audit; an enterprise ask).
- **Richer TUI timeline** — plan tree, per-finding live status, running citation
  count.
- **Quality**: optional cross-encoder rerank of sources, dedup of near-identical
  findings before synthesis.

## 7. Decisions needed

1. **Entry point**: a dedicated `tenant research` command + `/research` (recommended
   — clean mode boundary), or extend the existing `tenant orchestrate`?
2. **Scope now**: ship Phase A (single-wave plan→parallel→synthesize) first, then
   B? (Recommended — A is genuinely useful and de-risks B.)
3. **Sources**: web-only to start, or wire internal `wiki_search`/`memory_search`
   as research sources from day one?
4. **`think` tool**: add an explicit streamed reflection tool, or rely on the
   planner's existing prose reasoning?

---

## 8. Appendix — Onyx source map (for fidelity)

`backend/onyx/deep_research/dr_loop.py` (`run_deep_research_llm_loop`),
`dr_mock_tools.py` (`get_orchestrator_tools`: research_agent / think_tool /
generate_report), `tools/fake_tools/research_agent.py` (`run_research_agent_call`,
`MAX_RESEARCH_CYCLES=8`), `models.py` (`ResearchAgentCallResult`,
`CombinedResearchAgentCallResult`, `CitationMapping`), `chat/citation_utils.py`
(`collapse_citations`), prompts: `RESEARCH_PLAN_PROMPT`, `CLARIFICATION_PROMPT`,
`RESEARCH_REPORT_PROMPT`, `FINAL_REPORT_PROMPT`. Caps:
`MAX_ORCHESTRATOR_CYCLES=8` (4 reasoning), `MAX_FINAL_REPORT_TOKENS=20000`,
`DEEP_RESEARCH_FORCE_REPORT_SECONDS=1800`, "never more than 3 research_agent
calls in parallel". (Current `main` replaced the old LangGraph
`agents/agent_search` package with this plain orchestrator loop.)
