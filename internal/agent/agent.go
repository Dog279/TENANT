package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
	"tenant/internal/runtrace"
)

// DefaultActiveRouteBudget bounds the ACTIVE-routing join when the operator has
// not configured `route.timeout_ms`. It is ONE deadline measured from
// EventTurnStart covering BOTH the classifier hook and routed-role resolution —
// so a classifier that eats the whole budget leaves resolveRoutedRole none, and
// it returns DeadlineExceeded without making a single call ("routed role
// unavailable; using planner"). Sized for a warm local classifier (Supra 51M
// measures ~115ms warm, ~2.7s cold from an unloaded Ollama).
const DefaultActiveRouteBudget = 250 * time.Millisecond

// activeRouteBudgetNS holds the operator's configured budget in nanoseconds.
// Package-level and atomic rather than a Config field because EVERY agent in
// the process (main, tenant-discord, tenant-cron, imessage, peers) must share
// the single `route.timeout_ms`, and because `/route` can change it at runtime —
// per-Config copies taken at construction would silently go stale. Set from the
// same choke point that builds the classifier, so the two can never disagree.
var activeRouteBudgetNS atomic.Int64

// SetActiveRouteBudget applies the operator's configured active-routing budget.
// A non-positive duration restores DefaultActiveRouteBudget.
func SetActiveRouteBudget(d time.Duration) {
	if d <= 0 {
		activeRouteBudgetNS.Store(0)
		return
	}
	activeRouteBudgetNS.Store(int64(d))
}

// ActiveRouteBudget reports the budget Turn will apply.
func ActiveRouteBudget() time.Duration {
	if v := activeRouteBudgetNS.Load(); v > 0 {
		return time.Duration(v)
	}
	return DefaultActiveRouteBudget
}

// ErrLoopCeilingReached is set in TurnResult.Error when the agent
// hit Profile.PlanLoopCeiling without the model emitting a final
// response. The runtime forces a final synthesis call (no tools) and
// returns whatever the model produces.
var ErrLoopCeilingReached = errors.New("agent: plan loop ceiling reached")

// ErrTooManyValidationFailures is set when 2 consecutive tool-call
// validation passes failed. The runtime stops the loop rather than
// trying to coax a small model into emitting valid JSON forever.
var ErrTooManyValidationFailures = errors.New("agent: too many consecutive tool-call validation failures")

// ErrOscillationDetected is set when the model emits a byte-identical tool-call
// set for oscillationLimit+1 consecutive iterations — a weak model locked in a
// loop. The runtime stops iterating and forces a synthesis from results already
// gathered, rather than burning the whole loop ceiling. (TEN-261)
var ErrOscillationDetected = errors.New("agent: oscillation detected — identical tool call repeated")

// oscillationLimit is the number of consecutive identical tool-call iterations
// (after the first) that trips the oscillation guard. 2 → three identical
// iterations in a row before we stop and synthesize.
const oscillationLimit = 2

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
	// ConfirmSensitiveEgress reuses the host capability/approval broker for a
	// detected sensitive-result flow into an outbound tool. nil fails closed.
	ConfirmSensitiveEgress func(ctx context.Context, action, detail string) bool
	// Runs, when set, durably records each turn. Trace IDs are minted even
	// when no store is configured so callers can always correlate a result.
	Runs      *runtrace.Store
	Assembler *assemble.Assembler // optional; if nil, agent builds one from Router

	Logger      *slog.Logger
	PlannerRole model.Role // default "planner"
	// RouteHook, when set, observes the typed prompt out-of-band. Turn launches
	// it asynchronously immediately after EventTurnStart. In active mode the
	// hook may return a role for this turn; RouteActive gates the bounded join
	// so shadow callers retain phase one's fully asynchronous path.
	RouteHook    RouteHook
	RouteActive  RouteActive
	RouteSticky  RouteSticky
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

// RouteRequest is the agent-owned metadata passed to the optional route hook.
// It deliberately contains no classifier types, keeping agent independent of
// the route package.
type RouteRequest struct {
	AgentID       string
	SessionID     string
	UserQuery     string
	Surface       RouteSurface
	Override      RouteOverride
	Active        bool
	PlannerRole   model.Role
	PreviousRole  model.Role
	StickyRole    model.Role
	ContextTokens int
	ContextKnown  bool
}

// RouteObservation is the generic result of an out-of-band route hook. Text is
// suitable for a one-line operator event; Tags are persisted on the episode.
// Applied is set only when Role is an active, policy-approved selection.
type RouteObservation struct {
	Text          string
	Tags          []string
	Role          model.Role
	Applied       bool
	StickySession bool
	// ConstrainProfiles makes AllowedProfiles an exact ID->provider-kind
	// allowlist for the selected primary and its fallback chain. An empty map
	// then denies every routed profile.
	ConstrainProfiles    bool
	AllowedProfiles      map[string]string
	ExpectedProfileID    string
	ExpectedProviderKind string
	// EffortHint asks for a per-request ReasoningEffort variant of the routed
	// profile (TEN-292). Best-effort: an unbuildable variant silently keeps
	// the routed base profile.
	EffortHint string
}

// RouteHook observes one prompt. Agent invokes it in a goroutine and recovers
// hook panics, so routing remains an optional optimization.
type RouteHook func(context.Context, RouteRequest) RouteObservation

// RouteActive reports whether a request is eligible for active routing. It is
// intentionally separate from RouteHook so shadow mode never waits for a
// classifier. The cmd layer owns the rollout-stage and surface policy.
type RouteActive func(RouteRequest) bool

// RouteSticky snapshots the live sticky-session policy independently of a
// classifier result, so a timeout/failure can still pin the planner tier if
// that planner emits tool-call syntax.
type RouteSticky func(RouteRequest) bool

// RouteSurface identifies the caller driving a turn. Rollout policy decides
// which surfaces may actively route; Interactive is also used for prefixes.
type RouteSurface string

const (
	RouteSurfaceTUI       RouteSurface = "tui"
	RouteSurfaceDashboard RouteSurface = "dashboard"
	RouteSurfaceRelay     RouteSurface = "relay"
	RouteSurfaceCron      RouteSurface = "cron"
	RouteSurfacePeer      RouteSurface = "peer"
	RouteSurfaceResearch  RouteSurface = "research"
)

func (s RouteSurface) Interactive() bool {
	return s == RouteSurfaceTUI || s == RouteSurfaceDashboard
}

// RouteOverride is a one-turn operator clamp parsed from !big / !local.
type RouteOverride string

const (
	RouteOverrideBig   RouteOverride = "big"
	RouteOverrideLocal RouteOverride = "local"
)

// ParseRoutePrefix strips one supported one-shot prefix. A bare prefix leaves
// an empty query, which Turn rejects like any other empty user message.
func ParseRoutePrefix(query string) (string, RouteOverride) {
	trimmed := strings.TrimSpace(query)
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return query, ""
	}
	var override RouteOverride
	switch fields[0] {
	case "!big":
		override = RouteOverrideBig
	case "!local":
		override = RouteOverrideLocal
	default:
		return query, ""
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0])), override
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

	// warnedGates dedupes the unrecognized-capability-gate warning (one per
	// gate per agent lifetime). Mutated only from Turn (turns are serialized
	// per agent), like the compaction state below.
	warnedGates map[string]bool

	// compaction is the post-turn compaction trigger's hysteresis state
	// (TEN-102). Mutated only from Turn's post-turn defer (turns are serialized
	// per agent).
	compaction *hysteresis
	infoFlow   *infoFlowTracker

	// Active-route continuity is per Agent/SessionID. routeMu keeps diagnostic
	// tests and any accidentally concurrent callers race-free; production turns
	// are serialized by their owning surface.
	routeMu            sync.Mutex
	previousRoute      model.Role
	stickyRoute        model.Role
	routeContextTokens int
	routeContextBytes  int
	routeContextKnown  bool
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
	a := &Agent{
		cfg: cfg, log: log, plannerRole: pr, embedderRole: er,
		compaction: newCompactionHysteresis(), infoFlow: newInfoFlowTracker(),
	}
	a.router.Store(cfg.Router)
	return a, nil
}

// TurnRequest is one user-facing call into the agent.
type TurnRequest struct {
	UserQuery string
	// RouteSurface is empty for background/headless callers, which is always
	// shadow-only. TUI and dashboard set their explicit interactive surface.
	RouteSurface  RouteSurface
	RouteOverride RouteOverride
	// LoopCeiling overrides the profile's PlanLoopCeiling for THIS turn only.
	// 0 = inherit the profile default (normal turns; back-compat). >0 = cap
	// planner↔tool iterations at this value. <0 = unlimited (omit the per-turn
	// cap). Used by /goal runs so a long autonomous loop can iterate freely
	// without raising the global ceiling every normal turn shares (TEN-216).
	LoopCeiling int
}

// TurnQueueEntry is the observable, content-free view of one request waiting
// for a shared agent. Position is one-based in the order requests will be
// admitted; ETA is the scheduler's best estimate based on recent turn times.
// UserQuery is deliberately not exposed because dashboard status is an
// operational surface, not a transcript surface.
type TurnQueueEntry struct {
	Position int
	Source   RouteSurface
	Age      time.Duration
	ETA      time.Duration
}

// TurnQueueStatus is a point-in-time snapshot of shared-turn ingress. Depth
// excludes the currently active turn. OldestAge is zero for an empty queue.
type TurnQueueStatus struct {
	Depth     int
	OldestAge time.Duration
	Entries   []TurnQueueEntry
}

// TurnResult summarizes what happened during the turn.
type TurnResult struct {
	TraceID    string
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
func (a *Agent) Turn(ctx context.Context, req TurnRequest) (result *TurnResult, err error) {
	if req.RouteSurface.Interactive() {
		query, prefixOverride := ParseRoutePrefix(req.UserQuery)
		req.UserQuery = query
		if req.RouteOverride == "" {
			req.RouteOverride = prefixOverride
		}
	}
	if req.UserQuery == "" {
		return nil, errors.New("agent: TurnRequest.UserQuery required")
	}
	traceID := runtrace.NewID()
	span := runtrace.NewSpan(traceID)
	span.SetModel(string(a.plannerRole), "unresolved")
	ctx = runtrace.WithSpan(ctx, span)
	result = &TurnResult{TraceID: traceID}
	started := time.Now().UTC()
	source := string(req.RouteSurface)
	if source == "" {
		source = "headless"
	}
	run := runtrace.Run{
		TraceID: traceID, AgentID: a.cfg.AgentID, SessionID: a.cfg.SessionID,
		Source: source, StartedAt: started,
	}
	if a.cfg.Runs != nil {
		if startErr := a.cfg.Runs.Start(ctx, run); startErr != nil {
			a.log.Warn("agent: start run record failed", "trace_id", traceID, "err", startErr)
		}
		defer func() {
			completed := time.Now().UTC()
			run.CompletedAt = &completed
			run.LatencyMS = completed.Sub(started).Milliseconds()
			span.Apply(&run)
			switch {
			case err != nil:
				run.Outcome, run.ErrorClass = "error", runErrorClass(err)
			case result != nil && result.Error != nil:
				run.Outcome, run.ErrorClass = "error", runErrorClass(result.Error)
			case result != nil && result.Truncated:
				run.Outcome = "partial"
			default:
				run.Outcome = "success"
			}
			finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if finishErr := a.cfg.Runs.Finish(finishCtx, run); finishErr != nil {
				a.log.Warn("agent: finish run record failed", "trace_id", traceID, "err", finishErr)
			}
		}()
	}
	a.emit(Event{Kind: EventTurnStart, TraceID: traceID, Text: req.UserQuery})
	activeRouteDeadline := time.Now().Add(ActiveRouteBudget())
	var routeState *turnRouteState
	var routeDeadline time.Time
	var routeReq RouteRequest
	joinRoute := false
	routeStickySession := false
	if a.cfg.RouteHook != nil {
		previous, sticky, contextTokens, contextKnown := a.routeSnapshot(
			a.cfg.Working.Messages(), req.UserQuery,
		)
		routeReq = RouteRequest{
			AgentID: a.cfg.AgentID, SessionID: a.cfg.SessionID, UserQuery: req.UserQuery,
			Surface: req.RouteSurface, Override: req.RouteOverride,
			PlannerRole:  a.plannerRole,
			PreviousRole: previous, StickyRole: sticky,
			ContextTokens: contextTokens, ContextKnown: contextKnown,
		}
		joinRoute = a.routeActive(routeReq)
		routeReq.Active = joinRoute
		if joinRoute {
			routeDeadline = activeRouteDeadline
			routeStickySession = a.routeSticky(routeReq)
		}
		routeState = &turnRouteState{store: a.cfg.Episodic, log: a.log, ready: make(chan routeReady, 1)}
		routeCtx := context.WithoutCancel(ctx)
		cancelRoute := func() {}
		if joinRoute {
			routeCtx, cancelRoute = context.WithDeadline(routeCtx, routeDeadline)
		}
		go func() {
			defer cancelRoute()
			a.runRouteHook(routeCtx, routeReq, routeState)
		}()
	}

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

	// Active selection joins only after embedding, and only until the deadline
	// measured from EventTurnStart. Any timeout, hook failure, or routed-role
	// resolution error keeps the already-resolved planner path exactly intact.
	selectedRole := a.plannerRole
	if joinRoute && routeState != nil {
		if obs, ok := routeState.await(routeDeadline); ok {
			routeStickySession = routeStickySession || obs.StickySession
			if obs.Applied && obs.Role != "" {
				if routed, routedProfile, routeErr := resolveRoutedRole(ctx, router, obs, routeDeadline); routeErr != nil {
					a.log.Warn("agent: routed role unavailable; using planner",
						"agent", a.cfg.AgentID, "role", obs.Role, "err", routeErr)
					routeState.appendTags("route:routing_error=true")
				} else {
					planner, profile = routed, routedProfile
					selectedRole = obs.Role
				}
			}
		}
	}
	// Track the role actually used, including planner fallbacks caused by a
	// deadline, classifier/policy failure, or routed-role resolution error.
	a.noteRouteSelection(selectedRole)
	modelName := profile.Model
	if modelName == "" {
		modelName = profile.ID
	}
	provider := profile.ProviderKind
	if provider == "" {
		provider = profile.Backend
	}
	span.SetModel(modelName, provider)

	// 4. Resolve which tools to surface this turn (top-K per profile).
	availableTools, err := a.cfg.Tools.Search(ctx, queryEmbedding, profile.MaxToolsPerCall)
	if err != nil {
		return nil, fmt.Errorf("agent: tool search: %w", err)
	}
	// Capability gate (TEN-103): only offer tools the active model may use, so a
	// small local planner never sees an augmentation tool like memory_recall.
	availableTools, droppedGated := filterGatedTools(availableTools, profile)
	a.warnUnknownGateDrops(droppedGated, profile)
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
	// After the turn (any exit path), compact the working set if the working
	// tier crossed the hysteresis high-watermark — so the NEXT turn starts lean.
	// Best-effort and post-response, so it never blocks the answer. Hysteresis
	// (vs a single line) stops compaction re-firing every turn near the
	// threshold; the agent owns the state because the assembler is per-turn.
	defer func() {
		// result is a NAMED return (TEN-300 named it so the run-trace finish
		// defer can inspect the outcome). The error paths below do `return nil,
		// err`, which sets result to nil before defers run — so this compaction
		// pass MUST nil-check result first or it dereferences nil (a spawned
		// sub-agent whose planner/assemble errors would otherwise crash the app).
		if result == nil || a.cfg.Compactor == nil || ctx.Err() != nil || len(result.Reports) == 0 {
			return
		}
		last := result.Reports[len(result.Reports)-1]
		if a.compaction.shouldCompact(last.WorkingUsageFrac) {
			a.compactWorking(ctx)
		}
	}()
	consecutiveValidationFailures := 0
	// Oscillation guard (TEN-261): track the dispatched tool-call signature across
	// iterations so a weak model that locks onto the same call breaks out instead
	// of burning the whole ceiling. The operator's PlanLoopCeiling stays in force.
	var lastToolSig string
	repeatRuns := 0
	var stuckErr error
	overflowCompacted := false // one-shot emergency compaction on a mid-turn context overflow
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
		contextUpper := estimateAssembledContextUpper(assembled.Messages, iterTools)
		a.noteRouteContext(max(br.Total, contextUpper), estimateRouteContextBytes(a.cfg.Working.Messages()))
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
			// A mid-turn context overflow means the assembled prompt outgrew the
			// server's window. Accurate token budgeting should prevent this, but a
			// context length misconfigured above the server's real per-request
			// window, or a single oversized turn, can still hit it. Compact the
			// working set once and retry the iteration rather than killing the
			// turn; the one-shot guard stops an unrecoverable overflow from
			// looping. (R640 llama.cpp "Failed to tokenize prompt" turn deaths.)
			if errors.Is(err, model.ErrContextOverflow) && !overflowCompacted && a.cfg.Compactor != nil {
				overflowCompacted = true
				a.emit(Event{Kind: EventError, Iter: iter, Text: "context overflow — compacting and retrying: " + err.Error()})
				a.compactWorking(ctx)
				iter--
				continue
			}
			a.emit(Event{Kind: EventError, Iter: iter, Text: err.Error()})
			return nil, fmt.Errorf("agent: planner generate iter=%d: %w", iter, err)
		}
		if resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 {
			span.AddUsage(resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
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
		if joinRoute && routeStickySession && len(resp.ToolCalls) > 0 {
			a.pinRoute(selectedRole)
		}

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
					a.persistEpisode(ctx, req.UserQuery, final, result.ToolTrace, queryEmbedding, embedder, routeState)
					return result, nil
				}
				// Synthesis also empty — fall through to the original
				// behavior so we don't loop forever on a stuck model.
			}
			// Final response. Persist episode and return.
			result.Response = resp.Text
			a.emit(Event{Kind: EventFinal, Iter: iter, Text: resp.Text})
			a.persistEpisode(ctx, req.UserQuery, resp.Text, result.ToolTrace, queryEmbedding, embedder, routeState)
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
				span.AddTool(runtrace.ToolCall{ID: call.ID, Name: call.Name, Arguments: string(call.Arguments), Result: err.Error(), Outcome: "validation_error"})
				a.feedValidationError(ctx, call, err)
				continue
			}
			// Capability gate (TEN-103) — defense in depth: validation +
			// dispatch route by name against the FULL registry, so a model that
			// emits a gated tool it wasn't offered (hallucination, leakage) would
			// otherwise still reach the dispatcher. Refuse it here with a clean
			// tool error instead.
			if spec, ok := a.cfg.Tools.Get(call.Name); ok && spec.Gate != "" && !profile.AllowsTool(spec.Gate) {
				span.AddTool(runtrace.ToolCall{ID: call.ID, Name: call.Name, Arguments: string(call.Arguments), Outcome: "denied"})
				a.feedToolResult(ctx, ToolCallResult{
					Call:    call,
					Result:  fmt.Sprintf("%q is not available for the current model.", call.Name),
					IsError: true,
				}, profile.ToolResultCap())
				continue
			}
			if spec, ok := a.cfg.Tools.Get(call.Name); ok && spec.Outbound {
				if label, matched := a.infoFlow.match(call.Arguments); matched {
					detail := fmt.Sprintf("%s arguments contain sensitive data from %s; allow this outbound flow?", call.Name, label.Provenance)
					approved := a.cfg.ConfirmSensitiveEgress != nil &&
						a.cfg.ConfirmSensitiveEgress(ctx, "sensitive_egress", detail)
					if !approved {
						a.feedToolResult(ctx, ToolCallResult{
							Call: call, IsError: true,
							Result: fmt.Sprintf("blocked: %s arguments contain sensitive data from %s and outbound egress was not approved", call.Name, label.Provenance),
						}, profile.ToolResultCap())
						continue
					}
				}
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
			if spec, ok := a.cfg.Tools.Get(br.Call.Name); ok && br.Err == nil && !br.IsError {
				br.Labels = append([]model.InfoFlowLabel(nil), spec.ResultLabels...)
				a.infoFlow.record(br.Labels, br.Result)
			}
			result.ToolTrace = append(result.ToolTrace, br)
			toolOutcome := "success"
			if br.IsError || br.Err != nil {
				toolOutcome = "error"
			}
			toolResult := br.Result
			if br.Err != nil && toolResult == "" {
				toolResult = br.Err.Error()
			}
			span.AddTool(runtrace.ToolCall{ID: br.Call.ID, Name: br.Call.Name, Arguments: string(br.Call.Arguments), Result: toolResult, Outcome: toolOutcome})
			a.emit(Event{Kind: EventToolResult, Iter: iter, Tool: br.Call.Name, Result: br.Result, IsErr: br.IsError})
			a.feedToolResult(ctx, br, profile.ToolResultCap())
		}

		// Oscillation guard (TEN-261): if the dispatched call-set is byte-identical
		// to the previous iteration's for oscillationLimit consecutive rounds, the
		// model is stuck — stop and synthesize from what we have instead of looping
		// to the ceiling. Frontier-safe: a healthy model never repeats an identical
		// call-set this many times in a row. Operator PlanLoopCeiling is untouched.
		if sig := toolCallSignature(validCalls); sig != "" && sig == lastToolSig {
			if repeatRuns++; repeatRuns >= oscillationLimit {
				a.log.Warn("agent: oscillation — identical tool call repeated; forcing synthesis", "iter", iter, "identical_runs", repeatRuns+1)
				a.emit(Event{Kind: EventError, Iter: iter, Text: "stuck repeating the same tool call — synthesizing from gathered results"})
				stuckErr = ErrOscillationDetected
				break
			}
		} else {
			repeatRuns = 0
			lastToolSig = sig
		}
	}

	// 7. Loop ended without a final response — ceiling hit, or the oscillation
	// guard broke us out (stuckErr). Either way, force a synthesis from gathered
	// results; the reported error distinguishes the two causes.
	loopErr := ErrLoopCeilingReached
	if stuckErr != nil {
		loopErr = stuckErr
	}
	final, err := a.synthesizeFinal(ctx, planner, profile, asm, queryEmbedding, goalHeader)
	if err != nil {
		result.Error = fmt.Errorf("%w; forced synthesis also failed: %v", loopErr, err)
		a.emit(Event{Kind: EventError, Text: result.Error.Error()})
		return result, nil
	}
	result.Response = final
	result.Truncated = true
	result.Error = loopErr
	a.emit(Event{Kind: EventTruncated, Text: final})
	a.persistEpisode(ctx, req.UserQuery, final, result.ToolTrace, queryEmbedding, nil, routeState)
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
func filterGatedTools(specs []model.ToolSpec, profile model.Profile) (kept, dropped []model.ToolSpec) {
	kept = make([]model.ToolSpec, 0, len(specs))
	for _, s := range specs {
		if s.Gate != "" && !profile.AllowsTool(s.Gate) {
			dropped = append(dropped, s)
			continue
		}
		kept = append(kept, s)
	}
	return kept, dropped
}

// warnUnknownGateDrops makes deny-by-ignorance VISIBLE. Denying an
// unrecognized gate is the right posture (fail-safe); doing it silently is
// not - a hidden tool reads as "not available" to the model and as "it
// should be there, the code registers it" to the operator, the exact
// shape that cost days on the cron-surface starvation. A gate the profile
// KNOWS and denies (explicit capability, heuristic said no) is deliberate
// and stays quiet. Warned once per gate per agent - turns are serialized
// per agent, so the map needs no lock (same discipline as compaction).
func (a *Agent) warnUnknownGateDrops(dropped []model.ToolSpec, profile model.Profile) {
	for _, s := range dropped {
		if profile.GateKnown(s.Gate) || a.warnedGates[s.Gate] {
			continue
		}
		if a.warnedGates == nil {
			a.warnedGates = map[string]bool{}
		}
		a.warnedGates[s.Gate] = true
		a.log.Warn("tool hidden: the active profile has no Capabilities entry for its gate and no built-in heuristic covers it - if no other profile configures this gate either, the tool will never surface anywhere",
			"gate", s.Gate, "tool", s.Name, "profile", string(profile.ID))
		a.emit(Event{Kind: EventToolCatalog,
			Text: fmt.Sprintf("tool %s hidden: capability gate %q is unrecognized by the active profile (no Capabilities entry, no heuristic)", s.Name, s.Gate)})
	}
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
	if span := runtrace.SpanFromContext(ctx); span != nil {
		span.AddUsage(resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
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
		if span := runtrace.SpanFromContext(ctx); span != nil {
			span.AddRetry()
		}
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

func (a *Agent) feedToolResult(ctx context.Context, r ToolCallResult, maxResultTokens int) {
	content := r.Result
	if r.Err != nil {
		content = fmt.Sprintf("tool dispatch error: %s", r.Err.Error())
	}
	now := time.Now().UTC()
	// Cap the copy that re-enters the model's CONTEXT (working set) so a large
	// tool result — a 20K web_read, a wide SQL dump — doesn't dilute a small
	// model's attention or blow the working budget. The archive below always
	// keeps the FULL body, so nothing durable is lost (microcompaction and
	// recall read the archive, not this capped copy).
	a.cfg.Working.Append(working.Message{
		Role:       "tool",
		ToolCallID: r.Call.ID,
		Content:    capToolResultForContext(content, maxResultTokens),
		Timestamp:  now,
		Meta:       infoFlowMeta(r.Labels),
	})
	a.archiveEvent(ctx, archive.Event{
		Timestamp: now, AgentID: a.cfg.AgentID, SessionID: a.cfg.SessionID,
		Role: "tool",
		ToolResult: &archive.ToolResult{
			CallID:  r.Call.ID,
			Content: content, // full, untruncated — durable history
			IsError: r.IsError || r.Err != nil,
			Labels:  append([]model.InfoFlowLabel(nil), r.Labels...),
		},
	})
}

func infoFlowMeta(labels []model.InfoFlowLabel) map[string]any {
	if len(labels) == 0 {
		return nil
	}
	return map[string]any{"infoflow_labels": append([]model.InfoFlowLabel(nil), labels...)}
}

// capToolResultForContext truncates a tool result to roughly maxTokens before it
// re-enters the model's context (working set). maxTokens <= 0 disables the cap
// (frontier path: content returned unchanged). The cut is scrubbed to a valid
// UTF-8 boundary and carries an explicit marker, so the model knows the result
// was truncated and can refine the query or request a specific section. The full
// result is preserved in the archive by the caller.
func capToolResultForContext(content string, maxTokens int) string {
	if maxTokens <= 0 {
		return content
	}
	const charsPerToken = 4 // coarse heuristic — a safety cap, not a tokenizer
	maxChars := maxTokens * charsPerToken
	if len(content) <= maxChars {
		return content
	}
	// Slicing can split a multi-byte rune; ToValidUTF8 drops the partial tail so
	// we never emit invalid UTF-8 into the prompt.
	head := strings.ToValidUTF8(content[:maxChars], "")
	return fmt.Sprintf("%s\n…[truncated: tool result exceeded the %d-token context cap (%d of %d bytes shown). Refine the query or request a specific section to see the rest.]",
		head, maxTokens, len(head), len(content))
}

// toolCallSignature builds a stable, order-independent signature of a dispatched
// tool-call set — each call's name + raw arguments, sorted and joined. The
// oscillation guard compares it across iterations to detect a model repeating
// the identical call-set. Empty string for no calls.
func toolCallSignature(calls []model.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	parts := make([]string, len(calls))
	for i, c := range calls {
		parts[i] = c.Name + ":" + string(c.Arguments)
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
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
func (a *Agent) persistEpisode(ctx context.Context, prompt, response string, trace []ToolCallResult, queryEmbedding []float32, embedder model.Embedder, routeState *turnRouteState) {
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
	var routeTags []string
	var routeVersion uint64
	if routeState != nil {
		routeTags, routeVersion = routeState.snapshot()
	}
	id, err := a.cfg.Episodic.Insert(ctx, &episodic.Episode{
		AgentID:     a.cfg.AgentID,
		Visibility:  visibility,
		SessionID:   a.cfg.SessionID,
		SourceRunID: runtrace.IDFromContext(ctx),
		Prompt:      prompt,
		Response:    response,
		ToolCalls:   toolCalls,
		Outcome:     outcome,
		Tags:        routeTags,
		EmbedderID:  string(embedderID),
		Embedding:   queryEmbedding,
	})
	if err != nil {
		a.log.Warn("agent: episode insert failed", "err", err)
	} else if routeState != nil {
		routeState.episodeInserted(id, routeVersion)
	}
	_ = embedder // reserved for future per-content embedding (response embedding)
}

func runErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, ErrLoopCeilingReached):
		return "loop_ceiling"
	case errors.Is(err, ErrTooManyValidationFailures):
		return "validation_failures"
	case errors.Is(err, ErrOscillationDetected):
		return "oscillation"
	default:
		return "runtime_error"
	}
}

// turnRouteState joins a fire-and-forget observation to an episode without
// waiting in Turn. If the observation wins the race, its tags go into Insert;
// if the episode wins, the hook appends them afterward on a background path.
type turnRouteState struct {
	mu        sync.Mutex
	tags      []string
	version   uint64
	episodeID int64
	store     *episodic.Store
	log       *slog.Logger
	ready     chan routeReady
}

type routeReady struct {
	obs RouteObservation
	at  time.Time
}

func (a *Agent) routeSnapshot(msgs []working.Message, query string) (model.Role, model.Role, int, bool) {
	a.routeMu.Lock()
	defer a.routeMu.Unlock()
	previous := a.previousRoute
	if previous == "" {
		previous = a.plannerRole
	}
	if !a.routeContextKnown {
		return previous, a.stickyRoute, estimateRouteContext(msgs, query), false
	}
	currentBytes := estimateRouteContextBytes(msgs) + len(query)
	tokens := a.routeContextTokens
	if delta := currentBytes - a.routeContextBytes; delta > 0 {
		// A byte is a conservative upper bound for one tokenizer token. Add
		// every byte introduced since the last real assembler budget report.
		tokens += delta
	}
	return previous, a.stickyRoute, tokens, true
}

func (a *Agent) noteRouteSelection(role model.Role) {
	if role == "" {
		return
	}
	a.routeMu.Lock()
	a.previousRoute = role
	a.routeMu.Unlock()
}

func (a *Agent) pinRoute(role model.Role) {
	if role == "" {
		return
	}
	a.routeMu.Lock()
	if a.stickyRoute == "" {
		a.stickyRoute = role
	}
	a.routeMu.Unlock()
}

func (a *Agent) noteRouteContext(tokens, workingBytes int) {
	if tokens <= 0 {
		return
	}
	a.routeMu.Lock()
	a.routeContextTokens = tokens
	a.routeContextBytes = workingBytes
	a.routeContextKnown = true
	a.routeMu.Unlock()
}

func (a *Agent) routeActive(req RouteRequest) (active bool) {
	if a.cfg.RouteActive == nil {
		return false
	}
	defer func() {
		if v := recover(); v != nil {
			a.log.Warn("agent: route active gate panicked; using shadow", "agent", a.cfg.AgentID, "panic", v)
			active = false
		}
	}()
	return a.cfg.RouteActive(req)
}

func (a *Agent) routeSticky(req RouteRequest) (sticky bool) {
	if a.cfg.RouteSticky == nil {
		return false
	}
	defer func() {
		if v := recover(); v != nil {
			a.log.Warn("agent: route sticky gate panicked; not pinning", "agent", a.cfg.AgentID, "panic", v)
			sticky = false
		}
	}()
	return a.cfg.RouteSticky(req)
}

func estimateRouteContext(msgs []working.Message, query string) int {
	n := estimateRouteContextBytes(msgs) + len(query)
	if n == 0 {
		return 0
	}
	return (n + 3) / 4
}

func estimateRouteContextBytes(msgs []working.Message) int {
	n := 0
	for _, msg := range msgs {
		n += len(msg.Role) + len(msg.Content)
		for _, call := range msg.ToolCalls {
			n += len(call.Name) + len(call.Arguments)
		}
	}
	return n
}

// estimateAssembledContextUpper is a conservative token upper bound for the
// exact prompt that just ran. Modern tokenizers cannot emit more tokens than
// the UTF-8 bytes they encode; the fixed per-item allowance covers chat/JSON
// framing that is not present in the field strings themselves. Retaining this
// measured bound lets a later big->small transition prove the smaller
// OperationalContextBudget fits instead of trusting chars/4.
func estimateAssembledContextUpper(messages []model.Message, tools []model.ToolSpec) int {
	const framingBytes = 64
	n := framingBytes
	for _, msg := range messages {
		n += framingBytes + len(msg.Role) + len(msg.Content) + len(msg.ToolCallID) + len(msg.Name)
		for _, call := range msg.ToolCalls {
			n += framingBytes + len(call.ID) + len(call.Name) + len(call.Arguments)
		}
	}
	for _, tool := range tools {
		n += framingBytes + len(tool.Name) + len(tool.Description) + len(tool.Parameters)
	}
	return n
}

type routedRoleResult struct {
	llm     model.LLM
	profile model.Profile
	err     error
	at      time.Time
}

func resolveRoutedRole(parent context.Context, router *model.Router, obs RouteObservation, deadline time.Time) (model.LLM, model.Profile, error) {
	if router == nil {
		return nil, model.Profile{}, errors.New("model router unavailable")
	}
	if !time.Now().Before(deadline) {
		return nil, model.Profile{}, context.DeadlineExceeded
	}
	resolveCtx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	result := make(chan routedRoleResult, 1)
	go func() {
		got := routedRoleResult{}
		defer func() {
			if v := recover(); v != nil {
				got.err = fmt.Errorf("routed role resolution panic: %v", v)
			}
			got.at = time.Now()
			result <- got
		}()
		var allow model.ProfileFilter
		if obs.ConstrainProfiles {
			allow = func(p model.Profile) bool {
				kind, ok := obs.AllowedProfiles[p.ID]
				return ok && kind == p.ProviderKind
			}
		}
		llm, profile, err := router.LLMForRoleFiltered(resolveCtx, obs.Role, allow)
		if err == nil && obs.ExpectedProfileID != "" && profile.ID != obs.ExpectedProfileID {
			err = fmt.Errorf("routed profile changed from %q to %q", obs.ExpectedProfileID, profile.ID)
		}
		if err == nil && obs.ExpectedProviderKind != "" && profile.ProviderKind != obs.ExpectedProviderKind {
			err = fmt.Errorf("routed provider changed from %q to %q", obs.ExpectedProviderKind, profile.ProviderKind)
		}
		// EffortHint (TEN-292): applied only after the identity checks pass, so
		// the variant can never mask a changed binding. Best-effort — a variant
		// that fails to build keeps the validated base LLM, and the returned
		// wrapper falls back to that same base LLM (with its TEN-246 chain) at
		// generation time.
		if err == nil && obs.EffortHint != "" {
			if vllm, ok := router.LLMWithEffort(resolveCtx, profile, llm, obs.EffortHint); ok {
				llm = vllm
				profile.ReasoningEffort = obs.EffortHint
			}
		}
		got.llm, got.profile, got.err = llm, profile, err
	}()
	select {
	case got := <-result:
		if got.at.After(deadline) {
			return nil, model.Profile{}, context.DeadlineExceeded
		}
		return got.llm, got.profile, got.err
	case <-resolveCtx.Done():
		return nil, model.Profile{}, resolveCtx.Err()
	}
}

func (a *Agent) runRouteHook(ctx context.Context, req RouteRequest, state *turnRouteState) {
	var obs RouteObservation
	defer func() {
		if v := recover(); v != nil {
			a.log.Warn("agent: route hook panicked; ignored", "agent", a.cfg.AgentID, "panic", v)
		}
		state.publish(obs)
		if strings.TrimSpace(obs.Text) != "" {
			a.emit(Event{Kind: EventRoute, Text: obs.Text})
		}
	}()
	obs = a.cfg.RouteHook(ctx, req)
}

func (s *turnRouteState) publish(obs RouteObservation) {
	tags := compactTags(obs.Tags)
	s.mu.Lock()
	s.tags = tags
	s.version++
	id := s.episodeID
	s.mu.Unlock()
	if s.ready != nil {
		select {
		case s.ready <- routeReady{obs: obs, at: time.Now()}:
		default:
		}
	}
	if id != 0 && len(tags) > 0 {
		s.addTagsAsync(id, tags)
	}
}

func (s *turnRouteState) appendTags(tags ...string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.tags = compactTags(append(s.tags, tags...))
	s.version++
	id := s.episodeID
	all := append([]string(nil), s.tags...)
	s.mu.Unlock()
	if id != 0 {
		s.addTagsAsync(id, all)
	}
}

func (s *turnRouteState) await(deadline time.Time) (RouteObservation, bool) {
	if s == nil || s.ready == nil {
		return RouteObservation{}, false
	}
	wait := time.Until(deadline)
	if wait <= 0 {
		select {
		case ready := <-s.ready:
			return ready.obs, !ready.at.After(deadline)
		default:
			return RouteObservation{}, false
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case ready := <-s.ready:
		return ready.obs, !ready.at.After(deadline)
	case <-timer.C:
		return RouteObservation{}, false
	}
}

func (s *turnRouteState) snapshot() ([]string, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tags...), s.version
}

func (s *turnRouteState) episodeInserted(id int64, includedVersion uint64) {
	s.mu.Lock()
	s.episodeID = id
	tags := append([]string(nil), s.tags...)
	late := s.version > includedVersion
	s.mu.Unlock()
	if late && len(tags) > 0 {
		s.addTagsAsync(id, tags)
	}
}

func (s *turnRouteState) addTagsAsync(id int64, tags []string) {
	if s.store == nil || id == 0 || len(tags) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.store.AddTags(ctx, id, tags); err != nil && s.log != nil {
			s.log.Warn("agent: append route episode tags failed", "episode", id, "err", err)
		}
	}()
}

func compactTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out
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
