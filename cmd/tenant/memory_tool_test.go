package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

type stubEmbedder struct {
	vec []float32
	err error
}

func (s stubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = s.vec
	}
	return out, nil
}

func remember(t *testing.T, tool memoryTool, fact string) (string, bool) {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"fact": fact})
	out, isErr, err := tool.Dispatch(context.Background(), model.ToolCall{Name: "memory_remember", Arguments: args})
	if err != nil {
		t.Fatalf("dispatch err: %v", err)
	}
	return out, isErr
}

func TestMemoryTool_RemembersRetrievableFact(t *testing.T) {
	ctx := context.Background()
	ss, err := semantic.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	tool := memoryTool{sem: ss, emb: stubEmbedder{vec: []float32{1, 0}}, embedderID: "t/2", agentID: "main"}

	out, isErr := remember(t, tool, "user prefers tabs over spaces")
	if isErr || !strings.Contains(out, "remembered") {
		t.Fatalf("remember failed: out=%q isErr=%v", out, isErr)
	}
	// It must be persisted as a high-confidence fact, retrievable by vector.
	hits, _ := ss.Search(ctx, semantic.Query{AgentIDs: []string{"main"}, Embedding: []float32{1, 0}, K: 5})
	if len(hits) != 1 || !strings.Contains(hits[0].Fact.Fact, "tabs over spaces") {
		t.Fatalf("fact not retrievable: %+v", hits)
	}
	if hits[0].Fact.Confidence < 0.9 {
		t.Fatalf("directive fact should be high-confidence, got %.2f", hits[0].Fact.Confidence)
	}
}

// A successful remember must also fire the note callback so the always-on
// profile updates immediately (not on the background cadence).
func TestMemoryTool_NotesProfileImmediately(t *testing.T) {
	ss, err := semantic.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	var noted string
	tool := memoryTool{
		sem: ss, emb: stubEmbedder{vec: []float32{1, 0}}, embedderID: "t/2", agentID: "main",
		note: func(f string) { noted = f },
	}
	if _, isErr := remember(t, tool, "user prefers dark mode"); isErr {
		t.Fatal("remember should succeed")
	}
	if noted != "user prefers dark mode" {
		t.Fatalf("note callback not fired with the fact, got %q", noted)
	}
}

// A FAILED remember (embedder down) must NOT note the profile.
func TestMemoryTool_NoNoteOnFailure(t *testing.T) {
	ss, _ := semantic.Open(filepath.Join(t.TempDir(), "f.db"))
	defer ss.Close()
	noteCalled := false
	tool := memoryTool{
		sem: ss, emb: stubEmbedder{err: errors.New("down")}, embedderID: "t/2", agentID: "main",
		note: func(string) { noteCalled = true },
	}
	remember(t, tool, "x")
	if noteCalled {
		t.Fatal("note must not fire when the write fails")
	}
}

func TestMemoryTool_EmbedderDownFailsCleanly(t *testing.T) {
	ss, err := semantic.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	tool := memoryTool{sem: ss, emb: stubEmbedder{err: errors.New("endpoint down")}, embedderID: "t/2", agentID: "main"}

	out, isErr := remember(t, tool, "user lives in Greenland")
	if !isErr || !strings.Contains(out, "embedder is unavailable") {
		t.Fatalf("embedder-down should fail cleanly: out=%q isErr=%v", out, isErr)
	}
	if n, _ := ss.Count(context.Background(), false, false); n != 0 {
		t.Fatalf("nothing should be persisted when embedding fails, got %d", n)
	}
}

func TestMemoryTool_EmptyFactRejected(t *testing.T) {
	tool := memoryTool{sem: nil, emb: stubEmbedder{vec: []float32{1}}, embedderID: "t/2", agentID: "main"}
	out, isErr := remember(t, tool, "   ")
	if !isErr || !strings.Contains(out, "required") {
		t.Fatalf("empty fact should be rejected: out=%q", out)
	}
}
