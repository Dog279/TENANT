# Tenant Local-Model Adaptation

Status: DRAFT
Author: design session 2026-05-16
Supersedes specific assumptions in: DESIGN.md, MEMORY-DESIGN.md
Trigger: design pivot to local-only inference (no cloud)

---

## Constraint shift

| Original assumption | New reality |
|---|---|
| Cloud Claude / GPT / Gemini available | Local models only (Ollama / vLLM / llama.cpp) |
| 128K minimum, 1M maximum context | **Design target: 128K.** Qwen 3.6 and Gemma 4 generations handle this reliably across hardware tiers. |
| Cloud-grade tool calling | Best local matches cloud for single calls; nested/parallel/refusal still weaker below 70B |
| Cloud embedder (Voyage `voyage-3`) | Local embedder (Nomic / BGE-M3 / Qwen3-Embedding) |
| Reasoning depth = Claude Opus 4.7 | 30-200B param ceiling; reasoning chains drift, bound loops |
| Per-token cost matters | Hardware sunk cost; tokens/sec is the bottleneck |
| Single model serves all needs | Multiple models per agent (planner / executor / embedder) |

Everything above the MCP wire layer needs to be re-evaluated. The wire layer itself is fine
— JSON-RPC doesn't care which model you point it at.

---

## 2026 local model landscape (anchoring data)

**30B class (single 24GB GPU territory):**
- Qwen 3.6-27B / Qwen 3.6-35B-A3B (MoE, ~3B active) — strongest local tool calling in this tier
- GLM-4.5-Air — strong agentic
- Qwen3-Coder-30B-A3B-Instruct — best for code-focused agents
- Holo3-35B-A3B — top BenchLM verified agentic score (82.6)

**70B class (workstation, 2x24GB or 48GB single):**
- Llama 4 70B
- Qwen 2.5/3 72B — outperforms GPT-4 on several BFCL agentic benchmarks
- Mistral Large 2 (123B) — borderline this tier

**200B+ class (multi-GPU / MoE workstation):**
- DeepSeek V4 / V4-Flash — claims 1M but ~256K reliable
- Qwen 3.5 235B
- Llama 4 Behemoth (rumored)

**Context length — 128K design target.** The Qwen 3.6 and Gemma 4 generations both target
128K natively, with materially better long-context retrieval than the 2025 generation.
Designing for 128K across the whole model range lets us ship one budget table instead of
three and lets the agent run on the same prompts whether the user has a 30B or a 200B
backend loaded.

| Class | 128K behavior | Notes |
|---|---|---|
| 27-35B (Qwen 3.6-35B-A3B, Gemma 4 27B) | Usable; some middle-degradation past ~60K | Use sandwich placement; lean on retrieval |
| 70-72B (Qwen 3.6-72B, Gemma 4 70B, Llama 4 70B) | Solid through ~96K, gentle drift past | Best price/perf at 128K |
| 200B+ MoE (DeepSeek V4, Qwen 3.5 235B) | Reliable through 128K; can stretch to 256K | Use 128K mode unless user opts in |

**Tool calling quality cliff:** single function calls work everywhere 30B+. Nested arguments,
parallel calls, and tool refusal degrade sharply below 70B. Plan accordingly — context
length does NOT fix tool-calling reliability; that's a separate problem solved by retrieval
gating, grammar constraints, and validation.

**Inference backends:**
- **vLLM** — production. Has guided decoding (grammar / JSON schema / regex). Native tool-call format support. The right pick for any multi-user or production workload.
- **Ollama** — single-user. Convenient. Structured output support is limited. Streaming + tool calls is broken (use stream=False).
- **llama.cpp** — embedded. Pure CPU/GPU local. The fallback when nothing else fits.
- **LM Studio / KoboldCpp / others** — OpenAI-compatible HTTP. Cover via a generic backend.

---

## What changes in Tenant

### MCP core protocol layer

**Unchanged.** JSON-RPC is JSON-RPC. The 919 LOC we already shipped is fine.

Two upgrades to plan for now that we know the model profile:
1. **`notifications/cancelled` emission** — already TODO'd in `session.go`. Small-model generations can take 30+ seconds; user-initiated cancel must work end-to-end. Promote from "nice-to-have" to v1 blocker.
2. **Streaming response support in Transport** — small models are slow (20-40 tok/s on a 4090); the UX needs token-streaming. Add a `StreamingTransport` extension to the interface.

### Memory architecture — major rework

The 1M-context budget was the load-bearing assumption. With 32K-128K reality, everything resizes.

**What changes:**

- **Soul shrinks** from 5K → 1-2K. Smaller models are more sensitive to prompt length; a 5K soul eats too much of a 32K window. Tighter persona, fewer instructions, more retrieval-leaning.
- **Working set shrinks** from 30K-200K → 4-30K. Last 5-15 turns verbatim, not 30-200.
- **Retrieval matters MORE.** When the working set can't hold everything, T2 episodic and T3 semantic do more heavy lifting. K (top retrieved) gets tuned per model class.
- **Compaction trigger fires earlier.** 60% of 32K is 19K, not 600K. Hierarchical summarization moves from "nice optimization" to "mandatory after every ~5 turns of growth."
- **Distillation cadence increases.** Atomic facts are denser than raw episodes; we want fewer episodes and more facts in context.
- **Tool definitions get retrieved**, not bulk-loaded. Small models drown in 20-tool soups. Pick top-K relevant tools per turn (vector retrieve against tool descriptions).
- **"Lost in the middle" placement matters.** Per Liu et al. and confirmed in 2026 local benchmarks: models retrieve best from the start and end of context. Sandwich pattern: critical info at top AND bottom of context, less-critical in the middle.
- **Embedder defaults flip to local.** No cloud Voyage. New defaults:
  - **Nomic Embed v1.5** (274MB, 768d, 8K ctx, MTEB 62.39) — lightweight default, runs on anything
  - **Qwen3-Embedding-8B** (variable 32-1024d Matryoshka, MTEB 70.58, SOTA open) — quality option when VRAM allows
  - **BGE-M3** (1024d, 8K ctx, 100+ languages, dense + sparse + multi-vector hybrid) — multilingual

  Matryoshka dimensions let us **store at full 1024d, query at 256d** for fast first-pass retrieval, re-rank top hits at full dim. Major latency win on the read path.

### Token budget — single 128K table

One budget across the model range. Hardware tier affects tool-calling parallelism and plan-loop
depth, NOT context layout. Everything below assumes Qwen 3.6 / Gemma 4 / DeepSeek V4 class
backends.

| Tier | Budget | Notes |
|---|---|---|
| System + Soul | 2K | tight persona, no fluff |
| Tool defs (top-K retrieved) | 4K | NOT all tools, top-K only — see "Tool calling reliability" below |
| Working set (recent turns) | 25K | last ~25 turns verbatim, or compacted equivalent |
| Episode retrieval (K=6) | 5K | |
| Semantic facts (K=12) | 3K | facts are denser, keep more |
| Document scratchpad | 5K | optional — user-loaded docs, web research results |
| **Response reserve** | **84K** | room for multi-step responses, plan-then-execute output |
| **Total** | **128K** | |

**Placement strategy (sandwich pattern, mitigates lost-in-the-middle):** soul + working set
+ tool defs go at the START. Episode + fact retrieval results go at the END (right before
the active turn). The middle holds working-set history and scratchpad. Models retrieve best
from the edges; we put the most decision-critical context there.

**Compaction trigger:** at 60% of context (77K). Compact the oldest 30% of the working set
via hierarchical summarization (recent verbatim, medium-term topic summaries, old as themes).
Async via the MCP 2026 Tasks primitive — non-blocking, runs while the agent continues.

**Sizing knobs that ARE per-class** (not the budget table itself):

| Profile field | 30B class | 70B class | 200B class |
|---|---|---|---|
| `MaxToolsPerCall` (top-K tools retrieved per turn) | 3 | 5 | 10 |
| `MaxParallelTools` | 1 (serialize) | 3 | 5 |
| `PlanLoopCeiling` | 5 iterations | 10 | 15 |
| `RequiresGrammarConstraints` | yes (hard) | yes (recommended) | yes (recommended) |

Soul size and working-set tokens stay constant. What varies is how many tools the model
can juggle and how deep the planner can recurse before drift takes over.

### NEW: Model abstraction layer

New package `internal/model/` becomes load-bearing. Without this we'd hard-code Anthropic
assumptions; with it we can hot-swap inference backends per agent or per call.

```go
package model

type LLM interface {
    Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)
    GenerateStream(ctx context.Context, req GenerateRequest) (<-chan StreamChunk, error)
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}

type GenerateRequest struct {
    Messages       []Message
    Tools          []ToolSpec       // pre-filtered by retrieval, not the full registry
    ToolChoice     ToolChoice       // "auto" | "required" | specific tool name
    JSONSchema     json.RawMessage  // for grammar-constrained generation
    MaxTokens      int
    Temperature    float32
    StopSequences  []string
}

type Profile struct {
    ID                       string     // "qwen3.6-35b-a3b-instruct"
    Role                     string     // planner | executor | coder | summarizer | embedder | default
    Backend                  Backend    // ollama, vllm, llamacpp (vLLM is v1 default)
    Endpoint                 string     // e.g. http://your-llm-host:8000
    ContextLength            int        // 128_000 — what the model supports
    OperationalContextBudget int        // ~80% of ContextLength — what we'll actually use under concurrent load (per Codex review)
    ReserveSoul              int        // identity / persona / persistent user facts (T0) — varies per model class
    ReserveSystemPrompt      int        // structural rules, format specs, tool-use protocol — varies per model class
    ReserveToolDefs          int        // tokens reserved for tool definitions
    ReserveResponse          int        // tokens reserved for assistant response
    ToolFormat               ToolFormat // qwen, llama, gemma, mistral, openai
    EmbedDim                 int        // 768 (nomic) / 1024 (bge-m3, qwen3-embed)
    SupportsGrammar          bool       // vLLM yes, Ollama partial, llama.cpp yes
    MaxToolsPerCall          int        // top-K tools retrieved per turn (3 / 5 / 10)
    MaxParallelTools         int        // 1 (30B serialize) / 3 (70B) / 5 (200B)
    PlanLoopCeiling          int        // 5 (30B) / 10 (70B) / 15 (200B) ReAct iterations max
    Capabilities             map[string]any // advisory metadata: reasoning_depth, code_quality, tool_call_reliability, latency_tier, structured_output_fidelity (per Codex review — used by v1.1 capability-based routing)
}

// LLM interface (added per Codex review — token counting must live in the model layer,
// not be guessed by the memory layer):
type LLM interface {
    Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)
    GenerateStream(ctx context.Context, req GenerateRequest) (<-chan StreamChunk, error)
    TokenCount(ctx context.Context, text string) (int, error)  // via vLLM /tokenize
}

type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Registry maps model ID → Profile. Populated at startup from a
// shipped YAML (profiles/*.yaml) plus user overrides.
type Registry struct {
    profiles map[string]Profile
}
```

**Why this matters:** every other layer (memory, planner, tool dispatcher) consults the
active model's `Profile` to size its outputs. Soul size, tool count, retrieval K, parallel
tool ceiling all flow from one source of truth.

### Tool calling reliability layer (NEW)

Small models hallucinate tool args. Mitigations stack:

1. **Tool retrieval per turn** — embed tool descriptions, retrieve top-K by intent similarity, present only those to the model. A 30B model with 3 relevant tools beats a 30B model with 30 tools every time.
2. **Grammar-constrained generation by default** — vLLM guided decoding (JSON schema or context-free grammar). Ollama path uses `format: json` plus best-effort schema in the prompt. If the backend has no constrained-decoding support, the runtime warns at startup and disables tool calling for that backend.
3. **Strict schema validation post-generation** — JSON schema validate every tool call before dispatch. On failure, retry once with the error message injected into the prompt. After 2 failures, surface to user instead of looping.
4. **Serialize tool calls below 70B.** `MaxParallelTools=1` for 30B class. Small models routinely emit valid JSON for one tool but fail at parallel arrays.
5. **Tool definition style adapter** — each model family has its preferred prompt format (Qwen Hermes-style, Llama `<|python_tag|>`, Mistral their own JSON). The adapter lives in `internal/model/toolfmt/`.

### Planning loop adaptation

Original design implied open-ended ReAct loops. With local models, that drifts. Constraints:

- **Hard ceiling: 5 iterations** for 30B, 10 for 70B+, 15 for 200B+. After the ceiling, bail.
- **Goal re-statement every iteration** — prepend `"You are working on: {original_goal}"` to every loop turn. 30B models forget the goal by step 3 without this.
- **Plan-then-execute scaffold** for tasks with >2 steps. Force the model to emit a numbered plan first (constrained-decoded), then execute step-by-step. Prevents drift.
- **Explicit failure escape** — after N tool-call failures, the loop exits to user instead of trying again. No "loop until something works" — local models will literally loop forever.

### Soul / identity gets MORE important

With small models, prompt engineering IS the stability mechanism. The soul becomes the
primary anchor against drift.

Concrete changes:

- **Per-model-size soul templates.** A soul that works for Qwen 72B may overwhelm Qwen 30B. Ship 3 templates: `compact` (30B), `standard` (70B), `extended` (200B).
- **Persistent reminder pattern** — re-inject the top 3 user facts every N turns inside the system prompt, not just at session start. Combats long-conversation drift.
- **Soul auto-edit threshold raises to never.** Original doc allowed auto-apply at confidence >0.95. With small models proposing the edits, ANY soul change must be human-reviewed. Period.

### Multi-agent / cross-LLM discussion

Still works architecturally. Expectations change:

- Two 30B models talking is closer to "two interns brainstorming" than "two senior engineers." Set user expectations.
- Use a **mixed-size pool**: one 70B+ as "lead" for planning, multiple 30B's as "workers" for parallel tool execution. Lead delegates, workers report back, lead synthesizes. Plays to each tier's strengths.
- Cross-agent memory sharing is unchanged — the store doesn't care.

---

## What gets cut

These were in the original design and don't earn their cost with local models:

- **1M context optimization.** Cut. Build for 32K-128K reliable.
- **Subtle multi-step planning.** Replaced with bounded plan-then-execute scaffolding.
- **Quiet self-improvement.** Already flagged dangerous in original; doubly so with small models. Human review on every soul edit.
- **Open-ended deep research loops.** Replaced with bounded research (max N web fetches, max M summarization rounds, then return what we have).
- **Complex parallel tool fan-out (>3 in flight).** Below 70B, serialize. Above 70B, cap at 3 below 200B, 5 above.

---

## What gets promoted

These were "v1.1" or "nice-to-have" before; with local models they become v1 blockers:

- **Tool retrieval** (filter to relevant tools per turn) — was v1.1, now v1.
- **Grammar-constrained generation** — was assumed, now must verify per backend.
- **Validation + retry on tool calls** — was implicit, now explicit gate.
- **Hierarchical context compaction** — was optional, now mandatory beyond ~30 turns at 32K.
- **Per-model profile registry** — was implied, now load-bearing.
- **`notifications/cancelled`** — was TODO'd, now v1.
- **Streaming Transport extension** — was "later", now v1 for UX.

---

## Updated package layout

Adds two new packages to the previous layout:

```
internal/
├── mcp/                 # UNCHANGED — already built (919 LOC)
├── memory/              # mostly unchanged; budgets shrink, embedder swaps
│   └── ... (see MEMORY-DESIGN.md, adjust budgets per profile)
├── model/               # NEW — model abstraction
│   ├── model.go         # LLM interface, GenerateRequest, StreamChunk
│   ├── profile.go       # Profile + Registry
│   ├── profiles/        # shipped YAML profiles
│   │   ├── qwen3-coder-30b.yaml
│   │   ├── qwen3-72b.yaml
│   │   ├── llama4-70b.yaml
│   │   ├── deepseek-v4.yaml
│   │   └── ...
│   ├── backend/
│   │   ├── backend.go   # Backend interface
│   │   ├── ollama.go
│   │   ├── vllm.go
│   │   ├── llamacpp.go
│   │   └── openai_compat.go
│   └── toolfmt/         # per-model tool-call format adapters
│       ├── qwen.go
│       ├── llama.go
│       └── mistral.go
└── agent/               # NEW — agent runtime (planner + tool dispatcher)
    ├── agent.go
    ├── planner.go       # plan-then-execute scaffold
    ├── loop.go          # bounded ReAct loop with retry / cancel
    ├── toolretrieval.go # top-K tool selection per turn
    └── validate.go      # JSON-schema gate on tool calls
```

---

## Backend support matrix (v1)

| Backend | Streaming | Tool calls | Grammar / JSON schema | When to use |
|---|---|---|---|---|
| **vLLM** | yes | yes (native) | yes (guided decoding) | Production / multi-user / any agent workload |
| **Ollama** | yes (but broken with tools — use stream=false then) | yes | partial (`format:json` + prompt) | Single-user, low scale, easy install |
| **llama.cpp HTTP** | yes | manual (via prompt) | yes (grammars) | Pure embedded, no other deps |
| **OpenAI-compat** | yes | yes (if backend supports) | depends | LM Studio, KoboldCpp, generic |

Default backend selection in v1: **Ollama for single-user setups (zero-config),
vLLM for power users with VRAM**. Pluggable per-agent via Soul config.

---

## Updated build order

Revises the main DESIGN.md schedule. New week 5 inserts the model + memory + agent runtime
since nothing useful runs without them.

| Week | Original plan | Updated plan |
|---|---|---|
| 1 | Scaffold, MCP hello-world | UNCHANGED — already done |
| 2 | GSuite plugin | UNCHANGED |
| 3 | iMessage sidecar | UNCHANGED |
| 4 | Wiki plugin | UNCHANGED — but swap default embedder to Nomic Embed v1.5 |
| **5a** | (none) | **NEW: `internal/model/` — LLM interface + Profile + Ollama + vLLM backends** |
| **5b** | (none) | **NEW: `internal/memory/` — T0 Soul + T5 Archive + T2 Episodic (sqlite-vec)** |
| **5c** | (none) | **NEW: `internal/agent/` — bounded ReAct loop + tool retrieval + JSON validation gate** |
| 5 (orig) | SQL plugin | shifts to week 6 |
| 6 (orig) | Continual learning | shifts to week 7-8 (now sits on real memory + agent infra) |

Net schedule impact: +2-3 weeks vs original. Worth it — these are the load-bearing pieces
that everything else depends on.

---

## Honest cuts from MEMORY-DESIGN.md

Specific revisions to the prior memory doc:

| MEMORY-DESIGN.md said | Now says |
|---|---|
| Embedder: Voyage `voyage-3` (cloud, 1024d) | Embedder: Nomic Embed v1.5 (768d) default, Qwen3-Embedding-8B for quality |
| Soul ~5K always-in-context | Soul ~2K. Single template; per-model tuning is a v1.1 refinement, not v1. |
| Working set 30K (128K budget) / 200K (1M budget) | 25K (128K budget). One table, all model classes. |
| Compaction at 60% of context | Same trigger. Absolute threshold = 77K. Async via MCP Tasks primitive. |
| K=8 episodes / K=8 facts default | K=6 episodes / K=12 facts. Constant across model class. |
| Tool definitions in context (implicit full registry) | Tool retrieval: top-K relevant tools per turn. K varies per model class (3 / 5 / 10). |
| Soul auto-edit at confidence >0.95 | Soul auto-edit: never with local models. Human review required. |
| 1M context table | Removed. Single 128K table above. |

The five-tier architecture (T0-T5) survives. The storage choice (sqlite-vec → pgvector
upgrade path) survives. The MCP-server exposure pattern survives. What changes is sizing
and emphasis.

---

## Open questions (small-model-specific)

1. **Default model for "out of the box"?** Recommend shipping with two officially-supported
   defaults: **Qwen 3.6-35B-A3B** (best local tool calling, MoE so ~3B active, fits 24GB
   VRAM at 4-bit) and **Gemma 4 27B** (broader hardware support, Google's instruction-tuning
   strength). Either works as the default — Qwen for power users, Gemma for accessibility.
2. **Ship with which inference backend default?** **Ollama for zero-config**, **vLLM
   documented as the production upgrade path** (required for full guided-decoding + native
   tool calling).
3. **Mixed-model agent: one 70B planner + multiple 30B workers?** Defer to v1.1. The
   single-model path needs to work first. Architecture supports it because Profile is
   per-call, but we don't ship the orchestration pattern in v1.
4. **JSON-schema enforcement: hard-fail or best-effort?** **Hard-fail with one retry, then
   surface to user.** Never silent loop.
5. **Tool retrieval K per model class?** 3 / 5 / 10 as defaults. Tune from real traces.
6. **Soul template — single template or per-model variants?** **Single template for v1.**
   Per-model tuning is a v1.1 refinement once we have usage data.
7. **Embedder swap penalty** — switching embedders requires re-indexing. The architecture
   already separates the embedder from the store (Embedder interface). Migration: run new
   embedder over existing rows, write new embedding column, swap atomically. Document the
   pattern in v1; don't build a UI for it.
8. **Quality benchmark suite?** Once we have an agent loop, adopt **BFCL (Berkeley Function
   Calling Leaderboard)** for tool-call accuracy regression testing + a small Tenant-internal
   eval suite for end-to-end agent behavior on representative tasks.

---

## Recommended next move

The MCP protocol layer is solid. The honest path is:

**A) Build the model abstraction layer first** (`internal/model/`). Nothing useful runs
without it; every other layer (memory, agent, plugins) consults it. ~500-800 LOC for
interface + Ollama + vLLM backends + shipped profiles for Qwen 3.6-35B-A3B, Qwen 3.6-72B,
Gemma 4 27B, Gemma 4 70B, DeepSeek V4. ~1 week.

**B) Then T0 Soul + T2 Episodic with sqlite-vec + Nomic embedder.** Memory pieces that need
the model layer to embed. ~600 LOC. ~1 week.

**C) Then bounded agent loop + tool retrieval + JSON validation.** The actual planner.
~600 LOC. ~1 week.

After that we have a working tiny agent and can wire actual plugins. Total ~3 weeks to
"agent that responds in iMessage using GSuite, with persistent memory, running on Qwen 3.6
or Gemma 4 locally with a 128K window."

Alternative path I'd reject: building plugins (GSuite, etc.) before the model + agent layer. They'd ship but the agent wouldn't know how to use them well on a small model — the per-turn tool count, JSON validation, and retrieval gating all live in the layer we'd be skipping.

---

---

## GSTACK REVIEW REPORT

| Review | Trigger | Why | Runs | Status | Findings |
|--------|---------|-----|------|--------|----------|
| CEO Review | `/plan-ceo-review` | Scope & strategy | 0 | — | — |
| Codex Review | `/codex review` (plan-review variant) | Independent 2nd opinion | 1 | issues_found | 4 substantive findings: tokenizer-aware counting gap (adopted), supported-vs-operational context distinction (adopted), capability metadata on Profile (adopted as hybrid), simplification of backend pluggability (already deferred). 1 disagreement: drop LLM interface — kept for test infrastructure value. |
| Eng Review | `/plan-eng-review` | Architecture & tests (required) | 1 | clean | 3 architecture decisions made via question (interface split, role taxonomy, parallelism). 5 code-quality calls without question. 37 test gaps identified for greenfield code (0 currently covered). 5 performance calls without question. 3 TODOs captured. Codex outside voice triggered 4 additional changes incorporated. |
| Design Review | `/plan-design-review` | UI/UX gaps | 0 | — (no UI in this PR) | — |

**CODEX:** Surfaced 2 critical gaps the inside review missed (tokenizer-aware counting, supported-vs-operational context budget) and 1 architectural critique partially adopted as the hybrid role+capability path. Worth the 2 minutes.

**CROSS-MODEL:** Inside review and Codex agreed on parallelism strategy, interface split, backend deferral. Disagreed on routing primitive (resolved as hybrid). Disagreed on dropping abstraction layer entirely (kept LLM interface for test value).

**UNRESOLVED:** 0

**VERDICT:** ENG CLEARED with Codex-driven additions to scope (tokenizer + budget reserves + Capabilities map). Ready to implement `internal/model/`.

---

## Sources consulted

- [Best Open-Source LLMs for Agentic Coding 2026 — MindStudio](https://www.mindstudio.ai/blog/best-open-source-llms-agentic-coding-2026)
- [LLM Agent & Tool-Use Benchmarks (2026) — BenchLM](https://benchlm.ai/llm-agent-benchmarks)
- [Long Context Local LLMs May 2026 — PromptQuorum](https://www.promptquorum.com/local-llms/long-context-local-llms)
- [Best Local LLMs for Function Calling — InsiderLLM](https://insiderllm.com/guides/function-calling-local-llms/)
- [Ollama vs vLLM vs TGI Benchmark 2026 — Medium](https://medium.com/@anupkawarase.akz/ollama-vs-vllm-vs-tgi-local-llm-serving-benchmark-2026-ba7d8474fea7)
- [Reliable Structured Output from Local LLMs — Markaicode](https://markaicode.com/ollama-structured-output-pipeline/)
- [Tool Calling — vLLM docs](https://docs.vllm.ai/en/latest/features/tool_calling/)
- [Best Open-Source Embedding Models 2026 — BentoML](https://www.bentoml.com/blog/a-guide-to-open-source-embedding-models)
- [Best Embedding Model for RAG 2026 — Milvus Blog](https://milvus.io/blog/choose-embedding-model-rag-2026.md)
- [Ollama Embedding Models — Morph](https://www.morphllm.com/ollama-embedding-models)
- [LLM Function Calling Implementation Guide 2026 — PremAI](https://blog.premai.io/llm-function-calling-complete-implementation-guide-2026/)
