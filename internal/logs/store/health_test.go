package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/logs/event"
)

// Quarantine refuses while any process has the store open, and otherwise
// moves the database with its -wal and -shm; the store opened in its place
// is empty and has a new ID.
func TestQuarantineMovesTheSidecars(t *testing.T) {
	path := tempDB(t)
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	old := s.StoreID()
	if _, err := Quarantine(path); !errors.Is(err, datalock.ErrBusy) {
		t.Fatalf("Quarantine while open = %v, want ErrBusy", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"-wal", "-shm"} { // as a crash leaves them
		if err := os.WriteFile(path+side, []byte(side), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	moved, err := Quarantine(path)
	if err != nil || !strings.HasPrefix(moved, path+".corrupt-") {
		t.Fatalf("Quarantine = %q, %v", moved, err)
	}
	for _, side := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(path + side); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("logbook.db%s is still in place", side)
		}
		if _, err := os.Stat(moved + side); err != nil {
			t.Errorf("logbook.db%s did not move: %v", side, err)
		}
	}
	fresh := open(t, path, "")
	if fresh.StoreID() == old {
		t.Error("the new store kept the old store ID")
	}
}

// Salvage copies a moved-aside database's events into the live store,
// marked recovered; a file that isn't a database salvages nothing.
func TestSalvage(t *testing.T) {
	ctx := context.Background()
	path := tempDB(t)
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	batch := make([]event.Event, 1200) // several pages
	for i := range batch {
		batch[i] = ev("x", "event "+strconv.Itoa(i), "n", i)
	}
	if err := s.Commit(ctx, batch); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	moved, err := Quarantine(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh := open(t, path, "")
	if n, err := Salvage(ctx, moved, fresh); n != 1200 || err != nil {
		t.Fatalf("Salvage = %d, %v", n, err)
	}
	got, _, _, err := fresh.Since(ctx, Cursor{}, 2000)
	if err != nil || len(got) != 1200 || got[1199].Msg != "event 1199" || !slices.Contains(got[0].Keywords, "recovered") {
		t.Fatalf("salvaged %d events (%v): first %+v", len(got), err, got[0])
	}

	junk := filepath.Join(t.TempDir(), "junk.db")
	_ = os.WriteFile(junk, []byte(strings.Repeat("junk", 2000)), 0o600)
	if n, err := Salvage(ctx, junk, fresh); n != 0 || err == nil {
		t.Errorf("Salvage of junk = %d, %v", n, err)
	}
}

// Check passes a sound database and finds a damaged page.
func TestCheckFindsDamage(t *testing.T) {
	ctx := context.Background()
	path := tempDB(t)
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	batch := make([]event.Event, 3000)
	for i := range batch {
		batch[i] = ev("x", strings.Repeat("filler ", 20)+strconv.Itoa(i))
	}
	if err := s.Commit(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(ctx); err != nil {
		t.Fatalf("Check of a sound store = %v", err)
	}
	if _, err := s.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	_, _ = f.WriteAt([]byte(strings.Repeat("\xa5", 8192)), st.Size()/2/4096*4096)
	_ = f.Close()
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Check(ctx); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Check of a damaged store = %v, want ErrCorrupt", err)
	}
}

// The dropped and suppressed counters add up per log.
func TestAddLost(t *testing.T) {
	ctx := context.Background()
	s := open(t, tempDB(t), "")
	if err := s.Commit(ctx, []event.Event{ev("x", "m")}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.AddLost(ctx, map[string]Lost{"x": {Dropped: 2, Suppressed: 5}, "y": {Suppressed: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := s.Logs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []LogInfo{{Name: "x", Events: 1, Bytes: logs[0].Bytes, LastWrite: logs[0].LastWrite, Dropped: 4, Suppressed: 10}, {Name: "y", Suppressed: 2}}
	if !slices.Equal(logs, want) || logs[0].Bytes == 0 {
		t.Errorf("Logs = %+v, want %+v", logs, want)
	}
}

// A boot row is unfinished until it stops cleanly or is reported, and only
// one of several checkers reports it.
func TestBoots(t *testing.T) {
	ctx := context.Background()
	s := open(t, tempDB(t), "")
	const a, b = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	start := time.Now().UTC().Add(-time.Hour)
	for _, id := range []string{a, b} {
		if err := s.BeginBoot(ctx, Boot{ID: id, PID: 42, Role: "serve", Version: "v1", Host: "h", Started: start}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EndBoot(ctx, b, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.Unfinished(ctx, "")
	if err != nil || len(got) != 1 || got[0].ID != a || got[0].Role != "serve" || got[0].PID != 42 || !got[0].Started.Equal(start) {
		t.Fatalf("Unfinished = %+v, %v", got, err)
	}
	if got, _ := s.Unfinished(ctx, a); len(got) != 0 {
		t.Errorf("Unfinished lists its own process: %+v", got)
	}
	e := ev("x", "last")
	e.Proc.BootID, e.Time = a, start.Add(time.Minute)
	if err := s.Commit(ctx, []event.Event{e}); err != nil {
		t.Fatal(err)
	}
	if last, err := s.LastEvent(ctx, a, start); err != nil || !last.Equal(e.Time) {
		t.Errorf("LastEvent = %v, %v, want %v", last, err, e.Time)
	}
	first, _ := s.MarkReported(ctx, a)
	second, _ := s.MarkReported(ctx, a)
	if !first || second {
		t.Errorf("MarkReported = %v then %v, want true then false", first, second)
	}
	if got, _ := s.Unfinished(ctx, ""); len(got) != 0 {
		t.Errorf("a reported row is still unfinished: %+v", got)
	}
}
