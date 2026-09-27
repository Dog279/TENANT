package logs

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/fsperm"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/store"
	"tenant/internal/runtrace"
)

// Boot records and crash capture (docs/design/logging-v2.md §12.2). Serve and
// the TUI, never a one-shot command (so a Ctrl-C never reads as a crash),
// keep for their lifetime:
//
//	boots/<boot_id>.lock  held exclusively until the process exits
//	a row in boots        in logbook.db: pid, role, version, host, start
//	                      time and a heartbeat every 30 s; an orderly stop
//	                      marks it clean
//	crash/<boot_id>.txt   the runtime's fatal output (debug.SetCrashOutput)
//
// The OS drops a lock when its process exits, so a row with no clean stop
// whose lock can be taken belongs to a process that died. Only the lock
// decides, never the heartbeat's age: after a laptop wakes, a live process
// would look long dead. The check runs at start and every minute in the
// housekeeper, so a serve crash while the TUI is open is reported within a
// minute. With no store (logs.store false, or it didn't open) nothing is
// kept: an unexpected exit goes unreported and a crash dump goes to stderr
// only.

var bootsEvery = time.Minute // the dead-process check; tests shorten it

// bootRec is this process's boot record. The hub holds its lock, so the lock
// lives as long as the hub, however the caller keeps the stop: an
// unreachable lock's file would be closed by the garbage collector, and the
// process read as dead.
type bootRec struct {
	id   string
	lock *datalock.Lock
}

// crashHead bounds how much of a crash dump its event carries (the log book
// caps values further). The whole dump stays readable in the system folder.
const crashHead = 64 << 10

// BeginBoot starts this process's boot record and crash capture as role,
// then files what earlier runs left behind. It returns the orderly stop, or
// nil when the process keeps no record: no store is attached, or starting
// one failed, which is logged.
func (h *Hub) BeginBoot(role string) (stop func()) {
	if h.store() == nil || h.root == "" {
		return nil
	}
	stop, err := h.beginBoot(runtrace.BootID(), role)
	if err != nil {
		h.emit(catalog.NoBootRecord, "err", err)
	}
	_ = h.CheckBoots(context.Background())
	return stop
}

func (h *Hub) beginBoot(id, role string) (func(), error) {
	boots, crashDir := filepath.Join(h.root, "boots"), filepath.Join(h.root, "crash")
	if err := errors.Join(fsperm.MkdirAll(boots), fsperm.MkdirAll(crashDir)); err != nil {
		return nil, err
	}
	lockPath, crashPath := filepath.Join(boots, id+".lock"), filepath.Join(crashDir, id+".txt")
	lock, err := datalock.ExclusiveFile(lockPath)
	if err != nil {
		return nil, err
	}
	f, err := fsperm.OpenFile(crashPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err == nil {
		err = debug.SetCrashOutput(f, debug.CrashOptions{}) // it keeps its own copy of the handle
		_ = f.Close()
	}
	if err == nil {
		p := process()
		err = h.store().BeginBoot(context.Background(), store.Boot{ID: id, PID: p.PID, Role: role,
			Version: p.Version, Host: p.Host, Started: time.Now().UTC()})
	}
	release := func() {
		// Windows can't delete a file the runtime still holds: release it first.
		_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		removeIfEmpty(crashPath)
		_ = lock.Release()
		_ = os.Remove(lockPath) // after the release, for Windows too
	}
	if err != nil {
		release()
		return nil, err
	}
	h.lb.boot.Store(&bootRec{id: id, lock: lock})
	return func() {
		h.lb.boot.Store(nil)
		// ponytail: a store failing right now leaves the row unclean, and the
		// next start reports an unexpected end; retry if that ever shows up.
		if st := h.store(); st != nil {
			_ = st.EndBoot(context.Background(), id, time.Now())
		}
		release()
	}, nil
}

// CheckBoots files each earlier process that ended without a clean stop,
// once: a crash when the runtime left its dump, else an unexpected end.
// Nothing of a process is read until its boot lock is ours, so a live
// process is never reported and its crash file never read. BeginBoot runs it
// at start, and the housekeeper every minute.
func (h *Hub) CheckBoots(ctx context.Context) error {
	st := h.store()
	if st == nil || h.root == "" {
		return nil
	}
	boots, err := st.Unfinished(ctx, runtrace.BootID())
	if err != nil {
		return err
	}
	for _, b := range boots {
		if !ValidID(b.ID) { // it names files: never a path
			continue
		}
		lockPath := filepath.Join(h.root, "boots", b.ID+".lock")
		lock, err := datalock.ExclusiveFile(lockPath)
		if err != nil {
			continue // its process is alive
		}
		if ok, err := st.MarkReported(ctx, b.ID); err == nil && ok {
			h.reportDead(ctx, st, b)
		}
		_ = lock.Release()
		_ = os.Remove(lockPath) // after the release: Windows can't delete an open file
	}
	return nil
}

// reportDead files one process that ended without a clean stop: a crash when
// the runtime left its fatal output, else an unexpected end (killed, power
// lost, os.Exit). The dump moves into the system folder, which the
// dashboard's file view lists; the event carries its head and its path.
func (h *Hub) reportDead(ctx context.Context, st *store.Store, b store.Boot) {
	last, _ := st.LastEvent(ctx, b.ID, b.Heartbeat)
	kv := []any{"last", last, "boot", b.ID, "pid", b.PID, "role", b.Role, "version", b.Version, "host", b.Host, "started", b.Started}
	crashPath := filepath.Join(h.root, "crash", b.ID+".txt")
	if head := readHead(crashPath, crashHead); head != "" {
		first, _, _ := strings.Cut(head, "\n")
		dst := filepath.Join(h.root, "system", "crash-"+b.ID+".txt")
		if fsperm.MkdirAll(filepath.Dir(dst)) == nil && os.Rename(crashPath, dst) == nil {
			crashPath = dst
		}
		h.emit(catalog.Crashed, append(kv, "panic", first, "file", crashPath, "stack", head)...)
		return
	}
	removeIfEmpty(crashPath)
	h.emit(catalog.Unexpected, kv...)
}

// ValidID reports whether id is in runtrace's form (a boot, trace or
// activity ID): 32 lowercase hex characters.
func ValidID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && hex.EncodeToString(b) == id
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
