package usage_test

import (
	"context"
	"database/sql"
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
	if err := s.Record(ctx, "main", "claude-x", 100, 40, "trace-1"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.Record(ctx, "main", "claude-x", 250, 80, ""); err != nil {
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

	// ts must be a non-empty RFC3339 string for the cost-audit trail, and the
	// row names the turn that spent the tokens (TEN-368).
	var ts, agentID, model, trace string
	row = s.DB().QueryRowContext(ctx,
		`SELECT ts, agent_id, model, trace_id FROM token_usage ORDER BY id LIMIT 1`)
	if err := row.Scan(&ts, &agentID, &model, &trace); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if ts == "" || agentID != "main" || model != "claude-x" || trace != "trace-1" {
		t.Fatalf("got ts=%q agent=%q model=%q trace=%q", ts, agentID, model, trace)
	}
}

// A ledger from before the trace column gains it on open, keeping its rows.
func TestOpenAddsTraceToAnOldLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE token_usage (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL,
		agent_id TEXT NOT NULL, model TEXT NOT NULL, tokens_in INTEGER NOT NULL, tokens_out INTEGER NOT NULL);
		INSERT INTO token_usage (ts, agent_id, model, tokens_in, tokens_out) VALUES ('t', 'main', 'm', 1, 2);`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := usage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Record(context.Background(), "main", "m", 3, 4, "trace-2"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM token_usage WHERE trace_id = 'trace-2' OR trace_id IS NULL`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows = %d (err %v), want the old row and the traced one", n, err)
	}
}

// A nil *Store must be a safe no-op so a failed Open never panics callers.
func TestNilStoreSafe(t *testing.T) {
	var s *usage.Store
	if err := s.Record(context.Background(), "a", "m", 1, 2, ""); err != nil {
		t.Fatalf("nil Record: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}
