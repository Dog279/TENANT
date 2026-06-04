// Package semantic implements T3 of Tenant's memory architecture: the
// distilled-facts layer. Where T2 (episodic) stores every turn-pair,
// T3 stores only the durable claims that emerged: "user prefers Go
// over Python", "project deadline is 2026-06-15", "this codebase uses
// pgvector". Facts are denser, more reliable, and ranked higher than
// raw episodes during prompt assembly.
//
// Storage shares the same SQLite layout as episodic (pure-Go,
// brute-force cosine, FTS5 mirror). The semantic store may share a
// physical database file with episodic but maintains its own tables.
//
// Two non-obvious behaviors:
//
//   - Supersede chain. When a new fact contradicts an old one, the
//     distill job calls Supersede(oldID, newID). The old row stays
//     for audit (with superseded_by set); Search filters it out.
//
//   - Confidence decay. Facts grow stale. effectiveConfidence at
//     retrieval time applies a linear decay over (last_confirmed →
//     now). 30-day grace, full decay at 1 year. Reaffirm() resets
//     the clock when the distill job sees an episode that re-validates
//     the fact.
package semantic

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

const (
	VisibilityPrivate = "private"
	VisibilityShared  = "shared"
	VisibilityPublic  = "public"
)

// Fact is one distilled atomic claim.
type Fact struct {
	ID             int64
	AgentID        string
	Visibility     string
	Fact           string  // one-sentence atomic claim
	SourceEpisodes []int64 // provenance: which episodes produced this fact
	Confidence     float64 // 0..1 base confidence
	FirstSeen      time.Time
	LastConfirmed  time.Time
	SupersededBy   int64 // 0 = current, non-zero = superseded by this fact ID
	EmbedderID     string
	Embedding      []float32
	Tombstoned     bool
}

// EffectiveConfidence returns the confidence after time-decay relative
// to now. Pure function so callers can rank consistently and tests can
// verify decay curves without faking time.
//
// Decay model: linear from grace period (30d) to full decay (365d).
//   age <  30d        → base
//   age >= 365d       → 0
//   in between        → base * (1 - (age - 30d) / (335d))
func (f *Fact) EffectiveConfidence(now time.Time) float64 {
	const (
		grace     = 30 * 24 * time.Hour
		fullDecay = 365 * 24 * time.Hour
	)
	age := now.Sub(f.LastConfirmed)
	switch {
	case age < grace:
		return f.Confidence
	case age >= fullDecay:
		return 0
	default:
		decayRange := fullDecay - grace
		decayProgress := float64(age-grace) / float64(decayRange)
		return f.Confidence * (1.0 - decayProgress)
	}
}

// ErrNotFound is returned by Get / Supersede / Reaffirm when no row matches.
var ErrNotFound = errors.New("semantic: fact not found")

// Store wraps the SQLite connection for the facts tier.
type Store struct {
	db *sql.DB
}

// Open returns a Store backed by SQLite at path. Use ":memory:" for tests.
// Sibling tables (e.g. episodic in the same file) are not disturbed —
// schema only creates IF NOT EXISTS.
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("semantic: open: %w", err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if _, err := db.ExecContext(context.Background(), schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("semantic: init schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database. Idempotent.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// DB exposes the underlying *sql.DB for callers that need raw access
// (e.g. joins with sibling tier tables).
func (s *Store) DB() *sql.DB { return s.db }
