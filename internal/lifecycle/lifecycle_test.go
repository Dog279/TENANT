package lifecycle

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/logs/event/eventtest"
)

// Stages run in order, and within a stage the last added stops first.
func TestShutdownRunsStagesInOrder(t *testing.T) {
	g := New(nil)
	var got []string
	rec := func(name string) func() { return func() { got = append(got, name) } }
	g.AddFunc(Release, "stores", rec("stores"))
	g.AddFunc(Unblock, "approvals", rec("approvals"))
	g.AddFunc(Drain, "cron", rec("cron"))
	g.AddFunc(Intake, "relay", rec("relay"))
	g.AddFunc(Release, "runtime", rec("runtime"))
	g.AddFunc(Intake, "peer", rec("peer"))
	g.AddFunc(Drain, "improve", rec("improve"))

	want := []string{"approvals", "peer", "relay", "improve", "cron", "runtime", "stores"}
	if steps := g.Steps(); !slices.Equal(steps, []string{
		"unblock/approvals", "intake/peer", "intake/relay", "drain/improve", "drain/cron", "release/runtime", "release/stores",
	}) {
		t.Errorf("Steps = %v", steps)
	}
	if err := g.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// Only the first Shutdown stops anything, so a command can defer it and also
// call it at the end.
func TestShutdownRunsOnce(t *testing.T) {
	g := New(nil)
	n := 0
	g.AddFunc(Release, "x", func() { n++ })
	_ = g.Shutdown()
	_ = g.Shutdown()
	if n != 1 {
		t.Fatalf("stop ran %d times", n)
	}
}

// A failing or panicking stop is reported, and every other stop still runs.
func TestShutdownReportsFailuresAndContinues(t *testing.T) {
	g := New(nil)
	ran := map[string]bool{}
	boom := errors.New("boom")
	g.Add(Intake, "fails", func() error { ran["fails"] = true; return boom })
	g.AddFunc(Drain, "panics", func() { ran["panics"] = true; panic("oops") })
	g.AddFunc(Release, "after", func() { ran["after"] = true })
	err := g.Shutdown()
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the stop's error", err)
	}
	if err == nil || !strings.Contains(err.Error(), "drain/panics: panic: oops") || !strings.Contains(err.Error(), "intake/fails") {
		t.Errorf("err = %v, want both failures named", err)
	}
	for _, name := range []string{"fails", "panics", "after"} {
		if !ran[name] {
			t.Errorf("%s did not run", name)
		}
	}
}

// A stop added once shutdown has begun runs at once instead of leaking.
func TestAddAfterShutdownRunsAtOnce(t *testing.T) {
	g := New(nil)
	_ = g.Shutdown()
	ran := false
	g.AddFunc(Release, "late", func() { ran = true })
	if !ran {
		t.Fatal("a stop added after Shutdown must run immediately")
	}
}

// Concurrent Add and Shutdown neither race nor lose a stop.
func TestConcurrentAddAndShutdown(t *testing.T) {
	g := New(nil)
	var mu sync.Mutex
	ran := 0
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.AddFunc(Release, "s", func() { mu.Lock(); ran++; mu.Unlock() })
		}()
	}
	go func() { _ = g.Shutdown() }()
	wg.Wait()
	_ = g.Shutdown()
	mu.Lock()
	defer mu.Unlock()
	if ran != 50 {
		t.Fatalf("%d of 50 stops ran", ran)
	}
}

func TestNilStopsAreIgnored(t *testing.T) {
	g := New(nil)
	g.Add(Release, "nil", nil)
	g.AddFunc(Release, "nil", nil)
	if steps := g.Steps(); len(steps) != 0 {
		t.Fatalf("Steps = %v", steps)
	}
}

// The shutdown and each stage with a stop are activities in the system log
// (TEN-383): a start, then a stop or a fail with the duration. A failed or
// panicking stop is an event under its stage's activity.
func TestShutdownIsFiledAsActivities(t *testing.T) {
	rec := eventtest.Capture(t)

	g := New(nil)
	g.AddFunc(Unblock, "approvals", func() {})
	g.Add(Intake, "relay", func() error { return errors.New("boom") })
	g.AddFunc(Release, "store", func() { panic("oops") })
	_ = g.Shutdown()

	var got []string
	acts := map[string]string{} // "id/stage" of each start → its activity
	evs := rec.Events()
	for _, e := range evs {
		if e.Log != "system" || e.Provider != "system" || strings.Join(catalog.Group(e.Log), "/") != "Tenant" {
			t.Errorf("event %d filed in %s/%s", e.ID, e.Provider, e.Log)
		}
		stage, _ := field(e, "stage").(string)
		got = append(got, strings.TrimSpace(fmt.Sprintf("%d %s %s", e.ID, e.Opcode, stage)))
		switch {
		case e.Opcode == event.OpStart:
			acts[fmt.Sprint(e.ID)+stage] = e.Corr.ActivityID
		case e.Opcode != "" && field(e, "duration") == nil:
			t.Errorf("%d %s has no duration", e.ID, e.Opcode)
		}
	}
	want := []string{
		"20 start",
		"21 start unblock", "21 stop unblock",
		"21 start intake", "2020  intake", "21 fail intake",
		"21 start release", "3020  release", "21 fail release",
		"20 fail",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A stage belongs to the shutdown, a failed step to its stage.
	if e := evs[1]; e.Corr.RelatedID != acts["20"] {
		t.Errorf("stage related = %q, want the shutdown's %q", e.Corr.RelatedID, acts["20"])
	}
	if e := evs[4]; e.Corr.ActivityID != acts["21intake"] || field(e, "step") != "relay" {
		t.Errorf("step failure: activity %q step %v", e.Corr.ActivityID, field(e, "step"))
	}
	if e := evs[7]; e.Level != event.Error || !strings.Contains(fmt.Sprint(field(e, "stack")), "lifecycle") {
		t.Errorf("panic event: level %v, stack %.80v", e.Level, field(e, "stack"))
	}
}

func field(e *event.Event, key string) any {
	for _, f := range e.Data {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}
