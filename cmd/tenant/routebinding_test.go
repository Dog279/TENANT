package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"tenant/internal/model"
)

func TestInstallRouteBigRoleSurvivesPlannerSwap(t *testing.T) {
	lc := &launchConfig{
		Providers: map[string]*providerConfig{
			"big": {Kind: "vllm", Endpoint: "http://big.local:8000", Model: "big-model"},
		},
		Route: &routeConfig{BigRole: "big"},
	}
	r := model.NewRouter(model.NewEmptyRegistry(), testRouteBindingLogger())
	if err := installRouteBigRole(r, t.TempDir(), lc, 0, testRouteBindingLogger()); err != nil {
		t.Fatal(err)
	}

	before, err := r.ForRole(plannerBigRole)
	if err != nil {
		t.Fatal(err)
	}
	if before.ID != "big/vllm-planner" || before.ProviderKind != "vllm" {
		t.Fatalf("planner-big = %+v", before)
	}
	if err := r.SetProfiles([]model.Profile{{
		ID: "vllm-planner", Role: model.RolePlanner, Backend: "vllm",
		ProviderKind: "vllm", Endpoint: "http://small.local:8000", ContextLength: 32000,
	}}); err != nil {
		t.Fatal(err)
	}
	after, err := r.ForRole(plannerBigRole)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID || after.Endpoint != before.Endpoint {
		t.Fatalf("planner swap changed planner-big: before=%+v after=%+v", before, after)
	}
}

func TestInstallFallbackChainPlannerBigFiltersItsPrimary(t *testing.T) {
	lc := &launchConfig{
		Provider: "small",
		Providers: map[string]*providerConfig{
			"small":  {Kind: "vllm", Endpoint: "http://small.local:8000", Model: "small-model"},
			"big":    {Kind: "vllm", Endpoint: "http://big.local:8000", Model: "big-model"},
			"backup": {Kind: "vllm", Endpoint: "http://backup.local:8000", Model: "backup-model"},
		},
		Fallbacks: []string{"big"},
		Route:     &routeConfig{BigRole: "big"},
	}
	r := model.NewRouter(model.NewEmptyRegistry(), testRouteBindingLogger())
	if err := installRouteBigRole(r, t.TempDir(), lc, 0, testRouteBindingLogger()); err != nil {
		t.Fatal(err)
	}
	installFallbackChain(r, t.TempDir(), lc, 0, testRouteBindingLogger())
	llm, _, err := r.LLMForRole(context.Background(), plannerBigRole)
	if err != nil {
		t.Fatal(err)
	}
	if _, isFallback := llm.(*model.FallbackLLM); isFallback {
		t.Fatal("planner-big fallback chain retained its own primary")
	}

	lc.Fallbacks = []string{"big", "backup"}
	installFallbackChain(r, t.TempDir(), lc, 0, testRouteBindingLogger())
	llm, _, err = r.LLMForRole(context.Background(), plannerBigRole)
	if err != nil {
		t.Fatal(err)
	}
	if _, isFallback := llm.(*model.FallbackLLM); !isFallback {
		t.Fatalf("planner-big did not inherit remaining planner fallback; got %T", llm)
	}
}

func TestGeneratedProfilesCarryProviderKind(t *testing.T) {
	tests := []struct {
		name string
		cfg  commonFlags
		want string
	}{
		{name: "hosted openai-compatible", cfg: commonFlags{backend: "vllm", genKind: "openai", vllmEndpoint: "https://api.openai.com", vllmModel: "gpt-test"}, want: "openai"},
		{name: "anthropic", cfg: commonFlags{backend: "anthropic", genKind: "anthropic", vllmEndpoint: "https://api.anthropic.com", vllmModel: "claude-test", genAPIKey: "test"}, want: "anthropic"},
		{name: "echo", cfg: commonFlags{backend: "echo", genKind: "echo"}, want: "echo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profiles, _, err := genProfiles(&tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(profiles) == 0 {
				t.Fatal("no generated profiles")
			}
			for _, p := range profiles {
				if p.ProviderKind != tt.want {
					t.Errorf("profile %s ProviderKind = %q, want %q", p.ID, p.ProviderKind, tt.want)
				}
			}
		})
	}
}

func testRouteBindingLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
