package main

// builtinagents.go is TEN-132: the expert sub-agents shipped in the binary. The
// orchestrator can spawn any of them with ZERO config — they inherit the user's
// primary model (empty Provider) and carry a condensed persona, so a fresh user
// gets quality specialists after only pointing tenant at a model endpoint.
//
// Personas live under builtinsouls/agents/<Role>/{IDENTITY,SOUL,RULES}.md. The
// SPAWNABLE soul is IDENTITY + RULES only: the full SOUL.md voice essay is
// dropped here because a single-shot leaf sub-agent never earns the tenure that
// justifies a ~2K-token persona, and the full triplet would overrun the system-
// prompt reserve on a small primary. The complete triplet stays embedded for the
// primary-agent path (e.g. /memory soul import the Main persona).

import (
	"embed"
	"sort"
	"strings"
)

//go:embed builtinsouls/agents
var builtinAgentsFS embed.FS

// builtinSpecialists are the embedded spawnable experts. Main is deliberately
// NOT here: its conductor identity (owns the tracker, single voice to the user,
// emits lettered options) contradicts the headless member prompt a spawned
// sub-agent runs under. Main ships as a recommended PRIMARY soul instead.
var builtinSpecialists = []struct{ name, desc string }{
	{"Programmer", "implements features and fixes: smallest diff, root-cause only, regression test, clean build before done"},
	{"Researcher", "deep multi-source research: pulls the primary source, adversarially verifies every claim, cites everything"},
	{"Writer", "docs, READMEs, commit/PR/ticket prose, summaries: direct voice, accuracy over polish, never overclaims"},
	{"QA", "adversarial verifier: tries to break the work, verifies against reality with file:line evidence, hunts edge cases"},
	{"Strategist", "founder-mode scoping + neutral judge: challenges the premise, finds the narrowest high-value wedge, decides what NOT to build"},
}

// builtinAgentProfiles composes the embedded specialists into spawnable profiles.
// Soul = IDENTITY + RULES; Provider empty (inherit the primary router); Builtin
// marks them read-only in the CRUD/UI.
func builtinAgentProfiles() map[string]*agentProfile {
	out := make(map[string]*agentProfile, len(builtinSpecialists))
	for _, s := range builtinSpecialists {
		identity, err1 := builtinAgentsFS.ReadFile("builtinsouls/agents/" + s.name + "/IDENTITY.md")
		rules, err2 := builtinAgentsFS.ReadFile("builtinsouls/agents/" + s.name + "/RULES.md")
		if err1 != nil || err2 != nil {
			// Embedded at compile time, so a read miss is a build-tree mistake.
			// Skip rather than ship a half-composed soul (a loader test catches it).
			continue
		}
		out[s.name] = &agentProfile{
			Builtin:     true,
			Description: s.desc,
			Soul:        strings.TrimSpace(string(identity)) + "\n\n" + strings.TrimSpace(string(rules)),
		}
	}
	return out
}

// effectiveAgents merges the built-in specialists UNDER the operator's configured
// profiles: a user entry of the same name wins (override by name). Built-ins are
// computed unconditionally so they survive a nil/failed config load.
func effectiveAgents(lc *launchConfig) map[string]*agentProfile {
	out := builtinAgentProfiles()
	if lc != nil {
		for name, ap := range lc.Agents {
			out[name] = ap // operator config overrides the built-in by name
		}
	}
	return out
}

// sortedAgentNames returns profile names in deterministic order so the
// orchestrator's system-prompt agent list is stable across launches.
func sortedAgentNames(agents map[string]*agentProfile) []string {
	names := make([]string, 0, len(agents))
	for n := range agents {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
