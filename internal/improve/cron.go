package improve

// cron.go is a small INTERVAL scheduler persisted in SQLite. Despite the "cron"
// name, schedules are fixed time.Durations (e.g. every 30m), not crontab
// expressions — each run sets NextRun = now + Schedule. It mirrors the package's
// other stores (open, CRUD, list-due); a CronEntry can be wrapped in a CronJob
// to satisfy the scheduler's Job interface.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// cronSchema is the table for persisted cron job definitions.
const cronSchema = `
CREATE TABLE IF NOT EXISTS cron_jobs (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    schedule    TEXT NOT NULL,
    enabled     INTEGER NOT NULL,
    last_run    INTEGER,
    next_run    INTEGER NOT NULL,
    command     TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);`

// ErrCronNotFound signals a cron entry id doesn't exist.
var ErrCronNotFound = errors.New("improve: cron: not found")

// CronEntry is one persisted job definition. Schedule is a fixed interval
// (NextRun = run time + Schedule), not a crontab expression. Times are persisted
// at one-second granularity.
type CronEntry struct {
	ID        string
	Name      string
	Schedule  time.Duration // fixed interval between runs; must be > 0
	Enabled   bool
	LastRun   *time.Time
	NextRun   time.Time
	Command   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CronStore persists job definitions in a SQLite table. The zero value is not
// usable — call OpenCronStore. Safe for concurrent use (delegates to *sql.DB).
type CronStore struct {
	db *sql.DB
}

// OpenCronStore opens (or creates) the cron table at path. Use ":memory:" for an
// ephemeral store (e.g. tests).
func OpenCronStore(path string) (*CronStore, error) {
	dsn := path
	if path != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)", path)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("improve: open cron store: %w", err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if _, err := db.ExecContext(context.Background(), cronSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("improve: init cron schema: %w", err)
	}
	return &CronStore{db: db}, nil
}

// Close closes the store. Idempotent.
func (cs *CronStore) Close() error {
	if cs.db == nil {
		return nil
	}
	err := cs.db.Close()
	cs.db = nil
	return err
}

// Create inserts a new entry. The caller sets ID; Name, Command and a positive
// Schedule are required (each returns a descriptive error if missing/invalid).
// Newly created jobs are always Enabled, with NextRun = now + Schedule. To stage
// a paused job, Create then Update it with Enabled=false.
func (cs *CronStore) Create(entry *CronEntry) error {
	if entry == nil {
		return fmt.Errorf("improve: cron create: nil entry")
	}
	if entry.ID == "" {
		return fmt.Errorf("improve: cron create: empty ID")
	}
	if entry.Name == "" {
		return fmt.Errorf("improve: cron create: empty Name")
	}
	if entry.Command == "" {
		return fmt.Errorf("improve: cron create: empty Command")
	}
	if entry.Schedule <= 0 {
		return fmt.Errorf("improve: cron create: schedule must be > 0, got %v", entry.Schedule)
	}
	now := time.Now()
	entry.CreatedAt = now
	entry.UpdatedAt = now
	entry.NextRun = now.Add(entry.Schedule)
	entry.Enabled = true

	_, err := cs.db.ExecContext(context.Background(), `
        INSERT INTO cron_jobs
            (id, name, schedule, enabled, last_run, next_run, command, created_at, updated_at)
        VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?)
    `, entry.ID, entry.Name, entry.Schedule.String(),
		boolInt(entry.Enabled),
		unixepoch(entry.NextRun),
		entry.Command,
		unixepoch(entry.CreatedAt), unixepoch(entry.UpdatedAt))
	if err != nil {
		return fmt.Errorf("improve: cron create: %w", err)
	}
	return nil
}

// Update replaces the mutable fields of the entry matched by ID and bumps
// updated_at. Returns ErrCronNotFound if no row has that id.
func (cs *CronStore) Update(entry *CronEntry) error {
	if entry == nil {
		return fmt.Errorf("improve: cron update: nil entry")
	}
	if entry.ID == "" {
		return fmt.Errorf("improve: cron update: empty ID")
	}
	now := unixepoch(time.Now())
	res, err := cs.db.ExecContext(context.Background(), `
        UPDATE cron_jobs
           SET name = ?, schedule = ?, enabled = ?, last_run = ?,
               next_run = ?, command = ?, updated_at = ?
         WHERE id = ?
    `, entry.Name, entry.Schedule.String(),
		boolInt(entry.Enabled),
		nullUnixepoch(entry.LastRun),
		unixepoch(entry.NextRun),
		entry.Command, now, entry.ID)
	if err != nil {
		return fmt.Errorf("improve: cron update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("improve: cron update rows: %w", err)
	}
	if n == 0 {
		return ErrCronNotFound
	}
	return nil
}

// Delete removes an entry by id. Deleting a missing id is a no-op (nil error).
func (cs *CronStore) Delete(id string) error {
	if id == "" {
		return fmt.Errorf("improve: cron delete: empty id")
	}
	_, err := cs.db.ExecContext(context.Background(),
		"DELETE FROM cron_jobs WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("improve: cron delete: %w", err)
	}
	return nil
}

// cronSelectCols is the canonical column order shared by Get/List/ListDue and
// decoded by scanEntries.
const cronSelectCols = `id, name, schedule, enabled, last_run, next_run, command, created_at, updated_at`

// Get returns the entry for id, or ErrCronNotFound.
func (cs *CronStore) Get(id string) (*CronEntry, error) {
	if id == "" {
		return nil, fmt.Errorf("improve: cron get: empty id")
	}
	rows, err := cs.db.QueryContext(context.Background(),
		"SELECT "+cronSelectCols+" FROM cron_jobs WHERE id = ?", id)
	if err != nil {
		return nil, fmt.Errorf("improve: cron get %q: %w", id, err)
	}
	defer rows.Close()
	out, err := scanEntries(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrCronNotFound
	}
	return out[0], nil
}

// List returns all entries, newest first by updated_at.
func (cs *CronStore) List() ([]*CronEntry, error) {
	rows, err := cs.db.QueryContext(context.Background(),
		"SELECT "+cronSelectCols+" FROM cron_jobs ORDER BY updated_at DESC")
	if err != nil {
		return nil, fmt.Errorf("improve: cron list: %w", err)
	}
	defer rows.Close()
	return scanEntries(rows)
}

// ListDue returns enabled entries whose NextRun has passed (now >= next_run),
// ordered by next_run ASC. Called by the scheduler each tick.
func (cs *CronStore) ListDue() ([]*CronEntry, error) {
	rows, err := cs.db.QueryContext(context.Background(),
		"SELECT "+cronSelectCols+" FROM cron_jobs WHERE enabled = 1 AND next_run <= unixepoch() ORDER BY next_run ASC")
	if err != nil {
		return nil, fmt.Errorf("improve: cron list due: %w", err)
	}
	defer rows.Close()
	return scanEntries(rows)
}

// scanEntries decodes a row set (in cronSelectCols order) into CronEntries.
func scanEntries(rows *sql.Rows) ([]*CronEntry, error) {
	var out []*CronEntry
	for rows.Next() {
		var id, name, schedule, command string
		var enabled int
		var lastRun *int64
		var nextRun, createdAt, updatedAt int64
		if err := rows.Scan(
			&id, &name, &schedule, &enabled,
			&lastRun, &nextRun, &command,
			&createdAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("improve: cron scan: %w", err)
		}
		e := &CronEntry{
			ID:        id,
			Name:      name,
			Schedule:  parseDuration(schedule),
			Enabled:   enabled == 1,
			NextRun:   time.Unix(nextRun, 0),
			Command:   command,
			CreatedAt: time.Unix(createdAt, 0),
			UpdatedAt: time.Unix(updatedAt, 0),
		}
		if lastRun != nil {
			t := time.Unix(*lastRun, 0)
			e.LastRun = &t
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("improve: cron rows: %w", err)
	}
	return out, nil
}

// CronJob adapts a CronEntry to the scheduler's Job interface. On Run it
// executes fn, then advances LastRun/NextRun in the store.
type CronJob struct {
	entry *CronEntry
	store *CronStore
	fn    func(ctx context.Context, entry *CronEntry) (JobResult, error)
}

// NewCronJob creates a Job backed by a cron entry. fn performs the actual work
// (e.g. feeding entry.Command to the agent).
func NewCronJob(entry *CronEntry, store *CronStore,
	fn func(ctx context.Context, entry *CronEntry) (JobResult, error),
) *CronJob {
	return &CronJob{entry: entry, store: store, fn: fn}
}

// Name implements Job.
func (j *CronJob) Name() string { return "cron:" + j.entry.ID }

// Run executes the callback and ALWAYS advances LastRun/NextRun — whether fn
// succeeded or failed — so a persistently-failing job retries on its next
// interval instead of hot-looping every tick. The fn error is returned to the
// caller; a store-persist error is non-fatal and surfaced under
// Details["cron_update_error"].
func (j *CronJob) Run(ctx context.Context) (JobResult, error) {
	res, runErr := j.fn(ctx, j.entry)

	now := time.Now()
	j.entry.LastRun = &now
	j.entry.NextRun = now.Add(j.entry.Schedule)
	if j.store != nil {
		if err := j.store.Update(j.entry); err != nil {
			if res.Details == nil {
				res.Details = map[string]any{}
			}
			res.Details["cron_update_error"] = err.Error()
		}
	}
	return res, runErr
}

// helpers

func unixepoch(t time.Time) int64 { return t.Unix() }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullUnixepoch(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	v := t.Unix()
	return &v
}

// parseDuration is the inverse of Duration.String for stored schedules. A
// corrupt value (only reachable via external DB tampering) yields 0.
func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}
