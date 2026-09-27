package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The log book's archives hold events that left the database:
// <logs root>/archive/<log>-<first>-<last>.ndjson.gz, one JSON event per line,
// and for security a closing (rec, hash) line (§10.2). The archive writer
// (TEN-371) owns that shape: archiveSelect writes a line's keys, and line
// names them.

// ForgetArchived rewrites the archives in dir (<logs root>/archive) without
// the events m selects. In a security file each such line becomes a
// tombstone, as Forget makes one; any other file drops it, and a file left
// with no lines is removed. Lines that are not events are kept as they are.
// With apply false nothing changes: it only counts what would.
//
// A rewrite goes to a temporary file that replaces the old one, so a reader
// sees one or the other, never half of each.
func ForgetArchived(dir string, m Match, apply bool) (dropped, tombstoned int, err error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.ndjson.gz"))
	if err != nil {
		return 0, 0, err
	}
	for _, path := range files {
		log, ok := archiveLog(filepath.Base(path))
		if !ok {
			continue // not an archive of the log book's shape: leave it alone
		}
		d, t, err := forgetArchive(path, log == SecurityLog, m, apply)
		if err != nil {
			return dropped, tombstoned, err
		}
		dropped += d
		tombstoned += t
	}
	return dropped, tombstoned, nil
}

// archiveLog reads the log out of <log>-<first>-<last>.ndjson.gz.
func archiveLog(name string) (string, bool) {
	name, ok := strings.CutSuffix(name, ".ndjson.gz")
	if i := strings.LastIndexByte(name, '-'); ok && i > 0 {
		if j := strings.LastIndexByte(name[:i], '-'); j > 0 {
			return name[:j], true
		}
	}
	return "", false
}

func forgetArchive(path string, security bool, m Match, apply bool) (dropped, tombstoned int, err error) {
	// readLines closes the file before writeArchive replaces it, which
	// Windows can't do to an open file.
	var kept [][]byte
	err = readLines(path, func(line []byte) error {
		var obj map[string]json.RawMessage
		var session, trace string
		if json.Unmarshal(line, &obj) == nil {
			_ = json.Unmarshal(obj["session_id"], &session)
			_ = json.Unmarshal(obj["trace_id"], &trace) // runtrace's 32 hex characters, or the ID's text
		}
		switch {
		case !m.Selects(session, trace):
		case !security:
			dropped++
			return nil
		default:
			tomb, _ := json.Marshal(Tombstone)
			if string(obj["msg"]) == string(tomb) {
				break // tombstoned already
			}
			obj["msg"] = tomb
			for _, k := range []string{"data", "dtypes", "salt"} { // data_hash, rec and hash stay, so the chain still verifies
				delete(obj, k)
			}
			line, _ = json.Marshal(obj) // a map of raw JSON always marshals
			tombstoned++
		}
		kept = append(kept, bytes.Clone(line))
		return nil
	})
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, 0, nil
	case err != nil:
		return 0, 0, err
	case !apply || dropped+tombstoned == 0:
		return dropped, tombstoned, nil
	case len(kept) == 0:
		return dropped, tombstoned, os.Remove(path)
	}
	if err := writeArchive(path, kept); err != nil {
		return 0, 0, err
	}
	return dropped, tombstoned, nil
}
