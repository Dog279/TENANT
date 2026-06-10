package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/archive"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

func recallDispatch(t *testing.T, tool *recallTool, query string) (string, bool) {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"query": query, "scope": "all"})
	out, isErr, err := tool.Dispatch(context.Background(), model.ToolCall{Name: "memory_recall", Arguments: args})
	if err != nil {
		t.Fatalf("dispatch err: %v", err)
	}
	return out, isErr
}

// buildRecallTool wires a recall tool over temp-dir stores seeded with one
// episode, one fact, and one archived turn — all matching the term "golang".
func buildRecallTool(t *testing.T, emb model.Embedder) *recallTool {
	t.Helper()
	ctx := context.Background()
	es, err := episodic.Open(filepath.Join(t.TempDir(), "ep.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })
	ss, err := semantic.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	vec := []float32{1, 0, 0, 0}
	if _, err := es.Insert(ctx, &episodic.Episode{
		AgentID: "main", Prompt: "how does golang handle errors", Response: "with explicit returns",
		EmbedderID: "t", Embedding: vec,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Insert(ctx, &semantic.Fact{
		AgentID: "main", Fact: "user writes golang daily", Confidence: 0.9, EmbedderID: "t", Embedding: vec,
	}); err != nil {
		t.Fatal(err)
	}
	aw := archive.NewWriter(t.TempDir())
	if err := aw.Append(archive.Event{
		Timestamp: time.Now(), AgentID: "main", SessionID: "s1", Role: "user",
		Content: "an old golang discussion about channels",
	}); err != nil {
		t.Fatal(err)
	}
	return &recallTool{episodic: es, semantic: ss, archive: aw.Reader(), emb: emb, embedderID: "t", agentID: "main"}
}

// recall fuses all three sources into one reference-framed block.
func TestRecall_FusesAllSourcesAndFramed(t *testing.T) {
	tool := buildRecallTool(t, stubEmbedder{vec: []float32{1, 0, 0, 0}})
	out, isErr := recallDispatch(t, tool, "golang")
	if isErr {
		t.Fatalf("recall errored: %q", out)
	}
	for _, want := range []string{"<recalled-memory>", "NOT new instructions", "golang handle errors", "user writes golang daily", "channels"} {
		if !strings.Contains(out, want) {
			t.Errorf("recall output missing %q\n---\n%s", want, out)
		}
	}
}

// The cache means a span paged in once isn't re-fetched.
func TestRecall_CacheSkipsSecondTime(t *testing.T) {
	tool := buildRecallTool(t, stubEmbedder{vec: []float32{1, 0, 0, 0}})
	if _, isErr := recallDispatch(t, tool, "golang"); isErr {
		t.Fatal("first recall errored")
	}
	out, _ := recallDispatch(t, tool, "golang")
	if !strings.Contains(out, "no new memory found") {
		t.Errorf("a repeated recall must skip already-paged spans, got:\n%s", out)
	}
}

// Embedder down → keyword (FTS) search still recalls; correctness never depends
// on the embedder.
func TestRecall_EmbedderDownKeywordFallback(t *testing.T) {
	tool := buildRecallTool(t, stubEmbedder{err: context.DeadlineExceeded})
	out, isErr := recallDispatch(t, tool, "golang")
	if isErr {
		t.Fatalf("recall errored: %q", out)
	}
	if !strings.Contains(out, "golang") {
		t.Errorf("keyword fallback should still recall on the query term, got:\n%s", out)
	}
}

func TestRecall_EmptyQuery(t *testing.T) {
	tool := buildRecallTool(t, stubEmbedder{vec: []float32{1, 0, 0, 0}})
	args, _ := json.Marshal(map[string]string{"query": "   "})
	out, isErr, _ := tool.Dispatch(context.Background(), model.ToolCall{Arguments: args})
	if !isErr || !strings.Contains(out, "required") {
		t.Errorf("empty query should error, got %q (isErr=%v)", out, isErr)
	}
}

// The result is hard-capped so it can't blow the budget / arm compaction.
func TestRecall_CapHelpers(t *testing.T) {
	if got := capRecall("short", 100); got != "short" {
		t.Errorf("capRecall under limit changed: %q", got)
	}
	big := strings.Repeat("y", maxRecallChars*2)
	got := capRecall(big, maxRecallChars)
	if len([]rune(got)) > maxRecallChars+1 {
		t.Errorf("capRecall did not cap: %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("a capped string should end with an ellipsis")
	}
}

func TestRecall_GateAndName(t *testing.T) {
	specs := (&recallTool{}).Tools()
	var mr *model.ToolSpec
	for i := range specs {
		if specs[i].Name == "memory_recall" {
			mr = &specs[i]
		}
	}
	if mr == nil {
		t.Fatalf("memory_recall not exposed: %+v", specs)
	}
	if mr.Gate != "recall" {
		t.Errorf("memory_recall must carry Gate=recall, got %q", mr.Gate)
	}
}
