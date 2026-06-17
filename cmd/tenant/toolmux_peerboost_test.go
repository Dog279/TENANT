package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"tenant/internal/model"
)

// buildKnowledgeMux makes a ranked-size catalog containing wiki_search,
// memory_search, web_search + filler, with an embedder mapping each tool's
// description to the given vector. Query is fixed at {1,0,0,0}.
func buildKnowledgeMux(t *testing.T, vecByDesc map[string][]float32) *toolMux {
	t.Helper()
	m := &toolMux{
		byName:     map[string]*toolEntry{},
		activators: map[string]func() (plugin, func(), error){},
		activated:  map[string]bool{},
		peerDisp:   map[string]plugin{},
	}
	specs := []model.ToolSpec{
		{Name: "wiki_search", Description: "wiki", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "memory_search", Description: "memory", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "web_search", Description: "web", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
	for i := 0; i < 19; i++ { // pad past rankActivateThreshold (20)
		specs = append(specs, model.ToolSpec{
			Name: fmt.Sprintf("tool_%02d", i), Description: "filler", Parameters: json.RawMessage(`{"type":"object"}`),
		})
	}
	m.add("p", &rankFakePlugin{specs: specs})
	m.SetEmbedder("e", &rankFakeEmbedder{byDesc: vecByDesc})
	return m
}

func indexOf(specs []model.ToolSpec, name string) int {
	for i, s := range specs {
		if s.Name == name {
			return i
		}
	}
	return -1
}

// TestPeerBoost_FlipsKnowledgeAboveWebWhenComparable: with comparable base sims,
// a connected peer lifts wiki/memory ABOVE web_search; with no peer, web wins.
func TestPeerBoost_FlipsKnowledgeAboveWebWhenComparable(t *testing.T) {
	vecs := map[string][]float32{
		"web":    {0.7071, 0.7071, 0, 0}, // cosine ~0.707 vs {1,0,0,0}
		"wiki":   {0.6, 0.8, 0, 0},       // cosine 0.6
		"memory": {0.6, 0.8, 0, 0},       // cosine 0.6
		"filler": {0, 1, 0, 0},           // cosine 0
	}
	query := []float32{1, 0, 0, 0}

	// No peer: web_search (0.707) ranks above wiki_search (0.6).
	m := buildKnowledgeMux(t, vecs)
	got, err := m.Search(context.Background(), query, 0)
	if err != nil {
		t.Fatal(err)
	}
	if iw, ik := indexOf(got, "web_search"), indexOf(got, "wiki_search"); !(iw >= 0 && (ik < 0 || iw < ik)) {
		t.Fatalf("no peer: expected web_search ranked above wiki_search; web=%d wiki=%d", iw, ik)
	}

	// Peer connected: boost lifts wiki_search/memory_search ABOVE web_search.
	m2 := buildKnowledgeMux(t, vecs)
	m2.peerDisp["alice"] = &rankFakePlugin{}
	got2, err := m2.Search(context.Background(), query, 0)
	if err != nil {
		t.Fatal(err)
	}
	iw, ik, im := indexOf(got2, "web_search"), indexOf(got2, "wiki_search"), indexOf(got2, "memory_search")
	if ik < 0 || im < 0 || iw < 0 {
		t.Fatalf("peer: all three must be surfaced; wiki=%d memory=%d web=%d", ik, im, iw)
	}
	if !(ik < iw && im < iw) {
		t.Fatalf("peer: boost should rank wiki(%d)+memory(%d) ABOVE web(%d)", ik, im, iw)
	}
}

// TestPeerBoost_DoesNotStarveWebOnWebQuery: the guardrail — even with peers
// connected, a genuinely web-dominant query keeps web_search surfaced (and on
// top). The boost is salience, not a hard route.
func TestPeerBoost_DoesNotStarveWebOnWebQuery(t *testing.T) {
	vecs := map[string][]float32{
		"web":    {1, 0, 0, 0}, // cosine 1.0 — clearly the right tool
		"wiki":   {0, 1, 0, 0}, // cosine 0.0
		"memory": {0, 1, 0, 0}, // cosine 0.0
		"filler": {0, 1, 0, 0}, // cosine 0.0
	}
	m := buildKnowledgeMux(t, vecs)
	m.peerDisp["alice"] = &rankFakePlugin{} // peers connected → boost active
	got, err := m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if err != nil {
		t.Fatal(err)
	}
	iw := indexOf(got, "web_search")
	if iw != 0 {
		t.Fatalf("web-dominant query: web_search must stay on top despite peer boost (boosted knowledge=0.25 < web=1.0); got index %d", iw)
	}
}

// TestPeerBoost_CoreUnionStillSurfacesWhenRankedOut: even when the knowledge
// tools rank BELOW the keep cut (large catalog, tiny cap) AND peers are
// connected, they are still surfaced via the coreToolNames union — the boost is
// salience, the core membership is the hard guarantee.
func TestPeerBoost_CoreUnionStillSurfacesWhenRankedOut(t *testing.T) {
	// 60 filler tools all highly relevant; knowledge tools deliberately
	// irrelevant so even +boost can't lift them into the (capped) keep set.
	m := &toolMux{
		byName:     map[string]*toolEntry{},
		activators: map[string]func() (plugin, func(), error){},
		activated:  map[string]bool{},
		peerDisp:   map[string]plugin{"alice": &rankFakePlugin{}},
	}
	specs := []model.ToolSpec{
		{Name: "wiki_search", Description: "irrelevant", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "memory_search", Description: "irrelevant", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "web_search", Description: "hot", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
	for i := 0; i < 60; i++ {
		specs = append(specs, model.ToolSpec{Name: fmt.Sprintf("tool_%02d", i), Description: "hot", Parameters: json.RawMessage(`{"type":"object"}`)})
	}
	m.add("p", &rankFakePlugin{specs: specs})
	m.SetEmbedder("e", &rankFakeEmbedder{byDesc: map[string][]float32{
		"hot":        {1, 0, 0, 0}, // cosine 1.0 — fills the keep set
		"irrelevant": {0, 1, 0, 0}, // cosine 0.0 — even +0.25 stays at the bottom
	}})
	got, err := m.Search(context.Background(), []float32{1, 0, 0, 0}, 8) // tiny cap
	if err != nil {
		t.Fatal(err)
	}
	if indexOf(got, "wiki_search") < 0 || indexOf(got, "memory_search") < 0 {
		t.Fatalf("core knowledge tools must be surfaced via the core union even when ranked out; got %v", toolNames(got))
	}
}

// TestPeerBoost_NoBoostWithoutPeers: with NO peer connected, the federated
// knowledge tools are NOT boosted — they rank by cosine alone (web_search, more
// relevant to the query, ranks above wiki_search).
func TestPeerBoost_NoBoostWithoutPeers(t *testing.T) {
	vecs := map[string][]float32{
		"web":    {0.7071, 0.7071, 0, 0}, // 0.707
		"wiki":   {0.6, 0.8, 0, 0},       // 0.6
		"memory": {0.6, 0.8, 0, 0},       // 0.6
		"filler": {0, 1, 0, 0},
	}
	m := buildKnowledgeMux(t, vecs) // no peerDisp entries
	got, err := m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if err != nil {
		t.Fatal(err)
	}
	iw, ik := indexOf(got, "web_search"), indexOf(got, "wiki_search")
	if iw < 0 || ik < 0 || iw > ik {
		t.Fatalf("no peers: web_search (0.707) should rank above wiki_search (0.6, unboosted); web=%d wiki=%d", iw, ik)
	}
}

// TestCoreToolNames_KnowledgeTier: both knowledge tools are always-surfaced.
func TestCoreToolNames_KnowledgeTier(t *testing.T) {
	for _, n := range []string{"memory_search", "wiki_search"} {
		if !coreToolNames[n] {
			t.Errorf("%s must be in coreToolNames (always surfaced)", n)
		}
	}
}

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
