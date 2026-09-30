package improve

import (
	"context"
	"testing"
	"time"
)

type recordedCountJob struct{ runs int }

func (j *recordedCountJob) Name() string { return "count" }

func (j *recordedCountJob) Run(context.Context) (JobResult, error) {
	j.runs++
	return JobResult{}, nil
}

// A relaunch must not count as "never run": the clock lives in Meta.
func TestRegisterRecordedKeepsTheClockAcrossLaunches(t *testing.T) {
	ctx := context.Background()
	meta, err := OpenMeta(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	const key = "consolidate_last_run:main"
	launch := func(m *Meta) *recordedCountJob {
		t.Helper()
		s, job := NewScheduler(0), &recordedCountJob{}
		s.RegisterRecorded(ctx, job, 6*time.Hour, m, key)
		s.RunDue(ctx)
		return job
	}

	// First launch, no record: the clock starts now and is saved; nothing runs.
	if job := launch(meta); job.runs != 0 {
		t.Fatalf("first launch ran the job %d times", job.runs)
	}
	if _, ok, _ := meta.GetInt64(ctx, key); !ok {
		t.Fatal("the clock was not saved")
	}
	// A relaunch within the interval: still nothing (the in-memory clock fired here).
	if job := launch(meta); job.runs != 0 {
		t.Fatalf("relaunch ran the job %d times", job.runs)
	}
	// Once the recorded run is older than the interval, a launch runs it once
	// and stamps the new run.
	old := time.Now().Add(-7 * time.Hour).Unix()
	if err := meta.SetInt64(ctx, key, old); err != nil {
		t.Fatal(err)
	}
	s, job := NewScheduler(0), &recordedCountJob{}
	s.RegisterRecorded(ctx, job, 6*time.Hour, meta, key)
	s.RunDue(ctx)
	s.RunDue(ctx)
	if job.runs != 1 {
		t.Fatalf("an overdue job ran %d times, want 1", job.runs)
	}
	if sec, _, _ := meta.GetInt64(ctx, key); sec <= old {
		t.Error("the run was not stamped")
	}
	// Without a meta store it falls back to the plain interval.
	if job := launch(nil); job.runs != 1 {
		t.Errorf("no meta: ran %d times, want 1 (plain interval)", job.runs)
	}
}
