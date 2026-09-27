package lifecycle

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
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
