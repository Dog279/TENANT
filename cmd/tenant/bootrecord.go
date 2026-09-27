package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/fsperm"
	"tenant/internal/lifecycle"
	"tenant/internal/logs"
	"tenant/internal/runtrace"
	"tenant/internal/version"
)

// Crash capture and unexpected-exit detection (TEN-365). Serve and the TUI,
// and never a one-shot command (so a Ctrl-C never reads as a crash), keep
// three files under the log root for their lifetime:
//
//	boots/<boot_id>.lock  held exclusively until the process exits
//	boots/<boot_id>.json  pid, role, version, host and start time; an
//	                      orderly stop marks it clean
//	crash/<boot_id>.txt   the runtime's fatal output (debug.SetCrashOutput)
//
// The OS drops a lock when its process exits, so a record whose lock can be
// taken belongs to a process that is gone, and without its clean mark that
// process ended unexpectedly. Only the lock decides, never a record's age:
// after a laptop wakes, a live process would look long dead.
//
// ponytail: checked at start only, so a serve crash is reported when serve or
// the TUI next starts; LOG-014 moves the records into logbook.db and checks
// every minute.

// bootRecord is what a later start knows about a process that died.
type bootRecord struct {
	PID      int       `json:"pid"`
	Role     string    `json:"role"`
	Version  string    `json:"version"`
	Host     string    `json:"host"`
	Started  time.Time `json:"started"`
	Clean    bool      `json:"clean,omitempty"`    // stopped in order
	Reported bool      `json:"reported,omitempty"` // its unexpected end is filed
}

// crashHead bounds how much of a crash dump its event carries (the log book
// caps values further). The whole dump stays readable in the system folder.
const crashHead = 64 << 10

// startBootRecord starts this process's boot record and crash capture, files
// what earlier runs left behind, and registers the clean stop. Call it right
// after the lifecycle group is made, so the clean stop is the first Release
// step registered and the last to run.
//
// Defer the returned function. A panic on the calling goroutine runs the
// deferred shutdown before the runtime prints the crash; onPanic sees it
// first, so that shutdown neither marks the record clean nor turns the crash
// file off.
func startBootRecord(hub *logs.Hub, life *lifecycle.Group, role string) (onPanic func()) {
	var crashed atomic.Bool
	onPanic = func() {
		if r := recover(); r != nil {
			crashed.Store(true)
			panic(r)
		}
	}
	debug.SetTraceback("all") // an unrecovered panic dumps every goroutine, not only its own
	root := hub.Root()
	if root == "" {
		return onPanic // a memory-only hub: nowhere to keep records
	}
	id := runtrace.BootID()
	if stop, err := beginBoot(root, id, role); err != nil {
		hub.Logf("system", slog.LevelWarn, "crash capture unavailable; an unexpected exit will go unreported", "err", err)
	} else {
		life.AddFunc(lifecycle.Release, "boot record", func() {
			if !crashed.Load() {
				stop()
			}
		})
	}
	reportDeadBoots(hub, id)
	return onPanic
}

// beginBoot takes this process's boot lock, points the runtime's crash output
// at its crash file and writes its record. stop undoes it all for a clean exit.
func beginBoot(root, id, role string) (stop func(), err error) {
	boots, crashDir := filepath.Join(root, "boots"), filepath.Join(root, "crash")
	if err := errors.Join(fsperm.MkdirAll(boots), fsperm.MkdirAll(crashDir)); err != nil {
		return nil, err
	}
	lock, err := datalock.ExclusiveFile(filepath.Join(boots, id+".lock"))
	if err != nil {
		return nil, err
	}
	recPath, crashPath := filepath.Join(boots, id+".json"), filepath.Join(crashDir, id+".txt")
	host, _ := os.Hostname()
	rec := bootRecord{PID: os.Getpid(), Role: role, Version: version.Version, Host: host, Started: time.Now().UTC()}
	f, err := fsperm.OpenFile(crashPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err == nil {
		err = debug.SetCrashOutput(f, debug.CrashOptions{}) // it keeps its own copy of the handle
		_ = f.Close()
	}
	if err == nil {
		err = writeBootRecord(recPath, rec)
	}
	if err != nil {
		_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		_ = lock.Release()
		return nil, err
	}
	return func() {
		// Windows can't delete a file the runtime still holds: release it first.
		_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		removeIfEmpty(crashPath)
		rec.Clean = true
		_ = writeBootRecord(recPath, rec)
		_ = lock.Release()
	}, nil
}

// reportDeadBoots files each earlier process that ended without a clean stop,
// then clears its files. Nothing of a process is read until its boot lock is
// ours, so a live process is never reported and its crash file never read.
// It walks the records, not the locks: a record is written only once its lock
// is held, while a lock file exists a moment before it is locked.
func reportDeadBoots(hub *logs.Hub, self string) {
	boots := filepath.Join(hub.Root(), "boots")
	ents, _ := os.ReadDir(boots)
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || id == self {
			continue
		}
		lockPath := filepath.Join(boots, id+".lock")
		lock, err := datalock.ExclusiveFile(lockPath)
		if err != nil {
			continue // its process is alive
		}
		recPath, crashPath := filepath.Join(boots, e.Name()), filepath.Join(hub.Root(), "crash", id+".txt")
		if rec, err := readBootRecord(recPath); err == nil && !rec.Clean && !rec.Reported {
			reportDeadBoot(hub, id, rec, crashPath)
			rec.Reported = true
			_ = writeBootRecord(recPath, rec) // never filed twice, should a removal below fail
		}
		removeIfEmpty(crashPath)
		_ = os.Remove(recPath)
		_ = lock.Release()
		_ = os.Remove(lockPath) // after the release: Windows can't delete an open file
	}
}

// reportDeadBoot files one process that ended without a clean stop: a crash
// when the runtime left its fatal output, else an unexpected end (killed,
// power lost, os.Exit). The dump moves into the system folder, which the
// dashboard's file view lists; the event carries its head and its path.
func reportDeadBoot(hub *logs.Hub, id string, rec bootRecord, crashPath string) {
	msg := "the previous run ended unexpectedly"
	kv := []any{"severity", "critical", "boot", id, "pid", rec.PID, "role", rec.Role,
		"version", rec.Version, "host", rec.Host, "started", rec.Started.Format(time.RFC3339)}
	if head := readHead(crashPath, crashHead); head != "" {
		first, _, _ := strings.Cut(head, "\n")
		msg = "the previous run crashed: " + first
		dst := filepath.Join(hub.Root(), "system", "crash-"+id+".txt")
		if fsperm.MkdirAll(filepath.Dir(dst)) == nil && os.Rename(crashPath, dst) == nil {
			crashPath = dst
		}
		kv = append(kv, "file", crashPath, "stack", head)
	}
	hub.Logf("system", logs.LevelCritical, msg, kv...)
}

func writeBootRecord(path string, rec bootRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return fsperm.WriteFile(path, b)
}

func readBootRecord(path string) (bootRecord, error) {
	var rec bootRecord
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &rec)
	}
	return rec, err
}

// readHead returns up to n bytes from the start of path ("" when it is empty
// or missing).
func readHead(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, n))
	return string(b)
}

func removeIfEmpty(path string) {
	if st, err := os.Stat(path); err == nil && st.Size() == 0 {
		_ = os.Remove(path)
	}
}
