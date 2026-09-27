package logs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

func plenty(string) (uint64, uint64, error) { return 500 << 30, 1 << 40, nil }

// msgs lists the messages of st's events in log.
func msgs(t *testing.T, st *store.Store, log string) []string {
	t.Helper()
	var out []string
	for _, e := range all(t, st) {
		if e.Log == log {
			out = append(out, e.Msg)
		}
	}
	return out
}

// Of two processes sharing a log book, exactly one runs the housekeeping
// tasks; a failing task is logged and the loop goes on; when the holder dies
// without letting go, the other takes over once its lease expires.
func TestHousekeeperOneHolderAndTakeover(t *testing.T) {
	every, ttl := leaseEvery, leaseTTL
	leaseEvery, leaseTTL = 20*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { leaseEvery, leaseTTL = every, ttl }) // runs after the hubs close
	path := filepath.Join(t.TempDir(), "logbook.db")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs [2]atomic.Int32
	hubs := make([]*Hub, 2)
	for i := range hubs {
		h, _, _ := openHub(t, Options{})
		st, err := store.Open(path, store.Options{BootID: fmt.Sprintf("%032x", i+1)})
		if err != nil {
			t.Fatal(err)
		}
		h.AttachStore(st)
		h.Housekeep("count", 0, func(context.Context) error { runs[i].Add(1); return nil })
		h.Housekeep("boom", time.Hour, func(context.Context) error { return errors.New("boom") })
		h.Start(ctx)
		hubs[i] = h
	}
	waitFor(t, 5*time.Second, func() bool { return runs[0].Load()+runs[1].Load() >= 5 })
	holder := 0
	if runs[1].Load() > 0 {
		holder = 1
	}
	other := 1 - holder
	if runs[other].Load() != 0 {
		t.Fatalf("both processes ran the tasks: %d and %d times", runs[0].Load(), runs[1].Load())
	}
	waitFor(t, 5*time.Second, func() bool { // the ring is fed once the writer commits
		ring, _ := hubs[holder].Snapshot()
		return slices.ContainsFunc(ring, func(e Entry) bool { return strings.Contains(e.Msg, "task boom failed") && e.Field("err") == "boom" })
	})

	closed := time.Now()
	_ = hubs[holder].Close() // as if it died: the lease is left to expire
	waitFor(t, 5*time.Second, func() bool { return runs[other].Load() > 0 })
	if took := time.Since(closed); took < leaseTTL/2 {
		t.Errorf("took over after %v, before the lease could expire", took)
	}
}

func waitFor(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}

// Low disk sheds by free space, injected here: under 1 GB or 5% free the
// diagnostic events, under 256 MB also operational ones below warning. The
// Security log always writes; shed events count as dropped, and crossing
// into low disk leaves a warning.
func TestLowDiskSheds(t *testing.T) {
	for _, c := range []struct {
		name        string
		free, total uint64
		want        []string
	}{
		{"plenty", 50 << 30, 100 << 30, []string{"info", "warning", "diag"}},
		{"under 5 percent", 4 << 30, 100 << 30, []string{"info", "warning"}},
		{"under 1 GB", 900 << 20, 2 << 30, []string{"info", "warning"}},
		{"under 256 MB", 100 << 20, 100 << 30, []string{"warning"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, _ := openHub(t, Options{})
			st := attach(t, h)
			h.hk.diskSpace = func(string) (uint64, uint64, error) { return c.free, c.total, nil }
			ctx := context.Background()
			h.refresh(ctx, h.store())
			p := h.pipe.Load()
			for _, e := range []event.Event{
				{Log: "x", Level: event.Info, Msg: "info"},
				{Log: "x", Level: event.Warning, Msg: "warning"},
				{Log: "x", Level: event.Info, Channel: event.Diagnostic, Msg: "diag"},
				{Log: store.SecurityLog, Level: event.Info, Msg: "audit"},
			} {
				p.enqueue(ctx, &e)
			}
			dropped := h.Dropped()["x"]
			_ = h.Close()
			if got := msgs(t, st, "x"); !slices.Equal(got, c.want) {
				t.Errorf("wrote %v, want %v", got, c.want)
			}
			if got := msgs(t, st, store.SecurityLog); !slices.Equal(got, []string{"audit"}) {
				t.Errorf("security wrote %v", got)
			}
			if int(dropped) != 3-len(c.want) {
				t.Errorf("dropped %d", dropped)
			}
			warned := slices.ContainsFunc(msgs(t, st, "logbook"), func(m string) bool { return strings.Contains(m, "low disk") })
			if warned != (len(c.want) < 3) {
				t.Errorf("low-disk warning written: %v", warned)
			}
		})
	}
}

// Each log's minimum level, enabled switch and max age apply live: from
// config.json (the Budget func, re-read every tick) and from the logs table,
// which another process's `tenant logs config` writes.
func TestPoliciesApplyLive(t *testing.T) {
	var mu sync.Mutex
	b := store.DefaultBudget()
	budget := func() store.Budget { mu.Lock(); defer mu.Unlock(); return b }
	h, root, _ := openHub(t, Options{Budget: budget})
	other := attach(t, h)
	h.hk.diskSpace = plenty
	ctx := context.Background()
	p := h.pipe.Load()
	emit := func(log string, lvl event.Level, msg string) {
		p.enqueue(ctx, &event.Event{Log: log, Level: lvl, Msg: msg})
	}

	h.refresh(ctx, h.store())
	emit("a", event.Info, "a info before")
	mu.Lock()
	b.Logs = maps.Clone(b.Logs)
	b.Logs["a"] = store.Policy{MaxAge: 7 * 24 * time.Hour, OnFull: store.Overwrite, MinLevel: event.Warning}
	mu.Unlock()
	if err := other.SetPolicy(ctx, "b", store.Policy{OnFull: store.Overwrite, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	h.refresh(ctx, h.store())
	emit("a", event.Info, "a info after")
	emit("a", event.Warning, "a warning after")
	emit("b", event.Critical, "b critical")

	// Mirror files follow their log's max age: a's 7 days, c's default 30.
	for _, log := range []string{"a", "c"} {
		_ = os.MkdirAll(filepath.Join(root, log), 0o755)
		_ = os.WriteFile(filepath.Join(root, log, "2026-09-01.log"), []byte("x\n"), 0o644)
	}
	h.mu.Lock()
	h.scanLocked()
	h.pruneLocked()
	h.mu.Unlock()
	_, aErr := os.Stat(filepath.Join(root, "a", "2026-09-01.log"))
	_, cErr := os.Stat(filepath.Join(root, "c", "2026-09-01.log"))
	if !os.IsNotExist(aErr) || cErr != nil {
		t.Errorf("after the prune: a's old file %v, c's %v", aErr, cErr)
	}

	_ = h.Close()
	if got := msgs(t, other, "a"); !slices.Equal(got, []string{"a info before", "a warning after"}) {
		t.Errorf("a wrote %v", got)
	}
	if got := msgs(t, other, "b"); len(got) != 0 {
		t.Errorf("disabled b wrote %v", got)
	}
}

// A log whose policy keeps every record is warned about once per crossing,
// not at every pass.
func TestKeepWarnsOncePerCrossing(t *testing.T) {
	b := store.DefaultBudget()
	b.Logs["x"] = store.Policy{MaxBytes: 1, OnFull: store.Keep}
	h, _, _ := openHub(t, Options{Budget: func() store.Budget { return b }})
	other := attach(t, h)
	h.hk.diskSpace = plenty
	ctx := context.Background()
	if err := other.Commit(ctx, []event.Event{{Log: "x", Provider: "x", Level: event.Info, Msg: "kept"}}); err != nil {
		t.Fatal(err)
	}
	h.refresh(ctx, h.store())
	for range 3 {
		if err := h.retain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	_ = h.Close()
	var warned int
	for _, m := range msgs(t, other, "logbook") {
		if strings.Contains(m, "log x is past its size limit") {
			warned++
		}
	}
	if warned != 1 || !slices.Equal(msgs(t, other, "x"), []string{"kept"}) {
		t.Errorf("warned %d times; x holds %v", warned, msgs(t, other, "x"))
	}
}
