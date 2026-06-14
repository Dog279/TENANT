package main

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/soul"
	"tenant/internal/orchestra"
	"tenant/internal/plugins/web"
)

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
	local.add("web", newLazyWeb(web.Config{}, web.Policy{}, "", nil, nil))
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
	if err := lc.save(dir); err != nil {
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
