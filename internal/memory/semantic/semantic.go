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

//go:embed signals_schema.sql
var signalsSchemaSQL string

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

// Decay model constants. Linear from grace to full decay; base
// EffectiveConfidence uses baseFullDecay (365d), unchanged from v1.
const (
	decayGrace    = 30 * 24 * time.Hour
	baseFullDecay = 365 * 24 * time.Hour
	// decayHorizonK scales the full-decay horizon by importance, anchored
	// at DefaultImportance (0.5 → ×1.0 → 365d, exactly v1). k=4: a 0.9
	// fact lasts ≈2.6yr, a 1.0 fact 3yr; a 0.1 fact ≈91d (floored). This
	// is the broad longevity guarantee (design §7, review finding 2):
	// importance — not just the ≤5 pins — buys years.
	decayHorizonK   = 4.0
	minDecayStretch = 0.25 // floor so even junk never decays in <~91d
)

// EffectiveConfidence returns the confidence after time-decay relative
// to now. Pure function so callers can rank consistently and tests can
// verify decay curves without faking time.
//
// Decay model: linear from grace period (30d) to full decay (365d).
//
//	age <  30d        → base
//	age >= 365d       → 0
//	in between        → base * (1 - (age - 30d) / (335d))
//
// This is the signal-free curve; EffectiveConfidenceWithSignals applies
// the importance-stretched horizon. Kept as-is so callers without signals
// (and existing tests) are unaffected.
func (f *Fact) EffectiveConfidence(now time.Time) float64 {
	return f.effectiveConfidenceWithHorizon(now, baseFullDecay)
}

// effectiveConfidenceWithHorizon is the shared decay curve with a tunable
// full-decay horizon. Grace is always 30d.
func (f *Fact) effectiveConfidenceWithHorizon(now time.Time, fullDecay time.Duration) float64 {
	if fullDecay <= decayGrace {
		fullDecay = decayGrace + time.Hour // degenerate guard; keep grace < fullDecay
	}
	age := now.Sub(f.LastConfirmed)
	switch {
	case age < decayGrace:
		return f.Confidence
	case age >= fullDecay:
		return 0
	default:
		decayRange := fullDecay - decayGrace
		decayProgress := float64(age-decayGrace) / float64(decayRange)
		return f.Confidence * (1.0 - decayProgress)
	}
}

// scaledFullDecay stretches (or shrinks) the full-decay horizon by
// importance, anchored so importance=DefaultImportance reproduces the
// original 365d exactly. Above neutral → longer life; below → shorter.
func scaledFullDecay(importance float64) time.Duration {
	stretch := 1 + decayHorizonK*(importance-DefaultImportance)
	if stretch < minDecayStretch {
		stretch = minDecayStretch
	}
	return time.Duration(float64(baseFullDecay) * stretch)
}

// EffectiveConfidenceWithSignals applies decay with importance-stretched
// longevity. Pinned facts never decay (hard core-block immunity). A fact
// with default signals (importance=0.5, not pinned) decays EXACTLY as
// EffectiveConfidence — the additive guarantee.
func (f *Fact) EffectiveConfidenceWithSignals(now time.Time, sig Signals) float64 {
	if sig.Pinned {
		return f.Confidence
	}
	return f.effectiveConfidenceWithHorizon(now, scaledFullDecay(sig.Importance))
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
		// The file-path DSN sets foreign_keys(ON) per-connection via a
		// _pragma query param; the bare :memory: DSN can't, so enable it
		// explicitly. Pinned to one connection above, so this one PRAGMA
		// applies for the store's whole life — keeping fact_signals'
		// ON DELETE CASCADE (and facts.superseded_by) enforced in tests
		// and the in-memory eval harness, matching disk behavior.
		if _, err := db.ExecContext(context.Background(), "PRAGMA foreign_keys = ON"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("semantic: enable foreign_keys: %w", err)
		}
	}
	if _, err := db.ExecContext(context.Background(), schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("semantic: init schema: %w", err)
	}
	// Phase 1 SME: the fact_signals side table. Separate IF-NOT-EXISTS
	// script so it lands additively on existing databases (and so the
	// hot facts schema above is never edited). Idempotent.
	if _, err := db.ExecContext(context.Background(), signalsSchemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("semantic: init signals schema: %w", err)
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
