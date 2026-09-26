package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func openWAL(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSnapshotSQLiteWhileAWriterIsActive(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "episodes.db")
	db := openWAL(t, src)
	if _, err := db.Exec(`CREATE TABLE ep (id INTEGER PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if _, err := db.Exec(`INSERT INTO ep (body) VALUES (?)`, fmt.Sprintf("row %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// Keep writing while the snapshot runs.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = db.Exec(`INSERT INTO ep (body) VALUES (?)`, fmt.Sprintf("late %d", i))
		}
	}()
	dst := filepath.Join(dir, "snap.db")
	err := snapshotSQLite(ctx, src, dst)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if err := integrityCheck(ctx, dst); err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dst + side); err == nil {
			t.Fatalf("snapshot left a %s file", side)
		}
	}
	snap, err := openReadOnly(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	var n int
	if err := snap.QueryRow(`SELECT count(*) FROM ep`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 200 {
		t.Fatalf("snapshot has %d rows, want at least the 200 committed before it", n)
	}
	if ok, err := isSQLite(dst); err != nil || !ok {
		t.Fatalf("isSQLite(snapshot) = %v, %v", ok, err)
	}
}

func TestSnapshotLeavesTheSourceAlone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "facts.db")
	db := openWAL(t, src)
	if _, err := db.Exec(`CREATE TABLE f (x)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshotSQLite(ctx, src, filepath.Join(dir, "copy.db")); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("snapshot changed the source database")
	}
}

func TestIntegrityCheckCatchesCorruption(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT); CREATE INDEX tv ON t(v)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		if _, err := db.Exec(`INSERT INTO t (v) VALUES (?)`, fmt.Sprintf("value-%04d-padding-padding", i)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Scribble over a page past the header.
	for i := 4096 * 2; i < 4096*2+512 && i < len(data); i++ {
		data[i] ^= 0x5a
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := integrityCheck(ctx, path); err == nil {
		t.Fatal("integrity_check passed a scribbled database")
	}
}

// An index that disagrees with its table makes integrity_check report rows
// instead of failing outright, the quieter kind of damage.
func TestIntegrityCheckReportsInconsistentIndex(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "idx.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	stmts := []string{
		`CREATE TABLE t (a TEXT, b TEXT)`,
		`CREATE INDEX ti ON t(a)`,
		`INSERT INTO t VALUES ('x', 'y'), ('p', 'q')`,
		`PRAGMA writable_schema = ON`,
		`UPDATE sqlite_master SET sql = 'CREATE INDEX ti ON t(b)' WHERE name = 'ti'`,
		`PRAGMA writable_schema = OFF`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Skipf("cannot build the fixture (%s): %v", s, err)
		}
	}
	db.Close()
	err = integrityCheck(ctx, path)
	if err == nil || !strings.Contains(err.Error(), "integrity_check") {
		t.Fatalf("err = %v, want integrity_check rows reported", err)
	}
}

func TestSchemaNewer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mk := func(name, ddl string) string {
		p := filepath.Join(dir, name)
		db, err := sql.Open("sqlite", "file:"+p)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ref := mk("ref.db", `CREATE TABLE a (id, x); CREATE TABLE b (id)`)
	older := mk("old.db", `CREATE TABLE a (id)`)
	newer := mk("new.db", `CREATE TABLE a (id, x, y); CREATE TABLE b (id); CREATE TABLE c (id)`)
	rs, err := ReadSchema(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	os_, err := ReadSchema(ctx, older)
	if err != nil {
		t.Fatal(err)
	}
	if got := os_.Newer(rs); len(got) != 0 {
		t.Fatalf("older schema flagged as newer: %v", got)
	}
	ns, err := ReadSchema(ctx, newer)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ns.Newer(rs), []string{"column a.y", "table c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Newer = %v, want %v", got, want)
	}
}
