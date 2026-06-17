package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"tenant/internal/model"
)

// TestToolMuxSearch_MaxToolsCapTrimsHard — the TEN-226 cut: honoring the
// profile's MaxToolsPerCall ceiling trims far harder than the historical 75%.
func TestToolMuxSearch_MaxToolsCapTrimsHard(t *testing.T) {
	const n = 40
	m := newRankMux(t, n)
	m.SetEmbedder("test-emb", &rankFakeEmbedder{})

	// No cap (0) → historical ~75% (drop 25%): keep = max(12, 40-10) = 30.
	got0, _ := m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if len(got0) != 30 {
		t.Errorf("no cap should keep 75%% (30 of 40); got %d", len(got0))
	}
	// Cap=12 → keep 12 (the big token cut). No tool_NN is a core name, so the
	// core union adds nothing.
	got12, _ := m.Search(context.Background(), []float32{1, 0, 0, 0}, 12)
	if len(got12) != 12 {
		t.Errorf("MaxToolsPerCall=12 should cap to 12; got %d", len(got12))
	}
}

// TestToolMuxSearch_FloorBeatsTinyCap — a tiny cap (the small profile's 3)
// must not starve the agent: the floor wins.
func TestToolMuxSearch_FloorBeatsTinyCap(t *testing.T) {
	const n = 40
	m := newRankMux(t, n)
	m.SetEmbedder("test-emb", &rankFakeEmbedder{})
	got, _ := m.Search(context.Background(), []float32{1, 0, 0, 0}, 3)
	if len(got) != rankMinKeepFloor {
		t.Errorf("cap below floor should be raised to floor=%d; got %d", rankMinKeepFloor, len(got))
	}
}

// TestToolMuxSearch_CoreSetNeverRankedOut — a core tool (os_exec) that scores
// LAST must still be surfaced even under a tight cap. The cardinal rule.
func TestToolMuxSearch_CoreSetNeverRankedOut(t *testing.T) {
	const n = 25
	m := newRankMux(t, n)
	m.add("osplug", &rankFakePlugin{specs: []model.ToolSpec{{
		Name: "os_exec", Description: "COLD core", Parameters: json.RawMessage(`{"type":"object"}`),
	}}})
	// All tool_NN are HOT (sim 1); os_exec is orthogonal (sim 0) → ranks dead last.
	emb := &rankFakeEmbedder{byDesc: map[string][]float32{"COLD core": {0, 1, 0, 0}}}
	for i := 0; i < n; i++ {
		emb.byDesc[fmt.Sprintf("tool %d description", i)] = []float32{1, 0, 0, 0}
	}
	m.SetEmbedder("test-emb", emb)

	got, _ := m.Search(context.Background(), []float32{1, 0, 0, 0}, 12) // tight cap fills with HOT tools
	found := false
	for _, s := range got {
		if s.Name == "os_exec" {
			found = true
		}
	}
	if !found {
		t.Errorf("core tool os_exec must be force-included despite COLD rank + cap; surfaced=%v", toolNames(got))
	}
}

// TestToolMuxSearch_DimMismatchFallsBack — query embedding of a different
// dimension than the cached tool embeddings must fall back to the full enabled
// set with a visible reason, not rank on all-zero cosines (alphabetical garbage).
func TestToolMuxSearch_DimMismatchFallsBack(t *testing.T) {
	const n = rankActivateThreshold + 5
	m := newRankMux(t, n)
	m.SetEmbedder("test-emb", &rankFakeEmbedder{}) // cache vectors are 4-d
	// Prime + rank with a matching 4-d query.
	if got := mustSearch(t, m, []float32{1, 0, 0, 0}, 0); len(got) >= n {
		t.Fatalf("4-d query should rank (trim); got %d of %d", len(got), n)
	}
	// 3-d query → dim mismatch → full set.
	got := mustSearch(t, m, []float32{1, 0, 0}, 0)
	if len(got) != n {
		t.Errorf("dim mismatch should fall back to the full enabled set; got %d want %d", len(got), n)
	}
	ranked, _, _, reason, _ := m.RankingStatus()
	if ranked || !strings.Contains(reason, "dim mismatch") {
		t.Errorf("dim guard should record a mismatch fallback; ranked=%v reason=%q", ranked, reason)
	}
}

// TestToolMuxSearch_AdoptLiveMCPInvalidatesCache — adopting a live remote MCP
// (e.g. Atlassian's 31 tools) after the first precompute must invalidate the
// cache so the new tools get real embeddings, not the neutral sim=0.5 that would
// let them crowd out relevant local tools.
func TestToolMuxSearch_AdoptLiveMCPInvalidatesCache(t *testing.T) {
	const n = rankActivateThreshold + 5
	m := newRankMux(t, n)
	emb := &rankFakeEmbedder{}
	m.SetEmbedder("test-emb", emb)
	_, _ = m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if emb.calls != 1 {
		t.Fatalf("first Search should precompute once; calls=%d", emb.calls)
	}
	m.adoptLiveMCP("mcp:remote", &rankFakePlugin{specs: []model.ToolSpec{{
		Name: "remote_tool", Description: "adopted remote tool", Parameters: json.RawMessage(`{"type":"object"}`),
	}}}, nil)
	_, _ = m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if emb.calls != 2 {
		t.Errorf("adoptLiveMCP should invalidate cache → re-precompute (calls=2); got %d", emb.calls)
	}
	if emb.lastN != n+1 {
		t.Errorf("re-precompute should batch the adopted tool too; lastN=%d want %d", emb.lastN, n+1)
	}
}

func mustSearch(t *testing.T, m *toolMux, q []float32, maxTools int) []model.ToolSpec {
	t.Helper()
	got, err := m.Search(context.Background(), q, maxTools)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return got
}
