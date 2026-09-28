package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tenant/internal/logs/event"
)

// A privacy request removes a forgotten session from the log book
// (docs/design/logging-v2.md §6.5): its events leave every log but security,
// and in security a tombstone empties each record and keeps what the chain
// needs to verify (§10.2).

// Match selects the events of forgotten sessions and runs.
type Match struct {
	Sessions map[string]bool // session_id values
	Traces   map[string]bool // trace_id values as text: runtrace's 32 hex characters, or any other ID
}

// Selects reports whether m selects an event carrying these IDs.
func (m Match) Selects(session, trace string) bool {
	return session != "" && m.Sessions[session] || trace != "" && m.Traces[trace]
}

// Tombstone is the message of a security record a privacy request emptied.
// Its data and salt are gone; its record number, data_hash and hash stay, so
// the chain still verifies, and with the salt gone nobody can confirm a
// guessed value against the hash.
const Tombstone = "«deleted by a privacy request»"

// hit is one event a Match selects.
type hit struct {
	seq, logID, level, timeNS, bytes int64
	log                              string
	security                         bool
}

// hits finds the events m selects, oldest first, on a reader connection.
// Security records already tombstoned are left out, so a second Forget finds
// nothing to do.
//
// ponytail: no index covers session_id, so this reads every event that has a
// session or a trace (about a second at the 2 GB budget); add an index on
// session_id if forgets ever get frequent.
func (s *Store) hits(ctx context.Context, m Match) ([]hit, error) {
	if len(m.Sessions) == 0 && len(m.Traces) == 0 {
		return nil, nil
	}
	rows, err := s.r.QueryContext(ctx, `SELECT e.seq, e.log_id, e.level, e.time_ns, e.bytes, n.name,
		coalesce(e.session_id, ''), e.trace_id, e.msg = ?
		FROM events e JOIN names n ON n.id = e.log_id
		WHERE e.session_id IS NOT NULL OR e.trace_id IS NOT NULL ORDER BY e.seq`, Tombstone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hit
	for rows.Next() {
		var h hit
		var session string
		var trace any
		var tomb bool
		if err := rows.Scan(&h.seq, &h.logID, &h.level, &h.timeNS, &h.bytes, &h.log, &session, &trace, &tomb); err != nil {
			return nil, err
		}
		h.security = h.log == SecurityLog
		if m.Selects(session, idText(trace)) && !(h.security && tomb) {
			out = append(out, h)
		}
	}
	return out, rows.Err()
}

// forgetChunk bounds the rows one Forget transaction touches, as retention
// does, so the writers waiting on the lock are not held up for long.
const forgetChunk = 500

// Matching calls fn with each event m selects, oldest first, sensitive fields
// included: a deletion preview and a privacy export read through it.
// Security records already tombstoned are left out.
func (s *Store) Matching(ctx context.Context, m Match, fn func(event.Event) error) error {
	hs, err := s.hits(ctx, m)
	if err != nil {
		return fmt.Errorf("store: matching: %w", err)
	}
	for len(hs) > 0 {
		n := min(len(hs), forgetChunk)
		args := make([]any, n)
		for i, h := range hs[:n] {
			args[i] = h.seq
		}
		hs = hs[n:]
		if err := s.each(ctx, args, fn); err != nil {
			return fmt.Errorf("store: matching: %w", err)
		}
	}
	return nil
}

func (s *Store) each(ctx context.Context, seqs []any, fn func(event.Event) error) error {
	evs, err := queryEvents(ctx, s.r, selectEvents+` WHERE e.seq IN (?`+strings.Repeat(", ?", len(seqs)-1)+`) ORDER BY e.seq`, seqs...)
	for _, e := range evs {
		if err := fn(e); err != nil {
			return err
		}
	}
	return err
}

// ForgetResult is what Forget changed.
type ForgetResult struct {
	Deleted    map[string]int // events deleted, per log
	Tombstoned int            // security records emptied
	// Busy reports that another connection was reading during the final
	// checkpoint, so older copies of the freed pages may stay in the WAL
	// until the next one.
	Busy bool
}

// Forget removes the events m selects by the house recipe (internal/privacy):
//   - rows are deleted on the writer connection, whose secure_delete zeroes
//     the space they held, with their full-text rows in the same transaction
//     and their rollups and log sizes taken down;
//   - a security record is tombstoned instead: Tombstone replaces its
//     message, and its data and salt are cleared;
//   - then the full-text index is optimized, so no deleted words stay in its
//     segments, and the WAL is checkpointed and truncated.
func (s *Store) Forget(ctx context.Context, m Match) (ForgetResult, error) {
	res := ForgetResult{Deleted: map[string]int{}}
	hs, err := s.hits(ctx, m)
	if err != nil {
		return res, fmt.Errorf("store: forget: %w", err)
	}
	if len(hs) == 0 {
		return res, nil
	}
	for len(hs) > 0 {
		n := min(len(hs), forgetChunk)
		if err := s.forgetChunk(ctx, hs[:n], &res); err != nil {
			return res, fmt.Errorf("store: forget: %w", err)
		}
		hs = hs[n:]
	}
	if _, err := s.w.ExecContext(ctx, `INSERT INTO events_fts (events_fts) VALUES ('optimize')`); err != nil {
		return res, fmt.Errorf("store: forget: optimize the search index: %w", err)
	}
	res.Busy, err = s.Checkpoint(ctx)
	return res, err
}

func (s *Store) forgetChunk(ctx context.Context, hs []hit, res *ForgetResult) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	exec := func(q string, args ...any) error {
		_, err := tx.ExecContext(ctx, q, args...)
		return err
	}
	deleted, tombstoned := map[string]int{}, 0
	for _, h := range hs {
		err := exec(`DELETE FROM events_fts WHERE rowid = ?`, h.seq)
		if h.security {
			if err == nil {
				err = exec(`UPDATE events SET msg = ?, data = NULL, dtypes = NULL, salt = NULL WHERE seq = ?`, Tombstone, h.seq)
			}
			tombstoned++
		} else {
			if err == nil && h.level <= int64(event.Warning) {
				err = exec(`UPDATE rollup_id SET n = n - 1 WHERE (hour, provider_id, event_id) =
					(SELECT time_ns / 3600000000000, provider_id, event_id FROM events WHERE seq = ?)`, h.seq)
			}
			if err == nil {
				err = exec(`DELETE FROM events WHERE seq = ?`, h.seq)
			}
			if err == nil {
				err = exec(`UPDATE rollup SET n = n - 1 WHERE minute = ? AND log_id = ? AND level = ?`, h.timeNS/int64(time.Minute), h.logID, h.level)
			}
			if err == nil {
				err = exec(`UPDATE rollup_hour SET n = n - 1 WHERE hour = ? AND log_id = ? AND level = ?`, h.timeNS/int64(time.Hour), h.logID, h.level)
			}
			if err == nil {
				err = exec(`UPDATE logs SET bytes = bytes - ? WHERE log_id = ?`, h.bytes, h.logID)
			}
			deleted[h.log]++
		}
		if err != nil {
			return err
		}
	}
	if err := exec(`DELETE FROM rollup WHERE n <= 0`); err != nil {
		return err
	}
	if err := exec(`DELETE FROM rollup_hour WHERE n <= 0`); err != nil {
		return err
	}
	if err := exec(`DELETE FROM rollup_id WHERE n <= 0`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for log, n := range deleted {
		res.Deleted[log] += n
	}
	res.Tombstoned += tombstoned
	return nil
}
