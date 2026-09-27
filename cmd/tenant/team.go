package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/logs"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/skills"
	"tenant/internal/memory/soul"
	"tenant/internal/memory/userprofile"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/orchestra"
	"tenant/internal/plugins/web"
	"tenant/internal/tui"
)

// member is one spawned sub-agent's bookkeeping.
type member struct {
	id, role, task string
	status         string // running | done | error
	result         string
	// flow is the sensitive provenance result quotes: what this agent read
	// that its answer repeats (TEN-336). Reported back by team_await.
	flow agent.Flow
	done chan struct{}
	// cancel lets the orchestrator preempt a STILL-RUNNING agent. The agent's
	// loop notices ctx.Err() at the next iteration boundary and triggers the
	// salvage path (synthesizeFinal on a fresh ctx) — so a wave-timeout in
	// /research extracts a partial-but-useful report from the agent's working
	// memory instead of abandoning it with "(no result)."
	cancel context.CancelFunc
}

// TeamConfig wires a TeamRuntime with the shared infrastructure every
// sub-agent receives — so a spawned agent has the FULL workability of the
// main agent: the shared plugin tools PLUS its own per-agent comms/memory/
// skill tools, retrieval, context compaction, and the user profile. Each
// agent keeps its OWN identity (role soul) + private memory scope, while
// sharing the plugin tools, the shared-visibility memory tier, and the bus.
type TeamConfig struct {
	Bus *orchestra.Bus
	// BaseCtx, if set, is the lifetime a spawned sub-agent inherits instead of
	// the caller's context. Under serve/TUI the caller is a turn that stays
	// open until the orchestrator awaits, so inheriting it is right. Over the
	// MCP bridge the caller is a single JSON-RPC request that ends the moment
	// spawn_agent returns — sub-agents were dying mid-thought ("embed query
	// failed: context canceled") and every await reported "(no result)".
	BaseCtx    context.Context
	Router     *model.Router
	Stores     *stores
	Shared     *toolMux             // shared plugin tools (agent-id-independent)
	Skills     *skills.Store        // shared skill library (may be nil)
	Embedder   model.Embedder       // for per-agent memory writes (may be nil)
	EmbedderID string               //
	Compressor agent.Compactor      // shared context compressor (may be nil)
	Profile    *userprofile.Profile // shared user model, injected each turn (may be nil)
	OrchID     string
	Log        *slog.Logger
	// MaxConcurrent bounds simultaneously-running sub-agents. <=0 uses
	// defaultMaxConcurrentAgents. This is a safety bound, not a tuning knob:
	// unlimited spawning is how a single question turned into ~30 contradictory
	// workers and a wedged turn.
	MaxConcurrent int
	Observe       func(agentID string, e agent.Event) // live view hook (may be nil)
	// ConfirmSensitiveEgress is the existing origin-aware approval callback.
	// nil makes sensitive outbound flows fail closed.
	ConfirmSensitiveEgress func(context.Context, string, string) bool
	Budgets                orchestra.Budgets // zero fields use safe defaults

	// Web, when true, gives each agent its OWN lazy browser session
	// (isolated, so concurrent navigation can't clobber a shared tab).
	Web       bool
	WebConfig web.Config
	WebPolicy web.Policy
	Shots     string // screenshot dir

	// AgentProfiles is the named-sub-agent registry: when Spawn's role
	// matches a name here, the spawned agent runs on the profile's pinned
	// model + identity instead of inheriting the orchestrator's defaults.
	// nil/empty = no profiles (legacy behavior: every spawn shares the
	// orchestrator's router).
	AgentProfiles map[string]*agentProfile
	// SpawnRole, when set, classifies a spawn task (TEN-292 N-way rules) and
	// suggests a planner role for UNPINNED spawns only — spawns matching a
	// pinned AgentProfile always bypass routing. Best-effort: (_, false)
	// keeps today's exact spawn behavior.
	SpawnRole func(task string) (model.Role, bool)
	// CfgDir is needed when (lazily) building a per-profile router so we can
	// resolve provider config + secrets. Required if AgentProfiles is set.
	CfgDir string
	// EmbedProfile is the shared embedder profile (Ollama nomic-embed-text
	// or whatever the operator configured) — adopted into each per-profile
	// router so sub-agents share the embedding space.
	EmbedProfile model.Profile

	// LocalRestore + LocalOnChange wire persistence into the per-agent
	// local mux for the ORCHESTRATOR agent only. Without these,
	// /enable/disable on a local-only tool (e.g. web_*) reverts every
	// launch because the local mux is rebuilt fresh. Pass the same
	// settings-file callback used for the shared mux so /enable
	// decisions survive restarts regardless of which side owns the tool.
	LocalRestore  map[string]bool
	LocalOnChange func(map[string]bool)
}

// TeamRuntime spawns and tracks sub-agents that run concurrently and talk
// over the orchestra Bus. The orchestrator decides what to spawn at runtime
// (the spawn_agent tool).
type TeamRuntime struct {
	cfg    TeamConfig
	budget *orchestra.Governor

	mu     sync.Mutex
	order  []string
	agents map[string]*member
	seq    int

	cleanMu  sync.Mutex
	cleanups []func() // per-agent browser sessions etc., closed at shutdown

	// routerMu guards profileRouters: profile-specific routers are built on
	// first spawn of that profile and cached so subsequent spawns reuse the
	// same connection pool. SetAgentProfiles flushes the cache.
	routerMu       sync.Mutex
	profileRouters map[string]*model.Router
}

func newTeamRuntime(cfg TeamConfig) *TeamRuntime {
	r := &TeamRuntime{
		cfg:            cfg,
		agents:         map[string]*member{},
		profileRouters: map[string]*model.Router{},
	}
	// Budget observer, DAMPENED: the governor notifies on every acquire
	// (including each tool call), which flooded the activity feed with one
	// "team budget" line per tool call. Forward a status only when something an
	// operator acts on changed — agent/browser counts, a termination reason —
	// or every 10s as a heartbeat while a run is active. Dashboards needing
	// exact live numbers read BudgetStatus() directly.
	var (
		obsMu      sync.Mutex
		lastEmit   time.Time
		lastAgents int
		lastBrowse int
		lastTerm   string
	)
	r.budget = orchestra.NewGovernor(cfg.Budgets, func(st orchestra.BudgetStatus) {
		if r.cfg.Observe == nil {
			return
		}
		obsMu.Lock()
		significant := st.TerminationReason != lastTerm ||
			st.ActiveSubagents != lastAgents || st.BrowserInstances != lastBrowse ||
			time.Since(lastEmit) >= 10*time.Second
		if significant {
			lastEmit, lastAgents, lastBrowse, lastTerm = time.Now(), st.ActiveSubagents, st.BrowserInstances, st.TerminationReason
		}
		obsMu.Unlock()
		if !significant {
			return
		}
		r.cfg.Observe(r.cfg.OrchID, agent.Event{Kind: agent.EventTeamBudget, Text: st.String(), TeamBudget: &st})
	})
	return r
}

// SetAgentProfiles replaces the registry of named sub-agent profiles and
// invalidates the cached per-profile routers. Used by the /agents control
// to apply edits live (next spawn picks up the new model/identity).
func (r *TeamRuntime) SetAgentProfiles(profiles map[string]*agentProfile) {
	r.routerMu.Lock()
	r.cfg.AgentProfiles = profiles
	r.profileRouters = map[string]*model.Router{}
	r.routerMu.Unlock()
}

// AgentProfiles returns a snapshot of the current registry for display
// (/agents listing). Returns a fresh map — safe for callers to iterate.
func (r *TeamRuntime) AgentProfiles() map[string]*agentProfile {
	r.routerMu.Lock()
	defer r.routerMu.Unlock()
	out := make(map[string]*agentProfile, len(r.cfg.AgentProfiles))
	for k, v := range r.cfg.AgentProfiles {
		out[k] = v
	}
	return out
}

// routerForProfile returns the per-profile router (building + caching on
// first call). Returns the SHARED orchestrator router when the role doesn't
// match any profile — that's the fallback path for unnamed spawns.
// On a build error, logs + falls back to the shared router so a misconfigured
// profile doesn't kill the spawn — the agent runs (on the wrong model) and
// surfaces a visible warning.
func (r *TeamRuntime) routerForProfile(role string) (*model.Router, *agentProfile) {
	r.routerMu.Lock()
	ap := r.cfg.AgentProfiles[role]
	if ap == nil {
		// Case-insensitive fallback: the orchestrator is a model and may emit
		// "programmer" for the "Programmer" persona — still fire it (TEN-132).
		for name, p := range r.cfg.AgentProfiles {
			if strings.EqualFold(name, role) {
				ap = p
				break
			}
		}
	}
	if ap == nil {
		r.routerMu.Unlock()
		return r.cfg.Router, nil
	}
	// A profile with no pinned provider (the built-in specialists) inherits the
	// orchestrator's primary router — its value is the soul, not a model swap.
	// Return before the cache/build path so we never hit buildProfileRouter's
	// "unknown provider" error or cache the shared router under the role.
	if ap.Provider == "" {
		r.routerMu.Unlock()
		return r.cfg.Router, ap
	}
	if cached := r.profileRouters[role]; cached != nil {
		r.routerMu.Unlock()
		return cached, ap
	}
	r.routerMu.Unlock()

	// Build outside the lock — router construction does network probes (token
	// estimation, etc.) and shouldn't block other spawns.
	lc, err := loadLaunchConfig(r.cfg.CfgDir)
	if err != nil {
		if r.cfg.Log != nil {
			r.cfg.Log.Warn("team: load config for profile router; using shared router", "profile", role, "err", err)
		}
		return r.cfg.Router, ap
	}
	pr, err := buildProfileRouter(role, ap, lc, r.cfg.CfgDir, r.cfg.EmbedProfile, r.cfg.Log)
	if err != nil {
		if r.cfg.Log != nil {
			r.cfg.Log.Warn("team: build profile router; using shared router", "profile", role, "err", err)
		}
		return r.cfg.Router, ap
	}
	// Cache. Double-check under the lock in case a parallel call beat us.
	r.routerMu.Lock()
	if existing := r.profileRouters[role]; existing != nil {
		r.routerMu.Unlock()
		return existing, ap
	}
	r.profileRouters[role] = pr
	r.routerMu.Unlock()
	return pr, ap
}

// spawnRoleSafely guards the spawn classifier with the same panic isolation
// the turn hook gets: a broken classifier must never kill a spawn.
func spawnRoleSafely(fn func(string) (model.Role, bool), task string) (role model.Role, ok bool) {
	defer func() {
		if v := recover(); v != nil {
			logs.For("subagents").Error("team: spawn classifier panicked; using the planner role", "panic", v, "stack", string(debug.Stack()))
			role, ok = "", false
		}
	}()
	return fn(task)
}

// addCleanup registers a teardown (e.g. a per-agent browser session) to run
// at Close. Thread-safe — lazy sessions are created from agent goroutines.
func (r *TeamRuntime) addCleanup(fn func()) {
	r.cleanMu.Lock()
	r.cleanups = append(r.cleanups, fn)
	r.cleanMu.Unlock()
}

// Close tears down everything the team created (per-agent browser
// sessions), in reverse order. Call once at the end of a run.
func (r *TeamRuntime) Close() {
	r.budget.End()
	r.cleanMu.Lock()
	cs := r.cleanups
	r.cleanups = nil
	r.cleanMu.Unlock()
	for i := len(cs) - 1; i >= 0; i-- {
		cs[i]()
	}
}

// addWebTool gives a local tool set its own lazy browser, if the team is
// web-enabled. Shared between sub-agents (agentTools) and the orchestrator.
func (r *TeamRuntime) addWebTool(local *toolMux, agentID string) {
	if !r.cfg.Web {
		return
	}
	cfg := r.cfg.WebConfig
	upstream := cfg.ReaderObserver
	cfg.ReaderObserver = func(a web.ReaderActivity) {
		if upstream != nil {
			upstream(a)
		}
		r.emit(agentID, readerActivityEvent(a))
	}
	local.Add("web", newLazyWeb(cfg, r.cfg.WebPolicy, r.cfg.Shots, r.addCleanup, r.budget.AcquireBrowser, r.cfg.Log))
}

// composite merges a shared tool set (plugins) with an agent's local tools
// (comms, memory, skills, and for the orchestrator spawn/await). Local
// tools are always surfaced; anything not local falls through to shared.
// Implements both agent.ToolRegistry and agent.ToolDispatcher.
type composite struct {
	shared  *toolMux
	local   *toolMux
	budget  *orchestra.Governor
	agentID string
}

// Compile-time guard: the composite MUST satisfy agent.RankingReporter, else the
// per-turn ranking diagnostic silently falls through for the TUI/orchestrate
// paths (which hand the agent a composite, not a bare mux) — TEN-225.
var _ agent.RankingReporter = composite{}

// SetMCPAuthNotifier installs the connector auth-change notifier on both
// muxes: remote MCP connectors register on whichever mux built them, and the
// callback is read at fire time, so forwarding to both is exact.
func (c composite) SetMCPAuthNotifier(fn func(label, url string, needsReauth bool, err string)) {
	for _, m := range []*toolMux{c.shared, c.local} {
		if m != nil {
			m.SetMCPAuthNotifier(fn)
		}
	}
}

// RemoteMCPAuthProblems unions the dead connectors of both muxes (a label can
// only live on one).
func (c composite) RemoteMCPAuthProblems() []mcpServerStatus {
	var out []mcpServerStatus
	seen := map[string]bool{}
	for _, m := range []*toolMux{c.shared, c.local} {
		if m == nil {
			continue
		}
		for _, s := range m.RemoteMCPAuthProblems() {
			if !seen[s.Label] {
				seen[s.Label] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func (c composite) Get(name string) (model.ToolSpec, bool) {
	if s, ok := c.local.Get(name); ok {
		return s, true
	}
	if c.shared != nil {
		return c.shared.Get(name)
	}
	return model.ToolSpec{}, false
}

// Search composes the sub-agent's per-spawn local toolbelt with the
// shared mux. INTENTIONAL ASYMMETRY: local tools always surface in full
// (c.local.All()) because they are the sub-agent's specialized, hand-
// selected set — a sub-agent spawned with one specific tool to do one
// specific job must never have that tool ranked-out beneath it. The
// shared mux may apply embedding-based ranking when its catalog crosses
// the activation threshold (see toolmux.go Search docs); we want that
// trimming on the GENERAL catalog, not the SPECIALIZED one. Keep this
// asymmetry — if it ever needs to change, document why in this comment.
func (c composite) Search(ctx context.Context, emb []float32, k int) ([]model.ToolSpec, error) {
	out := c.local.All()
	if c.shared != nil {
		s, err := c.shared.Search(ctx, emb, k)
		if err != nil {
			return nil, err
		}
		out = append(out, s...)
	}
	return out, nil
}

func (c composite) All() []model.ToolSpec {
	out := c.local.All()
	if c.shared != nil {
		out = append(out, c.shared.All()...)
	}
	return out
}

func (c composite) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if c.budget != nil {
		if call.Name == "spawn_agent" {
			c.budget.Ensure(ctx)
		}
		if err := c.budget.AcquireToolCall(c.agentID); err != nil {
			return err.Error(), true, nil
		}
	}
	if _, ok := c.local.Get(call.Name); ok {
		return c.local.Dispatch(ctx, call)
	}
	if c.shared != nil {
		return c.shared.Dispatch(ctx, call)
	}
	return "unknown tool: " + call.Name, true, nil
}

// --- tui.ToolControl (so /tools + /enable work over both layers) ---

func (c composite) ToolList() []tui.ToolInfo {
	out := c.local.ToolList()
	if c.shared != nil {
		out = append(out, c.shared.ToolList()...)
	}
	return out
}

func (c composite) SetEnabled(target string, on bool) (int, string, error) {
	if n, scope, err := c.local.SetEnabled(target, on); n > 0 || err != nil {
		return n, scope, err
	}
	if c.shared != nil {
		return c.shared.SetEnabled(target, on)
	}
	return 0, "", nil
}

// SetPluginEnabled applies the same local-first-then-shared delegation
// as SetEnabled. Both layers may contribute matches (e.g. when a plugin
// is registered on both sides); we sum counts so the user sees the
// whole-cluster effect of the toggle.
func (c composite) SetPluginEnabled(label string, on bool) (int, string, error) {
	nLocal, _, err := c.local.SetPluginEnabled(label, on)
	if err != nil {
		return nLocal, "plugin", err
	}
	nShared := 0
	if c.shared != nil {
		nShared, _, err = c.shared.SetPluginEnabled(label, on)
		if err != nil {
			return nLocal + nShared, "plugin", err
		}
	}
	total := nLocal + nShared
	scope := ""
	if total > 0 {
		scope = "plugin"
	}
	return total, scope, nil
}

// Plugins unions the plugin label sets across local + shared muxes.
// Sorted, deduped — drives the "did you mean" hint.
func (c composite) Plugins() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range c.local.Plugins() {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if c.shared != nil {
		for _, p := range c.shared.Plugins() {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// RankingStatus delegates to the SHARED mux — that's where the large plugin/MCP
// catalog lives and where cosine ranking actually runs (composite.Search ranks
// only the shared layer; local tools are always fully included). This makes the
// composite satisfy agent.RankingReporter so the per-turn diagnostic + /tools
// status see the real ranking decision (TEN-225). ok=false until the shared mux
// has run at least one Search.
func (c composite) RankingStatus() (ranked bool, surfaced, catalog int, reason string, ok bool) {
	if c.shared == nil {
		return false, 0, 0, "", false
	}
	return c.shared.RankingStatus()
}

// Spawn creates a sub-agent for (role, task), starts it running in its own
// goroutine, and returns its id immediately (non-blocking — the team works
// concurrently). The sub-agent reports its result back to the orchestrator
// over the bus when it finishes.
// defaultMaxConcurrentAgents bounds simultaneous sub-agents when the operator
// has not chosen a limit. Sized to keep genuine fan-out useful (a handful of
// parallel workers) while making a runaway loop fail fast and visibly.
const defaultMaxConcurrentAgents = 4

// maxConcurrent is the live cap; MaxConcurrent <= 0 means "use the default".
func (r *TeamRuntime) maxConcurrent() int {
	if r.cfg.MaxConcurrent > 0 {
		return r.cfg.MaxConcurrent
	}
	return defaultMaxConcurrentAgents
}

func (r *TeamRuntime) Spawn(ctx context.Context, role, task string) (string, error) {
	role = strings.TrimSpace(role)
	task = strings.TrimSpace(task)
	if role == "" || task == "" {
		return "", fmt.Errorf("spawn needs both a role and a task")
	}
	// The team run outlives the spawning request when the runtime has a
	// long-lived base (TUI, serve); otherwise it lives as long as the request.
	base := ctx
	if r.cfg.BaseCtx != nil {
		base = r.cfg.BaseCtx
	}
	r.budget.Ensure(base)
	runCtx, releaseAgent, err := r.budget.AcquireAgent()
	if err != nil {
		return "", err
	}

	r.mu.Lock()
	// Bound concurrent sub-agents. Spawning was previously unlimited, and an
	// over-eager orchestrator used that to fan out ~30 workers onto one
	// question: each answered independently, they contradicted each other, and
	// the parent turn never converged (it also leaked a browser session per
	// agent). Refusing here — rather than trusting the prompt — keeps a runaway
	// fan-out from taking the agent down, and the error tells the orchestrator
	// to await what it already started.
	live := 0
	for _, m := range r.agents {
		if m.status == "running" {
			live++
		}
	}
	if max := r.maxConcurrent(); live >= max {
		r.mu.Unlock()
		releaseAgent() // hand the budget slot back, or every refusal leaks one
		return "", fmt.Errorf("refused: %d sub-agents are already running (limit %d) — await or collect the running ones before spawning more", live, max)
	}
	r.seq++
	id := fmt.Sprintf("%s-%s-%d", r.cfg.OrchID, slug(role), r.seq)
	// Per-agent cancellable ctx so the orchestrator can preempt a specific
	// stuck agent (wave-timeout salvage path) without cancelling the entire
	// research run.
	// Keep the spawning turn's exec surface, so the sub-agent runs os_exec
	// under its parent's sandbox profile even though its lifetime is the
	// team run's (runCtx), not the spawning request's.
	agCtx, cancel := context.WithCancel(inheritExecSurface(runCtx, ctx))
	m := &member{id: id, role: role, task: task, status: "running",
		done: make(chan struct{}), cancel: cancel}
	r.agents[id] = m
	r.order = append(r.order, id)
	r.mu.Unlock()

	r.cfg.Bus.Register(id)

	// Named agent profile lookup: if the role matches a registered profile,
	// build/cache a router pinned to its provider+model and graft its soul
	// markdown into the system prompt. Unmatched roles fall through to the
	// orchestrator's shared router + the role-stamped base soul (the legacy
	// behavior, unchanged).
	router, profile := r.routerForProfile(role)
	sl := roleSoul(r.cfg.Stores.soul, id, role)
	systemPrompt := memberPrompt(id, role, r.cfg.OrchID)
	if profile != nil {
		systemPrompt = profileSystemPrompt(id, role, r.cfg.OrchID, profile.Soul)
	}
	tools, closeTools := r.agentTools(id) // full plugin set + this agent's own tools

	// Spawn-time N-way routing (TEN-292): UNPINNED spawns only — a spawn that
	// matched a pinned AgentProfile (profile with a provider) keeps its pinned
	// router untouched. The suggested role must already resolve on the router
	// this spawn will use; anything else keeps today's planner role exactly.
	plannerRole := model.Role("")
	if r.cfg.SpawnRole != nil && (profile == nil || profile.Provider == "") {
		if suggested, ok := spawnRoleSafely(r.cfg.SpawnRole, task); ok && suggested != "" {
			if _, err := router.ForRole(suggested); err == nil {
				plannerRole = suggested
				if r.cfg.Log != nil {
					r.cfg.Log.Info("team: spawn routed by rule", "agent", id, "role", string(suggested))
				}
			} else if r.cfg.Log != nil {
				r.cfg.Log.Warn("team: spawn rule role unbound on spawn router; using planner",
					"agent", id, "role", string(suggested))
			}
		}
	}

	ag, err := agent.New(agent.Config{
		AgentID:                id,
		PlannerRole:            plannerRole,
		Router:                 router,
		Soul:                   sl,
		Working:                working.New(),
		Archive:                r.cfg.Stores.archive,
		Episodic:               r.cfg.Stores.episodic,
		Semantic:               r.cfg.Stores.semantic,
		Runs:                   r.cfg.Stores.runs,
		Tools:                  tools,
		Dispatcher:             tools,
		ConfirmSensitiveEgress: r.cfg.ConfirmSensitiveEgress,
		// A task that quoted the spawner's sensitive results carries that
		// provenance, so this agent's egress of it asks too (TEN-336).
		InboundFlow:  agent.DispatchFlow(ctx),
		Logger:       r.cfg.Log,
		SystemPrompt: systemPrompt,
		Observer:     func(e agent.Event) { r.emit(id, e) },
		Skills:       r.skillRetriever(id),
		Compactor:    r.cfg.Compressor,
		UserProfile:  r.cfg.Profile,
		// Sub-agents persist episodes as `shared` so the parent orchestrator's
		// retrieval can surface their work via the assembler's agent-glob filter
		// (TEN-45). Without this, the orchestrator's normal episodic retrieval
		// can't see what its own /research sub-agents produced — the proximate
		// cause of the 2026-05-26 lost-context bug (TEN-43).
		EpisodeVisibility: episodic.VisibilityShared,
	})
	if err != nil {
		closeTools()
		releaseAgent()
		r.finish(m, "error", "failed to start: "+err.Error())
		return "", err
	}

	go func() {
		defer cancel() // release the ctx when this agent finishes naturally
		defer releaseAgent()
		defer closeTools()
		res, terr := ag.Turn(agCtx, agent.TurnRequest{UserQuery: task})
		var status, result string
		switch {
		case context.Cause(agCtx) != nil && r.budget.Status().TerminationReason != "":
			status, result = "terminated", "terminated: "+r.budget.Status().TerminationReason
		case terr != nil:
			status, result = "error", "error: "+terr.Error()
		case res != nil && strings.TrimSpace(res.Response) != "":
			status, result = "done", res.Response
		default:
			status, result = "done", "(no result)"
		}
		// Broadcast completion to the WHOLE team (not just the orchestrator)
		// so a peer that's still working can see this result via team_check/
		// team_history. Send BEFORE finish: finish closes m.done, which is
		// what Await unblocks on — a send after it races the orchestrator's
		// post-await inbox read (the missed-DONE flake under -race).
		_ = r.cfg.Bus.Send(orchestra.Message{From: id,
			Content: "DONE — " + clip(result, 600)})
		flow := ag.FlowFor(result)
		r.mu.Lock()
		m.flow = flow
		r.mu.Unlock()
		r.finish(m, status, result)
	}()

	return id, nil
}

// agentTools builds an agent's FULL tool set: the shared plugin tools plus
// per-agent comms + memory (+ skills) bound to its id. This is what gives a
// sub-agent the same workability as the main agent. The agent-id-bound
// tools (memory_remember writes the agent's own facts; skill_save saves
// under its id) live in the local layer; the shared external-resource
// plugins (wiki/sql/web/os/…) are shared.
func (r *TeamRuntime) agentTools(id string) (composite, func()) {
	local := newToolMux()
	local.Add("team", orchestra.CommsTool{Bus: r.cfg.Bus, Self: id})
	local.Add("memory", memoryTool{sem: r.cfg.Stores.semantic, emb: r.cfg.Embedder, embedderID: r.cfg.EmbedderID, agentID: id})
	if r.cfg.Skills != nil {
		local.Add("skills", skillTool{st: r.cfg.Skills, emb: r.cfg.Embedder, agentID: id})
	}
	// This agent's OWN lazy browser session, with the per-agent reader-audit
	// observer (TEN-319) and the browser-instance budget (TEN-326). Cleanup is
	// captured explicitly for the sub-agent lifecycle rather than via addCleanup.
	closeTools := func() {}
	if r.cfg.Web {
		cfg := r.cfg.WebConfig
		upstream := cfg.ReaderObserver
		cfg.ReaderObserver = func(a web.ReaderActivity) {
			if upstream != nil {
				upstream(a)
			}
			r.emit(id, readerActivityEvent(a))
		}
		lw := newLazyWeb(cfg, r.cfg.WebPolicy, r.cfg.Shots, nil, r.budget.AcquireBrowser, r.cfg.Log)
		local.Add("web", lw)
		closeTools = lw.Close
	}
	// Orchestrator only: restore persisted local enable/disable state
	// AND wire the save hook so /enable web_search etc. survives a
	// restart. Sub-agents are ephemeral; their local state isn't
	// persisted by design.
	if id == r.cfg.OrchID {
		if len(r.cfg.LocalRestore) > 0 {
			_ = local.Restore(r.cfg.LocalRestore) // notes ignored — same set already in shared's restore log
		}
		if r.cfg.LocalOnChange != nil {
			local.SetOnChange(r.cfg.LocalOnChange)
		}
	}
	return composite{shared: r.cfg.Shared, local: local, budget: r.budget, agentID: id}, closeTools
}

func (r *TeamRuntime) skillRetriever(id string) agent.SkillRetriever {
	if r.cfg.Skills == nil {
		return nil
	}
	return skillRetriever{st: r.cfg.Skills, agentID: id}
}

// resultFlow joins the sensitive provenance of every member's result, for
// team_await to report to the orchestrator (TEN-336).
func (r *TeamRuntime) resultFlow() agent.Flow {
	r.mu.Lock()
	defer r.mu.Unlock()
	flows := make([]agent.Flow, 0, len(r.order))
	for _, id := range r.order {
		flows = append(flows, r.agents[id].flow)
	}
	return agent.JoinFlows(flows...)
}

func (r *TeamRuntime) finish(m *member, status, result string) {
	r.mu.Lock()
	m.status = status
	m.result = result
	r.mu.Unlock()
	close(m.done)
}

func (r *TeamRuntime) emit(id string, e agent.Event) {
	if e.Kind == agent.EventUsage {
		_ = r.budget.RecordUsage(e.PromptTokens, e.CompletionTokens)
	}
	if r.cfg.Observe != nil {
		r.cfg.Observe(id, e)
	}
}

// BeginRun opens an explicit team accounting window. Deep-research callers use
// this because they spawn directly rather than through the spawn_agent tool.
func (r *TeamRuntime) BeginRun(ctx context.Context) { r.budget.Begin(ctx) }

// EndRun closes the current accounting window after all children are joined.
func (r *TeamRuntime) EndRun(reason ...string) {
	if len(reason) > 0 && reason[0] != "" {
		r.budget.EndWithReason(reason[0])
		return
	}
	r.budget.End()
}

// BudgetStatus exposes active usage and the last termination reason to status
// surfaces without parsing human-readable event text.
func (r *TeamRuntime) BudgetStatus() orchestra.BudgetStatus { return r.budget.Status() }

// ObserveMain accounts the orchestrator's model usage while a team run is
// active, then forwards the event to the existing observer.
func (r *TeamRuntime) ObserveMain(e agent.Event, next func(agent.Event)) {
	if e.Kind == agent.EventUsage {
		_ = r.budget.RecordUsage(e.PromptTokens, e.CompletionTokens)
	}
	if next != nil {
		next(e)
	}
}

// Await blocks until every currently-running sub-agent finishes (or the
// timeout/ctx fires), then returns a synthesized summary of each agent's
// result for the orchestrator to fold into its final answer.
func (r *TeamRuntime) Await(ctx context.Context, timeout time.Duration) string {
	r.mu.Lock()
	waits := make([]*member, 0, len(r.order))
	for _, id := range r.order {
		if m := r.agents[id]; m.status == "running" {
			waits = append(waits, m)
		}
	}
	r.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	stopped := false
	for _, m := range waits {
		if stopped {
			break
		}
		select {
		case <-m.done:
		case <-timer.C:
			stopped = true
		case <-ctx.Done():
			stopped = true
		}
	}
	return r.summary(stopped)
}

// CancelStuck cancels every still-running agent's ctx and waits up to
// `grace` for them to finalize. The agent's loop notices the cancellation,
// triggers its salvage path (a short tools-off synthesize on a fresh ctx),
// and writes a real result into working memory before returning. This
// converts a wave-timeout from "(no result)" into a partial-but-useful
// report — the agent had been gathering page text the whole time, we just
// hadn't asked it to synthesize yet. Safe to call on a finished runtime
// (no-op). Returns the number of agents that were cancelled.
func (r *TeamRuntime) CancelStuck(grace time.Duration) int {
	r.mu.Lock()
	var stuck []*member
	for _, id := range r.order {
		if m := r.agents[id]; m.status == "running" && m.cancel != nil {
			stuck = append(stuck, m)
		}
	}
	r.mu.Unlock()
	for _, m := range stuck {
		m.cancel()
	}
	if len(stuck) == 0 || grace <= 0 {
		return len(stuck)
	}
	// Wait briefly for each agent's salvage to finish + finish() to fire.
	// Per-agent timer so a hopelessly stuck one can't block the rest of the
	// grace budget.
	deadline := time.Now().Add(grace)
	for _, m := range stuck {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		timer := time.NewTimer(remaining)
		select {
		case <-m.done:
		case <-timer.C:
		}
		timer.Stop()
	}
	return len(stuck)
}

// TeamResult is one sub-agent's outcome, for callers (e.g. deep research) that
// need each agent's report separately rather than the concatenated summary.
type TeamResult struct {
	ID, Role, Task, Status, Result string
}

// Results returns every spawned sub-agent's current outcome, in spawn order.
func (r *TeamRuntime) Results() []TeamResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]TeamResult, 0, len(r.order))
	for _, id := range r.order {
		m := r.agents[id]
		out = append(out, TeamResult{ID: m.id, Role: m.role, Task: m.task, Status: m.status, Result: m.result})
	}
	return out
}

// ResultsFor returns outcomes for just the given ids, in the order requested.
// Used by callers (deep research) that share a long-lived runtime and must
// scope to their OWN spawns, not every agent ever spawned this session.
func (r *TeamRuntime) ResultsFor(ids []string) []TeamResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]TeamResult, 0, len(ids))
	for _, id := range ids {
		if m, ok := r.agents[id]; ok {
			out = append(out, TeamResult{ID: m.id, Role: m.role, Task: m.task, Status: m.status, Result: m.result})
		}
	}
	return out
}

func (r *TeamRuntime) summary(timedOut bool) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.order) == 0 {
		return "no sub-agents have been spawned yet"
	}
	var b strings.Builder
	if timedOut {
		b.WriteString("(timed out waiting; reporting current state)\n\n")
	}
	for _, id := range r.order {
		m := r.agents[id]
		fmt.Fprintf(&b, "## %s (%s) [%s]\n%s\n\n", m.id, m.role, m.status, strings.TrimSpace(m.result))
	}
	return strings.TrimRight(b.String(), "\n")
}

// roleSoul derives a sub-agent identity from the base soul: same team
// values/rules, but its own id, name, and role. Shallow copy is safe —
// only scalar identity fields are overwritten; the shared slices are
// read-only at render time.
func roleSoul(base *soul.Soul, id, role string) *soul.Soul {
	s := &soul.Soul{}
	if base != nil {
		*s = *base
	}
	s.Agent.ID = id
	s.Agent.Name = role
	s.Agent.Role = role
	return s
}

func memberPrompt(id, role, orchID string) string {
	// The knowledge-first search-order policy (TEN-249) is shared verbatim with
	// the main agent via searchPolicyPrompt() — so the trust-but-verify nuance
	// and the "use only the tools you have" hedge stay in one place (DRY) and
	// sub-agents get the identical policy. It already covers "knowledge first,
	// web for open-world", so the research line below only adds the HOW (browse).
	return fmt.Sprintf("You are %q, the %s on a collaborating team led by %q. Do your assigned task using "+
		"your full toolset — use tools to do REAL work, not guesses. If you are researching something, "+
		"gather actual information: use web_navigate + web_read to read live sources on the internet when "+
		"web tools are available (don't rely only on prior knowledge).%s "+
		"CRITICAL — never fabricate: ground every claim in what you ACTUALLY read or ran. If a tool fails, "+
		"a page won't load (e.g. a 404), or you can't find the information, SAY SO plainly — do not invent "+
		"facts, specs, numbers, or sources to fill the gap. \"I couldn't find X\" is a correct, required "+
		"answer. Work INDEPENDENTLY: do your own part and report it — don't wait on other sub-agents' "+
		"outputs (the lead agent combines everyone's results). Use memory_remember to persist durable facts "+
		"you learn, and team_send/team_broadcast/team_check/team_history/team_roster to coordinate. Resolve "+
		"disagreements yourself using your identity and rules — don't ask the user. Be concise. Your final "+
		"response IS your deliverable: make it the actual content (your findings/argument), not a status "+
		"update.", id, role, orchID, searchPolicyPrompt())
}

const orchestratorPrompt = "You are the ORCHESTRATOR of a team of AI agents. Break the user's request into " +
	"roles and spawn sub-agents with spawn_agent(role, task) — they run CONCURRENTLY over the team bus. " +
	"Spawn agents ONLY for INDEPENDENT, parallel work. Do NOT spawn an agent whose job is to wait for or " +
	"combine other agents' outputs — sub-agents run at the same time and can't reliably receive each " +
	"other's results; YOU are the synthesis layer, so do any comparison/combination yourself. After " +
	"spawning the agents the task needs, call team_await ONCE — it BLOCKS until the whole team finishes " +
	"and returns their results, and you MUST await before writing any final answer that depends on a " +
	"worker's output. You don't have to await the instant you spawn, though: if you have your own " +
	"INDEPENDENT part to do (or more independent workers to spawn), do that first, then team_await once " +
	"you need the workers' results. (Example: spawn a coder to implement X, keep researching Y yourself, " +
	"then team_await once and combine.) Do NOT poll team_check in a loop to wait; it returns immediately " +
	"and you will run out of steps before they finish. Only once team_await returns, synthesize ONE final " +
	"answer from the collected results — and if a sub-agent reports it couldn't find something, reflect " +
	"that honestly rather than inventing details."

// spawnTool gives the orchestrator the spawn_agent + team_await verbs.
type spawnTool struct {
	rt      *TeamRuntime
	timeout time.Duration // default await timeout
}

func (spawnTool) obj(props string, req ...string) json.RawMessage {
	r := ""
	for i, x := range req {
		if i > 0 {
			r += ","
		}
		r += `"` + x + `"`
	}
	return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":[` + r + `]}`)
}

func (t spawnTool) Tools() []model.ToolSpec {
	return []model.ToolSpec{
		{
			Name:        "spawn_agent",
			Description: "Spawn a sub-agent to work concurrently on part of the task. Give it a short role (e.g. \"researcher\", \"writer\") and a clear, self-contained task. It runs in parallel and can talk to the team; returns its id immediately.",
			Parameters:  t.obj(`"role":{"type":"string"},"task":{"type":"string","description":"a clear, self-contained instruction"}`, "role", "task"),
			// The task leaves this agent: the sub-agent may run on another
			// model or provider, and it has outbound tools of its own. A task
			// quoting sensitive results needs approval, and the approved flow
			// seeds the sub-agent's tracker (TEN-336).
			Outbound: true,
		},
		{
			Name:        "team_await",
			Description: "BLOCK until ALL spawned sub-agents finish, then return each one's result. Call this ONCE when you need the team's results — at the latest, before writing any final answer that depends on them. You may do your own independent work or spawn more independent workers first; you do not have to await immediately. This is how you WAIT for the team — do NOT poll team_check in a loop to wait (team_check returns immediately and you'll run out of steps before they finish).",
			Parameters:  t.obj(``),
		},
	}
}

func (t spawnTool) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	switch call.Name {
	case "spawn_agent":
		var a struct {
			Role string `json:"role"`
			Task string `json:"task"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			return "invalid arguments: " + err.Error(), true, nil
		}
		id, err := t.rt.Spawn(ctx, a.Role, a.Task)
		if err != nil {
			return "spawn failed: " + err.Error(), true, nil
		}
		return "spawned " + id + " (running concurrently). Keep doing your own independent work or " +
			"spawn more independent workers; call team_await ONCE when you need the results — at the " +
			"latest before any final answer that depends on them. Don't poll team_check in a loop to " +
			"wait (it returns immediately and wastes your steps).", false, nil
	case "team_await":
		to := t.timeout
		if to <= 0 {
			to = 3 * time.Minute
		}
		out := t.rt.Await(ctx, to)
		// Sub-agents' answers can quote what they read; hand that provenance
		// back so the orchestrator tracks it before egress (TEN-336).
		agent.ReportFlow(ctx, t.rt.resultFlow())
		// An await TIMEOUT with workers still active is a normal beat of long
		// autonomous work: report current state and let them keep working (the
		// orchestrator can await again). Ending the run here cancelled the run
		// context and executed every still-active sub-agent mid-task
		// ("terminated: team await ended with N subagent(s) still active").
		// The accounting window closes only when all children have joined;
		// stragglers stay bounded by the run's wall-time/token budgets and the
		// TeamRuntime Shutdown teardown.
		if t.rt.BudgetStatus().ActiveSubagents == 0 {
			t.rt.EndRun()
		}
		return out, false, nil
	default:
		return "unknown orchestrator tool: " + call.Name, true, nil
	}
}

// slug makes a role safe for an agent id.
func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "agent"
	}
	return out
}
