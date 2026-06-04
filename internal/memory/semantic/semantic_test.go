package semantic_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"tenant/internal/memory/semantic"
)

func mkStore(t *testing.T) *semantic.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "facts.db")
	s, err := semantic.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mkFact(claim string, embedding []float32) *semantic.Fact {
	return &semantic.Fact{
		AgentID:    "main",
		Visibility: semantic.VisibilityPrivate,
		Fact:       claim,
		Confidence: 1.0,
		EmbedderID: "test-embedder",
		Embedding:  embedding,
	}
}

func vecA() []float32 { return []float32{1, 0, 0, 0} }
func vecB() []float32 { return []float32{0, 1, 0, 0} }
func vecC() []float32 { return []float32{0, 0, 1, 0} }

func TestStore_InsertAndGet(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	f := mkFact("user prefers Go over Python", vecA())
	f.SourceEpisodes = []int64{1, 2, 3}
	id, err := s.Insert(ctx, f)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Fact != "user prefers Go over Python" {
		t.Errorf("Fact roundtrip: %q", got.Fact)
	}
	if len(got.SourceEpisodes) != 3 {
		t.Errorf("SourceEpisodes roundtrip: %v", got.SourceEpisodes)
	}
	if got.Confidence != 1.0 {
		t.Errorf("Confidence default: %v", got.Confidence)
	}
	if got.LastConfirmed.IsZero() {
		t.Error("LastConfirmed not auto-set")
	}
}

func TestStore_InsertRequiresFields(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	cases := map[string]*semantic.Fact{
		"missing AgentID":    {Fact: "x", EmbedderID: "e", Embedding: vecA()},
		"missing Fact text":  {AgentID: "a", EmbedderID: "e", Embedding: vecA()},
		"missing EmbedderID": {AgentID: "a", Fact: "x", Embedding: vecA()},
		"missing Embedding":  {AgentID: "a", Fact: "x", EmbedderID: "e"},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Insert(ctx, f); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestStore_GetMissingReturnsErrNotFound(t *testing.T) {
	s := mkStore(t)
	_, err := s.Get(context.Background(), 999)
	if !errors.Is(err, semantic.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStore_SupersedeHidesOldFromSearch(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	oldID, _ := s.Insert(ctx, mkFact("user prefers Python", vecA()))
	newID, _ := s.Insert(ctx, mkFact("user prefers Go", vecA()))

	if err := s.Supersede(ctx, oldID, newID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	// Get still returns the old fact (audit).
	got, err := s.Get(ctx, oldID)
	if err != nil {
		t.Fatalf("Get superseded: %v", err)
	}
	if got.SupersededBy != newID {
		t.Errorf("SupersededBy = %d, want %d", got.SupersededBy, newID)
	}
	// Search filters it out.
	hits, err := s.Search(ctx, semantic.Query{Embedding: vecA(), K: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range hits {
		if h.Fact.ID == oldID {
			t.Errorf("Search returned superseded fact id=%d", oldID)
		}
	}
}

func TestStore_SupersedeRejectsSelf(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	id, _ := s.Insert(ctx, mkFact("a", vecA()))
	if err := s.Supersede(ctx, id, id); err == nil {
		t.Fatal("expected error superseding self")
	}
}

// Restore clears a tombstone so the fact returns to the live count, and
// errors with ErrNotFound on a missing id. Reaffirm alone would NOT do
// this (it leaves tombstoned=1), which is why Restore exists.
func TestStore_RestoreUnTombstones(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	id, _ := s.Insert(ctx, mkFact("user likes tea", vecA()))
	if err := s.Tombstone(ctx, id); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	if n, _ := s.Count(ctx, false, false); n != 0 {
		t.Fatalf("live count after tombstone = %d, want 0", n)
	}
	if err := s.Restore(ctx, id); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n, _ := s.Count(ctx, false, false); n != 1 {
		t.Fatalf("live count after restore = %d, want 1", n)
	}
	got, _ := s.Get(ctx, id)
	if got.Tombstoned {
		t.Fatal("fact still tombstoned after Restore")
	}
	if err := s.Restore(ctx, 999); !errors.Is(err, semantic.ErrNotFound) {
		t.Fatalf("Restore missing id err = %v, want ErrNotFound", err)
	}
}

// ListPage paginates live facts by id DESC keyset, excludes tombstoned +
// superseded rows, and walks the whole store across pages without gaps or
// repeats.
func TestStore_ListPageKeyset(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 5; i++ {
		id, _ := s.Insert(ctx, mkFact(fmt.Sprintf("fact %d", i), vecA()))
		ids = append(ids, id)
	}
	// Tombstone one + supersede another so they're excluded from paging.
	_ = s.Tombstone(ctx, ids[0])
	_ = s.Supersede(ctx, ids[1], ids[4])

	// Page size 2 over the 3 remaining live facts (ids[2], ids[3], ids[4]).
	var got []int64
	var cursor int64
	for {
		page, err := s.ListPage(ctx, "main", cursor, 2)
		if err != nil {
			t.Fatalf("ListPage: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, f := range page {
			got = append(got, f.ID)
			if f.Tombstoned {
				t.Fatalf("ListPage returned tombstoned fact %d", f.ID)
			}
		}
		cursor = page[len(page)-1].ID
		if len(page) < 2 {
			break
		}
	}
	// Expect ids[4], ids[3], ids[2] in descending order, no dups.
	want := []int64{ids[4], ids[3], ids[2]}
	if len(got) != len(want) {
		t.Fatalf("paged ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("page order = %v, want %v", got, want)
		}
	}
}

func TestStore_SupersedeRejectsMissingIDs(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	id, _ := s.Insert(ctx, mkFact("a", vecA()))
	if err := s.Supersede(ctx, id, 999); !errors.Is(err, semantic.ErrNotFound) {
		t.Errorf("supersede unknown new: %v", err)
	}
	if err := s.Supersede(ctx, 999, id); !errors.Is(err, semantic.ErrNotFound) {
		t.Errorf("supersede unknown old: %v", err)
	}
}

func TestStore_ReaffirmBumpsLastConfirmed(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	f := mkFact("x", vecA())
	// Insert with a stale LastConfirmed (a year ago).
	f.LastConfirmed = time.Now().UTC().Add(-365 * 24 * time.Hour)
	id, err := s.Insert(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get(ctx, id)
	if err := s.Reaffirm(ctx, id); err != nil {
		t.Fatalf("Reaffirm: %v", err)
	}
	after, _ := s.Get(ctx, id)
	if !after.LastConfirmed.After(before.LastConfirmed) {
		t.Errorf("LastConfirmed did not advance: %v -> %v", before.LastConfirmed, after.LastConfirmed)
	}
}

func TestStore_ReaffirmMissingIsError(t *testing.T) {
	s := mkStore(t)
	if err := s.Reaffirm(context.Background(), 999); !errors.Is(err, semantic.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStore_TombstoneHidesFromSearch(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	id, _ := s.Insert(ctx, mkFact("forget this", vecA()))
	if err := s.Tombstone(ctx, id); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	got, _ := s.Get(ctx, id)
	if !got.Tombstoned {
		t.Error("Tombstoned flag not set on Get")
	}
	hits, _ := s.Search(ctx, semantic.Query{Embedding: vecA(), K: 10})
	if len(hits) != 0 {
		t.Errorf("Search returned tombstoned fact: %+v", hits)
	}
}

func TestStore_CountVariants(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	live1, _ := s.Insert(ctx, mkFact("a", vecA()))
	live2, _ := s.Insert(ctx, mkFact("b", vecB()))
	old, _ := s.Insert(ctx, mkFact("c-old", vecC()))
	newer, _ := s.Insert(ctx, mkFact("c-new", vecC()))
	_ = s.Supersede(ctx, old, newer)
	_ = s.Tombstone(ctx, live2)

	full, _ := s.Count(ctx, true, true)
	noTomb, _ := s.Count(ctx, false, true)
	noSup, _ := s.Count(ctx, true, false)
	liveOnly, _ := s.Count(ctx, false, false)

	if full != 4 {
		t.Errorf("full count = %d, want 4", full)
	}
	if noTomb != 3 {
		t.Errorf("no-tombstoned count = %d, want 3", noTomb)
	}
	if noSup != 3 {
		t.Errorf("no-superseded count = %d, want 3", noSup)
	}
	if liveOnly != 2 {
		t.Errorf("live-only count = %d, want 2 (got=%d, live1=%d newer=%d)", liveOnly, liveOnly, live1, newer)
	}
}

func TestFact_EffectiveConfidenceCurve(t *testing.T) {
	now := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		lastConf  time.Time
		base      float64
		want      float64
		tolerance float64
	}{
		{"fresh (within grace)", now.Add(-1 * 24 * time.Hour), 1.0, 1.0, 0.001},
		{"end of grace (30d)", now.Add(-30 * 24 * time.Hour), 1.0, 1.0, 0.001},
		{"midway (about half-decayed)", now.Add(-(30 + 167) * 24 * time.Hour), 1.0, 0.5, 0.01},
		{"just under full decay", now.Add(-364 * 24 * time.Hour), 1.0, 0.003, 0.01},
		{"past full decay", now.Add(-400 * 24 * time.Hour), 1.0, 0.0, 0.001},
		{"low base, fresh", now.Add(-1 * 24 * time.Hour), 0.5, 0.5, 0.001},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &semantic.Fact{Confidence: c.base, LastConfirmed: c.lastConf}
			got := f.EffectiveConfidence(now)
			diff := got - c.want
			if diff < 0 {
				diff = -diff
			}
			if diff > c.tolerance {
				t.Errorf("EffectiveConfidence = %v, want %v (±%v)", got, c.want, c.tolerance)
			}
		})
	}
}

func TestSearch_VectorOnly(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	_, _ = s.Insert(ctx, mkFact("about B", vecB()))
	_, _ = s.Insert(ctx, mkFact("about C", vecC()))
	idA, _ := s.Insert(ctx, mkFact("about A", vecA()))

	hits, err := s.Search(ctx, semantic.Query{Embedding: vecA(), K: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].Fact.ID != idA {
		t.Fatalf("top hit = %+v, want id=%d", hits[0], idA)
	}
}

func TestSearch_FTSOnly(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	_, _ = s.Insert(ctx, mkFact("user prefers Python", vecB()))
	idGo, _ := s.Insert(ctx, mkFact("user prefers golang for backend services", vecA()))

	hits, err := s.Search(ctx, semantic.Query{Keywords: "golang", K: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Fact.ID != idGo {
		t.Fatalf("hits = %+v, want one match on id=%d", hits, idGo)
	}
}

func TestSearch_DecayedFactsDropOut(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)

	// Fresh fact, embed exactly A.
	fresh := mkFact("fresh fact", vecA())
	fresh.LastConfirmed = now.Add(-1 * 24 * time.Hour)
	freshID, _ := s.Insert(ctx, fresh)

	// Ancient fact, embed exactly A (would match vec equally).
	old := mkFact("ancient fact", vecA())
	old.LastConfirmed = now.Add(-400 * 24 * time.Hour) // past full decay
	_, _ = s.Insert(ctx, old)

	hits, err := s.Search(ctx, semantic.Query{
		Embedding: vecA(), K: 10, Now: now,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Only the fresh fact should appear; decayed-to-zero drops out.
	if len(hits) != 1 || hits[0].Fact.ID != freshID {
		t.Fatalf("expected only fresh fact, got %+v", hits)
	}
}

func TestSearch_HigherConfidenceWinsAtSameRank(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	// Same vec, same age; differ only in base confidence.
	lo := mkFact("low conf", vecA())
	lo.Confidence = 0.3
	lo.LastConfirmed = now
	loID, _ := s.Insert(ctx, lo)

	hi := mkFact("high conf", vecA())
	hi.Confidence = 0.9
	hi.LastConfirmed = now
	hiID, _ := s.Insert(ctx, hi)

	hits, err := s.Search(ctx, semantic.Query{Embedding: vecA(), K: 2, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if hits[0].Fact.ID != hiID {
		t.Errorf("high-conf fact didn't rank first; got %d, want %d (lo=%d)", hits[0].Fact.ID, hiID, loID)
	}
}

func TestSearch_RequiresAtLeastOneSignal(t *testing.T) {
	s := mkStore(t)
	if _, err := s.Search(context.Background(), semantic.Query{K: 5}); err == nil {
		t.Fatal("expected error with neither Embedding nor Keywords")
	}
}

func TestSearch_FiltersByAgentID(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	for _, ag := range []string{"alice", "bob"} {
		f := mkFact("about A", vecA())
		f.AgentID = ag
		_, _ = s.Insert(ctx, f)
	}
	hits, err := s.Search(ctx, semantic.Query{
		AgentIDs: []string{"alice"}, Embedding: vecA(), K: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Fact.AgentID != "alice" {
		t.Fatalf("agent filter broken: %+v", hits)
	}
}

func TestSearch_FiltersByVisibility(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	for _, v := range []string{semantic.VisibilityPrivate, semantic.VisibilityShared} {
		f := mkFact("about A", vecA())
		f.Visibility = v
		_, _ = s.Insert(ctx, f)
	}
	hits, err := s.Search(ctx, semantic.Query{
		Visibility: []string{semantic.VisibilityShared}, Embedding: vecA(), K: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Fact.Visibility != semantic.VisibilityShared {
		t.Fatalf("visibility filter broken: %+v", hits)
	}
}

func TestSearch_FiltersByTimeWindow(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	for _, ts := range []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	} {
		f := mkFact("about A", vecA())
		f.FirstSeen = ts
		f.LastConfirmed = ts
		_, _ = s.Insert(ctx, f)
	}
	hits, err := s.Search(ctx, semantic.Query{
		Embedding: vecA(),
		After:     time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		Before:    time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		K:         10,
		Now:       time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("time filter broken: got %d hits", len(hits))
	}
}

func TestSearch_TopKCap(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		_, _ = s.Insert(ctx, mkFact("about A", vecA()))
	}
	hits, err := s.Search(ctx, semantic.Query{Embedding: vecA(), K: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 5 {
		t.Fatalf("got %d hits, want 5", len(hits))
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
