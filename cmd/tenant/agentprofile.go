package main

// Agent profiles — named sub-agent recipes the orchestrator can spawn with
// their own pinned model + identity. Defined in launchConfig.Agents; managed
// via `tenant agents` (CLI) and `/agents` (TUI); executed via TeamRuntime.Spawn
// which looks up the role against the registry before falling back to the
// orchestrator's defaults.
//
// Per-profile routers are built LAZILY on first spawn and cached on the
// runtime so we don't rebuild for every researcher in a wave. The cache is
// invalidated by a live `/agents` mutation (the agentctl re-publishes the
// registry to the runtime).
//
// Identity (the profile's "soul" markdown) is grafted onto the spawned
// agent's SystemPrompt — the canonical soul.Soul object stays operator-
// owned (values, safety rules), while the profile adds the persona text
// for THIS sub-agent. This separation means an operator can't accidentally
// have a profile override the team-level safety boundaries.

import (
	"fmt"
	"log/slog"
	"strings"

	"tenant/internal/model"
)

// buildProfileRouter builds a fresh router pinned to one agent profile's
// provider+model. Reuses the orchestrator's embedder profile (the local
// Ollama stays the same — sub-agents shouldn't burn cloud embedding
// credits) by adopting it into the new registry.
//
// Returns the router with backend factories already registered. Caller is
// responsible for caching to avoid rebuilding per spawn.
func buildProfileRouter(profileName string, ap *agentProfile, lc *launchConfig, cfgDir string,
	sharedEmbedProfile model.Profile, log *slog.Logger) (*model.Router, error) {

	if ap == nil {
		return nil, fmt.Errorf("agent profile %q is nil", profileName)
	}
	pc := lc.Providers[ap.Provider]
	if pc == nil {
		return nil, fmt.Errorf("agent %q references unknown provider %q (configure it via /model add-cloud or tenant setup)",
			profileName, ap.Provider)
	}
	pk, ok := providerKinds[pc.Kind]
	if !ok {
		return nil, fmt.Errorf("agent %q: provider %q has unknown kind %q", profileName, ap.Provider, pc.Kind)
	}

	// Pre-flight: a keyed provider with no resolvable secret is a config
	// problem the operator must fix BEFORE the first spawn — surface it
	// here with a useful error, not as a 401 mid-research.
	apiKey := resolveSecret(cfgDir, ap.Provider, pc.Auth)
	if pk.NeedsKey && apiKey == "" {
		return nil, fmt.Errorf("agent %q: provider %q needs an API key but none is configured (set %s env var or rerun /model add-cloud %s <key>)",
			profileName, ap.Provider, pk.KeyEnv, ap.Provider)
	}

	// Pick the model: profile override > provider default > catalog default.
	mdl := strings.TrimSpace(ap.Model)
	if mdl == "" {
		mdl = pc.Model
	}
	if mdl == "" {
		mdl = pk.DefaultModel
	}
	if mdl == "" {
		return nil, fmt.Errorf("agent %q: provider %q has no Model configured and the profile didn't override one", profileName, ap.Provider)
	}

	// Build a commonFlags view that genProfiles can consume — same shape as
	// the live `/model use` swap path.
	c := &commonFlags{
		cfgDir:       cfgDir,
		backend:      pk.Backend,
		genKind:      pc.Kind,
		vllmEndpoint: pc.Endpoint,
		vllmModel:    mdl,
		vllmToolFmt:  firstNonEmpty(pc.ToolFmt, pk.DefaultToolFmt),
		genAPIKey:    apiKey,
		planCeiling:  lc.PlanLoopCeiling,
	}
	gen, factories, err := genProfiles(c)
	if err != nil {
		return nil, fmt.Errorf("agent %q: build profiles: %w", profileName, err)
	}

	// Register the gen profiles for this provider + the shared embedder so
	// the agent's assemble step can still embed retrieval queries. The
	// embedder lives on the orchestrator's local server (Ollama) and is
	// purposely NOT replaced — every sub-agent shares the same embedding
	// space, so semantic memory works across them.
	reg := model.NewEmptyRegistry()
	for _, p := range gen {
		if err := reg.Add(p); err != nil {
			return nil, fmt.Errorf("agent %q: register %s: %w", profileName, p.ID, err)
		}
	}
	if sharedEmbedProfile.ID != "" {
		if err := reg.Add(sharedEmbedProfile); err != nil {
			return nil, fmt.Errorf("agent %q: register embed profile: %w", profileName, err)
		}
	}
	r := model.NewRouter(reg, log)
	for kind, f := range factories {
		r.RegisterBackend(kind, f)
	}
	// Pin the gen roles to this profile's IDs (the registry's first-pick
	// default already does this, but be explicit so a future multi-profile
	// registry never picks wrong).
	for _, p := range gen {
		if err := r.PinRole(p.Role, p.ID); err != nil {
			return nil, fmt.Errorf("agent %q: pin %s: %w", profileName, p.Role, err)
		}
	}
	return r, nil
}

// profileSystemPrompt composes the spawned agent's SystemPrompt: the base
// team-member prompt (sets up the orchestrator relationship + safety rules)
// followed by the profile's identity markdown. When the profile has no
// identity, this is identical to memberPrompt — backward compatible.
func profileSystemPrompt(id, role, orchID, profileSoul string) string {
	base := memberPrompt(id, role, orchID)
	if strings.TrimSpace(profileSoul) == "" {
		return base
	}
	return base + "\n\n--- Your identity ---\n" + strings.TrimSpace(profileSoul)
}

// renderAgentsForOrchestrator builds the system-prompt snippet that tells the
// orchestrator which named agents it can spawn (plus their descriptions and
// pinned models). Empty string when no profiles are configured — keeps the
// orchestrator's existing prompt unchanged for operators who never define
// any.
func renderAgentsForOrchestrator(agents map[string]*agentProfile) string {
	if len(agents) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n--- Available specialized sub-agents ---\n")
	b.WriteString("You have these named team members available via spawn_agent(role=<name>, task=<...>).\n")
	b.WriteString("Each carries a specialized identity:\n")
	// Sorted for a stable system prompt across launches (the map order isn't).
	for _, name := range sortedAgentNames(agents) {
		ap := agents[name]
		if ap.Provider == "" {
			fmt.Fprintf(&b, "  • %s — your model (inherited)", name)
		} else {
			fmt.Fprintf(&b, "  • %s — model: %s", name, ap.Provider)
			if ap.Model != "" {
				fmt.Fprintf(&b, "/%s", ap.Model)
			}
		}
		if d := strings.TrimSpace(ap.Description); d != "" {
			fmt.Fprintf(&b, " — %s", d)
		}
		b.WriteString("\n")
	}
	b.WriteString("Spawn one of THESE by name to get specialized work. Spawning any other role\n")
	b.WriteString("name falls back to a generic team member on your own model.\n")
	// TEN-139: steer coding/implementation work to the specialist BY DEFAULT
	// (data-driven off the rendered descriptions, so a user's custom coding
	// agent works too — no hardcoded name), framed as the case where spawning
	// "genuinely helps" so it doesn't fight the base prompt's answer-directly
	// gate. TEN-140: the concurrent delegate-and-keep-working pattern, carrying
	// the MUST-await-before-a-dependent-final-answer boundary so it can't be
	// read as license to under-await. Both only emit when agents exist, so the
	// len(agents)==0 short-circuit above keeps the no-team prompt unchanged.
	b.WriteString("\nCoding, implementation, and debugging are where spawning genuinely helps: when a " +
		"team member's description marks them the coding/implementation specialist, delegate that work " +
		"to them with spawn_agent BY DEFAULT rather than doing it yourself. After spawning you don't have " +
		"to block — keep doing your own independent work (or spawn more independent workers), and call " +
		"team_await only when you need their results, and always before any final answer that depends on " +
		"a worker.")
	return b.String()
}
