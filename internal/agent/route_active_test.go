package agent_test

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
)

const testBigRole model.Role = "planner-big"

func newRouteTestAgent(t *testing.T, hook agent.RouteHook, small, big, embed *testllm.Fake, tools []model.ToolSpec, disp agent.ToolDispatcher, options ...func(*agent.Config)) *agent.Agent {
	t.Helper()
	reg := model.NewEmptyRegistry()
	profiles := []model.Profile{
		{ID: "small", Role: model.RolePlanner, Backend: "small", Endpoint: "test://small", ContextLength: 32000, OperationalContextBudget: 24000, MaxToolsPerCall: 8, MaxParallelTools: 1, PlanLoopCeiling: 4},
		{ID: "big", Role: testBigRole, Backend: "big", Endpoint: "test://big", ContextLength: 64000, OperationalContextBudget: 50000, MaxToolsPerCall: 8, MaxParallelTools: 1, PlanLoopCeiling: 4},
		{ID: "embed", Role: model.RoleEmbedder, Backend: "embed", Endpoint: "test://embed", ContextLength: 8192, OperationalContextBudget: 8192},
	}
	for _, p := range profiles {
		if err := reg.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	router := model.NewRouter(reg, slog.Default())
	router.RegisterBackend("small", func(context.Context, model.Profile, *slog.Logger) (any, error) { return small, nil })
	router.RegisterBackend("big", func(context.Context, model.Profile, *slog.Logger) (any, error) { return big, nil })
	router.RegisterBackend("embed", func(context.Context, model.Profile, *slog.Logger) (any, error) { return embed, nil })
	registry := agent.NewStaticRegistry()
	for _, tool := range tools {
		registry.Register(tool)
	}
	if disp == nil {
		disp = agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "ok", false, nil })
	}
	cfg := agent.Config{
		AgentID: "route-active", SessionID: "session", Router: router, Working: working.New(),
		Tools: registry, Dispatcher: disp, RouteHook: hook,
		RouteActive: func(agent.RouteRequest) bool { return true },
		RouteSticky: func(agent.RouteRequest) bool { return true },
	}
	for _, option := range options {
		option(&cfg)
	}
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAgentStickyPinsPlannerAfterRouteDeadline(t *testing.T) {
	small, big, embed := testllm.New(), testllm.New(), testllm.New()
	var generations atomic.Int32
	small.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		if generations.Add(1) == 1 {
			return &model.GenerateResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "echo"}}}, nil
		}
		return &model.GenerateResponse{Text: "done"}, nil
	}
	seen := make(chan model.Role, 2)
	var routeCalls atomic.Int32
	hook := func(_ context.Context, req agent.RouteRequest) agent.RouteObservation {
		seen <- req.StickyRole
		if routeCalls.Add(1) == 1 {
			time.Sleep(350 * time.Millisecond)
		}
		return agent.RouteObservation{Role: model.RolePlanner, Applied: true, StickySession: true}
	}
	a := newRouteTestAgent(t, hook, small, big, embed,
		[]model.ToolSpec{{Name: "echo"}},
		agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "ok", false, nil }))
	if _, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "tool", RouteSurface: agent.RouteSurfaceTUI}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "again", RouteSurface: agent.RouteSurfaceTUI}); err != nil {
		t.Fatal(err)
	}
	if first, second := <-seen, <-seen; first != "" || second != model.RolePlanner {
		t.Fatalf("sticky roles = %q then %q, want empty then planner", first, second)
	}
}

func TestAgentShadowRouteKeepsConfiguredHookLifetime(t *testing.T) {
	small, big, embed := testllm.New(), testllm.New(), testllm.New()
	small.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "small"}, nil
	}
	done := make(chan error, 1)
	hook := func(ctx context.Context, _ agent.RouteRequest) agent.RouteObservation {
		time.Sleep(300 * time.Millisecond)
		done <- ctx.Err()
		return agent.RouteObservation{}
	}
	a := newRouteTestAgent(t, hook, small, big, embed, nil, nil, func(cfg *agent.Config) {
		cfg.RouteActive = func(agent.RouteRequest) bool { return false }
	})
	if _, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "shadow", RouteSurface: agent.RouteSurfaceRelay}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shadow hook was capped by active deadline: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shadow hook did not finish")
	}
}

func TestAgentAllowsConfiguredActiveRoutingOnRelay(t *testing.T) {
	small, big, embed := testllm.New(), testllm.New(), testllm.New()
	small.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "small"}, nil
	}
	big.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "big"}, nil
	}
	a := newRouteTestAgent(t, func(context.Context, agent.RouteRequest) agent.RouteObservation {
		return agent.RouteObservation{Role: testBigRole, Applied: true}
	}, small, big, embed, nil, nil)
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "relay", RouteSurface: agent.RouteSurfaceRelay})
	if err != nil {
		t.Fatal(err)
	}
	if res.Response != "big" {
		t.Fatalf("relay response = %q, want configured big route", res.Response)
	}
}

func TestAgentRoutedRoleResolutionSharesHardDeadline(t *testing.T) {
	small, big, embed := testllm.New(), testllm.New(), testllm.New()
	small.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "small"}, nil
	}
	a := newRouteTestAgent(t, func(context.Context, agent.RouteRequest) agent.RouteObservation {
		return agent.RouteObservation{Role: testBigRole, Applied: true}
	}, small, big, embed, nil, nil)
	release := make(chan struct{})
	a.Router().RegisterBackend("big", func(context.Context, model.Profile, *slog.Logger) (any, error) {
		<-release // deliberately ignores the route context
		return big, nil
	})
	start := time.Now()
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "route", RouteSurface: agent.RouteSurfaceTUI})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if res.Response != "small" || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("resolution fallback = %q after %s", res.Response, time.Since(start))
	}
}

func TestAgentRejectsProfileChangedAfterPolicySnapshot(t *testing.T) {
	small, big, embed := testllm.New(), testllm.New(), testllm.New()
	small.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "small"}, nil
	}
	var a *agent.Agent
	hook := func(context.Context, agent.RouteRequest) agent.RouteObservation {
		if err := a.Router().AddFallbackProfiles(model.Profile{
			ID: "big", Role: testBigRole, Backend: "big", ProviderKind: "openai",
			Endpoint: "https://cloud", ContextLength: 64000,
		}); err != nil {
			panic(err)
		}
		return agent.RouteObservation{
			Role: testBigRole, Applied: true, ConstrainProfiles: true,
			AllowedProfiles:   map[string]string{"big": "vllm"},
			ExpectedProfileID: "big", ExpectedProviderKind: "vllm",
		}
	}
	a = newRouteTestAgent(t, hook, small, big, embed, nil, nil)
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "private", RouteSurface: agent.RouteSurfaceTUI})
	if err != nil {
		t.Fatal(err)
	}
	if res.Response != "small" {
		t.Fatalf("response = %q, want planner fallback", res.Response)
	}
}

func TestAgentActiveRouteDeadlineJoin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
		want  string
	}{
		{name: "fast classifier routes", delay: 10 * time.Millisecond, want: "big"},
		{name: "slow classifier keeps planner", delay: 800 * time.Millisecond, want: "small"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			small, big, embed := testllm.New(), testllm.New(), testllm.New()
			small.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
				return &model.GenerateResponse{Text: "small", FinishReason: "stop"}, nil
			}
			big.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
				return &model.GenerateResponse{Text: "big", FinishReason: "stop"}, nil
			}
			hook := func(context.Context, agent.RouteRequest) agent.RouteObservation {
				time.Sleep(tc.delay)
				return agent.RouteObservation{Role: testBigRole, Applied: true}
			}
			a := newRouteTestAgent(t, hook, small, big, embed, nil, nil)
			start := time.Now()
			res, err := a.Turn(context.Background(), agent.TurnRequest{
				UserQuery: "route me", RouteSurface: agent.RouteSurfaceTUI,
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Response != tc.want {
				t.Fatalf("response = %q, want %q", res.Response, tc.want)
			}
			if tc.delay > 250*time.Millisecond && time.Since(start) > 500*time.Millisecond {
				t.Fatalf("slow route delayed turn for %s", time.Since(start))
			}
		})
	}
}

func TestAgentStickyRoutePinsAfterModelEmitsToolCall(t *testing.T) {
	small, big, embed := testllm.New(), testllm.New(), testllm.New()
	bigCalls := 0
	big.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		bigCalls++
		if bigCalls == 1 {
			return &model.GenerateResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "echo"}}, FinishReason: "tool_calls"}, nil
		}
		return &model.GenerateResponse{Text: "done", FinishReason: "stop"}, nil
	}
	seenSticky := make(chan model.Role, 2)
	hook := func(_ context.Context, req agent.RouteRequest) agent.RouteObservation {
		seenSticky <- req.StickyRole
		return agent.RouteObservation{Role: testBigRole, Applied: true, StickySession: true}
	}
	a := newRouteTestAgent(t, hook, small, big, embed,
		[]model.ToolSpec{{Name: "echo"}},
		agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "ok", false, nil }))

	if _, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "use a tool", RouteSurface: agent.RouteSurfaceTUI}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "again", RouteSurface: agent.RouteSurfaceTUI}); err != nil {
		t.Fatal(err)
	}
	if first, second := <-seenSticky, <-seenSticky; first != "" || second != testBigRole {
		t.Fatalf("sticky roles = %q then %q, want empty then %q", first, second, testBigRole)
	}
}

func TestParseRoutePrefix(t *testing.T) {
	for _, tc := range []struct {
		in       string
		query    string
		override agent.RouteOverride
	}{
		{"!big solve this", "solve this", agent.RouteOverrideBig},
		{" !local keep private ", "keep private", agent.RouteOverrideLocal},
		{"ordinary", "ordinary", ""},
	} {
		query, override := agent.ParseRoutePrefix(tc.in)
		if query != tc.query || override != tc.override {
			t.Errorf("ParseRoutePrefix(%q) = %q, %q", tc.in, query, override)
		}
	}
}

func TestAgentTurnStripsRoutePrefixBeforeEventsHookAndModel(t *testing.T) {
	small, big, embed := testllm.New(), testllm.New(), testllm.New()
	modelQuery := make(chan string, 1)
	small.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				modelQuery <- req.Messages[i].Content
				break
			}
		}
		return &model.GenerateResponse{Text: "ok"}, nil
	}
	hookReq := make(chan agent.RouteRequest, 1)
	var starts []string
	a := newRouteTestAgent(t, func(_ context.Context, req agent.RouteRequest) agent.RouteObservation {
		hookReq <- req
		return agent.RouteObservation{Role: model.RolePlanner, Applied: true}
	}, small, big, embed, nil, nil, func(cfg *agent.Config) {
		cfg.Observer = func(ev agent.Event) {
			if ev.Kind == agent.EventTurnStart {
				starts = append(starts, ev.Text)
			}
		}
	})
	if _, err := a.Turn(context.Background(), agent.TurnRequest{
		UserQuery: "!big solve this", RouteSurface: agent.RouteSurfaceTUI,
	}); err != nil {
		t.Fatal(err)
	}
	seen := <-hookReq
	if seen.UserQuery != "solve this" || seen.Override != agent.RouteOverrideBig {
		t.Fatalf("hook request = %+v", seen)
	}
	if got := <-modelQuery; got != "solve this" {
		t.Fatalf("model query = %q", got)
	}
	if len(starts) != 1 || starts[0] != "solve this" {
		t.Fatalf("turn starts = %v", starts)
	}
	if _, err := a.Turn(context.Background(), agent.TurnRequest{
		UserQuery: "!local", RouteSurface: agent.RouteSurfaceTUI,
	}); err == nil {
		t.Fatal("bare route prefix did not produce an empty-query error")
	}
}
