// Package store is the log book's system of record: <data>/logs/logbook.db,
// one SQLite database that every Tenant process writes through its own
// writer connection (docs/design/logging-v2.md §5, §6.1).
//
// Commit files a batch in one transaction: the events, their full-text rows,
// the per-minute and per-hour rollups and each log's byte count. Commit order is sequence
// order, so a reader holding a Cursor (the store's ID plus a seq) never
// misses a row: Since returns everything after it. A cursor from another
// store (a rebuilt or restored database) restarts from the beginning.
//
// # Data encoding
//
// The data column is a JSON object keyed by field name, in field order, so
// json_extract(data, '$.key') serves data.<key> filters. Its values:
//
//	string, int64, float64, bool, nil   the JSON value
//	json.RawMessage                     embedded as JSON (a filter can reach into it)
//	time.Time                           an RFC 3339 string, UTC, nanoseconds
//	time.Duration                       its nanoseconds, a JSON number
//
// The dtypes column keeps what JSON can't: one letter per field, in order —
// s string, i int64, f float64, b bool, n nil, j JSON, t time, d duration —
// upper case when the field is sensitive. So "sIt" is a string, a sensitive
// int64 and a time. A value no type fits (a NaN, invalid JSON) is kept as its
// text. Both columns are NULL when an event has no data.
//
// Names (logs, providers, tasks, opcodes, roles, hosts, versions) are
// interned in the names table. Trace, activity, related and boot IDs in
// runtrace's form (32 lowercase hex characters) are kept as 16-byte BLOBs;
// any other ID is kept as its text.
//
// # The Security log's chain
//
// Every security record is linked, in the transaction that commits it, into
// a keyed hash chain (docs/design/logging-v2.md §10.2, TEN-396). Its columns:
//
//	rec        the record number: the chain head's (meta 'chain_head') plus one; no gaps
//	salt       16 random bytes
//	data_hash  SHA-256(salt ‖ str(msg) ‖ str(data) ‖ str(dtypes))
//	hash       HMAC-SHA256(key, previous hash ‖ header ‖ data_hash)
//
// The previous hash of record 1 is 32 zero bytes. str(s) is s's length as
// a 4-byte big-endian integer, then its bytes; num(n) is n as an 8-byte
// big-endian two's-complement integer; an absent value is "" or 0. msg, data,
// dtypes and keywords are the columns' exact text (keywords a JSON array),
// names are their text, and IDs their text form (lowercase hex for a 16-byte
// ID). The header is, in this order:
//
//	num(rec) num(time_ns) str(log) str(provider) num(event_id) num(version)
//	num(level) num(channel) str(task) str(opcode) str(keywords)
//	str(trace_id) str(activity_id) str(related_id)
//	str(session_id) str(approval_id) str(tool_call_id) str(actor) str(surface)
//	num(pid) str(role) str(boot_id) str(host) str(proc_version)
//
// seq and bytes are left out: the chain survives a restore, and the chain
// columns themselves are what it produces. The values are read back from
// the committed row as an archive line holds them (archiveSelect), so the
// database, the archives and an export bundle all hash alike. A privacy
// tombstone clears msg, data, dtypes and salt and keeps data_hash, so the
// chain still verifies.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tenant/internal/fsperm"
	"tenant/internal/logs/event"

	_ "modernc.org/sqlite"
)

// SecurityLog is the log whose records are chained (TEN-396).
const SecurityLog = "security"

// pollEvery is how often live subscribers look for other processes' commits.
const pollEvery = 500 * time.Millisecond

// migrations are the schema, forward-only: migrations[i] takes the database
// from version i to i+1, and meta's schema_version records where it stands.
// Never edit a released one; append.
var migrations = []string{`
CREATE TABLE names (id INTEGER PRIMARY KEY, kind TEXT NOT NULL, name TEXT NOT NULL, UNIQUE (kind, name));
CREATE TABLE events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  log_id INTEGER NOT NULL, provider_id INTEGER NOT NULL,
  event_id INTEGER NOT NULL DEFAULT 0, version INTEGER NOT NULL DEFAULT 0,
  time_ns INTEGER NOT NULL, level INTEGER NOT NULL, channel INTEGER NOT NULL DEFAULT 0,
  task_id INTEGER, opcode INTEGER, keywords TEXT,
  msg TEXT NOT NULL, data TEXT, dtypes TEXT,
  trace_id BLOB, activity_id BLOB, related_id BLOB,
  session_id TEXT, approval_id TEXT, tool_call_id TEXT,
  actor TEXT, surface TEXT,
  pid INTEGER, role INTEGER, boot_id BLOB, host INTEGER, proc_version INTEGER,
  bytes INTEGER NOT NULL,
  rec INTEGER, salt BLOB, data_hash BLOB, hash BLOB
);
CREATE UNIQUE INDEX events_rec ON events(rec) WHERE rec IS NOT NULL;
CREATE INDEX events_time     ON events(time_ns);
CREATE INDEX events_log_time ON events(log_id, time_ns);
CREATE INDEX events_admin    ON events(time_ns) WHERE level <= 3;
CREATE INDEX events_id       ON events(provider_id, event_id, time_ns);
CREATE INDEX events_trace    ON events(trace_id)    WHERE trace_id    IS NOT NULL;
CREATE INDEX events_activity ON events(activity_id) WHERE activity_id IS NOT NULL;
CREATE INDEX events_related  ON events(related_id)  WHERE related_id  IS NOT NULL;
CREATE VIRTUAL TABLE events_fts USING fts5(msg, content='', contentless_delete=1);
CREATE TABLE rollup (minute INTEGER, log_id INTEGER, level INTEGER, n INTEGER,
  PRIMARY KEY (minute, log_id, level)) WITHOUT ROWID;
CREATE TABLE logs (log_id INTEGER PRIMARY KEY, display TEXT, grp TEXT, kind TEXT,
  max_bytes INTEGER, max_age_s INTEGER, on_full TEXT, min_level INTEGER, enabled INTEGER,
  debug_until_ns INTEGER, bytes INTEGER, dropped INTEGER, suppressed INTEGER, cleared_ns INTEGER);
CREATE TABLE boots (boot_id BLOB PRIMARY KEY, pid INTEGER, role INTEGER, version TEXT, host TEXT,
  started_ns INTEGER, heartbeat_ns INTEGER, stopped_ns INTEGER, clean INTEGER, reported INTEGER);
CREATE TABLE leases (name TEXT PRIMARY KEY, boot_id BLOB, expires_ns INTEGER);
CREATE TABLE views (name TEXT PRIMARY KEY, filter TEXT NOT NULL, builtin INTEGER, created_ns INTEGER);
`,
	// 2: retention (TEN-371) sums and sheds the diagnostic channel's bytes
	// without scanning every event.
	`CREATE INDEX events_diag ON events(time_ns, bytes) WHERE channel = 1;`,
	// 3: the Summary page (TEN-380) reads per-hour rollups, which Commit
	// keeps beside the minutes (addHourRollups): by log and level, and by
	// event ID for warnings and worse. Filled here from what the store holds.
	`CREATE TABLE rollup_hour (hour INTEGER, log_id INTEGER, level INTEGER, n INTEGER,
  PRIMARY KEY (hour, log_id, level)) WITHOUT ROWID;
CREATE TABLE rollup_id (hour INTEGER, provider_id INTEGER, event_id INTEGER, n INTEGER,
  PRIMARY KEY (hour, provider_id, event_id)) WITHOUT ROWID;
INSERT INTO rollup_hour (hour, log_id, level, n) SELECT minute / 60, log_id, level, sum(n) FROM rollup GROUP BY 1, 2, 3;
INSERT INTO rollup_id (hour, provider_id, event_id, n)
  SELECT time_ns / 3600000000000, provider_id, event_id, count(*) FROM events WHERE level <= 3 GROUP BY 1, 2, 3;`,
}

// Options configures a Store.
type Options struct {
	// BootID names this process in the leases it takes: runtrace.BootID().
	BootID string
	// ChainKey is the Security log's HMAC key. Without it a commit holding a
	// security record is refused (ErrNoChainKey).
	ChainKey []byte
}

// Store is an open logbook.db. Safe for concurrent use.
type Store struct {
	w    *sql.DB // the process's one writer connection
	r    *sql.DB // readers
	id   [16]byte
	boot []byte
	key  []byte // the chain key; nil refuses security records

	mu    sync.Mutex       // one Commit at a time; guards names
	names map[string]int64 // kind "\x00" name → names.id, committed ones only

	subMu    sync.Mutex
	subs     map[chan struct{}]struct{}
	pollOnce sync.Once
	head     atomic.Uint64 // the newest seq a subscriber was woken for
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// Cursor is a position in one store: the rows after Seq.
type Cursor struct {
	StoreID [16]byte // a new database, restore or rebuild gets a new ID, so old cursors restart
	Seq     uint64
}

// String is the cursor's text form, <store ID hex>.<seq>.
func (c Cursor) String() string {
	return hex.EncodeToString(c.StoreID[:]) + "." + strconv.FormatUint(c.Seq, 10)
}

// ParseCursor reads a cursor's text form.
func ParseCursor(s string) (Cursor, error) {
	var c Cursor
	id, seq, ok := strings.Cut(s, ".")
	n, err := strconv.ParseUint(seq, 10, 64)
	if !ok || len(id) != 32 || err != nil {
		return c, fmt.Errorf("store: bad cursor %q", s)
	}
	if _, err := hex.Decode(c.StoreID[:], []byte(id)); err != nil {
		return c, fmt.Errorf("store: bad cursor %q", s)
	}
	c.Seq = n
	return c, nil
}

// ErrNotFound is Get's error for a seq the store doesn't hold.
var ErrNotFound = errors.New("store: no such event")

// writerPragmas are the writer connection's settings. auto_vacuum only takes
// on a new database, before its first table.
func writerPragmas() []string {
	p := []string{"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)", "secure_delete(ON)", "auto_vacuum(INCREMENTAL)"}
	if runtime.GOOS == "darwin" { // FULL alone doesn't flush the drive's cache there
		p = append(p, "fullfsync(ON)", "checkpoint_fullfsync(ON)")
	}
	return p
}

// Open opens the store at path, creating it (owner-only) and bringing its
// schema up to date.
func Open(path string, opts Options) (*Store, error) {
	if err := fsperm.PrepareFile(path); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	dsn := "file:" + path + "?_txlock=immediate"
	for _, p := range writerPragmas() {
		dsn += "&_pragma=" + p
	}
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)")
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("store: open: %w", err)
	}
	s := &Store{w: w, r: r, boot: idBytes(opts.BootID), key: opts.ChainKey, names: map[string]int64{},
		subs: map[chan struct{}]struct{}{}, stop: make(chan struct{})}
	if err := s.migrate(context.Background()); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// migrate brings the schema up to date and mints the store ID on creation.
// A store that is up to date is only read, so opening one never waits on
// the write lock. Otherwise the work runs in one write transaction that
// reads again, so two processes creating the store at once agree.
func (s *Store) migrate(ctx context.Context) error {
	if v, id := meta(ctx, s.r); v == strconv.Itoa(len(migrations)) && s.setID(id) == nil {
		return nil
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	version, id := meta(ctx, tx)
	v, _ := strconv.Atoi(version)
	if v > len(migrations) {
		return fmt.Errorf("store: logbook.db is schema version %d, newer than this build's %d", v, len(migrations))
	}
	for ; v < len(migrations); v++ {
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			return fmt.Errorf("store: migrate to version %d: %w", v+1, err)
		}
	}
	if id == "" {
		var b [16]byte
		_, _ = rand.Read(b[:]) // crypto/rand never fails (Go 1.24+)
		id = hex.EncodeToString(b[:])
	}
	if err := s.setID(id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?), ('store_id', ?)`,
		strconv.Itoa(v), id); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return tx.Commit()
}

// meta reads the schema version and the store ID ("" when absent).
func meta(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (version, id string) {
	_ = q.QueryRowContext(ctx, `SELECT coalesce((SELECT value FROM meta WHERE key = 'schema_version'), ''),
		coalesce((SELECT value FROM meta WHERE key = 'store_id'), '')`).Scan(&version, &id)
	return version, id
}

func (s *Store) setID(id string) error {
	if len(id) != 32 {
		return fmt.Errorf("store: bad store_id %q in meta", id)
	}
	if _, err := hex.Decode(s.id[:], []byte(id)); err != nil {
		return fmt.Errorf("store: bad store_id %q in meta", id)
	}
	return nil
}

// StoreID identifies this database; cursors carry it.
func (s *Store) StoreID() [16]byte { return s.id }

// Close stops the subscriber poll and closes the connections. Commits after
// Close return an error.
func (s *Store) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
	return errors.Join(s.r.Close(), s.w.Close())
}

// Commit files batch in one transaction: the events, their full-text rows,
// the rollups and each log's bytes. On success it writes each event's
// assigned Seq back into batch, then wakes the live subscribers. An event
// with no Time is stamped now.
func (s *Store) Commit(ctx context.Context, batch []event.Event) error {
	if len(batch) == 0 {
		return nil
	}
	if len(s.key) == 0 && slices.ContainsFunc(batch, func(e event.Event) bool { return e.Log == SecurityLog }) {
		return s.commitWithoutSecurity(ctx, batch)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	defer tx.Rollback()
	ins, err := tx.PrepareContext(ctx, insertEvent)
	if err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	fts, err := tx.PrepareContext(ctx, `INSERT INTO events_fts (rowid, msg) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	in := interner{s: s, tx: tx, ctx: ctx, added: map[string]int64{}}
	type rollKey struct{ minute, log, level int64 }
	rolls := map[rollKey]int64{}
	bytesByLog := map[int64]int64{}
	seqs := make([]uint64, len(batch))
	for i := range batch {
		e := &batch[i]
		if e.Time.IsZero() {
			e.Time = time.Now().UTC()
		}
		r, err := in.row(e)
		if err != nil {
			return fmt.Errorf("store: commit: %w", err)
		}
		res, err := ins.ExecContext(ctx, r.args()...)
		if err != nil {
			return fmt.Errorf("store: commit: %w", err)
		}
		seq, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: commit: %w", err)
		}
		if _, err := fts.ExecContext(ctx, seq, e.Msg); err != nil {
			return fmt.Errorf("store: commit: %w", err)
		}
		if e.Log == SecurityLog {
			if err := s.chain(ctx, tx, seq, r); err != nil {
				return fmt.Errorf("store: commit: %w", err)
			}
		}
		seqs[i] = uint64(seq)
		rolls[rollKey{r.timeNS / int64(time.Minute), r.logID, int64(e.Level)}]++
		bytesByLog[r.logID] += r.bytes
	}
	for k, n := range rolls {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rollup (minute, log_id, level, n) VALUES (?, ?, ?, ?)
			ON CONFLICT (minute, log_id, level) DO UPDATE SET n = n + excluded.n`, k.minute, k.log, k.level, n); err != nil {
			return fmt.Errorf("store: commit: %w", err)
		}
	}
	if err := addHourRollups(ctx, tx, seqs[0], seqs[len(seqs)-1]); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	for id, b := range bytesByLog {
		if _, err := tx.ExecContext(ctx, `INSERT INTO logs (log_id, bytes) VALUES (?, ?)
			ON CONFLICT (log_id) DO UPDATE SET bytes = coalesce(bytes, 0) + excluded.bytes`, id, b); err != nil {
			return fmt.Errorf("store: commit: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	for k, id := range in.added {
		s.names[k] = id
	}
	for i := range batch {
		batch[i].Seq = seqs[i]
	}
	s.head.Store(seqs[len(seqs)-1])
	s.wake()
	return nil
}

// Since returns up to limit events after from, oldest first (limit <= 0 means
// 1000), and the cursor to pass next time. A cursor from another store
// restarts from the beginning, and restarted says so; the zero Cursor is the
// beginning.
func (s *Store) Since(ctx context.Context, from Cursor, limit int) (evs []event.Event, next Cursor, restarted bool, err error) {
	if from.StoreID != s.id {
		restarted = from != Cursor{}
		from = Cursor{StoreID: s.id}
	}
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.r.QueryContext(ctx, selectEvents+` WHERE e.seq > ? ORDER BY e.seq LIMIT ?`, from.Seq, limit)
	if err != nil {
		return nil, from, restarted, fmt.Errorf("store: since: %w", err)
	}
	defer rows.Close()
	next = from
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, from, restarted, fmt.Errorf("store: since: %w", err)
		}
		evs = append(evs, e)
		next.Seq = e.Seq
	}
	if err := rows.Err(); err != nil {
		return nil, from, restarted, fmt.Errorf("store: since: %w", err)
	}
	return evs, next, restarted, nil
}

// Get returns the event with the given seq, or ErrNotFound.
func (s *Store) Get(ctx context.Context, seq uint64) (event.Event, error) {
	e, err := scanEvent(s.r.QueryRowContext(ctx, selectEvents+` WHERE e.seq = ?`, seq))
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, fmt.Errorf("store: get: %w", err)
	}
	return e, nil
}

// LogInfo is one log the store holds.
type LogInfo struct {
	Name  string
	Bytes int64 // the summed payload of its events: its share of the budget
}

// Logs lists every log that has events, by name.
func (s *Store) Logs(ctx context.Context) ([]LogInfo, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT n.name, coalesce(l.bytes, 0) FROM logs l JOIN names n ON n.id = l.log_id ORDER BY n.name`)
	if err != nil {
		return nil, fmt.Errorf("store: logs: %w", err)
	}
	defer rows.Close()
	var out []LogInfo
	for rows.Next() {
		var li LogInfo
		if err := rows.Scan(&li.Name, &li.Bytes); err != nil {
			return nil, fmt.Errorf("store: logs: %w", err)
		}
		out = append(out, li)
	}
	return out, rows.Err()
}

// Subscribe returns a channel of its own that is woken after every commit:
// at once for this process's, and within pollEvery for other processes'.
// A wake-up means "call Since"; wake-ups coalesce. cancel unsubscribes.
func (s *Store) Subscribe() (wake <-chan struct{}, cancel func()) {
	ch := make(chan struct{}, 1)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	s.pollOnce.Do(func() {
		s.wg.Add(1)
		go s.poll()
	})
	return ch, func() {
		s.subMu.Lock()
		delete(s.subs, ch)
		s.subMu.Unlock()
	}
}

func (s *Store) wake() {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// poll wakes the subscribers when another process has committed. The seq
// column is the rowid, so max(seq) is one lookup.
func (s *Store) poll() {
	defer s.wg.Done()
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		var max sql.NullInt64
		if err := s.r.QueryRow(`SELECT max(seq) FROM events`).Scan(&max); err != nil || !max.Valid {
			continue
		}
		if seq := uint64(max.Int64); seq > s.head.Load() {
			s.head.Store(seq)
			s.wake()
		}
	}
}

// TryLease takes or renews the named lease for ttl and reports whether this
// process holds it. Exactly one process holds a lease at a time: it is taken
// under the write lock, and another process can take it only once it expires.
func (s *Store) TryLease(ctx context.Context, name string, ttl time.Duration) (bool, error) {
	if len(s.boot) == 0 {
		return false, errors.New("store: lease: no boot ID in Options")
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: lease: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	var holder []byte
	var expires int64
	err = tx.QueryRowContext(ctx, `SELECT boot_id, expires_ns FROM leases WHERE name = ?`, name).Scan(&holder, &expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, fmt.Errorf("store: lease: %w", err)
	case expires > now && string(holder) != string(s.boot):
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO leases (name, boot_id, expires_ns) VALUES (?, ?, ?)`,
		name, s.boot, now+ttl.Nanoseconds()); err != nil {
		return false, fmt.Errorf("store: lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: lease: %w", err)
	}
	return true, nil
}

// Reidentify gives the database at path a new store ID. A restore calls it:
// the restored file's seq counter is back where the backup left it, so a
// cursor from before the restore could skip the rows committed after it,
// unless every cursor restarts.
func Reidentify(path string) error {
	s, err := Open(path, Options{})
	if err != nil {
		return err
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	_, err = s.w.Exec(`UPDATE meta SET value = ? WHERE key = 'store_id'`, id)
	return errors.Join(err, s.Close())
}

// commitWithoutSecurity commits a batch's other events when the store has no
// chain key: an unchained security record is never written, but it must not
// cost the rest of the batch their place. It returns ErrNoChainKey, so an
// Audit caller learns its record was refused; the text mirror has it.
func (s *Store) commitWithoutSecurity(ctx context.Context, batch []event.Event) error {
	var keep []event.Event
	var at []int
	for i := range batch {
		if batch[i].Log != SecurityLog {
			keep, at = append(keep, batch[i]), append(at, i)
		}
	}
	if err := s.Commit(ctx, keep); err != nil {
		return err
	}
	for j, i := range at {
		batch[i].Seq, batch[i].Time = keep[j].Seq, keep[j].Time
	}
	return ErrNoChainKey
}
