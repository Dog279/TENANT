package cron

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Defaults for a new Engine. The tick is how often the loop checks for due
// jobs; per-run is the wall-clock ceiling on a single job (an agent turn), so a
// hung job can't block shutdown or the queue forever; maxJobs caps the total so
// a confused agent (or operator) can't create a swarm of recurring turns.
const (
	defaultTick    = 30 * time.Second
	defaultPerRun  = 5 * time.Minute
	defaultMaxJobs = 50
	defaultHistory = 64
)

// Job kinds. "" is treated as KindPrompt for backward compatibility with config
// written before the kind field existed.
const (
	KindPrompt = "prompt"
	KindShell  = "shell"
)

// JobDef is the persisted definition of a job — the only part written to
// config.json. Runtime state (next/last run, status) lives in memory and run
// history is persisted separately. New fields are omitempty so old config loads
// with zero values = prompt / non-exec / engine-default-timezone.
type JobDef struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Spec    string `json:"spec"`
	Prompt  string `json:"prompt"`
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind,omitempty"` // "" / "prompt" | "shell"
	Exec    bool   `json:"exec,omitempty"` // prompt jobs: run with the exec (dangerous) tool surface
	TZ      string `json:"tz,omitempty"`   // IANA zone (e.g. "America/Los_Angeles"); "" = engine default
}

// KindOf returns the effective kind ("" => prompt).
func (d JobDef) KindOf() string {
	if d.Kind == "" {
		return KindPrompt
	}
	return d.Kind
}

// Job is a job's full state: its definition plus runtime fields. Values returned
// by List/Add are copies — mutating them does not affect the engine.
type Job struct {
	JobDef
	NextRun     time.Time // zero = not scheduled (disabled or impossible spec)
	LastRun     time.Time // zero = never run
	LastStatus  string    // "", "ok", "error"
	LastError   string
	LastSummary string
	Runs        int

	schedule *Schedule
	loc      *time.Location // resolved from TZ; nil = use engine default
}

// AddSpec is the input to Add. Using a struct (not positional args) keeps the
// call sites readable as fields are added.
type AddSpec struct {
	Name    string
	Spec    string
	Prompt  string
	Enabled bool
	Kind    string // "" / "prompt" | "shell"
	Exec    bool
	TZ      string
}

// Runner executes one job (an agent turn, or a shell command) and returns a
// short human-readable summary. A nil Runner makes the engine inert (runs report
// an error instead of panicking).
type Runner func(ctx context.Context, job Job) (summary string, err error)

// RunRecord is one entry in the engine's bounded run history (persisted across
// restarts when a history store is wired via Prime).
type RunRecord struct {
	JobID     string    `json:"job_id"`
	Name      string    `json:"name,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Duration  int64     `json:"duration_ms"`
	Summary   string    `json:"summary,omitempty"`
	Err       string    `json:"err,omitempty"`
	OK        bool      `json:"ok"`
}

// PrimeOptions configures the engine once after construction, before Start:
// the default timezone, missed-run catch-up, and the persisted history seed +
// sink. Prime recomputes every job's NextRun in one pass.
type PrimeOptions struct {
	Location       *time.Location
	Catchup        bool
	History        []RunRecord
	HistoryPersist func([]RunRecord) error
}

// Engine schedules and runs cron jobs. It mirrors improve.Scheduler's lifecycle
// (Start launches one goroutine; Stop cancels any in-flight run and drains).
// All job runs — whether fired by the loop or by RunNow — are serialized through
// runMu, because they share underlying runner agents whose working sets are not
// safe for concurrent turns.
type Engine struct {
	mu      sync.Mutex
	jobs    []*Job
	history []RunRecord

	runner         Runner
	persist        func([]JobDef) error
	historyPersist func([]RunRecord) error
	now            func() time.Time
	log            *slog.Logger
	notify         func(string) // optional feed sink for run results (non-blocking)

	defaultLoc *time.Location // default timezone for jobs without an explicit TZ
	catchup    bool           // fire a missed safe job once on startup
	paused     func() bool    // when true, defer all runs (e.g. model degraded to echo)

	tick    time.Duration
	perRun  time.Duration
	maxJobs int
	histCap int

	runMu sync.Mutex // serializes every call into runner

	running    bool
	loopCancel context.CancelFunc
	wg         sync.WaitGroup
}

// NewEngine builds an engine seeded from persisted defs. persist (may be nil) is
// called whenever the set of definitions changes. log may be nil. The default
// timezone is the server local zone until overridden by Prime.
func NewEngine(defs []JobDef, runner Runner, persist func([]JobDef) error, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	e := &Engine{
		runner: runner, persist: persist, now: time.Now, log: log, defaultLoc: time.Local,
		tick: defaultTick, perRun: defaultPerRun, maxJobs: defaultMaxJobs, histCap: defaultHistory,
	}
	for _, d := range defs {
		j := &Job{JobDef: d}
		if sched, err := Parse(d.Spec); err != nil {
			// A persisted spec that no longer parses is kept but inert, with the
			// error visible, rather than dropped silently.
			j.LastStatus = "error"
			j.LastError = "invalid schedule: " + err.Error()
			j.Enabled = false
		} else {
			j.schedule = sched
			if tz := strings.TrimSpace(d.TZ); tz != "" {
				if loc, lerr := time.LoadLocation(tz); lerr != nil {
					j.LastStatus = "error"
					j.LastError = "invalid timezone: " + lerr.Error()
					j.Enabled = false
				} else {
					j.loc = loc
				}
			}
		}
		e.jobs = append(e.jobs, j)
	}
	// Initial scheduling (forward-from-now; catch-up is applied later in Prime
	// once history — and thus LastRun — is seeded). No lock needed: construction
	// is single-threaded.
	e.scheduleAllLocked(e.now())
	return e
}

// Prime applies post-construction configuration (timezone, catch-up, persisted
// history) and recomputes scheduling in one pass. Call once before Start.
func (e *Engine) Prime(o PrimeOptions) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if o.Location != nil {
		e.defaultLoc = o.Location
	}
	// While degraded (echo), force catch-up OFF so missed jobs don't fire at boot
	// — before the operator has even seen the banner — against a fake model.
	e.catchup = o.Catchup && !(e.paused != nil && e.paused())
	e.historyPersist = o.HistoryPersist
	if len(o.History) > 0 {
		h := o.History
		if len(h) > e.histCap {
			h = h[len(h)-e.histCap:]
		}
		e.history = append([]RunRecord(nil), h...)
		// Backfill each job's LastRun/LastStatus from its most recent record so
		// catch-up and the UI reflect pre-restart state.
		runs := map[string]int{}
		latest := map[string]RunRecord{}
		for _, r := range e.history {
			latest[r.JobID] = r
			runs[r.JobID]++
		}
		for _, j := range e.jobs {
			if r, ok := latest[j.ID]; ok {
				j.LastRun = r.StartedAt
				j.LastSummary = r.Summary
				j.Runs = runs[j.ID]
				if r.OK {
					j.LastStatus = "ok"
				} else {
					j.LastStatus = "error"
					j.LastError = r.Err
				}
			}
		}
	}
	e.scheduleAllLocked(e.now())
}

// SetNotify installs an optional sink for one-line run-result notices.
func (e *Engine) SetNotify(fn func(string)) {
	e.mu.Lock()
	e.notify = fn
	e.mu.Unlock()
}

// SetPaused installs a predicate that, while true, defers ALL job execution:
// runDue selects nothing and advances no schedule state, so jobs simply run once
// the predicate clears. Used to suppress cron while the model is degraded to the
// echo fallback (a scheduled agent turn would otherwise act on a fake plan, and
// an exec/shell job could run real side effects). Call before Start.
func (e *Engine) SetPaused(fn func() bool) {
	e.mu.Lock()
	e.paused = fn
	e.mu.Unlock()
}

func (e *Engine) isPaused() bool {
	e.mu.Lock()
	fn := e.paused
	e.mu.Unlock()
	return fn != nil && fn()
}

// effLoc returns a job's effective location (its own TZ, else the engine default).
func (e *Engine) effLoc(j *Job) *time.Location {
	if j.loc != nil {
		return j.loc
	}
	if e.defaultLoc != nil {
		return e.defaultLoc
	}
	return time.Local
}

// nextFor computes the next fire time strictly after `after`, in the job's
// effective location. The single source of truth for scheduling — every site
// that advances a job calls this so timezone handling can't drift.
func (e *Engine) nextFor(j *Job, after time.Time) time.Time {
	if j.schedule == nil {
		return time.Time{}
	}
	return j.schedule.Next(after.In(e.effLoc(j)))
}

// scheduleJobLocked (re)computes one job's NextRun, applying catch-up for safe
// (non-exec, non-shell) jobs that missed a cycle while the process was down.
func (e *Engine) scheduleJobLocked(j *Job, now time.Time) {
	if !j.Enabled || j.schedule == nil {
		j.NextRun = time.Time{}
		return
	}
	// Catch-up: only for previously-run SAFE jobs. exec/shell jobs resume
	// forward-from-now so a destructive job never fires unexpectedly on restart.
	if e.catchup && !j.LastRun.IsZero() && j.KindOf() == KindPrompt && !j.Exec {
		if cand := e.nextFor(j, j.LastRun); !cand.IsZero() && !cand.After(now) {
			j.NextRun = now // due while down → fire once on the next tick
			return
		}
	}
	j.NextRun = e.nextFor(j, now)
}

func (e *Engine) scheduleAllLocked(now time.Time) {
	for _, j := range e.jobs {
		e.scheduleJobLocked(j, now)
	}
}

// Has reports whether a job with the given id exists.
func (e *Engine) Has(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.indexLocked(id) >= 0
}

// History returns a copy of the run-history ring, newest last.
func (e *Engine) History() []RunRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]RunRecord, len(e.history))
	copy(out, e.history)
	return out
}

// List returns a snapshot of all jobs, sorted by name then id (copies).
func (e *Engine) List() []Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Job, 0, len(e.jobs))
	for _, j := range e.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].Name != out[k].Name {
			return out[i].Name < out[k].Name
		}
		return out[i].ID < out[k].ID
	})
	return out
}

// Add validates the spec/timezone, enforces the job cap, persists, and returns
// the new job.
func (e *Engine) Add(spec AddSpec) (Job, error) {
	name := strings.TrimSpace(spec.Name)
	prompt := strings.TrimSpace(spec.Prompt)
	if prompt == "" {
		return Job{}, fmt.Errorf("cron: job prompt/command is required")
	}
	kind := spec.Kind
	if kind == "" {
		kind = KindPrompt
	}
	if kind != KindPrompt && kind != KindShell {
		return Job{}, fmt.Errorf("cron: unknown kind %q (want prompt or shell)", kind)
	}
	sched, err := Parse(spec.Spec)
	if err != nil {
		return Job{}, err
	}
	var loc *time.Location
	tz := strings.TrimSpace(spec.TZ)
	if tz != "" {
		loc, err = time.LoadLocation(tz)
		if err != nil {
			return Job{}, fmt.Errorf("cron: bad timezone %q: %w", tz, err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.jobs) >= e.maxJobs {
		return Job{}, fmt.Errorf("cron: job limit reached (%d) — remove one before adding another", e.maxJobs)
	}
	j := &Job{
		JobDef: JobDef{
			ID: newID(), Name: name, Spec: sched.String(), Prompt: prompt,
			Enabled: spec.Enabled, Kind: kind, Exec: spec.Exec, TZ: tz,
		},
		schedule: sched, loc: loc,
	}
	e.scheduleJobLocked(j, e.now())
	e.jobs = append(e.jobs, j)
	if err := e.persistLocked(); err != nil {
		e.jobs = e.jobs[:len(e.jobs)-1]
		return Job{}, err
	}
	return *j, nil
}

// Remove deletes a job by id. removed is false (no error) when no such job.
func (e *Engine) Remove(id string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	idx := e.indexLocked(id)
	if idx < 0 {
		return false, nil
	}
	removed := e.jobs[idx]
	e.jobs = append(e.jobs[:idx], e.jobs[idx+1:]...)
	if err := e.persistLocked(); err != nil {
		e.jobs = append(e.jobs, nil)
		copy(e.jobs[idx+1:], e.jobs[idx:])
		e.jobs[idx] = removed
		return false, err
	}
	return true, nil
}

// SetEnabled toggles a job. Enabling (re)computes NextRun. changed is false (no
// error) when the job already had that state or doesn't exist.
func (e *Engine) SetEnabled(id string, on bool) (Job, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	idx := e.indexLocked(id)
	if idx < 0 {
		return Job{}, false, nil
	}
	j := e.jobs[idx]
	if j.Enabled == on {
		return *j, false, nil
	}
	prevEnabled, prevNext := j.Enabled, j.NextRun
	j.Enabled = on
	e.scheduleJobLocked(j, e.now())
	if err := e.persistLocked(); err != nil {
		j.Enabled, j.NextRun = prevEnabled, prevNext
		return Job{}, false, err
	}
	return *j, true, nil
}

// RunNow runs a job immediately (serialized with the scheduler loop) and returns
// its run record. ctx bounds the run; the per-run timeout still applies.
func (e *Engine) RunNow(ctx context.Context, id string) (RunRecord, error) {
	e.mu.Lock()
	idx := e.indexLocked(id)
	if idx < 0 {
		e.mu.Unlock()
		return RunRecord{}, fmt.Errorf("cron: no job with id %q", id)
	}
	job := *e.jobs[idx]
	e.mu.Unlock()
	return e.execute(ctx, job), nil
}

// runDue runs every enabled job whose NextRun has passed as of `now`. NextRun is
// advanced BEFORE the (possibly long) run so a slow job is never re-selected by
// the next tick (no double-fire).
func (e *Engine) runDue(ctx context.Context, now time.Time) []RunRecord {
	if e.isPaused() {
		return nil // model degraded: defer every due job (no select, no advance)
	}
	e.mu.Lock()
	var due []Job
	for _, j := range e.jobs {
		if !j.Enabled || j.schedule == nil || j.NextRun.IsZero() {
			continue
		}
		if !now.Before(j.NextRun) { // now >= NextRun
			due = append(due, *j)
			j.NextRun = e.nextFor(j, now) // advance first
		}
	}
	e.mu.Unlock()

	var out []RunRecord
	for _, job := range due {
		if ctx.Err() != nil {
			break
		}
		out = append(out, e.execute(ctx, job))
	}
	return out
}

// execute runs one job through the serialized runner with the per-run timeout,
// records the outcome on the live job, appends + persists history, and notifies.
func (e *Engine) execute(ctx context.Context, job Job) RunRecord {
	e.runMu.Lock()
	defer e.runMu.Unlock()

	start := e.now()
	rec := RunRecord{JobID: job.ID, Name: job.Name, StartedAt: start}

	var (
		summary string
		err     error
	)
	if e.runner == nil {
		err = fmt.Errorf("cron: no runner configured")
	} else {
		runCtx := ctx
		var cancel context.CancelFunc
		if e.perRun > 0 {
			runCtx, cancel = context.WithTimeout(ctx, e.perRun)
		}
		summary, err = e.runner(runCtx, job)
		if cancel != nil {
			cancel()
		}
	}

	rec.Duration = e.now().Sub(start).Milliseconds()
	rec.Summary = summary
	rec.OK = err == nil
	if err != nil {
		rec.Err = err.Error()
	}

	e.mu.Lock()
	if idx := e.indexLocked(job.ID); idx >= 0 {
		j := e.jobs[idx]
		j.LastRun = start
		j.Runs++
		j.LastSummary = summary
		if err != nil {
			j.LastStatus = "error"
			j.LastError = err.Error()
		} else {
			j.LastStatus = "ok"
			j.LastError = ""
		}
	}
	e.history = append(e.history, rec)
	if len(e.history) > e.histCap {
		e.history = append([]RunRecord(nil), e.history[len(e.history)-e.histCap:]...)
	}
	notify := e.notify
	persist := e.historyPersist
	var snapshot []RunRecord
	if persist != nil {
		snapshot = append([]RunRecord(nil), e.history...)
	}
	e.mu.Unlock()

	if persist != nil {
		if perr := persist(snapshot); perr != nil {
			e.log.Warn("cron: persist history", "err", perr)
		}
	}
	if notify != nil {
		notify(formatRunLine(rec))
	}
	if err != nil {
		e.log.Warn("cron: job failed", "id", job.ID, "name", job.Name, "err", err)
	} else {
		e.log.Debug("cron: job ran", "id", job.ID, "name", job.Name)
	}
	return rec
}

// Start launches the background loop. Idempotent-guarded: a second Start while
// running returns an error. The loop fires runDue every tick until ctx is
// cancelled or Stop is called.
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return fmt.Errorf("cron: engine already running")
	}
	loopCtx, cancel := context.WithCancel(ctx)
	e.running = true
	e.loopCancel = cancel
	tick := e.tick
	e.mu.Unlock()

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-t.C:
				e.runDue(loopCtx, e.now())
			}
		}
	}()
	return nil
}

// Stop cancels any in-flight run (via the loop context) and blocks until the
// loop goroutine has returned. Safe to call if never started.
func (e *Engine) Stop() {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return
	}
	e.running = false
	cancel := e.loopCancel
	e.loopCancel = nil
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	e.wg.Wait()
}

func (e *Engine) persistLocked() error {
	if e.persist == nil {
		return nil
	}
	defs := make([]JobDef, len(e.jobs))
	for i, j := range e.jobs {
		defs[i] = j.JobDef
	}
	return e.persist(defs)
}

func (e *Engine) indexLocked(id string) int {
	for i, j := range e.jobs {
		if j.ID == id {
			return i
		}
	}
	return -1
}

func formatRunLine(r RunRecord) string {
	label := r.Name
	if label == "" {
		label = r.JobID
	}
	if r.OK {
		return fmt.Sprintf("cron: ran %q (%dms)", label, r.Duration)
	}
	return fmt.Sprintf("cron: job %q failed: %s", label, r.Err)
}

// newID returns a short random hex id for a job.
func newID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
