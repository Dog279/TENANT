package agent

import "testing"

// TEN-405: the helper catalog is rendered on every turn, so a helper added
// while the agent runs appears without a restart.
func TestSystemPrompt_RendersAgentsEachTurn(t *testing.T) {
	catalog := "\n• researcher"
	a := &Agent{cfg: Config{SystemPrompt: "base", RenderAgents: func() string { return catalog }}}
	if got := a.systemPrompt(); got != "base\n• researcher" {
		t.Fatalf("first turn: %q", got)
	}
	catalog = "\n• researcher\n• reviewer"
	if got := a.systemPrompt(); got != "base\n• researcher\n• reviewer" {
		t.Fatalf("a helper added mid-run is missing: %q", got)
	}
	if got := (&Agent{cfg: Config{SystemPrompt: "base"}}).systemPrompt(); got != "base" {
		t.Fatalf("no RenderAgents: %q", got)
	}
}
