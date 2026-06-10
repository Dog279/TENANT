package main

import (
	"context"
	"testing"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
)

// fakeReembedEmbedder produces vectors of a fixed dimension (non-zero), to
// simulate switching to a different embed model.
type fakeReembedEmbedder struct{ dim int }

func (f fakeReembedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, f.dim)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}

func TestReembed_RecoversAfterDimChange(t *testing.T) {
	ctx := context.Background()
	es, err := episodic.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	ss, err := semantic.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()

	// Seed one episode + one fact at the OLD 4d dimension.
	if _, err := es.Insert(ctx, &episodic.Episode{
		AgentID: "main", Prompt: "what do I prefer", Response: "Go",
		EmbedderID: "old", Embedding: []float32{1, 2, 3, 4},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Insert(ctx, &semantic.Fact{
		AgentID: "main", Fact: "user prefers Go", Confidence: 1,
		EmbedderID: "old", Embedding: []float32{1, 2, 3, 4},
	}); err != nil {
		t.Fatal(err)
	}

	emb := fakeReembedEmbedder{dim: 8}

	// Re-embed to the new 8d dimension.
	eps, _ := es.List(ctx, episodic.ListFilter{IncludeTombstoned: true})
	epDone, epSkip, err := reembedEpisodes(ctx, emb, es, eps, 8, "new")
	if err != nil || epDone != 1 || epSkip != 0 {
		t.Fatalf("episodes: done=%d skip=%d err=%v (want 1,0)", epDone, epSkip, err)
	}
	fcts, _ := ss.List(ctx, semantic.ListFilter{})
	fDone, fSkip, err := reembedFacts(ctx, emb, ss, fcts, 8, "new")
	if err != nil || fDone != 1 || fSkip != 0 {
		t.Fatalf("facts: done=%d skip=%d err=%v (want 1,0)", fDone, fSkip, err)
	}

	// Stored vectors are now 8d with the new embedder id.
	eps2, _ := es.List(ctx, episodic.ListFilter{IncludeTombstoned: true})
	if len(eps2[0].Embedding) != 8 || eps2[0].EmbedderID != "new" {
		t.Errorf("episode not re-embedded: dim=%d id=%q", len(eps2[0].Embedding), eps2[0].EmbedderID)
	}
	fcts2, _ := ss.List(ctx, semantic.ListFilter{})
	if len(fcts2[0].Embedding) != 8 || fcts2[0].EmbedderID != "new" {
		t.Errorf("fact not re-embedded: dim=%d id=%q", len(fcts2[0].Embedding), fcts2[0].EmbedderID)
	}

	// Idempotent: a second pass skips everything (already at the live dim).
	epDone2, epSkip2, _ := reembedEpisodes(ctx, emb, es, eps2, 8, "new")
	if epDone2 != 0 || epSkip2 != 1 {
		t.Errorf("second pass should skip: done=%d skip=%d", epDone2, epSkip2)
	}
}
