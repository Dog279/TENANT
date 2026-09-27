// Package datalock keeps a restore from swapping a data directory out from
// under a running Tenant (TEN-305).
//
// Every process that uses a data dir holds a shared lock on <data>/.tenant.lock
// for as long as it runs; `tenant backup restore` takes the same lock
// exclusively. Shared holders never block each other, and neither side waits:
// whoever cannot get the lock is told why at once. The operating system drops
// a lock when its process exits, so a crash never leaves a stale one behind.
//
// File applies the same locks to one record file that several processes
// append to, so a compaction never drops another process's rows.
package datalock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
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

// fileWait bounds how long File waits out a conflicting holder: each hold is
// one append or one compaction.
var fileWait = 10 * time.Second

// File locks the record file at path (a log that serve and the TUI both append
// to) through the sibling path+".lock", which is never replaced. Appenders take
// it shared; a compaction that rewrites the file takes it exclusively, so no
// append lands in a copy the rewrite is about to replace. Unlike the data-dir
// lock it waits, up to fileWait, and then fails with ErrBusy.
func File(path string, exclusive bool) (*Lock, error) {
	deadline := time.Now().Add(fileWait)
	for {
		l, err := acquire(path+".lock", exclusive)
		if !errors.Is(err, ErrBusy) || time.Now().After(deadline) {
			return l, err
		}
		// ponytail: polls, so a waiter wakes up to 5ms late; a blocking OS wait
		// (with a deadline) if record-file contention ever gets hot.
		time.Sleep(5 * time.Millisecond)
	}
}

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
