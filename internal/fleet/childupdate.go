package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"tenant/internal/secrets"
)

// child-update.json is where a child keeps the update it is in the middle
// of, and afterwards what became of it. It is a file and not memory because
// the process that starts an update is not the one that finishes it: the old
// binary starts the new one, and either may be stopped at any moment by the
// operating system. Whatever process starts next on the directory reads it
// and finishes the update, or undoes it (cmd/tenant, childupdate.go).
const childUpdateFileName = "child-update.json"

// The steps of an update in progress (ChildUpdate.Step).
const (
	// StepTrial: the old binary runs the new one beside itself, from where it
	// was unpacked, and waits for it to reach the master. The installed binary
	// is still the old one.
	StepTrial = "trial"
	// StepProven: the new binary reached the master in its trial. The old one
	// stops it and puts it in place.
	StepProven = "proven"
	// StepCommit: the new binary is (being) put in place. The process that
	// runs it next must reach the master by Deadline, or put the old one back.
	StepCommit = "commit"
)

// ChildUpdate is child-update.json. While an update is in progress its State
// is UpdateTrial and Step says how far it got; when it is over the State is
// UpdateDone or UpdateRolledBack, and stays so until the next update.
type ChildUpdate struct {
	UpdateReport
	Step     string    `json:"step,omitempty"`
	SHA256   string    `json:"sha256,omitempty"`  // of the archive that was sent
	Deadline time.Time `json:"deadline,omitzero"` // StepCommit: reach the master by then, or roll back
}

// ChildUpdatePath is child-update.json in dir.
func ChildUpdatePath(dir string) string { return filepath.Join(dir, childUpdateFileName) }

// ReadChildUpdate reads child-update.json; ok is false when there is none.
func ReadChildUpdate(dir string) (u ChildUpdate, ok bool, err error) {
	b, err := secrets.Peek(ChildUpdatePath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return u, false, nil
	}
	if err != nil {
		return u, false, fmt.Errorf("fleet: child update: %w", err)
	}
	if len(b) > maxChildBytes {
		return u, false, errors.New("fleet: child update: the file is too large")
	}
	if err := json.Unmarshal(b, &u); err != nil {
		return ChildUpdate{}, false, fmt.Errorf("fleet: child update: %w", err)
	}
	return u, true, nil
}

// WriteChildUpdate replaces child-update.json, owner-only, atomically.
func WriteChildUpdate(dir string, u ChildUpdate) error {
	b, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(ChildUpdatePath(dir), append(b, '\n'))
}
