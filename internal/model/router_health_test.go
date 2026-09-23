package model

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// TEN-358: the router hands its health-gating config to every fallback chain it
// builds, so TEN-282's gate can be turned on from config instead of being
// reachable only from tests. These reuse fallback_health_test.go's fake clock
// and latency-injecting links.

// newGatedRouter binds a planner chain [primary → backup] over the given LLMs.
func newGatedRouter(t *testing.T, primary, backup LLM) *Router {
	t.Helper()
	reg := NewEmptyRegistry()
	if err := reg.Add(Profile{ID: "primary", Role: RolePlanner, Backend: "primary", Endpoint: "http://primary", ContextLength: 1000}); err != nil {
		t.Fatal(err)
	}
	r := NewRouter(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for kind, llm := range map[string]LLM{"primary": primary, "backup": backup} {
		r.RegisterBackend(kind, func(context.Context, Profile, *slog.Logger) (any, error) { return llm, nil })
	}
	if err := r.AddFallbackProfiles(Profile{ID: "backup", Role: RolePlanner, Backend: "backup", Endpoint: "http://backup", ContextLength: 1000}); err != nil {
		t.Fatal(err)
	}
	r.SetFallbackChain(RolePlanner, []string{"backup"})
	return r
}

func plannerChain(t *testing.T, r *Router) *FallbackLLM {
	t.Helper()
	llm, _, err := r.LLMForRole(context.Background(), RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	fb, ok := llm.(*FallbackLLM)
	if !ok {
		t.Fatalf("planner resolved to %T, want *FallbackLLM", llm)
	}
	return fb
}

func gateOf(fb *FallbackLLM) HealthConfig {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.health
}

func TestRouterHealthGatingDemotesSlowPrimary(t *testing.T) {
	clk := newClock()
	primary := &slowLink{
		inner:   &scriptLLM{name: "primary", genText: "slow-ok"},
		clk:     clk,
		latency: func() time.Duration { return 2 * time.Second },
	}
	backup := &slowLink{
		inner:   &scriptLLM{name: "backup", genText: "fast-ok"},
		clk:     clk,
		latency: func() time.Duration { return 50 * time.Millisecond },
	}
	r := newGatedRouter(t, primary, backup)
	cfg := HealthConfig{SlowThreshold: time.Second, MinSamples: 2, Alpha: 1, Cooldown: time.Minute}
	r.SetHealthGating(cfg)

	fb := plannerChain(t, r)
	fb.now = clk.now // measure with the links' clock instead of wall time
	for i := 0; i < cfg.MinSamples; i++ {
		resp, err := fb.Generate(context.Background(), GenerateRequest{})
		if err != nil || resp.Text != "slow-ok" {
			t.Fatalf("warmup call %d: want the slow primary; resp=%+v err=%v", i, resp, err)
		}
	}
	resp, err := fb.Generate(context.Background(), GenerateRequest{})
	if err != nil || resp.Text != "fast-ok" {
		t.Fatalf("once the gate trips the chain should skip the slow primary; resp=%+v err=%v", resp, err)
	}
	if plannerChain(t, r) != fb {
		t.Fatal("the gated chain must stay cached so the demotion carries into the next turn")
	}
}

func TestRouterHealthGatingReachesEveryNewChain(t *testing.T) {
	r := newGatedRouter(t, &scriptLLM{name: "primary"}, &scriptLLM{name: "backup"})
	cfg := HealthConfig{SlowThreshold: 20 * time.Second}
	r.SetHealthGating(cfg)
	if got := r.HealthGating(); got != cfg {
		t.Fatalf("HealthGating() = %+v, want %+v", got, cfg)
	}
	if got, want := gateOf(plannerChain(t, r)), cfg.Normalized(); got != want {
		t.Errorf("cached chain gate = %+v, want %+v", got, want)
	}
	// Filtered resolutions build their own uncached chain; it is gated too.
	llm, _, err := r.LLMForRoleFiltered(context.Background(), RolePlanner, func(Profile) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	fb, ok := llm.(*FallbackLLM)
	if !ok {
		t.Fatalf("filtered planner resolved to %T, want *FallbackLLM", llm)
	}
	if got, want := gateOf(fb), cfg.Normalized(); got != want {
		t.Errorf("filtered chain gate = %+v, want %+v", got, want)
	}
}

func TestRouterSetHealthGatingReconfiguresCachedChain(t *testing.T) {
	r := newGatedRouter(t, &scriptLLM{name: "primary"}, &scriptLLM{name: "backup"})
	fb := plannerChain(t, r)
	if got := gateOf(fb); got.SlowThreshold != 0 {
		t.Fatalf("gate is on before anything configured it: %+v", got)
	}

	cfg := HealthConfig{SlowThreshold: 20 * time.Second, MinSamples: 3}
	r.SetHealthGating(cfg)
	if plannerChain(t, r) != fb {
		t.Fatal("SetHealthGating rebuilt the cached chain, which drops its cooldown state")
	}
	if got, want := gateOf(fb), cfg.Normalized(); got != want {
		t.Fatalf("cached chain gate = %+v, want %+v", got, want)
	}

	r.SetHealthGating(HealthConfig{})
	if got := gateOf(fb); got.SlowThreshold != 0 {
		t.Fatalf("the zero config should turn the gate off; got %+v", got)
	}
}

func TestRouterEffortVariantIsNotHealthGated(t *testing.T) {
	r := newGatedRouter(t, &scriptLLM{name: "primary"}, &scriptLLM{name: "backup"})
	r.SetHealthGating(HealthConfig{SlowThreshold: time.Second})
	base, err := r.ForRole(RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	llm, ok := r.LLMWithEffort(context.Background(), base, &scriptLLM{name: "base"}, "high")
	if !ok {
		t.Fatal("effort variant was not built")
	}
	fb, isChain := llm.(*FallbackLLM)
	if !isChain {
		t.Fatalf("effort variant resolved to %T, want *FallbackLLM", llm)
	}
	if got := gateOf(fb); got.SlowThreshold != 0 {
		t.Fatalf("a higher-effort variant is expected to be slower and must not be gated; got %+v", got)
	}
}

func TestHealthConfigNormalized(t *testing.T) {
	tests := []struct {
		name string
		in   HealthConfig
		want HealthConfig
	}{
		{
			name: "off is returned unchanged",
			in:   HealthConfig{MinSamples: -1, Alpha: 7},
			want: HealthConfig{MinSamples: -1, Alpha: 7},
		},
		{
			name: "defaults fill an enabled config",
			in:   HealthConfig{SlowThreshold: time.Second},
			want: HealthConfig{SlowThreshold: time.Second, MinSamples: 5, Alpha: 0.3, Cooldown: 30 * time.Second},
		},
		{
			name: "valid values are kept",
			in:   HealthConfig{SlowThreshold: time.Second, MinSamples: 2, Alpha: 0.5, Cooldown: time.Minute},
			want: HealthConfig{SlowThreshold: time.Second, MinSamples: 2, Alpha: 0.5, Cooldown: time.Minute},
		},
		{
			name: "out-of-range alpha falls back",
			in:   HealthConfig{SlowThreshold: time.Second, MinSamples: 2, Alpha: 1.5, Cooldown: time.Minute},
			want: HealthConfig{SlowThreshold: time.Second, MinSamples: 2, Alpha: 0.3, Cooldown: time.Minute},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Normalized(); got != tt.want {
				t.Fatalf("Normalized() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
