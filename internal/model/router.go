package model

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// BackendFactory constructs an LLM (or Embedder) for a given Profile.
// The Router calls it the first time a Profile is resolved, caches the
// result, and reuses thereafter. Multiple factories register per backend
// kind ("vllm", future "ollama", etc.).
type BackendFactory func(ctx context.Context, p Profile, log *slog.Logger) (any, error)

// Router resolves Role → Profile → backend instance. With one model per
// endpoint, routing is a direct lookup — no load balancing, no health
// gating in v1. Health and failover are TODOs for when we have data on
// real failure modes.
type Router struct {
	reg       *Registry
	factories map[string]BackendFactory
	log       *slog.Logger

	mu       sync.RWMutex
	llmsByID map[string]LLM
	embsByID map[string]Embedder

	rolePref map[Role]string // first-pick profile ID per role
}

// NewRouter builds a Router on top of a Registry. Backend factories must
// be registered before any Resolve call.
func NewRouter(reg *Registry, log *slog.Logger) *Router {
	if log == nil {
		log = slog.Default()
	}
	r := &Router{
		reg:       reg,
		factories: make(map[string]BackendFactory),
		log:       log,
		llmsByID:  make(map[string]LLM),
		embsByID:  make(map[string]Embedder),
		rolePref:  make(map[Role]string),
	}
	// Default role preferences: first profile registered for each role.
	for _, id := range reg.IDs() {
		p, _ := reg.ByID(id)
		if _, set := r.rolePref[p.Role]; !set {
			r.rolePref[p.Role] = id
		}
	}
	return r
}

// RegisterBackend attaches a factory for a backend kind. Factories are
// invoked once per Profile and the result is cached. Safe to call at runtime
// (e.g. a live model swap that introduces a new backend kind).
func (r *Router) RegisterBackend(kind string, f BackendFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[kind] = f
}

// PinRole overrides the default role preference. Useful when the user
// has multiple profiles for the same role and wants a specific one
// (e.g. "use the Mac Studio Gemma as the planner, not the DGX Qwen").
func (r *Router) PinRole(role Role, profileID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.reg.ByID(profileID); !ok {
		return fmt.Errorf("PinRole: %w: %s", ErrInvalidProfile, profileID)
	}
	r.rolePref[role] = profileID
	return nil
}

// SetProfiles upserts the given profiles and points their roles at them,
// dropping any cached backend instance for those IDs so the next call
// reconstructs against the new endpoint/model. This is the live model-swap
// primitive: because every consumer shares this one Router, mutating it in
// place re-routes the main agent, sub-agents, AND background jobs at once.
func (r *Router) SetProfiles(profiles []Profile) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range profiles {
		if err := r.reg.Upsert(p); err != nil {
			return err
		}
		delete(r.llmsByID, p.ID) // invalidate stale cached instance
		delete(r.embsByID, p.ID)
		r.rolePref[p.Role] = p.ID
	}
	return nil
}

// SetPlanLoopCeiling updates PlanLoopCeiling in place on the active
// generation-role profiles (planner/executor/summarizer). The cached backend
// instances are untouched (the ceiling doesn't affect them), so this is a
// cheap live-tune: the next turn reads the new value. Returns how many
// profiles were updated.
func (r *Router) SetPlanLoopCeiling(n int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, role := range []Role{RolePlanner, RoleExecutor, RoleSummarizer} {
		id, ok := r.rolePref[role]
		if !ok {
			continue
		}
		p, ok := r.reg.ByID(id)
		if !ok {
			continue
		}
		p.PlanLoopCeiling = n
		if r.reg.Upsert(p) == nil {
			count++
		}
	}
	return count
}

// ForRole returns the Profile bound to role. Returns ErrRoleNotRegistered
// if nothing is bound.
func (r *Router) ForRole(role Role) (Profile, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.rolePref[role]
	if !ok {
		return Profile{}, fmt.Errorf("%w: %s", ErrRoleNotRegistered, role)
	}
	p, _ := r.reg.ByID(id)
	return p, nil
}

// LLMForRole returns a backend instance ready to call Generate /
// GenerateStream / TokenCount on behalf of the named role.
func (r *Router) LLMForRole(ctx context.Context, role Role) (LLM, Profile, error) {
	p, err := r.ForRole(role)
	if err != nil {
		return nil, Profile{}, err
	}
	llm, err := r.llmFor(ctx, p)
	if err != nil {
		return nil, Profile{}, err
	}
	return llm, p, nil
}

// EmbedderForRole returns the Embedder instance bound to role. Typically
// called with RoleEmbedder; can serve any role whose profile points at
// an embedding model.
func (r *Router) EmbedderForRole(ctx context.Context, role Role) (Embedder, Profile, error) {
	p, err := r.ForRole(role)
	if err != nil {
		return nil, Profile{}, err
	}
	emb, err := r.embedderFor(ctx, p)
	if err != nil {
		return nil, Profile{}, err
	}
	return emb, p, nil
}

func (r *Router) llmFor(ctx context.Context, p Profile) (LLM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if llm, ok := r.llmsByID[p.ID]; ok {
		return llm, nil
	}
	f, ok := r.factories[p.Backend]
	if !ok {
		return nil, fmt.Errorf("no factory registered for backend %q", p.Backend)
	}
	inst, err := f(ctx, p, r.log)
	if err != nil {
		return nil, fmt.Errorf("constructing %s: %w", p.ID, err)
	}
	llm, ok := inst.(LLM)
	if !ok {
		return nil, fmt.Errorf("backend %q for profile %s does not implement LLM", p.Backend, p.ID)
	}
	r.llmsByID[p.ID] = llm
	r.log.Debug("router: constructed LLM", "profile", p.ID, "backend", p.Backend, "endpoint", p.Endpoint)
	return llm, nil
}

func (r *Router) embedderFor(ctx context.Context, p Profile) (Embedder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.embsByID[p.ID]; ok {
		return e, nil
	}
	f, ok := r.factories[p.Backend]
	if !ok {
		return nil, fmt.Errorf("no factory registered for backend %q", p.Backend)
	}
	inst, err := f(ctx, p, r.log)
	if err != nil {
		return nil, fmt.Errorf("constructing %s: %w", p.ID, err)
	}
	emb, ok := inst.(Embedder)
	if !ok {
		return nil, fmt.Errorf("backend %q for profile %s does not implement Embedder", p.Backend, p.ID)
	}
	r.embsByID[p.ID] = emb
	r.log.Debug("router: constructed Embedder", "profile", p.ID, "backend", p.Backend, "endpoint", p.Endpoint)
	return emb, nil
}
