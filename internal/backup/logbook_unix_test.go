//go:build !windows

package backup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// sqliteShared is where SQLite takes its shared lock on a database file:
// the 510 bytes after the pending and reserved bytes.
const sqliteShared = 0x40000002

// TestLogbookLockProbe runs in a second process: it reports whether another
// process holds SQLite's shared lock on the database at
// TENANT_LOGBOOK_PROBE. POSIX shows a process only the locks of others.
func TestLogbookLockProbe(t *testing.T) {
	path := os.Getenv("TENANT_LOGBOOK_PROBE")
	if path == "" {
		t.Skip("probe process only")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	lk := unix.Flock_t{Type: unix.F_WRLCK, Start: sqliteShared, Len: 510}
	if err := unix.FcntlFlock(f.Fd(), unix.F_GETLK, &lk); err != nil {
		t.Fatal(err)
	}
	if lk.Type == unix.F_UNLCK {
		t.Fatal("unlocked")
	}
	os.Stdout.WriteString("held by " + strconv.Itoa(int(lk.Pid)) + "\n")
}

// A WAL connection holds a shared lock on its database for as long as it is
// open; it is how another process's last connection knows not to checkpoint
// and delete the WAL under it. A backup taken while this process has
// logbook.db open must leave that lock in place: opening and closing the raw
// file would drop it.
func TestBackupKeepsTheLocksOnAnOpenLogbook(t *testing.T) {
	tr := newTree(t, false)
	path := filepath.Join(tr.data, "logs", "logbook.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	live := openWAL(t, path)
	for _, q := range []string{`CREATE TABLE events (seq INTEGER PRIMARY KEY, msg TEXT)`, `INSERT INTO events (msg) VALUES ('one')`} {
		if _, err := live.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	probe := func() (string, bool) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLogbookLockProbe$")
		cmd.Env = append(os.Environ(), "TENANT_LOGBOOK_PROBE="+path)
		out, err := cmd.CombinedOutput()
		return string(out), err == nil
	}
	if out, held := probe(); !held {
		t.Fatalf("fixture: the open log book holds no shared lock:\n%s", out)
	}
	if _, err := Create(context.Background(), filepath.Join(tr.root, "b.tar.gz"), tr.opts(Keys{})); err != nil {
		t.Fatal(err)
	}
	if out, held := probe(); !held {
		t.Fatalf("the backup dropped this process's lock on logbook.db:\n%s", out)
	}
	if _, err := live.Exec(`INSERT INTO events (msg) VALUES ('two')`); err != nil {
		t.Fatalf("commit after the backup: %v", err)
	}
}
