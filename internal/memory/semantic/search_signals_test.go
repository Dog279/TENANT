package semantic_test

import (
	"context"
	"testing"
	"time"

	"tenant/internal/memory/semantic"
)

// A neutral signals row must not change a fact's search score vs. having
// no signals row at all — the additive guarantee at the Search level.
func TestSearch_NeutralSignalsDoNotChangeScore(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, _ := s.Insert(ctx, mkFact("user prefers Go", vecA()))

	before, err := s.Search(ctx, semantic.Query{AgentIDs: []string{"main"}, Embedding: vecA(), K: 10, Now: now})
	if err != nil || len(before) != 1 {
		t.Fatalf("search before: %v hits=%d", err, len(before))
	}
	if err := s.UpsertSignals(ctx, semantic.Signals{FactID: id, Importance: semantic.DefaultImportance}); err != nil {
		t.Fatalf("upsert neutral: %v", err)
	}
	after, err := s.Search(ctx, semantic.Query{AgentIDs: []string{"main"}, Embedding: vecA(), K: 10, Now: now})
	if err != nil || len(after) != 1 {
		t.Fatalf("search after: %v hits=%d", err, len(after))
	}
	if before[0].Score != after[0].Score {
		t.Errorf("neutral signals changed score: %v → %v", before[0].Score, after[0].Score)
	}
}

// A clearly-more-relevant fact must outrank a less-relevant one even when
// the latter is maximally important + pinned — the MODULATE-never-reorder
// invariant the ranking comment documents.
func TestSearch_RelevanceDominatesOverImportance(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	relevant, _ := s.Insert(ctx, mkFact("on topic", vecA()))
	other, _ := s.Insert(ctx, mkFact("off topic", vecB()))

	// Make the off-topic fact maximally important + pinned.
	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: other, Importance: 1.0, Pinned: true})
	// And the relevant fact low importance.
	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: relevant, Importance: 0.1})

	hits, err := s.Search(ctx, semantic.Query{AgentIDs: []string{"main"}, Embedding: vecA(), K: 10, Now: now})
	if err != nil || len(hits) == 0 {
		t.Fatalf("search: %v hits=%d", err, len(hits))
	}
	if hits[0].Fact.ID != relevant {
		t.Errorf("relevance should dominate: top hit = %d, want %d", hits[0].Fact.ID, relevant)
	}
}

// Among equally-relevant but AGED facts, the higher-importance one ranks
// higher (stretched decay horizon + quality modulation), where it
// previously would have tied / been arbitrary.
func TestSearch_ImportanceRanksAgedFactsHigher(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mk := func(claim string) int64 {
		f := mkFact(claim, vecA()) // identical embedding ⇒ identical relevance
		f.LastConfirmed = now.Add(-200 * day)
		f.FirstSeen = now.Add(-200 * day)
		id, err := s.Insert(ctx, f)
		if err != nil {
			t.Fatalf("insert %q: %v", claim, err)
		}
		return id
	}
	high := mk("load-bearing decision")
	low := mk("incidental detail")
	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: high, Importance: 0.9, ConfirmCount: 1})
	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: low, Importance: 0.5, ConfirmCount: 1})

	hits, err := s.Search(ctx, semantic.Query{AgentIDs: []string{"main"}, Embedding: vecA(), K: 10, Now: now})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if hits[0].Fact.ID != high {
		t.Errorf("aged high-importance fact should rank first: got %d want %d (scores %v / %v)",
			hits[0].Fact.ID, high, hits[0].Score, hits[1].Score)
	}
}
