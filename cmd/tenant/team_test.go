package main

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/memory/soul"
	"tenant/internal/orchestra"
	"tenant/internal/plugins/web"
)

func TestTeamRuntime_WebReaderActivityUsesAgentEventing(t *testing.T) {
	var gotID string
	var gotEvent agent.Event
	rt := newTeamRuntime(TeamConfig{
		Web: true,
		Observe: func(id string, ev agent.Event) {
			gotID, gotEvent = id, ev
		},
	})
	local := newToolMux()
	rt.addWebTool(local, "researcher-1")
	entry := local.byName["web_read"]
	if entry == nil {
		t.Fatal("per-agent web tool was not registered")
	}
	lw, ok := entry.owner.(*lazyWeb)
	if !ok || lw.cfg.ReaderObserver == nil {
		t.Fatalf("web owner lacks reader activity wiring: %#v", entry.owner)
	}
	lw.cfg.ReaderObserver(web.ReaderActivity{Provider: "r.jina.ai", TargetHost: "example.com"})
	if gotID != "researcher-1" || gotEvent.Tool != "web_reader_remote" || !strings.Contains(gotEvent.Args, "example.com") {
		t.Fatalf("reader activity was not attributed through agent eventing: id=%q event=%+v", gotID, gotEvent)
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"researcher":   "researcher",
		"Lead Writer":  "lead-writer",
		"  api_design": "api-design",
		"!!!":          "agent",
		"":             "agent",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRoleSoul_StampsIdentityKeepsTeamRules(t *testing.T) {
	base := soul.NewDefault("main")
	base.Values.Items = []string{"honesty"}
	s := roleSoul(base, "main-researcher-1", "researcher")
	if s.Agent.ID != "main-researcher-1" || s.Agent.Name != "researcher" || s.Agent.Role != "researcher" {
		t.Fatalf("identity not stamped: %+v", s.Agent)
	}
	if len(s.Values.Items) != 1 || s.Values.Items[0] != "honesty" {
		t.Fatalf("team values should carry over: %+v", s.Values.Items)
	}
	// Base must be untouched (shallow copy only overwrites scalars).
	if base.Agent.ID != "main" {
		t.Fatalf("base soul mutated: %s", base.Agent.ID)
	}
}

// Integration: with the echo backend (no real model needed), spawning
// sub-agents must actually run them concurrently, Await must block until
// they finish, and each must report DONE back to the orchestrator on the
// bus. Exercises the real TeamRuntime wiring end-to-end.
func TestTeamRuntime_SpawnAndAwait(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &commonFlags{backend: "echo", agent: "main", dataDir: t.TempDir(), cfgDir: t.TempDir()}
	router, err := buildRouter(c, log)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	st, closeStores, err := openStores(c)
	if err != nil {
		t.Fatalf("stores: %v", err)
	}
	defer closeStores()

	bus := orchestra.NewBus()
	bus.Register("main")
	bus.Register("peer") // a passive teammate to prove completions are broadcast
	rt := newTeamRuntime(TeamConfig{
		Bus: bus, Router: router, Stores: st, Shared: newToolMux(), OrchID: "main", Log: log,
	})

	ctx := context.Background()
	id1, err := rt.Spawn(ctx, "researcher", "find the answer")
	if err != nil {
		t.Fatalf("spawn 1: %v", err)
	}
	id2, err := rt.Spawn(ctx, "writer", "write it up")
	if err != nil {
		t.Fatalf("spawn 2: %v", err)
	}
	if id1 == id2 {
		t.Fatal("spawned agents must get distinct ids")
	}

	summary := rt.Await(ctx, 30*time.Second)
	for _, want := range []string{id1, id2, "researcher", "writer", "[done]"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("await summary missing %q:\n%s", want, summary)
		}
	}

	// Each finished sub-agent BROADCASTs DONE — so both the orchestrator and
	// a passive peer see both completions (bug fix: results are no longer
	// direct-to-orchestrator only, so a still-working peer can observe them).
	for _, who := range []string{"main", "peer"} {
		inbox := bus.Inbox(who)
		if len(inbox) != 2 {
			t.Fatalf("%s should see 2 completion messages, got %d", who, len(inbox))
		}
		for _, m := range inbox {
			if !strings.HasPrefix(m.Content, "DONE") {
				t.Fatalf("completion message malformed: %q", m.Content)
			}
		}
	}
}

// The per-agent web tool must appear exactly ONCE in an agent's toolset:
// the shared mux's web stub is disabled (suppressed) so it isn't
// advertised, and the agent's own lazyWeb provides web. Duplicated specs
// would confuse the model.
func TestComposite_PerAgentWebNotDuplicated(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &commonFlags{agent: "main", dataDir: t.TempDir()}
	// pluginFlags{} ⇒ web NOT enabled in the shared mux (registered as a
	// disabled stub, which Search/All exclude).
	shared, _, cleanup, err := buildToolMux(context.Background(), c, nil, &pluginFlags{}, nil, log)
	if err != nil {
		t.Fatalf("buildToolMux: %v", err)
	}
	defer cleanup()

	local := newToolMux()
	local.add("web", newLazyWeb(web.Config{}, web.Policy{}, "", nil, nil, nil))
	comp := composite{shared: shared, local: local}

	count := 0
	for _, s := range comp.All() {
		if s.Name == "web_navigate" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("web_navigate must appear exactly once, got %d", count)
	}
}

// routerForProfile looks up the role in AgentProfiles. Unknown role → falls
// back to the shared orchestrator router with nil profile. Known role → builds
// + caches a per-profile router and returns the profile. Cache hit on the
// second call returns the SAME router pointer.
func TestRouterForProfile_Lookup(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	c := &commonFlags{backend: "echo", agent: "main", dataDir: t.TempDir(), cfgDir: dir}
	router, _ := buildRouter(c, log)
	st, closeStores, _ := openStores(c)
	defer closeStores()

	// Seed launchConfig with a provider + agent profile so the lookup has
	// real data to resolve against.
	lc := &launchConfig{
		Provider: "echo-fallback",
		Providers: map[string]*providerConfig{
			"echo-fallback": {Kind: "echo", Model: "echo"},
		},
		Agents: map[string]*agentProfile{
			"researcher": {Provider: "echo-fallback", Description: "test"},
		},
	}
	if err := lc.Save(dir); err != nil {
		t.Fatal(err)
	}
	bus := orchestra.NewBus()
	bus.Register("main")
	rt := newTeamRuntime(TeamConfig{
		Bus: bus, Router: router, Stores: st, Shared: newToolMux(),
		OrchID: "main", Log: log,
		AgentProfiles: lc.Agents,
		CfgDir:        dir,
	})

	// Unknown role → shared router, nil profile.
	gotR, gotP := rt.routerForProfile("unknown-role")
	if gotR != router {
		t.Error("unknown role should return the shared router")
	}
	if gotP != nil {
		t.Errorf("unknown role profile should be nil, got %+v", gotP)
	}

	// Known role → new router + profile.
	gotR, gotP = rt.routerForProfile("researcher")
	if gotR == router {
		t.Error("researcher should get its own router, not the shared one")
	}
	if gotP == nil || gotP.Provider != "echo-fallback" {
		t.Errorf("profile lookup wrong: %+v", gotP)
	}
	// Cache: second call returns the same pointer.
	gotR2, _ := rt.routerForProfile("researcher")
	if gotR2 != gotR {
		t.Error("router cache miss on second lookup")
	}

	// SetAgentProfiles flushes the cache.
	rt.SetAgentProfiles(map[string]*agentProfile{
		"researcher": {Provider: "echo-fallback", Description: "edited"},
		// echo's model needs to be resolvable from the provider (no change there).
	})
	gotR3, gotP3 := rt.routerForProfile("researcher")
	if gotR3 == gotR {
		t.Error("SetAgentProfiles should invalidate the router cache")
	}
	if gotP3 == nil || gotP3.Description != "edited" {
		t.Errorf("publish didn't update profile: %+v", gotP3)
	}
}

// AgentProfiles snapshot returns a defensive copy.
func TestAgentProfiles_Snapshot(t *testing.T) {
	rt := newTeamRuntime(TeamConfig{
		AgentProfiles: map[string]*agentProfile{
			"a": {Provider: "p"},
		},
	})
	snap := rt.AgentProfiles()
	snap["b"] = &agentProfile{Provider: "evil"}
	// Internal map must not have been mutated.
	if _, leaked := rt.cfg.AgentProfiles["b"]; leaked {
		t.Error("AgentProfiles() should return a defensive copy")
	}
}

func TestSpawnTool_RequiresRoleAndTask(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &commonFlags{backend: "echo", agent: "main", dataDir: t.TempDir(), cfgDir: t.TempDir()}
	router, _ := buildRouter(c, log)
	st, closeStores, _ := openStores(c)
	defer closeStores()
	bus := orchestra.NewBus()
	bus.Register("main")
	rt := newTeamRuntime(TeamConfig{
		Bus: bus, Router: router, Stores: st, Shared: newToolMux(), OrchID: "main", Log: log,
	})

	if _, err := rt.Spawn(context.Background(), "", "task"); err == nil {
		t.Fatal("spawn with empty role must error")
	}
	if _, err := rt.Spawn(context.Background(), "role", ""); err == nil {
		t.Fatal("spawn with empty task must error")
	}
}

func TestTeamRuntime_SpawnRefusedAtAgentsPerRunCap(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &commonFlags{backend: "echo", agent: "main", dataDir: t.TempDir(), cfgDir: t.TempDir()}
	router, err := buildRouter(c, log)
	if err != nil {
		t.Fatal(err)
	}
	st, closeStores, err := openStores(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStores()
	bus := orchestra.NewBus()
	bus.Register("main")
	rt := newTeamRuntime(TeamConfig{
		Bus: bus, Router: router, Stores: st, Shared: newToolMux(), OrchID: "main", Log: log,
		Budgets: orchestra.Budgets{MaxAgentsPerRun: 1},
	})
	defer rt.Close()
	if _, err := rt.Spawn(context.Background(), "first", "work"); err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	rt.Await(context.Background(), 30*time.Second) // returns once the agent is done; it must not outlive the stores
	if _, err := rt.Spawn(context.Background(), "second", "work"); err == nil || !strings.Contains(err.Error(), "agents per run") {
		t.Fatalf("second spawn should be structurally refused with cap context, got %v", err)
	}
}

// TEN-140: the orchestrator prompt gains the delegate-and-keep-working pattern
// WITHOUT regressing the fan-out → await → synthesize default or its guardrails.
func TestOrchestratorPrompt_KeepWorkingAndDefaults(t *testing.T) {
	// Keep-working steering is present (TEN-140), with the must-await boundary
	// front-loaded so it can't be read as license to under-await.
	keepWorking := []string{
		"don't have to await the instant you spawn",
		"more independent workers",
		"team_await once you need the workers' results",
		"MUST await before writing any final answer",
	}
	for _, w := range keepWorking {
		if !strings.Contains(orchestratorPrompt, w) {
			t.Errorf("orchestratorPrompt missing keep-working steering %q", w)
		}
	}
	// Fan-out → await-once → synthesize default + guardrails are PRESERVED.
	defaults := []string{
		"spawn_agent(role, task)",
		"CONCURRENTLY",
		"call team_await ONCE",
		"Do NOT poll team_check",
		"synthesize ONE final",
		"INDEPENDENT, parallel work",
	}
	for _, w := range defaults {
		if !strings.Contains(orchestratorPrompt, w) {
			t.Errorf("orchestratorPrompt lost default/guardrail text %q", w)
		}
	}
}

// TEN-225: the composite (what the TUI/orchestrate agent actually holds) must
// delegate RankingStatus to the SHARED mux, where ranking runs — otherwise the
// per-turn diagnostic + /tools status silently fall through.
func TestComposite_RankingStatus_DelegatesToShared(t *testing.T) {
	shared := newToolMux()
	for i := 0; i < 25; i++ { // above rankActivateThreshold, no embedder → fallback
		shared.add("p", fakePlugin{name: "tool" + strconv.Itoa(i)})
	}
	c := composite{shared: shared, local: newToolMux()}

	// Before any Search → not measured.
	if _, _, _, _, ok := c.RankingStatus(); ok {
		t.Error("RankingStatus should be unmeasured before the shared mux runs a Search")
	}

	if _, err := c.Search(context.Background(), []float32{1, 0}, 12); err != nil {
		t.Fatalf("composite Search: %v", err)
	}
	ranked, _, catalog, reason, ok := c.RankingStatus()
	if !ok {
		t.Fatal("RankingStatus should be set after a Search through the composite")
	}
	if ranked {
		t.Error("no embedder → ranking OFF")
	}
	if catalog != 25 || !strings.Contains(reason, "embedder") {
		t.Errorf("expected shared-mux status (catalog=25, embedder reason); got catalog=%d reason=%q", catalog, reason)
	}
}

// Unlimited spawning is what let one question become ~30 contradictory
// sub-agents that never converged (and leaked a browser session each). The cap
// must refuse in code rather than rely on the orchestrator's restraint, and it
// must say what to do instead so the model awaits rather than retrying.
func TestTeamRuntime_SpawnCapRefusesRunaway(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &commonFlags{backend: "echo", agent: "main", dataDir: t.TempDir(), cfgDir: t.TempDir()}
	router, err := buildRouter(c, log)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	st, closeStores, err := openStores(c)
	if err != nil {
		t.Fatalf("stores: %v", err)
	}
	defer closeStores()

	bus := orchestra.NewBus()
	bus.Register("main")
	rt := newTeamRuntime(TeamConfig{
		Bus: bus, Router: router, Stores: st, Shared: newToolMux(),
		OrchID: "main", Log: log, MaxConcurrent: 2,
	})

	if got := rt.maxConcurrent(); got != 2 {
		t.Fatalf("configured cap = %d, want 2", got)
	}

	// Hold two slots open so the third spawn hits the bound deterministically,
	// without depending on how fast the echo backend finishes.
	rt.mu.Lock()
	for _, id := range []string{"held-1", "held-2"} {
		rt.agents[id] = &member{id: id, role: "held", task: "t", status: "running", done: make(chan struct{})}
		rt.order = append(rt.order, id)
	}
	rt.mu.Unlock()

	_, err = rt.Spawn(context.Background(), "researcher", "one too many")
	if err == nil {
		t.Fatal("spawning past the cap must be refused")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("refusal should name the cause: %v", err)
	}
	if !strings.Contains(err.Error(), "await") {
		t.Errorf("refusal should tell the orchestrator to await rather than retry: %v", err)
	}

	// Freeing a slot lets work proceed - the cap bounds concurrency, it does
	// not permanently disable spawning.
	rt.mu.Lock()
	rt.agents["held-1"].status = "done"
	rt.mu.Unlock()
	id, err := rt.Spawn(context.Background(), "researcher", "now there is room")
	if err != nil {
		t.Fatalf("spawn after a slot freed should succeed: %v", err)
	}
	// Let that turn finish before the deferred store close: an agent still
	// searching memory when the stores close races the DB (or hits a nil one).
	rt.mu.Lock()
	done := rt.agents[id].done
	rt.mu.Unlock()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the spawned agent never finished")
	}
}

// An unset cap must fall back to the documented default rather than 0, which
// would refuse every spawn.
func TestTeamRuntime_DefaultCap(t *testing.T) {
	rt := newTeamRuntime(TeamConfig{})
	if got := rt.maxConcurrent(); got != defaultMaxConcurrentAgents {
		t.Fatalf("default cap = %d, want %d", got, defaultMaxConcurrentAgents)
	}
	rt2 := newTeamRuntime(TeamConfig{MaxConcurrent: -1})
	if got := rt2.maxConcurrent(); got != defaultMaxConcurrentAgents {
		t.Fatalf("negative cap = %d, want the default %d", got, defaultMaxConcurrentAgents)
	}
}

// Over the MCP bridge the caller's context is a single JSON-RPC request that
// ends as soon as spawn_agent returns. Inheriting it killed every sub-agent
// mid-thought — the spawn "succeeded", the roster listed it, and the await
// reported "(no result)", which reads as a model problem rather than a
// lifetime problem.
func TestTeamRuntime_BaseCtxOutlivesTheCallingRequest(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &commonFlags{backend: "echo", agent: "main", dataDir: t.TempDir(), cfgDir: t.TempDir()}
	router, err := buildRouter(c, log)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	st, closeStores, err := openStores(c)
	if err != nil {
		t.Fatalf("stores: %v", err)
	}
	defer closeStores()

	bus := orchestra.NewBus()
	bus.Register("main")
	rt := newTeamRuntime(TeamConfig{
		Bus: bus, Router: router, Stores: st, Shared: newToolMux(),
		OrchID: "main", Log: log, BaseCtx: context.Background(),
	})

	// Exactly the bridge's shape: the request context dies the instant the
	// tool call returns.
	reqCtx, endRequest := context.WithCancel(context.Background())
	id, err := rt.Spawn(reqCtx, "researcher", "find the answer")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	endRequest()

	summary := rt.Await(context.Background(), 30*time.Second)
	if !strings.Contains(summary, id) {
		t.Fatalf("await lost the agent:\n%s", summary)
	}
	if !strings.Contains(summary, "[done]") {
		t.Errorf("agent did not finish after its caller went away:\n%s", summary)
	}
	if strings.Contains(summary, "(no result)") {
		t.Errorf("agent produced no result — it was cancelled with its caller:\n%s", summary)
	}
}
