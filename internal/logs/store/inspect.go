package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// What doctor reads of the processes sharing the store (TEN-375).

// Lease is who holds the named lease: the holder's boot ID, "" when no
// process ever took it, and when it runs out.
func (s *Store) Lease(ctx context.Context, name string) (holder string, expires time.Time, err error) {
	var id any
	var ns int64
	err = s.r.QueryRowContext(ctx, `SELECT boot_id, coalesce(expires_ns, 0) FROM leases WHERE name = ?`, name).Scan(&id, &ns)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("store: lease: %w", err)
	}
	return idText(id), time.Unix(0, ns).UTC(), nil
}

// Boot returns the boots row of the process id, or ErrNotFound.
func (s *Store) Boot(ctx context.Context, id string) (Boot, error) {
	b, err := scanBoot(s.r.QueryRowContext(ctx, bootSelect+` WHERE b.boot_id = ?`, idBytes(id)))
	b.ID = id
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	if err != nil {
		return b, fmt.Errorf("store: boots: %w", err)
	}
	return b, nil
}
