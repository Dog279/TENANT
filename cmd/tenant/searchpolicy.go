package main

// searchpolicy.go (TEN-249): the knowledge-first search-order policy appended to
// the agent's system prompt. Diagnosis: with a peer connected the agent reached
// for web_search over its OWN knowledge tier (memory_search/wiki_search) — which
// is exactly what federation (TEN-243) extends to peers. The tools were ranked as
// equal siblings by query-description similarity with no policy telling the model
// when to prefer which. This paragraph supplies that policy:
//
//   - knowledge-first for INTERNAL/project questions (what you/your team know,
//     past decisions, conventions, saved notes),
//   - web-first for OPEN-WORLD/current info (news, prices, weather, external docs),
//   - stakes-based "trust but verify": rely on peer/internal results directly for
//     internal facts; cross-check high-stakes/external claims against the web
//     before acting.
//
// The prompt decides WHEN to use each tier; the ranking boost (toolmux.go) only
// makes the knowledge tier VISIBLE/salient when peers are connected. Kept terse —
// it rides on every main-agent turn, trading against the TEN-225/226 token work.

// searchPolicyPrompt returns the policy paragraph, leading-space-prefixed so it
// concatenates cleanly onto an existing system prompt.
func searchPolicyPrompt() string {
	return " When you need information, search your OWN knowledge FIRST for anything internal or " +
		"project-specific — what you or your team know, past decisions, conventions, saved notes: use " +
		"memory_search (and wiki_search if a wiki/notes tool is available) — these ALSO cover any connected " +
		"peers. Go straight to web_search for open-world or current information (news, prices, weather, " +
		"external or library docs). Peer and external results are flagged \"trust but verify\": rely on them " +
		"directly for internal facts, but cross-check high-stakes or external factual claims against the web " +
		"before you act on them. Use only the tools you actually have."
}
