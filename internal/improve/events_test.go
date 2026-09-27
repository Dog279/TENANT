package improve_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"tenant/internal/improve"
	"tenant/internal/logs/event"
)

// recorder is an event emitter that keeps what it is given.
type recorder struct {
	mu     sync.Mutex
	events []*event.Event
}

func (r *recorder) Emit(_ context.Context, e *event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) Audit(ctx context.Context, e *event.Event) error { r.Emit(ctx, e); return nil }

func record(t *testing.T) *recorder {
	r := &recorder{}
	event.Install(r)
	t.Cleanup(func() { event.Install(nil) })
	return r
}

func field(e *event.Event, key string) any {
	for _, f := range e.Data {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

// Each job run is an activity: start, its outcome with the runs it learned
// from, and stop; what the job logs belongs to it; a failed run is a fail
// event at error.
func TestJobRunsAreActivities(t *testing.T) {
	r := record(t)
	s := improve.NewScheduler(0)
	s.Register(&fakeJob{name: "learn", fn: func(ctx context.Context) (improve.JobResult, error) {
		improve.StepFailed.Log(ctx, "job", "learn", "step", "write", "err", "busy")
		return improve.JobResult{Summary: "learned 2 facts", Changed: true, SourceRuns: []string{"run-a", "run-b"}}, nil
	}}, 0)
	s.Register(&fakeJob{name: "broken", fn: func(context.Context) (improve.JobResult, error) {
		return improve.JobResult{}, errors.New("store gone")
	}}, 0)
	s.RunAll(context.Background())

	if len(r.events) != 6 {
		t.Fatalf("got %d events, want 6 (start, warning, outcome, stop; start, fail)", len(r.events))
	}
	start, warn, outcome, stop, bStart, bFail := r.events[0], r.events[1], r.events[2], r.events[3], r.events[4], r.events[5]
	act := start.Corr.ActivityID
	for _, e := range []*event.Event{start, warn, outcome, stop} {
		if e.Provider != "improve" || e.Corr.ActivityID != act || act == "" {
			t.Fatalf("event %s/%d is not under the run's activity %q: %+v", e.Provider, e.ID, act, e.Corr)
		}
	}
	if start.Opcode != event.OpStart || stop.Opcode != event.OpStop || start.Msg != "Self-improvement job learn" {
		t.Errorf("start %+v, stop %+v", start, stop)
	}
	if outcome.ID != 1002 || outcome.Msg != "learn: learned 2 facts" || field(outcome, "changed") != true ||
		string(field(outcome, "source_runs").(json.RawMessage)) != `["run-a","run-b"]` {
		t.Errorf("outcome: %+v", outcome)
	}
	if bStart.Corr.ActivityID == act || bFail.Opcode != event.OpFail || bFail.Level != event.Error || field(bFail, "error") != "store gone" {
		t.Errorf("the failed run: start %+v, end %+v", bStart, bFail)
	}
}
