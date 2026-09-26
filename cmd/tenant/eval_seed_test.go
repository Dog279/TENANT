package main

import (
	"context"
	"path/filepath"
	"testing"

	"tenant/internal/eval"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
)

// An eval task's injected facts land in its memory marked as made by an eval
// run, so the provenance inspector never mistakes them for learned facts
// (TEN-297).
func TestSeedTaskMemoryMarksEvalFacts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ss, err := semantic.Open(filepath.Join(dir, "facts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	es, err := episodic.Open(filepath.Join(dir, "eps.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })

	task := &eval.Task{
		InjectedFacts:    []eval.InjectedFact{{Text: "User's cat is called Miso"}},
		InjectedEpisodes: []eval.InjectedEpisode{{Prompt: "my cat?", Response: "Miso"}},
	}
	if err := seedTaskMemory(ctx, es, ss, stubEmbedder{vec: []float32{1, 0}}, "t", "main", task); err != nil {
		t.Fatal(err)
	}
	facts, err := ss.List(ctx, semantic.ListFilter{AgentIDs: []string{"main"}})
	if err != nil || len(facts) != 1 {
		t.Fatalf("seeded facts = %v (%v)", facts, err)
	}
	if facts[0].Confidence != 1.0 {
		t.Fatalf("a fact without a confidence seeded at %.2f, want 1.0", facts[0].Confidence)
	}
	if o, ok, err := ss.Origin(ctx, facts[0].ID); err != nil || !ok || o.Job != "eval" {
		t.Fatalf("seeded fact origin = %+v %v %v, want eval", o, ok, err)
	}
	if n, err := es.Count(ctx, false); err != nil || n != 1 {
		t.Fatalf("seeded episodes = %d (%v), want 1", n, err)
	}
}
