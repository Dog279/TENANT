package fleet

import (
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"
)

// On Windows a file that anything has open cannot be replaced. A save waits
// a moment like that out. If the file stays open, the caller is told, the
// change is not half made, and it goes through once the file is let go.
func TestSaveOutlastsAFileHeldOpen(t *testing.T) {
	m := newMaster(t)
	one, _ := m.add(t, "one", StateConfirmed)
	two, _ := m.add(t, "two", StateConfirmed)
	path := StorePath(m.dir)

	// Open for a moment, as a scanner has it.
	held := must(os.Open(path))(t)
	release := time.AfterFunc(150*time.Millisecond, func() { _ = held.Close() })
	defer release.Stop()
	if err := m.store.Revoke(one.ID); err != nil {
		t.Fatalf("a revoke while the file was open for a moment: %v", err)
	}
	if err := Open(m.dir).Check(one.ID); !errors.Is(err, ErrRevoked) {
		t.Fatalf("the revoke is not in the file: %v", err)
	}

	// Open for good.
	stuck := must(os.Open(path))(t)
	defer stuck.Close()
	before := must(os.ReadFile(path))(t)
	start := time.Now()
	err := m.store.Pause(two.ID)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("a save over a file held open: err = %v, want the refusal Windows gave", err)
	}
	if waited := time.Since(start); waited < 500*time.Millisecond || waited > 10*time.Second {
		t.Errorf("the save gave up after %s, want about a second", waited)
	}
	if after := must(os.ReadFile(path))(t); string(after) != string(before) {
		t.Error("a save that failed changed the file")
	}
	if err := m.store.Check(two.ID); err != nil {
		t.Errorf("after a failed pause the machine is not as it was: %v", err)
	}
	if left := must(os.ReadDir(m.dir))(t); len(left) != 3 { // fleet.json, its lock, the identity
		t.Errorf("a failed save left files behind: %v", left)
	}

	_ = stuck.Close()
	if err := m.store.Pause(two.ID); err != nil {
		t.Fatalf("the same change once the file was let go: %v", err)
	}
	if err := m.store.Check(two.ID); !errors.Is(err, ErrPaused) {
		t.Errorf("Check = %v", err)
	}
}
