package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

func openTempStores(t *testing.T) (*semantic.Store, *episodic.Store) {
	t.Helper()
	dir := t.TempDir()
	ss, err := semantic.Open(filepath.Join(dir, "facts.db"))
	if err != nil {
		t.Fatalf("open semantic: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	es, err := episodic.Open(filepath.Join(dir, "episodes.db"))
	if err != nil {
		t.Fatalf("open episodic: %v", err)
	}
	t.Cleanup(func() { es.Close() })
	return ss, es
}

func memCall(t *testing.T, d *memorySearchDispatcher, q string) (string, bool, error) {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"query": q, "k": 8})
	return d.Dispatch(context.Background(), model.ToolCall{Name: "memory_search", Arguments: args})
}

func TestMemorySearch_FullVisibilityIncludesPrivate(t *testing.T) {
	ss, es := openTempStores(t)
	ctx := context.Background()
	// A PRIVATE fact + a private episode — the local tool must surface them
	// (it's the user's own machine), unlike the peer tool which hides private.
	emb := []float32{0.1, 0.2, 0.3} // dummy — search below is keyword-only
	if _, err := ss.Insert(ctx, &semantic.Fact{AgentID: "default", Visibility: semantic.VisibilityPrivate, Fact: "the deploy key rotates on fridays", Confidence: 0.9, EmbedderID: "test", Embedding: emb}); err != nil {
		t.Fatalf("insert fact: %v", err)
	}
	if _, err := es.Insert(ctx, &episodic.Episode{AgentID: "default", Visibility: episodic.VisibilityPrivate, Prompt: "how do I rotate the deploy key", Response: "run the rotate script", EmbedderID: "test", Embedding: emb}); err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	d := newMemorySearchDispatcher("me", ss, es, nil) // nil embedder ⇒ keyword-only
	out, isErr, err := memCall(t, d, "deploy key rotate")
	if err != nil || isErr {
		t.Fatalf("memory_search errored: isErr=%v err=%v out=%q", isErr, err, out)
	}
	if !strings.Contains(out, "deploy key rotates") {
		t.Errorf("private fact should be recalled locally: %q", out)
	}
	if !strings.Contains(out, "rotate the deploy key") {
		t.Errorf("private episode should be recalled locally: %q", out)
	}
}

func TestMemorySearch_EmptyAndBadArgs(t *testing.T) {
	ss, es := openTempStores(t)
	d := newMemorySearchDispatcher("me", ss, es, nil)

	// Empty query → isErr with a clear message.
	out, isErr, _ := d.Dispatch(context.Background(), model.ToolCall{Name: "memory_search", Arguments: json.RawMessage(`{"query":"  "}`)})
	if !isErr || !strings.Contains(out, "query is required") {
		t.Errorf("empty query should error clearly: isErr=%v out=%q", isErr, out)
	}

	// No matches → clean "(no results)" so federation skips it.
	out, isErr, _ = memCall(t, d, "nothing-matches-this-zzzzz")
	if isErr || out != "(no results)" {
		t.Errorf("no-match should be a clean empty signal: isErr=%v out=%q", isErr, out)
	}

	// Wrong tool name routed in → unknown.
	out, isErr, _ = d.Dispatch(context.Background(), model.ToolCall{Name: "something_else"})
	if !isErr || !strings.Contains(out, "unknown tool") {
		t.Errorf("foreign tool name should be rejected: isErr=%v out=%q", isErr, out)
	}
}
