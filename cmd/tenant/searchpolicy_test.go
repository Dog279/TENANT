package main

import (
	"strings"
	"testing"
)

// TestSearchPolicyPrompt_Content: the policy names the knowledge tier, the web
// tool, and the trust-but-verify/peer language.
func TestSearchPolicyPrompt_Content(t *testing.T) {
	p := searchPolicyPrompt()
	for _, want := range []string{"memory_search", "wiki_search", "web_search", "trust but verify", "peers"} {
		if !strings.Contains(p, want) {
			t.Errorf("searchPolicyPrompt missing %q", want)
		}
	}
}
