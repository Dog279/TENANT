package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// What the API and tenant logs administer (docs/design/logging-v2.md §9.4,
// §9.5): the newest cursor, clearing a log, the timed debug switch and saved
// views.

// Latest is the cursor of the newest event: Seq 0 when the store is empty.
func (s *Store) Latest(ctx context.Context) (Cursor, error) {
	c := Cursor{StoreID: s.id}
	var seq int64
	if err := s.r.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) FROM events`).Scan(&seq); err != nil {
		return c, fmt.Errorf("store: latest: %w", err)
	}
	c.Seq = uint64(seq)
	return c, nil
}

// ErrProtected is Clear's error for the Security log, which is never
// cleared.
var ErrProtected = errors.New("store: the Security log can't be cleared")

// Clear deletes every event of log, in retention's short transactions, with
// its rollups, and notes when it was cleared. It returns how many events it
// deleted. Events committed while it runs may survive it.
//
// ponytail: the event-ID rollups are taken down before the deletes, so one
// committed in between is deleted but still counted there; do both in each
// delete's transaction if the Summary's top IDs ever need to be exact.
func (s *Store) Clear(ctx context.Context, log string) (int64, error) {
	if log == SecurityLog {
		return 0, ErrProtected
	}
	var id int64
	switch err := s.r.QueryRowContext(ctx, `SELECT id FROM names WHERE kind = 'log' AND name = ?`, log).Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("store: clear: %w", err)
	}
	if _, err := s.w.ExecContext(ctx, `UPDATE rollup_id SET n = rollup_id.n - c.k FROM
		(SELECT time_ns / 3600000000000 AS hour, provider_id, event_id, count(*) AS k FROM events
		 WHERE log_id = ? AND level <= 3 GROUP BY 1, 2, 3) AS c
		WHERE rollup_id.hour = c.hour AND rollup_id.provider_id = c.provider_id AND rollup_id.event_id = c.event_id`, id); err != nil {
		return 0, fmt.Errorf("store: clear: %w", err)
	}
	rep := Report{Deleted: map[string]int64{}}
	r := &retention{s: s, rep: &rep, names: map[int64]string{id: log}}
	if _, err := r.sweep(ctx, "", -1, "log_id = ?", id); err != nil {
		return rep.Deleted[log], err
	}
	_, err := s.w.ExecContext(ctx, `DELETE FROM rollup WHERE log_id = ?`, id)
	if err == nil {
		_, err = s.w.ExecContext(ctx, `DELETE FROM rollup_hour WHERE log_id = ?`, id)
	}
	if err == nil {
		_, err = s.w.ExecContext(ctx, `DELETE FROM rollup_id WHERE n <= 0`)
	}
	if err == nil {
		_, err = s.w.ExecContext(ctx, `UPDATE logs SET bytes = 0, cleared_ns = ? WHERE log_id = ?`, time.Now().UnixNano(), id)
	}
	if err != nil {
		return rep.Deleted[log], fmt.Errorf("store: clear: %w", err)
	}
	return rep.Deleted[log], nil
}

// SetDebug turns log's timed debug switch on until until, or off with the
// zero time. While it is on, the log's verbose events are written whatever
// the level floor (§6.2). Every process reads it at its next housekeeper
// tick.
func (s *Store) SetDebug(ctx context.Context, log string, until time.Time) error {
	var ns any // NULL: off
	if !until.IsZero() {
		ns = until.UnixNano()
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: set debug: %w", err)
	}
	defer tx.Rollback()
	var id int64
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO names (kind, name) VALUES ('log', ?)`, log); err == nil {
		err = tx.QueryRowContext(ctx, `SELECT id FROM names WHERE kind = 'log' AND name = ?`, log).Scan(&id)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO logs (log_id, debug_until_ns) VALUES (?, ?)
			ON CONFLICT (log_id) DO UPDATE SET debug_until_ns = excluded.debug_until_ns`, id, ns)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("store: set debug: %w", err)
	}
	return nil
}

// Debugging lists the logs whose debug switch is on now, with when each goes
// off.
func (s *Store) Debugging(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT n.name, l.debug_until_ns FROM logs l JOIN names n ON n.id = l.log_id
		WHERE l.debug_until_ns > ?`, time.Now().UnixNano())
	if err != nil {
		return nil, fmt.Errorf("store: debugging: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var ns int64
		if err := rows.Scan(&name, &ns); err != nil {
			return nil, fmt.Errorf("store: debugging: %w", err)
		}
		out[name] = time.Unix(0, ns).UTC()
	}
	return out, rows.Err()
}

// View is a saved filter: a custom view.
type View struct {
	Name, Filter string
	Created      time.Time
}

// Views lists the saved views, by name.
func (s *Store) Views(ctx context.Context) ([]View, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT name, filter, coalesce(created_ns, 0) FROM views ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: views: %w", err)
	}
	defer rows.Close()
	var out []View
	for rows.Next() {
		var v View
		var ns int64
		if err := rows.Scan(&v.Name, &v.Filter, &ns); err != nil {
			return nil, fmt.Errorf("store: views: %w", err)
		}
		v.Created = time.Unix(0, ns).UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}

// SaveView saves filter under name, replacing a view of that name.
func (s *Store) SaveView(ctx context.Context, name, filter string) error {
	if _, err := s.w.ExecContext(ctx, `INSERT OR REPLACE INTO views (name, filter, builtin, created_ns) VALUES (?, ?, 0, ?)`,
		name, filter, time.Now().UnixNano()); err != nil {
		return fmt.Errorf("store: save view: %w", err)
	}
	return nil
}

// DeleteView deletes the view name and reports whether there was one.
func (s *Store) DeleteView(ctx context.Context, name string) (bool, error) {
	res, err := s.w.ExecContext(ctx, `DELETE FROM views WHERE name = ?`, name)
	if err != nil {
		return false, fmt.Errorf("store: delete view: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
