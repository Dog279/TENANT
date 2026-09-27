package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Meta is key's value in the meta table, "" when it has none. The intake
// (internal/logs) keeps its progress there, so a restored database resumes
// from what it holds.
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.r.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: meta: %w", err)
	}
	return v, nil
}

// SetMeta sets key's value in the meta table; "" deletes it.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	q, args := `INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, []any{key, value}
	if value == "" {
		q, args = `DELETE FROM meta WHERE key = ?`, args[:1]
	}
	if _, err := s.w.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("store: meta: %w", err)
	}
	return nil
}

// Stamps calls fn with the time and message of every event of log in
// [from, to), so an import can skip what the store already holds.
func (s *Store) Stamps(ctx context.Context, log string, from, to time.Time, fn func(at time.Time, msg string)) error {
	rows, err := s.r.QueryContext(ctx, `SELECT e.time_ns, e.msg FROM events e
		WHERE e.log_id = (SELECT id FROM names WHERE kind = 'log' AND name = ?) AND e.time_ns >= ? AND e.time_ns < ?`,
		log, from.UnixNano(), to.UnixNano())
	if err != nil {
		return fmt.Errorf("store: stamps: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ns int64
		var msg string
		if err := rows.Scan(&ns, &msg); err != nil {
			return fmt.Errorf("store: stamps: %w", err)
		}
		fn(time.Unix(0, ns).UTC(), msg)
	}
	return rows.Err()
}
