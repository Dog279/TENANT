package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/archive"
	"tenant/internal/model"
)

// TestRecallByCallID covers TEN-170: recall(call_id) fetches the exact archived
// tool-result body by the id from a compaction elision marker.
func TestRecallByCallID(t *testing.T) {
	dir := t.TempDir()
	w := archive.NewWriter(dir)
	body := "THE BIG TOOL OUTPUT that compaction elided — line 1\nline 2\nline 3"
	if err := w.Append(archive.Event{
		Timestamp:  time.Now().UTC(),
		AgentID:    "main",
		SessionID:  "s1",
		Role:       "tool",
		ToolResult: &archive.ToolResult{CallID: "call-77", Content: body},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	rt := &recallTool{archive: w.Reader(), agentID: "main"}
	call := func(args string) (string, bool, error) {
		return rt.Dispatch(context.Background(), model.ToolCall{Name: "recall", Arguments: json.RawMessage(args)})
	}

	// Hit: returns the full body, reference-framed.
	out, isErr, err := call(`{"call_id":"call-77"}`)
	if err != nil || isErr {
		t.Fatalf("recall hit errored: isErr=%v err=%v", isErr, err)
	}
	if !strings.Contains(out, "THE BIG TOOL OUTPUT") || !strings.Contains(out, "line 3") {
		t.Fatalf("recalled body missing:\n%s", out)
	}
	if !strings.Contains(out, "<recalled-memory>") || !strings.Contains(out, "call-77") {
		t.Errorf("output not reference-framed / id missing:\n%s", out)
	}

	// Miss: unknown id → helpful, non-error message.
	out2, isErr2, _ := call(`{"call_id":"does-not-exist"}`)
	if isErr2 || !strings.Contains(out2, "no archived tool result") {
		t.Fatalf("miss handling: isErr=%v out=%q", isErr2, out2)
	}

	// Empty id → caller error.
	_, isErr3, _ := call(`{"call_id":""}`)
	if !isErr3 {
		t.Error("empty call_id should be a caller error")
	}
}

// TestRecallToolSpecs: both memory_recall (query) and recall (call_id) are
// exposed and gated.
func TestRecallToolSpecs(t *testing.T) {
	specs := (&recallTool{}).Tools()
	byName := map[string]model.ToolSpec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	for _, name := range []string{"memory_recall", "recall"} {
		s, ok := byName[name]
		if !ok {
			t.Fatalf("tool %q not exposed", name)
		}
		if s.Gate != "recall" {
			t.Errorf("tool %q gate = %q, want recall", name, s.Gate)
		}
	}
}
