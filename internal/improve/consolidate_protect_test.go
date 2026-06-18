package improve

import (
	"context"
	"testing"

	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

// A pinned fact in a mergeable cluster must be EXCLUDED from consolidation
// candidacy — the others merge, but the protected fact survives intact
// (design §7: the holistic/cosine merge can no longer eat load-bearing facts).
func TestConsolidationJob_SkipsProtectedFacts(t *testing.T) {
	ctx := context.Background()
	ss, router, fakeLLM, fakeEmb := consolidateScaffold(t)

	seed := func(text string, v []float32) int64 {
		id, err := ss.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: text, EmbedderID: "e", Embedding: v, Confidence: 0.8})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// 3 paraphrases of one claim (would all merge into one).
	idA := seed("Tenant is a Go MCP framework", []float32{1, 0, 0, 0})
	seed("Tenant: an MCP framework written in Go", []float32{0.99, 0.08, 0, 0})
	seed("Go-based project Tenant implements MCP", []float32{0.98, 0.05, 0.02, 0})

	// Pin A → merge-protected.
	if err := ss.UpsertSignals(ctx, semantic.Signals{FactID: idA, Pinned: true, Importance: 0.5}); err != nil {
		t.Fatal(err)
	}

	fakeLLM.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: `{"merge":true,"fact":"Tenant is a Go MCP framework"}`, FinishReason: "stop"}, nil
	}
	fakeEmb.EmbedFn = func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{0.97, 0.06, 0.01, 0}
		}
		return out, nil
	}

	job := &ConsolidationJob{Semantic: ss, Router: router, AgentID: "main", ClusterThreshold: 0.9}
	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// B + C merged into 1; A excluded and untouched ⇒ A + merged = 2 live.
	if after, _ := ss.Count(ctx, false, false); after != 2 {
		t.Fatalf("live after = %d, want 2 (A protected + 1 merged); summary=%q", after, res.Summary)
	}
	a, err := ss.Get(ctx, idA)
	if err != nil {
		t.Fatalf("Get A: %v", err)
	}
	if a.SupersededBy != 0 || a.Tombstoned {
		t.Errorf("protected fact A was merged away: superseded_by=%d tombstoned=%v", a.SupersededBy, a.Tombstoned)
	}
	if got := res.Details["protected_excluded"]; got != 1 {
		t.Errorf("protected_excluded = %v, want 1", got)
	}
}

// When the WHOLE store is protected, nothing is left to merge — and the
// job reports it cleanly rather than erroring.
func TestConsolidationJob_AllProtectedNoMerge(t *testing.T) {
	ctx := context.Background()
	ss, router, _, _ := consolidateScaffold(t)
	for _, tc := range []struct {
		text string
		v    []float32
	}{{"A", []float32{1, 0, 0}}, {"A reworded", []float32{0.99, 0.05, 0}}} {
		id, err := ss.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: tc.text, EmbedderID: "e", Embedding: tc.v, Confidence: 0.8})
		if err != nil {
			t.Fatal(err)
		}
		if err := ss.SetPinned(ctx, id, true); err != nil {
			t.Fatal(err)
		}
	}
	job := &ConsolidationJob{Semantic: ss, Router: router, AgentID: "main", ClusterThreshold: 0.9}
	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Changed {
		t.Errorf("all-protected run should not change anything; summary=%q", res.Summary)
	}
	if after, _ := ss.Count(ctx, false, false); after != 2 {
		t.Errorf("live = %d, want 2 (both protected, untouched)", after)
	}
}
