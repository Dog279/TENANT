package assemble

import (
	"testing"

	"tenant/internal/memory/semantic"
)

func TestDedupeFacts_DropsNearDuplicatesKeepsFirst(t *testing.T) {
	mk := func(text string, v []float32) *semantic.Fact { return &semantic.Fact{Fact: text, Embedding: v} }
	in := []*semantic.Fact{
		mk("Tenant is a Go MCP framework", []float32{1, 0, 0}),
		mk("Tenant: MCP framework in Go", []float32{0.99, 0.06, 0}), // near-dup of #1
		mk("User runs Windows", []float32{0, 1, 0}),                 // distinct
		mk("Go-based Tenant, MCP", []float32{0.98, 0.04, 0.01}),     // near-dup of #1
	}
	out := dedupeFacts(in, 0.90)
	if len(out) != 2 {
		t.Fatalf("kept %d, want 2 (one canonical + one distinct)", len(out))
	}
	// Highest-ranked (first) occurrence is the one kept.
	if out[0].Fact != "Tenant is a Go MCP framework" || out[1].Fact != "User runs Windows" {
		t.Fatalf("kept wrong facts: %q, %q", out[0].Fact, out[1].Fact)
	}
}

func TestDedupeFacts_KeepsAllDistinct(t *testing.T) {
	mk := func(v []float32) *semantic.Fact { return &semantic.Fact{Embedding: v} }
	in := []*semantic.Fact{mk([]float32{1, 0}), mk([]float32{0, 1}), mk([]float32{0.5, 0.5})}
	if got := dedupeFacts(in, 0.90); len(got) != 3 {
		t.Fatalf("kept %d, want 3 distinct", len(got))
	}
}

func TestDedupeFacts_MissingEmbeddingNeverDropped(t *testing.T) {
	in := []*semantic.Fact{
		{Fact: "a", Embedding: []float32{1, 0}},
		{Fact: "b"},                             // no embedding → can't be judged a dup, must survive
		{Fact: "c", Embedding: []float32{1, 0}}, // dup of a
	}
	out := dedupeFacts(in, 0.90)
	if len(out) != 2 {
		t.Fatalf("kept %d, want 2 (a + b; c is a dup of a)", len(out))
	}
}
