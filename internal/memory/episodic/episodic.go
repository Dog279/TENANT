// Package episodic implements T2 of Tenant's memory architecture: every
// turn-pair (prompt + response + tool calls + outcome) stored with an
// embedding for vector retrieval and indexed in FTS5 for keyword search.
//
// Storage: SQLite via modernc.org/sqlite (pure Go, no CGO — keeps the
// single-binary deploy story intact). Vectors live as little-endian
// float32 BLOBs; cosine similarity runs brute-force in Go. This works
// well up to ~100K episodes on personal hardware; past that point swap
// to sqlite-vec (CGO) or the pgvector upgrade path described in
// docs/MEMORY-DESIGN.md.
package episodic

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"tenant/internal/fsperm"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Visibility values for the visibility column. Constants here so
// callers don't drift on string values.
const (
	VisibilityPrivate = "private"
	VisibilityShared  = "shared"
	VisibilityPublic  = "public"
)

// Outcome values for the outcome column.
const (
	OutcomeSuccess = "success"
	OutcomeError   = "error"
	OutcomeUnknown = "unknown"
)

// Episode is one turn-pair as stored.
type Episode struct {
	ID           int64
	AgentID      string
	Visibility   string
	SessionID    string
	SourceRunID  string
	Timestamp    time.Time
	Prompt       string
	Response     string
	ToolCalls    []ToolCallRef
	Outcome      string
	UserFeedback string
	Tags         []string
	EmbedderID   string
	Embedding    []float32
	Tombstoned   bool
}

// ToolCallRef is the stable archive-side shape of a tool call. Decoupled
// from model.ToolCall so internal LLM-interface refactors don't ripple
// into the episodic store schema.
type ToolCallRef struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON string
}

// ErrNotFound is returned by Get when no episode matches the ID.
var ErrNotFound = errors.New("episodic: episode not found")

// Store wraps the SQLite connection. Safe for concurrent use — SQLite
// handles reader/writer arbitration via WAL.
type Store struct {
	db *sql.DB
}

// Open returns a Store backed by the SQLite database at path. Uses WAL
// mode for crash safety + concurrent readers. Path can be ":memory:"
// for tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		if err := fsperm.PrepareFile(path); err != nil {
			return nil, fmt.Errorf("episodic: prepare private database: %w", err)
		}
		// WAL + relaxed sync for write throughput on personal hardware.
		// busy_timeout handles transient lock contention without errors.
		dsn = fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("episodic: open: %w", err)
	}
	// Single conn for in-memory DBs so writes are visible to subsequent reads.
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if _, err := db.ExecContext(context.Background(), schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("episodic: init schema: %w", err)
	}
	// CREATE TABLE IF NOT EXISTS does not add columns to existing databases.
	// Keep the additive run-lineage migration local and idempotent.
	if err := ensureColumn(db, "episodes", "source_run_id", "TEXT"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("episodic: migrate source_run_id: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_episodes_source_run ON episodes(source_run_id)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("episodic: index source_run_id: %w", err)
	}
	return &Store{db: db}, nil
}

func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition)
	return err
}

// Close closes the underlying database. Safe to call multiple times.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// DB exposes the underlying *sql.DB. Use sparingly — most callers
// should use the typed methods. Useful for joins from sibling tables
// (T3 facts, T4 procedural) that share the same database file.
func (s *Store) DB() *sql.DB { return s.db }
