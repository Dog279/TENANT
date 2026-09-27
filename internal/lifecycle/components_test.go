package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"tenant/internal/logs/event"
)

// recordComponents installs a recorder and empties the registry for the test.
func recordComponents(t *testing.T) *recorder {
	t.Helper()
	rec := &recorder{}
	event.Install(rec)
	reset := func() {
		reg.Lock()
		reg.m = map[string]*Comp{}
		reg.Unlock()
	}
	reset()
	t.Cleanup(func() { event.Install(nil); reset() })
	return rec
}

func state(name string) (Component, bool) {
	for _, c := range Components() {
		if c.Name == name {
			return c, true
		}
	}
	return Component{}, false
}

// Every state change is a system event, and one run's events share its
// activity; setting the same state again is not a change, and a new start
// after a stop is a new run.
func TestComponentStateChangesAreSystemEvents(t *testing.T) {
	rec := recordComponents(t)
	c := Track(context.Background(), "discord relay", Connector, "discord", Starting, nil)
	c.Set(Running, nil)
	c.Set(Running, nil)
	c.Set(Degraded, errors.New("gateway reset"))
	if got, _ := state("discord relay"); got.State != Degraded || got.Error != "gateway reset" || got.Started.IsZero() {
		t.Errorf("degraded: %+v", got)
	}
	c.Set(Running, nil)
	c.Set(Stopped, nil)
	if got, _ := state("discord relay"); got.State != Stopped || !got.Started.IsZero() || got.Error != "gateway reset" {
		t.Errorf("stopped: %+v (the last error stays)", got)
	}
	Track(context.Background(), "discord relay", Connector, "discord", Running, nil)
	c.Set(Failed, errors.New("token revoked"))

	var got []string
	for _, e := range rec.events {
		if e.Log != "system" || e.Provider != "system" || field(e, "component") != "discord relay" || field(e, "log") != "discord" {
			t.Errorf("event %d: log %s provider %s data %v", e.ID, e.Log, e.Provider, e.Data)
		}
		got = append(got, fmt.Sprint(e.ID, e.Opcode, e.Level))
	}
	want := []string{"101startinfo", "102info", "2101warning", "102info", "103info", "102startinfo", "3101error"}
	if !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	first, second := rec.events[0].Corr.ActivityID, rec.events[5].Corr.ActivityID
	for i, e := range rec.events {
		want := first
		if i >= 5 {
			want = second
		}
		if e.Corr.ActivityID != want {
			t.Errorf("event %d: activity %q, want %q", i, e.Corr.ActivityID, want)
		}
	}
	if first == "" || first == second {
		t.Errorf("runs' activities %q and %q", first, second)
	}
	if got, _ := state("discord relay"); got.State != Failed || got.Error != "token revoked" || got.Activity != second {
		t.Errorf("failed: %+v", got)
	}
}

// A child is its own component while it runs, even beside another of the
// same name, and it is dropped when it ends; a failed child is a warning.
func TestChildComponentsComeAndGo(t *testing.T) {
	rec := recordComponents(t)
	a := Track(context.Background(), "chrome", Child, "browser", Running, nil)
	b := Track(context.Background(), "chrome", Child, "browser", Running, nil)
	if n := len(Components()); n != 2 {
		t.Fatalf("%d components, want 2 children", n)
	}
	a.Set(Stopped, nil)
	b.Set(Failed, errors.New("exit status 1"))
	if cs := Components(); len(cs) != 0 {
		t.Fatalf("children left: %+v", cs)
	}
	if e := rec.events[len(rec.events)-1]; e.ID != 2102 || e.Level != event.Warning {
		t.Errorf("child failure filed as %d at %v", e.ID, e.Level)
	}
}

// Group.Start is Add for a component: it is running once added, and its stop
// marks it stopped, or failed with the stop's error.
func TestGroupStartTracksAComponent(t *testing.T) {
	recordComponents(t)
	g := New(nil)
	g.StartFunc(Drain, "cron", Subsystem, "cron", func() {})
	g.Start(Intake, "dashboard", Server, "dashboard", func() error { return errors.New("listener gone") })
	if c, _ := state("cron"); c.State != Running || c.Kind != Subsystem || c.Log != "cron" {
		t.Fatalf("cron: %+v", c)
	}
	if steps := g.Steps(); !slices.Equal(steps, []string{"intake/dashboard", "drain/cron"}) {
		t.Errorf("Steps = %v", steps)
	}
	_ = g.Shutdown()
	if c, _ := state("cron"); c.State != Stopped {
		t.Errorf("cron after shutdown: %s", c.State)
	}
	if c, _ := state("dashboard"); c.State != Failed || c.Error != "listener gone" {
		t.Errorf("dashboard after shutdown: %+v", c)
	}
}
