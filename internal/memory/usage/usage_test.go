package usage_test

import (
	"context"
	"path/filepath"
	"testing"

	"tenant/internal/memory/usage"
)

func mkStore(t *testing.T) *usage.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := usage.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRecordPersists(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	if err := s.Record(ctx, "main", "claude-x", 100, 40); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.Record(ctx, "main", "claude-x", 250, 80); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var n, sumIn, sumOut int
	row := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0) FROM token_usage`)
	if err := row.Scan(&n, &sumIn, &sumOut); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 2 || sumIn != 350 || sumOut != 120 {
		t.Fatalf("got n=%d in=%d out=%d; want 2/350/120", n, sumIn, sumOut)
	}

	// ts must be a non-empty RFC3339 string for the cost-audit trail.
	var ts, agentID, model string
	row = s.DB().QueryRowContext(ctx,
		`SELECT ts, agent_id, model FROM token_usage ORDER BY id LIMIT 1`)
	if err := row.Scan(&ts, &agentID, &model); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if ts == "" || agentID != "main" || model != "claude-x" {
		t.Fatalf("got ts=%q agent=%q model=%q", ts, agentID, model)
	}
}

// A nil *Store must be a safe no-op so a failed Open never panics callers.
func TestNilStoreSafe(t *testing.T) {
	var s *usage.Store
	if err := s.Record(context.Background(), "a", "m", 1, 2); err != nil {
		t.Fatalf("nil Record: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}
