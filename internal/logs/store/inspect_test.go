package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/logs/query"
)

// A read-only open, as doctor and the security audit make, creates nothing
// for a store that isn't there, and changes nothing of one that is: no mode,
// no row. It still reads everything a report needs, whatever else holds the
// store open.
func TestOpenReadOnly(t *testing.T) {
	path := tempDB(t)
	if _, err := Open(path, Options{ReadOnly: true}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read-only open of a missing store: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a read-only open created %s: %v", filepath.Dir(path), err)
	}

	const boot = "0123456789abcdef0123456789abcdef"
	w := open(t, path, boot)
	if err := w.Commit(ctx, []event.Event{ev("system", "hello")}); err != nil {
		t.Fatal(err)
	}
	if ok, err := w.TryLease(ctx, "housekeeper", time.Minute); !ok || err != nil {
		t.Fatalf("lease: %v, %v", ok, err)
	}
	started := time.Now().UTC().Truncate(time.Second)
	if err := w.BeginBoot(ctx, Boot{ID: boot, PID: 42, Role: "serve", Host: "box", Started: started}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.StoreID() != w.StoreID() {
		t.Error("the read-only store has another ID")
	}
	if err := r.Check(ctx); err != nil {
		t.Errorf("check: %v", err)
	}
	if logs, err := r.Logs(ctx); err != nil || len(logs) != 1 {
		t.Errorf("logs = %+v, %v", logs, err)
	}
	holder, expires, err := r.Lease(ctx, "housekeeper")
	if err != nil || holder != boot || time.Until(expires) < 30*time.Second {
		t.Errorf("lease = %q until %v, %v", holder, expires, err)
	}
	if h, _, err := r.Lease(ctx, "nobody"); h != "" || err != nil {
		t.Errorf("an untaken lease = %q, %v", h, err)
	}
	if b, err := r.Boot(ctx, boot); err != nil || b.PID != 42 || b.Role != "serve" || b.Host != "box" || !b.Started.Equal(started) {
		t.Errorf("boot = %+v, %v", b, err)
	}
	if _, err := r.Boot(ctx, "ffffffffffffffffffffffffffffffff"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown boot: %v", err)
	}
	if err := r.Commit(ctx, []event.Event{ev("system", "not written")}); err == nil {
		t.Error("a read-only store took a commit")
	}
	if n, _ := w.Count(ctx, query.Filter{}); n != 1 {
		t.Errorf("%d events after the refused commit, want 1", n)
	}
	if fi, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o644 {
		t.Errorf("the read-only open changed the file: %v, %v", fi, err)
	}
}
