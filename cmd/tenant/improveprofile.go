package main

import (
	"log/slog"
	"strings"

	"tenant/internal/model"
)

// improveProposerRouter resolves the LLM router that the self-improvement
// PROPOSER/reflection jobs (soul-nudge proposer, fact-consolidation summarizer,
// distillation summarizer) should use for their generation calls (TEN-195).
//
// When improve.profile names a buildable entry in the agent-profile map, that
// pinned router is returned together with its summarizer model id (for
// provenance/logging). In every other case — empty profile, profile not found,
// profile with no provider, or a build failure — the MAIN router is returned so
// the jobs keep working exactly as before; misconfiguration logs a loud WARN
// rather than failing the loop.
//
// IMPORTANT: callers must use this router ONLY for the proposer/summarizer LLM.
// The embedder is always resolved off the main router (embedding-space
// consistency), and the SoulNudge A/B fitness scorer always runs on the daily
// model (the hard invariant — never grade the candidate on a different model
// than the one it will run under).
func improveProposerRouter(profile string, main *model.Router, agents map[string]*agentProfile,
	lc *launchConfig, cfgDir string, sharedEmbed model.Profile, log *slog.Logger) (router *model.Router, modelID string) {

	if log == nil {
		log = slog.Default()
	}
	if strings.TrimSpace(profile) == "" {
		return main, "" // not configured ⇒ today's behavior
	}
	if lc == nil || agents == nil {
		log.Warn("improve: profile configured but no launch config available; reflection jobs stay on the main router",
			"profile", profile)
		return main, ""
	}

	ap := agents[profile]
	if ap == nil {
		// Case-insensitive fallback, mirroring TeamRuntime.routerForProfile.
		for name, p := range agents {
			if strings.EqualFold(name, profile) {
				ap = p
				break
			}
		}
	}
	if ap == nil {
		log.Warn("improve: configured profile not found; reflection jobs stay on the main router",
			"profile", profile)
		return main, ""
	}
	// A profile with no pinned provider IS the main router (its value is a soul,
	// not a model swap) — return early so we never hit buildProfileRouter's
	// unknown-provider path. Same rule as routerForProfile.
	if ap.Provider == "" {
		return main, ""
	}

	pinned, err := buildProfileRouter(profile, ap, lc, cfgDir, sharedEmbed, log)
	if err != nil || pinned == nil {
		log.Warn("improve: failed to build pinned proposer router; reflection jobs stay on the main router",
			"profile", profile, "err", err)
		return main, ""
	}

	// Resolve the model id for provenance/logging (pure lookup, no probe).
	modelID = ap.Model
	if p, perr := pinned.ForRole(model.RoleSummarizer); perr == nil && p.Model != "" {
		modelID = p.Model
	}
	return pinned, modelID
}
