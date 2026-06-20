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
// endpoint, routing is a direct lookup — no load balancing.
//
// Failover + health (TEN-246 / TEN-282): handled in the FallbackLLM decorator
// (internal/model/fallback.go), composed here per role. It covers BOTH
// reactive failover (advance to the next link on a hard error — rate limit /
// out-of-credits / unreachable / 5xx, fail-closed otherwise) AND proactive
// latency health-gating (a link that returns 200 OK but is persistently SLOW
// gets a short cooldown via the same per-link mechanism, so the chain prefers
// the next link and re-probes after the cooldown — never a permanent demotion).
// Health-gating is OFF unless configured via FallbackLLM.SetHealthGating.
// STILL FUTURE: cross-endpoint load balancing / weighted routing, and feeding
// real production latency/error data into the thresholds.
type Router struct {
	reg       *Registry
	factories map[string]BackendFactory
	log       *slog.Logger

	mu       sync.RWMutex
	llmsByID map[string]LLM
	embsByID map[string]Embedder

	rolePref map[Role]string // first-pick profile ID per role

	// Auto-fallback (TEN-246): ordered fallback profile IDs per role (tried
	// after the role's primary), and a per-role cached *FallbackLLM wrapper so
	// its in-memory cooldown state persists across turns. failoverObserver gets
	// a FailoverEvent each time a link fails over (operator feed line). All
	// nil/empty by default ⇒ today's single-provider behavior.
	fallbackChains   map[Role][]string
	fallbackLLMs     map[Role]LLM
	failoverObserver func(FailoverEvent)
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

		fallbackChains: make(map[Role][]string),
		fallbackLLMs:   make(map[Role]LLM),
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
	// A primary changed → drop the cached fallback wrappers so they rebuild
	// against the fresh per-link LLMs (TEN-246).
	r.fallbackLLMs = make(map[Role]LLM)
	return nil
}

// SetFailoverObserver installs the callback fired when a fallback link fails
// over to the next (operator feed line). nil clears it.
func (r *Router) SetFailoverObserver(fn func(FailoverEvent)) {
	r.mu.Lock()
	r.failoverObserver = fn
	r.mu.Unlock()
}

// AddFallbackProfiles upserts profiles into the registry WITHOUT changing any
// role's primary binding (unlike SetProfiles, which re-roles). Used to register
// the namespaced per-provider fallback profiles (TEN-246).
func (r *Router) AddFallbackProfiles(profiles ...Profile) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range profiles {
		if err := r.reg.Upsert(p); err != nil {
			return err
		}
		delete(r.llmsByID, p.ID)
	}
	return nil
}

// SetFallbackChain sets the ordered fallback profile IDs tried after role's
// primary. Empty clears it. Invalidates the cached wrapper for that role.
func (r *Router) SetFallbackChain(role Role, profileIDs []string) {
	r.mu.Lock()
	if len(profileIDs) == 0 {
		delete(r.fallbackChains, role)
	} else {
		r.fallbackChains[role] = append([]string(nil), profileIDs...)
	}
	delete(r.fallbackLLMs, role)
	r.mu.Unlock()
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
	// No fallback configured for this role → today's behavior. The returned
	// Profile is always the PRIMARY's (used for tool format + context budget);
	// fallback is a transparent routing detail. (TEN-246)
	r.mu.RLock()
	chain := r.fallbackChains[role]
	cached := r.fallbackLLMs[role]
	obs := r.failoverObserver
	r.mu.RUnlock()
	if len(chain) == 0 {
		return llm, p, nil
	}
	if cached != nil {
		return cached, p, nil
	}
	// Build the wrapper once (cached so its cooldown state persists). Drop any
	// fallback link that can't be built (missing key/unknown backend) — a broken
	// fallback must never block the primary.
	links := []fbLink{{llm: llm, label: p.ID}}
	for _, id := range chain {
		fp, ok := r.reg.ByID(id)
		if !ok {
			r.log.Warn("router: fallback profile not registered; skipping", "id", id, "role", string(role))
			continue
		}
		flm, ferr := r.llmFor(ctx, fp)
		if ferr != nil {
			r.log.Warn("router: fallback link unbuildable; skipping", "id", id, "err", ferr.Error())
			continue
		}
		links = append(links, fbLink{llm: flm, label: id})
	}
	if len(links) < 2 {
		return llm, p, nil // no usable fallback → just the primary
	}
	fb := NewFallbackLLM(links, obs)
	r.mu.Lock()
	r.fallbackLLMs[role] = fb
	r.mu.Unlock()
	return fb, p, nil
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
