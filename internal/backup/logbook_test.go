package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
)

// logbook.db is captured as a database while its writer keeps committing;
// the archives and a script's own files come along, the text mirror does not
// unless asked for; a restore brings back what was committed.
func TestLogbookIsBackedUpWithoutItsMirror(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t, false)
	logs := filepath.Join(tr.data, "logs")
	live := openWAL(t, filepath.Join(logs, "logbook.db"))
	for _, q := range []string{`CREATE TABLE events (seq INTEGER PRIMARY KEY, msg TEXT)`, `INSERT INTO events (msg) VALUES ('in the wal')`} {
		if _, err := live.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(logs, "archive", "security-1-2.ndjson.gz"), "archived")
	write(t, filepath.Join(logs, "tui", "2026-09-26.1.log"), "mirror\n")
	write(t, filepath.Join(logs, "deploy", "output.txt"), "a script's own file\n")
	res, err := Create(ctx, filepath.Join(tr.root, "b.tar.gz"), tr.opts(Keys{}))
	if err != nil {
		t.Fatal(err)
	}
	got := entryPaths(res.Manifest)
	for _, want := range []string{"data/logs/logbook.db", "data/logs/archive/security-1-2.ndjson.gz", "data/logs/deploy/output.txt"} {
		if !slices.Contains(got, want) {
			t.Errorf("backup is missing %s (has %v)", want, got)
		}
	}
	for _, never := range []string{"data/logs/tui/2026-09-26.log", "data/logs/tui/2026-09-26.1.log", "data/logs/logbook.db-wal"} {
		if slices.Contains(got, never) {
			t.Errorf("backup carries %s", never)
		}
	}
	for _, e := range res.Manifest.Entries {
		if e.Path == "data/logs/logbook.db" && e.Kind != KindSQLite {
			t.Errorf("logbook.db captured as %s", e.Kind)
		}
	}
	if _, err := live.Exec(`INSERT INTO events (msg) VALUES ('after the backup')`); err != nil {
		t.Fatalf("the open log book broke: %v", err)
	}

	dst := &tree{root: t.TempDir()}
	dst.cfg, dst.data = filepath.Join(dst.root, "cfg"), filepath.Join(dst.root, "dat")
	if _, err := Restore(ctx, res.Path, RestoreOptions{ConfigDir: dst.cfg, DataDir: dst.data}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dst.data, "logs", "logbook.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var msg string
	if err := db.QueryRow(`SELECT group_concat(msg) FROM events`).Scan(&msg); err != nil || msg != "in the wal" {
		t.Errorf("restored log book holds %q (%v), want the commit made before the backup", msg, err)
	}

	opts := tr.opts(Keys{})
	opts.IncludeMirror = true
	res, err = Create(ctx, filepath.Join(tr.root, "with-mirror.tar.gz"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := entryPaths(res.Manifest); !slices.Contains(got, "data/logs/tui/2026-09-26.1.log") {
		t.Errorf("IncludeMirror left the mirror out: %v", got)
	}
}

func TestLogbookFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "journal") // logs.dir, moved
	for _, c := range []struct {
		rel            string
		sqlite, mirror bool
	}{
		{"logbook.db", true, false},
		{"turns/2026-09-26.log", false, true},
		{"turns/2026-09-26.3.log", false, true},
		{"turns/notes.log", false, false},
		{"archive/2026-09-26.log", false, false},
		{"turns/deep/2026-09-26.log", false, false},
		{"2026-09-26.log", false, false},
		{"../logbook.db", false, false},
	} {
		db, mirror := logbookFile(filepath.Join(root, filepath.FromSlash(c.rel)), root)
		if db != c.sqlite || mirror != c.mirror {
			t.Errorf("%s: sqlite %v, mirror %v; want %v, %v", c.rel, db, mirror, c.sqlite, c.mirror)
		}
	}
}
