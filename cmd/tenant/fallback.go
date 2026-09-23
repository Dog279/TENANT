package main

import (
	"fmt"
	"log/slog"
	"strings"

	"tenant/internal/model"
)

// fallback.go (TEN-246): wires launchConfig.Fallbacks into the router's
// per-role fallback chain. The FallbackLLM engine lives in internal/model;
// this just builds + registers the namespaced per-provider profiles.

// Prompt-router roles are deliberately open strings. They are pinned once at
// wiring/control time; turns only resolve them through LLMForRole and never
// mutate the shared Router.
const (
	plannerBigRole model.Role = "planner-big"
	routeForceRole model.Role = "route-force"
)

// fallbackGenRoles are the generation roles that get a fallback chain (the ones
// genProfiles produces). Judge/embedder are intentionally excluded.
var fallbackGenRoles = []model.Role{model.RolePlanner, model.RoleExecutor, model.RoleSummarizer}

// providerGenProfiles builds the generation profiles for a NAMED (possibly
// non-active) provider, namespacing the IDs with the provider name so they
// don't collide with the active provider's same-kind IDs. Mirrors the
// agentprofile + /model-use build path.
func providerGenProfiles(cfgDir string, lc *launchConfig, name string, planCeiling int) ([]model.Profile, map[string]model.BackendFactory, error) {
	pc := lc.Providers[name]
	if pc == nil {
		return nil, nil, fmt.Errorf("no provider %q", name)
	}
	pk := providerKinds[pc.Kind]
	mdl := strings.TrimSpace(pc.Model)
	if mdl == "" {
		mdl = pk.DefaultModel
	}
	c := &commonFlags{
		cfgDir:       cfgDir,
		backend:      pk.Backend,
		genKind:      pc.Kind,
		vllmEndpoint: firstNonEmpty(pc.Endpoint, pk.DefaultEndpoint),
		vllmModel:    mdl,
		vllmToolFmt:  firstNonEmpty(pc.ToolFmt, pk.DefaultToolFmt),
		genAPIKey:    resolveSecret(cfgDir, name, pc.Auth),
		planCeiling:  planCeiling,
	}
	// Carry the provider's persisted reasoning effort exactly like the active
	// provider path (wiring.go) does — without this, planner-big / forced /
	// fallback bindings silently run at default effort.
	if pk.SupportsReasoning {
		c.reasoning = pc.Reasoning
	}
	gen, factories, err := genProfiles(c)
	if err != nil {
		return nil, nil, err
	}
	for i := range gen {
		gen[i].ID = name + "/" + gen[i].ID
	}
	return gen, factories, nil
}

// installRouteBigRole binds the configured route.big_role provider to the
// stable planner-big role. The provider's planner profile stays declared as a
// planner and keeps its provider-namespaced ID; AddFallbackProfiles registers
// it without changing the live planner preference, and PinRole adds only the
// new open-string role. Consequently `/model use` can keep replacing planner
// while this binding survives like the independently pinned embedder.
func installRouteBigRole(r *model.Router, cfgDir string, lc *launchConfig, planCeiling int, log *slog.Logger) error {
	if r == nil {
		return fmt.Errorf("route: bind planner-big: model router is unavailable")
	}
	rc, err := resolveRouteConfig(lc)
	if err != nil {
		return err
	}
	providerName := strings.TrimSpace(rc.BigRole)
	if providerName == "" {
		return nil
	}
	gen, factories, err := providerGenProfiles(cfgDir, lc, providerName, planCeiling)
	if err != nil {
		err = fmt.Errorf("route: build planner-big provider %q: %w", providerName, err)
		// Routing disabled means routing must not be a dependency: a stale,
		// unbuildable big_role left behind by `/route off` degrades to a
		// warning instead of breaking startup or /model swaps. With routing
		// enabled the bind stays load-bearing and the error propagates.
		if !rc.Enabled {
			if log != nil {
				log.Warn("route: planner-big unavailable while routing is off; continuing", "err", err.Error())
			}
			return nil
		}
		return err
	}
	var planner model.Profile
	for _, p := range gen {
		if p.Role == model.RolePlanner {
			planner = p
			break
		}
	}
	if planner.ID == "" {
		return fmt.Errorf("route: provider %q did not produce a planner profile", providerName)
	}
	for kind, factory := range factories {
		r.RegisterBackend(kind, factory)
	}
	if err := r.AddFallbackProfiles(planner); err != nil {
		return fmt.Errorf("route: register planner-big profile %q: %w", planner.ID, err)
	}
	if err := r.PinRole(plannerBigRole, planner.ID); err != nil {
		return fmt.Errorf("route: pin planner-big profile %q: %w", planner.ID, err)
	}
	if log != nil {
		log.Info("route: bound planner-big role", "provider", providerName, "profile", planner.ID)
	}
	return nil
}

// installFallbackChain (re)asserts lc.Fallbacks onto the router. For each
// fallback provider it builds + registers namespaced gen profiles and appends
// them to each gen role's chain. Unknown/unbuildable providers are logged and
// SKIPPED — a broken fallback must never block the primary. The active provider
// is excluded. Idempotent (Upsert + re-Set), so it's safe to re-run after a
// /model swap.
func installFallbackChain(r *model.Router, cfgDir string, lc *launchConfig, planCeiling int, log *slog.Logger) {
	// Reset chains first so a re-assert (post-swap) reflects the current config.
	for _, role := range fallbackGenRoles {
		r.SetFallbackChain(role, nil)
	}
	r.SetFallbackChain(plannerBigRole, nil)
	if len(lc.Fallbacks) == 0 {
		return
	}
	seen := map[string]bool{strings.TrimSpace(lc.Provider): true} // exclude the active primary
	roleChains := map[model.Role][]string{}
	for _, raw := range lc.Fallbacks {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if lc.Providers[name] == nil {
			log.Warn("fallback: unknown provider; skipping", "name", name)
			continue
		}
		gen, factories, err := providerGenProfiles(cfgDir, lc, name, planCeiling)
		if err != nil {
			log.Warn("fallback: provider unbuildable; skipping", "name", name, "err", err.Error())
			continue
		}
		for kind, f := range factories {
			r.RegisterBackend(kind, f)
		}
		if err := r.AddFallbackProfiles(gen...); err != nil {
			log.Warn("fallback: register profiles failed; skipping", "name", name, "err", err.Error())
			continue
		}
		for _, p := range gen {
			roleChains[p.Role] = append(roleChains[p.Role], p.ID)
		}
		log.Info("fallback: registered provider", "name", name, "profiles", len(gen))
	}
	for role, ids := range roleChains {
		r.SetFallbackChain(role, ids)
	}
	// The big leg inherits the planner's provider chain, minus its own primary
	// when that provider is also listed as a fallback. Fallback profile Role
	// values need not equal the lookup role; LLMForRole follows profile IDs.
	if primary, err := r.ForRole(plannerBigRole); err == nil {
		ids := roleChains[model.RolePlanner]
		bigChain := make([]string, 0, len(ids))
		for _, id := range ids {
			if id != primary.ID {
				bigChain = append(bigChain, id)
			}
		}
		r.SetFallbackChain(plannerBigRole, bigChain)
	}
}

// fallbackLabelToProvider turns a fallback profile-ID ("qwen-gpu/vllm-planner")
// or a primary's role-ID ("vllm-planner") into a human label for the feed. The
// fallback IDs are provider-namespaced; the primary isn't, so it falls back to
// the supplied active-provider name.
func fallbackLabelToProvider(label, activeName string) string {
	if i := strings.IndexByte(label, '/'); i >= 0 {
		return label[:i]
	}
	return activeName
}
