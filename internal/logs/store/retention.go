package store

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tenant/internal/fsperm"
)

// Retention (docs/design/logging-v2.md §6.1, §6.2) is the housekeeper's pass
// that keeps each log inside its policy and the whole book inside its budget.
// It deletes the oldest events in chunks of at most chunkRows, each its own
// short transaction, so a pass never holds the write lock for long; then it
// gives the freed pages back and truncates the WAL.
const (
	chunkRows   = 500    // events per delete transaction: §12.3's 50 ms bound
	stepRows    = 10_000 // events picked, and archived to one file, at a time
	vacuumPages = 256    // pages per incremental_vacuum
	sizeRounds  = 3      // measure and delete, at most this often a pass
)

// Report is what a retention pass did.
type Report struct {
	Deleted  map[string]int64 // events removed, by log
	Full     map[string]int64 // of those, removed because their log was past its MaxBytes
	Archived []string         // archive files written
	Kept     []string         // logs past their MaxBytes that keep every record
	Over     bool             // the book is over its budget with nothing left it may delete
	Size     int64            // the database and its WAL afterwards, in bytes

	Chunks  int           // delete transactions
	MaxRows int           // the most events one of them deleted
	MaxLock time.Duration // the longest one of them held the write lock
}

// Retain runs one retention pass under b, as of now:
//  1. events older than their log's MaxAge go (the diagnostic channel's
//     MaxAge applies to diagnostic events too);
//  2. a log whose share of the files, apportioned by its events' summed
//     bytes, is past its MaxBytes loses its oldest events: overwritten,
//     archived first, or, for Keep, kept and reported;
//  3. the same for all diagnostic events together;
//  4. while the database and WAL are past b.TotalBytes, the oldest
//     diagnostic events go, then the oldest events of Overwrite logs. The
//     Security log, archived logs and kept ones are never shed; Over says
//     when that leaves the book over budget;
//  5. rollups older than every log's MaxAge go.
//
// Freed pages are vacuumed and the WAL truncated after the deletes, and sizes
// measured again, so the files end within the budget. Only the housekeeper
// runs it.
func (s *Store) Retain(ctx context.Context, b Budget, now time.Time) (Report, error) {
	rep := Report{Deleted: map[string]int64{}, Full: map[string]int64{}}
	path, err := s.file(ctx)
	if err != nil {
		return rep, err
	}
	r := &retention{s: s, b: b, rep: &rep, names: map[int64]string{}, archiveDir: filepath.Join(filepath.Dir(path), "archive")}
	logs, err := r.policies(ctx)
	if err != nil {
		return rep, err
	}
	for _, l := range logs {
		if l.MaxAge <= 0 {
			continue
		}
		where, args := "log_id = ? AND time_ns < ?", []any{l.id, now.Add(-l.MaxAge).UnixNano()}
		if l.name == SecurityLog { // a prefix of the chain: up to its first record still young
			where = "log_id = ? AND rec < coalesce((SELECT min(rec) FROM events WHERE log_id = ? AND time_ns >= ?), 1 << 62)"
			args = []any{l.id, l.id, now.Add(-l.MaxAge).UnixNano()}
		}
		if _, err := r.sweep(ctx, l.archive(), -1, where, args...); err != nil {
			return rep, err
		}
	}
	if age := b.Diagnostic.MaxAge; age > 0 {
		if _, err := r.sweep(ctx, "", -1, diagnostic+" AND time_ns < ?", r.security, now.Add(-age).UnixNano()); err != nil {
			return rep, err
		}
	}
	if err := r.pruneRollups(ctx, logs, now); err != nil {
		return rep, err
	}
	for round := 0; ; round++ {
		if err := s.compact(ctx); err != nil {
			return rep, err
		}
		if round == sizeRounds {
			break
		}
		if n, err := r.fit(ctx); err != nil || n == 0 {
			if err != nil {
				return rep, err
			}
			break
		}
	}
	db, wal, err := s.Size(ctx)
	rep.Size = db + wal
	return rep, err
}

// diagnostic matches the diagnostic events retention may delete: every log's
// but the Security log's (its id is the argument). channel = 1 is written
// literally so the planner picks events_diag.
const diagnostic = `channel = 1 AND log_id != ?`

type retention struct {
	s          *Store
	b          Budget
	rep        *Report
	names      map[int64]string
	security   int64 // the Security log's id, 0 when it has none
	archiveDir string
}

// policies reads the logs and their policies, noting their names.
func (r *retention) policies(ctx context.Context) ([]logPolicy, error) {
	logs, err := r.s.logPolicies(ctx, r.b)
	for _, l := range logs {
		r.names[l.id] = l.name
		if l.name == SecurityLog {
			r.security = l.id
		}
	}
	return logs, err
}

// archive names the log when its events are archived before they go: an
// Archive log's, and the Security log's whatever its policy.
func (l logPolicy) archive() string {
	if l.OnFull == Archive || l.name == SecurityLog {
		return l.name
	}
	return ""
}

// fit deletes what the size limits call for with the files as they are now,
// and returns how many events it deleted.
func (r *retention) fit(ctx context.Context) (int64, error) {
	logs, err := r.policies(ctx)
	if err != nil {
		return 0, err
	}
	db, wal, err := r.s.Size(ctx)
	if err != nil {
		return 0, err
	}
	size := db + wal
	var total int64
	for _, l := range logs {
		total += l.bytes
	}
	r.rep.Kept, r.rep.Over = nil, false
	if total <= 0 || size <= 0 {
		return 0, nil
	}
	// payload is how many payload bytes take up disk bytes on disk.
	payload := func(disk int64) int64 { return int64(float64(disk) * float64(total) / float64(size)) }
	before := r.deleted()
	var freed int64
	var shed []string // Overwrite logs' ids: what the whole book may shed
	for _, l := range logs {
		if l.OnFull != Archive && l.OnFull != Keep { // secure() keeps security out
			shed = append(shed, strconv.FormatInt(l.id, 10))
		}
		if l.MaxBytes <= 0 || l.bytes <= payload(l.MaxBytes) {
			continue
		}
		if l.OnFull == Keep {
			r.rep.Kept = append(r.rep.Kept, l.name)
			continue
		}
		was := r.rep.Deleted[l.name]
		n, err := r.sweep(ctx, l.archive(), l.bytes-payload(l.MaxBytes), "log_id = ?", l.id)
		r.rep.Full[l.name] += r.rep.Deleted[l.name] - was
		if err != nil {
			return 0, err
		}
		freed += n
	}
	var diag int64
	if err := r.s.r.QueryRowContext(ctx, `SELECT coalesce(sum(bytes), 0) FROM events WHERE channel = 1`).Scan(&diag); err != nil {
		return 0, fmt.Errorf("store: retain: %w", err)
	}
	if max := r.b.Diagnostic.MaxBytes; max > 0 && diag > payload(max) {
		n, err := r.sweep(ctx, "", diag-payload(max), diagnostic, r.security)
		if err != nil {
			return 0, err
		}
		freed += n
	}
	if r.b.TotalBytes > 0 && size > r.b.TotalBytes {
		want := payload(size-r.b.TotalBytes) - freed
		if want > 0 {
			n, err := r.sweep(ctx, "", want, diagnostic, r.security)
			if err != nil {
				return 0, err
			}
			want -= n
		}
		if want > 0 && len(shed) > 0 {
			n, err := r.sweep(ctx, "", want, "log_id IN ("+strings.Join(shed, ",")+")")
			if err != nil {
				return 0, err
			}
			want -= n
		}
		r.rep.Over = want > 0
	}
	return r.deleted() - before, nil
}

func (r *retention) deleted() (n int64) {
	for _, d := range r.rep.Deleted {
		n += d
	}
	return n
}

// sweep deletes the oldest events that match where (an expression over the
// events table's columns) until want payload bytes are gone, or every match
// when want < 0. Events of the log archive names are written to an archive
// file before any of them is deleted. It returns the payload bytes deleted.
func (r *retention) sweep(ctx context.Context, archive string, want int64, where string, args ...any) (int64, error) {
	pick := `SELECT e.seq, e.bytes FROM events e WHERE ` + where + ` ORDER BY e.time_ns, e.seq LIMIT ?`
	switch {
	case archive == SecurityLog: // in chain order, so the archives and the database stay contiguous; rows are picked before any is rendered
		pick = archiveSelect + ` WHERE e.seq IN (SELECT seq FROM events WHERE ` + where + ` ORDER BY rec LIMIT ?) ORDER BY e.rec`
	case archive != "":
		pick = archiveSelect + ` WHERE ` + where + ` ORDER BY e.time_ns, e.seq LIMIT ?`
	}
	args = append(args, stepRows)
	var freed int64
	for want < 0 || freed < want {
		seqs, lines, err := r.pick(ctx, pick, args, want-freed)
		if err != nil || len(seqs) == 0 {
			return freed, err
		}
		if archive != "" {
			if err := r.archive(archive, seqs, lines); err != nil {
				return freed, err
			}
		}
		for len(seqs) > 0 {
			n := min(len(seqs), chunkRows)
			b, err := r.delete(ctx, seqs[:n])
			if err != nil {
				return freed, err
			}
			freed += b
			seqs = seqs[n:]
		}
		if len(lines) < stepRows {
			break
		}
	}
	return freed, nil
}

// pick reads up to stepRows events for sweep, stopping once their payload
// reaches want (want < 0: no stop), and their archive lines when the query
// selects them.
func (r *retention) pick(ctx context.Context, q string, args []any, want int64) (seqs []int64, lines [][]byte, err error) {
	rows, err := r.s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("store: retain: %w", err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var got int64
	for (want < 0 || got < want) && rows.Next() {
		var seq, bytes int64
		var line []byte
		dest := []any{&seq, &bytes, &line}[:len(cols)]
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, fmt.Errorf("store: retain: %w", err)
		}
		seqs, lines, got = append(seqs, seq), append(lines, line), got+bytes
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: retain: %w", err)
	}
	return seqs, lines, nil
}

// delete removes one chunk of events, their full-text rows and their bytes
// from their logs' totals, in one transaction, and returns the payload bytes
// removed. An event already gone is skipped.
func (r *retention) delete(ctx context.Context, seqs []int64) (int64, error) {
	start := time.Now()
	tx, err := r.s.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: retain: %w", err)
	}
	defer tx.Rollback()
	del, err := tx.PrepareContext(ctx, `DELETE FROM events WHERE seq = ? RETURNING log_id, bytes`)
	if err != nil {
		return 0, fmt.Errorf("store: retain: %w", err)
	}
	fts, err := tx.PrepareContext(ctx, `DELETE FROM events_fts WHERE rowid = ?`)
	if err != nil {
		return 0, fmt.Errorf("store: retain: %w", err)
	}
	type tally struct{ n, bytes int64 }
	byLog := map[int64]*tally{}
	var freed int64
	for _, seq := range seqs {
		var id, b int64
		if err := del.QueryRowContext(ctx, seq).Scan(&id, &b); errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return 0, fmt.Errorf("store: retain: %w", err)
		}
		if _, err := fts.ExecContext(ctx, seq); err != nil {
			return 0, fmt.Errorf("store: retain: %w", err)
		}
		t := byLog[id]
		if t == nil {
			t = &tally{}
			byLog[id] = t
		}
		t.n++
		t.bytes += b
		freed += b
	}
	for id, t := range byLog {
		if _, err := tx.ExecContext(ctx, `UPDATE logs SET bytes = max(coalesce(bytes, 0) - ?, 0) WHERE log_id = ?`, t.bytes, id); err != nil {
			return 0, fmt.Errorf("store: retain: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: retain: %w", err)
	}
	held := time.Since(start)
	r.rep.Chunks++
	r.rep.MaxRows = max(r.rep.MaxRows, len(seqs))
	r.rep.MaxLock = max(r.rep.MaxLock, held)
	for id, t := range byLog {
		r.rep.Deleted[r.names[id]] += t.n
	}
	return freed, nil
}

// pruneRollups drops the per-minute counts older than the longest MaxAge of
// any log: no log holds events that old. None go while a log keeps forever.
func (r *retention) pruneRollups(ctx context.Context, logs []logPolicy, now time.Time) error {
	var longest time.Duration
	for _, l := range logs {
		if l.MaxAge <= 0 {
			return nil
		}
		longest = max(longest, l.MaxAge)
	}
	if longest == 0 {
		return nil
	}
	cutoff := now.Add(-longest).UnixNano() / int64(time.Minute)
	for {
		res, err := r.s.w.ExecContext(ctx, `DELETE FROM rollup WHERE (minute, log_id, level) IN
			(SELECT minute, log_id, level FROM rollup WHERE minute < ? LIMIT `+strconv.Itoa(chunkRows)+`)`, cutoff)
		if err != nil {
			return fmt.Errorf("store: retain: rollups: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
	}
}

// compact gives the freed pages back to the file system, vacuumPages at a
// time so it too holds the write lock only briefly, then checkpoints.
func (s *Store) compact(ctx context.Context) error {
	for last := int64(-1); ; {
		var free int64
		if err := s.w.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
			return fmt.Errorf("store: vacuum: %w", err)
		}
		if free == 0 || free == last {
			break
		}
		last = free
		if _, err := s.w.ExecContext(ctx, `PRAGMA incremental_vacuum(`+strconv.Itoa(vacuumPages)+`)`); err != nil {
			return fmt.Errorf("store: vacuum: %w", err)
		}
	}
	return s.Checkpoint(ctx)
}

// Checkpoint copies the WAL into the database and truncates it
// (wal_checkpoint(TRUNCATE)). A reader in the middle of a transaction can
// keep it from finishing; the next one catches up.
func (s *Store) Checkpoint(ctx context.Context) error {
	var busy, log, done int64
	if err := s.w.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &log, &done); err != nil {
		return fmt.Errorf("store: checkpoint: %w", err)
	}
	return nil
}

// Size is the store's size on disk: the database file and its WAL.
func (s *Store) Size(ctx context.Context) (db, wal int64, err error) {
	path, err := s.file(ctx)
	if err != nil {
		return 0, 0, err
	}
	if db, err = fileSize(path); err != nil {
		return 0, 0, fmt.Errorf("store: size: %w", err)
	}
	if wal, err = fileSize(path + "-wal"); errors.Is(err, os.ErrNotExist) {
		return db, 0, nil
	} else if err != nil {
		return 0, 0, fmt.Errorf("store: size: %w", err)
	}
	return db, wal, nil
}

// fileSize stats through a handle: on Windows a directory listing reports a
// stale size for a file another handle has open, which SQLite's always are.
func fileSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// file is the database's path, as SQLite opened it.
func (s *Store) file(ctx context.Context) (string, error) {
	var path string
	if err := s.r.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path); err != nil {
		return "", fmt.Errorf("store: database path: %w", err)
	}
	return path, nil
}

// archiveSelect reads events as archive lines, one JSON object each: every
// column, names resolved, IDs as hex, and data and keywords as the exact text
// they were stored (and, for security, hashed) as.
var archiveSelect = `SELECT e.seq, e.bytes, json_object('seq', e.seq, 'time_ns', e.time_ns,
  'log', l.name, 'provider', p.name, 'event_id', e.event_id, 'version', e.version,
  'level', e.level, 'channel', e.channel, 'task', t.name, 'opcode', o.name, 'keywords', e.keywords,
  'msg', e.msg, 'data', e.data, 'dtypes', e.dtypes,
  'trace_id', ` + hexID("e.trace_id") + `, 'activity_id', ` + hexID("e.activity_id") + `, 'related_id', ` + hexID("e.related_id") + `,
  'session_id', e.session_id, 'approval_id', e.approval_id, 'tool_call_id', e.tool_call_id,
  'actor', e.actor, 'surface', e.surface,
  'pid', e.pid, 'role', r.name, 'boot_id', ` + hexID("e.boot_id") + `, 'host', h.name, 'proc_version', v.name,
  'bytes', e.bytes, 'rec', e.rec, 'salt', ` + hexID("e.salt") + `, 'data_hash', ` + hexID("e.data_hash") + `, 'hash', ` + hexID("e.hash") + `)
FROM events e
JOIN names l ON l.id = e.log_id
JOIN names p ON p.id = e.provider_id
LEFT JOIN names t ON t.id = e.task_id
LEFT JOIN names o ON o.id = e.opcode
LEFT JOIN names r ON r.id = e.role
LEFT JOIN names h ON h.id = e.host
LEFT JOIN names v ON v.id = e.proc_version`

// hexID renders an ID column as idText does: a BLOB as lowercase hex, text
// as itself, NULL as null.
func hexID(col string) string {
	return "CASE typeof(" + col + ") WHEN 'blob' THEN lower(hex(" + col + ")) ELSE " + col + " END"
}

// archive writes one step's events to <archive dir>/<log>-<first seq>-<last
// seq>.ndjson.gz before they are deleted. Writing the same events again (a
// pass that died before its deletes) replaces the file. The Security log's
// are named by record number instead, and end with a closing line holding
// the last record's (rec, hash), so verification continues across files.
func (r *retention) archive(log string, seqs []int64, lines [][]byte) error {
	first, last := seqs[0], seqs[len(seqs)-1]
	if log == SecurityLog {
		var a, z line
		if err := errors.Join(json.Unmarshal(lines[0], &a), json.Unmarshal(lines[len(lines)-1], &z)); err != nil {
			return fmt.Errorf("store: archive: %w", err)
		}
		first, last = a.rec(), z.rec()
		closing, _ := json.Marshal(Head{Rec: last, Hash: z.Hash})
		lines = append(lines[:len(lines):len(lines)], closing)
	}
	name := fmt.Sprintf("%s-%d-%d.ndjson.gz", log, first, last)
	if err := writeArchive(filepath.Join(r.archiveDir, name), lines); err != nil {
		return fmt.Errorf("store: archive: %w", err)
	}
	r.rep.Archived = append(r.rep.Archived, name)
	return nil
}

// writeArchive writes lines to path as owner-only gzip NDJSON, one per line.
// It writes a temporary file, syncs it and renames it into place, so a file
// under its final name is always whole.
func writeArchive(path string, lines [][]byte) error {
	if err := fsperm.MkdirAll(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".archive-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // after a successful rename there is nothing left to remove
	z := gzip.NewWriter(f)
	for _, line := range lines {
		if _, err = z.Write(append(line, '\n')); err != nil {
			break
		}
	}
	err = errors.Join(err, z.Close(), f.Sync(), f.Close())
	if err == nil {
		err = fsperm.RestrictFile(f.Name())
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	return err
}
