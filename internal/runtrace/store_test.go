package runtrace

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRecordLifecycle(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:", Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	started := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	r := Run{TraceID: "trace-1", AgentID: "main", SessionID: "session", Source: "tui", StartedAt: started}
	if err := s.Start(ctx, r); err != nil {
		t.Fatal(err)
	}
	running, err := s.Get(ctx, r.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if running.Outcome != "running" || running.CompletedAt != nil {
		t.Fatalf("running record = %+v", running)
	}
	completed := started.Add(1250 * time.Millisecond)
	r.CompletedAt, r.LatencyMS, r.Outcome = &completed, 1250, "success"
	r.Model, r.Provider = "gpt-test", "openai"
	cost := 0.0123
	r.CostEstimateUSD = &cost
	r.PromptTokens, r.CompletionTokens, r.Retries = 11, 7, 1
	r.Fallbacks = []Fallback{{From: "primary", To: "backup", Reason: "rate limited"}}
	r.ToolCalls = []ToolCall{{ID: "tc-1", Name: "search", Arguments: `{"q":"x"}`, Result: "ok", Outcome: "success"}}
	r.Approvals = []Approval{{ID: "ap-1", RunID: r.TraceID, Action: "send", Outcome: "approved"}}
	if err := s.Finish(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, r.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "success" || got.Model != "gpt-test" || got.PromptTokens != 11 || got.Retries != 1 || got.CostEstimateUSD == nil || *got.CostEstimateUSD != cost {
		t.Fatalf("completed record = %+v", got)
	}
	if len(got.ToolCalls) != 1 || len(got.Approvals) != 1 || len(got.Fallbacks) != 1 {
		t.Fatalf("detail arrays = %+v", got)
	}
}

func TestExportJSONLShape(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:", Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	started := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	r := Run{TraceID: "trace-export", Source: "dashboard", StartedAt: started}
	if err := s.Start(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.Outcome = "error"
	r.ErrorClass = "deadline_exceeded"
	r.CompletedAt = &started
	if err := s.Finish(ctx, r); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.ExportJSONL(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1; lines != 1 {
		t.Fatalf("lines=%d: %q", lines, buf.String())
	}
	var shape map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &shape); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"trace_id", "source", "started_at", "latency_ms", "prompt_tokens", "completion_tokens", "retries", "fallbacks", "tool_calls", "approvals", "outcome", "error_class"} {
		if _, ok := shape[key]; !ok {
			t.Errorf("missing export key %q", key)
		}
	}
}

func TestRetentionPrunesByAgeAndCount(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	s, err := Open(":memory:", Config{MaxAge: 24 * time.Hour, MaxCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i, at := range []time.Time{now.Add(-48 * time.Hour), now.Add(-3 * time.Hour), now.Add(-2 * time.Hour), now.Add(-time.Hour)} {
		r := Run{TraceID: string(rune('a' + i)), Source: "test", StartedAt: at}
		if err := s.Start(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := s.Prune(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed=%d want 2", removed)
	}
	runs, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].TraceID != "c" || runs[1].TraceID != "d" {
		t.Fatalf("survivors=%+v", runs)
	}
}

func TestRedactionDropsToolBodies(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:", Config{RedactBodies: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	r := Run{TraceID: "redacted", Source: "relay", StartedAt: now, CompletedAt: &now, Outcome: "success",
		ToolCalls: []ToolCall{{Name: "secret", Arguments: `{"token":"secret"}`, Result: "private body", Outcome: "success"}}}
	if err := s.Start(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, r.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ToolCalls[0].Arguments != "" || got.ToolCalls[0].Result != "" {
		t.Fatalf("tool bodies retained: %+v", got.ToolCalls[0])
	}
	if got.ToolCalls[0].Name != "secret" || got.ToolCalls[0].Outcome != "success" {
		t.Fatalf("tool metadata lost: %+v", got.ToolCalls[0])
	}
}
