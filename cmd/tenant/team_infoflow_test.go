package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
	"tenant/internal/orchestra"
	"tenant/internal/plugins/osys"
)

// TEN-336 end to end through the real team runtime. The orchestrator reads a
// secret and spawns a task quoting it; spawn_agent is outbound, so that asks
// first. The approved flow seeds the child, whose (echo) answer quotes its
// task, and team_await hands the flow back, labelling the result.
func TestTeamSpawnAndAwaitCarryTheSensitiveFlow(t *testing.T) {
	const secret = "TEN336_TEAM_SENTINEL_0b5e27"

	run := func(t *testing.T, approve bool) (*agent.TurnResult, *TeamRuntime, []string) {
		t.Helper()
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		// Sub-agents run on the echo backend, which answers by quoting its task.
		c := &commonFlags{backend: "echo", agent: "main", dataDir: t.TempDir(), cfgDir: t.TempDir()}
		childRouter, err := buildRouter(c, log)
		if err != nil {
			t.Fatal(err)
		}
		st, closeStores, err := openStores(c)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(closeStores)

		var mu sync.Mutex
		var approvals []string
		confirm := func(_ context.Context, _, detail string) bool {
			mu.Lock()
			approvals = append(approvals, detail)
			mu.Unlock()
			return approve
		}
		bus := orchestra.NewBus()
		bus.Register("main")
		rt := newTeamRuntime(TeamConfig{
			Bus: bus, Router: childRouter, Stores: st, Shared: newToolMux(), OrchID: "main", Log: log,
			ConfirmSensitiveEgress: confirm,
		})
		team := spawnTool{rt: rt, timeout: 30 * time.Second}

		var osRead model.ToolSpec
		for _, spec := range osys.NewDispatcher(nil, osys.Policy{}).Tools() {
			if spec.Name == "os_read_file" {
				osRead = spec
			}
		}
		registry := agent.NewStaticRegistry()
		registry.Register(osRead)
		for _, spec := range team.Tools() {
			registry.Register(spec)
		}
		disp := agent.DispatcherFunc(func(ctx context.Context, call model.ToolCall) (string, bool, error) {
			if call.Name == "os_read_file" {
				return "api_token=" + secret, false, nil
			}
			return team.Dispatch(ctx, call)
		})

		// The orchestrator runs on a scripted fake model: read, spawn, await.
		reg := model.NewEmptyRegistry()
		for _, p := range []model.Profile{
			{ID: "orch", Role: model.RolePlanner, Backend: "orch", ProviderKind: "vllm", Endpoint: "http://orch", ContextLength: 32000},
			{ID: "embed", Role: model.RoleEmbedder, Backend: "embed", ProviderKind: "vllm", Endpoint: "http://embed", ContextLength: 8000},
		} {
			if err := reg.Add(p); err != nil {
				t.Fatal(err)
			}
		}
		router := model.NewRouter(reg, nil)
		planner, embedder := testllm.New(), testllm.New()
		script := []model.ToolCall{
			teamCall("read", "os_read_file", map[string]any{"path": "credentials.txt"}),
			teamCall("spawn", "spawn_agent", map[string]any{"role": "checker", "task": "check whether " + secret + " still works"}),
			teamCall("await", "team_await", map[string]any{}),
		}
		step := 0
		planner.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
			step++
			if step <= len(script) {
				return &model.GenerateResponse{ToolCalls: []model.ToolCall{script[step-1]}}, nil
			}
			return &model.GenerateResponse{Text: "done", FinishReason: "stop"}, nil
		}
		router.RegisterBackend("orch", func(context.Context, model.Profile, *slog.Logger) (any, error) { return planner, nil })
		router.RegisterBackend("embed", func(context.Context, model.Profile, *slog.Logger) (any, error) { return embedder, nil })

		orch, err := agent.New(agent.Config{
			AgentID: "main", SessionID: "team-flow", Router: router, Working: working.New(),
			Tools: registry, Dispatcher: disp, ConfirmSensitiveEgress: confirm,
		})
		if err != nil {
			t.Fatal(err)
		}
		res, err := orch.Turn(context.Background(), agent.TurnRequest{UserQuery: "delegate the check"})
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		return res, rt, append([]string(nil), approvals...)
	}

	t.Run("approved spawn carries the flow out and back", func(t *testing.T) {
		res, rt, approvals := run(t, true)
		if len(approvals) != 1 || !strings.Contains(approvals[0], "spawn_agent") {
			t.Fatalf("approvals = %q; the spawn quoting the secret must ask exactly once", approvals)
		}
		if len(rt.order) != 0 {
			t.Fatalf("team_await returned the child's result but still holds %v", rt.order)
		}
		var awaited *agent.ToolCallResult
		for i := range res.ToolTrace {
			if res.ToolTrace[i].Call.Name == "team_await" {
				awaited = &res.ToolTrace[i]
			}
		}
		if awaited == nil || !strings.Contains(awaited.Result, secret) {
			t.Fatalf("team_await result = %+v, want the child's answer quoting the task", awaited)
		}
		var labelled bool
		for _, l := range awaited.Labels {
			labelled = labelled || (l.Sensitivity == model.SensitivitySensitive && l.Provenance == model.ProvenanceLocalFile)
		}
		if !labelled {
			t.Fatalf("team_await labels = %+v, want the child's local_file provenance", awaited.Labels)
		}
	})

	t.Run("denied spawn starts no sub-agent", func(t *testing.T) {
		_, rt, approvals := run(t, false)
		if len(approvals) != 1 {
			t.Fatalf("approvals = %q, want the one denied spawn", approvals)
		}
		if status := rt.BudgetStatus(); status.ActiveSubagents != 0 || len(rt.order) != 0 {
			t.Fatalf("a denied spawn still started %d sub-agent(s)", len(rt.order))
		}
	})
}

func teamCall(id, name string, args map[string]any) model.ToolCall {
	raw, _ := json.Marshal(args)
	return model.ToolCall{ID: id, Name: name, Arguments: raw}
}
