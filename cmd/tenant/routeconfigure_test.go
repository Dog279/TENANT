package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tenant/internal/model"
	"tenant/internal/route"
)

func TestRouteLadderWord(t *testing.T) {
	cases := []struct {
		cfg  resolvedRouteConfig
		want string
	}{
		{resolvedRouteConfig{}, "off"},
		{resolvedRouteConfig{Enabled: true, Mode: route.ModeShadow}, "shadow"},
		{resolvedRouteConfig{Enabled: true, Mode: route.ModeShadow, Assist: true}, "assisted"},
		{resolvedRouteConfig{Enabled: true, Mode: route.ModeActive}, "on"},
		{resolvedRouteConfig{Enabled: true, Mode: route.ModeEverywhere}, "everywhere"},
	}
	for _, c := range cases {
		if got := routeLadderWord(c.cfg); got != c.want {
			t.Errorf("routeLadderWord(%+v) = %q, want %q", c.cfg, got, c.want)
		}
	}
}

func TestResolveRouteConfig_NilRouteStillInheritsClassifierEndpoint(t *testing.T) {
	// Regression (2026-07-16 dead classifier): a config with no route block
	// resolved Endpoint="" because the early return skipped inheritance; the
	// dormant Supra built from that stayed dead across a live enable.
	got, err := resolveRouteConfig(&launchConfig{
		Embed: &providerConfig{Kind: "ollama", Endpoint: "http://embed-box.local:11434"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != "http://embed-box.local:11434" {
		t.Fatalf("nil-route endpoint = %q, want the inherited embed endpoint", got.Endpoint)
	}
	got, err = resolveRouteConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint == "" {
		t.Fatal("nil config resolved an empty classifier endpoint")
	}
}

func classifierStub(t *testing.T, hits chan<- struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case hits <- struct{}{}:
		default:
		}
		_, _ = w.Write([]byte(`{"response":"Domain: General | Complexity: 1 | Math: False | Code: False | Route: small model | Justification: Simple."}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRouteRuntimeLiveConfigChangeRebuildsClassifier(t *testing.T) {
	// A session launched with NO route block must (a) already carry the
	// inherited embed endpoint in its classifier and (b) swap in a fresh
	// classifier when a persisted change alters the endpoint — no restart.
	hits1 := make(chan struct{}, 1)
	srv1 := classifierStub(t, hits1)
	hits2 := make(chan struct{}, 1)
	srv2 := classifierStub(t, hits2)

	cfgDir := t.TempDir()
	lc := &launchConfig{
		Provider:  "small",
		Providers: map[string]*providerConfig{"small": {Kind: "echo"}},
		Embed:     &providerConfig{Kind: "ollama", Endpoint: srv1.URL},
	}
	if err := lc.Save(cfgDir); err != nil {
		t.Fatal(err)
	}
	rt, err := newRouteRuntime(lc, cfgDir, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := rt.engine.Probe(context.Background()); err != nil {
		t.Fatalf("launch-time classifier probe failed (endpoint not inherited?): %v", err)
	}
	select {
	case <-hits1:
	case <-time.After(2 * time.Second):
		t.Fatal("probe never reached the inherited embed endpoint")
	}

	// Persist an explicit classifier endpoint (loopback), then trigger any
	// Set* — the runtime must rebuild the classifier from the new config.
	lc2, err := loadLaunchConfig(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	lc2.Route = &routeConfig{Provider: &routeProviderConfig{Endpoint: srv2.URL}}
	if err := lc2.Save(cfgDir); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.SetPolicy("sticky_session", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.engine.Probe(context.Background()); err != nil {
		t.Fatalf("post-change classifier probe failed: %v", err)
	}
	select {
	case <-hits2:
	case <-time.After(2 * time.Second):
		t.Fatal("classifier still pinned to the launch-time endpoint after a live config change")
	}
}

func TestRouteConfigureInfoInventoriesProviders(t *testing.T) {
	cfgDir := t.TempDir()
	lc := &launchConfig{
		Provider: "small",
		Providers: map[string]*providerConfig{
			"small":  {Kind: "vllm", Model: "qwen"},
			"cloud1": {Kind: "openai"}, // empty model → kind default
			"cloud2": {Kind: "anthropic", Model: "claude-x"},
			"dev":    {Kind: "echo"}, // offline stub — excluded
		},
		Route: &routeConfig{Enabled: true, Mode: route.ModeShadow, BigRole: "cloud2"},
	}
	if err := lc.Save(cfgDir); err != nil {
		t.Fatal(err)
	}
	rt, err := newRouteRuntime(lc, cfgDir, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	info, err := rt.ConfigureInfo()
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Cloud) != 2 || info.Cloud[0].Name != "cloud1" || info.Cloud[1].Name != "cloud2" {
		t.Fatalf("Cloud = %+v, want cloud1+cloud2 sorted", info.Cloud)
	}
	if info.Cloud[0].Model != "gpt-4o" {
		t.Errorf("empty cloud model should resolve to the kind default, got %q", info.Cloud[0].Model)
	}
	if info.Cloud[1].Model != "claude-x" || info.Cloud[1].Kind != "anthropic" {
		t.Errorf("Cloud[1] = %+v", info.Cloud[1])
	}
	if len(info.Local) != 1 || info.Local[0].Name != "small" || !info.Local[0].Active {
		t.Fatalf("Local = %+v, want only the active vllm provider (echo excluded)", info.Local)
	}
	if info.BigRole != "cloud2" || info.Mode != "shadow" {
		t.Errorf("BigRole/Mode = %q/%q", info.BigRole, info.Mode)
	}
	if !info.NeverCloud || info.MinComplexityForBig != 4 || !info.StickySession {
		t.Errorf("policy defaults = %+v", info)
	}
}

func TestRouteRuntimeSetBigPersistsAndBindsLive(t *testing.T) {
	cfgDir := t.TempDir()
	lc := &launchConfig{
		Provider: "small",
		Providers: map[string]*providerConfig{
			"small": {Kind: "echo"},
			"big":   {Kind: "echo"},
		},
		Route: &routeConfig{Enabled: true, Mode: route.ModeShadow},
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

	status, err := rt.SetBig("big", "echo-xl")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "big leg") || !strings.Contains(status, "big / echo-xl") {
		t.Errorf("status = %q", status)
	}
	p, err := router.ForRole(plannerBigRole)
	if err != nil || p.ID != "big/echo-planner" {
		t.Fatalf("planner-big after SetBig = %+v / %v", p, err)
	}
	persisted, err := loadLaunchConfig(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Route.BigRole != "big" || persisted.Providers["big"].Model != "echo-xl" {
		t.Fatalf("persisted route = %+v providers.big = %+v", persisted.Route, persisted.Providers["big"])
	}
	rt.mu.RLock()
	got := rt.cfg.BigRole
	rt.mu.RUnlock()
	if got != "big" {
		t.Fatalf("live cfg.BigRole = %q", got)
	}

	if _, err := rt.SetBig("nope", "x"); err == nil || !strings.Contains(err.Error(), "no provider") {
		t.Fatalf("unknown provider error = %v", err)
	}
}

func TestRouteRuntimeSetBigRollsBackOnBindFailure(t *testing.T) {
	// An anthropic provider with no resolvable key fails the planner-big
	// build with routing enabled; SetBig must roll the already-saved config
	// back so disk, r.cfg, and the router agree that nothing changed.
	t.Setenv("ANTHROPIC_API_KEY", "")
	cfgDir := t.TempDir()
	lc := &launchConfig{
		Provider: "small",
		Providers: map[string]*providerConfig{
			"small":    {Kind: "echo"},
			"bigcloud": {Kind: "anthropic"},
		},
		Route: &routeConfig{Enabled: true, Mode: route.ModeShadow},
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

	if _, err := rt.SetBig("bigcloud", "claude-x"); err == nil {
		t.Fatal("SetBig with an unbuildable provider should fail")
	}
	persisted, err := loadLaunchConfig(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Route.BigRole != "" || persisted.Providers["bigcloud"].Model != "" {
		t.Fatalf("failed SetBig leaked to disk: big_role=%q model=%q",
			persisted.Route.BigRole, persisted.Providers["bigcloud"].Model)
	}
	if _, err := router.ForRole(plannerBigRole); err == nil {
		t.Fatal("planner-big unexpectedly bound after failed SetBig")
	}
	rt.mu.RLock()
	got := rt.cfg.BigRole
	rt.mu.RUnlock()
	if got != "" {
		t.Fatalf("live cfg.BigRole = %q after failed SetBig", got)
	}
}

func TestRouteRuntimeSetPolicyPersistsExplicitValues(t *testing.T) {
	cfgDir := t.TempDir()
	rt, err := newRouteRuntime(&launchConfig{}, cfgDir, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := rt.SetPolicy("never_cloud", "false"); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.SetPolicy("min_complexity_for_big", "3"); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.SetPolicy("sticky_session", "false"); err != nil {
		t.Fatal(err)
	}

	persisted, err := loadLaunchConfig(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	// Pointer booleans must be written EXPLICITLY (nil means deny-by-default).
	if persisted.Route.NeverCloud == nil || *persisted.Route.NeverCloud {
		t.Fatalf("persisted never_cloud = %v, want explicit false", persisted.Route.NeverCloud)
	}
	if persisted.Route.StickySession == nil || *persisted.Route.StickySession {
		t.Fatalf("persisted sticky_session = %v, want explicit false", persisted.Route.StickySession)
	}
	if persisted.Route.MinComplexityForBig != 3 {
		t.Fatalf("persisted min_complexity_for_big = %d", persisted.Route.MinComplexityForBig)
	}
	rt.mu.RLock()
	cfg := rt.cfg
	rt.mu.RUnlock()
	if cfg.NeverCloud || cfg.StickySession || cfg.MinComplexityForBig != 3 {
		t.Fatalf("live cfg = %+v", cfg)
	}

	for _, bad := range [][2]string{
		{"min_complexity_for_big", "9"},
		{"min_complexity_for_big", "0"}, // resolveRouteConfig tolerates 0 as "unset"; SetPolicy must not
		{"min_complexity_for_big", "nope"},
		{"never_cloud", "maybe"},
		{"frobnicate", "x"},
	} {
		if _, err := rt.SetPolicy(bad[0], bad[1]); err == nil {
			t.Errorf("SetPolicy(%q, %q) unexpectedly succeeded", bad[0], bad[1])
		}
	}
	// Rejected writes must not have leaked to disk.
	persisted, err = loadLaunchConfig(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Route.MinComplexityForBig != 3 {
		t.Fatalf("rejected write leaked: min_complexity_for_big = %d", persisted.Route.MinComplexityForBig)
	}
}
