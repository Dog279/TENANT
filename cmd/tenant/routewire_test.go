package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
	"tenant/internal/route"
)

type routeClassifierFunc func(context.Context, string) (route.Signal, error)

func (f routeClassifierFunc) Classify(ctx context.Context, prompt string) (route.Signal, error) {
	return f(ctx, prompt)
}

// TestRouteDefaultSafe is the route_default_safe integration eval: the same
// full turn with routing disabled and with an unreachable aux port must be
// byte-identical at the planner boundary and user-visible result.
func TestRouteDefaultSafe(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, routed bool) ([]byte, []byte, *route.Engine) {
		t.Helper()
		reg := model.NewEmptyRegistry()
		for _, p := range []model.Profile{
			{ID: "small", Role: model.RolePlanner, Backend: "small", ProviderKind: "vllm", Endpoint: "http://small", ContextLength: 32000},
			{ID: "embed", Role: model.RoleEmbedder, Backend: "embed", ProviderKind: "vllm", Endpoint: "http://embed", ContextLength: 8000},
		} {
			if err := reg.Add(p); err != nil {
				t.Fatal(err)
			}
		}
		router := model.NewRouter(reg, nil)
		planner, embedder := testllm.New(), testllm.New()
		var plannerRequest []byte
		planner.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
			b, marshalErr := json.Marshal(req)
			plannerRequest = b
			return &model.GenerateResponse{Text: "byte-identical planner response", FinishReason: "stop"}, marshalErr
		}
		router.RegisterBackend("small", func(context.Context, model.Profile, *slog.Logger) (any, error) { return planner, nil })
		router.RegisterBackend("embed", func(context.Context, model.Profile, *slog.Logger) (any, error) { return embedder, nil })

		cfg := agent.Config{
			AgentID: "main", SessionID: "safe-default", Router: router, Working: working.New(),
			Tools: agent.NewStaticRegistry(), Dispatcher: agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "", false, nil }),
		}
		var engine *route.Engine
		if routed {
			engine = route.NewEngine(route.NewSupra(endpoint, route.DefaultModel, 50*time.Millisecond), t.TempDir(), 10)
			rt := &routeRuntime{
				cfg:    resolvedRouteConfig{Enabled: true, Mode: route.ModeActive, NeverCloud: true, MinComplexityForBig: 4},
				engine: engine, router: router, log: slog.Default(),
			}
			cfg.RouteHook, cfg.RouteActive, cfg.RouteSticky = rt.Hook(), rt.Active, rt.Sticky
		}
		ag, err := agent.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		res, err := ag.Turn(context.Background(), agent.TurnRequest{UserQuery: "explain the failure doctrine", RouteSurface: agent.RouteSurfaceTUI})
		if err != nil {
			t.Fatal(err)
		}
		behavior, err := json.Marshal(struct {
			Response   string
			Iterations int
			Tokens     int
			Truncated  bool
			Error      string
		}{res.Response, res.Iterations, res.Tokens, res.Truncated, fmt.Sprint(res.Error)})
		if err != nil {
			t.Fatal(err)
		}
		return behavior, plannerRequest, engine
	}

	disabledBehavior, disabledRequest, _ := run(t, false)
	downBehavior, downRequest, engine := run(t, true)
	if !bytes.Equal(downBehavior, disabledBehavior) || !bytes.Equal(downRequest, disabledRequest) {
		t.Fatalf("endpoint-down turn diverged from disabled\nbehavior: %s != %s\nrequest: %s != %s",
			downBehavior, disabledBehavior, downRequest, disabledRequest)
	}
	d, ok := engine.NthRecent(1)
	if !ok || d.Status != route.StatusError || d.SelectedTier != route.TierSmall || d.PolicyReason != string(route.ReasonClassifierFailure) {
		t.Fatalf("safe fallback telemetry = %+v", d)
	}
}

func TestRouteRuntimeClassifierDeadlineSelectsFastAndFallsBackSlow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
		want  string
	}{
		{name: "fast", delay: 10 * time.Millisecond, want: "big"},
		{name: "slow", delay: 800 * time.Millisecond, want: "small"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := model.NewEmptyRegistry()
			for _, p := range []model.Profile{
				{ID: "small", Role: model.RolePlanner, Backend: "small", ProviderKind: "vllm", Endpoint: "http://small", ContextLength: 32000},
				{ID: "big", Role: plannerBigRole, Backend: "big", ProviderKind: "vllm", Endpoint: "http://big", ContextLength: 64000},
				{ID: "embed", Role: model.RoleEmbedder, Backend: "embed", ProviderKind: "vllm", Endpoint: "http://embed", ContextLength: 8000},
			} {
				if err := reg.Add(p); err != nil {
					t.Fatal(err)
				}
			}
			router := model.NewRouter(reg, nil)
			for kind, response := range map[string]string{"small": "small", "big": "big", "embed": ""} {
				fake := testllm.New()
				text := response
				fake.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
					return &model.GenerateResponse{Text: text}, nil
				}
				router.RegisterBackend(kind, func(context.Context, model.Profile, *slog.Logger) (any, error) { return fake, nil })
			}
			classifier := routeClassifierFunc(func(ctx context.Context, _ string) (route.Signal, error) {
				select {
				case <-time.After(tc.delay):
					return route.Signal{Label: "big", Complexity: 5}, nil
				case <-ctx.Done():
					return route.Signal{}, ctx.Err()
				}
			})
			rt := &routeRuntime{
				cfg: resolvedRouteConfig{
					Enabled: true, Mode: route.ModeActive, NeverCloud: true,
					MinComplexityForBig: 4, StickySession: true,
				},
				// No decision log: the route hook records after Turn has stopped
				// waiting on it, so a log in t.TempDir() races the dir's removal.
				// This test checks selection only.
				engine: route.NewEngine(classifier, "", 10), router: router, log: slog.Default(),
			}
			ag, err := agent.New(agent.Config{
				AgentID: "main", SessionID: "s", Router: router, Working: working.New(),
				Tools: agent.NewStaticRegistry(), Dispatcher: agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "", false, nil }),
				RouteHook: rt.Hook(), RouteActive: rt.Active, RouteSticky: rt.Sticky,
			})
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			res, err := ag.Turn(context.Background(), agent.TurnRequest{UserQuery: "hard", RouteSurface: agent.RouteSurfaceTUI})
			if err != nil {
				t.Fatal(err)
			}
			if res.Response != tc.want {
				t.Fatalf("response = %q, want %q", res.Response, tc.want)
			}
			if tc.want == "small" && time.Since(start) > 500*time.Millisecond {
				t.Fatalf("slow classifier delayed turn for %s", time.Since(start))
			}
		})
	}
}

func TestRouteRuntimeRolloutSurfaceGates(t *testing.T) {
	relay := agent.RouteRequest{AgentID: "tenant-discord", Surface: agent.RouteSurfaceRelay}
	tuiReq := agent.RouteRequest{AgentID: "main", Surface: agent.RouteSurfaceTUI}
	tests := []struct {
		name string
		cfg  resolvedRouteConfig
		req  agent.RouteRequest
		want bool
	}{
		{"shadow", resolvedRouteConfig{Enabled: true, Mode: route.ModeShadow}, tuiReq, false},
		{"assisted observes", resolvedRouteConfig{Enabled: true, Mode: route.ModeShadow, Assist: true}, tuiReq, false},
		{"assisted accepts big", resolvedRouteConfig{Enabled: true, Mode: route.ModeShadow, Assist: true}, agent.RouteRequest{AgentID: "main", Surface: agent.RouteSurfaceTUI, Override: agent.RouteOverrideBig}, true},
		{"active interactive", resolvedRouteConfig{Enabled: true, Mode: route.ModeActive}, tuiReq, true},
		{"active keeps relay shadow", resolvedRouteConfig{Enabled: true, Mode: route.ModeActive}, relay, false},
		{"per-agent relay opt-in", resolvedRouteConfig{Enabled: true, Mode: route.ModeActive, Agents: map[string]routeAgentConfig{"tenant-discord": {Mode: route.ModeActive}}}, relay, true},
		{"everywhere relay", resolvedRouteConfig{Enabled: true, Mode: route.ModeEverywhere}, relay, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &routeRuntime{cfg: tt.cfg}
			if got := rt.Active(tt.req); got != tt.want {
				t.Fatalf("Active() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestRouteRuntimeHookRecordsShadowObservation(t *testing.T) {
	classifier := routeClassifierFunc(func(_ context.Context, prompt string) (route.Signal, error) {
		if prompt != "typed prompt" {
			t.Fatalf("prompt = %q", prompt)
		}
		return route.Signal{
			Domain: "Software Engineering", Complexity: 4, Code: true, Label: "big",
			Justification: "Concurrent code needs stronger reasoning.", Latency: 7 * time.Millisecond,
		}, nil
	})
	rt := &routeRuntime{
		cfg:    resolvedRouteConfig{Enabled: true, Mode: route.ModeShadow},
		engine: route.NewEngine(classifier, t.TempDir(), 10),
	}
	hook := rt.Hook()
	if hook == nil {
		t.Fatal("enabled runtime returned nil hook")
	}
	obs := hook(context.Background(), agent.RouteRequest{
		AgentID: "main", SessionID: "sess", UserQuery: "typed prompt",
	})
	for _, want := range []string{"shadow", "big", "complexity 4"} {
		if !strings.Contains(obs.Text, want) {
			t.Errorf("summary %q missing %q", obs.Text, want)
		}
	}
	joined := strings.Join(obs.Tags, ",")
	for _, want := range []string{"route:mode=shadow", "route:label=big", "route:complexity=4"} {
		if !strings.Contains(joined, want) {
			t.Errorf("tags %q missing %q", joined, want)
		}
	}
	why := rt.Why(1)
	for _, want := range []string{"Software Engineering", "Concurrent code needs stronger reasoning.", route.HashPrompt("typed prompt")} {
		if !strings.Contains(why, want) {
			t.Errorf("Why = %q; missing %q", why, want)
		}
	}
	if strings.Contains(why, "typed prompt") {
		t.Fatal("Why exposed prompt plaintext")
	}
}

func TestRouteRuntimeAssistedSuggestionBecomesGoldOnBigAcceptance(t *testing.T) {
	engine := route.NewEngine(routeClassifierFunc(func(context.Context, string) (route.Signal, error) {
		return route.Signal{Label: "big", Complexity: 5, Latency: time.Millisecond}, nil
	}), t.TempDir(), 10)
	rt := &routeRuntime{
		cfg:    resolvedRouteConfig{Enabled: true, Mode: route.ModeShadow, Assist: true},
		engine: engine, pendingSuggestions: make(map[string]string), log: slog.Default(),
	}
	first := agent.RouteRequest{
		AgentID: "main", SessionID: "assisted", UserQuery: "design the migration",
		Surface: agent.RouteSurfaceTUI, PlannerRole: model.RolePlanner,
	}
	obs := rt.Hook()(context.Background(), first)
	if !strings.Contains(obs.Text, "router would send this big") || !strings.Contains(obs.Text, "!big") {
		t.Fatalf("assisted observation = %q", obs.Text)
	}
	if d, ok := engine.NthRecent(1); !ok || !d.AssistedSuggestion || d.OperatorConfirmed {
		t.Fatalf("suggested decision = %+v", d)
	}

	accepted := first
	accepted.UserQuery = "apply the migration"
	accepted.Override = agent.RouteOverrideBig
	accepted.Active = rt.Active(accepted)
	rt.Hook()(context.Background(), accepted)
	deadline := time.Now().Add(time.Second)
	for {
		prior, ok := engine.NthRecent(2)
		if ok && prior.OperatorConfirmed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("prior suggestion was not marked gold: %+v", prior)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRouteRuntimeStatusUsesParseableProbeWithoutHistory(t *testing.T) {
	classifier := routeClassifierFunc(func(context.Context, string) (route.Signal, error) {
		return route.Signal{Domain: "General", Complexity: 1, Label: "small", Latency: 3 * time.Millisecond}, nil
	})
	rt := &routeRuntime{
		cfg: resolvedRouteConfig{
			Enabled: true, Mode: route.ModeShadow, Endpoint: "http://localhost:11434", Model: route.DefaultModel,
		},
		engine: route.NewEngine(classifier, t.TempDir(), 10),
	}
	status := rt.Status(context.Background())
	for _, want := range []string{"enabled: true", "mode: shadow", "classifier: healthy", "parseable"} {
		if !strings.Contains(status, want) {
			t.Errorf("Status = %q; missing %q", status, want)
		}
	}
	if got := rt.engine.Recent(0); len(got) != 0 {
		t.Fatalf("health probe entered decision history: %+v", got)
	}
}

func TestRouteRuntimeStatsRendersGraduationCriteria(t *testing.T) {
	rt := &routeRuntime{engine: route.NewEngine(nil, t.TempDir(), 10)}
	got := rt.Stats()
	for _, want := range []string{
		"Graduation gate: FAIL",
		"shadow/assisted observations",
		"parse failure rate",
		"raw classifier big rate",
		"big/small local-failure proxy ratio",
		"classify latency p95",
		"turn failures attributable to routing",
		"threshold",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Stats() = %q; missing %q", got, want)
		}
	}
}

func TestRouteRuntimeStatusAndWhyPreserveClassifierFailure(t *testing.T) {
	classifier := routeClassifierFunc(func(context.Context, string) (route.Signal, error) {
		return route.Signal{Raw: "not parseable", Latency: 11 * time.Millisecond}, errors.New("route: parse: malformed output")
	})
	rt := &routeRuntime{
		cfg:    resolvedRouteConfig{Enabled: true, Mode: route.ModeShadow, Endpoint: "http://localhost:11434", Model: "supra"},
		engine: route.NewEngine(classifier, t.TempDir(), 10),
	}
	if got := rt.Status(context.Background()); !strings.Contains(got, "classifier: unhealthy") {
		t.Fatalf("Status = %q", got)
	}
	hook := rt.Hook()
	hook(context.Background(), agent.RouteRequest{AgentID: "main", SessionID: "sess", UserQuery: "hello"})
	why := rt.Why(1)
	for _, want := range []string{"route: parse: malformed output", "raw output: not parseable"} {
		if !strings.Contains(why, want) {
			t.Errorf("Why = %q; missing %q", why, want)
		}
	}
}

func TestRouteRuntimeDisabled(t *testing.T) {
	rt, err := newRouteRuntime(&launchConfig{}, t.TempDir(), t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Hook() == nil {
		t.Fatal("disabled runtime must retain a live-control hook")
	}
	status := rt.Status(context.Background())
	if !strings.Contains(status, "enabled: false") || !strings.Contains(status, "not probed (disabled)") {
		t.Fatalf("Status = %q", status)
	}
}

func TestRouteRuntimeStatusUsesConfiguredTargetsWithoutLiveRouter(t *testing.T) {
	relayCloud := false
	lc := &launchConfig{
		Provider: "small",
		Providers: map[string]*providerConfig{
			"small": {Kind: "vllm"},
			"big":   {Kind: "openai"},
		},
		Route: &routeConfig{
			Mode: route.ModeShadow, BigRole: "big",
			Agents: map[string]routeAgentConfig{"tenant-discord": {NeverCloud: &relayCloud}},
		},
	}
	rt, err := newRouteRuntime(lc, t.TempDir(), t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	status := rt.Status(context.Background())
	for _, want := range []string{
		"small: vllm-planner (role planner, local=true)",
		"big: big/vllm-planner (role planner-big, local=false)",
		"agent tenant-discord: mode=shadow never_cloud=false PRIVACY OPT-IN",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("status %q missing %q", status, want)
		}
	}
}

func TestRouteRuntimeStatusWarnsWhenBaselinePlannerIsCloud(t *testing.T) {
	lc := &launchConfig{
		Provider: "cloud",
		Providers: map[string]*providerConfig{
			"cloud": {Kind: "openai"},
			"big":   {Kind: "vllm"},
		},
		Route: &routeConfig{Mode: route.ModeShadow, BigRole: "big"},
	}
	rt, err := newRouteRuntime(lc, t.TempDir(), t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status := rt.Status(context.Background()); !strings.Contains(status, "NOTICE: the default planner is cloud-backed") {
		t.Fatalf("status = %q", status)
	}
}

func TestRouteRuntimeRelayIsShadowAndPrivateByDefault(t *testing.T) {
	reg := model.NewEmptyRegistry()
	for _, p := range []model.Profile{
		{ID: "small", Role: model.RolePlanner, Backend: "fake", ProviderKind: "vllm", Endpoint: "http://local", ContextLength: 1000},
		{ID: "big", Role: plannerBigRole, Backend: "fake", ProviderKind: "openai", Endpoint: "https://cloud", ContextLength: 2000},
	} {
		if err := reg.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	router := model.NewRouter(reg, nil)
	classifier := routeClassifierFunc(func(context.Context, string) (route.Signal, error) {
		return route.Signal{Label: "big", Complexity: 5}, nil
	})
	cloudOptIn := false
	rt := &routeRuntime{
		cfg: resolvedRouteConfig{
			Enabled: true, Mode: route.ModeActive, NeverCloud: false, MinComplexityForBig: 4,
			Agents: map[string]routeAgentConfig{},
		},
		engine: route.NewEngine(classifier, t.TempDir(), 10), router: router, log: slog.Default(),
	}
	req := agent.RouteRequest{AgentID: "tenant-discord", UserQuery: "hard", Surface: agent.RouteSurfaceRelay}
	obs := rt.Hook()(context.Background(), req)
	if obs.Applied {
		t.Fatalf("relay observation applied active routing: %+v", obs)
	}
	d, _ := rt.engine.NthRecent(1)
	if d.Mode != route.ModeShadow || d.SelectedTier != route.TierSmall || d.PolicyReason != string(route.ReasonNeverCloud) {
		t.Fatalf("default relay decision = %+v", d)
	}
	rt.cfg.Agents["tenant-discord"] = routeAgentConfig{NeverCloud: &cloudOptIn}
	rt.Hook()(context.Background(), req)
	d, _ = rt.engine.NthRecent(1)
	if d.Mode != route.ModeShadow || d.SelectedTier != route.TierBig {
		t.Fatalf("explicit relay cloud opt-in decision = %+v", d)
	}
}

func TestRouteRuntimeSetModeInstallsLiveBigBinding(t *testing.T) {
	cfgDir := t.TempDir()
	lc := &launchConfig{
		Provider: "small",
		Providers: map[string]*providerConfig{
			"small": {Kind: "echo"},
			"big":   {Kind: "echo"},
		},
		Route: &routeConfig{Enabled: true, Mode: route.ModeShadow, BigRole: "big"},
	}
	if err := lc.Save(cfgDir); err != nil {
		t.Fatal(err)
	}
	reg := model.NewEmptyRegistry()
	if err := reg.Add(model.Profile{ID: "echo-planner", Role: model.RolePlanner, Backend: "echo", ProviderKind: "echo", Endpoint: "echo://local", ContextLength: 1000}); err != nil {
		t.Fatal(err)
	}
	router := model.NewRouter(reg, nil)
	rt, err := newRouteRuntime(lc, cfgDir, t.TempDir(), router, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.ForRole(plannerBigRole); err == nil {
		t.Fatal("planner-big unexpectedly bound before live activation")
	}
	if _, err := rt.SetMode("on"); err != nil {
		t.Fatal(err)
	}
	p, err := router.ForRole(plannerBigRole)
	if err != nil || p.ID != "big/echo-planner" {
		t.Fatalf("planner-big after /route on = %+v / %v", p, err)
	}
}

func TestRouteRuntimeNeverCloudUsesLiveProfileProvenance(t *testing.T) {
	reg := model.NewEmptyRegistry()
	for _, p := range []model.Profile{
		{ID: "small", Role: model.RolePlanner, Backend: "fake", ProviderKind: "vllm", Endpoint: "http://local", ContextLength: 1000},
		{ID: "big", Role: plannerBigRole, Backend: "fake", ProviderKind: "vllm", Endpoint: "http://big", ContextLength: 2000},
	} {
		if err := reg.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	router := model.NewRouter(reg, nil)
	classifier := routeClassifierFunc(func(context.Context, string) (route.Signal, error) {
		return route.Signal{Label: "big", Complexity: 5}, nil
	})
	rt := &routeRuntime{
		cfg: resolvedRouteConfig{
			Enabled: true, Mode: route.ModeActive, NeverCloud: true,
			MinComplexityForBig: 4, StickySession: true,
		},
		engine: route.NewEngine(classifier, t.TempDir(), 10), router: router, log: slog.Default(),
	}
	req := agent.RouteRequest{
		AgentID: "main", SessionID: "s", UserQuery: "hard task",
		Surface: agent.RouteSurfaceTUI, Override: agent.RouteOverrideBig, Active: true,
	}
	first := rt.Hook()(context.Background(), req)
	if !first.Applied || first.Role != plannerBigRole {
		t.Fatalf("local big decision = %+v", first)
	}

	// Upsert the same bound ID as cloud, mirroring a live provider mutation.
	// The next decision must read ProviderKind again, not trust startup state.
	if err := router.AddFallbackProfiles(model.Profile{
		ID: "big", Role: plannerBigRole, Backend: "fake", ProviderKind: "openai",
		Endpoint: "https://api.openai.com", ContextLength: 2000,
	}); err != nil {
		t.Fatal(err)
	}
	second := rt.Hook()(context.Background(), req)
	if !second.Applied || second.Role != model.RolePlanner {
		t.Fatalf("cloud big was not clamped to planner: %+v", second)
	}
	d, ok := rt.engine.NthRecent(1)
	if !ok || d.PolicyReason != string(route.ReasonNeverCloud) || d.SelectedProfileID != "small" {
		t.Fatalf("privacy decision = %+v", d)
	}
}

func TestRouteRuntimeNamedForcePiercesPrivacyAndLogs(t *testing.T) {
	reg := model.NewEmptyRegistry()
	for _, p := range []model.Profile{
		{ID: "small", Role: model.RolePlanner, Backend: "fake", ProviderKind: "vllm", Endpoint: "http://local", ContextLength: 1000},
		{ID: "cloud", Role: model.RolePlanner, Backend: "fake", ProviderKind: "openai", Endpoint: "https://api.openai.com", ContextLength: 2000},
	} {
		if err := reg.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	router := model.NewRouter(reg, nil)
	if err := router.PinRole(model.RolePlanner, "small"); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	rt := &routeRuntime{
		cfg: resolvedRouteConfig{
			Enabled: true, Mode: route.ModeActive, NeverCloud: true, ForceProfile: "cloud",
		},
		engine: route.NewEngine(routeClassifierFunc(func(context.Context, string) (route.Signal, error) {
			return route.Signal{Label: "small", Complexity: 1}, nil
		}), t.TempDir(), 10),
		router: router, log: logger,
	}
	if err := rt.bindForce("cloud"); err != nil {
		t.Fatal(err)
	}
	obs := rt.Hook()(context.Background(), agent.RouteRequest{
		AgentID: "main", SessionID: "s", UserQuery: "force it", Surface: agent.RouteSurfaceTUI, Active: true,
	})
	if !obs.Applied || obs.Role != routeForceRole {
		t.Fatalf("forced observation = %+v", obs)
	}
	d, ok := rt.engine.NthRecent(1)
	if !ok || !d.PrivacyPierced || d.PolicyReason != string(route.ReasonForceProfile) {
		t.Fatalf("forced decision = %+v", d)
	}
	if got := logs.String(); !strings.Contains(got, "PRIVACY OVERRIDE") || !strings.Contains(got, "cloud") {
		t.Fatalf("force was not loudly logged: %q", got)
	}
}

func TestRouteRuntimeForcePersistsStableProviderBindingAcrossSwap(t *testing.T) {
	cfgDir := t.TempDir()
	lc := routeCLITestConfig()
	if err := lc.Save(cfgDir); err != nil {
		t.Fatal(err)
	}
	reg := model.NewEmptyRegistry()
	if err := reg.Add(model.Profile{
		ID: "vllm-planner", Role: model.RolePlanner, Backend: "vllm", ProviderKind: "vllm",
		Endpoint: "http://localhost:8000", Model: "small", ContextLength: 32000,
	}); err != nil {
		t.Fatal(err)
	}
	router := model.NewRouter(reg, nil)
	rt, err := newRouteRuntime(lc, cfgDir, t.TempDir(), router, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Force("vllm-planner"); err != nil {
		t.Fatal(err)
	}
	persisted, err := loadLaunchConfig(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Route.ForceProfile; got != "small/vllm-planner" {
		t.Fatalf("stable force id = %q", got)
	}

	// Swap the normal planner, then simulate a restart with an empty registry.
	// Startup reconstructs the explicitly named provider/profile, not whatever
	// the generic vllm-planner ID happens to mean after the swap.
	persisted.Provider = "big"
	if err := persisted.Save(cfgDir); err != nil {
		t.Fatal(err)
	}
	restartedRouter := model.NewRouter(model.NewEmptyRegistry(), nil)
	restarted, err := newRouteRuntime(persisted, cfgDir, t.TempDir(), restartedRouter, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.BindConfiguredForce(); err != nil {
		t.Fatal(err)
	}
	p, err := restartedRouter.ForRole(routeForceRole)
	if err != nil {
		t.Fatal(err)
	}
	if p.ProviderKind != "vllm" || p.Model != "small" {
		t.Fatalf("restarted force binding = %+v", p)
	}
}
