package improve_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"tenant/internal/improve"
	"tenant/internal/logs/event"
	"tenant/internal/logs/event/eventtest"
)

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
	r := eventtest.Capture(t)
	s := improve.NewScheduler()
	s.Register(&fakeJob{name: "learn", fn: func(ctx context.Context) (improve.JobResult, error) {
		improve.StepFailed.Log(ctx, "job", "learn", "step", "write", "err", "busy")
		return improve.JobResult{Summary: "learned 2 facts", Changed: true, SourceRuns: []string{"run-a", "run-b"}}, nil
	}}, 0)
	s.Register(&fakeJob{name: "broken", fn: func(context.Context) (improve.JobResult, error) {
		return improve.JobResult{}, errors.New("store gone")
	}}, 0)
	s.RunAll(context.Background())

	evs := r.Events()
	if len(evs) != 6 {
		t.Fatalf("got %d events, want 6 (start, warning, outcome, stop; start, fail)", len(evs))
	}
	start, warn, outcome, stop, bStart, bFail := evs[0], evs[1], evs[2], evs[3], evs[4], evs[5]
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
