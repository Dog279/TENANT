package improve

import (
	"context"
	"time"
)

// recordedJob stamps the time each run starts in Meta under key, so the job's
// interval can be restored at the next launch (RegisterRecorded). Stamping the
// start, not the end, means a run cut short by a restart still counts: a heavy
// job isn't retried on every relaunch.
type recordedJob struct {
	Job
	meta *Meta
	key  string
}

func (r recordedJob) Run(ctx context.Context) (JobResult, error) {
	_ = r.meta.SetInt64(ctx, r.key, time.Now().Unix())
	return r.Job.Run(ctx)
}

// RegisterRecorded runs job every interval with its clock kept in meta under
// key. Register's clock lives in memory, so every launch counts as "never run"
// and fires each interval job at the first tick; a relaunch-heavy day then
// runs a heavy job like consolidation at every launch. With no record yet, the
// clock starts now and is saved, so the first run comes one interval later.
// A nil meta falls back to Register.
func (s *Scheduler) RegisterRecorded(ctx context.Context, job Job, interval time.Duration, meta *Meta, key string) {
	if meta == nil {
		s.Register(job, interval)
		return
	}
	seed := time.Now()
	if sec, ok, err := meta.GetInt64(ctx, key); err == nil && ok {
		seed = time.Unix(sec, 0)
	} else if err == nil {
		_ = meta.SetInt64(ctx, key, seed.Unix())
	}
	s.RegisterDue(recordedJob{Job: job, meta: meta, key: key}, func(last, now time.Time) bool {
		return now.Sub(last) >= interval
	}, seed)
}
