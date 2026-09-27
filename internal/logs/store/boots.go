package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Boot is one long-running process's row in the boots table (docs/design/
// logging-v2.md §12.2). The process holds its boot lock file for as long as
// it runs, and marks the row clean when it stops in order; a row with neither
// a clean stop nor a report whose lock can be taken belongs to a process that
// died. The lock alone decides: the heartbeat only estimates when it died.
type Boot struct {
	ID                  string // runtrace.BootID
	PID                 int
	Role, Version, Host string
	Started, Heartbeat  time.Time
}

// BeginBoot writes b's row. Take the process's boot lock first, so no other
// process can read the row as a dead one's.
func (s *Store) BeginBoot(ctx context.Context, b Boot) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: boot: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO names (kind, name) VALUES ('role', ?)`, b.Role); err != nil {
		return fmt.Errorf("store: boot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO boots (boot_id, pid, role, version, host, started_ns, heartbeat_ns)
		VALUES (?, ?, (SELECT id FROM names WHERE kind = 'role' AND name = ?), ?, ?, ?, ?)`,
		idBytes(b.ID), b.PID, b.Role, b.Version, b.Host, b.Started.UnixNano(), b.Started.UnixNano()); err != nil {
		return fmt.Errorf("store: boot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: boot: %w", err)
	}
	return nil
}

// Heartbeat records that the process id was alive at at.
func (s *Store) Heartbeat(ctx context.Context, id string, at time.Time) error {
	if _, err := s.w.ExecContext(ctx, `UPDATE boots SET heartbeat_ns = ? WHERE boot_id = ?`, at.UnixNano(), idBytes(id)); err != nil {
		return fmt.Errorf("store: heartbeat: %w", err)
	}
	return nil
}

// EndBoot marks the process id stopped in order at at.
func (s *Store) EndBoot(ctx context.Context, id string, at time.Time) error {
	if _, err := s.w.ExecContext(ctx, `UPDATE boots SET stopped_ns = ?, heartbeat_ns = ?, clean = 1 WHERE boot_id = ?`,
		at.UnixNano(), at.UnixNano(), idBytes(id)); err != nil {
		return fmt.Errorf("store: boot: %w", err)
	}
	return nil
}

// Unfinished lists the processes, other than self, whose rows have neither a
// clean stop nor a report: each is running, or died.
func (s *Store) Unfinished(ctx context.Context, self string) ([]Boot, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT b.boot_id, coalesce(b.pid, 0), coalesce(r.name, ''), coalesce(b.version, ''),
		coalesce(b.host, ''), coalesce(b.started_ns, 0), coalesce(b.heartbeat_ns, 0)
		FROM boots b LEFT JOIN names r ON r.id = b.role
		WHERE coalesce(b.clean, 0) = 0 AND coalesce(b.reported, 0) = 0 AND b.boot_id != ?`, idBytes(self))
	if err != nil {
		return nil, fmt.Errorf("store: boots: %w", err)
	}
	defer rows.Close()
	var out []Boot
	for rows.Next() {
		var b Boot
		var id any
		var started, beat int64
		if err := rows.Scan(&id, &b.PID, &b.Role, &b.Version, &b.Host, &started, &beat); err != nil {
			return nil, fmt.Errorf("store: boots: %w", err)
		}
		b.ID, b.Started, b.Heartbeat = idText(id), time.Unix(0, started).UTC(), time.Unix(0, beat).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarkReported flags id's row reported, and says whether this call did: of
// processes checking at once, only one files the report.
func (s *Store) MarkReported(ctx context.Context, id string) (bool, error) {
	res, err := s.w.ExecContext(ctx, `UPDATE boots SET reported = 1
		WHERE boot_id = ? AND coalesce(clean, 0) = 0 AND coalesce(reported, 0) = 0`, idBytes(id))
	if err != nil {
		return false, fmt.Errorf("store: boots: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// LastEvent is the time of the process id's last event at or after since, or
// since when it has none. Reading from since (its last heartbeat) keeps the
// scan to its last half minute or so.
func (s *Store) LastEvent(ctx context.Context, id string, since time.Time) (time.Time, error) {
	var last sql.NullInt64
	if err := s.r.QueryRowContext(ctx, `SELECT max(time_ns) FROM events WHERE time_ns >= ? AND boot_id = ?`,
		since.UnixNano(), idValue(id)).Scan(&last); err != nil {
		return since, fmt.Errorf("store: boots: %w", err)
	}
	if !last.Valid {
		return since, nil
	}
	return time.Unix(0, last.Int64).UTC(), nil
}
