package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tenant/internal/memory/archive"
	"tenant/internal/memory/assemble"
	"tenant/internal/memory/compress"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/soul"
	"tenant/internal/memory/userprofile"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/model/toolfmt"
)

// ErrLoopCeilingReached is set in TurnResult.Error when the agent
// hit Profile.PlanLoopCeiling without the model emitting a final
// response. The runtime forces a final synthesis call (no tools) and
// returns whatever the model produces.
var ErrLoopCeilingReached = errors.New("agent: plan loop ceiling reached")

// ErrTooManyValidationFailures is set when 2 consecutive tool-call
// validation passes failed. The runtime stops the loop rather than
// trying to coax a small model into emitting valid JSON forever.
var ErrTooManyValidationFailures = errors.New("agent: too many consecutive tool-call validation failures")

// Config wires the agent. All fields except SystemPrompt are required
// for a useful turn — passing nil where required returns an error
// from Turn rather than crashing.
type Config struct {
	AgentID   string
	SessionID string
	Router    *model.Router
	Soul      *soul.Soul
	// SoulLive, if set, supersedes Soul: the agent re-reads it every turn
	// (Load is concurrency-safe), so an operator soul edit applies next turn
	// without a torn read. Optional — nil keeps the static Soul above.
	SoulLive   *soul.Live
	Working    *working.Set
	Archive    *archive.Writer
	Episodic   *episodic.Store
	Semantic   *semantic.Store
	Tools      ToolRegistry
	Dispatcher ToolDispatcher
	Assembler  *assemble.Assembler // optional; if nil, agent builds one from Router

	Logger       *slog.Logger
	PlannerRole  model.Role // default "planner"
	EmbedderRole model.Role // default "embedder"
	SystemPrompt string     // task-level structural rules

	// Observer, if set, receives live Events during a turn (for a TUI
	// feed). Optional — nil means no events emitted.
	Observer func(Event)
	// Stream uses the planner's GenerateStream path so text arrives as
	// tokens (emitted as EventToken). Opt-in: existing callers keep the
	// proven buffered Generate path. Tool-call extraction is identical
	// either way (the backend reassembles tool calls in both paths).
	Stream bool

	// Skills, if set, supplies T4 reusable-procedure recipes retrieved
	// by query similarity and injected into the system prompt. Optional.
	Skills SkillRetriever

	// Compactor, if set, compresses the working set when the assembler
	// reports the budget is filling up (post-turn). Optional — nil keeps
	// the old behavior (oldest turns truncated by the assembler).
	Compactor Compactor

	// UserProfile, if set, is the synthesized always-on model of the user,
	// rendered into the system prompt every turn. The agent reads it fresh
	// each turn (Render is nil-safe), so a background synthesizer can
	// update the SAME pointer in place and the next turn reflects it.
	UserProfile *userprofile.Profile

	// RenderProjectSME, if set, returns the current per-project SME doc
	// (Phase 3 of docs/memory-sme-plan.md) to inject into the system reserve
	// every turn, alongside the user profile. Called fresh each turn and
	// backed by an in-memory cache the background ReflectionJob refreshes, so
	// it's cheap (no per-turn DB read). nil ⇒ no SME injected (additive).
	RenderProjectSME func() string

	// EpisodeVisibility overrides the default `private` visibility used
	// when this agent's turns are persisted to the episodic store.
	// Sub-agents spawned by an orchestrator (see cmd/tenant/team.go) set
	// this to `shared` so the parent orchestrator's retrieval can surface
	// their work via the assembler's agent-glob filter (TEN-45). Empty
	// (default) → falls through to `VisibilityPrivate`.
	EpisodeVisibility string

	// LazyToolLoad enables on-demand tool loading (TEN-228): the per-turn tool
	// array carries only the ranked working set + a `load_tool` meta-tool, while
	// a cheap name+one-line-description CATALOG of every other enabled tool goes
	// in the system prompt. The model calls load_tool(name) to pull a catalog
	// tool's full (minified) schema into the next loop iteration. Keeps tool
	// tokens flat as the catalog grows AND preserves access to unranked tools
	// (closes ranking's "needed a hidden tool" risk). Off by default — additive.
	LazyToolLoad bool
}

// Compactor compresses a working-set message slice — summarizing old
// turns into a handoff and keeping the recent tail. Returns the new slice
// and whether anything changed. Implemented by internal/memory/compress.
type Compactor interface {
	Compact(ctx context.Context, msgs []working.Message) ([]working.Message, bool, error)
}

// SkillCard is one retrieved T4 skill (a reusable recipe).
type SkillCard struct {
	Name        string
	Description string
	Recipe      string
}

// SkillRetriever returns the top-K skills most relevant to a query
// embedding. Implemented by the skills store (kept as an interface so
// the agent doesn't depend on the skills package directly).
type SkillRetriever interface {
	RetrieveSkills(ctx context.Context, embedding []float32, k int) ([]SkillCard, error)
}

// Agent runs the bounded ReAct loop.
type Agent struct {
	cfg                       Config
	log                       *slog.Logger
	plannerRole, embedderRole model.Role

	// Interjection support: a running turn can accept a mid-flight user
	// message, fold it into the working set, and let the planner decide to
	// address-and-resume or pivot. injectMu guards both fields.
	injectMu   sync.Mutex
	injections []string
	iterCancel context.CancelFunc // cancels the in-flight planner call; nil between calls

	// router is swappable at runtime (the /model live-switch). Turn reads it
	// once at the top, so a swap applies to the NEXT turn — no mid-turn races.
	router atomic.Pointer[model.Router]

	// compaction is the post-turn compaction trigger's hysteresis state
	// (TEN-102). Mutated only from Turn's post-turn defer (turns are serialized
	// per agent).
	compaction *hysteresis
}

// Router returns the active router. SetRouter swaps it (e.g. when the operator
// changes the primary model on the fly); the change takes effect next turn.
func (a *Agent) Router() *model.Router { return a.router.Load() }

// SetRouter atomically swaps the active router. No-op on nil.
func (a *Agent) SetRouter(r *model.Router) {
	if r != nil {
		a.router.Store(r)
	}
}

// soul returns the soul to render this turn. When a live holder is wired
// it wins (re-read each turn so operator edits apply without a restart or
// a torn read); otherwise the static Config.Soul is used.
func (a *Agent) soul() *soul.Soul {
	if a.cfg.SoulLive != nil {
		return a.cfg.SoulLive.Load()
	}
	return a.cfg.Soul
}

// projectSME renders the current per-project SME doc for system-reserve
// injection, or "" if none is configured (Phase 3). nil-safe.
func (a *Agent) projectSME() string {
	if a.cfg.RenderProjectSME == nil {
		return ""
	}
	return a.cfg.RenderProjectSME()
}

// Interject queues a user message to be folded into the currently running
// turn and interrupts the in-flight planner call so it's picked up right away.
// The agent addresses the message alongside its in-progress task and — unless
// the new instruction overrides it — resumes; the model decides, given the
// full in-progress context. No-op when no turn is running (the message is
// just queued and folded at the next iteration boundary).
func (a *Agent) Interject(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	a.injectMu.Lock()
	a.injections = append(a.injections, msg)
	cancel := a.iterCancel
	a.injectMu.Unlock()
	if cancel != nil {
		cancel() // bail the current planner call so the loop re-plans now
	}
}

func (a *Agent) setIterCancel(c context.CancelFunc) {
	a.injectMu.Lock()
	a.iterCancel = c
	a.injectMu.Unlock()
}

func (a *Agent) takeInjections() []string {
	a.injectMu.Lock()
	defer a.injectMu.Unlock()
	out := a.injections
	a.injections = nil
	return out
}

func (a *Agent) hasInjections() bool {
	a.injectMu.Lock()
	defer a.injectMu.Unlock()
	return len(a.injections) > 0
}

// New constructs an Agent. Returns an error if any required Config
// field is unset (cleaner than panicking deep inside Turn).
func New(cfg Config) (*Agent, error) {
	if cfg.AgentID == "" {
		return nil, errors.New("agent: AgentID required")
	}
	if cfg.Router == nil {
		return nil, errors.New("agent: Router required")
	}
	if cfg.Working == nil {
		return nil, errors.New("agent: Working set required")
	}
	if cfg.Tools == nil {
		return nil, errors.New("agent: Tools registry required")
	}
	if cfg.Dispatcher == nil {
		return nil, errors.New("agent: Dispatcher required")
	}
	if cfg.SessionID == "" {
		cfg.SessionID = newSessionID()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	pr := cfg.PlannerRole
	if pr == "" {
		pr = model.RolePlanner
	}
	er := cfg.EmbedderRole
	if er == "" {
		er = model.RoleEmbedder
	}
	a := &Agent{cfg: cfg, log: log, plannerRole: pr, embedderRole: er, compaction: newCompactionHysteresis()}
	a.router.Store(cfg.Router)
	return a, nil
}

// TurnRequest is one user-facing call into the agent.
type TurnRequest struct {
	UserQuery string
	// LoopCeiling overrides the profile's PlanLoopCeiling for THIS turn only.
	// 0 = inherit the profile default (normal turns; back-compat). >0 = cap
	// planner↔tool iterations at this value. <0 = unlimited (omit the per-turn
	// cap). Used by /goal runs so a long autonomous loop can iterate freely
	// without raising the global ceiling every normal turn shares (TEN-216).
	LoopCeiling int
}

// TurnResult summarizes what happened during the turn.
type TurnResult struct {
	Response   string
	ToolTrace  []ToolCallResult
	Iterations int
	Tokens     int                     // total prompt tokens used on the LAST iteration
	Reports    []assemble.BudgetReport // per-iteration budget reports
	Truncated  bool                    // true if ceiling forced a synthesis
	Error      error
}

// Turn runs one full conversational turn: receives the user query,
// loops planner ↔ tools up to PlanLoopCeiling, and returns the
// assistant's final response. Side effects:
//
//   - Working set grows with user/assistant/tool messages
//   - Archive gets one event per message (durable)
//   - Episodic store gets one row at end of turn (the pair)
//   - Soul is read but never mutated (use ProposeEdit out-of-band)
//
// Cancellation: ctx is checked at every loop boundary. A cancelled
// ctx stops the loop and surfaces ctx.Err() in TurnResult.Error.
func (a *Agent) Turn(ctx context.Context, req TurnRequest) (*TurnResult, error) {
	if req.UserQuery == "" {
		return nil, errors.New("agent: TurnRequest.UserQuery required")
	}
	a.emit(Event{Kind: EventTurnStart, Text: req.UserQuery})

	// 1. Resolve planner LLM + embedder. Capture the router ONCE so a
	// mid-turn /model swap can't change models under this turn's feet.
	router := a.Router()
	planner, profile, err := router.LLMForRole(ctx, a.plannerRole)
	if err != nil {
		return nil, fmt.Errorf("agent: resolve planner role: %w", err)
	}
	embedder, _, err := router.EmbedderForRole(ctx, a.embedderRole)
	if err != nil {
		return nil, fmt.Errorf("agent: resolve embedder role: %w", err)
	}

	// 2. Append user message to working + archive.
	now := time.Now().UTC()
	userMsg := working.Message{Role: "user", Content: req.UserQuery, Timestamp: now}
	a.cfg.Working.Append(userMsg)
	a.archiveEvent(ctx, archive.Event{
		Timestamp: now, AgentID: a.cfg.AgentID, SessionID: a.cfg.SessionID,
		Role: "user", Content: req.UserQuery,
	})

	// 3. Embed the user query for retrieval.
	queryEmbedding, err := a.embedQuery(ctx, embedder, req.UserQuery)
	if err != nil {
		// Retrieval is best-effort; embedding failures shouldn't kill
		// the turn. Log and continue without retrieval.
		a.log.Warn("agent: embed query failed; continuing without retrieval",
			"agent", a.cfg.AgentID, "err", err)
	}

	// 4. Resolve which tools to surface this turn (top-K per profile).
	availableTools, err := a.cfg.Tools.Search(ctx, queryEmbedding, profile.MaxToolsPerCall)
	if err != nil {
		return nil, fmt.Errorf("agent: tool search: %w", err)
	}
	// Capability gate (TEN-103): only offer tools the active model may use, so a
	// small local planner never sees an augmentation tool like memory_recall.
	availableTools = filterGatedTools(availableTools, profile)
	// Minify the SURVIVORS' JSON schemas before they hit the wire (TEN-227):
	// drop pure-doc keywords + compact whitespace + trim paragraph-length
	// property prose, preserving types/enums/required. Applied here (post rank +
	// gate) so it's uniform across backends AND the budget accounting below
	// counts the same minified specs the backend will send. Canonical registry
	// specs are untouched (operates on copies).
	availableTools = model.MinifyToolSchemas(availableTools)
	// Observability for embedding-ranked tool selection (TEN-225). Emit a
	// per-turn line whether ranking TRIMMED or fell back to the full catalog —
	// the silent-fallback case is exactly what hides a fat tool dump in the
	// prompt every message, so it must be VISIBLE, not silent. Prefer the
	// registry's precise reason (RankingReporter); fall back to the count
	// heuristic for registries that don't report it.
	if rr, ok := a.cfg.Tools.(RankingReporter); ok {
		if ranked, surfaced, catalog, reason, have := rr.RankingStatus(); have {
			if ranked {
				a.emit(Event{Kind: EventToolCatalog,
					Text: fmt.Sprintf("tool ranking ON — %d of %d enabled tools surfaced this turn", surfaced, catalog)})
			} else {
				a.emit(Event{Kind: EventToolCatalog,
					Text: fmt.Sprintf("tool ranking OFF — full catalog of %d tools surfaced (%s)", catalog, reason)})
			}
		}
	} else if all := a.cfg.Tools.All(); len(availableTools) < len(all) {
		a.emit(Event{
			Kind: EventToolCatalog,
			Text: fmt.Sprintf("ranked: %d of %d tools surfaced this turn", len(availableTools), len(all)),
		})
	}

	// 5. Build the assembler if not provided.
	asm := a.cfg.Assembler
	if asm == nil {
		asm = assemble.New(assemble.NewLLMCounter(planner))
	}

	// 5b. Retrieve relevant T4 skills (reusable recipes) and fold them
	// into the system prompt for this turn — rides the existing
	// system-prompt budget; no assembler surgery.
	sysPrompt := a.cfg.SystemPrompt
	if a.cfg.Skills != nil {
		if cards, serr := a.cfg.Skills.RetrieveSkills(ctx, queryEmbedding, 3); serr == nil && len(cards) > 0 {
			sysPrompt += renderSkills(cards)
			names := make([]string, len(cards))
			for i, c := range cards {
				names[i] = c.Name
			}
			a.emit(Event{Kind: EventSkills, Text: strings.Join(names, ", ")})
		}
	}

	// 5d. Lazy tool loading (TEN-228): inject the cheap catalog of every OTHER
	// enabled tool into the system prompt and seed the per-turn loaded set. The
	// per-iteration tool array carries the working set + load_tool + whatever the
	// model loads; the catalog tells it what else it can pull on demand. Off by
	// default — when off, `loaded` stays empty and nothing below changes.
	loaded := map[string]bool{}
	if a.cfg.LazyToolLoad {
		active := make(map[string]bool, len(availableTools))
		for _, t := range availableTools {
			active[t.Name] = true
		}
		sysPrompt += renderToolCatalog(a.cfg.Tools.All(), active)
	}

	// 5c. Derive the persistent goal header (TEN-102) from the latest compaction
	// summary in the DURABLE working set (not the assembler's truncated view, so
	// it survives even after the summary message itself is truncated from the
	// rendered window). Re-injected into the system block every turn, never
	// summarized — drift mitigation (arXiv:2510.07777). Empty before the first
	// compaction (the raw goal is still verbatim in-context then).
	goalHeader := compress.ExtractGoalHeader(latestSummaryContent(a.cfg.Working.Messages()))

	// 6. Run the bounded loop.
	result := &TurnResult{}
	// After the turn (any exit path), compact the working set if the working
	// tier crossed the hysteresis high-watermark — so the NEXT turn starts lean.
	// Best-effort and post-response, so it never blocks the answer. Hysteresis
	// (vs a single line) stops compaction re-firing every turn near the
	// threshold; the agent owns the state because the assembler is per-turn.
	defer func() {
		if a.cfg.Compactor == nil || ctx.Err() != nil || len(result.Reports) == 0 {
			return
		}
		last := result.Reports[len(result.Reports)-1]
		if a.compaction.shouldCompact(last.WorkingUsageFrac) {
			a.compactWorking(ctx)
		}
	}()
	consecutiveValidationFailures := 0
	ceiling := profile.PlanLoopCeiling
	if ceiling <= 0 {
		ceiling = 5 // safe default if profile didn't set it
	}
	// A per-turn override (e.g. an active /goal run) decouples THIS turn's
	// iteration budget from the global PlanLoopCeiling every normal turn shares:
	// >0 sets it, <0 = unlimited (omit the cap). 0 leaves the profile default in
	// place (TEN-216).
	if req.LoopCeiling != 0 {
		ceiling = req.LoopCeiling
	}
	unlimitedLoop := ceiling < 0

	for iter := 1; unlimitedLoop || iter <= ceiling; iter++ {
		if err := ctx.Err(); err != nil {
			// Don't return empty — the agent has been gathering tool data
			// (web_read, wiki_search, etc.) and that working memory IS the
			// answer; we just never asked the model to synthesize it. Run
			// a bounded best-effort synthesis on a fresh context so a wave-
			// timeout in the research orchestrator turns into a partial
			// report instead of "(no result)".
			if len(result.ToolTrace) > 0 {
				salCtx, salCancel := context.WithTimeout(context.Background(), 30*time.Second)
				if final, ferr := a.synthesizeFinal(salCtx, planner, profile, asm, queryEmbedding, goalHeader); ferr == nil && strings.TrimSpace(final) != "" {
					result.Response = final
					result.Truncated = true
					a.emit(Event{Kind: EventFinal, Iter: iter, Text: final})
					salCancel()
					return result, nil
				}
				salCancel()
			}
			result.Error = err
			return result, nil
		}

		// Fold any mid-turn user interjections into the working set before
		// assembling, so the planner sees them THIS iteration and decides
		// whether to address-and-resume or pivot.
		for _, msg := range a.takeInjections() {
			ts := time.Now().UTC()
			a.cfg.Working.Append(working.Message{
				Role: "user",
				Content: "[The user interjected mid-task. Address this now, then resume your " +
					"original task — unless it overrides the task.]\n" + msg,
				Timestamp: ts,
			})
			a.archiveEvent(ctx, archive.Event{
				Timestamp: ts, AgentID: a.cfg.AgentID, SessionID: a.cfg.SessionID,
				Role: "user", Content: msg,
			})
			a.emit(Event{Kind: EventInterject, Iter: iter, Text: msg})
		}

		result.Iterations = iter

		// Per-iteration tool array. With lazy loading (TEN-228) it's the working
		// set + load_tool + whatever the model has loaded so far, rebuilt each
		// iteration so a just-loaded tool's schema appears on the next step.
		// Otherwise it's the static surfaced set.
		iterTools := availableTools
		if a.cfg.LazyToolLoad {
			iterTools = a.buildLazyTools(availableTools, loaded)
		}

		assembled, err := asm.Assemble(ctx, assemble.Request{
			Profile:       profile,
			Soul:          a.soul(),
			SystemPrompt:  sysPrompt,
			UserProfile:   a.cfg.UserProfile.Render(),
			ProjectSME:    a.projectSME(),
			GoalHeader:    goalHeader,
			Tools:         iterTools,
			Working:       a.cfg.Working,
			EpisodicStore: a.cfg.Episodic,
			SemanticStore: a.cfg.Semantic,
			Query:         assemble.RetrievalQuery{Embedding: queryEmbedding},
			AgentID:       a.cfg.AgentID,
			// We deliberately DON'T pass UserQuery here — the user
			// query is already in the working set as the first append
			// of this turn. Passing it again would double-stamp.
		})
		if err != nil {
			return nil, fmt.Errorf("agent: assemble iter=%d: %w", iter, err)
		}
		result.Tokens = assembled.BudgetReport.Total
		result.Reports = append(result.Reports, assembled.BudgetReport)
		br := assembled.BudgetReport
		a.emit(Event{Kind: EventMemory, Iter: iter, Budget: &br})

		// Run the planner under a per-iteration cancel so an Interject can
		// bail this call and re-plan immediately with the new message.
		iterCtx, iterCancel := context.WithCancel(ctx)
		a.setIterCancel(iterCancel)
		resp, err := a.plan(iterCtx, planner, profile.ToolFormat, iter, model.GenerateRequest{
			Messages: assembled.Messages,
			Tools:    iterTools,
		})
		a.setIterCancel(nil)
		iterCancel()

		// Interrupt handling FIRST — independent of how a (possibly
		// streaming) planner call reported cancellation. A cancelled stream
		// can surface as an error OR as a truncated-but-nil-error response, so
		// we must not infer interrupt state from err alone.
		if ctx.Err() != nil {
			// Hard interrupt (Esc / app shutdown): stop the turn cleanly.
			result.Error = ctx.Err()
			return result, nil
		}
		if a.hasInjections() {
			// Soft interrupt: a user message was queued mid-call. Discard this
			// (possibly partial) result and re-plan with it folded in. Free
			// slot — the user steering shouldn't burn the loop budget.
			iter--
			continue
		}
		if err != nil {
			a.emit(Event{Kind: EventError, Iter: iter, Text: err.Error()})
			return nil, fmt.Errorf("agent: planner generate iter=%d: %w", iter, err)
		}
		if resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 {
			a.emit(Event{Kind: EventUsage, Iter: iter,
				PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens})
		}
		if resp.Text != "" {
			a.emit(Event{Kind: EventAssistant, Iter: iter, Text: resp.Text})
		}

		// Append the assistant message regardless of whether it has
		// tool calls — the model emitted it, archive it.
		asstMsg := working.Message{
			Role:      "assistant",
			Content:   resp.Text,
			ToolCalls: resp.ToolCalls,
			Timestamp: time.Now().UTC(),
		}
		a.cfg.Working.Append(asstMsg)
		a.archiveAssistant(ctx, resp)

		if len(resp.ToolCalls) == 0 {
			// Empty-final guard. Merged "thinking" models (aeon-ultimate /
			// qwen-derived merges) sometimes emit a turn with no tool calls
			// AND no visible text — the whole response was a <think> block
			// that got stripped, OR the model just bailed. Returning empty
			// here cascaded into "(no result)" research reports even when
			// the agent had gathered thousands of chars of real page text
			// via web_read. Treat it as a degenerate finish and force a
			// tools-off synthesis from the working memory, same path as
			// loop-ceiling — that gives the model one more chance to write
			// the answer it implicitly already has.
			if strings.TrimSpace(resp.Text) == "" && len(result.ToolTrace) > 0 {
				a.log.Warn("agent: degenerate empty-final after tool use; forcing synthesis",
					"agent", a.cfg.AgentID, "iter", iter, "tools_called", len(result.ToolTrace))
				final, ferr := a.synthesizeFinal(ctx, planner, profile, asm, queryEmbedding, goalHeader)
				if ferr == nil && strings.TrimSpace(final) != "" {
					result.Response = final
					a.emit(Event{Kind: EventFinal, Iter: iter, Text: final})
					a.persistEpisode(ctx, req.UserQuery, final, result.ToolTrace, queryEmbedding, embedder)
					return result, nil
				}
				// Synthesis also empty — fall through to the original
				// behavior so we don't loop forever on a stuck model.
			}
			// Final response. Persist episode and return.
			result.Response = resp.Text
			a.emit(Event{Kind: EventFinal, Iter: iter, Text: resp.Text})
			a.persistEpisode(ctx, req.UserQuery, resp.Text, result.ToolTrace, queryEmbedding, embedder)
			return result, nil
		}

		// Lazy tool loading (TEN-228): intercept load_tool calls in-agent — it
		// isn't a mux tool (so it would fail validation), and the agent owns the
		// per-turn loaded set. Handle them here, then drop them from the batch.
		// If load_tool was the ONLY call this iteration, re-plan so the next
		// iteration's tool array carries the freshly-loaded schema.
		if a.cfg.LazyToolLoad {
			if loads, rest := splitLoadToolCalls(resp.ToolCalls); len(loads) > 0 {
				for _, call := range loads {
					a.emit(Event{Kind: EventToolCall, Iter: iter, Tool: call.Name, Args: string(call.Arguments)})
					a.handleLoadTool(ctx, call, loaded)
				}
				// rest is a FRESH slice — does NOT alias the assistant message's
				// ToolCalls already recorded in the working set (see
				// splitLoadToolCalls).
				resp.ToolCalls = rest
				if len(resp.ToolCalls) == 0 {
					continue // only load_tool this iteration — re-plan with it loaded
				}
			}
		}

		// Validate every tool call. If ANY fail, feed errors back as
		// tool results and continue (giving the model a chance to fix).
		// Two consecutive iterations of all-invalid calls → bail.
		allInvalid := true
		validCalls := make([]model.ToolCall, 0, len(resp.ToolCalls))
		for _, call := range resp.ToolCalls {
			if err := validateToolCall(call, a.cfg.Tools); err != nil {
				a.feedValidationError(ctx, call, err)
				continue
			}
			// Capability gate (TEN-103) — defense in depth: validation +
			// dispatch route by name against the FULL registry, so a model that
			// emits a gated tool it wasn't offered (hallucination, leakage) would
			// otherwise still reach the dispatcher. Refuse it here with a clean
			// tool error instead.
			if spec, ok := a.cfg.Tools.Get(call.Name); ok && spec.Gate != "" && !profile.AllowsTool(spec.Gate) {
				a.feedToolResult(ctx, ToolCallResult{
					Call:    call,
					Result:  fmt.Sprintf("%q is not available for the current model.", call.Name),
					IsError: true,
				})
				continue
			}
			allInvalid = false
			validCalls = append(validCalls, call)
		}
		if allInvalid {
			consecutiveValidationFailures++
			if consecutiveValidationFailures >= 2 {
				result.Error = ErrTooManyValidationFailures
				return result, nil
			}
			continue
		}
		consecutiveValidationFailures = 0

		// Dispatch in bounded parallel.
		parallel := profile.MaxParallelTools
		if parallel <= 0 {
			parallel = 1
		}
		for _, call := range validCalls {
			a.emit(Event{Kind: EventToolCall, Iter: iter, Tool: call.Name, Args: string(call.Arguments)})
		}
		batchResults := dispatchBatch(ctx, validCalls, parallel, a.cfg.Dispatcher)
		for _, br := range batchResults {
			result.ToolTrace = append(result.ToolTrace, br)
			a.emit(Event{Kind: EventToolResult, Iter: iter, Tool: br.Call.Name, Result: br.Result, IsErr: br.IsError})
			a.feedToolResult(ctx, br)
		}
	}

	// 7. Ceiling reached without a final response. Force synthesis.
	final, err := a.synthesizeFinal(ctx, planner, profile, asm, queryEmbedding, goalHeader)
	if err != nil {
		result.Error = fmt.Errorf("%w; forced synthesis also failed: %v", ErrLoopCeilingReached, err)
		a.emit(Event{Kind: EventError, Text: result.Error.Error()})
		return result, nil
	}
	result.Response = final
	result.Truncated = true
	result.Error = ErrLoopCeilingReached
	a.emit(Event{Kind: EventTruncated, Text: final})
	a.persistEpisode(ctx, req.UserQuery, final, result.ToolTrace, queryEmbedding, nil)
	return result, nil
}

// microcompact applies the compactor's no-LLM tool-result elision pass when the
// configured compactor supports one (compress.Compressor does). It shrinks stale
// tool-result BODIES to stubs while the full bodies stay durable in the archive —
// lossless at the system level, so it runs BEFORE the LLM summarizer (and shrinks
// its input too). Best-effort: an error is logged and treated as "no change".
func (a *Agent) microcompact(msgs []working.Message) ([]working.Message, bool) {
	mc, ok := a.cfg.Compactor.(interface {
		Microcompact([]working.Message) ([]working.Message, bool, error)
	})
	if !ok {
		return msgs, false
	}
	out, changed, err := mc.Microcompact(msgs)
	if err != nil {
		a.log.Warn("agent: microcompact failed", "agent", a.cfg.AgentID, "err", err)
		return msgs, false
	}
	return out, changed
}

// runCompact invokes the configured Compactor. If it supports archive-sourced
// compaction (compress.Compressor does), the agent passes a Reader over its own
// archive + this session's id, so the summarizer works from the raw transcript
// (drift-free) instead of folding a prior summary. Falls back to plain Compact.
func (a *Agent) runCompact(ctx context.Context, msgs []working.Message) ([]working.Message, bool, error) {
	if ac, ok := a.cfg.Compactor.(interface {
		CompactWithArchive(context.Context, []working.Message, *archive.Reader, string) ([]working.Message, bool, error)
	}); ok {
		var rdr *archive.Reader
		if a.cfg.Archive != nil {
			rdr = a.cfg.Archive.Reader()
		}
		return ac.CompactWithArchive(ctx, msgs, rdr, a.cfg.SessionID)
	}
	return a.cfg.Compactor.Compact(ctx, msgs)
}

// compactWorking runs microcompaction (cheap, no LLM) and then the configured
// Compactor over the working set, swapping in the result. Best-effort: errors
// are logged, not propagated (a failed compaction must never break a turn).
func (a *Agent) compactWorking(ctx context.Context) {
	if a.cfg.Compactor == nil {
		return
	}
	msgs := a.cfg.Working.Messages()
	origLen := len(msgs)
	msgs, microChanged := a.microcompact(msgs)
	out, changed, err := a.runCompact(ctx, msgs)
	if err != nil {
		a.log.Warn("agent: working-set compaction failed", "agent", a.cfg.AgentID, "err", err)
		if microChanged {
			// The elision is lossless (bodies live in the archive); keep it even
			// though summarization failed.
			a.cfg.Working.Replace(msgs)
		}
		return
	}
	if changed {
		msgs = out
	}
	if !changed && !microChanged {
		return
	}
	a.cfg.Working.Replace(msgs)
	a.emit(Event{Kind: EventCompact, Text: fmt.Sprintf("%d → %d messages", origLen, len(msgs))})
}

// filterGatedTools drops specs whose capability gate the active profile
// disallows (Profile.AllowsTool), so the model is only offered tools it may
// actually use. Ungated specs (Gate=="") always pass. (TEN-103)
func filterGatedTools(specs []model.ToolSpec, profile model.Profile) []model.ToolSpec {
	out := make([]model.ToolSpec, 0, len(specs))
	for _, s := range specs {
		if s.Gate != "" && !profile.AllowsTool(s.Gate) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// latestSummaryContent returns the Content of the most recent compaction-summary
// message in the working set, or "" if none exists yet. The persistent goal
// header (TEN-102) is re-derived from it each turn — scanning the durable set
// (not a truncated render) is what lets the goal survive after the summary
// message itself is dropped from the rendered window.
func latestSummaryContent(msgs []working.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Kind == compress.KindCompactionSummary {
			return msgs[i].Content
		}
	}
	return ""
}

// CompactNow forces microcompaction + a working-set compaction regardless of the
// budget signal (the manual /compress command). Returns the before/after message
// counts. A no-op (before == after, nothing elided) means there wasn't enough to
// compact.
func (a *Agent) CompactNow(ctx context.Context) (before, after int, err error) {
	if a.cfg.Compactor == nil {
		return 0, 0, errors.New("agent: context compaction is not configured")
	}
	msgs := a.cfg.Working.Messages()
	before = len(msgs)
	msgs, microChanged := a.microcompact(msgs)
	out, changed, cerr := a.runCompact(ctx, msgs)
	if cerr != nil {
		return before, before, cerr
	}
	if changed {
		msgs = out
	}
	if changed || microChanged {
		a.cfg.Working.Replace(msgs)
		a.emit(Event{Kind: EventCompact, Text: fmt.Sprintf("%d → %d messages", before, len(msgs))})
	}
	return before, len(msgs), nil
}

// ClearContext wipes the working set — a fresh conversation for THIS session,
// like a terminal `clear` for the agent's short-term memory. Durable layers
// (episodic memory, distilled facts, the archive, the soul) are untouched, so
// recall and learned facts survive. Returns the number of messages cleared.
// Call only between turns (the TUI guards on !busy). (TEN-181)
func (a *Agent) ClearContext() int {
	n := a.cfg.Working.Len()
	a.cfg.Working.Reset()
	if a.compaction != nil {
		a.compaction.armed = false // disarm; the next turn re-evaluates from ~empty
	}
	return n
}

// embedQuery wraps the embedder call. Returns an empty embedding on
// failure (caller treats nil/empty as "no retrieval").
func (a *Agent) embedQuery(ctx context.Context, e model.Embedder, q string) ([]float32, error) {
	vecs, err := e.Embed(ctx, []string{q})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, errors.New("embedder returned no vectors")
	}
	return vecs[0], nil
}

// synthesizeFinal is the "no more tools, give me your best answer"
// fallback when PlanLoopCeiling is reached. Sets Tools to empty so
// the model can't keep stalling.
func (a *Agent) synthesizeFinal(ctx context.Context, planner model.LLM, profile model.Profile, asm *assemble.Assembler, queryEmbedding []float32, goalHeader string) (string, error) {
	final, err := asm.Assemble(ctx, assemble.Request{
		Profile:    profile,
		Soul:       a.soul(),
		GoalHeader: goalHeader,
		SystemPrompt: a.cfg.SystemPrompt + "\n\n" +
			"== FINAL ANSWER MODE — NO TOOLS ==\n" +
			"Tool usage is COMPLETE. No tools are available this turn. Write your final\n" +
			"answer DIRECTLY as plain markdown prose, using ONLY information already in\n" +
			"this conversation (from earlier tool results above).\n\n" +
			"CRITICAL: Do NOT emit ANY tool-call markup. This includes:\n" +
			"  - <tool_call>...</tool_call> blocks (any shape)\n" +
			"  - <function=NAME>...</function> tags\n" +
			"  - <parameter=KEY>...</parameter> tags\n" +
			"  - ```tool_code fenced blocks\n" +
			"  - `tool_name(args)` function-call prose\n" +
			"Any tool-call markup will be discarded as garbage. Write prose only.",
		UserProfile:   a.cfg.UserProfile.Render(),
		ProjectSME:    a.projectSME(),
		Tools:         nil, // explicitly no tools
		Working:       a.cfg.Working,
		EpisodicStore: a.cfg.Episodic,
		SemanticStore: a.cfg.Semantic,
		Query:         assemble.RetrievalQuery{Embedding: queryEmbedding},
		AgentID:       a.cfg.AgentID,
	})
	if err != nil {
		return "", err
	}
	resp, err := planner.Generate(ctx, model.GenerateRequest{
		Messages: final.Messages,
		// No tools — force a text-only finish. Stop sequences kill any model
		// that pattern-matches "I should call another tool" — vLLM stops
		// generation the instant the token sequence appears in the output.
		// Live trigger (aeon-ultimate): model emitted only <tool_call> markup
		// when forced into tools-off synthesis. Stop sequences make that a
		// short empty answer instead of a 200-char tool-call dump.
		StopSequences: []string{"<tool_call", "<function=", "<parameter=", "```tool_code"},
	})
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(resp.Text)
	// Fallback: if the model produced nothing usable (stop sequence fired
	// immediately, or the response is still tool-call shaped despite the
	// strong prompt), build a summary from the tool TRACE so downstream
	// synthesis has SOMETHING to work with — better than empty. The agent
	// gathered real page text via web_read etc., we just couldn't get the
	// model to narrate it.
	if text == "" || looksLikeToolCallMarkup(text) {
		text = summarizeToolMemory(a.cfg.Working.Messages())
	}
	now := time.Now().UTC()
	a.cfg.Working.Append(working.Message{
		Role: "assistant", Content: text, Timestamp: now,
	})
	a.archiveEvent(ctx, archive.Event{
		Timestamp: now, AgentID: a.cfg.AgentID, SessionID: a.cfg.SessionID,
		Role: "assistant", Content: text,
	})
	return text, nil
}

// looksLikeToolCallMarkup returns true if the entire string is dominated by
// tool-call markup (the model emitted "<tool_call>..." instead of prose).
// Cheap heuristic: ≥30% of the chars are inside tool-call tags, OR the
// non-whitespace text starts with a tool-call marker.
func looksLikeToolCallMarkup(s string) bool {
	if s == "" {
		return false
	}
	t := strings.TrimLeft(s, " \n\t\r")
	for _, marker := range []string{"<tool_call", "<function=", "<parameter=", "```tool_code"} {
		if strings.HasPrefix(t, marker) {
			return true
		}
	}
	// Count chars inside any tool_call/function block. >30% → garbage.
	total := len(s)
	if total == 0 {
		return false
	}
	junk := 0
	for _, marker := range []string{"<tool_call", "<function=", "<parameter="} {
		junk += strings.Count(s, marker) * len(marker)
	}
	return junk*100/total > 30
}

// summarizeToolMemory builds a minimal prose summary from the agent's
// gathered tool results, used as a last-resort answer when the model
// refuses to write one. NOT a great report — but unblocks downstream
// synthesis (the orchestrator's synthesizeReport can write the real
// narrative from this data). Each tool message becomes one bullet.
func summarizeToolMemory(msgs []working.Message) string {
	var b strings.Builder
	b.WriteString("Tool results gathered:\n")
	count := 0
	for _, m := range msgs {
		if m.Role != "tool" || strings.TrimSpace(m.Content) == "" {
			continue
		}
		// Cap each result so a 20K web_read doesn't dominate the answer.
		body := strings.TrimSpace(m.Content)
		if len(body) > 800 {
			body = body[:800] + "…"
		}
		fmt.Fprintf(&b, "- %s\n", body)
		count++
		if count >= 8 { // soft cap — keep the dump bounded
			break
		}
	}
	if count == 0 {
		return "" // nothing gathered — caller falls through to empty path
	}
	return b.String()
}

// renderSkills formats retrieved T4 skills as a system-prompt block.
func renderSkills(cards []SkillCard) string {
	var b strings.Builder
	b.WriteString("\n\n## Skills — reusable procedures you can follow when relevant:\n")
	for _, c := range cards {
		fmt.Fprintf(&b, "- %s: %s\n  %s\n", c.Name, c.Description, c.Recipe)
	}
	return b.String()
}

// plan runs one planner generation. Stream off → the buffered Generate
// path (unchanged). Stream on → consume GenerateStream, emitting each
// text delta as EventToken and checking the terminal StreamChunk.Error
// (the silent-failure guard), then assemble the same {Text, ToolCalls}
// the buffered path returns (the backend reassembles tool calls in
// both paths, incl. the Gemma text-block safety net).
// planMaxAttempts bounds how many times a single planner call is re-issued
// after a TRANSIENT stream failure. A hosted provider's load balancer recycles
// the pooled HTTP/2 connection with a GOAWAY as a matter of course; caught
// mid-stream that surfaces as a read error Go can't auto-retry, and with no
// retry here a single one killed whole /goal loops (TEN-215). The planner call
// is idempotent and nothing is committed to the working set until plan()
// returns, so re-issuing on a fresh connection is safe.
const planMaxAttempts = 3

// plan runs one planner call, retrying ONLY on transient connection drops
// (GOAWAY / mid-stream reset). Everything else — including a user interrupt —
// returns immediately. The streamed-token re-render on a retry is cosmetic;
// finalizeAssistant reconciles the bubble to the authoritative final text.
func (a *Agent) plan(ctx context.Context, planner model.LLM, format string, iter int, req model.GenerateRequest) (*model.GenerateResponse, error) {
	for attempt := 1; ; attempt++ {
		resp, err := a.planOnce(ctx, planner, format, iter, req)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil || attempt >= planMaxAttempts || !isRetryableStreamErr(err) {
			return nil, err
		}
		a.log.Warn("agent: planner stream dropped — retrying on a fresh connection",
			"iter", iter, "attempt", attempt, "err", err)
		// Brief, cancellable backoff so we don't hammer an endpoint that's
		// mid-recycle. The dropped connection is already marked dead by the
		// transport, so the next attempt dials fresh.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * 300 * time.Millisecond):
		}
	}
}

// planOnce performs exactly ONE planner call (streaming or buffered) and
// returns the fully-consumed response. A partial stream is never returned as
// success — a mid-stream error propagates so plan() can decide whether to
// re-issue.
func (a *Agent) planOnce(ctx context.Context, planner model.LLM, format string, iter int, req model.GenerateRequest) (*model.GenerateResponse, error) {
	if !a.cfg.Stream {
		return planner.Generate(ctx, req)
	}
	stream, err := planner.GenerateStream(ctx, req)
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	resp := &model.GenerateResponse{}
	for ch := range stream {
		if ch.Error != nil {
			return nil, ch.Error // terminal error — never treat a partial stream as success
		}
		if ch.Delta != "" {
			sb.WriteString(ch.Delta)
			a.emit(Event{Kind: EventToken, Iter: iter, Text: ch.Delta})
		}
		if ch.ToolCallDelta != nil {
			resp.ToolCalls = append(resp.ToolCalls, *ch.ToolCallDelta)
		}
		if ch.FinishReason != "" {
			resp.FinishReason = ch.FinishReason
		}
		if ch.Usage != nil {
			resp.Usage = *ch.Usage
		}
	}
	// Clean this model family's display artifacts from the streamed text
	// (harmony channel tokens / <think> blocks). Tool calls were already
	// extracted by the backend's text safety net on the raw stream, so
	// cleaning here only affects display.
	resp.Text = toolfmt.AdapterFor(format).CleanText(sb.String())
	return resp, nil
}

// isRetryableStreamErr reports whether a planner error is a TRANSIENT
// connection drop worth re-issuing — a hosted provider's load balancer
// recycling the pooled HTTP/2 connection (GOAWAY) or a mid-stream reset/EOF.
// It deliberately excludes semantic failures (context overflow, invalid
// request, rate limit, billing) and cancellation, where a retry can't help or
// is outright wrong: a context overflow retries into the same overflow, and a
// cancellation is the user's interrupt. Detection is by error-kind first, then
// a narrow substring set on the wrapped transport text (the stream error is
// formatted as "model: internal error: stream read: <net err>").
func isRetryableStreamErr(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, model.ErrContextOverflow),
		errors.Is(err, model.ErrInvalidRequest),
		errors.Is(err, model.ErrRateLimited),
		errors.Is(err, model.ErrInsufficientBalance),
		errors.Is(err, model.ErrCancelled),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return false
	}
	s := strings.ToLower(err.Error())
	for _, marker := range []string{
		"goaway",
		"http2:",
		"connection reset",
		"unexpected eof",
		"broken pipe",
		"use of closed network connection",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// feedValidationError appends a tool-result message representing the
// validation failure so the model sees the error on the next iteration.
func (a *Agent) feedValidationError(ctx context.Context, call model.ToolCall, validationErr error) {
	a.emit(Event{Kind: EventValidation, Tool: call.Name, Text: validationErr.Error()})
	errMsg := fmt.Sprintf("tool call validation failed: %s", validationErr.Error())
	now := time.Now().UTC()
	a.cfg.Working.Append(working.Message{
		Role:       "tool",
		ToolCallID: call.ID,
		Content:    errMsg,
		Timestamp:  now,
	})
	a.archiveEvent(ctx, archive.Event{
		Timestamp: now, AgentID: a.cfg.AgentID, SessionID: a.cfg.SessionID,
		Role:       "tool",
		ToolResult: &archive.ToolResult{CallID: call.ID, Content: errMsg, IsError: true},
	})
}

func (a *Agent) feedToolResult(ctx context.Context, r ToolCallResult) {
	content := r.Result
	if r.Err != nil {
		content = fmt.Sprintf("tool dispatch error: %s", r.Err.Error())
	}
	now := time.Now().UTC()
	a.cfg.Working.Append(working.Message{
		Role:       "tool",
		ToolCallID: r.Call.ID,
		Content:    content,
		Timestamp:  now,
	})
	a.archiveEvent(ctx, archive.Event{
		Timestamp: now, AgentID: a.cfg.AgentID, SessionID: a.cfg.SessionID,
		Role: "tool",
		ToolResult: &archive.ToolResult{
			CallID:  r.Call.ID,
			Content: content,
			IsError: r.IsError || r.Err != nil,
		},
	})
}

func (a *Agent) archiveAssistant(ctx context.Context, resp *model.GenerateResponse) {
	if a.cfg.Archive == nil {
		return
	}
	now := time.Now().UTC()
	ev := archive.Event{
		Timestamp: now, AgentID: a.cfg.AgentID, SessionID: a.cfg.SessionID,
		Role: "assistant", Content: resp.Text,
	}
	if len(resp.ToolCalls) > 0 {
		ev.ToolCalls = make([]archive.ToolCall, len(resp.ToolCalls))
		for i, tc := range resp.ToolCalls {
			ev.ToolCalls[i] = archive.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}
		}
	}
	a.archiveEvent(ctx, ev)
}

func (a *Agent) archiveEvent(_ context.Context, e archive.Event) {
	if a.cfg.Archive == nil {
		return
	}
	if err := a.cfg.Archive.Append(e); err != nil {
		a.log.Warn("agent: archive append failed", "err", err)
	}
}

// persistEpisode writes the completed turn-pair to the T2 episodic
// store. Embedder is allowed to be nil for the synthesis path (we
// pass through the precomputed queryEmbedding).
func (a *Agent) persistEpisode(ctx context.Context, prompt, response string, trace []ToolCallResult, queryEmbedding []float32, embedder model.Embedder) {
	if a.cfg.Episodic == nil {
		return
	}
	if len(queryEmbedding) == 0 {
		a.log.Debug("agent: skipping episode insert (no query embedding)")
		return
	}
	toolCalls := make([]episodic.ToolCallRef, 0, len(trace))
	for _, t := range trace {
		toolCalls = append(toolCalls, episodic.ToolCallRef{
			ID:        t.Call.ID,
			Name:      t.Call.Name,
			Arguments: string(t.Call.Arguments),
		})
	}
	outcome := episodic.OutcomeSuccess
	for _, t := range trace {
		if t.IsError || t.Err != nil {
			outcome = episodic.OutcomeError
			break
		}
	}
	embedderID := a.embedderRole
	visibility := a.cfg.EpisodeVisibility
	if visibility == "" {
		visibility = episodic.VisibilityPrivate
	}
	_, err := a.cfg.Episodic.Insert(ctx, &episodic.Episode{
		AgentID:    a.cfg.AgentID,
		Visibility: visibility,
		SessionID:  a.cfg.SessionID,
		Prompt:     prompt,
		Response:   response,
		ToolCalls:  toolCalls,
		Outcome:    outcome,
		EmbedderID: string(embedderID),
		Embedding:  queryEmbedding,
	})
	if err != nil {
		a.log.Warn("agent: episode insert failed", "err", err)
	}
	_ = embedder // reserved for future per-content embedding (response embedding)
}

// newSessionID generates an unguessable-ish session ID. Not for
// security — collision avoidance only. Format: "sess_" + 16 hex chars.
func newSessionID() string {
	const hex = "0123456789abcdef"
	now := time.Now().UnixNano()
	buf := make([]byte, 16)
	for i := range buf {
		buf[i] = hex[now&0xf]
		now >>= 4
	}
	return "sess_" + string(buf)
}
