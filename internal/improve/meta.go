// Package improve is Tenant's self-improvement layer. It is the
// "Crons + closed learning loop" idea from the Hermes agent framework,
// rebuilt Go-native: self-improvement is a scheduled background
// activity, not a per-turn tax. A Scheduler runs registered Jobs on
// cadences; distillation (T2 → T3) is the first job. Soul-nudge and
// skill-induction jobs slot in later behind the same Job interface.
//
// Design lineage (Hermes "Five Pillars": Memory, Skills, Soul, Crons,
// Self-Improvement): we keep Crons (Scheduler) and the learning loop
// (Jobs) but stay in Go and keep every job a small, testable unit.
package improve

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// metaSchema is the tiny key-value table the scheduler uses for
// cursors (e.g. last-distilled episode ID per agent) and any future
// scheduler state. Shares a SQLite file with the memory tiers — the
// cursor is about episodes, so co-locating is natural.
const metaSchema = `
CREATE TABLE IF NOT EXISTS tenant_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);`

// Meta is a minimal persistent key-value store. Not a general cache —
// it holds scheduler cursors and similar small durable state. Values
// are strings; callers encode/decode (cursors are just int64 as text).
type Meta struct {
	db *sql.DB
}

// OpenMeta opens (or creates) the meta table at path. ":memory:" is
// supported for tests. Safe to point at the same file as the episodic
// store — CREATE TABLE IF NOT EXISTS won't disturb sibling tables.
func OpenMeta(path string) (*Meta, error) {
	dsn := path
	if path != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)", path)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("improve: open meta: %w", err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if _, err := db.ExecContext(context.Background(), metaSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("improve: init meta schema: %w", err)
	}
	return &Meta{db: db}, nil
}

// Close closes the meta database. Idempotent.
func (m *Meta) Close() error {
	if m.db == nil {
		return nil
	}
	err := m.db.Close()
	m.db = nil
	return err
}

// Get returns the value for key. The bool is false if the key is
// absent (distinct from an empty-string value).
func (m *Meta) Get(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := m.db.QueryRowContext(ctx, `SELECT value FROM tenant_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("improve: meta get %q: %w", key, err)
	}
	return v, true, nil
}

// Set upserts key=value. updated_at is recorded for diagnostics.
func (m *Meta) Set(ctx context.Context, key, value string) error {
	_, err := m.db.ExecContext(ctx, `
        INSERT INTO tenant_meta (key, value, updated_at)
        VALUES (?, ?, unixepoch())
        ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
    `, key, value)
	if err != nil {
		return fmt.Errorf("improve: meta set %q: %w", key, err)
	}
	return nil
}

// GetInt64 is a convenience over Get for the common cursor case.
// Returns 0, false when the key is absent.
func (m *Meta) GetInt64(ctx context.Context, key string) (int64, bool, error) {
	s, ok, err := m.Get(ctx, key)
	if err != nil || !ok {
		return 0, ok, err
	}
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		return 0, true, fmt.Errorf("improve: meta %q not an int64 (%q): %w", key, s, err)
	}
	return n, true, nil
}

// SetInt64 stores n as text under key.
func (m *Meta) SetInt64(ctx context.Context, key string, n int64) error {
	return m.Set(ctx, key, fmt.Sprintf("%d", n))
}
