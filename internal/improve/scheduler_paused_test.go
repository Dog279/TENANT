package improve

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type countJob struct{ n *int32 }

func (j countJob) Name() string { return "count" }
func (j countJob) Run(ctx context.Context) (JobResult, error) {
	atomic.AddInt32(j.n, 1)
	return JobResult{}, nil
}

// While Paused returns true, RunDue and RunAll must not execute any job (so a
// degraded/echo model can't drive self-improvement into durable stores).
func TestSchedulerPausedSuppresses(t *testing.T) {
	var n int32
	s := NewScheduler(nil, 0)
	s.Register(countJob{&n}, time.Millisecond) // due immediately (lastRun zero)

	paused := true
	s.Paused = func() bool { return paused }

	s.RunDue(context.Background())
	if got := atomic.LoadInt32(&n); got != 0 {
		t.Fatalf("paused RunDue ran jobs: n=%d", got)
	}
	s.RunAll(context.Background())
	if got := atomic.LoadInt32(&n); got != 0 {
		t.Fatalf("paused RunAll ran jobs: n=%d", got)
	}

	paused = false
	s.RunDue(context.Background())
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("unpaused RunDue should run once: n=%d", got)
	}
}
