package logs

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/fsperm"
	"tenant/internal/runtrace"
)

// What doctor, a process of its own, can see of the processes sharing a log
// book (TEN-375): who runs, and who runs on its text files alone.
//
// A process running on the mirror alone (Degraded) says so in
// <root>/boots/<boot id>.mirror-only, removed when logbook.db is written
// again or the hub closes: logbook.db itself can't say it, since it is what
// failed. Whether that process still runs is its boot lock's to say. Serve
// and the TUI hold boots/<boot id>.lock while they keep a boot record; a
// process whose store never opened keeps none, so it holds the lock itself
// while it is marked.

// HousekeeperLease names the housekeeper's lease in the store's leases table.
const HousekeeperLease = leaseName

// MinSecretLen is the scrubber's floor: a shorter known value is left alone.
const MinSecretLen = minSecretLen

const mirrorOnlyExt = ".mirror-only"

// MirrorOnly is a process that runs, or ran, on its text files alone.
type MirrorOnly struct {
	Boot    string    `json:"boot"`
	PID     int       `json:"pid"`
	Host    string    `json:"host"`
	Since   time.Time `json:"since"`
	Why     string    `json:"why"`
	Running bool      `json:"-"` // it holds its boot lock; false: it exited without clearing the marker
	Path    string    `json:"-"` // the marker
}

// markMirrorOnly writes this process's marker when it starts running on its
// text files alone (why), and removes it when it stops ("").
func (h *Hub) markMirrorOnly(why string) {
	if h.root == "" {
		return
	}
	id := runtrace.BootID()
	dir := filepath.Join(h.root, "boots")
	lockPath := filepath.Join(dir, id+".lock")
	if why == "" {
		_ = os.Remove(filepath.Join(dir, id+mirrorOnlyExt))
		if l := h.lb.mark.Swap(nil); l != nil {
			_ = l.Release()
			_ = os.Remove(lockPath) // after the release, for Windows
		}
		return
	}
	if fsperm.MkdirAll(dir) != nil {
		return
	}
	if h.lb.boot.Load() == nil && h.lb.mark.Load() == nil {
		if l, err := datalock.ExclusiveFile(lockPath); err == nil {
			h.lb.mark.Store(l)
		}
	}
	p := process()
	b, _ := json.Marshal(MirrorOnly{Boot: id, PID: p.PID, Host: p.Host, Since: time.Now().UTC(), Why: why})
	_ = fsperm.WriteFile(filepath.Join(dir, id+mirrorOnlyExt), b)
}

// MirrorOnlyProcesses lists the mirror-only markers under root, the logs
// root, and whether each one's process still runs. It changes nothing.
func MirrorOnlyProcesses(root string) ([]MirrorOnly, error) {
	dir := filepath.Join(root, "boots")
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []MirrorOnly
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), mirrorOnlyExt)
		if !ok || !ValidID(id) || !e.Type().IsRegular() {
			continue
		}
		var m MirrorOnly
		path := filepath.Join(dir, e.Name())
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &m) // one being written reads empty: its process runs
		}
		m.Boot, m.Path, m.Running = id, path, BootAlive(root, id)
		out = append(out, m)
	}
	return out, nil
}

// BootAlive reports whether the process with boot ID id still runs: it
// holds its boot lock under root. It creates nothing: with no lock file
// there is no process; a lock it can take it releases at once, and leaves
// the file for CheckBoots.
func BootAlive(root, id string) bool {
	if !ValidID(id) {
		return false
	}
	path := filepath.Join(root, "boots", id+".lock")
	if _, err := os.Stat(path); err != nil {
		return false
	}
	l, err := datalock.ExclusiveFile(path)
	if err != nil {
		return errors.Is(err, datalock.ErrBusy)
	}
	_ = l.Release()
	return false
}
