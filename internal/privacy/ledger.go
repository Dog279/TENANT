package privacy

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"tenant/internal/fsperm"
)

// ledgerFile records, in the data dir, each backup Tenant writes and each
// forget that removes something, one JSON object per line. A backup made
// before a forget keeps what the forget removed until it is rotated out;
// status counts those (docs/design/logging-v2.md §6.5).
const ledgerFile = "privacy-ledger.jsonl"

type ledgerLine struct {
	At     time.Time `json:"at"`
	Backup string    `json:"backup,omitempty"` // the backup file written
	Forget bool      `json:"forget,omitempty"` // a forget removed data
}

// NoteBackup records a backup Tenant wrote at path.
func NoteBackup(d Data, path string, at time.Time) error {
	return appendLedger(d, ledgerLine{At: at.UTC(), Backup: path})
}

func appendLedger(d Data, l ledgerLine) error {
	b, _ := json.Marshal(l) // a ledgerLine always marshals
	f, err := fsperm.OpenFile(d.path(ledgerFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// BackupsBeforeForget reports when a forget last removed data (zero if none
// has), and which of the backups Tenant wrote before it are still where it
// wrote them: each may hold what was removed. A backup moved elsewhere is
// out of its sight.
func BackupsBeforeForget(d Data) (last time.Time, backups []string, err error) {
	f, err := os.Open(d.path(ledgerFile))
	if errors.Is(err, fs.ErrNotExist) {
		return last, nil, nil
	}
	if err != nil {
		return last, nil, err
	}
	defer f.Close()
	var made []ledgerLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l ledgerLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue // a torn last line
		}
		if l.Forget && l.At.After(last) {
			last = l.At
		}
		if l.Backup != "" {
			made = append(made, l)
		}
	}
	for _, l := range made {
		if l.At.Before(last) && exists(l.Backup) {
			backups = append(backups, l.Backup)
		}
	}
	return last, backups, sc.Err()
}
