package cron

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"tenant/internal/logs/event"
)

// captured keeps the events written while it is installed.
type captured struct {
	mu     sync.Mutex
	events []*event.Event
}

func (c *captured) Emit(_ context.Context, e *event.Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}
func (c *captured) Audit(ctx context.Context, e *event.Event) error { c.Emit(ctx, e); return nil }

func capture(t *testing.T) *captured {
	c := &captured{}
	event.Install(c)
	t.Cleanup(func() { event.Install(nil) })
	return c
}

// logged lists the messages of the cron events written, in order.
func (c *captured) logged() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		if e.Provider == "cron" {
			out = append(out, e.Msg)
		}
	}
	return out
}

// A run missed while Tenant was down is logged: caught up for a safe job,
// skipped with the reason for an exec job or with catch-up off. A saved job
// whose schedule no longer parses is logged when it loads.
func TestEngineLogsMissedRunsAndBadSchedules(t *testing.T) {
	now := time.Date(2026, 6, 7, 10, 0, 0, 0, time.Local)
	due := now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	_, perr := Parse("not a spec")
	defs := []JobDef{
		{ID: "safe", Spec: "@every 1h", Prompt: "p", Enabled: true},
		{ID: "execj", Name: "deploy", Spec: "@every 1h", Prompt: "p", Enabled: true, Exec: true},
		{ID: "bad", Spec: "not a spec", Prompt: "p", Enabled: true},
	}
	hist := []RunRecord{{JobID: "safe", StartedAt: now.Add(-2 * time.Hour)}, {JobID: "execj", StartedAt: now.Add(-2 * time.Hour)}}
	for _, catchup := range []bool{true, false} {
		c := capture(t)
		e := NewEngine(defs, nil, nil)
		e.now = func() time.Time { return now }
		e.Prime(PrimeOptions{Catchup: catchup, History: hist})
		want := []string{"Job bad can't be scheduled and stays disabled: invalid schedule: " + perr.Error()}
		if catchup {
			want = append(want, "Job safe missed its run at "+due+"; it runs once now",
				"Job deploy missed its run at "+due+": shell and exec jobs never catch up")
		} else {
			want = append(want, "Job safe missed its run at "+due+": catch-up is off",
				"Job deploy missed its run at "+due+": catch-up is off")
		}
		if got := c.logged(); !slices.Equal(got, want) {
			t.Errorf("catchup=%v:\n got %q\nwant %q", catchup, got, want)
		}
	}
}

// Adding, switching and removing a job are logged, and so is a run whose
// history could not be saved.
func TestEngineLogsJobChanges(t *testing.T) {
	c := capture(t)
	e, _ := newTestEngine(func(context.Context, Job) (string, string, error) { return "ok", "", nil }, nil)
	e.historyPersist = func(RunRecord) error { return errors.New("disk full") }
	j, err := e.Add(AddSpec{Name: "brief", Spec: "@daily", Prompt: "p", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.SetEnabled(j.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunNow(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Remove(j.ID); err != nil {
		t.Fatal(err)
	}
	want := []string{"Job brief added: @daily", "Job brief disabled",
		"The run of job brief was not saved to the run history: disk full", "Job brief removed"}
	if got := c.logged(); !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}
