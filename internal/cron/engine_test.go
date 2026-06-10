package cron

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock for deterministic engine tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func newTestEngine(runner Runner, persist func([]JobDef) error) (*Engine, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 6, 7, 10, 0, 0, 0, time.Local)}
	e := NewEngine(nil, runner, persist, nil)
	e.now = clk.now
	return e, clk
}

// addJob is a terse helper for the common enabled-prompt-job case.
func addJob(e *Engine, name, spec, prompt string) (Job, error) {
	return e.Add(AddSpec{Name: name, Spec: spec, Prompt: prompt, Enabled: true})
}

func TestEngineAddValidatesAndPersists(t *testing.T) {
	var saved [][]JobDef
	e, _ := newTestEngine(
		func(ctx context.Context, j Job) (string, error) { return "ok", nil },
		func(defs []JobDef) error { saved = append(saved, defs); return nil },
	)

	if _, err := addJob(e, "nightly", "0 9 * * *", "run tests"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := e.List(); len(got) != 1 || got[0].Name != "nightly" || got[0].NextRun.IsZero() {
		t.Fatalf("List after Add = %+v", got)
	}
	if len(saved) != 1 || len(saved[0]) != 1 {
		t.Fatalf("persist not called correctly: %v", saved)
	}

	// Bad spec rejected, not persisted.
	if _, err := addJob(e, "bad", "nonsense", "x"); err == nil {
		t.Error("Add with bad spec should error")
	}
	// Empty prompt rejected.
	if _, err := addJob(e, "bad", "0 9 * * *", "  "); err == nil {
		t.Error("Add with empty prompt should error")
	}
	// Bad timezone rejected.
	if _, err := e.Add(AddSpec{Spec: "0 9 * * *", Prompt: "x", TZ: "Mars/Phobos", Enabled: true}); err == nil {
		t.Error("Add with bad timezone should error")
	}
	// Unknown kind rejected.
	if _, err := e.Add(AddSpec{Spec: "@every 5m", Prompt: "x", Kind: "wat", Enabled: true}); err == nil {
		t.Error("Add with unknown kind should error")
	}
}

func TestEngineJobCap(t *testing.T) {
	e, _ := newTestEngine(func(ctx context.Context, j Job) (string, error) { return "", nil }, nil)
	e.maxJobs = 2
	if _, err := addJob(e, "a", "@every 5m", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := addJob(e, "b", "@every 5m", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := addJob(e, "c", "@every 5m", "p"); err == nil {
		t.Error("third Add should hit the cap")
	}
}

func TestEngineAddRollbackOnPersistFailure(t *testing.T) {
	e, _ := newTestEngine(
		func(ctx context.Context, j Job) (string, error) { return "", nil },
		func(defs []JobDef) error { return context.DeadlineExceeded },
	)
	if _, err := addJob(e, "a", "@every 5m", "p"); err == nil {
		t.Fatal("Add should surface persist error")
	}
	if got := e.List(); len(got) != 0 {
		t.Errorf("failed Add must roll back; List = %+v", got)
	}
}

func TestEngineRemoveAndSetEnabled(t *testing.T) {
	e, _ := newTestEngine(func(ctx context.Context, j Job) (string, error) { return "", nil }, nil)
	j, _ := addJob(e, "a", "0 9 * * *", "p")

	got, changed, err := e.SetEnabled(j.ID, false)
	if err != nil || !changed || got.Enabled || !got.NextRun.IsZero() {
		t.Fatalf("SetEnabled(false) = %+v changed=%v err=%v", got, changed, err)
	}
	if _, changed, _ := e.SetEnabled(j.ID, false); changed {
		t.Error("redundant SetEnabled should report changed=false")
	}
	got, _, _ = e.SetEnabled(j.ID, true)
	if got.NextRun.IsZero() {
		t.Error("re-enable should recompute NextRun")
	}

	if removed, _ := e.Remove(j.ID); !removed {
		t.Error("Remove should report removed")
	}
	if removed, _ := e.Remove(j.ID); removed {
		t.Error("removing a gone job should report removed=false")
	}
}

func TestEngineRunDueFiresOnce(t *testing.T) {
	var runs int32
	e, clk := newTestEngine(
		func(ctx context.Context, j Job) (string, error) { atomic.AddInt32(&runs, 1); return "done", nil },
		nil,
	)
	j, _ := addJob(e, "a", "@every 5m", "p")
	e.runDue(context.Background(), clk.now())
	if atomic.LoadInt32(&runs) != 0 {
		t.Fatal("job fired before due")
	}
	clk.set(j.NextRun.Add(time.Second))
	e.runDue(context.Background(), clk.now())
	if atomic.LoadInt32(&runs) != 1 {
		t.Fatalf("expected 1 run, got %d", runs)
	}
	if got := e.List(); got[0].LastStatus != "ok" || got[0].Runs != 1 || got[0].LastSummary != "done" {
		t.Errorf("job state after run = %+v", got[0])
	}
	e.runDue(context.Background(), clk.now())
	if atomic.LoadInt32(&runs) != 1 {
		t.Errorf("job re-fired; runs=%d", runs)
	}
}

func TestEngineNoDoubleFireWhileRunning(t *testing.T) {
	release := make(chan struct{})
	var runs int32
	e, clk := newTestEngine(
		func(ctx context.Context, j Job) (string, error) {
			atomic.AddInt32(&runs, 1)
			<-release
			return "", nil
		},
		nil,
	)
	j, _ := addJob(e, "a", "@every 5m", "p")
	clk.set(j.NextRun.Add(time.Second))

	done := make(chan struct{})
	go func() { e.runDue(context.Background(), clk.now()); close(done) }()
	time.Sleep(20 * time.Millisecond)
	e.runDue(context.Background(), clk.now())

	if got := atomic.LoadInt32(&runs); got != 1 {
		close(release)
		<-done
		t.Fatalf("double-fire: runs=%d, want 1", got)
	}
	close(release)
	<-done
}

func TestEngineStartStopCancelsInFlight(t *testing.T) {
	started := make(chan struct{})
	e, clk := newTestEngine(
		func(ctx context.Context, j Job) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		},
		nil,
	)
	e.tick = 5 * time.Millisecond
	j, _ := addJob(e, "a", "@every 5m", "p")
	clk.set(j.NextRun.Add(time.Second))

	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err == nil {
		t.Error("second Start should error")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("job never started")
	}
	done := make(chan struct{})
	go func() { e.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung on in-flight job")
	}
}

func TestEngineStopSafeWithoutStart(t *testing.T) {
	e, _ := newTestEngine(func(ctx context.Context, j Job) (string, error) { return "", nil }, nil)
	e.Stop()
}

func TestEngineNilRunnerNoPanic(t *testing.T) {
	e, _ := newTestEngine(nil, nil)
	j, _ := addJob(e, "a", "@every 5m", "p")
	rec, err := e.RunNow(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("RunNow err: %v", err)
	}
	if rec.OK || rec.Err == "" {
		t.Errorf("nil runner should record an error, got %+v", rec)
	}
}

func TestEngineRunNowAndHistory(t *testing.T) {
	e, _ := newTestEngine(func(ctx context.Context, j Job) (string, error) { return "hi", nil }, nil)
	j, _ := addJob(e, "a", "0 9 * * *", "p")
	rec, err := e.RunNow(context.Background(), j.ID)
	if err != nil || !rec.OK || rec.Summary != "hi" {
		t.Fatalf("RunNow = %+v err=%v", rec, err)
	}
	if h := e.History(); len(h) != 1 || !h[0].OK {
		t.Errorf("history = %+v", h)
	}
	if _, err := e.RunNow(context.Background(), "nope"); err == nil {
		t.Error("RunNow with unknown id should error")
	}
}

func TestEngineSeedFromDefs(t *testing.T) {
	defs := []JobDef{
		{ID: "1", Name: "good", Spec: "0 9 * * *", Prompt: "p", Enabled: true},
		{ID: "2", Name: "broken", Spec: "nonsense", Prompt: "p", Enabled: true},
		{ID: "3", Name: "badtz", Spec: "0 9 * * *", Prompt: "p", Enabled: true, TZ: "Mars/Phobos"},
	}
	e := NewEngine(defs, func(ctx context.Context, j Job) (string, error) { return "", nil }, nil, nil)
	for _, j := range e.List() {
		switch j.Name {
		case "good":
			if j.NextRun.IsZero() || !j.Enabled {
				t.Errorf("good job not scheduled: %+v", j)
			}
		case "broken", "badtz":
			if j.Enabled || j.LastStatus != "error" {
				t.Errorf("%s should be inert+error: %+v", j.Name, j)
			}
		}
	}
}

// --- extension coverage ---

// Prime backfills LastRun from persisted history and, with catch-up on, fires a
// missed SAFE job once; exec/shell jobs resume forward-from-now.
func TestEnginePrimeCatchupAndHistorySeed(t *testing.T) {
	now := time.Date(2026, 6, 7, 10, 0, 0, 0, time.Local)
	defs := []JobDef{
		{ID: "safe", Spec: "@every 1h", Prompt: "p", Enabled: true},
		{ID: "execj", Spec: "@every 1h", Prompt: "p", Enabled: true, Exec: true},
	}
	e := NewEngine(defs, func(ctx context.Context, j Job) (string, error) { return "", nil }, nil, nil)
	e.now = func() time.Time { return now }
	// Both last ran 2h ago → both are overdue by the interval.
	hist := []RunRecord{
		{JobID: "safe", StartedAt: now.Add(-2 * time.Hour), OK: true},
		{JobID: "execj", StartedAt: now.Add(-2 * time.Hour), OK: true},
	}
	e.Prime(PrimeOptions{Catchup: true, History: hist})

	jobs := map[string]Job{}
	for _, j := range e.List() {
		jobs[j.ID] = j
	}
	if jobs["safe"].LastRun.IsZero() {
		t.Error("LastRun should be backfilled from history")
	}
	if !jobs["safe"].NextRun.Equal(now) {
		t.Errorf("safe overdue job should catch up (NextRun=now); got %v", jobs["safe"].NextRun)
	}
	if !jobs["execj"].NextRun.After(now) {
		t.Errorf("exec job must NOT catch up; NextRun should be in the future, got %v", jobs["execj"].NextRun)
	}
}

// A never-run job (no LastRun) must not catch up even with catch-up enabled.
func TestEnginePrimeNeverRunNoCatchup(t *testing.T) {
	now := time.Date(2026, 6, 7, 10, 0, 0, 0, time.Local)
	e := NewEngine([]JobDef{{ID: "x", Spec: "@every 1h", Prompt: "p", Enabled: true}},
		func(ctx context.Context, j Job) (string, error) { return "", nil }, nil, nil)
	e.now = func() time.Time { return now }
	e.Prime(PrimeOptions{Catchup: true}) // no history
	if got := e.List()[0].NextRun; !got.After(now) {
		t.Errorf("never-run job must schedule forward, got %v", got)
	}
}

// History is persisted through the Prime-wired sink after each run.
func TestEngineHistoryPersist(t *testing.T) {
	var saved [][]RunRecord
	e, _ := newTestEngine(func(ctx context.Context, j Job) (string, error) { return "ok", nil }, nil)
	e.Prime(PrimeOptions{HistoryPersist: func(h []RunRecord) error {
		saved = append(saved, append([]RunRecord(nil), h...))
		return nil
	}})
	j, _ := addJob(e, "a", "0 9 * * *", "p")
	if _, err := e.RunNow(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || len(saved[0]) != 1 || saved[0][0].JobID != j.ID {
		t.Errorf("history not persisted: %v", saved)
	}
}

// While paused (model degraded), runDue defers every due job WITHOUT advancing
// its NextRun, so jobs simply run once the gate clears.
func TestEnginePausedDefers(t *testing.T) {
	var runs int32
	e, clk := newTestEngine(func(ctx context.Context, j Job) (string, error) { atomic.AddInt32(&runs, 1); return "", nil }, nil)
	paused := true
	e.SetPaused(func() bool { return paused })
	j, _ := addJob(e, "a", "@every 5m", "p")
	clk.set(j.NextRun.Add(time.Second))

	e.runDue(context.Background(), clk.now())
	if atomic.LoadInt32(&runs) != 0 {
		t.Fatal("paused engine must defer due jobs")
	}
	if got := e.List()[0].NextRun; !got.Equal(j.NextRun) {
		t.Errorf("paused runDue must NOT advance NextRun: %v vs %v", got, j.NextRun)
	}
	paused = false
	e.runDue(context.Background(), clk.now())
	if atomic.LoadInt32(&runs) != 1 {
		t.Fatalf("unpaused should run; runs=%d", runs)
	}
}

// Prime must force catch-up OFF while paused, so an overdue job does not fire at
// boot against a degraded model.
func TestEnginePrimeCatchupOffWhilePaused(t *testing.T) {
	now := time.Date(2026, 6, 7, 10, 0, 0, 0, time.Local)
	e := NewEngine([]JobDef{{ID: "x", Spec: "@every 1h", Prompt: "p", Enabled: true}},
		func(ctx context.Context, j Job) (string, error) { return "", nil }, nil, nil)
	e.now = func() time.Time { return now }
	e.SetPaused(func() bool { return true })
	hist := []RunRecord{{JobID: "x", StartedAt: now.Add(-2 * time.Hour), OK: true}}
	e.Prime(PrimeOptions{Catchup: true, History: hist})
	if got := e.List()[0].NextRun; !got.After(now) {
		t.Errorf("paused Prime must force catch-up off; NextRun=%v, want future", got)
	}
}

// A per-job timezone is respected when computing the next fire time: a "09:00
// daily" job in New York must land at 09:00 New-York time.
func TestEngineTimezone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	e := NewEngine(nil, func(ctx context.Context, j Job) (string, error) { return "", nil }, nil, nil)
	base := time.Date(2026, 6, 7, 0, 0, 0, 0, ny) // midnight in NY
	e.now = func() time.Time { return base }
	job, err := e.Add(AddSpec{Spec: "0 9 * * *", Prompt: "p", TZ: "America/New_York", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	got := job.NextRun.In(ny)
	if got.Hour() != 9 || got.Minute() != 0 {
		t.Errorf("NextRun in NY = %v, want 09:00", got)
	}
}
