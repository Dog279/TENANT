package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"tenant/internal/datalock"
)

// holdDataDir takes the shared data-dir lock for the rest of the process, so
// `tenant backup restore` cannot swap the directory out from under a running
// command (TEN-305). main installs lockDataDirForProcess; tests keep this
// no-op so they can remove their temp data dirs, which Windows refuses while
// a handle inside is open.
var holdDataDir = func(string) error { return nil }

var (
	heldMu    sync.Mutex
	heldLocks = map[string]*datalock.Lock{}
)

// lockDataDirForProcess holds dir's shared lock until the process exits. The
// lock only guards restore, so a data dir that cannot hold a lock file is not
// a reason to refuse to run; a restore in progress is.
func lockDataDirForProcess(dir string) error {
	key := filepath.Clean(dir)
	heldMu.Lock()
	defer heldMu.Unlock()
	if heldLocks[key] != nil {
		return nil
	}
	l, err := datalock.Shared(key)
	if errors.Is(err, datalock.ErrBusy) {
		return fmt.Errorf("a restore is replacing %s right now; run this again when it finishes", key)
	}
	if err != nil {
		return nil
	}
	heldLocks[key] = l
	return nil
}
