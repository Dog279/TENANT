package assemble

// Internal tests for unexported helpers. The black-box assemble_test
// package (assemble_test.go) cannot reach `filterAgent` or
// `extractArtifactURIs` directly — they're load-bearing for TEN-43's
// lost-context fixes and warrant focused unit coverage.

import "testing"

// TestFilterAgent_OrchestratorIncludesSubAgents — TEN-45.
// An orchestrator id (no `-`) returns both itself and its glob pattern
// so the episodic store's filter can match its spawned sub-agents.
func TestFilterAgent_OrchestratorIncludesSubAgents(t *testing.T) {
	got := filterAgent("main")
	want := []string{"main", "main-*"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("filterAgent(\"main\"): want %v, got %v", want, got)
	}
}

// TestFilterAgent_SubAgentSelfOnly — TEN-45.
// A sub-agent id (contains `-`) returns only itself — no reaching into a
// sibling orchestrator's namespace.
func TestFilterAgent_SubAgentSelfOnly(t *testing.T) {
	got := filterAgent("main-researcher-1")
	if len(got) != 1 || got[0] != "main-researcher-1" {
		t.Errorf("filterAgent(\"main-researcher-1\"): want [\"main-researcher-1\"], got %v", got)
	}
}

// TestFilterAgent_SiblingOrchestratorIsolation — TEN-45 negative test.
// A sibling orchestrator like `assistant` must NOT pull in `main-*` —
// only its own sub-agents.
func TestFilterAgent_SiblingOrchestratorIsolation(t *testing.T) {
	got := filterAgent("assistant")
	want := []string{"assistant", "assistant-*"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("filterAgent(\"assistant\"): want %v, got %v", want, got)
	}
}

// TestFilterAgent_EmptyReturnsNil — preserves legacy "no filter" path.
func TestFilterAgent_EmptyReturnsNil(t *testing.T) {
	if got := filterAgent(""); got != nil {
		t.Errorf("filterAgent(\"\"): want nil, got %v", got)
	}
}

// TestExtractArtifactURIs_Basic — TEN-46.
// Extracts wiki:/research:/file: URIs from a citation-style response.
func TestExtractArtifactURIs_Basic(t *testing.T) {
	text := `The summary is X.

## Sources
[1] wiki:research-atlassian-api-2026-05-26.md
[2] https://example.com/page
[3] research:20260526-214121-atlassian-api-in-go-not-cgo
[4] file:./cmd/tenant/research.go
[5] memory:fact-42
`
	got := extractArtifactURIs(text)
	want := []string{
		"wiki:research-atlassian-api-2026-05-26.md",
		"research:20260526-214121-atlassian-api-in-go-not-cgo",
		"file:./cmd/tenant/research.go",
		"memory:fact-42",
	}
	if len(got) != len(want) {
		t.Fatalf("count: want %d, got %d (%v)", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d]: want %q, got %q", i, w, got[i])
		}
	}
}

// TestExtractArtifactURIs_SkipsHTTP — TEN-46.
// http/https are web citations, not internal handles. Excluded.
func TestExtractArtifactURIs_SkipsHTTP(t *testing.T) {
	text := "See https://example.com and http://foo.org/bar"
	if got := extractArtifactURIs(text); len(got) != 0 {
		t.Errorf("want no extraction for http/https, got %v", got)
	}
}

// TestExtractArtifactURIs_Dedups — TEN-46.
// Same URI appearing twice (cited twice in references) is returned once.
func TestExtractArtifactURIs_Dedups(t *testing.T) {
	text := "wiki:foo.md and again wiki:foo.md and once more wiki:foo.md"
	got := extractArtifactURIs(text)
	if len(got) != 1 || got[0] != "wiki:foo.md" {
		t.Errorf("want one dedup'd wiki:foo.md, got %v", got)
	}
}

// TestExtractArtifactURIs_Empty — empty input doesn't allocate.
func TestExtractArtifactURIs_Empty(t *testing.T) {
	if got := extractArtifactURIs(""); got != nil {
		t.Errorf("want nil for empty input, got %v", got)
	}
}
