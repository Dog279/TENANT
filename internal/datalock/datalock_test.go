package datalock

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSharedHoldersCoexist(t *testing.T) {
	dir := t.TempDir()
	a, err := Shared(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := Shared(dir)
	if err != nil {
		t.Fatalf("second shared lock: %v", err)
	}
	defer b.Release()
}

func TestExclusiveExcludesEveryone(t *testing.T) {
	dir := t.TempDir()
	s, err := Shared(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Exclusive(dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("exclusive while shared held: err = %v, want ErrBusy", err)
	}
	if err := s.Release(); err != nil {
		t.Fatal(err)
	}
	x, err := Exclusive(dir)
	if err != nil {
		t.Fatalf("exclusive after release: %v", err)
	}
	if _, err := Shared(dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("shared while exclusive held: err = %v, want ErrBusy", err)
	}
	if _, err := Exclusive(dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("second exclusive: err = %v, want ErrBusy", err)
	}
	if err := x.Release(); err != nil {
		t.Fatal(err)
	}
	if err := x.Release(); err != nil {
		t.Fatalf("double release: %v", err)
	}
	var nilLock *Lock
	if err := nilLock.Release(); err != nil {
		t.Fatalf("nil release: %v", err)
	}
	s2, err := Shared(dir)
	if err != nil {
		t.Fatalf("shared after exclusive released: %v", err)
	}
	s2.Release()
}

// A record-file lock waits out a short conflicting hold, and gives up with
// ErrBusy once fileWait has passed.
func TestFileWaitsForTheHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	a, err := File(path, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := File(path, false)
	if err != nil {
		t.Fatalf("second shared record lock: %v", err)
	}
	b.Release()
	go func() {
		time.Sleep(50 * time.Millisecond)
		a.Release()
	}()
	x, err := File(path, true)
	if err != nil {
		t.Fatalf("exclusive after the shared holder let go: %v", err)
	}
	defer x.Release()

	saved := fileWait
	fileWait = 20 * time.Millisecond
	defer func() { fileWait = saved }()
	if _, err := File(path, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("shared while exclusive held: err = %v, want ErrBusy", err)
	}
}
