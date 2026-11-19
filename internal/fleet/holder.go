package fleet

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"tenant/internal/datalock"
)

// AtRestPaths are the master's fleet files that hold something worth
// stealing or forging: fleet.json, the master's identity, and the record of
// which process holds the fleet. They are written owner-only; doctor audits
// them and the file tools refuse them.
func AtRestPaths(dir string) []string {
	return []string{StorePath(dir), MasterIdentityPath(dir), filepath.Join(dir, holderFileName)}
}

// LockPaths are the fleet's lock files. They hold nothing, but one that is
// replaced no longer excludes anybody, so nothing but the fleet writes them.
func LockPaths(dir string) []string {
	return []string{StorePath(dir) + ".lock", filepath.Join(dir, holderFileName+".lock")}
}

// CurrentHolder is the process that holds the fleet in dir right now, by its
// own account, for a process that is not it: the command line. held is false
// when no process holds the fleet, whatever a holder file left by a crash
// says.
//
// It finds out by trying the holder's lock for an instant. A master that
// starts in that instant is told the fleet is held; one that holds its fleet
// for long tries again, so the instant costs it a few seconds at worst.
func CurrentHolder(dir string) (h Holder, held bool) {
	path := filepath.Join(dir, holderFileName)
	if _, err := os.Lstat(path + ".lock"); errors.Is(err, fs.ErrNotExist) {
		return Holder{}, false // no master ever ran here; leave nothing behind for having looked
	}
	lock, err := datalock.ExclusiveFile(path + ".lock")
	if err == nil {
		_ = lock.Release()
		return Holder{}, false
	}
	if !errors.Is(err, datalock.ErrBusy) {
		return Holder{}, false
	}
	if b, err := os.ReadFile(path); err == nil && len(b) <= 4<<10 {
		_ = json.Unmarshal(b, &h)
	}
	return h, true
}
