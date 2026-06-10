// Package usage persists per-LLM-call token consumption for long-term
// cost auditing. One row per call: who (agent_id), which model, how many
// input/output tokens, and when. Kept deliberately tiny and decoupled
// from the T2 episodic store — this is an append-only ledger, not a
// retrieval index.
//
// Storage: SQLite via modernc.org/sqlite (pure Go, no CGO — preserves the
// single-binary deploy story). Mirrors the episodic store's Open/Close
// shape so the wiring layer treats it the same way.
package usage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// schema is the single ledger table. Append-only; no updates or deletes.
const schema = `CREATE TABLE IF NOT EXISTS token_usage (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	ts         TEXT    NOT NULL,
	agent_id   TEXT    NOT NULL,
	model      TEXT    NOT NULL,
	tokens_in  INTEGER NOT NULL,
	tokens_out INTEGER NOT NULL
);`

// Store wraps the SQLite connection. Safe for concurrent use — SQLite
// handles reader/writer arbitration via WAL.
type Store struct {
	db *sql.DB
}

// Open returns a Store backed by the SQLite database at path. Uses WAL
// mode for crash safety + concurrent readers, matching the episodic
// store. Path can be ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)", path)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("usage: open: %w", err)
	}
	// Single conn for in-memory DBs so writes are visible to later reads.
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage: init schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Record appends one LLM call's token usage. The timestamp is set to the
// current time in RFC3339. Append-only single INSERT.
func (s *Store) Record(ctx context.Context, agentID, model string, in, out int) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO token_usage (ts, agent_id, model, tokens_in, tokens_out) VALUES (?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), agentID, model, in, out)
	if err != nil {
		return fmt.Errorf("usage: record: %w", err)
	}
	return nil
}

// DB exposes the underlying *sql.DB for cost-audit queries (e.g. SUM by
// model/day). Use sparingly — most callers only Record.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the underlying database. Safe to call multiple times.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}
