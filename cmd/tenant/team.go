package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"tenant/internal/agent"
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
	done           chan struct{}
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
	Bus        *orchestra.Bus
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
	Observe    func(agentID string, e agent.Event) // live view hook (may be nil)

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
	cfg TeamConfig

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
	return &TeamRuntime{
		cfg:            cfg,
		agents:         map[string]*member{},
		profileRouters: map[string]*model.Router{},
	}
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
func (r *TeamRuntime) addWebTool(local *toolMux) {
	if !r.cfg.Web {
		return
	}
	local.add("web", newLazyWeb(r.cfg.WebConfig, r.cfg.WebPolicy, r.cfg.Shots, r.addCleanup, r.cfg.Log))
}

// composite merges a shared tool set (plugins) with an agent's local tools
// (comms, memory, skills, and for the orchestrator spawn/await). Local
// tools are always surfaced; anything not local falls through to shared.
// Implements both agent.ToolRegistry and agent.ToolDispatcher.
type composite struct {
	shared *toolMux
	local  *toolMux
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

// Spawn creates a sub-agent for (role, task), starts it running in its own
// goroutine, and returns its id immediately (non-blocking — the team works
// concurrently). The sub-agent reports its result back to the orchestrator
// over the bus when it finishes.
func (r *TeamRuntime) Spawn(ctx context.Context, role, task string) (string, error) {
	role = strings.TrimSpace(role)
	task = strings.TrimSpace(task)
	if role == "" || task == "" {
		return "", fmt.Errorf("spawn needs both a role and a task")
	}

	r.mu.Lock()
	r.seq++
	id := fmt.Sprintf("%s-%s-%d", r.cfg.OrchID, slug(role), r.seq)
	// Per-agent cancellable ctx so the orchestrator can preempt a specific
	// stuck agent (wave-timeout salvage path) without cancelling the entire
	// research run.
	agCtx, cancel := context.WithCancel(ctx)
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
	tools := r.agentTools(id) // full plugin set + this agent's own tools

	ag, err := agent.New(agent.Config{
		AgentID:      id,
		Router:       router,
		Soul:         sl,
		Working:      working.New(),
		Archive:      r.cfg.Stores.archive,
		Episodic:     r.cfg.Stores.episodic,
		Semantic:     r.cfg.Stores.semantic,
		Tools:        tools,
		Dispatcher:   tools,
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
		r.finish(m, "error", "failed to start: "+err.Error())
		return "", err
	}

	go func() {
		defer cancel() // release the ctx when this agent finishes naturally
		res, terr := ag.Turn(agCtx, agent.TurnRequest{UserQuery: task})
		switch {
		case terr != nil:
			r.finish(m, "error", "error: "+terr.Error())
		case res != nil && strings.TrimSpace(res.Response) != "":
			r.finish(m, "done", res.Response)
		default:
			r.finish(m, "done", "(no result)")
		}
		// Broadcast completion to the WHOLE team (not just the orchestrator)
		// so a peer that's still working can see this result via team_check/
		// team_history. (The orchestrator also collects results via
		// team_await from the runtime, independent of the bus.)
		_ = r.cfg.Bus.Send(orchestra.Message{From: id,
			Content: "DONE — " + clip(m.result, 600)})
	}()

	return id, nil
}

// agentTools builds an agent's FULL tool set: the shared plugin tools plus
// per-agent comms + memory (+ skills) bound to its id. This is what gives a
// sub-agent the same workability as the main agent. The agent-id-bound
// tools (memory_remember writes the agent's own facts; skill_save saves
// under its id) live in the local layer; the shared external-resource
// plugins (wiki/sql/web/os/…) are shared.
func (r *TeamRuntime) agentTools(id string) composite {
	local := newToolMux()
	local.add("team", orchestra.CommsTool{Bus: r.cfg.Bus, Self: id})
	local.add("memory", memoryTool{sem: r.cfg.Stores.semantic, emb: r.cfg.Embedder, embedderID: r.cfg.EmbedderID, agentID: id})
	if r.cfg.Skills != nil {
		local.add("skills", skillTool{st: r.cfg.Skills, emb: r.cfg.Embedder, agentID: id})
	}
	r.addWebTool(local) // this agent's OWN lazy browser session
	// Orchestrator only: restore persisted local enable/disable state
	// AND wire the save hook so /enable web_search etc. survives a
	// restart. Sub-agents are ephemeral; their local state isn't
	// persisted by design.
	if id == r.cfg.OrchID {
		if len(r.cfg.LocalRestore) > 0 {
			_ = local.restore(r.cfg.LocalRestore) // notes ignored — same set already in shared's restore log
		}
		if r.cfg.LocalOnChange != nil {
			local.setOnChange(r.cfg.LocalOnChange)
		}
	}
	return composite{shared: r.cfg.Shared, local: local}
}

func (r *TeamRuntime) skillRetriever(id string) agent.SkillRetriever {
	if r.cfg.Skills == nil {
		return nil
	}
	return skillRetriever{st: r.cfg.Skills, agentID: id}
}

func (r *TeamRuntime) finish(m *member, status, result string) {
	r.mu.Lock()
	m.status = status
	m.result = result
	r.mu.Unlock()
	close(m.done)
}

func (r *TeamRuntime) emit(id string, e agent.Event) {
	if r.cfg.Observe != nil {
		r.cfg.Observe(id, e)
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
	return fmt.Sprintf("You are %q, the %s on a collaborating team led by %q. Do your assigned task using "+
		"your full toolset — use tools to do REAL work, not guesses. If you are researching something, "+
		"gather actual information: use web_navigate + web_read to read live sources on the internet when "+
		"web tools are available (don't rely only on local files or prior knowledge). "+
		"CRITICAL — never fabricate: ground every claim in what you ACTUALLY read or ran. If a tool fails, "+
		"a page won't load (e.g. a 404), or you can't find the information, SAY SO plainly — do not invent "+
		"facts, specs, numbers, or sources to fill the gap. \"I couldn't find X\" is a correct, required "+
		"answer. Work INDEPENDENTLY: do your own part and report it — don't wait on other sub-agents' "+
		"outputs (the lead agent combines everyone's results). Use memory_remember to persist durable facts "+
		"you learn, and team_send/team_broadcast/team_check/team_history/team_roster to coordinate. Resolve "+
		"disagreements yourself using your identity and rules — don't ask the user. Be concise. Your final "+
		"response IS your deliverable: make it the actual content (your findings/argument), not a status "+
		"update.", id, role, orchID)
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
		return t.rt.Await(ctx, to), false, nil
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
