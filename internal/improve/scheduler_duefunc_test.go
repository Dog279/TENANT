package improve

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A RegisterDue job fires exactly when its predicate says so, and the
// scheduler advances lastRun after each run — so a "fire once then wait"
// predicate (interval semantics) cannot double-fire within the window.
func TestRegisterDue_PredicateControlsFiring(t *testing.T) {
	var n int32
	s := NewScheduler(nil, 0)
	day := 24 * time.Hour
	s.RegisterDue(countJob{&n}, func(lastRun, now time.Time) bool {
		return lastRun.IsZero() || now.Sub(lastRun) >= day
	}, time.Time{}) // zero seed: never run → due at first check

	s.RunDue(context.Background())
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("first RunDue: ran %d times, want 1 (zero seed ⇒ due)", got)
	}
	// Immediately after the run, lastRun is fresh — must NOT fire again.
	s.RunDue(context.Background())
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("second RunDue: ran %d times, want 1 (fresh lastRun ⇒ not due)", got)
	}
}

// The TEN-196 laptop scenario: the seed restored from a durable record
// (trend.jsonl) decides launch-time due-ness. A fresh seed means a relaunch
// does NOT re-fire; a stale seed fires exactly once and then stands down.
func TestRegisterDue_SeedSurvivesRelaunch(t *testing.T) {
	day := 24 * time.Hour
	due := func(lastRun, now time.Time) bool {
		return lastRun.IsZero() || now.Sub(lastRun) >= day
	}

	// Relaunch with a run already recorded an hour ago: not due. This is the
	// "30 rebuilds in one dev day" case — launches 2..30 must stay silent.
	var fresh int32
	s := NewScheduler(nil, 0)
	s.RegisterDue(countJob{&fresh}, due, time.Now().Add(-time.Hour))
	s.RunDue(context.Background())
	s.RunDue(context.Background())
	if got := atomic.LoadInt32(&fresh); got != 0 {
		t.Fatalf("fresh seed: ran %d times, want 0", got)
	}

	// First launch of the next day (seed 25h old): due exactly once.
	var stale int32
	s2 := NewScheduler(nil, 0)
	s2.RegisterDue(countJob{&stale}, due, time.Now().Add(-25*time.Hour))
	s2.RunDue(context.Background())
	s2.RunDue(context.Background())
	if got := atomic.LoadInt32(&stale); got != 1 {
		t.Fatalf("stale seed: ran %d times, want exactly 1", got)
	}
}

// RunAll force-runs predicate jobs like any other — the manual "improve now"
// path must not skip them even when the predicate says not-due.
func TestRegisterDue_RunAllForcesRun(t *testing.T) {
	var n int32
	s := NewScheduler(nil, 0)
	s.RegisterDue(countJob{&n}, func(time.Time, time.Time) bool { return false }, time.Now())
	s.RunAll(context.Background())
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("RunAll: ran %d times, want 1 (force-run ignores predicate)", got)
	}
}

// Interval-registered jobs are untouched by the DueFunc addition: zero
// lastRun still means due at first check (existing semantics preserved).
func TestRegister_IntervalSemanticsUnchanged(t *testing.T) {
	var n int32
	s := NewScheduler(nil, 0)
	s.Register(countJob{&n}, time.Hour)
	s.RunDue(context.Background())
	s.RunDue(context.Background())
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("interval job: ran %d times, want 1 (due at first check, then waits)", got)
	}
}
