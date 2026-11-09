package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/logs"
	"tenant/internal/memory/soul"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
	"tenant/internal/orchestra"
	"tenant/internal/plugins/web"
	"tenant/internal/runtrace"
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
	owner, ok := local.Owner("web_read")
	if !ok {
		t.Fatal("per-agent web tool was not registered")
	}
	lw, ok := owner.(*lazyWeb)
	if !ok || lw.cfg.ReaderObserver == nil {
		t.Fatalf("web owner lacks reader activity wiring: %#v", owner)
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
	hub := logs.NewMemory(500)
	prev := logs.Default()
	logs.Install(hub)
	t.Cleanup(func() { logs.Install(prev) })
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

	// The spawning turn's trace becomes each sub-agent run's parent (TEN-368).
	ctx := runtrace.WithSpan(context.Background(), runtrace.NewSpan("orchestrator-turn"))
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

	runs, err := st.runs.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("sub-agent run records = %d, want 2", len(runs))
	}
	for _, r := range runs {
		if r.ParentTraceID != "orchestrator-turn" {
			t.Fatalf("sub-agent run %s parent = %q, want the spawning turn's trace", r.AgentID, r.ParentTraceID)
		}
	}

	// Each sub-agent's run is an activity in subagents: related to the
	// spawning turn, naming its own run's trace, stopped with a duration.
	es, _ := hub.Snapshot()
	ops := map[string]int{}
	for _, e := range es {
		if e.Field("event") != "subagents/1" {
			continue
		}
		ops[e.Field("opcode")]++
		ran := false
		for _, r := range runs {
			ran = ran || r.TraceID == e.Field("run")
		}
		start := e.Field("opcode") == "start"
		if e.Source != "subagents" || e.Field("trace") != "orchestrator-turn" || e.Field("activity") == "" || !ran ||
			start != (e.Field("related") == "orchestrator-turn") || start != (e.Field("duration") == "") {
			t.Errorf("sub-agent activity: %s", e.Line())
		}
	}
	if ops["start"] != 2 || ops["stop"] != 2 {
		t.Errorf("sub-agent activities: %v, want 2 starts and 2 stops", ops)
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
	local.Add("web", newLazyWeb(web.Config{}, web.Policy{}, "", nil, nil, nil))
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
		"team_await again while workers are still running",
		"MUST have every result you depend on before writing a final answer",
	}
	for _, w := range keepWorking {
		if !strings.Contains(orchestratorPrompt, w) {
			t.Errorf("orchestratorPrompt missing keep-working steering %q", w)
		}
	}
	// Fan-out → await (until every result is in, TEN-529) → synthesize default + guardrails are PRESERVED.
	defaults := []string{
		"spawn_agent(role, task)",
		"CONCURRENTLY",
		"call team_await — it BLOCKS until there is news",
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
	for i := 0; i < 25; i++ { // above toolmux.RankActivateThreshold, no embedder → fallback
		shared.Add("p", fakePlugin{name: "tool" + strconv.Itoa(i)})
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

// echoTeam is a team runtime whose sub-agents run on the echo backend, under
// budgets b and concurrency cap max (0 = the default); the bus holds "main".
func echoTeam(t *testing.T, b orchestra.Budgets, max int) (*TeamRuntime, *orchestra.Bus) {
	t.Helper()
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
	t.Cleanup(closeStores)
	bus := orchestra.NewBus()
	bus.Register("main")
	return newTeamRuntime(TeamConfig{Bus: bus, Router: router, Stores: st, Shared: newToolMux(),
		OrchID: "main", Log: log, Budgets: b, MaxConcurrent: max}), bus
}

// teamAwait is the orchestrator calling team_await in the turn of ctx.
func teamAwait(t *testing.T, rt *TeamRuntime, ctx context.Context) string {
	t.Helper()
	out, isErr, err := spawnTool{rt: rt, timeout: 30 * time.Second}.Dispatch(ctx, model.ToolCall{Name: "team_await", Arguments: json.RawMessage(`{}`)})
	if err != nil || isErr {
		t.Errorf("team_await: %q isErr=%v err=%v", out, isErr, err)
	}
	return out
}

// held is what the runtime keeps: its members, in spawn order.
func held(rt *TeamRuntime) []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.agents) != len(rt.order) {
		return append(slices.Clone(rt.order), "(agents and order disagree)")
	}
	return slices.Clone(rt.order)
}

// TEN-326: a server runs for months, so the runtime must not keep every
// sub-agent it ever spawned. team_await returns each result once, then drops
// it, and a finished agent has left the bus.
func TestTeamAwait_ReturnsEachResultOnceThenDropsIt(t *testing.T) {
	rt, bus := echoTeam(t, orchestra.Budgets{MaxAgentsPerRun: 100}, 0)
	turn := runtrace.WithSpan(context.Background(), runtrace.NewSpan("turn"))
	for i := range 20 {
		id, err := rt.Spawn(turn, "worker", "task "+strconv.Itoa(i))
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		if out := teamAwait(t, rt, turn); strings.Count(out, "## "+id+" ") != 1 {
			t.Fatalf("await %d should return %s once:\n%s", i, id, out)
		}
		if h := held(rt); len(h) != 0 {
			t.Fatalf("after await %d the runtime still holds %v", i, h)
		}
	}
	if out := teamAwait(t, rt, turn); !strings.Contains(out, "already returned") {
		t.Errorf("a result came back twice, or the empty answer claims nothing was spawned:\n%s", out)
	}
	if m := bus.Members(); len(m) != 1 {
		t.Errorf("finished sub-agents are still on the bus: %v", m)
	}
}

// inConversation is a turn of conversation conv, stamped as an agent's Turn
// stamps its tool calls: its session and the turn's trace.
func inConversation(conv, turn string) context.Context {
	return agent.WithConversation(runtrace.WithSpan(context.Background(), runtrace.NewSpan(turn)), conv)
}

// spawnFinished spawns a worker in ctx's turn and waits for it to finish.
func spawnFinished(t *testing.T, rt *TeamRuntime, ctx context.Context, task string) string {
	t.Helper()
	id, err := rt.Spawn(ctx, "worker", task)
	if err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	done := rt.agents[id].done
	rt.mu.Unlock()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never finished", id)
	}
	return id
}

// Serve shares one runtime between the dashboard, the relays and cron: a
// team_await reports its own conversation's sub-agents, never another's
// (TEN-326, and so no other conversation's provenance, TEN-336), and an
// await whose caller is gone takes nothing.
func TestTeamAwait_ReportsOnlyItsConversation(t *testing.T) {
	rt, _ := echoTeam(t, orchestra.Budgets{MaxAgentsPerRun: 100}, 0)
	dash, relay := inConversation("dashboard", "turn-1"), inConversation("discord", "turn-2")
	idD := spawnFinished(t, rt, dash, "the dashboard's task")
	idR := spawnFinished(t, rt, relay, "the relay's task")
	gone, cancel := context.WithCancel(relay)
	cancel()
	teamAwait(t, rt, gone)
	if out := teamAwait(t, rt, relay); strings.Count(out, "## "+idR+" ") != 1 || strings.Contains(out, idD) {
		t.Fatalf("the relay's await should show its own worker once, and not the dashboard's:\n%s", out)
	}
	if out := teamAwait(t, rt, dash); strings.Count(out, "## "+idD+" ") != 1 || strings.Contains(out, idR) {
		t.Fatalf("the dashboard's await should show its own worker once:\n%s", out)
	}
	if h := held(rt); len(h) != 0 {
		t.Fatalf("held %v after each conversation took its own", h)
	}
}

// A result nobody awaited stays with its conversation: the next turn's
// team_await there takes it, once, after that turn's own results, which come
// first so a long leftover can't push them past the tool-result cap.
func TestTeamAwait_NextTurnTakesItsConversationsLeftovers(t *testing.T) {
	rt, _ := echoTeam(t, orchestra.Budgets{MaxAgentsPerRun: 100}, 0)
	idX := spawnFinished(t, rt, inConversation("chat", "turn-1"), "turn 1's task") // turn 1 ends unawaited
	turn2 := inConversation("chat", "turn-2")
	idY := spawnFinished(t, rt, turn2, "turn 2's task")
	out := teamAwait(t, rt, turn2)
	x, y := strings.Index(out, "## "+idX+" "), strings.Index(out, "## "+idY+" ")
	if x < 0 || y < 0 || y > x || strings.Count(out, "## "+idX+" ") != 1 {
		t.Fatalf("turn 2's await should show its own worker, then turn 1's, once each:\n%s", out)
	}
	if out := teamAwait(t, rt, inConversation("chat", "turn-3")); strings.Contains(out, idX) || strings.Contains(out, idY) {
		t.Fatalf("a result came back twice:\n%s", out)
	}
}

// A conversation's unreported result is bounded by its own cap, not by what
// other conversations spawn across runs a third one's awaits end (TEN-326).
// The review's repro, at two agents per run.
func TestTeamRuntime_OtherConversationsDontEvictLeftovers(t *testing.T) {
	rt, _ := echoTeam(t, orchestra.Budgets{MaxAgentsPerRun: 2}, 0)
	idA := spawnFinished(t, rt, inConversation("t1", "a"), "a's task")
	t2, t3 := inConversation("t2", "b"), inConversation("t3", "c")
	teamAwait(t, rt, t2) // ends run 1
	spawnFinished(t, rt, t3, "b's task")
	spawnFinished(t, rt, t3, "c's task")
	teamAwait(t, rt, t2) // ends run 2
	spawnFinished(t, rt, t3, "d's task")
	if out := teamAwait(t, rt, inConversation("t1", "a2")); strings.Count(out, "## "+idA+" ") != 1 {
		t.Fatalf("t1's result was evicted by t3's spawns:\n%s", out)
	}
	teamAwait(t, rt, t3) // join t3's before the stores close
}

// A member nobody takes with team_await (/research reads its own with
// ResultsFor) is kept while it is among the newest run's worth of finished
// ones. The oldest finished go first, a running one never.
func TestTeamRuntime_SpawnDropsTheOldestFinishedPastARunsWorth(t *testing.T) {
	rt, _ := echoTeam(t, orchestra.Budgets{MaxAgentsPerRun: 3}, 0)
	closed := make(chan struct{})
	close(closed)
	rt.mu.Lock()
	for _, id := range []string{"stuck", "done-1", "done-2", "done-3", "done-4", "done-5"} {
		m := &member{id: id, status: "done", done: closed}
		if id == "stuck" {
			m.status, m.done = "running", make(chan struct{})
		}
		rt.agents[id] = m
		rt.order = append(rt.order, id)
	}
	rt.mu.Unlock()
	id, err := rt.Spawn(context.Background(), "worker", "one more")
	if err != nil {
		t.Fatal(err)
	}
	h := held(rt)
	rt.mu.Lock()
	done := rt.agents[id].done
	rt.mu.Unlock()
	<-done // before the stores close
	if want := []string{"stuck", "done-3", "done-4", "done-5", id}; !slices.Equal(h, want) {
		t.Fatalf("held %v, want %v", h, want)
	}
}

// /research's shape, run after run on one runtime: each run's results are all
// there for ResultsFor, and the runtime holds at most a run's worth besides.
func TestTeamRuntime_ResearchRunsStayBounded(t *testing.T) {
	const perRun = 3
	rt, bus := echoTeam(t, orchestra.Budgets{MaxAgentsPerRun: perRun}, 0)
	ctx := context.Background()
	for run := range 10 {
		rt.BeginRun(ctx)
		var mine []string
		for i := range perRun {
			id, err := rt.Spawn(ctx, "researcher", fmt.Sprintf("run %d, question %d", run, i))
			if err != nil {
				t.Fatalf("run %d spawn %d: %v", run, i, err)
			}
			mine = append(mine, id)
		}
		rt.Await(ctx, 30*time.Second)
		if got := rt.ResultsFor(mine); len(got) != perRun {
			t.Fatalf("run %d lost results before it read them: %+v", run, got)
		}
		rt.EndRun()
	}
	if h := held(rt); len(h) > 2*perRun {
		t.Fatalf("held %d members after 10 runs, want at most a run's worth plus the last run: %v", len(h), h)
	}
	if m := bus.Members(); len(m) != 1 {
		t.Errorf("finished sub-agents are still on the bus: %v", m)
	}
}

// A sub-agent's budget slot comes back before it finishes (its done closes):
// team_await, having seen every child finish, must find none holding a slot,
// or it skips ending the run and the accounting window stays open (TEN-326).
// The hook runs inside the release, so it sees the order, not a race: the one
// agent must still be running then.
func TestTeamRuntime_SlotComesBackBeforeDone(t *testing.T) {
	rt, _ := echoTeam(t, orchestra.Budgets{}, 0)
	var mu sync.Mutex
	active := 0
	late := make(chan bool, 1) // at the release: had the agent finished?
	rt.budget = orchestra.NewGovernor(orchestra.Budgets{}, func(st orchestra.BudgetStatus) {
		mu.Lock()
		released := st.ActiveSubagents < active
		active = st.ActiveSubagents
		mu.Unlock()
		if !released {
			return
		}
		rt.mu.Lock()
		running := 0
		for _, m := range rt.agents {
			if m.status == "running" {
				running++
			}
		}
		rt.mu.Unlock()
		late <- running == 0 // finished, or even reported and dropped
	})
	turn := runtrace.WithSpan(context.Background(), runtrace.NewSpan("turn"))
	if _, err := rt.Spawn(turn, "worker", "task"); err != nil {
		t.Fatal(err)
	}
	teamAwait(t, rt, turn)
	if <-late {
		t.Error("the agent's slot came back after it finished")
	}
	if st := rt.BudgetStatus(); st.Active || st.ActiveSubagents != 0 {
		t.Errorf("every child joined, but the run is still open: %+v", st)
	}
}

// Ending the run and admitting a spawn are decided under one lock (TEN-326).
// Here turn x's team_await runs at the moment turn y's spawn opens the run,
// before y would take its slot: x must not close that run, and y's agent must
// get its slot in it and run to the end, not be refused or cancelled.
func TestTeamAwait_CantEndTheRunASpawnJoins(t *testing.T) {
	rt, _ := echoTeam(t, orchestra.Budgets{}, 0)
	x, y := inConversation("dashboard", "turn-x"), inConversation("discord", "turn-y")
	var once sync.Once
	rt.budget = orchestra.NewGovernor(orchestra.Budgets{}, func(st orchestra.BudgetStatus) {
		if st.Active { // y's spawn has opened the run
			once.Do(func() { teamAwait(t, rt, x) })
		}
	})
	id, err := rt.Spawn(y, "worker", "y's task")
	if err != nil {
		t.Fatalf("y's spawn was refused: %v", err)
	}
	if out := teamAwait(t, rt, y); !strings.Contains(out, "## "+id+" (worker) [done]") {
		t.Fatalf("y's agent didn't run to the end:\n%s", out)
	}
}

// Conversations on one runtime spawn and await at the same time: each gets
// each of its own results exactly once, and the runtime ends up holding
// nothing.
func TestTeamRuntime_ConcurrentSpawnAndAwait(t *testing.T) {
	const turns, rounds = 4, 5
	// Exactly a slot per turn, and no retries: a slot comes back before its
	// agent's done, and no turn's await can end the run under another's spawn.
	rt, bus := echoTeam(t, orchestra.Budgets{MaxAgentsPerRun: turns * rounds, MaxConcurrentSubagents: turns}, turns)
	var wg sync.WaitGroup
	for n := range turns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := inConversation(fmt.Sprintf("conversation-%d", n), fmt.Sprintf("turn-%d", n))
			var mine []string
			var outs strings.Builder
			for i := range rounds {
				id, err := rt.Spawn(ctx, "worker", fmt.Sprintf("turn %d, task %d", n, i))
				if err != nil {
					t.Errorf("turn %d spawn %d: %v", n, i, err)
					return
				}
				mine = append(mine, id)
				outs.WriteString(teamAwait(t, rt, ctx))
			}
			for _, id := range mine {
				if c := strings.Count(outs.String(), "## "+id+" "); c != 1 {
					t.Errorf("%s came back to its turn %d times, want once", id, c)
				}
			}
		}()
	}
	wg.Wait()
	rt.Await(context.Background(), 30*time.Second) // nothing may outlive the stores
	if h := held(rt); len(h) != 0 {
		t.Errorf("the runtime still holds %v", h)
	}
	if m := bus.Members(); len(m) != 1 {
		t.Errorf("finished sub-agents are still on the bus: %v", m)
	}
}

// blockingTeam is a team runtime whose sub-agents wait in their first model
// call until their context ends: they run until something stops them.
func blockingTeam(t *testing.T) *TeamRuntime {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, closeStores, err := openStores(&commonFlags{agent: "main", dataDir: t.TempDir(), cfgDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeStores)
	reg := model.NewEmptyRegistry()
	for _, p := range []model.Profile{
		{ID: "worker", Role: model.RolePlanner, Backend: "worker", ProviderKind: "vllm", Endpoint: "http://worker", ContextLength: 32000},
		{ID: "embed", Role: model.RoleEmbedder, Backend: "embed", ProviderKind: "vllm", Endpoint: "http://embed", ContextLength: 8000},
	} {
		if err := reg.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	router := model.NewRouter(reg, nil)
	worker := testllm.New()
	worker.GenerateFn = func(ctx context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	router.RegisterBackend("worker", func(context.Context, model.Profile, *slog.Logger) (any, error) { return worker, nil })
	router.RegisterBackend("embed", func(context.Context, model.Profile, *slog.Logger) (any, error) { return testllm.New(), nil })
	bus := orchestra.NewBus()
	bus.Register("main")
	return newTeamRuntime(TeamConfig{Bus: bus, Router: router, Stores: st, Shared: newToolMux(), OrchID: "main", Log: log})
}

// The team run is no turn's (TEN-326): a turn that ends stops its own
// sub-agents only. Another conversation's worker runs on, in a run that
// isn't terminated with that turn.
func TestTeamRuntime_TurnEndStopsOnlyItsOwnSubAgents(t *testing.T) {
	rt := blockingTeam(t)
	a, endA := context.WithCancel(inConversation("dashboard", "turn-a"))
	b, endB := context.WithCancel(inConversation("discord", "turn-b"))
	defer endB()
	doneOf := func(ctx context.Context, task string) (string, chan struct{}) {
		t.Helper()
		id, err := rt.Spawn(ctx, "worker", task)
		if err != nil {
			t.Fatal(err)
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return id, rt.agents[id].done
	}
	_, doneA := doneOf(a, "a's task")
	idB, doneB := doneOf(b, "b's task")
	endA()
	select {
	case <-doneA:
	case <-time.After(30 * time.Second):
		t.Fatal("a's worker outlived a's turn")
	}
	if st := rt.BudgetStatus(); st.TerminationReason != "" {
		t.Errorf("a's turn ending terminated the team run: %q", st.TerminationReason)
	}
	rt.mu.Lock()
	running := rt.agents[idB].status == "running"
	rt.mu.Unlock()
	if !running {
		t.Error("a's turn ending stopped b's worker")
	}
	endB()
	<-doneB
}

// A run past its wall time, with no agent left in it, blocks nothing (TEN-326):
// the next spawn_agent opens a fresh run, and the orchestrator's tool calls
// and browser go ahead, without anybody having to team_await first.
func TestTeamRuntime_TerminatedIdleRunBlocksNothing(t *testing.T) {
	rt, _ := echoTeam(t, orchestra.Budgets{}, 0)
	terminated := make(chan struct{})
	var once sync.Once
	rt.budget = orchestra.NewGovernor(orchestra.Budgets{MaxWallTime: time.Millisecond}, func(st orchestra.BudgetStatus) {
		if st.TerminationReason != "" {
			once.Do(func() { close(terminated) })
		}
	})
	turn := inConversation("chat", "turn")
	spawnFinished(t, rt, turn, "first") // its run's wall time ends it
	select {
	case <-terminated:
	case <-time.After(30 * time.Second):
		t.Fatal("the run's wall time never ended it")
	}
	// While the terminated run is idle, the browser is free. Checked before the
	// next spawn: that spawn opens a fresh run, whose own 1ms wall time can end
	// it while its worker still runs, and then refusing the browser is right.
	if release, err := rt.budget.AcquireBrowser(); err != nil {
		t.Errorf("the orchestrator's browser after the run's wall time: %v", err)
	} else {
		release()
	}
	local := newToolMux()
	local.Add("orchestrator", spawnTool{rt: rt, timeout: 30 * time.Second})
	orch := composite{local: local, budget: rt.budget, agentID: "main"}
	out, isErr, err := orch.Dispatch(turn, model.ToolCall{Name: "spawn_agent", Arguments: json.RawMessage(`{"role":"worker","task":"second"}`)})
	if err != nil || isErr {
		t.Errorf("spawn_agent after the run's wall time: %q %v", out, err)
	}
	// Join the second before the stores close. team_await returns on news
	// (TEN-529), and the first worker's unreported result is news, so one await
	// can come back with the second still running.
	for strings.Contains(teamAwait(t, rt, turn), "still running") {
	}
}

// The per-turn tool-call cap is a turn's (TEN-326). Serve's surfaces share
// one orchestrator, and so one agent ID: a dashboard turn running past the
// cap used to terminate the team run, cancelling the relay's worker and
// refusing the relay's tool calls. Now only that turn is refused.
func TestTeamRuntime_PerTurnToolCapStopsOnlyThatTurn(t *testing.T) {
	rt := blockingTeam(t)
	rt.budget = orchestra.NewGovernor(orchestra.Budgets{MaxToolCallsPerTurn: 2}, nil)
	var hit string
	local := newToolMux()
	local.Add("orchestrator", spawnTool{rt: rt, timeout: 30 * time.Second})
	local.Add("look", fakePlugin{name: "look", lastHit: &hit})
	orch := composite{local: local, budget: rt.budget, agentID: "main"} // every serve surface's
	call := func(ctx context.Context, name, args string) (string, bool) {
		t.Helper()
		out, isErr, err := orch.Dispatch(ctx, model.ToolCall{Name: name, Arguments: json.RawMessage(args)})
		if err != nil {
			t.Fatal(err)
		}
		return out, isErr
	}
	relay, endRelay := context.WithCancel(inConversation("discord", "turn-r"))
	defer endRelay()
	if out, isErr := call(relay, "spawn_agent", `{"role":"worker","task":"the relay's task"}`); isErr {
		t.Fatalf("the relay's spawn: %s", out)
	}
	dash := inConversation("dashboard", "turn-d")
	for i := range 2 {
		if out, isErr := call(dash, "look_do", `{}`); isErr {
			t.Errorf("the dashboard's call %d, within its cap: %s", i+1, out)
		}
	}
	if out, isErr := call(dash, "look_do", `{}`); !isErr || !strings.Contains(out, "tool calls per turn") {
		t.Errorf("the dashboard's third call went past its cap: %s", out)
	}
	if st := rt.BudgetStatus(); st.TerminationReason != "" {
		t.Errorf("the dashboard's cap terminated the team run: %q", st.TerminationReason)
	}
	rt.mu.Lock()
	var done chan struct{}
	running := false
	for _, m := range rt.agents {
		done, running = m.done, m.status == "running"
	}
	rt.mu.Unlock()
	if !running {
		t.Error("the dashboard's cap stopped the relay's worker")
	}
	if out, isErr := call(relay, "look_do", `{}`); isErr {
		t.Errorf("the relay's call after the dashboard hit its cap: %s", out)
	}
	endRelay()
	<-done
}
