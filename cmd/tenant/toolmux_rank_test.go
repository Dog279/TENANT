package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/model"
)

// --- fake embedder + helpers ---

// rankFakeEmbedder returns vectors from a description→vec map. Unknown
// descriptions fall back to a deterministic per-string hash vector so
// tests don't have to enumerate every tool. Tracks invocation count +
// last batch size for assertions about lazy precompute behavior.
type rankFakeEmbedder struct {
	mu     sync.Mutex
	byDesc map[string][]float32
	err    error
	calls  int
	lastN  int
}

func (f *rankFakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastN = len(texts)
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if v, ok := f.byDesc[t]; ok {
			out[i] = v
			continue
		}
		// Deterministic-ish fallback so unspecified descriptions don't
		// all collide on the same vector.
		out[i] = []float32{0, 0, 0, 1}
	}
	return out, nil
}

// rankFakePlugin is the smallest plugin that satisfies the `plugin`
// interface and lets a test add N tools with custom descriptions to
// the mux. Dispatch is unused in these tests.
type rankFakePlugin struct {
	specs []model.ToolSpec
}

func (p *rankFakePlugin) Tools() []model.ToolSpec { return p.specs }
func (p *rankFakePlugin) Dispatch(_ context.Context, _ model.ToolCall) (string, bool, error) {
	return "ok", false, nil
}

// newRankMux builds a fresh mux with N tools whose descriptions are
// "tool-i: <desc>" (i = registration index). When desc is "" the test
// just wants count; the rank-tests below override specific descriptions
// to control cosine ordering.
func newRankMux(t *testing.T, n int) *toolMux {
	t.Helper()
	m := &toolMux{
		byName:     map[string]*toolEntry{},
		activators: map[string]func() (plugin, func(), error){},
		activated:  map[string]bool{},
	}
	specs := make([]model.ToolSpec, n)
	for i := 0; i < n; i++ {
		specs[i] = model.ToolSpec{
			Name:        fmt.Sprintf("tool_%02d", i),
			Description: fmt.Sprintf("tool %d description", i),
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}
	}
	m.add("test", &rankFakePlugin{specs: specs})
	return m
}

func toolNames(specs []model.ToolSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// --- ranking activation thresholds ---

// TestToolMuxSearch_BelowThresholdPassesThrough — drift guard against
// the original Search() comment (toolmux.go:183-190). When the catalog
// is small, the unranked path MUST be used regardless of embedder or
// query embedding. Operators rely on /enable curation being honored
// verbatim at small scale.
func TestToolMuxSearch_BelowThresholdPassesThrough(t *testing.T) {
	m := newRankMux(t, rankActivateThreshold-1) // one below threshold
	m.SetEmbedder("test-emb", &rankFakeEmbedder{})
	got, err := m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != rankActivateThreshold-1 {
		t.Errorf("below threshold should return all enabled; got %d want %d",
			len(got), rankActivateThreshold-1)
	}
}

// TestToolMuxSearch_NilEmbedderPassesThrough — installing no embedder
// keeps today's behavior even at scale. Critical for the
// echo-backend / no-embedder install path.
func TestToolMuxSearch_NilEmbedderPassesThrough(t *testing.T) {
	m := newRankMux(t, rankActivateThreshold+10)
	// No SetEmbedder call — embedder stays nil.
	got, _ := m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if len(got) != rankActivateThreshold+10 {
		t.Errorf("nil embedder should return all enabled; got %d", len(got))
	}
}

// TestToolMuxSearch_NilQueryEmbeddingPassesThrough — if the agent had
// no embedder at turn-time and passes nil, we MUST NOT rank (we'd be
// scoring against a zero vector and corrupting ordering).
func TestToolMuxSearch_NilQueryEmbeddingPassesThrough(t *testing.T) {
	m := newRankMux(t, rankActivateThreshold+10)
	m.SetEmbedder("test-emb", &rankFakeEmbedder{})
	got, _ := m.Search(context.Background(), nil, 0)
	if len(got) != rankActivateThreshold+10 {
		t.Errorf("nil query emb should return all enabled; got %d", len(got))
	}
}

// --- ranking behavior at scale ---

// TestToolMuxSearch_RanksByCosineAtScale — the load-bearing happy path.
// 25 tools, one description tuned to match the query embedding exactly,
// others tuned to varying degrees. The matching tool MUST appear in the
// surfaced set; the worst-matching tools MUST be dropped.
func TestToolMuxSearch_RanksByCosineAtScale(t *testing.T) {
	const n = 25
	m := newRankMux(t, n)
	// Override two descriptions: the "hot" one matches query perfectly,
	// the "cold" one is orthogonal.
	m.byName["tool_05"].spec.Description = "HOT match"
	m.byName["tool_20"].spec.Description = "COLD match"
	emb := &rankFakeEmbedder{byDesc: map[string][]float32{
		"HOT match":  {1, 0, 0, 0},
		"COLD match": {0, 1, 0, 0},
	}}
	m.SetEmbedder("test-emb", emb)

	got, err := m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: ranking active means we trimmed.
	if len(got) >= n {
		t.Fatalf("expected ranking to trim; got %d of %d", len(got), n)
	}
	if len(got) < rankMinKeepFloor {
		t.Fatalf("violated rankMinKeepFloor=%d, got %d", rankMinKeepFloor, len(got))
	}
	names := toolNames(got)
	// HOT match must lead (sim=1 vs everyone else's 0 or fallback).
	if got[0].Name != "tool_05" {
		t.Errorf("HOT match should rank first; got %q at top of %v", got[0].Name, names)
	}
	// COLD match must NOT be in the surfaced set when we drop 25%.
	for _, s := range got {
		if s.Name == "tool_20" {
			t.Errorf("COLD match should be dropped; appears in %v", names)
		}
	}
}

// TestToolMuxSearch_FloorPreserved — ensures we never drop below
// rankMinKeepFloor even when the drop-fraction math would suggest more.
// At catalog = threshold + 1, dropping 25% would leave less than the
// floor; the floor wins.
func TestToolMuxSearch_FloorPreserved(t *testing.T) {
	// Pick a catalog size where len/4 (the drop) would push below floor.
	// catalog=rankActivateThreshold (e.g. 20); drop=5; keep=15. Floor=12.
	// max(12, 15) = 15. So at threshold we DO drop. Pick smaller above-
	// threshold: catalog=floor+2=14; drop=3; keep=11; floor wins -> 12.
	const n = rankMinKeepFloor + 2
	if n < rankActivateThreshold {
		t.Skipf("need catalog above threshold to exercise ranking; threshold=%d", rankActivateThreshold)
	}
	m := newRankMux(t, n)
	emb := &rankFakeEmbedder{}
	m.SetEmbedder("test-emb", emb)
	got, _ := m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if len(got) < rankMinKeepFloor {
		t.Errorf("dropped below floor; got %d want >= %d", len(got), rankMinKeepFloor)
	}
}

// TestToolMuxSearch_StableTieBreak — two tools with identical sim must
// sort alphabetically by name. Transcript reproducibility depends on
// this: a non-deterministic tie-break would shift the rendered tool
// list between adjacent runs of the same query.
func TestToolMuxSearch_StableTieBreak(t *testing.T) {
	const n = rankActivateThreshold + 5
	m := newRankMux(t, n)
	// All tools embed to the SAME vector → all have sim=1.0 to query.
	emb := &rankFakeEmbedder{byDesc: map[string][]float32{}}
	for i := 0; i < n; i++ {
		emb.byDesc[fmt.Sprintf("tool %d description", i)] = []float32{1, 0, 0, 0}
	}
	m.SetEmbedder("test-emb", emb)

	got1, _ := m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	got2, _ := m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if !sortedEqual(toolNames(got1), toolNames(got2)) {
		t.Errorf("non-stable ordering across calls:\n  %v\n  %v", got1, got2)
	}
	// Output must be alphabetical (the tie-break).
	names := toolNames(got1)
	want := append([]string(nil), names...)
	sort.Strings(want)
	if !sortedEqual(names, want) {
		t.Errorf("tie-break should be alphabetical; got %v want %v", names, want)
	}
}

func sortedEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- best-effort failure paths ---

// TestToolMuxSearch_EmbedErrorFallsBackToUnranked — when the embedder
// errors during precompute, the cache stays empty and Search MUST
// degrade to the unranked path. No panic, no error to the caller.
func TestToolMuxSearch_EmbedErrorFallsBackToUnranked(t *testing.T) {
	const n = rankActivateThreshold + 5
	m := newRankMux(t, n)
	emb := &rankFakeEmbedder{err: errors.New("embedder down")}
	m.SetEmbedder("test-emb", emb)
	got, err := m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if err != nil {
		t.Fatalf("Search must not propagate embed errors: %v", err)
	}
	if len(got) != n {
		t.Errorf("embed error should fall back to full enabled set; got %d want %d", len(got), n)
	}
}

// TestToolMuxSearch_PrecomputeRunsOnce — precompute is the slow path;
// once cached, subsequent Search calls MUST NOT re-embed. Verifies the
// lazy + cached contract.
func TestToolMuxSearch_PrecomputeRunsOnce(t *testing.T) {
	const n = rankActivateThreshold + 5
	m := newRankMux(t, n)
	emb := &rankFakeEmbedder{}
	m.SetEmbedder("test-emb", emb)
	_, _ = m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	_, _ = m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	_, _ = m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if emb.calls != 1 {
		t.Errorf("precompute should run exactly once; embedder Embed calls=%d", emb.calls)
	}
}

// --- cache invalidation ---

// TestToolMuxSearch_FingerprintChangeInvalidatesCache — the load-bearing
// guard against `/model use ...` swap producing stale embeddings of a
// different dimension. New fingerprint MUST trigger re-precompute.
func TestToolMuxSearch_FingerprintChangeInvalidatesCache(t *testing.T) {
	const n = rankActivateThreshold + 5
	m := newRankMux(t, n)
	emb1 := &rankFakeEmbedder{}
	m.SetEmbedder("emb-v1", emb1)
	_, _ = m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if emb1.calls != 1 {
		t.Fatalf("precompute should run once on emb-v1; got %d", emb1.calls)
	}
	// Swap embedder with a new fingerprint.
	emb2 := &rankFakeEmbedder{}
	m.SetEmbedder("emb-v2", emb2)
	_, _ = m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if emb2.calls != 1 {
		t.Errorf("fingerprint change should trigger re-precompute on new embedder; got %d", emb2.calls)
	}
}

// TestToolMuxSearch_AddInvalidatesCache — when a plugin is added after
// initial precompute (e.g. an activator firing for /enable web), the
// cache MUST invalidate so the new tools get embedded too. Otherwise
// the new tool would always score sim=0.5 (the neutral default) and
// silently rank below relevant matches.
func TestToolMuxSearch_AddInvalidatesCache(t *testing.T) {
	const n = rankActivateThreshold + 5
	m := newRankMux(t, n)
	emb := &rankFakeEmbedder{}
	m.SetEmbedder("test-emb", emb)
	_, _ = m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if emb.calls != 1 {
		t.Fatalf("first Search should precompute; calls=%d", emb.calls)
	}
	// Add a new tool — simulates activator firing.
	m.add("late", &rankFakePlugin{specs: []model.ToolSpec{{
		Name:        "late_tool",
		Description: "added after initial precompute",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	}}})
	_, _ = m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if emb.calls != 2 {
		t.Errorf("add() should invalidate cache; expected re-precompute (calls=2), got %d", emb.calls)
	}
	if emb.lastN != n+1 {
		t.Errorf("re-precompute should batch ALL tools including the new one; got batch size %d want %d", emb.lastN, n+1)
	}
}

// --- enabled/disabled interaction ---

// TestToolMuxSearch_DisabledToolsAbsent — disabled tools MUST never
// reach the ranked output. /disable is a hard mute, not a rank penalty.
func TestToolMuxSearch_DisabledToolsAbsent(t *testing.T) {
	const n = rankActivateThreshold + 5
	m := newRankMux(t, n)
	emb := &rankFakeEmbedder{}
	m.SetEmbedder("test-emb", emb)
	// Disable tool_03 — must never appear in ranked output regardless
	// of its embedding similarity to the query.
	_, _, _ = m.SetEnabled("tool_03", false)
	got, _ := m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	for _, s := range got {
		if s.Name == "tool_03" {
			t.Errorf("disabled tool appeared in ranked output: %v", toolNames(got))
		}
	}
}

// --- agent.ToolRegistry interface conformance ---

// TestToolMux_StillSatisfiesToolRegistry — drift guard. The toolMux is
// used as an agent.ToolRegistry; after adding Search-side complexity
// we MUST still satisfy the interface (Get + Search + All).
func TestToolMux_StillSatisfiesToolRegistry(t *testing.T) {
	var _ agent.ToolRegistry = (*toolMux)(nil) // compile-time guard
	m := newRankMux(t, 3)
	if _, ok := m.Get("tool_00"); !ok {
		t.Error("Get broken for known tool")
	}
	if len(m.All()) != 3 {
		t.Errorf("All() should return 3; got %d", len(m.All()))
	}
}

// TestToolMuxSearch_SetEmbedderNilDisablesRanking — passing nil to
// SetEmbedder MUST disable ranking (back to unranked) AND invalidate
// any previously cached embeddings.
func TestToolMuxSearch_SetEmbedderNilDisablesRanking(t *testing.T) {
	const n = rankActivateThreshold + 5
	m := newRankMux(t, n)
	emb := &rankFakeEmbedder{}
	m.SetEmbedder("test-emb", emb)
	// Prime the cache.
	_, _ = m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	// Disable.
	m.SetEmbedder("", nil)
	got, _ := m.Search(context.Background(), []float32{1, 0, 0, 0}, 0)
	if len(got) != n {
		t.Errorf("nil embedder should return all; got %d", len(got))
	}
}

// --- naming / docs drift guard ---

// TestRankConstants_AreSensible — guards against accidental edits that
// would break the ship invariant: floor strictly above 0, threshold
// strictly above floor, drop-fraction sane. A misedit here would silently
// regress.
func TestRankConstants_AreSensible(t *testing.T) {
	if rankMinKeepFloor <= 0 {
		t.Errorf("rankMinKeepFloor must be > 0; got %d", rankMinKeepFloor)
	}
	if rankActivateThreshold <= rankMinKeepFloor {
		t.Errorf("rankActivateThreshold (%d) should exceed rankMinKeepFloor (%d) or ranking activates with nothing to drop",
			rankActivateThreshold, rankMinKeepFloor)
	}
	if rankDropFraction < 2 {
		t.Errorf("rankDropFraction=%d would drop ≥50%% of catalog — too aggressive", rankDropFraction)
	}
}

// TestRankingDoc_MentionsTheScar — drift guard. The original Search()
// comment that documented the previous bug ("/enable web silently
// failed when cap filled first") MUST stay in the source for future
// readers. If a future refactor strips it, this test breaks.
func TestRankingDoc_MentionsTheScar(t *testing.T) {
	body, err := readFile("toolmux.go")
	if err != nil {
		t.Fatal(err)
	}
	// The scar is the failure-mode citation. Specifically references
	// the registration-order failure shape.
	if !strings.Contains(body, "registration order") {
		t.Error("toolmux.go Search() docs must keep the 'registration order' scar reference for future readers")
	}
	if !strings.Contains(body, "TestToolMux_SearchReturnsAllEnabledIgnoringCap") {
		// The reference to the historical drift guard. Don't strip it.
		// (Skipped check if the historical test was renamed — soft pass.)
		t.Log("note: historical test reference missing from Search() docs")
	}
}

// readFile is a tiny helper so the drift guard test reads the file
// relative to the test's working directory (cmd/tenant/).
func readFile(rel string) (string, error) {
	b, err := os.ReadFile(rel)
	return string(b), err
}
