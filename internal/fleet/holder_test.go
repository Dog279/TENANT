package fleet

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The command line learns who holds a fleet from the holder's lock, not from
// the file a holder that crashed leaves behind.
func TestCurrentHolder(t *testing.T) {
	m := newMaster(t)
	if entries, _ := os.ReadDir(m.dir); len(entries) > 3 {
		t.Fatalf("unexpected files before anything held the fleet: %v", entries)
	}
	if h, held := CurrentHolder(m.dir); held {
		t.Fatalf("a fleet nobody started is held by %+v", h)
	}
	if _, err := os.Stat(filepath.Join(m.dir, holderFileName+".lock")); err == nil {
		t.Fatal("asking who holds the fleet left a lock file behind")
	}

	mgr, err := StartManager(ManagerConfig{Store: m.store, Identity: m.id, Addr: "127.0.0.1:0", Role: "serve"})
	if err != nil {
		t.Fatal(err)
	}
	h, held := CurrentHolder(m.dir)
	if !held || h.PID != os.Getpid() || h.Role != "serve" || h.Address != mgr.Addr().String() {
		t.Fatalf("the holder: %+v %v", h, held)
	}
	// Asking does not take the fleet from its holder.
	if _, err := StartManager(ManagerConfig{Store: m.store, Identity: m.id, Addr: "127.0.0.1:0"}); err == nil {
		t.Fatal("a second manager took a fleet that is held")
	}
	if err := mgr.Close(); err != nil {
		t.Fatal(err)
	}
	if h, held := CurrentHolder(m.dir); held {
		t.Fatalf("a released fleet is held by %+v", h)
	}

	// A holder that died without cleaning up left its file; its lock is gone.
	if err := os.WriteFile(filepath.Join(m.dir, holderFileName), []byte(`{"pid":4242,"role":"serve","address":"127.0.0.1:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if h, held := CurrentHolder(m.dir); held {
		t.Fatalf("a dead holder's file passes for a holder: %+v", h)
	}
	// And a new master can take the fleet after it.
	mgr, err = StartManager(ManagerConfig{Store: m.store, Identity: m.id, Addr: "127.0.0.1:0", Role: "tui"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if h, held := CurrentHolder(m.dir); !held || h.Role != "tui" {
		t.Fatalf("the holder after the takeover: %+v %v", h, held)
	}

	// The files that are worth guarding are named for the callers that guard them.
	for _, want := range []string{storeFileName, masterIdentityFile, holderFileName} {
		if !slices.Contains(AtRestPaths(m.dir), filepath.Join(m.dir, want)) {
			t.Errorf("AtRestPaths lacks %s", want)
		}
	}
	for _, want := range []string{storeFileName + ".lock", holderFileName + ".lock"} {
		if !slices.Contains(LockPaths(m.dir), filepath.Join(m.dir, want)) {
			t.Errorf("LockPaths lacks %s", want)
		}
		if _, err := os.Stat(filepath.Join(m.dir, want)); err != nil {
			t.Errorf("%s: %v", want, err)
		}
	}
}
