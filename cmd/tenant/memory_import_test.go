package main

import (
	"context"
	"strings"
	"testing"

	"tenant/internal/memory/mdimport"
	"tenant/internal/memory/semantic"
)

// fakeImportEmbedder yields fixed-dimension non-zero vectors (deterministic,
// offline) — mirrors fakeReembedEmbedder in memory_reembed_test.go.
type fakeImportEmbedder struct{ dim int }

func (f fakeImportEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, f.dim)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}

// sampleMemoryMD is a MEMORY.md-style doc: a heading + index intro prose, a
// bulleted index (nested bullets), a code fence, a horizontal rule, and blank
// lines. Exactly 5 importable claims (the prose line + 4 bullets); everything
// else is scaffolding the importer must skip.
const sampleMemoryMD = "# Memory Index\n" +
	"\n" +
	"Dylan is the owner of the Tenant project.\n" +
	"\n" +
	"## Topics\n" +
	"\n" +
	"- User prefers **Go** over Python.\n" +
	"- Project deadline is 2026-06-15.\n" +
	"  - The build must stay green on Windows.\n" +
	"- See the [docs](https://example.com/docs) for context.\n" +
	"\n" +
	"```go\n" +
	"// skip me\n" +
	"fmt.Println(\"not a claim\")\n" +
	"```\n" +
	"\n" +
	"---\n"

func newImportTestStore(t *testing.T) *semantic.Store {
	t.Helper()
	ss, err := semantic.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	return ss
}

func TestImportClaims_InsertsFactsAndSkipsScaffolding(t *testing.T) {
	ctx := context.Background()
	ss := newImportTestStore(t)
	emb := fakeImportEmbedder{dim: 8}

	claims := mdimport.Parse(sampleMemoryMD)
	if len(claims) != 5 {
		t.Fatalf("parser extracted %d claims, want 5 (1 prose + 4 bullets): %+v", len(claims), claims)
	}

	res, err := importClaims(ctx, emb, ss, "main", claims, importParams{
		EmbedderID: "embedder", Importance: 0.7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Parsed != 5 || res.Inserted != 5 || res.SkippedDup != 0 {
		t.Fatalf("res = %+v, want parsed=5 inserted=5 skipped=0", res)
	}

	facts, err := ss.List(ctx, semantic.ListFilter{AgentIDs: []string{"main"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 5 {
		t.Fatalf("stored %d facts, want 5", len(facts))
	}
	for _, f := range facts {
		if len(f.Embedding) != 8 || f.EmbedderID != "embedder" {
			t.Errorf("fact %q: dim=%d id=%q (want 8/embedder)", f.Fact, len(f.Embedding), f.EmbedderID)
		}
		// Markdown syntax must be stripped in the stored claim text.
		for _, bad := range []string{"**", "[docs]", "(https"} {
			if strings.Contains(f.Fact, bad) {
				t.Errorf("stored fact leaked markdown %q: %q", bad, f.Fact)
			}
		}
	}
}

func TestImportClaims_SetsImportanceAndProtected(t *testing.T) {
	ctx := context.Background()
	ss := newImportTestStore(t)
	emb := fakeImportEmbedder{dim: 8}

	claims := mdimport.Parse("- a durable user correction the model must respect")
	res, err := importClaims(ctx, emb, ss, "main", claims, importParams{
		EmbedderID: "embedder", Importance: 0.95, Protected: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Inserted != 1 {
		t.Fatalf("inserted %d, want 1", res.Inserted)
	}

	facts, _ := ss.List(ctx, semantic.ListFilter{AgentIDs: []string{"main"}})
	if len(facts) != 1 {
		t.Fatalf("stored %d facts, want 1", len(facts))
	}
	sig, err := ss.GetSignals(ctx, facts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if sig.Importance != 0.95 {
		t.Errorf("importance = %v, want 0.95", sig.Importance)
	}
	if !sig.Protected {
		t.Error("protected flag not set on imported fact")
	}
	if sig.Pinned {
		t.Error("pinned should not be set (import never pins)")
	}
}

func TestImportClaims_Idempotent(t *testing.T) {
	ctx := context.Background()
	ss := newImportTestStore(t)
	emb := fakeImportEmbedder{dim: 8}
	claims := mdimport.Parse(sampleMemoryMD)

	first, err := importClaims(ctx, emb, ss, "main", claims, importParams{EmbedderID: "embedder", Importance: 0.7})
	if err != nil {
		t.Fatal(err)
	}
	if first.Inserted != 5 {
		t.Fatalf("first run inserted %d, want 5", first.Inserted)
	}

	// Second run over the SAME parsed claims: every one already exists →
	// 0 inserted, all skipped as duplicate, store unchanged.
	second, err := importClaims(ctx, emb, ss, "main", claims, importParams{EmbedderID: "embedder", Importance: 0.7})
	if err != nil {
		t.Fatal(err)
	}
	if second.Inserted != 0 || second.SkippedDup != 5 {
		t.Fatalf("second run = inserted %d skipped %d, want 0/5 (idempotent)", second.Inserted, second.SkippedDup)
	}
	count, _ := ss.Count(ctx, false, false)
	if count != 5 {
		t.Errorf("live fact count = %d after re-import, want 5", count)
	}
}

func TestImportClaims_DryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	ss := newImportTestStore(t)
	emb := fakeImportEmbedder{dim: 8}
	claims := mdimport.Parse(sampleMemoryMD)

	res, err := importClaims(ctx, emb, ss, "main", claims, importParams{
		EmbedderID: "embedder", Importance: 0.7, DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.WouldImport != 5 || res.Inserted != 0 {
		t.Fatalf("dry-run res = %+v, want wouldImport=5 inserted=0", res)
	}
	if len(res.Preview) == 0 {
		t.Error("dry-run should populate a preview")
	}
	count, _ := ss.Count(ctx, false, false)
	if count != 0 {
		t.Errorf("dry-run wrote %d facts, want 0", count)
	}
}
