package improve

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"tenant/internal/memory/semantic"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
)

// --- pure helpers ---

func TestClusterFacts_GroupsSimilarSeparatesDistinct(t *testing.T) {
	mk := func(id int64, v []float32) *semantic.Fact { return &semantic.Fact{ID: id, Embedding: v} }
	facts := []*semantic.Fact{
		mk(1, []float32{1, 0, 0}),
		mk(2, []float32{0.99, 0.1, 0}),  // ~same as 1
		mk(3, []float32{0, 1, 0}),       // distinct
		mk(4, []float32{0.98, 0.05, 0}), // ~same as 1
	}
	var multi, singles int
	for _, c := range clusterFacts(facts, 0.9, 8) {
		if len(c) > 1 {
			multi++
		} else {
			singles++
		}
	}
	if multi != 1 || singles != 1 {
		t.Fatalf("got multi=%d singles=%d, want 1 multi + 1 single", multi, singles)
	}
}

func TestClusterFacts_RespectsMaxSize(t *testing.T) {
	var facts []*semantic.Fact
	for i := 0; i < 5; i++ {
		facts = append(facts, &semantic.Fact{ID: int64(i), Embedding: []float32{1, 0}})
	}
	clusters := clusterFacts(facts, 0.9, 2)
	for _, c := range clusters {
		if len(c) > 2 {
			t.Fatalf("cluster of %d exceeds maxSize 2", len(c))
		}
	}
}

func TestUnionSourcesAndMaxConfidence(t *testing.T) {
	cluster := []*semantic.Fact{
		{SourceEpisodes: []int64{1, 2}, Confidence: 0.7},
		{SourceEpisodes: []int64{2, 3}, Confidence: 0.9},
	}
	if got := unionSources(cluster); len(got) != 3 {
		t.Fatalf("unionSources = %v, want 3 unique", got)
	}
	if mc := maxConfidence(cluster); mc != 0.9 {
		t.Fatalf("maxConfidence = %v, want 0.9", mc)
	}
}

func TestFirstJSONObject(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                         `{"a":1}`,
		"prefix {\"a\":{\"b\":2}} tail":   `{"a":{"b":2}}`,
		"```json\n{\"merge\":false}\n```": `{"merge":false}`,
		"no json here":                    "no json here",
	}
	for in, want := range cases {
		if got := firstJSONObject(in); got != want {
			t.Errorf("firstJSONObject(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- integration: Run merges a duplicate cluster ---

func consolidateScaffold(t *testing.T) (*semantic.Store, *model.Router, *testllm.Fake, *testllm.Fake) {
	t.Helper()
	ss, err := semantic.Open(filepath.Join(t.TempDir(), "fact.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	router := model.NewRouter(reg, slog.Default())
	fakeLLM, fakeEmb := testllm.New(), testllm.New()
	router.RegisterBackend("vllm", func(_ context.Context, p model.Profile, _ *slog.Logger) (any, error) {
		if p.Role == model.RoleEmbedder {
			return fakeEmb, nil
		}
		return fakeLLM, nil
	})
	return ss, router, fakeLLM, fakeEmb
}

func TestConsolidationJob_MergesDuplicateCluster(t *testing.T) {
	ctx := context.Background()
	ss, router, fakeLLM, fakeEmb := consolidateScaffold(t)

	seed := func(text string, v []float32) {
		if _, err := ss.Insert(ctx, &semantic.Fact{
			AgentID: "main", Fact: text, EmbedderID: "e", Embedding: v, Confidence: 0.8,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 3 paraphrases of one claim + 1 distinct fact.
	seed("Tenant is a Go MCP framework", []float32{1, 0, 0, 0})
	seed("Tenant: an MCP framework written in Go", []float32{0.99, 0.08, 0, 0})
	seed("Go-based project Tenant implements MCP", []float32{0.98, 0.05, 0.02, 0})
	seed("User develops on Windows", []float32{0, 1, 0, 0})

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

	if before, _ := ss.Count(ctx, false, false); before != 4 {
		t.Fatalf("seeded live=%d, want 4", before)
	}

	job := &ConsolidationJob{Semantic: ss, Router: router, AgentID: "main", ClusterThreshold: 0.9}
	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 3 dups → 1 merged (3 superseded, 1 inserted); distinct fact untouched.
	if after, _ := ss.Count(ctx, false, false); after != 2 {
		t.Fatalf("live after consolidate = %d, want 2; summary=%q", after, res.Summary)
	}
	if !res.Changed {
		t.Fatalf("expected Changed=true; summary=%q", res.Summary)
	}

	// Idempotent: a second run finds nothing left to merge.
	res2, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res2.Changed {
		t.Fatalf("second run mutated: %q", res2.Summary)
	}
}

func TestConsolidationJob_DryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	ss, router, fakeLLM, _ := consolidateScaffold(t)
	for _, tc := range []struct {
		text string
		v    []float32
	}{{"A claim", []float32{1, 0, 0}}, {"A claim reworded", []float32{0.99, 0.05, 0}}} {
		if _, err := ss.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: tc.text, EmbedderID: "e", Embedding: tc.v, Confidence: 0.8}); err != nil {
			t.Fatal(err)
		}
	}
	fakeLLM.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: `{"merge":true,"fact":"A merged claim"}`, FinishReason: "stop"}, nil
	}
	job := &ConsolidationJob{Semantic: ss, Router: router, AgentID: "main", ClusterThreshold: 0.9, DryRun: true}
	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if after, _ := ss.Count(ctx, false, false); after != 2 {
		t.Fatalf("dry-run mutated store: live=%d, want 2", after)
	}
	if res.Changed {
		t.Fatalf("dry-run should report Changed=false; summary=%q", res.Summary)
	}
}

func TestConsolidationJob_DistinctClusterNotMerged(t *testing.T) {
	ctx := context.Background()
	ss, router, fakeLLM, _ := consolidateScaffold(t)
	// Two facts close enough to cluster but the summarizer judges them distinct.
	_, _ = ss.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: "User likes Go", EmbedderID: "e", Embedding: []float32{1, 0, 0}, Confidence: 0.8})
	_, _ = ss.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: "User likes Rust", EmbedderID: "e", Embedding: []float32{0.99, 0.05, 0}, Confidence: 0.8})
	fakeLLM.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: `{"merge":false}`, FinishReason: "stop"}, nil
	}
	job := &ConsolidationJob{Semantic: ss, Router: router, AgentID: "main", ClusterThreshold: 0.9}
	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if after, _ := ss.Count(ctx, false, false); after != 2 {
		t.Fatalf("distinct facts merged anyway: live=%d, want 2", after)
	}
	if res.Changed {
		t.Fatalf("nothing should change; summary=%q", res.Summary)
	}
}

func TestConsolidationJob_HolisticGroupsByMeaning(t *testing.T) {
	ctx := context.Background()
	ss, router, fakeLLM, fakeEmb := consolidateScaffold(t)
	seed := func(text string, v []float32) {
		if _, err := ss.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: text, EmbedderID: "e", Embedding: v, Confidence: 0.8}); err != nil {
			t.Fatal(err)
		}
	}
	// Orthogonal embeddings: cosine clustering would NEVER group 1 & 2.
	seed("Tenant is a Go MCP framework", []float32{1, 0, 0})           // 1
	seed("Tenant: an MCP framework written in Go", []float32{0, 1, 0}) // 2 (same claim, far vector)
	seed("User develops on Windows", []float32{0, 0, 1})               // 3 (distinct)

	fakeLLM.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: `{"groups":[{"members":[1,2],"fact":"Tenant is a Go MCP framework"}]}`, FinishReason: "stop"}, nil
	}
	fakeEmb.EmbedFn = func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{0.5, 0.5, 0}
		}
		return out, nil
	}

	job := &ConsolidationJob{Semantic: ss, Router: router, AgentID: "main", Holistic: true}
	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 1+2 merged (cosine never would), 3 untouched → live = 3 - 2 + 1 = 2.
	if after, _ := ss.Count(ctx, false, false); after != 2 {
		t.Fatalf("holistic live=%d, want 2; summary=%q", after, res.Summary)
	}
	if !res.Changed {
		t.Fatalf("expected Changed=true; summary=%q", res.Summary)
	}
}
