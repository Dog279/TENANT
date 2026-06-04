package episodic_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"tenant/internal/memory/episodic"
)

func mkStore(t *testing.T) *episodic.Store {
	t.Helper()
	// Use an on-disk DB under TempDir so file-rotation behavior matches prod.
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := episodic.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mkEpisode(content string, embedding []float32) *episodic.Episode {
	return &episodic.Episode{
		AgentID:    "main",
		Visibility: episodic.VisibilityPrivate,
		SessionID:  "sess1",
		Prompt:     "user: " + content,
		Response:   "assistant: response about " + content,
		Outcome:    episodic.OutcomeSuccess,
		Tags:       []string{"chat"},
		EmbedderID: "test-embedder",
		Embedding:  embedding,
	}
}

// orthogonal vectors of arbitrary "topic" — used to make cosine
// scores deterministic in tests.
func vecGo() []float32     { return []float32{1, 0, 0, 0, 0, 0, 0, 0} }
func vecPython() []float32 { return []float32{0, 1, 0, 0, 0, 0, 0, 0} }
func vecRust() []float32   { return []float32{0, 0, 1, 0, 0, 0, 0, 0} }

func TestStore_InsertAndGet(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	ep := mkEpisode("golang concurrency", vecGo())
	id, err := s.Insert(ctx, ep)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id <= 0 {
		t.Fatalf("Insert returned id=%d", id)
	}
	if ep.ID != id {
		t.Fatalf("ep.ID = %d, want %d", ep.ID, id)
	}
	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Prompt != ep.Prompt {
		t.Errorf("Prompt roundtrip failed: %q != %q", got.Prompt, ep.Prompt)
	}
	if len(got.Embedding) != len(ep.Embedding) || got.Embedding[0] != 1 {
		t.Errorf("Embedding roundtrip failed: got %v want %v", got.Embedding, ep.Embedding)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "chat" {
		t.Errorf("Tags roundtrip failed: %v", got.Tags)
	}
}

func TestStore_InsertRequiresFields(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	cases := map[string]*episodic.Episode{
		"missing AgentID":    {EmbedderID: "e", Embedding: vecGo()},
		"missing EmbedderID": {AgentID: "a", Embedding: vecGo()},
		"missing Embedding":  {AgentID: "a", EmbedderID: "e"},
	}
	for name, ep := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Insert(ctx, ep); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestStore_InsertWithToolCalls(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	ep := mkEpisode("with tools", vecGo())
	ep.ToolCalls = []episodic.ToolCallRef{
		{ID: "call_1", Name: "search", Arguments: `{"q":"go"}`},
	}
	id, err := s.Insert(ctx, ep)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "search" {
		t.Errorf("ToolCalls roundtrip failed: %+v", got.ToolCalls)
	}
}

// Get must tolerate a corrupt tool_calls / tags JSON blob in a stored row —
// the episode's prompt/response are independently useful and a single bad
// row should not block hydrate (which would block retrieval, which would
// block /research and every agent turn). A live trigger: an earlier
// model-output bug wrote literal "garbage" into tool_calls and surfaced
// months later as `assemble … invalid character 'g'`.
func TestStore_GetTolerantToCorruptToolCallsJSON(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	ep := mkEpisode("with bad meta", vecGo())
	id, err := s.Insert(ctx, ep)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Poison the row directly — simulate what the live corrupt rows look like.
	if _, err := s.DB().ExecContext(ctx,
		"UPDATE episodes SET tool_calls = ?, tags = ? WHERE id = ?",
		"garbage not json", "{also bad", id); err != nil {
		t.Fatalf("poison: %v", err)
	}
	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get must succeed on corrupt JSON fields, got: %v", err)
	}
	if got.Prompt != ep.Prompt {
		t.Errorf("prompt lost across tolerant hydrate: %q vs %q", got.Prompt, ep.Prompt)
	}
	if got.ToolCalls != nil {
		t.Errorf("corrupt tool_calls should become nil, got %+v", got.ToolCalls)
	}
	if got.Tags != nil {
		t.Errorf("corrupt tags should become nil, got %+v", got.Tags)
	}
}

func TestStore_GetMissingReturnsErrNotFound(t *testing.T) {
	s := mkStore(t)
	_, err := s.Get(context.Background(), 999)
	if !errors.Is(err, episodic.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStore_TombstoneHidesFromSearch(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	id, err := s.Insert(ctx, mkEpisode("golang concurrency", vecGo()))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tombstone(ctx, id); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	// Get still returns the episode (audit path).
	ep, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after Tombstone: %v", err)
	}
	if !ep.Tombstoned {
		t.Fatal("Tombstoned flag not set")
	}
	// Search must NOT find it.
	hits, err := s.Search(ctx, episodic.Query{Embedding: vecGo(), K: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("Search found tombstoned episode: %+v", hits)
	}
}

func TestStore_TombstoneMissingIsError(t *testing.T) {
	s := mkStore(t)
	err := s.Tombstone(context.Background(), 999)
	if !errors.Is(err, episodic.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStore_Count(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	for _, c := range []string{"a", "b", "c"} {
		if _, err := s.Insert(ctx, mkEpisode(c, vecGo())); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Count(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("Count = %d, want 3", n)
	}
}

func TestStore_CountExcludeTombstoned(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	id, _ := s.Insert(ctx, mkEpisode("a", vecGo()))
	_, _ = s.Insert(ctx, mkEpisode("b", vecGo()))
	_ = s.Tombstone(ctx, id)

	all, _ := s.Count(ctx, true)
	live, _ := s.Count(ctx, false)
	if all != 2 || live != 1 {
		t.Fatalf("Counts: all=%d live=%d; want all=2 live=1", all, live)
	}
}

func TestSearch_VectorOnly(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	// Insert orthogonal episodes: query for vecGo should rank "go" first.
	_, _ = s.Insert(ctx, mkEpisode("about python", vecPython()))
	_, _ = s.Insert(ctx, mkEpisode("about rust", vecRust()))
	goID, _ := s.Insert(ctx, mkEpisode("about golang", vecGo()))

	hits, err := s.Search(ctx, episodic.Query{Embedding: vecGo(), K: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].Episode.ID != goID {
		t.Fatalf("top hit = %+v, want id=%d", hits[0], goID)
	}
	if hits[0].VecRank != 1 {
		t.Errorf("top hit VecRank = %d, want 1", hits[0].VecRank)
	}
}

func TestSearch_FTSOnly(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	_, _ = s.Insert(ctx, mkEpisode("python is great", vecPython()))
	goID, _ := s.Insert(ctx, mkEpisode("golang concurrency rocks", vecGo()))

	hits, err := s.Search(ctx, episodic.Query{Keywords: "golang", K: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Episode.ID != goID {
		t.Fatalf("FTS hits = %+v, want one match on id=%d", hits, goID)
	}
	if hits[0].FTSRank != 1 {
		t.Errorf("FTSRank = %d, want 1", hits[0].FTSRank)
	}
}

func TestSearch_HybridFusion(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	// "golang concurrency" wins both signals — should fuse to top.
	_, _ = s.Insert(ctx, mkEpisode("python decorators", vecPython()))
	hybridID, _ := s.Insert(ctx, mkEpisode("golang concurrency model", vecGo()))
	_, _ = s.Insert(ctx, mkEpisode("rust ownership", vecRust()))

	hits, err := s.Search(ctx, episodic.Query{
		Embedding: vecGo(),
		Keywords:  "golang",
		K:         3,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].Episode.ID != hybridID {
		t.Fatalf("hybrid top hit = %+v, want id=%d", hits[0], hybridID)
	}
	if hits[0].VecRank != 1 || hits[0].FTSRank != 1 {
		t.Errorf("hybrid top should rank 1 in both: vec=%d fts=%d", hits[0].VecRank, hits[0].FTSRank)
	}
	// Fusion = 0.7*cosine + 0.3*(1/ftsRank). The hybrid winner shares
	// vecGo exactly (cosine ~1) and is FTS rank 1, so its score must be
	// the max and clearly above the vec-only / fts-only candidates.
	if hits[0].Score <= 0 {
		t.Errorf("top hit score = %v, want > 0", hits[0].Score)
	}
	for _, h := range hits[1:] {
		if h.Score >= hits[0].Score {
			t.Errorf("non-top hit %d scored %v >= top %v", h.Episode.ID, h.Score, hits[0].Score)
		}
	}
}

func TestSearch_RequiresAtLeastOneSignal(t *testing.T) {
	s := mkStore(t)
	_, err := s.Search(context.Background(), episodic.Query{K: 5})
	if err == nil {
		t.Fatal("expected error when both Embedding and Keywords empty")
	}
}

func TestSearch_FiltersByAgentID(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	for _, agent := range []string{"alice", "bob"} {
		ep := mkEpisode("about golang", vecGo())
		ep.AgentID = agent
		if _, err := s.Insert(ctx, ep); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.Search(ctx, episodic.Query{
		AgentIDs:  []string{"alice"},
		Embedding: vecGo(),
		K:         10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Episode.AgentID != "alice" {
		t.Fatalf("agent filter broken: got %+v", hits)
	}
}

func TestSearch_FiltersByVisibility(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	for _, v := range []string{episodic.VisibilityPrivate, episodic.VisibilityShared} {
		ep := mkEpisode("about golang", vecGo())
		ep.Visibility = v
		if _, err := s.Insert(ctx, ep); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.Search(ctx, episodic.Query{
		Visibility: []string{episodic.VisibilityShared},
		Embedding:  vecGo(),
		K:          10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Episode.Visibility != episodic.VisibilityShared {
		t.Fatalf("visibility filter broken: %+v", hits)
	}
}

func TestSearch_FiltersByTimeWindow(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for _, ts := range []time.Time{old, mid, recent} {
		ep := mkEpisode("about golang", vecGo())
		ep.Timestamp = ts
		if _, err := s.Insert(ctx, ep); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.Search(ctx, episodic.Query{
		Embedding: vecGo(),
		After:     time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		Before:    time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		K:         10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("time filter broken: got %d hits", len(hits))
	}
	if !hits[0].Episode.Timestamp.Equal(mid) {
		t.Errorf("got ts %v, want %v", hits[0].Episode.Timestamp, mid)
	}
}

func TestSearch_TopKCap(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		_, _ = s.Insert(ctx, mkEpisode("about golang", vecGo()))
	}
	hits, err := s.Search(ctx, episodic.Query{Embedding: vecGo(), K: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 5 {
		t.Fatalf("expected 5 hits, got %d", len(hits))
	}
}

func TestSearch_EmptyDBReturnsNoHitsNoError(t *testing.T) {
	s := mkStore(t)
	hits, err := s.Search(context.Background(), episodic.Query{Embedding: vecGo(), K: 10})
	if err != nil {
		t.Fatalf("Search on empty: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("got %d hits on empty DB", len(hits))
	}
}

func TestSearch_FTSAndVecDifferentHits(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	// Episode A: keyword match, no vec match (orthogonal).
	a, _ := s.Insert(ctx, mkEpisode("matching keyword golang", vecRust()))
	// Episode B: vec match, no keyword match.
	b, _ := s.Insert(ctx, mkEpisode("unrelated topic", vecGo()))
	// Both should appear in hybrid search.
	hits, err := s.Search(ctx, episodic.Query{
		Embedding: vecGo(),
		Keywords:  "golang",
		K:         5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 fused hits, got %d: %+v", len(hits), hits)
	}
	ids := map[int64]bool{}
	for _, h := range hits {
		ids[h.Episode.ID] = true
	}
	if !ids[a] || !ids[b] {
		t.Errorf("expected both a=%d and b=%d in hits, got %v", a, b, ids)
	}
}

func TestStore_CloseIsIdempotent(t *testing.T) {
	s := mkStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestEmbedding_RoundtripPreservesValues(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	vec := []float32{1.5, -2.25, 0, 0.001, 1e10, -1e-10}
	ep := mkEpisode("precision test", vec)
	id, err := s.Insert(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Embedding) != len(vec) {
		t.Fatalf("length mismatch: %d vs %d", len(got.Embedding), len(vec))
	}
	for i := range vec {
		if got.Embedding[i] != vec[i] {
			t.Errorf("dim %d: got %v want %v", i, got.Embedding[i], vec[i])
		}
	}
}


// TestSearch_AgentGlob — TEN-45.
// An agent-id ending in `*` matches all episodes whose agent_id starts
// with the prefix. This is how the assembler's filterAgent expands
// "main" → ["main", "main-*"] so an orchestrator sees its sub-agents.
func TestSearch_AgentGlob(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	cases := []struct {
		agent string
		vis   string
	}{
		{"main", episodic.VisibilityShared},
		{"main-researcher-1", episodic.VisibilityShared},
		{"main-researcher-2", episodic.VisibilityShared},
		{"other", episodic.VisibilityShared},
		{"otherthing", episodic.VisibilityShared}, // ensure "main-" prefix doesn't false-match "other"
	}
	for _, c := range cases {
		ep := mkEpisode("about golang", vecGo())
		ep.AgentID = c.agent
		ep.Visibility = c.vis
		if _, err := s.Insert(ctx, ep); err != nil {
			t.Fatal(err)
		}
	}
	// Query as orchestrator: ["main", "main-*"] should return self + sub-agents,
	// NOT "other" or "otherthing".
	hits, err := s.Search(ctx, episodic.Query{
		AgentIDs:   []string{"main", "main-*"},
		Visibility: []string{episodic.VisibilityShared},
		Embedding:  vecGo(),
		K:          10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("agent glob: want 3 hits (main + 2 sub-agents), got %d: %+v", len(hits), agentIDsFrom(hits))
	}
	wantAgents := map[string]bool{"main": true, "main-researcher-1": true, "main-researcher-2": true}
	for _, h := range hits {
		if !wantAgents[h.Episode.AgentID] {
			t.Errorf("unexpected agent in glob match: %q", h.Episode.AgentID)
		}
	}
}

// TestSearch_GlobRespectsVisibility — TEN-45.
// A sub-agent's PRIVATE episode does NOT cross to the orchestrator's
// query — only shared episodes traverse the namespace boundary.
func TestSearch_GlobRespectsVisibility(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	// Orchestrator's own episode: shared.
	ep1 := mkEpisode("about golang", vecGo())
	ep1.AgentID, ep1.Visibility = "main", episodic.VisibilityShared
	if _, err := s.Insert(ctx, ep1); err != nil {
		t.Fatal(err)
	}
	// Sub-agent's PRIVATE episode: must not surface to orchestrator.
	ep2 := mkEpisode("about golang", vecGo())
	ep2.AgentID, ep2.Visibility = "main-researcher-1", episodic.VisibilityPrivate
	if _, err := s.Insert(ctx, ep2); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, episodic.Query{
		AgentIDs:   []string{"main", "main-*"},
		Visibility: []string{episodic.VisibilityShared},
		Embedding:  vecGo(),
		K:          10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Episode.AgentID != "main" {
		t.Fatalf("privacy boundary broken: want only main's shared episode, got %v", agentIDsFrom(hits))
	}
}

func agentIDsFrom(hits []episodic.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Episode.AgentID
	}
	return out
}
