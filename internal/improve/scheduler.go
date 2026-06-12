package improve

import (
	"context"
	"errors"
	"sync"
	"time"

	"log/slog"
)

// Cadence-policy defaults for the background self-improvement loop
// (`tenant serve`). Distillation is cheap + cursor-based, so a modest
// interval keeps T3 facts fresh without burning summarizer calls; the
// tick is how often the loop checks whether any job is due.
const (
	DefaultDistillInterval     = 30 * time.Minute
	DefaultConsolidateInterval = 6 * time.Hour
	DefaultSchedulerTick       = 1 * time.Minute
)

// Job is one self-improvement task. v1 ships DistillJob. Future jobs
// (SoulNudgeJob: propose soul edits from recent patterns;
// SkillInductionJob: promote successful tool sequences to T4) satisfy
// this same interface and register the same way — the scheduler does
// not need to change to add them. This is the Hermes "learning loop
// is many jobs under one Crons" idea, Go-native.
type Job interface {
	// Name is a stable identifier used in logs and run history.
	Name() string
	// Run performs the job once. Returning an error does NOT stop the
	// scheduler — one failing job must not strand the others. The
	// scheduler records the error in run history.
	Run(ctx context.Context) (JobResult, error)
}

// JobResult is what a Job reports back. Changed signals whether the
// job mutated durable state (used by the scheduler to decide whether
// to surface a "self-improvement happened" notice).
type JobResult struct {
	Summary string         // one-line human-readable outcome
	Changed bool           // did this run alter durable state?
	Details map[string]any // structured detail for diagnostics
}

// JobRunRecord is one entry in the scheduler's run history. Hermes
// surfaces "what self-improvement did lately" to the user; we keep a
// bounded ring of these so the agent / a CLI can show it.
type JobRunRecord struct {
	JobName   string
	StartedAt time.Time
	Duration  time.Duration
	Result    JobResult
	Err       error
}

type scheduledJob struct {
	job      Job
	interval time.Duration
	lastRun  time.Time
	due      DueFunc // non-nil ⇒ custom due-ness (RegisterDue); interval ignored
}

// DueFunc decides whether a job is due. lastRun is the job's last completed
// run in this process — or the seed passed to RegisterDue, which lets a
// caller restore the clock from durable state across restarts. now is the
// tick time. Implementations must be cheap and side-effect free; the
// scheduler consults them on every tick.
type DueFunc func(lastRun, now time.Time) bool

// Scheduler runs registered jobs on cadences. It does NOT own a
// goroutine until Start is called — RunDue / RunAll are usable
// synchronously (e.g. from the agent loop on a turn boundary, or a
// CLI command) without a background loop.
type Scheduler struct {
	mu      sync.Mutex
	jobs    []*scheduledJob
	history []JobRunRecord
	histCap int
	log     *slog.Logger

	stop    chan struct{}
	doneWG  sync.WaitGroup
	running bool

	// OnRun, if set, is called after each job run (after history is
	// recorded). Used by the TUI to surface self-improvement in the
	// live feed. Called outside the scheduler lock; must be cheap.
	OnRun func(JobRunRecord)

	// OnStart, if set, is called just before a job begins running. Exists so
	// long jobs (the nightly eval takes minutes) can announce themselves in
	// the TUI feed instead of running as a black box between queue and
	// result (TEN-196 /eval now). Called outside the scheduler lock; must be
	// cheap.
	OnStart func(jobName string)

	// Paused, if set and returning true, suspends ALL job execution (RunDue +
	// RunAll skip without running or advancing any job's clock). Used to freeze
	// self-improvement while the model is degraded to the echo fallback —
	// otherwise consolidation/profile jobs would write echo-derived garbage to
	// durable stores and the distill cursor would advance past unprocessed
	// episodes. Must be cheap; called on every tick.
	Paused func() bool
}

// paused reports whether execution is currently suspended.
func (s *Scheduler) paused() bool { return s.Paused != nil && s.Paused() }

// NewScheduler returns an empty scheduler. historyCap bounds the
// in-memory run-history ring (<=0 → default 64).
func NewScheduler(log *slog.Logger, historyCap int) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	if historyCap <= 0 {
		historyCap = 64
	}
	return &Scheduler{log: log, histCap: historyCap}
}

// Register adds a job to run at most once per interval. interval <= 0
// means "only runs on explicit RunAll" (never auto-fires via RunDue
// or the background loop) — useful for manual-only jobs.
func (s *Scheduler) Register(job Job, interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, &scheduledJob{job: job, interval: interval})
}

// RegisterDue adds a job whose due-ness is decided by a custom predicate
// instead of a fixed uptime interval — e.g. "once per wall-clock day at
// 03:15", or an interval whose clock survives restarts. seed pre-loads
// lastRun (zero ⇒ never run), so callers can restore the clock from a
// durable record: the nightly eval seeds from trend.jsonl (TEN-196) so a
// relaunch doesn't re-fire a run that already happened today. RunAll still
// force-runs these jobs like any other.
func (s *Scheduler) RegisterDue(job Job, due DueFunc, seed time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, &scheduledJob{job: job, due: due, lastRun: seed})
}

// RunDue runs every job whose interval has elapsed since its last
// run. Jobs with interval <= 0 are skipped. Returns the records for
// the jobs that ran this call. Safe to call from any goroutine.
func (s *Scheduler) RunDue(ctx context.Context) []JobRunRecord {
	if s.paused() {
		return nil // model degraded: defer all jobs (don't run or advance clocks)
	}
	now := time.Now()
	s.mu.Lock()
	due := make([]*scheduledJob, 0, len(s.jobs))
	for _, sj := range s.jobs {
		if sj.due != nil { // custom predicate (RegisterDue) replaces interval logic
			if sj.due(sj.lastRun, now) {
				due = append(due, sj)
			}
			continue
		}
		if sj.interval <= 0 {
			continue
		}
		if sj.lastRun.IsZero() || now.Sub(sj.lastRun) >= sj.interval {
			due = append(due, sj)
		}
	}
	s.mu.Unlock()

	var out []JobRunRecord
	for _, sj := range due {
		if ctx.Err() != nil {
			break
		}
		rec := s.runOne(ctx, sj.job)
		s.mu.Lock()
		sj.lastRun = time.Now()
		s.mu.Unlock()
		out = append(out, rec)
	}
	return out
}

// RunAll force-runs every registered job regardless of interval.
// Resets each job's interval clock. Used for manual "improve now"
// triggers and tests.
func (s *Scheduler) RunAll(ctx context.Context) []JobRunRecord {
	if s.paused() {
		return nil // model degraded: even a manual "improve now" must not run on echo
	}
	s.mu.Lock()
	jobs := make([]*scheduledJob, len(s.jobs))
	copy(jobs, s.jobs)
	s.mu.Unlock()

	var out []JobRunRecord
	for _, sj := range jobs {
		if ctx.Err() != nil {
			break
		}
		rec := s.runOne(ctx, sj.job)
		s.mu.Lock()
		sj.lastRun = time.Now()
		s.mu.Unlock()
		out = append(out, rec)
	}
	return out
}

// Start launches a background goroutine that calls RunDue every tick.
// Idempotent-guarded: a second Start while running is a no-op error.
// Call Stop to shut down cleanly (waits for the loop goroutine and any
// in-flight job to finish).
func (s *Scheduler) Start(ctx context.Context, tick time.Duration) error {
	if tick <= 0 {
		return errors.New("improve: Start tick must be > 0")
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("improve: scheduler already running")
	}
	s.running = true
	s.stop = make(chan struct{})
	s.mu.Unlock()

	s.doneWG.Add(1)
	go func() {
		defer s.doneWG.Done()
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			case <-t.C:
				s.RunDue(ctx)
			}
		}
	}()
	return nil
}

// Stop signals the background loop to exit and blocks until it (and
// any job it was running) has returned. Safe to call if not started.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	close(s.stop)
	s.running = false
	s.mu.Unlock()
	s.doneWG.Wait()
}

// History returns a copy of the run-history ring, newest last.
func (s *Scheduler) History() []JobRunRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]JobRunRecord, len(s.history))
	copy(out, s.history)
	return out
}

func (s *Scheduler) runOne(ctx context.Context, job Job) JobRunRecord {
	if s.OnStart != nil {
		s.OnStart(job.Name())
	}
	start := time.Now()
	res, err := job.Run(ctx)
	rec := JobRunRecord{
		JobName:   job.Name(),
		StartedAt: start,
		Duration:  time.Since(start),
		Result:    res,
		Err:       err,
	}
	if err != nil {
		s.log.Warn("improve: job failed", "job", job.Name(), "err", err)
	} else if res.Changed {
		s.log.Info("improve: job made changes", "job", job.Name(), "summary", res.Summary)
	} else {
		s.log.Debug("improve: job ran, no changes", "job", job.Name())
	}
	s.appendHistory(rec)
	if s.OnRun != nil {
		s.OnRun(rec)
	}
	return rec
}

func (s *Scheduler) appendHistory(rec JobRunRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, rec)
	if len(s.history) > s.histCap {
		// Drop oldest. Copy to avoid unbounded backing-array growth.
		s.history = append([]JobRunRecord(nil), s.history[len(s.history)-s.histCap:]...)
	}
}
