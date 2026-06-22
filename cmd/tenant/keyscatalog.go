package main

// keyscatalog.go is the fixed, vetted catalog of services whose API keys the
// write-only settings page (TEN-145) manages. It is the ONLY mutable id-space:
// the dashboard can set/remove a key only for a CredID that appears here, so a
// hostile POST can't write an arbitrary credentials.json entry.
//
// IMPORTANT — every entry's CredID is the id the RUNTIME actually reads, verified
// against source (so a stored key is never a no-op):
//   - LLM providers: read via resolveSecret(cfgDir, <kind>, auth) → creds.get(<kind>);
//     stored via modelControl.AddCloudModel (sets Auth{Stored:true}). CredID == kind id.
//   - Web search:    read via credKey(cfgDir, <id>, $ENVs) → creds.get(<id>). (env wins.)
//   - Integrations:  read via resolveSecret/creds.get(skill:<id>:<field>).
//
// Deliberately EXCLUDED (their secrets are env/flag-only today — NOT read from
// credentials.json — so a GUI field would silently do nothing): X (X_BEARER_TOKEN)
// and iMessage/BlueBubbles (BLUEBUBBLES_PASSWORD). Add them here once they resolve
// through credentials.json.

// keyWriteKind selects how a key is persisted (and removed).
type keyWriteKind int

const (
	// keyProvider: an LLM provider — set via modelControl.AddCloudModel (which
	// registers the provider + stores the key + sets Auth.Stored), removed via
	// forgetProviderSecret + clearProviderStored.
	keyProvider keyWriteKind = iota
	// keyDirect: a web-search backend or integration skill — the key is written
	// straight to credentials.json under CredID (the skill-config framework's
	// catalog is an empty stub today, so we don't route through SkillConfigure).
	keyDirect
)

// keySpec is one catalog entry.
type keySpec struct {
	CredID   string       // credentials.json id the runtime reads (the mutation key)
	Name     string       // display label
	Category string       // "LLM provider" | "Web search" | "Integration"
	EnvVars  []string     // env vars that ALSO supply this key (informational only)
	Required bool         // surfaced as a "Required" badge when unset (never forces a key)
	Kind     keyWriteKind // how to persist/remove
}

// keyCatalog is ordered for display: LLM providers, then web search, then
// integrations. Source of truth verified 2026-06-08 against launchconfig.go
// (providerKinds), lazyweb.go (credKey), and the skill secret reads.
var keyCatalog = []keySpec{
	// LLM providers (CredID == provider kind id; all NeedsKey + Wired).
	{CredID: "openai", Name: "OpenAI", Category: "LLM provider", EnvVars: []string{"OPENAI_API_KEY"}, Kind: keyProvider},
	{CredID: "anthropic", Name: "Anthropic (Claude)", Category: "LLM provider", EnvVars: []string{"ANTHROPIC_API_KEY"}, Kind: keyProvider},
	{CredID: "grok", Name: "Grok (xAI)", Category: "LLM provider", EnvVars: []string{"XAI_API_KEY"}, Kind: keyProvider},
	{CredID: "zai", Name: "Z.ai (GLM)", Category: "LLM provider", EnvVars: []string{"ZAI_API_KEY"}, Kind: keyProvider},
	{CredID: "zai-coding-cn", Name: "Z.ai (GLM — China)", Category: "LLM provider", EnvVars: []string{"ZAI_API_KEY"}, Kind: keyProvider},
	{CredID: "zai-metered", Name: "Z.ai (GLM — metered)", Category: "LLM provider", EnvVars: []string{"ZAI_API_KEY"}, Kind: keyProvider},
	{CredID: "sakana", Name: "Sakana AI (Fugu)", Category: "LLM provider", EnvVars: []string{"SAKANA_API_KEY"}, Kind: keyProvider},

	// Web search backends (optional; key just upgrades the backend). env wins at
	// runtime, so EnvVars here genuinely reflect resolution order.
	{CredID: "tavily", Name: "Tavily Search", Category: "Web search", EnvVars: []string{"TAVILY_API_KEY", "TAVILY_KEY"}, Kind: keyDirect},
	{CredID: "brave_search", Name: "Brave Search", Category: "Web search", EnvVars: []string{"BRAVE_SEARCH_API_KEY", "BRAVE_API_KEY"}, Kind: keyDirect},
	{CredID: "jina", Name: "Jina Reader", Category: "Web search", EnvVars: []string{"JINA_API_KEY", "JINA_KEY"}, Kind: keyDirect},

	// Integrations (read from credentials.json by the agent runtime).
	// Discord is a paste-a-token service. Google Workspace is deliberately NOT
	// here: its credential is a serialized OAuth-token JSON blob obtained via an
	// OAuth flow (skillctl_gsuite.go), not a paste-able API key — a paste field
	// would be a misleading no-op. Manage it via the OAuth flow, not this page.
	{CredID: "skill:discord:token", Name: "Discord bot token", Category: "Integration", Required: true, Kind: keyDirect},
}

// lookupKeySpec is the single mutation chokepoint: only a catalog CredID is
// mutable. Returns false for anything else (rejects traversal / arbitrary ids).
func lookupKeySpec(credID string) (keySpec, bool) {
	for _, s := range keyCatalog {
		if s.CredID == credID {
			return s, true
		}
	}
	return keySpec{}, false
}
