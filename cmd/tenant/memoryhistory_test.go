package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

func histCall(t *testing.T, d *memoryHistoryDispatcher, asOf, query string) (string, bool, error) {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"as_of": asOf, "query": query})
	return d.Dispatch(context.Background(), model.ToolCall{Name: "memory_history", Arguments: args})
}

func TestMemoryHistory_AsOfRecallsHistoricalState(t *testing.T) {
	ss, _ := openTempStores(t)
	ctx := context.Background()
	T := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	emb := []float32{0.1, 0.2, 0.3}
	oldID, _ := ss.Insert(ctx, &semantic.Fact{AgentID: "default", Visibility: semantic.VisibilityPrivate, Fact: "endpoint is mcp.atlassian.com", Confidence: 0.9, EmbedderID: "t", Embedding: emb})
	newID, _ := ss.Insert(ctx, &semantic.Fact{AgentID: "default", Visibility: semantic.VisibilityPrivate, Fact: "endpoint is cf.mcp.atlassian.com", Confidence: 0.9, EmbedderID: "t", Embedding: emb})
	// Transition on 2026-03-01.
	if err := ss.SetValidTo(ctx, oldID, T); err != nil {
		t.Fatal(err)
	}
	if err := ss.UpsertSignals(ctx, semantic.Signals{FactID: newID, Importance: 0.5, ValidFrom: T}); err != nil {
		t.Fatal(err)
	}
	_ = ss.Supersede(ctx, oldID, newID)

	d := newMemoryHistoryDispatcher("me", ss)

	// Before the switch: the OLD endpoint (superseded, but historically true).
	out, isErr, err := histCall(t, d, "2026-02-01", "endpoint")
	if err != nil || isErr {
		t.Fatalf("history errored: isErr=%v err=%v out=%q", isErr, err, out)
	}
	// "mcp.atlassian.com" is a substring of "cf.mcp.atlassian.com", so assert
	// on the distinguishing prefix to tell the two endpoints apart.
	if !strings.Contains(out, "endpoint is mcp.atlassian.com") || strings.Contains(out, "cf.mcp.atlassian.com") {
		t.Errorf("as-of 2026-02 should show old endpoint only, got:\n%s", out)
	}

	// After the switch: the NEW endpoint.
	out2, _, _ := histCall(t, d, "2026-04-01", "endpoint")
	if !strings.Contains(out2, "cf.mcp.atlassian.com") {
		t.Errorf("as-of 2026-04 should show new endpoint, got:\n%s", out2)
	}
}

func TestMemoryHistory_BadDateErrors(t *testing.T) {
	ss, _ := openTempStores(t)
	d := newMemoryHistoryDispatcher("me", ss)
	out, isErr, _ := d.Dispatch(context.Background(), model.ToolCall{Name: "memory_history", Arguments: json.RawMessage(`{"as_of":"last tuesday"}`)})
	if !isErr || !strings.Contains(out, "as_of is required") {
		t.Errorf("unparseable date should error clearly: isErr=%v out=%q", isErr, out)
	}
}

func TestParseAsOf_Layouts(t *testing.T) {
	for _, s := range []string{"2026-01-15", "2026-01-15T09:30:00Z", "2026-01-15 09:30", "2026/01/15", "01/15/2026"} {
		if _, ok := parseAsOf(s); !ok {
			t.Errorf("parseAsOf(%q) should succeed", s)
		}
	}
	if _, ok := parseAsOf("garbage"); ok {
		t.Error("parseAsOf(garbage) should fail")
	}
}
