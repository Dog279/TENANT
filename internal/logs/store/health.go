package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"tenant/internal/datalock"
)

// The store's health (docs/design/logging-v2.md §12.1): its lock, a damaged
// database moved aside and salvaged, the integrity check, and the counters
// of events that were never written.
//
// Every process with the store open holds <logs root>/logbook.lock shared
// (Open). Quarantine and Reidentify (a restore) take it exclusively, so they
// never move or rewrite a database another process has open: that fails on
// Windows and corrupts the database on POSIX.

// ErrCorrupt marks an error SQLite gives for a damaged database file.
var ErrCorrupt = errors.New("store: logbook.db is corrupt")

// corrupt marks err with ErrCorrupt when SQLite says the file is damaged or
// is not a database.
func corrupt(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) {
		if c := se.Code() & 0xff; c == sqlite3.SQLITE_CORRUPT || c == sqlite3.SQLITE_NOTADB {
			return fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
	}
	return err
}

// lockFile is the store's lock: logbook.lock beside logbook.db.
func lockFile(path string) string { return strings.TrimSuffix(path, ".db") + ".lock" }

// SchemaVersion is the schema this build brings a store to.
func SchemaVersion() int { return len(migrations) }

// Quarantine moves the database at path aside, together with its -wal and
// -shm, to <path>.corrupt-<time>, and returns where the database went. A
// fresh Open then starts an empty store with a new ID. It takes the store's
// lock exclusively, so it fails with datalock.ErrBusy while any process has
// the store open, and moves nothing. Should one of the files not move, the
// others move back: a new store must never start beside a WAL that isn't
// its own.
func Quarantine(path string) (string, error) {
	lock, err := datalock.ExclusiveFile(lockFile(path))
	if err != nil {
		return "", fmt.Errorf("store: quarantine: %w", err)
	}
	defer lock.Release()
	to := path + ".corrupt-" + time.Now().UTC().Format("20060102-150405")
	var moved []string
	for _, side := range []string{"", "-wal", "-shm"} {
		err := os.Rename(path+side, to+side)
		switch {
		case err == nil:
			moved = append(moved, side)
		case side != "" && errors.Is(err, fs.ErrNotExist):
		default:
			for _, m := range slices.Backward(moved) {
				_ = os.Rename(to+m, path+m)
			}
			return "", fmt.Errorf("store: quarantine: %w", err)
		}
	}
	return to, nil
}

// salvageSkip is how many events Salvage reads at a time, as many as it can
// before an error, and how far it jumps past a stretch it can't read;
// salvageTries is how many jumps in a row it makes before it gives up.
const (
	salvageSkip  = 500
	salvageTries = 1000
)

// Salvage copies what it can still read of the events in the database at
// from (a file Quarantine moved aside) into to, each with the keyword
// recovered, and returns how many it copied. It reads in pages by seq and
// jumps past a stretch it can't read; err joins what it skipped over, and
// ErrNoChainKey when to has no key for the security records.
// `tenant logs recover` runs it.
//
// ponytail: salvaged Security records join to's chain as new records; a
// re-anchor record (TEN-396) should say where they came from.
func Salvage(ctx context.Context, from string, to *Store) (n int, err error) {
	db, err := sql.Open("sqlite", "file:"+from+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)")
	if err != nil {
		return 0, fmt.Errorf("store: salvage: %w", err)
	}
	defer db.Close()
	var errs []error
	for seq, tries := int64(0), 0; tries < salvageTries && ctx.Err() == nil; {
		evs, err := queryEvents(ctx, db, selectEvents+` WHERE e.seq > ? ORDER BY e.seq LIMIT ?`, seq, salvageSkip)
		if err != nil && len(evs) == 0 {
			errs = append(errs, fmt.Errorf("after seq %d: %w", seq, err))
			seq, tries = seq+salvageSkip, tries+1
			continue
		}
		if len(evs) == 0 {
			break
		}
		tries, seq = 0, int64(evs[len(evs)-1].Seq)
		for i := range evs {
			evs[i].Seq = 0
			evs[i].Keywords = append(slices.Clip(evs[i].Keywords), "recovered")
		}
		switch err := to.Commit(ctx, evs); {
		case errors.Is(err, ErrNoChainKey): // the rest committed
			if len(errs) == 0 || !errors.Is(errs[len(errs)-1], ErrNoChainKey) {
				errs = append(errs, err)
			}
		case err != nil:
			return n, fmt.Errorf("store: salvage: %w", err)
		}
		for _, e := range evs {
			if e.Seq != 0 {
				n++
			}
		}
	}
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return n, fmt.Errorf("store: salvage: %w", errors.Join(errs...))
	}
	return n, nil
}

// Check runs SQLite's quick_check: nil when the database is sound, else an
// ErrCorrupt error naming the first problems found.
func (s *Store) Check(ctx context.Context) error {
	rows, err := s.r.QueryContext(ctx, `PRAGMA quick_check(10)`)
	if err != nil {
		return fmt.Errorf("store: check: %w", corrupt(err))
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("store: check: %w", err)
		}
		if line != "ok" {
			found = append(found, line)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: check: %w", corrupt(err))
	}
	if len(found) > 0 {
		return fmt.Errorf("%w: %s", ErrCorrupt, strings.Join(found, "; "))
	}
	return nil
}

// Lost is what one log did not get: events dropped (a full queue, low disk)
// and events suppressed by their rate limit.
type Lost struct{ Dropped, Suppressed int64 }

// AddLost adds to each log's counters of events that were never written,
// which Logs reports.
func (s *Store) AddLost(ctx context.Context, lost map[string]Lost) error {
	if len(lost) == 0 {
		return nil
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: counters: %w", err)
	}
	defer tx.Rollback()
	for log, l := range lost {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO names (kind, name) VALUES ('log', ?)`, log); err != nil {
			return fmt.Errorf("store: counters: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO logs (log_id, dropped, suppressed)
			VALUES ((SELECT id FROM names WHERE kind = 'log' AND name = ?), ?, ?)
			ON CONFLICT (log_id) DO UPDATE SET dropped = coalesce(dropped, 0) + excluded.dropped,
			suppressed = coalesce(suppressed, 0) + excluded.suppressed`, log, l.Dropped, l.Suppressed); err != nil {
			return fmt.Errorf("store: counters: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: counters: %w", err)
	}
	return nil
}

// Fill is how full each log with a size limit is under b: its share of the
// files, apportioned by its events' payload as retention does, over its
// MaxBytes (1 is at the limit).
func (s *Store) Fill(ctx context.Context, b Budget) (map[string]float64, error) {
	logs, err := s.logPolicies(ctx, b)
	if err != nil {
		return nil, err
	}
	db, wal, err := s.Size(ctx)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, l := range logs {
		total += l.bytes
	}
	out := map[string]float64{}
	if total <= 0 {
		return out, nil
	}
	for _, l := range logs {
		if l.MaxBytes > 0 {
			out[l.name] = float64(l.bytes) / float64(total) * float64(db+wal) / float64(l.MaxBytes)
		}
	}
	return out, nil
}
