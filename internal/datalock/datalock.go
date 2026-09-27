// Package datalock keeps a restore from swapping a data directory out from
// under a running Tenant (TEN-305).
//
// Every process that uses a data dir holds a shared lock on <data>/.tenant.lock
// for as long as it runs; `tenant backup restore` takes the same lock
// exclusively. Shared holders never block each other, and neither side waits:
// whoever cannot get the lock is told why at once. The operating system drops
// a lock when its process exits, so a crash never leaves a stale one behind.
package datalock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrBusy is returned when the lock is held in a conflicting mode.
var ErrBusy = errors.New("datalock: busy")

// Lock is a held lock. Release it, or let the process exit.
type Lock struct {
	f *os.File
}

// FileName is the lock file inside a data dir. Backups skip it.
const FileName = ".tenant.lock"

// Path is the lock file for dataDir.
func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }

// Shared takes the lock that ordinary Tenant processes hold. It fails with
// ErrBusy only while a restore holds the lock exclusively.
func Shared(dataDir string) (*Lock, error) { return acquire(Path(dataDir), false) }

// Exclusive takes the lock a restore needs. It fails with ErrBusy while any
// other Tenant process holds it, shared or exclusive.
func Exclusive(dataDir string) (*Lock, error) { return acquire(Path(dataDir), true) }

// ExclusiveFile takes an exclusive lock on the file at path, creating it. Serve
// and the TUI each hold one as their boot lock (TEN-365): a boot lock that can
// be taken belongs to a process that has exited.
func ExclusiveFile(path string) (*Lock, error) { return acquire(path, true) }

func acquire(path string, exclusive bool) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("datalock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("datalock: %w", err)
	}
	if err := lockFile(f, exclusive); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Release drops the lock. It is safe to call on a nil Lock and more than once.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlockFile(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
