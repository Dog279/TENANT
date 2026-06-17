package model_test

import (
	"context"
	"log/slog"
	"testing"

	"tenant/internal/model"
	"tenant/internal/model/testllm"
)

// TestRouter_FallbackChain verifies the LLMForRole wrap (TEN-246): a chain makes
// LLMForRole return a cached *FallbackLLM; AddFallbackProfiles doesn't re-role
// the primary; SetProfiles invalidates the cached wrapper.
func TestRouter_FallbackChain(t *testing.T) {
	reg := model.NewEmptyRegistry()
	if err := reg.Add(model.Profile{ID: "primary", Role: model.RolePlanner, Backend: "fake", Endpoint: "http://p", ContextLength: 1000}); err != nil {
		t.Fatal(err)
	}
	r := model.NewRouter(reg, slog.Default())
	r.RegisterBackend("fake", func(_ context.Context, _ model.Profile, _ *slog.Logger) (any, error) {
		return testllm.New(), nil
	})
	ctx := context.Background()

	// No chain → bare LLM (not a FallbackLLM).
	llm, _, err := r.LLMForRole(ctx, model.RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := llm.(*model.FallbackLLM); ok {
		t.Error("no chain configured → should return the bare LLM, not a FallbackLLM")
	}

	// Register a fallback profile — must NOT change the primary binding.
	if err := r.AddFallbackProfiles(model.Profile{ID: "qwen/planner", Role: model.RolePlanner, Backend: "fake", Endpoint: "http://q", ContextLength: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, p, _ := r.LLMForRole(ctx, model.RolePlanner); p.ID != "primary" {
		t.Errorf("AddFallbackProfiles must not re-role the primary; got %q", p.ID)
	}

	// Set the chain → LLMForRole returns a *FallbackLLM, profile still the primary's.
	r.SetFallbackChain(model.RolePlanner, []string{"qwen/planner"})
	llm2, p2, err := r.LLMForRole(ctx, model.RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := llm2.(*model.FallbackLLM); !ok {
		t.Errorf("with a chain, LLMForRole should return a *FallbackLLM; got %T", llm2)
	}
	if p2.ID != "primary" {
		t.Errorf("returned profile must be the primary's (budget/tool-format source); got %q", p2.ID)
	}

	// Cached across calls so per-link cooldown state persists.
	if llm3, _, _ := r.LLMForRole(ctx, model.RolePlanner); llm3 != llm2 {
		t.Error("the FallbackLLM wrapper should be cached across calls (cooldown persistence)")
	}

	// SetProfiles invalidates the wrapper cache → rebuilt instance.
	if err := r.SetProfiles([]model.Profile{{ID: "primary", Role: model.RolePlanner, Backend: "fake", Endpoint: "http://p2", ContextLength: 1000}}); err != nil {
		t.Fatal(err)
	}
	llm4, _, _ := r.LLMForRole(ctx, model.RolePlanner)
	if llm4 == llm2 {
		t.Error("SetProfiles must invalidate the cached fallback wrapper")
	}
	if _, ok := llm4.(*model.FallbackLLM); !ok {
		t.Error("chain persists across SetProfiles → still a *FallbackLLM")
	}
}
