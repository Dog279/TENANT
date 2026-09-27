package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"tenant/internal/logs/event"
)

// What a log does once it passes its MaxBytes (docs/design/logging-v2.md §6.2).
const (
	Overwrite = "overwrite" // its oldest records are deleted
	Archive   = "archive"   // its oldest records move to <logs root>/archive/ first
	Keep      = "keep"      // nothing is deleted; the housekeeper warns once
)

// Policy is how one log is kept and written.
type Policy struct {
	MaxBytes int64         // its share of the database and WAL (0 ⇒ no limit)
	MaxAge   time.Duration // older records are removed (0 ⇒ kept forever)
	OnFull   string        // Overwrite, Archive or Keep
	MinLevel event.Level   // less severe events are not written (0 ⇒ every level)
	Disabled bool          // nothing is written to it
}

// Budget is the retention configuration, config.json's logs block: the
// policy of each log that has none of its own in the logs table (SetPolicy),
// the limits on every log's diagnostic records together, and the size of the
// whole book.
type Budget struct {
	Logs        map[string]Policy // by log: security, system and logs.per_log
	Operational Policy            // every other log
	Diagnostic  Policy            // all diagnostic records together: MaxBytes and MaxAge
	TotalBytes  int64             // the database plus its WAL (0 ⇒ no limit)
}

// DefaultBudget is §6.2's table.
func DefaultBudget() Budget {
	const day = 24 * time.Hour
	return Budget{
		Logs: map[string]Policy{
			SecurityLog: {MaxBytes: 256 << 20, MaxAge: 365 * day, OnFull: Archive},
			"system":    {MaxBytes: 128 << 20, MaxAge: 90 * day, OnFull: Overwrite},
		},
		Operational: Policy{MaxBytes: 64 << 20, MaxAge: 30 * day, OnFull: Overwrite},
		Diagnostic:  Policy{MaxBytes: 256 << 20, MaxAge: 7 * day, OnFull: Overwrite},
		TotalBytes:  2 << 30,
	}
}

// For is log's policy under b.
func (b Budget) For(log string) Policy {
	p, ok := b.Logs[log]
	if !ok {
		p = b.Operational
	}
	return secure(log, p)
}

// secure keeps the Security log whole, whatever a setting says: it is always
// written, at every level, and never overwritten.
func secure(log string, p Policy) Policy {
	if log == SecurityLog {
		p.MinLevel, p.Disabled = 0, false
		if p.OnFull != Keep {
			p.OnFull = Archive
		}
	}
	return p
}

// logPolicy is a log the store holds, with the policy in force for it.
type logPolicy struct {
	Policy
	id    int64
	name  string
	bytes int64 // its events' summed payload
}

// logPolicies lists every log in the logs table with its policy: its own
// where it has one (a row's on_full is set only by SetPolicy), else b's.
func (s *Store) logPolicies(ctx context.Context, b Budget) ([]logPolicy, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT l.log_id, n.name, coalesce(l.bytes, 0), l.on_full,
		coalesce(l.max_bytes, 0), coalesce(l.max_age_s, 0), coalesce(l.min_level, 0), coalesce(l.enabled, 1)
		FROM logs l JOIN names n ON n.id = l.log_id`)
	if err != nil {
		return nil, fmt.Errorf("store: policies: %w", err)
	}
	defer rows.Close()
	var out []logPolicy
	for rows.Next() {
		var l logPolicy
		var onFull sql.NullString
		var maxAge, minLevel, enabled int64
		if err := rows.Scan(&l.id, &l.name, &l.bytes, &onFull, &l.MaxBytes, &maxAge, &minLevel, &enabled); err != nil {
			return nil, fmt.Errorf("store: policies: %w", err)
		}
		if onFull.Valid {
			l.Policy = secure(l.name, Policy{MaxBytes: l.MaxBytes, MaxAge: time.Duration(maxAge) * time.Second,
				OnFull: onFull.String, MinLevel: event.Level(minLevel), Disabled: enabled == 0})
		} else {
			l.Policy = b.For(l.name)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Policies returns the policy in force for every log the store holds: the
// log's own where SetPolicy gave it one, else b's.
func (s *Store) Policies(ctx context.Context, b Budget) (map[string]Policy, error) {
	logs, err := s.logPolicies(ctx, b)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Policy, len(logs))
	for _, l := range logs {
		out[l.name] = l.Policy
	}
	return out, nil
}

// Policy returns the policy in force for log under b.
func (s *Store) Policy(ctx context.Context, b Budget, log string) (Policy, error) {
	ps, err := s.Policies(ctx, b)
	if err != nil {
		return Policy{}, err
	}
	if p, ok := ps[log]; ok {
		return p, nil
	}
	return b.For(log), nil
}

// SetPolicy gives log a policy of its own, which wins over config.json's
// (tenant logs config, the Properties dialog) and reaches every process at
// its next housekeeper tick. The zero Policy returns the log to config's.
func (s *Store) SetPolicy(ctx context.Context, log string, p Policy) error {
	own := p != Policy{}
	switch {
	case log == "":
		return errors.New("store: set policy: no log named")
	case !own:
	case p.OnFull != Overwrite && p.OnFull != Archive && p.OnFull != Keep:
		return fmt.Errorf("store: set policy: when full %q: want %s, %s or %s", p.OnFull, Overwrite, Archive, Keep)
	case p.MaxBytes < 0 || p.MaxAge < 0:
		return errors.New("store: set policy: a negative limit")
	case p.MinLevel < 0 || p.MinLevel > event.Verbose:
		return fmt.Errorf("store: set policy: level %d", p.MinLevel)
	case log == SecurityLog && (p.Disabled || p.OnFull == Overwrite || p.MinLevel != 0 && p.MinLevel != event.Verbose):
		return errors.New("store: set policy: the Security log is always written in full and never overwritten")
	}
	var args [5]any // NULL: follow config
	if own {
		args = [5]any{p.MaxBytes, int64(p.MaxAge / time.Second), p.OnFull, int64(p.MinLevel), 1}
		if p.Disabled {
			args[4] = 0
		}
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: set policy: %w", err)
	}
	defer tx.Rollback()
	var id int64
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO names (kind, name) VALUES ('log', ?)`, log); err == nil {
		err = tx.QueryRowContext(ctx, `SELECT id FROM names WHERE kind = 'log' AND name = ?`, log).Scan(&id)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO logs (log_id, max_bytes, max_age_s, on_full, min_level, enabled) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (log_id) DO UPDATE SET max_bytes = excluded.max_bytes, max_age_s = excluded.max_age_s,
			on_full = excluded.on_full, min_level = excluded.min_level, enabled = excluded.enabled`,
			id, args[0], args[1], args[2], args[3], args[4])
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("store: set policy: %w", err)
	}
	return nil
}
