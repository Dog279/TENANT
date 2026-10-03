package selfupdate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// State is the update-state.json marker an update writes before it swaps the
// binary and removes once the new version is confirmed running. One left
// behind means an OS stop interrupted the update at Step; serve reports it at
// boot, doctor warns, and the next `tenant update` finishes or offers rollback.
type State struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Step    string    `json:"step"`
	Started time.Time `json:"started"`
}

// WriteState writes the marker atomically, owner-only.
func WriteState(path string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, 0o600, bytes.NewReader(b))
}

// ReadState reads the marker; ok is false when there is none.
func ReadState(path string) (s State, ok bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return s, true, nil
}

// ClearState removes the marker; none is not an error.
func ClearState(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
