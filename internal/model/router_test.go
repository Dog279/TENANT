package model_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"tenant/internal/model"
	"tenant/internal/model/testllm"
)

func TestRouter_ForRoleFound(t *testing.T) {
	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r := model.NewRouter(reg, slog.Default())
	p, err := r.ForRole(model.RolePlanner)
	if err != nil {
		t.Fatalf("ForRole(planner): %v", err)
	}
	if p.Role != model.RolePlanner {
		t.Fatalf("ForRole returned role %q, want planner", p.Role)
	}
}

func TestRouter_ForRoleNotRegistered(t *testing.T) {
	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r := model.NewRouter(reg, slog.Default())
	_, err = r.ForRole(model.Role("nonexistent-role"))
	if !errors.Is(err, model.ErrRoleNotRegistered) {
		t.Fatalf("err = %v, want ErrRoleNotRegistered", err)
	}
}

func TestRouter_PinRole(t *testing.T) {
	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r := model.NewRouter(reg, slog.Default())
	if err := r.PinRole(model.RolePlanner, "gemma4-70b"); err != nil {
		t.Fatalf("PinRole: %v", err)
	}
	p, err := r.ForRole(model.RolePlanner)
	if err != nil {
		t.Fatalf("ForRole(planner): %v", err)
	}
	if p.ID != "gemma4-70b" {
		t.Fatalf("after PinRole, ForRole returned %q, want gemma4-70b", p.ID)
	}
}

func TestRouter_PinRoleUnknownProfile(t *testing.T) {
	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r := model.NewRouter(reg, slog.Default())
	err = r.PinRole(model.RolePlanner, "no-such-profile")
	if !errors.Is(err, model.ErrInvalidProfile) {
		t.Fatalf("err = %v, want ErrInvalidProfile", err)
	}
}

// SetProfiles is the live model-swap primitive: it re-points a role at a new
// profile and invalidates the cached backend so the next call reconstructs
// against the new endpoint. Because consumers share one Router, this re-routes
// everyone at once.
func TestRouter_SetProfilesLiveSwap(t *testing.T) {
	reg := model.NewEmptyRegistry()
	if err := reg.Add(model.Profile{ID: "gen-a", Role: model.RolePlanner, Backend: "fake", Endpoint: "http://a", ContextLength: 1000}); err != nil {
		t.Fatal(err)
	}
	r := model.NewRouter(reg, slog.Default())
	var built int
	r.RegisterBackend("fake", func(_ context.Context, _ model.Profile, _ *slog.Logger) (any, error) {
		built++
		return testllm.New(), nil
	})

	if _, p, _ := r.LLMForRole(context.Background(), model.RolePlanner); p.ID != "gen-a" {
		t.Fatalf("initial planner = %q, want gen-a", p.ID)
	}
	if built != 1 {
		t.Fatalf("built=%d, want 1", built)
	}

	// Swap to a new profile for the planner role (different endpoint).
	if err := r.SetProfiles([]model.Profile{{ID: "gen-b", Role: model.RolePlanner, Backend: "fake", Endpoint: "http://b", ContextLength: 2000}}); err != nil {
		t.Fatalf("SetProfiles: %v", err)
	}
	_, p, err := r.LLMForRole(context.Background(), model.RolePlanner)
	if err != nil {
		t.Fatalf("LLMForRole after swap: %v", err)
	}
	if p.ID != "gen-b" || p.Endpoint != "http://b" {
		t.Fatalf("after swap planner = %+v, want gen-b@http://b", p)
	}
	if built != 2 {
		t.Fatalf("built=%d, want 2 (cache invalidated → reconstructed)", built)
	}

	// Upsert the SAME id with a new endpoint (the vllm-planner re-point case).
	if err := r.SetProfiles([]model.Profile{{ID: "gen-b", Role: model.RolePlanner, Backend: "fake", Endpoint: "http://b2", ContextLength: 2000}}); err != nil {
		t.Fatal(err)
	}
	if _, p, _ := r.LLMForRole(context.Background(), model.RolePlanner); p.Endpoint != "http://b2" {
		t.Fatalf("same-id upsert did not replace endpoint: %q", p.Endpoint)
	}
	if built != 3 {
		t.Fatalf("built=%d, want 3", built)
	}
}

// SetPlanLoopCeiling live-tunes the iteration cap on the active gen profiles
// without disturbing the cached backend instance.
func TestRouter_SetPlanLoopCeiling(t *testing.T) {
	reg := model.NewEmptyRegistry()
	if err := reg.Add(model.Profile{ID: "p", Role: model.RolePlanner, Backend: "fake", Endpoint: "http://a", ContextLength: 1000, PlanLoopCeiling: 8}); err != nil {
		t.Fatal(err)
	}
	r := model.NewRouter(reg, slog.Default())
	r.RegisterBackend("fake", func(_ context.Context, _ model.Profile, _ *slog.Logger) (any, error) { return testllm.New(), nil })

	if p, _ := r.ForRole(model.RolePlanner); p.PlanLoopCeiling != 8 {
		t.Fatalf("initial ceiling = %d, want 8", p.PlanLoopCeiling)
	}
	if n := r.SetPlanLoopCeiling(30); n != 1 {
		t.Fatalf("updated %d profiles, want 1", n)
	}
	if p, _ := r.ForRole(model.RolePlanner); p.PlanLoopCeiling != 30 {
		t.Fatalf("ceiling after tune = %d, want 30", p.PlanLoopCeiling)
	}
}

func TestRouter_LLMForRoleUsesFactory(t *testing.T) {
	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r := model.NewRouter(reg, slog.Default())
	fake := testllm.New()
	// Replace vllm factory with one that returns our fake.
	r.RegisterBackend("vllm", func(_ context.Context, p model.Profile, _ *slog.Logger) (any, error) {
		return fake, nil
	})

	llm, p, err := r.LLMForRole(context.Background(), model.RolePlanner)
	if err != nil {
		t.Fatalf("LLMForRole: %v", err)
	}
	if p.Role != model.RolePlanner {
		t.Fatalf("returned profile role = %q, want planner", p.Role)
	}
	if llm == nil {
		t.Fatal("LLMForRole returned nil llm")
	}

	// Second call must return the cached instance, not invoke factory again.
	llm2, _, err := r.LLMForRole(context.Background(), model.RolePlanner)
	if err != nil {
		t.Fatalf("second LLMForRole: %v", err)
	}
	if llm != llm2 {
		t.Fatal("router did not cache LLM instance across calls")
	}
}

func TestRouter_EmbedderForRole(t *testing.T) {
	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r := model.NewRouter(reg, slog.Default())
	fake := testllm.New()
	r.RegisterBackend("vllm", func(_ context.Context, p model.Profile, _ *slog.Logger) (any, error) {
		return fake, nil
	})

	emb, p, err := r.EmbedderForRole(context.Background(), model.RoleEmbedder)
	if err != nil {
		t.Fatalf("EmbedderForRole: %v", err)
	}
	if p.Role != model.RoleEmbedder {
		t.Fatalf("returned profile role = %q, want embedder", p.Role)
	}
	vecs, err := emb.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 1 {
		t.Fatalf("Embed returned %d vectors, want 1", len(vecs))
	}
}
