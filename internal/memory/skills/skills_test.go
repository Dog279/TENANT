package skills

import (
	"context"
	"testing"
)

func mk(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUpsertGetListCount(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	id, err := s.Upsert(ctx, &Skill{AgentID: "a", Name: "deploy", Description: "ship the binary", Recipe: "go build then scp", Embedding: []float32{1, 0, 0}})
	if err != nil || id == 0 {
		t.Fatalf("upsert: id=%d err=%v", id, err)
	}
	// Upsert same name updates, not duplicates.
	if _, err := s.Upsert(ctx, &Skill{AgentID: "a", Name: "deploy", Description: "ship it v2", Recipe: "r2", Embedding: []float32{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count(ctx, "a"); n != 1 {
		t.Fatalf("count = %d, want 1 (upsert should not duplicate)", n)
	}
	got, err := s.Get(ctx, id)
	if err != nil || got.Description != "ship it v2" {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	list, _ := s.List(ctx, ListFilter{AgentID: "a"})
	if len(list) != 1 {
		t.Fatalf("list = %d, want 1", len(list))
	}
}

func TestSearchRanksByCosine(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	_, _ = s.Upsert(ctx, &Skill{AgentID: "a", Name: "x", Description: "x", Recipe: "r", Embedding: []float32{1, 0, 0}})
	_, _ = s.Upsert(ctx, &Skill{AgentID: "a", Name: "y", Description: "y", Recipe: "r", Embedding: []float32{0, 1, 0}})
	hits, err := s.Search(ctx, Query{AgentID: "a", Embedding: []float32{0.9, 0.1, 0}, K: 2})
	if err != nil || len(hits) != 2 {
		t.Fatalf("search: %d hits err=%v", len(hits), err)
	}
	if hits[0].Skill.Name != "x" {
		t.Fatalf("expected x ranked first (closest vector), got %s", hits[0].Skill.Name)
	}
}

func TestEnableDisableHidesFromSearch(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	_, _ = s.Upsert(ctx, &Skill{AgentID: "a", Name: "k", Description: "k", Recipe: "r", Embedding: []float32{1, 0}})
	if hits, _ := s.Search(ctx, Query{AgentID: "a", Embedding: []float32{1, 0}, K: 5}); len(hits) != 1 {
		t.Fatal("enabled skill should be searchable")
	}
	ok, err := s.SetEnabledByName(ctx, "a", "k", false)
	if err != nil || !ok {
		t.Fatalf("disable: ok=%v err=%v", ok, err)
	}
	if hits, _ := s.Search(ctx, Query{AgentID: "a", Embedding: []float32{1, 0}, K: 5}); len(hits) != 0 {
		t.Fatal("disabled skill must not be searchable")
	}
}

func TestProposedAcceptAndTombstone(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	id, _ := s.Upsert(ctx, &Skill{AgentID: "a", Name: "p", Description: "p", Recipe: "r", Status: StatusProposed, Embedding: []float32{1}})
	// Proposed (disabled) is not in live Search.
	if hits, _ := s.Search(ctx, Query{AgentID: "a", Embedding: []float32{1}, K: 5}); len(hits) != 0 {
		t.Fatal("proposed skill should not be retrievable until accepted")
	}
	if err := s.Accept(ctx, id); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.Search(ctx, Query{AgentID: "a", Embedding: []float32{1}, K: 5}); len(hits) != 1 {
		t.Fatal("accepted skill should be live + searchable")
	}
	if err := s.Tombstone(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count(ctx, "a"); n != 0 {
		t.Fatalf("tombstoned skill should not count as live: %d", n)
	}
}
