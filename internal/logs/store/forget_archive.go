package store

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"tenant/internal/fsperm"
)

// The log book's archives hold events that left the database:
// <logs root>/archive/<log>-<first>-<last>.ndjson.gz, one JSON event per line,
// and for security a closing (rec, hash) line (§10.2). The archive writer
// (TEN-371) owns that shape; the line fields a privacy rewrite reads and
// changes are named here, to keep in step with it.
const (
	archiveSession = "session_id"
	archiveTrace   = "trace_id" // runtrace's 32 hex characters, or the ID's text
	archiveMsg     = "msg"
)

// archiveErased are what a security tombstone removes from an archived
// record; data_hash, rec and hash stay, so the chain still verifies.
var archiveErased = []string{"data", "dtypes", "salt"}

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
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return 0, 0, err
	}
	var out bytes.Buffer
	kept := 0
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var obj map[string]json.RawMessage
		var session, trace string
		if json.Unmarshal(line, &obj) == nil {
			_ = json.Unmarshal(obj[archiveSession], &session)
			_ = json.Unmarshal(obj[archiveTrace], &trace)
		}
		switch {
		case !m.Selects(session, trace):
		case !security:
			dropped++
			continue
		default:
			tomb, _ := json.Marshal(Tombstone)
			if string(obj[archiveMsg]) == string(tomb) {
				break // tombstoned already
			}
			obj[archiveMsg] = tomb
			for _, k := range archiveErased {
				delete(obj, k)
			}
			line, _ = json.Marshal(obj) // a map of raw JSON always marshals
			tombstoned++
		}
		out.Write(line)
		out.WriteByte('\n')
		kept++
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	if !apply || dropped+tombstoned == 0 {
		return dropped, tombstoned, nil
	}
	_ = f.Close() // Windows can't replace a file that is still open
	if kept == 0 {
		return dropped, tombstoned, os.Remove(path)
	}
	tmp := path + ".tmp"
	w, err := fsperm.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return 0, 0, err
	}
	zw := gzip.NewWriter(w)
	_, err = zw.Write(out.Bytes())
	if err == nil {
		err = zw.Close()
	}
	if err == nil {
		err = w.Sync()
	}
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, 0, err
	}
	return dropped, tombstoned, nil
}
