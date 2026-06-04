# Tenant Memory Architecture

Status: DRAFT
Author: design session 2026-05-16
Companion to: DESIGN.md (main v1 plan)

---

## Requirements (from session)

- Support context lengths **128K minimum, 1M maximum** (Claude Opus 4.7 / Sonnet 4.6 ceiling)
- **Vector-backed long-term memory** that multiple agents can read/write
- **Persistent identity/soul** — stable self-concept across sessions
- Support **agent-to-agent discussion** sharing memory
- "Quietly enhance the product per use case" (from main design doc)

---

## Research summary (May 2026 state of the art)

### Dominant pattern: tiered memory (MemGPT / Letta)

The canonical architecture from MemGPT (UC Berkeley, 2023) and its commercial fork Letta is
three tiers: **core memory** (always in-context, like RAM), **recall memory** (searchable
conversation history, like a disk cache), and **archival memory** (long-term vector store,
like cold storage). The runtime decides when to promote, summarize, or evict.

Five production patterns are documented across 21 frameworks in 2026: In-Process/Working-Only,
Flat External Vector Store, Tiered Memory, Knowledge-Graph + Vector Hybrid, and Enterprise
Context Layer. Tiered + KG-Hybrid are converging as the production defaults.

### Two competing libraries

- **Mem0** — lightweight memory layer you bolt onto any agent framework. SDK extracts facts
  from conversations, stores in a vector store, injects relevant memories on retrieval. Has an
  MCP-compatible local server (OpenMemory) that works with Claude Desktop, Cursor, etc.
- **Letta** — full agent runtime that owns the memory lifecycle (promote/demote/compress).
  Heavier integration but better long-horizon behavior.

The 2026 consensus: **start with Mem0-shaped patterns** during validation, **move to Letta-shaped
patterns** once long-horizon memory is proven worth the runtime investment. For Tenant, we own
the runtime anyway — so we can adopt the Letta-shaped lifecycle without taking the dependency.

### MCP-native memory is already a category

The official `modelcontextprotocol/servers` repo ships a Memory server (knowledge-graph
based). Mem0 ships OpenMemory as an MCP server. The MCP 2026 roadmap is adding a **Tasks
primitive** for async long-running operations — this is the right substrate for "compact this
6-hour conversation in the background."

**Implication for Tenant:** memory should be exposed as MCP resources + tools so other MCP
clients can read from Tenant's memory store. This is a network effect — Tenant becomes the
memory layer for the user's whole agent ecosystem, not just Tenant itself.

### Vector DB landscape — honest pushback on Mongo / MariaDB

You suggested Mongo or MariaDB. Both work but neither is the natural 2026 choice. The data:

| Store | Speed vs Mongo | Scale ceiling | Best for | Caveats |
|---|---|---|---|---|
| **pgvector** | 4–15x faster | ~5–10M vectors native, >100M with pgvectorscale | Self-hosted production | Postgres operational overhead |
| **sqlite-vec** | Competitive up to ~1M vectors (brute-force, no ANN as of early 2026) | ~1M vectors | Embedded / personal / single-user | No ANN index yet; doesn't scale past low millions |
| **MongoDB Atlas Vector Search** | Baseline | Tested to 15.3M vectors with <50ms p99 (with quantization) | Teams already on Atlas | Cloud-only — self-hosted Mongo Community does NOT have it |
| **MariaDB vector** | Mature in 11.7+ | Documented up to ~10M | Teams already on MariaDB | Younger ecosystem; fewer Go drivers with first-class vector support |

**Recommendation:** pgvector for production, sqlite-vec for embedded. Don't pick Mongo unless
you're already running Atlas — self-hosted Mongo loses the feature entirely. Don't pick MariaDB
unless the rest of the stack is already MariaDB.

### Context length / compaction

Claude Opus 4.7, Opus 4.6, and Sonnet 4.6 all support a 1M context window. Compaction best
practice from Anthropic's own cookbook: **compact at ~60% utilization** to avoid context rot
before quality degrades. The recommended pattern is hierarchical summarization — recent turns
verbatim, medium-term summarized at topic level, older compressed to high-level themes.

---

## Tenant memory architecture

Six tiers, modeled on the Letta tier idea but adapted for our shipping constraints. Each tier
has its own storage backend, retrieval policy, and token budget.

```
┌─────────────────────────────────────────────────────────────────┐
│  PROMPT ASSEMBLER  (budget-aware, runs every turn)              │
│  ┌─────────────┐ ┌─────────────┐ ┌─────────────┐ ┌───────────┐ │
│  │ T0: Soul    │ │ T1: Working │ │ T2 retrieval│ │ T3 facts  │ │
│  │ ~2-5K       │ │ Recent N    │ │ Top-K       │ │ Top-K     │ │
│  │ always in   │ │ turns       │ │ episodes    │ │ semantic  │ │
│  └─────────────┘ └─────────────┘ └─────────────┘ └───────────┘ │
└─────────────────────────────────────────────────────────────────┘
        │                ▲              ▲              ▲
        │                │              │              │
        ▼                │              │              │
┌─────────────┐  ┌──────────────┐  ┌───────────┐  ┌──────────┐
│ T0 Soul     │  │ T5 Archival  │  │ T2 Episodic│  │ T3 Semantic│
│ TOML files  │  │ JSONL append │  │ sqlite-vec │  │ sqlite-vec │
│ git-tracked │  │ rotating     │  │ + fts5     │  │ (distilled)│
│ ~10KB each  │  │ ~unbounded   │  │            │  │            │
└─────────────┘  └──────────────┘  └───────────┘  └──────────┘
                        ▲                ▲              ▲
                        │                │              │
                  ┌─────┴────────────────┴──────────────┘
                  │ MEMORY WRITE PIPELINE (per turn)
                  │  1. Append raw turn → T5 archival
                  │  2. Embed + index turn → T2 episodic
                  │  3. Async distill job → T3 semantic facts
                  └────────────────────────────────────────
```

### T0: Soul / Identity

**What:** Stable self-concept the agent carries across every session.
- Persona (name, role, voice)
- Values (what the agent cares about)
- User profile (who the user is, persistent preferences the user explicitly told us)
- Operating instructions (e.g. "always cite sources", "prefer concise responses")

**Storage:** Plain TOML at `~/.config/tenant/soul/{agent_id}.toml`. Git-tracked so changes
are auditable. Small (target <10KB per agent).

**Lifecycle:** Hand-curated initially. Agent can *propose* edits via a `propose_soul_edit`
tool — proposed edits land in a review queue (`soul/proposed/`), require `tenant soul accept`
to merge. This is the "loud self-improvement" pattern from the main design doc — visible,
reversible, never silent.

**In context:** Always. Cost: ~2-5K tokens.

### T1: Working memory

**What:** The current conversation, in full fidelity.

**Storage:** In-process (Go struct, backed by T5 archival on each append for durability).

**Lifecycle:** Sliding window with anchoring. Always keep the last N turns verbatim. When
budget pressure hits, compact older turns via hierarchical summarization. Compaction trigger
fires at **60% of context budget** per Anthropic's published guidance.

**In context:** Always. Cost: variable, the dominant variable token consumer.

### T2: Episodic memory (vector-indexed events)

**What:** Every turn-pair (user prompt + assistant reply + tool calls + outcome) stored
individually with embedding + metadata.

**Storage:** SQLite + sqlite-vec (v1) → Postgres + pgvector (v2). Schema:

```sql
CREATE TABLE episodes (
  id INTEGER PRIMARY KEY,
  agent_id TEXT NOT NULL,         -- which agent recorded this
  visibility TEXT NOT NULL,       -- 'private' | 'shared' (for multi-agent)
  ts INTEGER NOT NULL,            -- unix epoch
  prompt TEXT NOT NULL,
  response TEXT NOT NULL,
  tool_calls JSONB,               -- normalized tool sequence
  outcome TEXT,                   -- 'success' | 'error' | 'unknown'
  user_feedback TEXT,             -- 'ack' | 'undo' | null
  session_id TEXT,
  tags TEXT                       -- json array
);
CREATE VIRTUAL TABLE episodes_vec USING vec0(
  id INTEGER PRIMARY KEY,
  embedding FLOAT[1024]           -- voyage-3 = 1024d
);
CREATE VIRTUAL TABLE episodes_fts USING fts5(prompt, response, tags);
```

**Retrieval:** Hybrid — cosine top-K on `episodes_vec` + BM25 top-K on `episodes_fts`,
merged via reciprocal rank fusion (RRF). Filter by `agent_id` + `visibility` for ACL.

**Lifecycle:** Append-only. Never delete (legal/audit). Use `tombstoned` flag if user requests
removal from retrieval set.

**In context:** Top-K retrieved per turn. Default K=8. Cost: ~3-5K tokens.

### T3: Semantic memory (distilled facts)

**What:** Atomic, deduplicated facts extracted from episodes. "User prefers Python over JS."
"User's daughter is named Mia." "User's project Tenant uses Go 1.26."

**Storage:** Same SQLite/Postgres store, separate table:

```sql
CREATE TABLE facts (
  id INTEGER PRIMARY KEY,
  agent_id TEXT NOT NULL,
  visibility TEXT NOT NULL,
  fact TEXT NOT NULL,             -- atomic claim, one sentence
  source_episode_ids TEXT,        -- json array — provenance
  confidence REAL NOT NULL,       -- 0-1
  first_seen INTEGER NOT NULL,
  last_confirmed INTEGER NOT NULL,
  superseded_by INTEGER           -- if contradicted by newer fact
);
CREATE VIRTUAL TABLE facts_vec USING vec0(
  id INTEGER PRIMARY KEY,
  embedding FLOAT[1024]
);
```

**Lifecycle:** Background distillation job runs every N turns or every M minutes (whichever
first). Job prompt: "From these new episodes, extract atomic facts about the user, the
project, or the agent's preferences. Flag contradictions with existing facts."

Contradiction handling: newer fact `superseded_by` older; old fact stays in table for audit
but is filtered out of retrieval. Confidence decays linearly with time if `last_confirmed`
is stale.

**In context:** Top-K retrieved per turn. Higher retrieval priority than episodes (smaller,
denser, more reliable). Default K=8. Cost: ~1-2K tokens.

### T4: Procedural memory (tool sequences / skills)

**What:** Repeated successful tool sequences for specific intents. From main design doc.

**Storage:** Same SQLite/Postgres store, separate table. Indexed by intent embedding.

**Lifecycle:** Promotion job (deferred to v1.1 per main DESIGN.md).

**In context:** Not directly — these become *named tools* the planner can invoke explicitly,
not retrieved chunks.

### T5: Archival (raw transcripts)

**What:** Append-only JSONL of every (timestamp, agent, role, content, tool calls, response)
event. Source of truth — all other tiers can be rebuilt from this.

**Storage:** `~/.local/share/tenant/archive/{yyyy-mm}/{session_id}.jsonl`. Rotating monthly.

**Lifecycle:** Append-only. Never read at inference. Used for audit, replay, training data
collection.

**In context:** Never. Cost: 0 tokens.

---

## Token budgeting (128K → 1M)

Single function: `func AssembleContext(ctx, agent, budget int) Prompt`. Allocates budget
proportionally with hard reserves for soul + working set.

**At 128K (e.g. Haiku-class budget):**

| Tier | Budget | Notes |
|---|---|---|
| Soul (T0) | 5K | hard reserve |
| Working set (T1) | 30K | last ~30 turns or compacted equivalent |
| Episode retrieval (T2) | 6K | K=8, ~750 tok each |
| Semantic facts (T3) | 2K | K=8, ~250 tok each |
| Tool definitions | 5K | depends on registered tools |
| **Response reserve** | **80K** | hard reserve for assistant output |
| Total | 128K | |

**At 1M (Opus 4.7 / Sonnet 4.6):**

| Tier | Budget | Notes |
|---|---|---|
| Soul (T0) | 5K | hard reserve |
| Working set (T1) | 200K | huge — can keep ~200 turns verbatim |
| Episode retrieval (T2) | 40K | K=32, deeper context |
| Semantic facts (T3) | 10K | K=32 |
| Tool definitions | 10K | |
| Documents loaded by user | 200K | optional |
| **Response reserve** | **535K** | room for long-form output |
| Total | 1M | |

**Compaction trigger:** at 60% of budget, kick off background compaction of the working set's
oldest 30%. Hierarchical summarization: recent turns verbatim, medium-term topic summaries,
old material compressed to high-level themes. Compaction is a Tasks-primitive async job
(MCP 2026 spec) — non-blocking.

---

## Multi-agent / cross-LLM memory

Two agents talking to each other and learning together is the headline feature. Architecture:

```
┌─────────┐         ┌─────────┐
│ Agent A │◄───────►│ Agent B │
│ (own    │  MCP    │ (own    │
│  Soul)  │ session │  Soul)  │
└────┬────┘         └────┬────┘
     │                   │
     └────────┬──────────┘
              │
              ▼
    ┌──────────────────┐
    │ Shared memory    │  ← same sqlite-vec / pgvector store
    │ visibility=shared│
    └──────────────────┘
    ┌──────────────────┐
    │ Agent A private  │  ← visibility=private, agent_id=A
    │ memory           │
    └──────────────────┘
    ┌──────────────────┐
    │ Agent B private  │
    │ memory           │
    └──────────────────┘
```

**Identity per agent:** Each agent has its own Soul. When Agent A speaks, episodes are tagged
`agent_id=A`. When agents converse, both write episodes — A's view labeled "self/other=B",
B's view labeled "self/other=A". Two-perspective episodic memory of the same conversation.

**Visibility:**
- `private` — only the writing agent can retrieve
- `shared` — any agent in the same `tenant` namespace can retrieve
- `public` — across-tenant retrieval (multi-tenant v2 only)

**Coordination:** No bus needed for v1. Each agent reads/writes the store directly. SQLite-vec
handles concurrent readers fine; for high write contention move to Postgres + pgvector.

**Conflict on shared facts:** When Agent A asserts "X is true" and Agent B asserts "X is
false", facts are tagged with `source_agent_id` and the conflict is surfaced to the user
(not silently merged).

---

## MCP integration — Tenant memory as an MCP server

Tenant exposes its memory store as an MCP server. Other MCP clients (Claude Desktop, Cursor,
Zed, etc.) can connect and read/write. This is the network effect.

Resources exposed:
- `memory://soul/{agent_id}` — read-only soul
- `memory://facts/{agent_id}` — readable fact list (filtered by visibility)

Tools exposed:
- `memory_search(query, K, agent_id?, visibility?)` → episodes + facts ranked by relevance
- `memory_fact_add(fact, confidence)` → returns id
- `memory_episode_add(prompt, response, ...)` → returns id (mostly auto-called by runtime)
- `memory_soul_propose_edit(diff)` → goes to review queue

This means: if you use Claude Desktop with the Tenant MCP server connected, Claude reads from
your Tenant memory. Same for Cursor, Zed, anything MCP-compatible. **One memory layer, every
agent.**

---

## Recommended storage choice for Tenant v1

**SQLite + sqlite-vec + sqlite-fts5** — one file, embedded, matches the design-doc choice for
the wiki indexer (same store, same backup story). Brute-force vector search is fine up to ~1M
vectors which is way more than personal-scale needs.

Upgrade path: when multi-tenant or >1M vectors, swap to **Postgres + pgvector**. Schema is
SQL-compatible with minor syntax differences; the storage interface in Tenant abstracts both.

**Explicit reject:** MongoDB Atlas (cloud lock-in, self-hosted Mongo loses vector search) and
MariaDB (works but no ecosystem advantage and slower than pgvector in benchmarks). Both stay
documented as v2 escape hatches if your operational needs change.

---

## Package layout

```
internal/memory/
├── memory.go              # Store interface, Entry types
├── soul/
│   ├── soul.go            # T0: TOML load/save, propose-edit flow
│   └── soul_test.go
├── working/
│   └── working.go         # T1: in-process sliding window + compaction trigger
├── store/
│   ├── store.go           # Store interface (Episode, Fact CRUD + Search)
│   ├── sqlite/
│   │   ├── sqlite.go      # sqlite-vec + fts5 implementation
│   │   └── schema.sql
│   └── pgvector/          # v2
│       └── pgvector.go
├── archive/
│   └── archive.go         # T5: rotating JSONL writer
├── embed/
│   ├── embed.go           # Embedder interface
│   ├── voyage.go          # Voyage AI client (default)
│   └── ollama.go          # local fallback
├── distill/
│   └── distill.go         # T2 → T3 fact extraction job
├── assemble/
│   └── assemble.go        # Token-budgeted prompt assembly
└── mcp_server/
    └── server.go          # exposes memory store as an MCP server
```

---

## Open questions

1. **Embedding model default** — Voyage `voyage-3` (1024d, $0.06/M tokens) is the natural
   Anthropic-partner pick. Confirm or pick alternative.
2. **Local-embed fallback** — `nomic-embed-text` via Ollama (768d) means rebuilding the
   index if you switch. Worth it for offline mode?
3. **Distillation cadence** — every N turns? Every M minutes? Cost vs freshness tradeoff.
4. **Sharing model for v1** — single-user multi-agent (you have multiple agents on your own
   machine sharing memory), or true multi-tenant (other people's agents)? Big architectural
   delta — the former is week-7, the latter is a quarter.
5. **Soul-edit auto-apply threshold** — never auto-apply (always review)? Auto-apply at
   confidence >0.95? Setting matters because it determines how "quiet" self-improvement is.
6. **Per-agent retrieval scoping** — when Agent A retrieves, does it see Agent B's shared
   episodes by default, or opt-in?
7. **Encryption at rest** — soul + facts about the user are sensitive. SQLCipher? OS keychain
   for DB key?

---

## Build order

Inserts cleanly between weeks 4 and 6 of the main DESIGN.md plan:

| Week | Work |
|---|---|
| 4 (existing) | Wiki/markdown plugin — already uses sqlite-fts5 + Voyage embeddings |
| **5a (new)** | T0 Soul + T5 Archive — easiest tiers, no embedding deps yet |
| **5b (new)** | T2 Episodic store (sqlite-vec schema, write path) — reuses week 4's embedder |
| **5c (new)** | T1 Working set + token-budgeted assembler |
| **5d (new)** | T3 Semantic distillation job |
| **5e (new)** | MCP memory server (expose to external clients) |
| 6 (existing) | Continual learning — now sits on top of T2 retrieval |

Estimated insertion cost: ~2 weeks of solo evening/weekend work.

---

## Sources consulted

- [State of AI Agent Memory 2026 — Mem0](https://mem0.ai/blog/state-of-ai-agent-memory-2026)
- [Mem0 vs Letta vs MemGPT 2026 — TokenMix](https://tokenmix.ai/blog/ai-agent-memory-mem0-vs-letta-vs-memgpt-2026)
- [Agent Memory Architectures: Patterns and Trade-offs (2026) — Atlan](https://atlan.com/know/agent-memory-architectures/)
- [Best Vector Databases in 2026 — MarkTechPost](https://www.marktechpost.com/2026/05/10/best-vector-databases-in-2026-pricing-scale-limits-and-architecture-tradeoffs-across-nine-leading-systems/)
- [pgvector vs MongoDB performance — MyScale](https://www.myscale.com/blog/pgvector-vs-mongodb-comprehensive-performance-analysis/)
- [sqlite-vec vs pgvector comparison — Grokipedia](https://grokipedia.com/page/Comparison_of_sqlite-vec_and_pgvector)
- [Letta GitHub](https://github.com/letta-ai/letta)
- [Agent Memory Techniques — NirDiamant](https://github.com/NirDiamant/Agent_Memory_Techniques)
- [Memory in AI: MCP, A2A & Agent Context Protocols — Orca](https://orca.security/resources/blog/bringing-memory-to-ai-mcp-a2a-agent-context-protocols/)
- [Context windows — Claude API Docs](https://platform.claude.com/docs/en/build-with-claude/context-windows)
- [Context engineering: memory, compaction, tool clearing — Claude Cookbook](https://platform.claude.com/cookbook/tool-use-context-engineering-context-engineering-tools)
- [Claude 1M context window — MindStudio](https://www.mindstudio.ai/blog/claude-1m-token-context-window-agents)
- [MCP Roadmap 2026 — a2a-mcp.org](https://a2a-mcp.org/blog/mcp-2026-roadmap)
