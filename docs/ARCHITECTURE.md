# Tenant — As-Built Architecture Reference

Status: LIVING DOC — update when a module's public API or storage changes.
Purpose: canonical reference for the code that exists. Read this first after
context compaction or when picking the project back up. For design *intent*
see DESIGN.md / MEMORY-DESIGN.md / LOCAL-MODEL-ADAPTATION.md; this doc
describes what is *actually built*.

Last verified: 2026-05-18 — 30 packages, ~17,300 LOC source, ~8,600 LOC tests,
333 tests passing on Go 1.26.3 (pure Go, no CGO; Chrome external like
vLLM/Ollama). **Web, SQL AND Wiki plugins verified live** with real
Gemma 4: web → go.dev cited answer + click mutated DOM + "Buy Now"
blocked; SQL → real SQLite, schema→query→correct answer, "DROP table"
blocked (DB intact, 3 rows); wiki → real Nomic 768d, cited answers, a
model tool-call *hallucination* caught via `TENANT_LOG=debug` and
fixed with deterministic RAG grounding. Same 3-property proof
(capability live / deterrence live / enforcement deterministic) for
web+sql; wiki is read-only (no gate by design — Karpathy ethos).
**GSuite (#4, Gmail+Calendar, dual auth: service-account+DWD or
gcloud ADC)** is deterministic + wire-contract verified (real RSA
JWT-bearer signature checked; gate blocks send/create with
"nothing left the building" proof) — its live Google leg is
**operator-run and labelled as such, not faked** (no creds in-env;
real inbox not touched uninvited). **X (#5, read+post, native Go port
of xurl: app-bearer + OAuth2-PKCE)** same stance — PKCE S256 math,
refresh-token rotation persisted, and the post/delete gate proven
deterministically; live X leg operator-run, not faked (no keys
solicited, real account not posted to). **iMessage (#6, read+send via
a BlueBubbles REST server)** same stance — envelope/endpoint wire
contract + send `tempGuid`/method shape + the send gate proven
deterministically; live leg operator-run (no BlueBubbles/Mac in env,
no one messaged). **All six spec'd plugins now shipped.** **Terminal
UI** (`tenant tui`, §5.14): a Bubble Tea full-screen experience —
streaming chat + live activity feed (memory/budget, tokens, tool
calls/results) + status bar; backed by a new opt-in agent `Observer`
event stream + token streaming (which also closed the StreamChunk.Error
v1 item). The one deliberate minimal-deps exception (owner-approved),
still pure Go.
`tenant doctor` verified by induced-failure runs. Verified end-to-end
with REAL models: generation = Gemma 4 (gemma-4-26b) on a networked DGX
Spark vLLM @ localhost:8000; embeddings = Nomic Embed v1.5 (768d)
via local Ollama OpenAI-compat @ localhost:11434. Heterogeneous
role-routing (vLLM gen + Ollama embed) confirmed. Semantic retrieval
ranks by meaning (cosine magnitude). **Tool calling hardened &
verified against real Gemma 4**: full round-trip (native tool_call →
arg normalization → validate → dispatch → result → replay → synthesis),
`tenant tool-test` → `add(17,25)=42` → "The result is 42."

---

## 1. What Tenant is

A Go-native MCP agent framework, single binary, local-LLM-only (no cloud).
Runs on a vLLM-first multi-endpoint fleet with role-based model routing
(planner / executor / coder / summarizer / embedder). Six-tier memory with
distillation and a self-improvement scheduler.

**Build:** `go build -o tenant ./cmd/tenant` (pure Go, no CGO, single binary)
**Test:** `go test ./...` (all 196 pass; `-race` needs `CGO_ENABLED=1`)
**Module:** `tenant` (local, no remote) — Go 1.26, toolchain go1.26.3

---

## 2. Package dependency graph

```
cmd/tenant  (wiring + demo)
   │
   ├──────────────┬───────────────┬──────────────┐
   ▼              ▼               ▼              ▼
internal/agent  internal/improve  (memory tiers) internal/model
   │                │                              │
   │                ▼                              │
   │          memory/distill ──────────────────────┤
   │                │                              │
   ▼                ▼                              ▼
memory/assemble  memory/episodic  memory/semantic  model/backend/vllm
   │              memory/archive   memory/soul      model/toolfmt
   ▼              memory/working                    model/testllm
internal/mcp ── mcp/transport                       model/profiles (go:embed)

Rule of thumb: model has NO deps on memory/agent. memory tiers are
independent of each other (assemble + distill compose them). agent sits
on top of everything. improve sits beside agent (scheduled jobs).
mcp is standalone (wire protocol only, no LLM knowledge).
mcpserver sits beside agent: mcp.Handler over stdio, reads soul +
episodic + semantic, exposes them to external MCP clients.
```

---

## 3. Physical storage map

| Tier / data | Format | Path (Windows shown) | Rationale |
|---|---|---|---|
| T0 Soul | TOML | `%APPDATA%\tenant\soul\{agent}.toml` | tiny, human-curated, git-trackable, diffable |
| Soul proposals | TOML | `%APPDATA%\tenant\soul\proposed\*.toml` | review queue (never auto-applied) |
| T1 Working | in-RAM `[]Message` | (process memory) | session-scoped, mirrored to T5 for durability |
| T2 Episodic | SQLite + FTS5 + BLOB | `%LOCALAPPDATA%\tenant\episodes.db` | hybrid vec+keyword retrieval |
| T3 Semantic | SQLite + FTS5 + BLOB | `%LOCALAPPDATA%\tenant\facts.db` | same + supersede/decay |
| T5 Archive | JSONL (append-only) | `%LOCALAPPDATA%\tenant\archive\YYYY-MM\{session}.jsonl` | write-once audit/replay |
| Scheduler cursors | SQLite KV | `%LOCALAPPDATA%\tenant\tenant_meta.db` | tiny durable state |

Config dir = `os.UserConfigDir()` (Roaming on Windows / `~/.config` Linux /
`~/Library/Application Support` macOS). Data dir = LOCALAPPDATA / `$XDG_DATA_HOME`
or `~/.local/share` / `~/Library/Application Support`. **Memory is never
Markdown** — Soul is TOML; markdown is only the transient render target of
`Soul.Render()` at prompt-build time.

---

## 4. Data flow — one agent turn

```
user query
  │
  ▼
agent.Turn(ctx, {UserQuery})
  ├─ Router.LLMForRole("planner")  → model.LLM + Profile
  ├─ Router.EmbedderForRole("embedder") → model.Embedder
  ├─ append user msg → working.Set + archive.Writer (write-through)
  ├─ embedder.Embed(query) → queryEmbedding
  ├─ tools.Search(embedding, profile.MaxToolsPerCall) → top-K ToolSpec
  │
  └─ LOOP (≤ profile.PlanLoopCeiling):
       ├─ assemble.Assemble(profile, soul, working, episodic, semantic, ...)
       │     → []model.Message  (sandwich placement, budget enforced)
       ├─ planner.Generate(messages, tools) → GenerateResponse
       ├─ append assistant msg → working + archive
       ├─ no tool calls?  → final: persist episode (T2), return
       ├─ validate tool calls (JSON + required fields)
       │     all invalid 2x → bail (ErrTooManyValidationFailures)
       └─ dispatchBatch(valid, profile.MaxParallelTools) → feed results back

      ceiling hit → synthesize final (Tools=nil), Truncated=true

[later, scheduled]  improve.Scheduler → DistillJob → distill.Run(cursor)
  → episodes since cursor → summarizer LLM → atomic facts
  → embed → semantic.Search(K=1) → Reaffirm (≥threshold) or Insert
  → cursor advanced in tenant_meta
```

---

## 5. Modules

### 5.1 `internal/mcp` + `internal/mcp/transport`

**Purpose:** MCP wire protocol (JSON-RPC 2.0). No LLM knowledge — pure
transport. Used by tool plugins (future) and when Tenant acts as an MCP
server (future).

**Files:** `mcp.go` (Message, ID, Error, std codes), `session.go` (Session,
Handler, Call/Notify, read pump, response routing), `initialize.go`
(lifecycle types), `client.go` (Client + lifecycle state machine),
`version.go` (`ProtocolVersion = "2025-06-18"`, `LibraryVersion`),
`transport/transport.go` (`Transport` interface, `ErrClosed`,
`ErrFrameTooLarge`), `transport/stdio.go` (newline-delimited stdio:
NewStdioSelf / NewStdioStreams / NewStdioProcess).

**Public API:**
- `mcp.NewSession(t transport.Transport, h Handler) *Session`
- `Session.Start(ctx)`, `.Call(ctx, method, params) (json.RawMessage, error)`, `.Notify(...)`, `.Close()`
- `mcp.CallTyped[Resp](ctx, s, method, params) (Resp, error)` — generic typed call
- `mcp.NewClient(t, opts...) *Client`; `Client.Initialize(ctx)`, `.Ping(ctx)`, `.Close()`

**Storage:** none. **Depends on:** nothing internal.
**Invariants:** no panics in protocol layer (malformed frames logged+skipped);
idempotent Close; strict protocol-version match; ctx propagation.
**Status:** complete, incl. the agent-on-MCP path: **`notifications/
cancelled`** (bidirectional — outbound on `Call` ctx-cancel; inbound
cancels the matching in-flight handler's ctx; a cancelled request gets
no response) and **`notifications/progress`** (`CallWithProgress`
tags `_meta.progressToken` + routes incoming progress to a per-call
sink; `SendProgress`/`ProgressTokenFrom` for the server side — this is
the spec-correct realization of "stream output mid-call"; the byte
transport was already full-duplex multi-frame, so no new transport
interface was needed). Both consumed at the session layer, not
forwarded to the app handler. **6 tests.**

### 5.2 `internal/model` (+ `backend/vllm`, `toolfmt`, `testllm`, `profiles`)

**Purpose:** abstraction over local LLM inference. Split `LLM` (generate) and
`Embedder` (embed) interfaces; role-based `Router`; per-model `Profile`.

**Public API:**
- `model.LLM`: `Generate`, `GenerateStream`, `TokenCount(ctx, text)`
- `model.Embedder`: `Embed(ctx, texts)`
- `model.Profile` — fields: `ID, Role, Backend, Endpoint, Model,
  ContextLength, OperationalContextBudget, ReserveSoul, ReserveSystemPrompt,
  ReserveToolDefs, ReserveResponse, ToolFormat, EmbedDim, SupportsGrammar,
  MaxToolsPerCall, MaxParallelTools, PlanLoopCeiling, Capabilities`
- `Profile.WritableBudget()` — operational budget minus all reserves; the
  number the assembler sizes against (NOT ContextLength)
- `model.NewRegistry(userDir) *Registry` — go:embed defaults + disk override,
  dup-ID = error; `Registry.ByID/ByRole/IDs`
- `model.NewRouter(reg, log) *Router`; `Router.RegisterBackend(kind, factory)`,
  `.ForRole`, `.PinRole`, `.LLMForRole(ctx, role) (LLM, Profile, error)`,
  `.EmbedderForRole(...)`
- `model.NewEmbedCache(cap) *EmbedCache` — LRU keyed by `(embedder_id, sha256(text))`
- `vllm.New(ctx, profile, log) (any, error)` — satisfies LLM + Embedder; uses
  `/v1/chat/completions`, `/v1/embeddings`, `/tokenize`; typed error mapping
- `toolfmt.AdapterFor(format) Adapter` — qwen / gemma / llama / mistral /
  openai; `FormatToolPrompt` + `ParseToolCalls` (safety net when vLLM's
  server-side parser misses)
- `testllm.New() *Fake` — programmable LLM+Embedder test double (records calls)

**Storage:** `profiles/*.yaml` embedded in binary via `//go:embed`. Shipped
roles: planner (qwen3.6-72b, gemma4-70b), executor (qwen3.6-35b-a3b),
embedder (qwen3-embedding-8b), summarizer (qwen3-summarizer).
**Depends on:** nothing internal (foundational layer).
**Invariants:** vLLM-first (other backends deferred); HTTP→typed-error
mapping (`ErrEndpointDown/ContextOverflow/RateLimited/Cancelled/Invalid/Internal`);
streaming terminal chunk carries `Error` (callers MUST check).
**Echo backend** (`model/backend/echo`): deterministic, dependency-free
LLM+Embedder. `Generate` echoes the last user message (returns minimal
valid `{"facts":[]}` JSON when `JSONSchema` is set, so distillation runs
offline). `Embed` = feature-hashed bag-of-words, L2-normalized (shared
words → nonzero cosine, so retrieval is genuinely demonstrable).
`EmbedDim=128`. Registered via `echo.New` (a `model.BackendFactory`).
This is what makes the whole stack runnable with zero external deps —
the CLI's default backend. Swap a profile's `Backend` to `vllm` +
real `Endpoint` for production; nothing else changes.
`model.NewEmptyRegistry()` + `Registry.Add(Profile)` let the CLI build
an all-echo role map in code (embedded YAML stays vLLM-only).
**Tool-calling wire contract (hardened vs real Gemma 4):** OpenAI/vLLM
`tool_calls[].function.arguments` is a STRINGIFIED JSON object, not a
nested object, both directions. Inbound: `normalizeToolArgs` unquotes
the string (tolerant of object/null/junk) so the agent's
`validateToolCall` map-unmarshal works. Outbound: `toWireMessages`
maps internal flat `model.ToolCall` → nested
`{id,type:"function",function:{name,arguments:"<stringified>"}}` —
without it, replaying an assistant tool-call message (turn 2 of any
tool flow) is rejected with HTTP 400. Both bugs were invisible to
fakes; only a real 2-turn tool round-trip surfaced them.
**Status:** complete for vLLM + echo, tool-calling real-model verified.
**64 tests** (model 24 + vllm 15 + toolfmt 17 + echo 8).

### 5.3 `internal/memory/soul` — T0 Identity

**Purpose:** persistent agent identity + persistent user facts. Always in
the system prompt. Hand-curated; agent edits go through a review queue.

**Public API:**
- `soul.Soul` — blocks: `Agent, Values, User, Instructions, Meta`
- `soul.Load(baseDir, agentID) (*Soul, error)` (`ErrNotFound` if absent)
- `soul.NewDefault(agentID) *Soul` — first-run scaffold
- `Soul.Save(baseDir)` — atomic (tmp+fsync+rename), bumps `Meta.Version`
- `Soul.Render() string` — markdown system-prompt fragment (transient, never stored)
- `soul.ProposeEdit(baseDir, agentID, reason, *Soul) (id, error)` → review queue
- `soul.ListProposals`, `soul.Accept(baseDir, id)`, `soul.Reject(...)`
- `soul.DefaultDir()` (config dir), `soul.Path`, `soul.SoulDir`

**Storage:** TOML at `{configDir}/soul/{agent}.toml`; proposals at
`soul/proposed/`. **Depends on:** `go-toml/v2`.
**Invariants:** soul edits NEVER auto-applied (loud self-improvement);
Render does not truncate (assembler enforces budget). **17 tests.**

### 5.4 `internal/memory/working` — T1 Working set

**Purpose:** in-process sliding window of the current conversation.

**Public API:** `working.New() *Set`; `Set.Append(Message)`, `.Messages()`
(defensive copy), `.Len()`, `.Trim(n) int`, `.Reset()`. `Message{Role,
Content, ToolCalls, ToolCallID, Timestamp}`.
**Storage:** RAM only (durability via T5 mirror by the agent loop).
**Invariants:** concurrent-safe; Trim keeps the most-recent N; compaction
(summarize old turns) is v1.1 — Trim drops wholesale for now. **9 tests.**

### 5.5 `internal/memory/episodic` — T2 Episodic

**Purpose:** every turn-pair, vector + FTS5 indexed for retrieval.

**Public API:**
- `episodic.Open(path) (*Store, error)` (":memory:" for tests; WAL mode)
- `Store.Insert(ctx, *Episode) (int64, error)`, `.Get`, `.Tombstone`,
  `.Count(ctx, includeTombstoned)`, `.List(ctx, ListFilter)` (chronological,
  `SinceID` cursor — used by distill), `.Search(ctx, Query) ([]Hit, error)`
- `Query{AgentIDs, Visibility, Embedding, Keywords, K, After, Before}`
- Search = brute-force cosine + FTS5 BM25, fused by reciprocal-rank-fusion
  (`rrfK=60`, `candidateLimit=50`)

**Storage:** SQLite `episodes.db` — table `episodes` + `episodes_fts`
(external-content FTS5) + 3 sync triggers. Embeddings = little-endian
float32 BLOB. **Depends on:** `modernc.org/sqlite` (pure Go).
**Fusion (rewritten, real-embedding-verified):** the vector channel
carries the cosine SIMILARITY, not just rank. `fuseRelevance` =
`0.7*cosine + 0.3*(1/ftsRank)` for hybrid hits; cosine-only or
down-weighted (`0.5*`) keyword-only. Rank-only RRF was near-flat on
small candidate sets (1/61 vs 1/64) and let confidence reorder
relevance — caught only by running real Nomic embeddings (orthogonal
test/echo vectors hid it). **Invariants:** tombstone hides from Search,
keeps for Get (audit); brute-force cosine OK to ~100K rows (ANN swap is
a TODO). **20 tests.**

### 5.5a `internal/memory/ftsutil` — shared FTS query sanitizer

**Purpose:** turn free-text into a safe + useful SQLite FTS5 MATCH
expr. `Sanitize(q)`: lowercases, splits on non-alnum (strips FTS5
operators `: * " ( ) -` → no injection), drops English stop words +
1-char tokens, OR-joins. Empty result → "" (store treats as no keyword
signal → vector-only). Shared by `mcpserver` + the CLI so they behave
identically. **Why it exists:** real embeddings exposed that
unfiltered queries ("what does the user enjoy") keyword-matched nearly
every row (all facts contain "the/user/is"), drowning the vector
signal in fusion. **6 tests.**

### 5.6 `internal/memory/semantic` — T3 Semantic

**Purpose:** distilled atomic facts. Denser, higher retrieval priority,
time-decayed confidence.

**Public API:**
- `semantic.Open(path) (*Store, error)`
- `Store.Insert`, `.Get`, `.Supersede(old, new)`, `.Reaffirm(id)` (bump
  last_confirmed), `.Tombstone`, `.Count(ctx, inclTomb, inclSuperseded)`,
  `.Search(ctx, Query) ([]Hit, error)`
- `Fact.EffectiveConfidence(now)` — linear decay: 30-day grace → 0 at 365d
- Search score = RRF × effectiveConfidence; decayed-to-zero facts drop out

**Storage:** SQLite `facts.db` — table `facts` (+ `superseded_by` FK,
`confidence`, `first_seen`, `last_confirmed`) + `facts_fts` + triggers.
**Invariants:** Search filters tombstoned AND superseded; supersede keeps
the chain for audit; Supersede-on-contradiction is v1.1 (LLM judge — TODO).
**21 tests.**

### 5.7 `internal/memory/archive` — T5 Archive

**Purpose:** append-only source-of-truth event log. Never read at inference.

**Public API:** `archive.NewWriter(baseDir) *Writer`; `Writer.Append(Event)`
(concurrent-safe, monthly rotation, session-ID sanitized for path safety).
`archive.NewReader(baseDir) *Reader`; `Reader.Stream(Filter) iter.Seq2[Event,error]`
(Go 1.23 iterator, chronological). `Event{Timestamp, AgentID, SessionID,
Role, Content, ToolCalls, ToolResult, Metadata}`.
**Storage:** JSONL at `archive/YYYY-MM/{session}.jsonl`, one Event/line.
**Invariants:** append-only (deletion = tombstone in higher tiers, never
rewrite history); per-Append open+close (concurrent-safe via mutex);
malformed line surfaces as stream error, doesn't halt iteration. **14 tests.**

### 5.8 `internal/memory/assemble` — Prompt assembler

**Purpose:** combine T0+T1+T2+T3 + tools + query into a budgeted
`[]model.Message` with sandwich placement.

**Public API:**
- `assemble.New(TokenCounter) *Assembler`; `Assembler.Assemble(ctx, Request) (*Result, error)`
- `TokenCounter` interface; `CounterFunc` (closures/tests);
  `NewLLMCounter(llm)` (prod, wraps `LLM.TokenCount`)
- `Request{Profile, Soul, SystemPrompt, Tools, Working, EpisodicStore,
  SemanticStore, Query{Embedding,Keywords,EpisodeK,FactK}, AgentID,
  Visibility, UserQuery, Shares}`
- `Result{Messages, BudgetReport{per-tier tokens, CompactionRecommended,
  Truncations}}`

**Placement:** `[system: soul+rules+tools]` → `[working turns]` →
`[user: retrieved facts+episodes + active query]`. Edges retrieve best.
**Budget:** WritableBudget split — default 65% working / 15% facts /
20% episodes (configurable via `Shares`). Truncates oldest working +
lowest-ranked retrieval. `CompactionRecommended` at 60% variable usage.
**Invariants:** fail-fast on count errors (no silent oversized prompts);
retrieval agent-scoped (no cross-agent leak). **14 tests.**

### 5.9 `internal/memory/distill` — T2→T3 distillation

**Purpose:** turn episodes into facts via the summarizer LLM.

**Public API:**
- `distill.Distiller{Router, Episodic, Semantic, AgentID, BatchSize,
  SimilarityThreshold, SummarizerRole, EmbedderRole, Logger}`
- `Distiller.Run(ctx, sinceEpisodeID) (*RunResult, error)`
- `RunResult{EpisodesProcessed, FactsExtracted, FactsInserted,
  FactsReaffirmed, LastEpisodeID, BatchErrors}`

**Flow:** list episodes > cursor → batch (default 15) → summarizer
(`systemPrompt` + `factsJSONSchema` grammar) → parse → embed all facts in
one call → per fact: `semantic.Search(K=1)` + direct cosine ≥ threshold
(0.92) → Reaffirm, else Insert with provenance.
**Invariants:** cursor advances past failed batches (no poison-batch wedge);
hard error does NOT advance; hallucinated source IDs filtered; Supersede +
semantic dedup are v1.1 (LLM judge — TODO).
**Real-model hardening (verified vs Gemma 4):** `extractJSONObject` pulls
the first balanced `{...}` out of noisy output (```` ```json ```` fences,
leading/trailing prose) — real models wrap JSON despite instructions and
vLLM `guided_json` is NOT reliably enforced across builds, so grammar
constraints are never trusted; output is always defensively parsed.
**13 tests** + extraction case-table. **Status:** complete, real-model verified.

### 5.10 `internal/agent` — Agent runtime

**Purpose:** the bounded ReAct loop. The thing that makes Tenant *do*
something.

**Public API:**
- `agent.New(Config) (*Agent, error)` — Config: `AgentID, SessionID,
  Router, Soul, Working, Archive, Episodic, Semantic, Tools, Dispatcher,
  Assembler?, Logger, PlannerRole, EmbedderRole, SystemPrompt,
  Observer?, Stream?`
- **Live observability:** `Config.Observer func(Event)` receives typed
  `Event`s mid-turn (turn_start / memory+budget / token / assistant /
  tool_call / tool_result / validation / final / truncated / error) —
  what the TUI renders. Opt-in: nil ⇒ zero overhead, unchanged behavior.
- **Streaming:** `Config.Stream` switches the planner call to
  `GenerateStream`, emitting each text delta as `EventToken` and
  checking the terminal `StreamChunk.Error` (closes that v1 item).
  Tool-call extraction is identical to the buffered path — the backend
  reassembles tool calls in both (incl. Gemma's text-block safety net,
  now also applied in `vllm` streaming). Opt-in: existing callers keep
  buffered `Generate`.
- `Agent.Turn(ctx, TurnRequest{UserQuery}) (*TurnResult, error)`
- `TurnResult{Response, ToolTrace, Iterations, Tokens, Reports[],
  Truncated, Error}`
- `agent.ToolRegistry` iface + `NewStaticRegistry()` (Register/Get/Search/All)
- `agent.ToolDispatcher` iface + `DispatcherFunc`
- Sentinels: `ErrLoopCeilingReached`, `ErrTooManyValidationFailures`

**Invariants:** loop bounded by `Profile.PlanLoopCeiling`; tool fan-out
bounded by `Profile.MaxParallelTools` (semaphore); 2 consecutive
all-invalid tool batches → bail; ceiling → forced synthesis (Tools=nil),
`Truncated=true`; archive best-effort (warn, don't fail turn); episode
persisted at end of turn (one row/pair, query embedding). **17 tests**
(incl. streaming token emission + streaming tool-call handling).

### 5.11 `internal/improve` — Self-improvement scheduler

**Purpose:** Hermes-style "Crons + closed learning loop", Go-native.
Multiple jobs under one scheduler on cadences.

**Public API:**
- `improve.OpenMeta(path) (*Meta, error)` — SQLite KV (`tenant_meta`);
  `Get/Set`, `GetInt64/SetInt64`
- `improve.NewScheduler(log, historyCap) *Scheduler`
- `Scheduler.Register(Job, interval)` (interval≤0 = manual-only),
  `.RunDue(ctx)`, `.RunAll(ctx)`, `.Start(ctx, tick)`/`.Stop()`, `.History()`
- `improve.Job` iface: `Name()`, `Run(ctx) (JobResult, error)`
- `improve.NewDistillJob(*distill.Distiller, *Meta, agentID) *DistillJob`
- **Cadence-policy defaults:** `DefaultDistillInterval` (30m),
  `DefaultSchedulerTick` (1m) — used by `tenant serve` so the learning
  loop runs without anyone invoking `tenant distill` by hand.

**Storage:** SQLite `tenant_meta.db`, key `distill_cursor:{agentID}`.
**Invariants:** one failing job never stops others; cursor agent-scoped;
scheduler usable without a goroutine (RunDue/RunAll sync); clean
Start/Stop lifecycle. Soul-nudge job slots in behind `Job` (v1.1 —
TODO). **17 tests.** Now also runs `SkillInductionJob` (T4 — see below).

### 5.11a `internal/memory/skills` — T4 skill library (retrieved recipes)

**Purpose:** the Voyager/Hermes procedural tier. A **skill** is a named,
reusable *recipe* (description + steps) with an intent embedding. The
agent retrieves the top-K relevant enabled skills by query similarity
and they're injected into the system prompt (`agent.Config.Skills`,
`SkillRetriever`), so the agent *follows* them with its existing tools
— skills are knowledge, not separate executors. Storage: own SQLite
(`skills.db`), brute-force cosine (small set), JSON embedding; no decay
(skills earn their place via `success_count`). Lifecycle: `live` /
`proposed` / `tombstoned`, plus an `enabled` flag (toggled via
`/skills` in the TUI).

**Created two ways** (operator's call): **manually** — the agent saves
one it worked out via the `skill_save` tool, or a human via `/skills
add`; and by **induction** — `improve.SkillInductionJob` scans recent
episodes for tool-call sequences that recurred across ≥N *successful*
turns and proposes them (`status=proposed`, disabled) for human review
(`/skills accept`). The review gate keeps the heuristic honest.

**Verified:** store CRUD + cosine ranking + enable/disable hides from
retrieval + proposed→accept→tombstone lifecycle; induction proposes a
repeated sequence + ignores single-tool/failed turns + is idempotent.
8 tests. **Status:** T4 recipes + heuristic induction shipped; macro
skills + success-weighted retrieval + LLM-named induction are v1.1.

### 5.12 `internal/mcpserver` — MCP memory server

**Purpose:** expose the memory layer as an MCP server so any MCP client
(Claude Desktop, Cursor, Zed) can read the soul, browse facts, and
search episodes+facts. Network-effect move + the cleanest way to inspect
what the agent has learned from outside Tenant.

**Public API:**
- `mcpserver.New(Config) (*Server, error)` — Config: `AgentID, SoulDir,
  Episodic, Semantic, Embedder?, AllowWrites, ServerName, ServerVersion, Logger`
- `Server.Serve(ctx) error` — runs over stdin/stdout (how MCP clients
  spawn server subprocesses); blocks until disconnect/ctx
- `Server.Handler() mcp.Handler` — for in-process embedding + tests

**MCP surface:** `initialize`, `ping`, `tools/list`, `tools/call`,
`resources/list`, `resources/read`.
- Tools: `memory_search{query,k?,kind?}` (hybrid keyword+vector when
  Embedder set), `memory_fact_add{fact,confidence?}` (only if AllowWrites
  + Embedder; externally-added facts get `visibility=shared`)
- Resources: `memory://soul/{agent}` (rendered soul, text/markdown),
  `memory://facts/{agent}` (current facts via `semantic.List`, newest-
  confirmed first)

**Storage:** none of its own — reads soul (TOML), episodic + semantic
(SQLite). Added `semantic.Store.List(ListFilter)` as a prerequisite
(facts resource needs "all current facts" — semantic had Search/Get/Count
only).
**Depends on:** `mcp`, `mcp/transport`, `memory/{soul,episodic,semantic}`,
`model` (Embedder iface only).
**Invariants:** every read/search/write scoped to `cfg.AgentID` (no
cross-agent leak — tested); unknown tool = isError content (MCP
convention), unknown resource = protocol error; embed failure degrades
search to keyword-only (not fatal); read-only by default (AllowWrites
gates fact_add); FTS query sanitized (free text → OR'd alnum tokens, no
FTS5 syntax injection). **15 tests** (real mcp.Client over in-memory
pipes exercising the full protocol). **Status:** complete.

### 5.12a `internal/plugins/web` — Web utility plugin (plugin #1)

**Purpose:** stateful headless-Chrome session the agent drives to
read/explore (later: interact/transact) the live web. Real Chrome (via
`chromedp`/CDP) not HTTP fetch — "act as a normal user" needs JS
rendering, SPA/cookie state. Chrome = external runtime; binary stays
pure-Go single-binary, no CGO.

**Public API:** `web.NewSession(Config)` (starts/owns Chrome + 1 tab;
`Headless`, `ChromePath` auto-detect win/mac/linux); `Session.Close`.
`web.NewDispatcher(sess, Policy, shotDir)` → implements
`agent.ToolDispatcher`; `Dispatcher.Tools()` → tool specs for the
registry.

**Blast-radius gate (structural, not bolted-on):** `ActionClass`
{Read, Interact, Transact}. `Policy.gate` keys off the *class* (a new
tool must pick a class — can't silently bypass). Defaults SAFE:
read always; interact per `AllowInteract`; **transact DENIED unless
`Policy.Confirm` explicitly approves** (nil ⇒ deny all — the safe
default; the model cannot change policy). Blocked actions return a
tool-error so the model learns the boundary, not a hard failure.

**Tools (this layer):** `web_navigate` (http/https only — file://,
javascript:, data: rejected), `web_read` (innerText, 8k-char capped),
`web_find` (text contains), `web_links` (≤200 anchors),
`web_screenshot`. Interact (`web_click/fill/select`) + transact
(`web_submit/purchase`, auth) ship next turns behind the same gate.

**Interact layer (web_click/fill/select):** locator = visible text
(preferred — how the model "sees" the page) OR CSS selector; shared
JS resolver tries CSS then text/value/aria/placeholder match. `Locate`
inspects the element with NO side effect so the gate decision is made
on the element the user would actually click. **Dangerous clicks
escalate to ClassTransact**: `classifyClick` matches the target's text
against buy/checkout/pay/delete/sign-in/etc. → routed through the
transact gate (deny-by-default) even though invoked via web_click —
the boundary is about what the button DOES, not which tool called it.
`web_fill` never echoes the value (password/PII → soul "User privacy").

**Tested:** `browser`+`fakeBrowser` → routing, arg validation, URL
scheme blocking, classifyClick, value-redaction, and the full policy
gate fully unit-tested without Chrome (24 tests). Three properties,
all proven: **capability** (LIVE: Gemma+real Chrome — web_click fired,
DOM mutated, web_read confirmed), **deterrence** (LIVE: Gemma refused
"Buy Now" unprompted from the tool desc — 0 calls), **enforcement**
(deterministic unit test: dangerous web_click → gate blocks, Click()
provably never runs regardless of model behavior — the security
guarantee does not depend on the model cooperating). Also fixed:
`MaxToolsPerCall` 5→12 for vllm gen profiles — 5 (a 30B-class default)
silently truncated web_click/fill/select; 26B handles 12 fine. **Status:**
read/explore + interact complete & live-verified; transact = next turn
(gate already enforced for dangerous clicks).

### 5.12b `internal/plugins/sql` — SQL plugin (plugin #2)

**Purpose:** the agent queries databases via tools, behind a blast-
radius gate — the data-loss analog of the web plugin's transact gate.
A local model emitting `DROP TABLE` / `DELETE` w/o WHERE is
irreversible, so statements are CLASSIFIED, never trusted by tool name.

**Public API:** `sql.Open(Config{Driver,DSN})` (v1: `sqlite` via
modernc — already a dep; pgx/Postgres = next, driver layer shaped for
it). `sql.NewDispatcher(store, Policy)` → `agent.ToolDispatcher`;
`Dispatcher.Tools()` → sql_schema / sql_query / sql_exec.

**Classifier (`Classify`)**: strips `--` and `/* */` comments
(quote-aware so a string containing `--` survives), rejects multi-
statement injection (`SELECT 1; DROP …`), leading-keyword → Class.
Unknown verb ⇒ ClassDDL (fail closed). `ClassRead` SELECT/WITH-SELECT/
EXPLAIN; `ClassWrite` INSERT/UPDATE/DELETE; `ClassDDL` everything else
incl. PRAGMA.

**Gate (`Policy`)**: read always; write only if `AllowWrite`; DDL only
if `Confirm` approves (nil ⇒ deny — safe default). **Defense in
depth:** `sql_query` refuses any non-Read class AND only ever calls
`db.Query` (a SELECT can't mutate even if classification were fooled);
`sql_exec` carries the write/DDL gate. Result row+byte caps
(200 / 16KB) with explicit TRUNCATED notice; per-query ctx timeout.
`sql_schema` introspects via PRAGMA on names from sqlite_master — model
never supplies SQL there (injection-free by construction).

**Tested:** classifier (incl. injection: multi-stmt, comment-smuggled
DROP, `--` inside a string literal, trailing `;` OK), gate (read-
always / write-flag / DDL-confirm, with empirical "table still exists"
assertions), sql_query-refuses-non-read, schema, row-cap. 10 tests.
**Live (real Gemma 4 + real SQLite):** schema→query→"Sprocket $42, 8
in stock" (model wrote correct ORDER BY LIMIT); "drop the table"
deterred (model asked for confirmation) + DB verified intact (3 rows).
**Status:** SQLite complete & live-verified; Postgres next (driver
shaped, no live PG here).

### 5.12c `internal/plugins/wiki` — Knowledge plugin (plugin #3)

**Purpose:** the agent answers from a directory of markdown notes.
Built deliberately in the **Karpathy ethos** (nanoGPT/micrograd): one
purpose, no framework magic, plain inspectable files, the simple
correct thing.

**Design:**
- The `.md`/`.txt` files are **canonical and read-only** — this plugin
  has no dangerous mode, so (unlike web/sql) **no safety gate**. One
  capability, done plainly.
- The index is a **derived, disposable** single JSON sidecar you can
  `cat`. No sqlite-vec / FTS vtable / ANN — just `[]float32` + a
  cosine loop. Brute force is fine at personal scale and maximally
  transparent. Delete it → next search rebuilds; nothing lost.
- **Embedder-fingerprinted** (`model/<liveDim>`): a different
  embedder/dim ⇒ full rebuild. Directly bakes in the silent
  dimension-mismatch lesson `tenant doctor` exists to catch — and the
  fingerprint uses the embedder's *probed actual* output dim, not the
  declared one (declared can lie).
- **Incremental**: per-file `(mtime,size)` fingerprint; only changed/
  new files re-embed; deleted files drop. Engineered enough.
- Chunked by markdown heading then rune-windowed (1200/150 overlap);
  each chunk carries its heading so retrieval is contextual and the
  heading is embedded + lexically scanned (strongest single signal).
- `ReadFile` is path-traversal-guarded (Clean + `..`/abs reject +
  Rel-escape check) — model-supplied paths constrained to the root.

**Obsidian link-graph awareness** (`links.go` — one readable file, no
graph library): parses `[[Note]]`, `[[Note|alias]]`, `[[Note#Heading]]`,
`![[embed]]`, `#tags` (incl. nested), and YAML frontmatter `aliases:`/
`tags:` (yaml.v3, flexible list/scalar shapes). Links/tags inside
fenced or inline code are stripped first (not real links). Resolution
is Obsidian-default: by basename (case-insensitive), then path, then
alias; ambiguous ⇒ lexicographically-smallest path; unresolved links
kept as weak signal. **Raw** targets are persisted (not resolved
paths) so a renamed/added note re-links the whole vault without
re-embedding; the forward/back graph is *derived* in-memory from them
(can't drift). Two ways the graph drives RAG, per the project owner's
call — **both**:
  - *Semantic half:* a chunk's embed text is enriched with its
    outgoing link titles + tags, so the vector reflects the note's
    **connections**, not just its prose.
  - *Graph half (1-hop expansion):* after cosine+lexical scoring, the
    top hits (≥ 0.5× top score, ≤ 3) are "anchors"; any note one link
    away (forward **or** backlink) gets a decayed bonus
    (`0.4 × anchorScore`, boost-only so cosine stays primary). A
    strongly-linked note rides along even with mediocre cosine; the
    `Hit.Via` field records which anchor pulled it in (provenance the
    agent and user see).

**Public API:** `wiki.New(root, sidecar, embedID, Embedder)` →
`Reindex` / `Search` / `ReadFile` / `List` / `Links`.
`wiki.NewDispatcher(ix)` → `agent.ToolDispatcher`; `Tools()` →
wiki_search / wiki_read / **wiki_links** (forward+back+tags, the
explicit agentic-traversal half) / wiki_list / wiki_reindex. Search
results render `(via X)` + `#tags`.

**Deterministic grounding (RAG), not blind agentism:** `tenant wiki`
**pre-retrieves** the top notes and injects them into the turn, then
still exposes the wiki_* tools for multi-hop (search again / read a
note in full). This is the same philosophy as the safety gates —
*enforce, don't hope*. Forced by a live finding: gemma-4-26b would
**hallucinate** "I searched your notes, found nothing" emitting *no
`tool_code` block at all* — prompt nagging could not fix a model
confabulating tool use; pre-retrieval makes the answer correct
regardless of whether the model ever calls a tool.

**Tested:** New-rejects-non-dir, reindex+search (heading carried),
incremental (clean reindex embeds 0 / modify re-embeds one / delete
drops), embedder-mismatch **and** index-format-bump force rebuild,
sidecar-is-plain-JSON, dispatch search/read/links/list/reindex,
path-traversal refused, bad args, tool specs; **link-graph:**
frontmatter (inline/block/scalar/singular/malformed-still-strips),
wikilink extraction (alias/heading/embed/subdir, code-stripped),
tag extraction (nested, not `# heading`/`word#frag`/numeric, code-
stripped), resolver (basename/case/alias/path/ambiguous/unresolved),
graph-expansion pulls a zero-overlap linked note in with `Via` set,
wiki_links forward/back/tags/alias + unknown refused. The fake
embedder runs at 256d (at 64d FNV collisions dominate ~7-token notes
and it violates its own "shared words → closer vectors" contract —
verified empirically). 16 tests.
**Live (real Gemma 4 + real Nomic 768d):** earlier — deploy Q cited
[deploy.md]; goroutine-stack Q (exposed the hallucination) correct +
cited; out-of-vault Q → honest "no info," no hallucination. Link-graph
— Obsidian-style vault (`[[wikilinks]]` + frontmatter aliases +
`#tags` + `.obsidian/` auto-skipped): cross-note synthesis cited
[goroutines.md, channels.md, scheduler.md]; and the decisive one — a
10-note vault where the answer note (`frobnitz-protocol.md`, ~zero
lexical/semantic overlap with the question) fell outside plain top-6
yet was pulled into context **solely** by the `[[Frobnitz Protocol]]`
edge from the top hit, producing the exact cited bytes. That answer
is unobtainable without the link graph.
**Status:** complete & live-verified, Obsidian-link-aware.

### 5.12d `internal/plugins/gsuite` — Google Workspace (plugin #4)

**Purpose:** the agent uses Gmail (search/read/send) + Calendar
(list/create). **Two auth paths** (operator's call):
- **Service account + domain-wide delegation** — a hand-rolled RS256
  JWT-bearer assertion (`iss`=client_email, **`sub`=the impersonated
  user**, `scope`, `aud`=token_uri) exchanged for an access token. The
  Workspace-admin route. No `google.golang.org/api` — the flow is
  ~80 lines of stdlib crypto, consistent with Tenant's minimal-deps,
  single-binary ethos (same discipline as hand-rolled toolfmt/MCP).
- **gcloud ADC** — shell out to `gcloud auth application-default
  print-access-token`, cache until expiry. Zero setup, no key files.

Both flow through one `tokenSource` (clock-injected `cachedSource`
refreshing a minute early). Every Google call goes through one
injectable `httpDoer` + JSON transport that surfaces Google's
`{"error":{...}}` envelope.

**Blast-radius gate (`Policy`)** — identical philosophy to sql/web:
Read (search/read/list) always; **Send** (gmail_send,
calendar_create) *leaves the building* (mails people / notifies
attendees) so it is denied unless `AllowSend`, or a per-action
`Confirm` explicitly approves it (nil ⇒ deny — safe default). The
model cannot change the policy. **Least privilege bonus:** when send
is off, the requested OAuth scopes themselves downgrade to read-only
(`gmail.readonly`+`calendar.readonly`), so the token literally cannot
send even if the gate were bypassed.

**Public API:** `gsuite.Open(Config{Auth,SAJSON,Subject,AllowSend,…})`
→ `Service{Gmail,Calendar}`. `gsuite.NewDispatcher(svc, Policy)` →
`agent.ToolDispatcher`; `Tools()` → gmail_search / gmail_read /
gmail_send / calendar_list / calendar_create.

**Verified — honest scope:** unlike web/sql/wiki there is **no live
Google in this environment and operator credentials were deliberately
not solicited** (no pasting service-account keys; no uninvited access
to a real inbox). So the live leg is **operator-run**, and that is
stated, not faked. What *is* deterministically proven (httptest
servers speak the exact Gmail/Calendar wire JSON): the JWT-bearer
assertion is decoded and its **signature verified against a real
generated RSA key** + alg/iss/**sub**/scope/aud/exp asserted; token
cache refreshes at expiry (fake clock); gcloud path invokes the right
CLI and caches; PKCS1/PKCS8 key parse; Gmail search/read (base64url
MIME walk)/send (RFC 5322 `raw` decoded & asserted); Calendar
list(all-day+timed)/create(body+attendees); **gate: send/create
blocked read-only with "nothing left the building" assertions,
allowed via flag, allowed per-action via Confirm**; scope downgrade;
bad-args; auth-path validation. 10 tests.
**Status:** complete; deterministic + wire-contract verified; live
Google leg operator-run (commands in the CLI section).

### 5.12e `internal/plugins/x` — X / Twitter (plugin #5)

**Purpose:** the agent reads + posts on X. A **native-Go port of the
`xurl` auth+request layer** (the piece Hermes wraps for X access) —
no shelling to the Rust binary, no SDK; the X API v2 is plain REST and
OAuth2 PKCE is ~100 lines of stdlib (same minimal-deps single-binary
discipline as gsuite).

**Two auth paths** (operator's call, mirrors gsuite's shape):
- **App-only Bearer** — read-only public data, zero flow (token/env).
- **OAuth2 Authorization-Code + PKCE** — user context (post/reply/
  delete *as the user*). `S256` challenge = base64url(sha256(verifier));
  one-time browser consent via a localhost callback (`tenant x
  --login`); refresh token cached at `<data>/x-token.json` (0600,
  plain JSON — same transparency ethos as the wiki sidecar). **X
  rotates refresh tokens**, so every refresh re-persists. State param
  checked (CSRF). Reads prefer the bearer; if only a user token
  exists it reads with that; a bearer **cannot** post (enforced in
  the API layer, not just the gate).

**Blast-radius gate (`Policy`)** — identical philosophy to
sql/web/gsuite: Read (search/get_tweet/get_user/timeline) always;
**Post** (x_post, x_delete) is public + effectively irreversible, so
denied unless `AllowPost` or a per-action `Confirm` (nil ⇒ deny).
**Least privilege:** with posting off, the PKCE login requests
read-only scopes (`tweet.read users.read offline.access`) — the
resulting token literally cannot tweet even if the gate were bypassed.

**Public API:** `x.Open(Config{Bearer,TokenPath,AllowPost,…})` →
`Service`; `x.Login(ctx, LoginConfig)` (PKCE consent + persist);
`x.NewDispatcher(svc, Policy)` → `agent.ToolDispatcher`; `Tools()` →
x_search / x_get_tweet / x_get_user / x_user_timeline / x_post /
x_delete.

**Verified — honest scope (same stance as gsuite):** no live X in
this environment and **API keys were deliberately not solicited; a
real account is not posted to uninvited**. Live leg is **operator-run
and labelled**, not faked. Deterministically proven (httptest servers
speak the exact X API v2 wire JSON): **PKCE `S256` math** (challenge
= b64url(sha256(verifier)), verifier length in RFC-7636 range), auth
URL params, code+refresh grants over an httptest token endpoint,
**refresh-token rotation persisted to disk + reloaded**, cache
refresh-at-expiry (fake clock); bearer source; client
search/get_tweet/get_user/timeline (incl. `includes.users` handle
mapping)/post(body shape)/reply(`in_reply_to_tweet_id`)/delete
(DELETE path) + error-envelope parse; **gate: post/delete blocked
read-only with "nothing left the building" assertions, allowed via
flag, allowed per-action via Confirm**; bearer-cannot-post enforced
below the gate; Open credential selection; bad args. 14 tests.
**Status:** complete; deterministic + wire-contract verified; live X
leg operator-run (`tenant x --login` then `tenant x "…"`).

### 5.12f `internal/plugins/imessage` — iMessage via BlueBubbles (plugin #6)

**Purpose:** the agent reads + sends iMessages. There is no official
Apple API, so this is a **native-Go REST client for a BlueBubbles
server** — the maintained open-source bridge that runs on a Mac with
Messages and owns the chat.db / AppleScript / Private-API plumbing.
Tenant treats it as a networked dependency (like vLLM/Ollama/Chrome);
thin client, no SDK.

**Auth:** server URL + password (no OAuth — simpler than gsuite/x).
The password is added as a query param on every call; responses are
the BlueBubbles `{status,message,data}` envelope (errors surfaced
from `message`).

**Scope v1:** read (list chats / read a chat's recent messages /
search by text via the message-query `where text LIKE`) + send (send
to an existing chat; start a new chat with a phone/email). Sending
uses BlueBubbles' **apple-script** method by default (works on any
install) or **private-api** via flag. apple-script sends carry a
client-generated `tempGuid` for dedupe.

**Blast-radius gate (`Policy`)** — identical philosophy to
sql/web/gsuite/x: Read always; **Send** (imessage_send,
imessage_new_chat) messages a real person, so denied unless
`AllowSend` or a per-action `Confirm` (nil ⇒ deny).

**Public API:** `imessage.Open(Config{URL,Password,PrivateAPI,…})` →
`Service` (ListChats/ChatMessages/SearchMessages/SendText/NewChat/
Ping); `imessage.NewDispatcher(svc, Policy)` → `agent.ToolDispatcher`;
`Tools()` → imessage_list_chats / imessage_read_chat / imessage_search
/ imessage_send / imessage_new_chat.

**Verified — honest scope (same stance as gsuite/x):** no live
BlueBubbles/Mac in this env and **no server password solicited; no
one messaged**. Live leg **operator-run and labelled**, not faked.
Deterministically proven (httptest serves the exact BlueBubbles
envelope+endpoints): transport adds the password param + decodes the
envelope + surfaces error messages (401 "Invalid password!"); client
list-chats (display-name fallback to participant), read-chat
(`isFromMe`→"me", ms-epoch→time), search, **send (chatGuid + message
+ `tempGuid` + method shape)**, new-chat (addresses); **private-api
method selection**; **gate: send/new_chat blocked read-only with
"nothing was sent" assertions, allowed via flag, allowed per-action
via Confirm**; Open validation; bad args; ping. 8 tests.
**Status:** complete; deterministic + wire-contract verified; live
iMessage leg operator-run (point `--bb-url`/`--bb-password` at a
running BlueBubbles server).

### 5.12g `internal/plugins/osys` — Operating system (plugin #7)

**Purpose:** inspect the machine and (gated) run shell commands —
cross-platform (Windows/Linux/macOS). The **highest-blast-radius
plugin**: an agent with a shell can destroy a host. Named `osys` to
avoid clashing with stdlib `os`.

**Tools:** `os_sysinfo` (os/arch/host/cpu/user/home/cwd), `os_read_file`
(capped), `os_list_dir`, `os_processes` (runs a FIXED `ps aux` /
`tasklist` — no model input, benign), and `os_exec` (gated). Shell is
auto-selected: PowerShell (`-NoProfile -NonInteractive -Command`) on
Windows, `sh -c` on Unix; exec is ctx-timeout-bounded (60s) and output
is capped.

**Gate (`Policy`)** — operator's chosen model: exec is **off** unless
`AllowExec`; even then a **danger classifier** (`Classify`) hard-blocks
catastrophic commands (`rm -rf`, `Remove-Item -Recurse`, `mkfs`,
`dd of=`, disk format, `shutdown`/`Stop-Computer`, fork bombs,
`curl|sh`, `find / -delete`, …) unless `Confirm` approves. With
`AllowExec` off, a per-command `Confirm` can still allow individual
runs. The classifier scans the whole string so chaining
(`ls && rm -rf /`) can't hide the dangerous part.

**HONEST SECURITY LIMIT (stated, not glossed):** the classifier is a
guardrail against accidents + obvious destructive cases, **not a
sandbox**. An obfuscated/indirected command (base64, odd flag order)
can slip past it when `AllowExec` is on without `Confirm`. Real
containment = keep exec off unless needed, run Tenant as a
least-privilege user, prefer a container/VM. Also: `os_read_file` reads
any path the process can — enabling this plugin is consent to
filesystem read (exfiltration risk when combined with the send-capable
plugins). Documented so the operator opts in knowingly.

**Public API:** `osys.Open(Config{ExecTimeout})` → `Service`
(`SysInfo`/`ReadFile`/`ListDir`/`Processes`/`Exec`);
`osys.NewDispatcher(svc, Policy{AllowExec,Confirm})`; `osys.Classify`.

**Verified:** classifier (12 catastrophic patterns flagged incl.
chained; 7 benign pass — `rm file.txt` without -r/-f is *not* flagged);
**gate with "nothing ran" assertions** (disabled → blocked + runner
never called; enabled+benign → runs; enabled+dangerous+no-confirm →
blocked + not run; dangerous+confirm → runs; per-command confirm
without the flag → runs); real read/list/sysinfo over a temp dir; tool
specs. Exec runs through an injected runner in tests (no real
processes). 5 test funcs.
**Status:** complete; deterministic-verified. CLI: `tenant os` and the
TUI `--os`/`--os-allow-exec` flags.

### 5.13 `cmd/tenant` — CLI (main.go + wiring.go + commands.go)

A real subcommand CLI (stdlib `flag`, no cobra). Runs fully offline on
the `echo` backend (default); `--backend vllm` for production.

**Subcommands:**
- `tenant version`
- `tenant chat` — interactive agent loop; one stdin line = one
  `agent.Turn`; conversation to stdout, logs to stderr, memory persists
- `tenant distill` — one distillation pass via `improve.DistillJob`
  (real cursor persistence in `tenant_meta.db`)
- `tenant serve [--distill-every 30m] [--tick 1m]` — the long-running
  home for background self-improvement: registers DistillJob on the
  scheduler and ticks it on a cadence, so learning happens without
  manual `tenant distill`. SIGINT/SIGTERM shuts down cleanly (drains
  the in-flight job, prints run history). Scoped to the improve
  scheduler; a full always-on agent server is still future.
- `tenant os "<task>" [--allow-exec]` — agent inspects the machine
  (sysinfo/read/list/processes) + runs shell commands (gated; off
  unless `--allow-exec`, destructive commands hard-denied). §5.12g.
- `tenant tui [--wiki-dir D] [--sql-db F] [--web] [--gsuite] [--x]
  [--imessage] [--os [--os-allow-exec]] …` — the full-screen terminal
  experience (§5.14):
  streaming chat + live activity feed + status bar. Enabled plugins are
  merged via a tool multiplexer so the agent can use them all in one
  conversation; tool calls/results stream into the feed. Background
  self-improvement (distillation) runs in-process by default
  (`--distill-every 5m`, `--self-improve=false` to disable) and its job
  runs stream into the feed. Logs route to `<data>/tui.log` (stderr
  would corrupt the alt-screen).
- `tenant memory search <query> [--flags]` — CLI hybrid search over
  episodes+facts (query tokens parsed before flags — Go `flag` stops at
  first non-flag, so the CLI splits leading non-flag args as the query)
- `tenant mcp-memory [--allow-writes]` — runs `mcpserver.Server.Serve`
  over the process's stdio (how an MCP client launches it). **stdout is
  the JSON-RPC channel — all logs go to stderr.**
- `tenant mcp-selftest` — spawns `tenant mcp-memory` as a real OS
  subprocess via `transport.NewStdioProcess`, connects as an `mcp.Client`,
  runs initialize/tools/resources/call. Proves the stdio server works as
  a real subprocess (how Claude Desktop spawns it), not just in pipes.
- `tenant web "<task>" [--show] [--allow-interact]` — agent drives
  real Chrome (read/explore+interact). Read always; interact per flag;
  dangerous clicks transact-gated. Live-verified vs real Gemma 4.
- `tenant sql "<question>" --db FILE [--allow-write]` — agent queries a
  SQLite DB. Read always; writes per flag; DDL hard-denied (Confirm
  nil). Live-verified vs real Gemma 4.
- `tenant wiki "<question>" --dir DIR` — agent answers from a markdown
  / Obsidian knowledge base (read-only, no gate). Obsidian-link-aware:
  `[[wikilinks]]`/`#tags`/frontmatter feed both embed enrichment and
  1-hop graph-expansion. Pre-retrieves top notes (RAG grounding) then
  exposes wiki_* tools incl. wiki_links for multi-hop. Per-vault hashed
  sidecar under `<data>/wiki/`. Live-verified vs real Gemma 4 + Nomic.
- `tenant gsuite "<task>" [--auth gcloud|sa] [--sa-json FILE --subject
  USER] [--allow-send]` — agent uses Gmail + Calendar. Read-only by
  default (and OAuth scopes downgrade to read-only too); `--allow-send`
  enables gmail_send/calendar_create. Operator live-run, e.g.:
  `gcloud auth application-default login --scopes=https://www.googleapis.com/auth/gmail.readonly,https://www.googleapis.com/auth/calendar.readonly`
  then `tenant gsuite "what's on my calendar this week" --backend vllm
  --vllm-endpoint … --vllm-model gemma-4-26b`. (gcloud not installed in
  this env; not auto-run against a real inbox — privacy.)
- `tenant x --login --client-id ID [--allow-post]` then `tenant x
  "<task>" [--bearer TOK | $X_BEARER_TOKEN] [--allow-post]` — agent
  uses X/Twitter. Reads via app bearer; `--login` runs the OAuth2
  PKCE consent (localhost callback, refresh token → `<data>/
  x-token.json`). Read-only by default (PKCE scopes downgrade too);
  `--allow-post` enables x_post/x_delete. Operator live-run (no X
  creds in this env; real account not posted to uninvited).
- `tenant imessage "<task>" [--bb-url URL | $BLUEBUBBLES_URL]
  [--bb-password PW | $BLUEBUBBLES_PASSWORD] [--private-api]
  [--allow-send]` — agent uses iMessage via a BlueBubbles server
  (URL+password, no OAuth). Read-only by default; `--allow-send`
  enables imessage_send/imessage_new_chat; sends via apple-script
  unless `--private-api`. Operator live-run (no BlueBubbles/Mac in
  this env; no one messaged).
- `TENANT_LOG=debug|info` (env) raises the log level on any subcommand
  — surfaces agent/backend internals incl. the raw model completion
  when no tool call is parsed (how the wiki hallucination was caught).
- `tenant tool-test [-q QUERY]` — hardening harness: registers an `add`
  tool + a dispatcher, runs a turn that requires it, prints the full
  ToolTrace. Verifies the whole tool path (arg normalization → validate
  → dispatch → result feedback → synthesis) against a real model.
  Verdict-asserting: fails if the tool never fires.
- `tenant doctor [--deep] [--fix] [--json]` — diagnose/repair when
  things break (hermes/openclaw pattern). 11 checks, each OK/WARN/FAIL
  + actionable fix: dirs, router/roles, gen endpoint reachable + model
  served, /tokenize, embedder reachable, **embedding-dim consistency**
  (the silent cosine-killer: stored 128d vs live 768d → retrieval
  silently broken — instant FAIL now), sqlite store integrity, soul
  TOML parse, stale distill cursor. `--deep`: live tool-call probe +
  in-process MCP self-loop. `--fix`: safe reversible repairs only
  (mkdir dirs, reset stale cursor — old value saved to `:prev`).
  Non-zero exit on any FAIL (CI-usable). Verified: 11/11 OK on real
  DGX+Ollama; correctly FAILs dead endpoint / dim mismatch, WARNs
  wrong model name, FIXES stale cursor.

**Common flags:** `--backend echo|vllm`, `--agent ID`, `--data DIR`,
`--config DIR`, `--vllm-endpoint/-model/-tool-format` (gen roles),
`--embed-endpoint/-model/-dim` (embedder role — any OpenAI-compat
`/v1/embeddings`: real vLLM OR Ollama). Wiring (`wiring.go`):
`buildRouter` — echo → in-memory echo profiles; vllm + endpoints →
**heterogeneous role routing**: gen roles → vLLM, embedder → vLLM/
Ollama if `--embed-endpoint` set else echo stand-in. Both backend
factories registered; Router resolves each role to its profile's
backend (the eng-review multi-endpoint design, proven on real HW).
**Status:** functional end-to-end. Verified: chat → episodes persist →
search ranks by relevance → distill advances cursor → mcp-selftest exits 0.
`tenant serve` now provides a long-running daemon for **background
self-improvement** (distillation on a cadence; clean signal shutdown,
verified). A full always-on *agent* server (resident turn-serving +
real-time plugin sockets like BlueBubbles) is still future.

---

### 5.14 `internal/tui` — Terminal experience (Bubble Tea)

**Purpose:** the production way to use Tenant — chat with the agent
and *watch what's happening*. One screen: a streaming chat transcript
(left), a live **activity feed** (right: memory assembly + context-
budget %, token streaming, tool calls + results, validation/errors),
and a **status bar** (state/backend/model/agent · ctx % · last tool).
Inspired by the OpenClaw-style agent TUI.

**Dependency note (deliberate):** this is the one place Tenant departs
from its strict minimal-deps ethos — it pulls in **Bubble Tea**
(`charmbracelet/bubbletea` + `lipgloss` + `bubbles`) and their pure-Go
transitive deps. Signed off by the owner: hand-rolling a TUI (resize,
scrollback, input, ANSI) would be a large permanent maintenance burden,
and Bubble Tea is the industry-standard Go choice. Still pure Go, no
CGO — the single-binary story holds.

**Design:** the agent is constructed with `Stream: true` + an
`Observer` that pushes `agent.Event`s into a buffered channel; the
Bubble Tea model drains it via a re-issued `listen` command and renders
live. A turn runs off the UI goroutine (returns `turnDoneMsg`). Token
events append to the in-flight assistant bubble; everything else lands
in the feed. `internal/tui` depends only on `internal/agent` (the
command wires router/stores/dispatcher), so the UI stays decoupled.

**Scope:** memory-backed streaming chat + the live feed, **plus
optional tool plugins** via a `toolMux` (`cmd/tenant/toolmux.go`) that
merges any enabled plugins' specs and routes `Dispatch` by tool name
(distinct prefixes ⇒ no collision; first-registrant-wins on dup).
Enable per-plugin with flags — `--wiki-dir`, `--sql-db`, `--web`,
`--gsuite`, `--x`, `--imessage`, `--os` (+ each plugin's config/allow
flags). Dangerous actions (write/send/post/exec) stay off unless the
operator opts in. Tool calls + results then stream into the feed live.

**Slash commands + runtime tool control (no restart):** input starting
with `/` is a command, not a message. `/tools` lists every tool and its
on/off state grouped by plugin; `/enable <tool|plugin>` and `/disable
<tool|plugin>` toggle a single tool or a whole plugin **live**.
Mechanism: the `toolMux` (`cmd/tenant/toolmux.go`) is the agent's
ToolRegistry *and* Dispatcher, with a per-tool `enabled` flag behind a
RWMutex. Disabling drops the tool from `Search` (so it leaves the
assembled prompt entirely — zero context + compute) and from `Get` (so
it's uncallable + fails validation if attempted); re-enabling restores
it next turn. The TUI toggles via a small `tui.ToolControl` interface
the mux implements. This is the advantage over Hermes/OpenClaw: prune
unused/unconfigured tools mid-session without bouncing the gateway.
**Every plugin is always visible** in `/tools`: configured ones load
real; unconfigured ones register as **disabled stubs** that advertise
their real specs and return a "needs setup" status if enabled+called
(the framework runs even unauthenticated — like a 401). **`/skills`**
manages the T4 library (list / add / enable / disable / forget /
accept). **`/memory`** inspects + lightly edits the memory tiers:
`stats`, `search <q>`, `facts [q]`, `recent [n]`, `forget fact:<id>|
ep:<id>` (tombstone), `soul` (view), `soul import <path.md>` (replace
operating instructions from a markdown file — the soul is view-only in
the TUI, edited by import or in the OS), `distill` (run a T2→T3 pass
now). Also `/help`, `/quit`.

**Copyable chat:** `Ctrl-Y` yanks the full transcript to the clipboard
(`atotto/clipboard`) and writes it to `<data>/transcript.txt` as a
robust fallback — for troubleshooting/sharing responses. Alt-screen
TUIs make native terminal selection unreliable, so this is the copy path.

**First-run installer + health banner (`bootstrap.go`):** on launch
the TUI runs an idempotent setup — `expandPath` resolves a leading `~`
(Go/PowerShell don't), then `ensureSetup` detects-and-creates only
what's missing (data/config dirs, an editable default **soul** file,
the wiki vault + a starter `welcome.md`, the sql db's parent) and
reports each "found/created" line into the feed. `healthCheck` then
probes the generation + embeddings endpoints (2s timeout) and seeds
the feed with their status ("generation OK …", "EMBEDDINGS UNREACHABLE
… degraded"). So a first run self-installs and a misconfigured
endpoint is obvious in-UI, not a cryptic crash. (This is also why a
down embedder no longer kills launch — see §5.12c wiki resilience.)

**Background self-improvement in the feed:** the TUI also runs the
distillation scheduler in-process (default on, `--distill-every 5m` —
trial-friendly cadence). Each job run streams into the feed via the
scheduler's `OnRun` hook → a `System` line channel the model drains
(`⚙ self-improve: distill — …`). Shares the live agent's episodic/
semantic stores (SQLite WAL ⇒ safe concurrent access); drained
cleanly on quit. `--self-improve=false` to disable.

**Verified:** the agent event/streaming layer is unit-tested
deterministically (token emission, streaming tool-call handling); the
TUI launches + renders (alt-screen smoke). Interactive rendering isn't
auto-tested (no TTY in CI) — honest.

## 6. Cross-cutting invariants (apply everywhere)

1. **Pure Go, no CGO.** Single-binary deploy is non-negotiable. SQLite =
   `modernc.org/sqlite`. No sqlite-vec (CGO) — brute-force cosine instead.
2. **Typed errors + `errors.Is`/`As`.** Every package exports sentinels;
   wrap with `%w`.
3. **Agent + visibility scoping.** Every retrieval filters by `agent_id`
   and `visibility IN (private,shared[,public])`. No cross-agent leak.
4. **Append-only audit floor.** T5 is never rewritten. "Forget" =
   tombstone in T2/T3, never delete from archive.
5. **Loud self-improvement.** Soul edits are proposed, never auto-applied.
6. **Budget from `WritableBudget()`**, never raw `ContextLength`. Token
   counts come from `LLM.TokenCount` (vLLM `/tokenize`), never estimated
   in the hot path.
7. **Fail-fast on sizing.** Assembler errors rather than emit an
   oversized prompt.
8. **Storage co-location pattern.** New SQLite-backed state = its own
   `Open(path)` + `CREATE TABLE IF NOT EXISTS`; safe to share a file
   with sibling tables.

---

## 7. What is NOT built (deferred — see TODOS.md for full context)

| Item | Tier/area | When |
|---|---|---|
| T4 deeper: macro skills, success-weighted retrieval, LLM-named induction | memory/skills + improve | v1.1 (T4 recipes + heuristic induction shipped) |
| Soul-nudge job | improve (`SoulNudgeJob`) | v1.1 |
| Supersede-on-contradiction + semantic dedup | distill (LLM judge) | v1.1 |
| Ollama / llama.cpp / OpenAI-compat backends | model/backend | post-v1 |
| Working-set hierarchical compaction | memory/working | v1.1 |
| Persistent embedding cache | model | v1.1 |
| ANN vector index (sqlite-vec/pgvector) | episodic/semantic | when >100K rows |
| BFCL + internal eval suite | testing | v1.1 (gates tuning) |
| Per-profile budget tuning (Soul/System) | model/profiles | v1.1 (needs evals) |
| GEPA/DSPy prompt evolution | improve | not on roadmap |
| Web plugin: transact (submit/purchase/auth) | internal/plugins/web | gated + dangerous-click enforced; needs Confirm-policy decision before explicit purchase tools |
| SQL plugin: Postgres (pgx) | internal/plugins/sql | next (driver shaped; SQLite done) |
| GSuite: live Google verification | internal/plugins/gsuite | operator-run (no creds in this env; deterministic + wire-contract proven) |
| GSuite: Drive, Contacts, threads/attachments | internal/plugins/gsuite | v1.1 (Gmail+Calendar shipped) |
| X: live verification | internal/plugins/x | operator-run (no X creds in env; deterministic + PKCE + wire-contract proven) |
| X: likes/retweets/follow/DMs, media upload | internal/plugins/x | v1.1 (read+post shipped) |
| iMessage: live verification | internal/plugins/imessage | operator-run (no BlueBubbles/Mac in env; deterministic + wire-contract proven) |
| iMessage: real-time socket, tapbacks, attachments | internal/plugins/imessage | v1.1 (read+send shipped; needs `tenant serve` daemon for socket) |

**All six spec'd plugins shipped** (web, sql, wiki, gsuite, x, imessage) **plus the OS plugin (#7, `osys` — cross-platform sysinfo/read/exec, gated by a danger classifier; §5.12g).** **Recently completed (was deferred):** MCP memory server → `internal/mcpserver` (§5.12); Wiki plugin (#3) → `internal/plugins/wiki` (§5.12c, live-verified); GSuite plugin (#4, Gmail+Calendar, dual auth) → `internal/plugins/gsuite` (§5.12d); X plugin (#5, read+post, bearer + OAuth2-PKCE, native xurl port) → `internal/plugins/x` (§5.12e); iMessage plugin (#6, read+send via BlueBubbles REST) → `internal/plugins/imessage` (§5.12f); **the three v1 MCP/runtime gaps — `notifications/cancelled` (bidirectional) + `notifications/progress` in `internal/mcp` (§5.1), and the distillation cadence policy + `tenant serve` daemon in `internal/improve`/CLI (§5.11, §5.13).**

---

## 8. Commands

```
go build -o tenant ./cmd/tenant       # single binary, pure Go
go test ./...                          # 218 tests, ~13s
go vet ./...                           # clean

# Run it (echo backend = offline, deterministic):
printf 'I prefer Go\nwhat do I prefer?\n' | ./tenant chat
./tenant distill
./tenant memory search "what do I prefer"
./tenant mcp-selftest                  # spawns mcp-memory subprocess, drives MCP
./tenant mcp-memory                    # serve memory over stdio (for Claude Desktop etc.)
```

**Wiring an MCP client (e.g. Claude Desktop):** point it at
`tenant mcp-memory --agent main` (stdio). It will see the `memory_search`
tool and `memory://soul/{agent}` + `memory://facts/{agent}` resources.

Test doubles: `model/testllm.Fake` (programmable LLM+Embedder, records
calls). SQLite tests use `:memory:` or `t.TempDir()`. No network in tests.
