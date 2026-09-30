package runtrace

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
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
	r.ThinkMS, r.ToolsMS = 900, 300
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
	if got.ThinkMS != 900 || got.ToolsMS != 300 {
		t.Fatalf("think/tools = %d/%d ms, want 900/300", got.ThinkMS, got.ToolsMS)
	}
}

// A turn's span sums its model calls and its tool batches (TEN-342), and Apply
// hands the totals to the run record.
func TestSpanPhases(t *testing.T) {
	s := NewSpan("t")
	s.AddThink(2 * time.Second)
	s.AddThink(500 * time.Millisecond)
	s.AddToolTime(time.Second)
	if think, tools := s.Phases(); think != 2500*time.Millisecond || tools != time.Second {
		t.Fatalf("phases = %v, %v", think, tools)
	}
	var r Run
	s.Apply(&r)
	if r.ThinkMS != 2500 || r.ToolsMS != 1000 {
		t.Fatalf("applied = %d/%d ms", r.ThinkMS, r.ToolsMS)
	}
	var nilSpan *Span
	nilSpan.AddThink(time.Second) // a turn without a span records nothing
	if think, tools := nilSpan.Phases(); think != 0 || tools != 0 {
		t.Fatal("nil span has phases")
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
	for _, key := range []string{"trace_id", "source", "started_at", "latency_ms", "think_ms", "tools_ms", "prompt_tokens", "completion_tokens", "retries", "fallbacks", "tool_calls", "approvals", "outcome", "error_class"} {
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

// A sub-agent's run names the turn that spawned it, and a runs.db from before
// the column existed gains it on open (TEN-368).
func TestParentTraceSurvivesAndMigrates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runs.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(schema, "CREATE TABLE IF NOT EXISTS runs", "CREATE TABLE runs", 1)
	if _, err := old.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO runs (trace_id,agent_id,session_id,source,started_at) VALUES ('before','main','','tui','2026-07-17T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if r, err := s.Get(ctx, "before"); err != nil || r.ParentTraceID != "" || r.ThinkMS != 0 || r.ToolsMS != 0 {
		t.Fatalf("legacy row = %+v, err = %v", r, err)
	}
	if err := s.Start(ctx, Run{TraceID: "child", ParentTraceID: "before", Source: "headless"}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Get(ctx, "child"); err != nil || r.ParentTraceID != "before" {
		t.Fatalf("child = %+v, err = %v; want parent_trace_id before", r, err)
	}
}

// An open store prunes on its timer, not only when it opens, and the timer
// stops with the store.
func TestOpenStorePrunesOnATimer(t *testing.T) {
	saved := pruneEvery
	pruneEvery = 10 * time.Millisecond
	defer func() { pruneEvery = saved }()
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "runs.db"), Config{MaxCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := s.Start(ctx, Run{TraceID: id, Source: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		runs, err := s.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runs = %d after the prune timer, want 1", len(runs))
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// _txlock=immediate: a transaction holds the write lock from BEGIN, so another
// connection's write is refused (busy) rather than interleaved.
func TestTransactionsTakeTheWriteLockAtBegin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Exec(`INSERT INTO runs (trace_id,source,started_at) VALUES ('x','test','2026-07-17T12:00:00Z')`); err == nil {
		t.Fatal("a write went through while an open transaction should hold the write lock")
	}
}
