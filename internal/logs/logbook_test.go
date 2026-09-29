package logs

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// has reports whether st holds an event of def.
func has(t *testing.T, st *store.Store, def *event.Def) bool {
	t.Helper()
	s := def.Spec()
	return slices.ContainsFunc(all(t, st), func(e event.Event) bool { return e.Provider == s.Provider && e.ID == s.ID })
}

// A commit that keeps failing (a full disk, a write lock held past its
// timeout) is retried with backoff; after three failures the process runs on
// the text files alone, raising a critical event and the operator's banner,
// and keeps trying. The first commit that succeeds ends the outage and
// imports the text files' lines from it.
func TestWriteFailuresRunOnTheMirrorThenRecover(t *testing.T) {
	for _, fault := range []error{errors.New("database or disk is full (13)"), errors.New("database is locked (5)")} {
		t.Run(fault.Error(), func(t *testing.T) {
			first, most, commit, recoverFn := retryFirst, retryMax, commitBatch, recoverMirror
			// On the text files alone a commit is tried only for a batch that
			// comes after the backoff, which doubles with each try. A few more
			// batches while down (a slow runner's) grew it past the next flush:
			// "back" was only mirrored, nothing came after it, and the store
			// was never tried again (windows-latest). Here it doesn't grow, so
			// the batch after a failed try is always tried.
			retryFirst, retryMax = time.Millisecond, 0
			t.Cleanup(func() { retryFirst, retryMax, commitBatch, recoverMirror = first, most, commit, recoverFn })
			var failing atomic.Bool
			var attempts atomic.Int32
			failing.Store(true)
			commitBatch = func(st *store.Store, ctx context.Context, b []event.Event) error {
				if failing.Load() {
					attempts.Add(1)
					return fault
				}
				return commit(st, ctx, b)
			}
			type window struct {
				from, to time.Time
				keyword  string
			}
			imported := make(chan window, 1)
			recoverMirror = func(_ *Hub, _ context.Context, from, to time.Time, keyword string) (int, error) {
				imported <- window{from, to, keyword}
				return 7, nil
			}

			h, root, _ := openHub(t, Options{Now: time.Now})
			st := attach(t, h)
			var mu sync.Mutex
			var alerts []string
			var downAfter int32 // the attempts made when the banner went up
			h.OnAlert(func(s string) {
				mu.Lock()
				if len(alerts) == 0 {
					downAfter = attempts.Load()
				}
				alerts = append(alerts, s)
				mu.Unlock()
			})

			lost := time.Now()
			h.Logf("x", slog.LevelInfo, "during the outage")
			waitFor(t, 5*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(alerts) > 0 })
			mu.Lock()
			n := downAfter
			mu.Unlock()
			if n != failLimit || h.Degraded() == "" {
				t.Errorf("%d attempts before running on the mirror (degraded %q), want %d", n, h.Degraded(), failLimit)
			}
			h.Logf("x", slog.LevelInfo, "still out")
			waitFor(t, 5*time.Second, func() bool {
				return strings.Contains(readLogs(t, filepath.Join(root, "x")), "still out") &&
					strings.Contains(readLogs(t, filepath.Join(root, "logbook")), "runs on its text files alone")
			})
			if n := strings.Count(readLogs(t, filepath.Join(root, "logbook")), "could not commit"); n != failLimit {
				t.Errorf("%d commit failures in the logbook file, want %d", n, failLimit)
			}
			if got := msgs(t, st, "x"); len(got) != 0 {
				t.Errorf("the store took %v while failing", got)
			}

			failing.Store(false)
			h.Logf("x", slog.LevelInfo, "back")
			var w window
			select {
			case w = <-imported:
			case <-time.After(5 * time.Second):
				t.Fatal("the outage's lines were never imported")
			}
			if w.keyword != "recovered" || w.from.After(lost.Add(time.Millisecond)) || w.to.Before(w.from) {
				t.Errorf("imported %v to %v as %q; the first lost event was at %v", w.from, w.to, w.keyword, lost)
			}
			waitFor(t, 5*time.Second, func() bool {
				return has(t, st, catalog.RecoveryImport) && slices.Contains(msgs(t, st, "x"), "back")
			})
			if h.Degraded() != "" || !has(t, st, catalog.MirrorOnlyEnded) {
				t.Errorf("after recovery: degraded %q, store has x %v", h.Degraded(), msgs(t, st, "x"))
			}
			mu.Lock()
			defer mu.Unlock()
			if len(alerts) != 2 || !strings.HasPrefix(alerts[0], "⚠ log book: logbook.db can't be written") || !strings.HasPrefix(alerts[1], "✅") {
				t.Errorf("alerts = %q", alerts)
			}
		})
	}
}

// A corrupt database found at open is moved aside with its -wal and -shm,
// and a fresh store starts, with a critical event. While another process
// holds the store's lock, nothing moves: the process runs on its text files
// alone and says so.
func TestCorruptStoreQuarantined(t *testing.T) {
	garbage := func(root string) {
		for _, side := range []string{"", "-wal", "-shm"} {
			if err := os.WriteFile(filepath.Join(root, "logbook.db"+side), []byte(strings.Repeat("not a database ", 400)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("moved aside", func(t *testing.T) {
		h, root, _ := openHub(t, Options{})
		garbage(root)
		if _, err := h.OpenStore(nil); err != nil {
			t.Fatal(err)
		}
		// SQLite drops the garbage sidecars when the open fails; store's
		// TestQuarantineMovesTheSidecars covers moving them.
		if moved, _ := filepath.Glob(filepath.Join(root, "logbook.db.corrupt-*")); len(moved) != 1 {
			t.Fatalf("moved aside: %v", moved)
		}
		st := h.store()
		if h.Degraded() != "" || st == nil {
			t.Fatalf("degraded %q, store %v", h.Degraded(), st)
		}
		id := st.StoreID()
		r := readerAt(t, root)
		waitFor(t, 5*time.Second, func() bool { return has(t, r, catalog.Quarantined) && has(t, r, catalog.LogbookOpened) })
		for _, e := range all(t, r) {
			if e.ID == catalog.Quarantined.Spec().ID && e.Provider == "logbook" &&
				(e.Data[1].Value != hex.EncodeToString(id[:]) || !strings.HasPrefix(e.Data[0].Value.(string), filepath.Join(root, "logbook.db.corrupt-"))) {
				t.Errorf("quarantine event = %+v", e)
			}
		}
	})

	t.Run("in use elsewhere", func(t *testing.T) {
		h, root, _ := openHub(t, Options{})
		garbage(root)
		other, err := datalock.File(filepath.Join(root, "logbook"), false) // another process has it open
		if err != nil {
			t.Fatal(err)
		}
		defer other.Release()
		if _, err := h.OpenStore(nil); !errors.Is(err, store.ErrCorrupt) {
			t.Fatalf("OpenStore = %v, want ErrCorrupt", err)
		}
		if moved, _ := filepath.Glob(filepath.Join(root, "logbook.db.corrupt-*")); len(moved) != 0 {
			t.Fatalf("moved %v while another process had it open", moved)
		}
		if h.store() != nil || !strings.Contains(h.Degraded(), "corrupt") {
			t.Fatalf("store %v, degraded %q", h.store(), h.Degraded())
		}
		if f := readLogs(t, filepath.Join(root, "logbook")); !strings.Contains(f, "other Tenant processes have it open") || !strings.Contains(f, "severity=critical") {
			t.Errorf("logbook file:\n%s", f)
		}
	})
}

// Each provider and event ID has its own bucket; an uncatalogued line's is
// its template, so one chatty line doesn't silence its provider; the
// Security log is never limited. What was suppressed is reported, and
// counted per log in the store.
func TestRateLimits(t *testing.T) {
	h, _, _ := openHub(t, Options{Now: time.Now})
	st := attach(t, h)
	h.lb.lim.set(0.001, 3) // three, then next to nothing
	ctx := context.Background()
	p := h.pipe.Load()
	send := func(e event.Event) { p.enqueue(ctx, &e) }
	for i := range 5 {
		send(event.Event{Log: "mcp.github", Provider: "mcp.github", ID: 3001, Level: event.Error, Msg: "call failed " + strconv.Itoa(i)})
		send(event.Event{Log: "mcp.github", Provider: "mcp.github", ID: 3002, Level: event.Error, Msg: "other"})
		send(event.Event{Log: "chatty", Provider: "chatty", Level: event.Info, Msg: fmt.Sprintf(`poll %d of "job-%d" took %dms`, i, i, i*7)})
		send(event.Event{Log: "security", Provider: "security", Level: event.Info, Msg: "audit " + strconv.Itoa(i)})
	}
	send(event.Event{Log: "chatty", Provider: "chatty", Level: event.Info, Msg: "a different line"})
	h.report(ctx, h.store(), time.Minute)
	waitFor(t, 5*time.Second, func() bool { return len(msgs(t, st, "logbook")) >= 3 })

	count := func(log string) int { return len(msgs(t, st, log)) }
	if count("mcp.github") != 6 || count("chatty") != 4 || count("security") != 5 {
		t.Errorf("kept mcp.github %d (want 3+3), chatty %d (want 3+1), security %d (want 5)", count("mcp.github"), count("chatty"), count("security"))
	}
	report := strings.Join(msgs(t, st, "logbook"), "\n")
	for _, want := range []string{"Suppressed 2 × mcp.github 3001 in the last 1m0s", "Suppressed 2 × mcp.github 3002", `Suppressed 2 × chatty "poll of took" in the last`} {
		if !strings.Contains(report, want) {
			t.Errorf("the report lacks %q:\n%s", want, report)
		}
	}
	logs, err := st.Logs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range logs {
		if want := map[string]int64{"mcp.github": 4, "chatty": 2}[l.Name]; l.Suppressed != want {
			t.Errorf("%s: suppressed %d, want %d", l.Name, l.Suppressed, want)
		}
	}
}

func TestTemplate(t *testing.T) {
	for in, want := range map[string]string{
		`poll 3 of "job-3" took 21ms`:            "poll of took",
		`poll 4 of "job \"x\"" took 9ms`:         "poll of took",
		"open 'a.txt': permission denied":        "open : permission denied",
		"can't reach host-7 after 3 tries":       "can't reach after tries",
		"turn 3fa2b9c0d1e2f3a4 done in `1.2s`":   "turn done in",
		strings.Repeat("x ", 200) + "tail 12345": strings.TrimSpace(strings.Repeat("x ", 128)),
	} {
		if got := template(in); got != want {
			t.Errorf("template(%q) = %q, want %q", in, got, want)
		}
	}
}

// A gap of more than 2 seconds between the wall clock and the monotonic
// clock is a system warning.
func TestClockJump(t *testing.T) {
	now := clockNow
	t.Cleanup(func() { clockNow = now })
	wall, mono := time.Now().Round(0), time.Duration(0)
	clockNow = func() (time.Time, time.Duration) { return wall, mono }
	h, _, _ := openHub(t, Options{})
	st := attach(t, h)
	ctx := context.Background()
	h.watch(ctx, h.store())
	wall, mono = wall.Add(31*time.Second), mono+30*time.Second // drift within the slack
	h.watch(ctx, h.store())
	wall, mono = wall.Add(-time.Hour), mono+30*time.Second // the clock was set back
	h.watch(ctx, h.store())
	waitFor(t, 5*time.Second, func() bool { return len(msgs(t, st, "system")) > 0 })
	time.Sleep(3 * batchEvery)
	if got := msgs(t, st, "system"); len(got) != 1 || got[0] != "The wall clock moved -1h0m30s against the monotonic clock" {
		t.Errorf("system events = %q", got)
	}
}

// The housekeeper reports a log at 80% and at 95% of its size limit, once
// per crossing, and says what it did when one filled.
func TestNearFullOncePerCrossing(t *testing.T) {
	prev := batchEvery
	batchEvery = time.Hour // a report committed between the passes grows the WAL, and the second pass sees ~98%
	t.Cleanup(func() { batchEvery = prev })
	var mu sync.Mutex
	b := store.DefaultBudget()
	h, _, _ := openHub(t, Options{Budget: func() store.Budget { mu.Lock(); defer mu.Unlock(); return b }})
	other := attach(t, h)
	h.hk.diskSpace = plenty
	ctx := context.Background()
	batch := make([]event.Event, 500)
	for i := range batch {
		batch[i] = event.Event{Log: "x", Provider: "x", Level: event.Info, Msg: strings.Repeat("filler ", 30)}
	}
	if err := other.Commit(ctx, batch); err != nil {
		t.Fatal(err)
	}
	h.refresh(ctx, h.store())
	if err := h.retain(ctx); err != nil { // settle the files' size
		t.Fatal(err)
	}
	db, wal, _ := other.Size(ctx)
	mu.Lock()
	b.Logs["x"] = store.Policy{MaxBytes: (db + wal) * 100 / 90, OnFull: store.Overwrite}
	mu.Unlock()
	h.refresh(ctx, h.store())
	for range 2 {
		if err := h.retain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	b.Logs["x"] = store.Policy{MaxBytes: (db + wal) / 2, OnFull: store.Overwrite} // full: its oldest go
	mu.Unlock()
	h.refresh(ctx, h.store())
	if err := h.retain(ctx); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	var near, overwritten int
	for _, m := range msgs(t, other, "logbook") {
		switch {
		case strings.HasPrefix(m, "Log x is at 9"):
			near++
		case strings.HasPrefix(m, "Log x was full; its oldest"):
			overwritten++
		}
	}
	if near != 1 || overwritten != 1 {
		t.Errorf("near-full reports %d (want 1), overwritten %d (want 1): %q", near, overwritten, msgs(t, other, "logbook"))
	}
}

// A serve that dies while the TUI runs is reported by the TUI's housekeeper
// within a minute (here, its shortened period), with no restart.
func TestDeadProcessReportedWhileAnotherRuns(t *testing.T) {
	every, ttl, boots := leaseEvery, leaseTTL, bootsEvery
	leaseEvery, leaseTTL, bootsEvery = 20*time.Millisecond, 300*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { leaseEvery, leaseTTL, bootsEvery = every, ttl, boots })
	root := t.TempDir()
	cmd, stdin := startHelper(t, "live", root)
	defer stdin.Close()

	tui, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer tui.Close()
	if _, err := tui.OpenStore(nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tui.Start(ctx)
	time.Sleep(10 * leaseEvery)
	r := readerAt(t, root)
	if has(t, r, catalog.Unexpected) {
		t.Fatal("a live process was reported dead")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	waitFor(t, 10*time.Second, func() bool { return has(t, r, catalog.Unexpected) })
}

// A store with no chain key refuses only the security records
// (ErrNoChainKey): the store works, so that never counts toward running on
// the text files alone.
func TestNoChainKeyIsNotAWriteFailure(t *testing.T) {
	h, root, _ := openHub(t, Options{})
	st, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Each commit's security records, in order: a retried commit carries its
	// record again. (Timing it instead failed on a slow runner: four real
	// commits took longer than the first retry's wait.)
	commit := commitBatch
	t.Cleanup(func() { commitBatch = commit })
	var mu sync.Mutex
	var tried []string
	commitBatch = func(st *store.Store, ctx context.Context, b []event.Event) error {
		mu.Lock()
		for _, e := range b {
			if e.Log == store.SecurityLog {
				tried = append(tried, e.Msg)
			}
		}
		mu.Unlock()
		return commit(st, ctx, b)
	}
	h.AttachStore(st)
	ctx := context.Background()
	var want []string
	for i := range failLimit + 1 {
		audit := event.Event{Log: store.SecurityLog, Provider: "security", Level: event.Info, Msg: "audit " + strconv.Itoa(i)}
		if err := h.pipe.Load().audit(ctx, &audit); !errors.Is(err, store.ErrNoChainKey) {
			t.Fatalf("audit = %v, want ErrNoChainKey", err)
		}
		want = append(want, audit.Msg)
	}
	mu.Lock()
	got := slices.Clone(tried)
	mu.Unlock()
	if !slices.Equal(got, want) {
		t.Errorf("the refused records were committed as %q, want each once: %q", got, want)
	}
	time.Sleep(3 * batchEvery)
	if h.Degraded() != "" || strings.Contains(readLogs(t, filepath.Join(root, "logbook")), "could not commit") {
		t.Errorf("a refused security record counted as a write failure: degraded %q", h.Degraded())
	}
}
