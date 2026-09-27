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
	b := Boot{ID: id}
	var started, beat int64
	err := s.r.QueryRowContext(ctx, `SELECT coalesce(b.pid, 0), coalesce(r.name, ''), coalesce(b.version, ''),
		coalesce(b.host, ''), coalesce(b.started_ns, 0), coalesce(b.heartbeat_ns, 0)
		FROM boots b LEFT JOIN names r ON r.id = b.role WHERE b.boot_id = ?`, idBytes(id)).
		Scan(&b.PID, &b.Role, &b.Version, &b.Host, &started, &beat)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	if err != nil {
		return b, fmt.Errorf("store: boots: %w", err)
	}
	b.Started, b.Heartbeat = time.Unix(0, started).UTC(), time.Unix(0, beat).UTC()
	return b, nil
}
