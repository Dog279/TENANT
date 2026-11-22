package shadow

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
)

// helperEnv makes the test binary stand in for another Tenant process that
// owns the fleet's memory: "crash" writes, snapshots, writes again and waits
// to be killed; "load" loads one machine's memory and reports what it cost.
const helperEnv = "TENANT_SHADOW_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		if err := helper(mode); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helper(mode string) error {
	ctx := context.Background()
	key, err := OpenKey(os.Getenv("TENANT_SHADOW_KEY"))
	if err != nil {
		return err
	}
	defer key.Close()
	// No background snapshots: what reaches the disk is what this asks for.
	s, err := OpenStore(os.Getenv("TENANT_SHADOW_DIR"), key, os.Getenv("TENANT_SHADOW_RECIPIENT"), Options{FlushAfter: time.Hour, IdleAfter: time.Hour})
	if err != nil {
		return err
	}
	note := func(p *Pair, text string) error {
		if _, err := p.Episodic.Insert(ctx, &episodic.Episode{AgentID: "main", Prompt: text, Response: strings.Repeat(text+" ", 50), EmbedderID: "e", Embedding: testVec(1)}); err != nil {
			return err
		}
		_, err := p.Semantic.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: text, EmbedderID: "e", Embedding: testVec(2)})
		return err
	}
	switch mode {
	case "crash":
		p, err := s.Acquire(ctx, hostA)
		if err != nil {
			return err
		}
		if err := note(p, os.Getenv("TENANT_SHADOW_SAVED")); err != nil {
			return err
		}
		if err := s.Flush(ctx, hostA); err != nil {
			return err
		}
		fmt.Println("flushed")
		for range 50 {
			if err := note(p, os.Getenv("TENANT_SHADOW_LOST")); err != nil {
				return err
			}
		}
		// Work that would use SQLite's temporary space, if it were on disk.
		if _, err := p.Episodic.DB().Exec(`CREATE TEMP TABLE scratch AS SELECT * FROM episodes ORDER BY response DESC`); err != nil {
			return err
		}
		fmt.Println("written")
		time.Sleep(time.Hour) // the parent kills this process here
		return errors.New("not killed")
	case "load":
		before, _, _ := processMemory()
		start := time.Now()
		p, err := s.Acquire(ctx, hostA)
		if err != nil {
			return err
		}
		took := time.Since(start)
		episodes, _ := p.Episodic.Count(ctx, true)
		facts, _ := p.Semantic.Count(ctx, true, true)
		_, peak, ok := processMemory()
		// What stays for as long as the machine is loaded: the Go heap gives
		// back what the load used and no longer needs.
		runtime.GC()
		debug.FreeOSMemory()
		kept, _, _ := processMemory()
		fmt.Printf("%d %d %d %d %d %t\n", before, kept, peak, took.Nanoseconds(), episodes+facts, ok)
		p.Release()
		return s.Close()
	}
	return fmt.Errorf("no helper %q", mode)
}

func (f *fleet) helper(mode string, env ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(append(os.Environ(), helperEnv+"="+mode, "TENANT_SHADOW_DIR="+f.dir,
		"TENANT_SHADOW_KEY="+f.keyPath, "TENANT_SHADOW_RECIPIENT="+f.recipient), env...)
	return cmd
}

func token() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

const sqliteMagic = "SQLite format 3\x00"

// readable lists the files under root that hold any of the needles in the
// clear.
func readable(t testing.TB, root string, needles ...string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // gone between the listing and the look: a temporary file, renamed
		}
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		if d.Name() == lockFile {
			// Windows will not let another handle read a locked file. It
			// is empty, which its size shows.
			if info, err := d.Info(); err != nil || info.Size() != 0 {
				hits = append(hits, fmt.Sprintf("%s is not empty", path))
			}
			return nil
		}
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		raw, gone := readUnlessGone(t, path)
		if gone {
			return nil
		}
		for _, n := range needles {
			if bytes.Contains(raw, []byte(n)) {
				hits = append(hits, fmt.Sprintf("%s holds %q", path, n))
			}
		}
		return nil
	})
	must(t, err)
	return hits
}

// readUnlessGone reads path, trying again for a second while Windows refuses
// it (a sharing violation while the store has it open or renames it). A file
// that is gone meanwhile, a temporary one renamed into place, is forgiven:
// gone is true, and the walk reads it under its final name. A file that is
// there and still cannot be read fails the test.
func readUnlessGone(t testing.TB, path string) (raw []byte, gone bool) {
	t.Helper()
	var err error
	for range 200 {
		raw, err = os.ReadFile(path)
		switch {
		case err == nil:
			return raw, false
		case errors.Is(err, fs.ErrNotExist):
			return nil, true
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(err)
	return nil, false
}

// watch records the name of everything that appears under the roots until
// stop is called. It reads no file, so it gets in nobody's way.
func watch(roots ...string) (stop func() map[string]bool) {
	seen := map[string]bool{}
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			for _, root := range roots {
				_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
					if err == nil && path != root {
						rel, _ := filepath.Rel(root, path)
						seen[filepath.Base(root)+"/"+filepath.ToSlash(rel)] = true
					}
					return nil
				})
			}
			select {
			case <-quit:
				return
			case <-time.After(200 * time.Microsecond):
			}
		}
	}()
	return func() map[string]bool {
		close(quit)
		<-done
		return seen
	}
}

// The control for the next test: with SQLite's temporary storage on disk, the
// same work does make a temporary file, and sqliteTempFiles sees it. Without
// this, "no temporary file was seen" would prove nothing.
func sortSpills(t *testing.T, db *sql.DB) (during []string) {
	t.Helper()
	rows, err := db.Query(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 30000)
		SELECT i, hex(randomblob(150)) AS r FROM n ORDER BY r`)
	must(t, err)
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the sort returned nothing")
	}
	// Mid-result: the sorter has taken everything in and is holding it.
	during, _ = sqliteTempFiles()
	return during
}

func TestMemDB_SQLiteKeepsItsWorkingFilesInRAM(t *testing.T) {
	if raceEnabled {
		// Sorts and temporary tables of 30,000 rows in a SQLite that is itself
		// Go code: about seventy times slower under the race detector (3 s
		// becomes 4 minutes on four cores, and on CI's runner the package
		// ran past its ten), and nothing here runs beside anything else.
		t.Skip("minutes of single-threaded SQLite under the race detector; run without -race")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	for _, k := range []string{"TMP", "TEMP", "TMPDIR", "SQLITE_TMPDIR"} {
		t.Setenv(k, tmp)
	}
	_, supported := sqliteTempFiles()
	cwd := files(t, ".")

	// The control: an in-memory database as the driver opens one by default,
	// told to keep temporary storage in files.
	control, err := sql.Open("sqlite", ":memory:?_pragma=temp_store(1)")
	must(t, err)
	defer control.Close()
	control.SetMaxOpenConns(1)
	spilled := sortSpills(t, control)
	_, err = control.Exec(`CREATE TEMP TABLE big AS WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 30000) SELECT i, hex(randomblob(150)) AS r FROM n`)
	must(t, err)
	tempTable, _ := sqliteTempFiles()
	switch {
	case !supported:
		t.Log("this system gives no way to see SQLite's temporary files; only the settings are checked (UNVERIFIED here)")
	case len(spilled) == 0 || len(tempTable) == 0:
		t.Fatalf("the control made no temporary file (sort: %v, temp table: %v): the check below would pass whatever the shadow did", spilled, tempTable)
	default:
		t.Logf("control: a sort spilled to %v, a temporary table to %v", spilled, tempTable)
	}
	must(t, control.Close())
	if left, _ := sqliteTempFiles(); len(left) != 0 {
		t.Fatalf("the control's temporary files outlived it: %v", left)
	}

	// A shadow's database, from a serialized image as a loaded shadow is.
	p := newTestPair(t, nil, nil)
	fillSynthetic(t, p.es, p.ss, 60, 30)
	ep, sem := p.images(t)
	for name, image := range map[string][]byte{"episodic": ep, "semantic": sem, "new": nil} {
		db, err := openMem(ctx, image, image != nil)
		must(t, err)
		defer db.Close()
		for pragma, want := range map[string]string{"journal_mode": "memory", "temp_store": "2", "secure_delete": "1"} {
			var got string
			must(t, db.QueryRow("PRAGMA "+pragma).Scan(&got))
			if got != want {
				t.Errorf("%s: PRAGMA %s = %q, want %q", name, pragma, got, want)
			}
		}
		none := func(stage string) {
			t.Helper()
			if open, _ := sqliteTempFiles(); len(open) != 0 {
				t.Errorf("%s, %s: SQLite has temporary files open: %v", name, stage, open)
			}
			if now := files(t, tmp); len(now) != 0 {
				t.Errorf("%s, %s: files in the temporary directory: %v", name, stage, now)
			}
			if now := files(t, "."); !slices.Equal(now, cwd) {
				t.Errorf("%s, %s: files appeared beside the package: %v", name, stage, now)
			}
		}
		if during := sortSpills(t, db.DB); len(during) != 0 {
			t.Errorf("%s: a sort spilled to disk: %v", name, during)
		}
		none("after a sort")
		_, err = db.Exec(`CREATE TEMP TABLE big AS WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 30000) SELECT i, hex(randomblob(150)) AS r FROM n`)
		must(t, err)
		none("with a large temporary table")
		// A transaction large enough to spill a journal, rolled back; an
		// index build; a VACUUM, which copies the whole database.
		tx, err := db.Begin()
		must(t, err)
		_, err = tx.Exec(`CREATE TABLE filler AS SELECT * FROM temp.big`)
		must(t, err)
		_, err = tx.Exec(`UPDATE filler SET r = r || 'x'`)
		must(t, err)
		none("inside a large transaction")
		must(t, tx.Rollback())
		_, err = db.Exec(`CREATE TABLE kept AS SELECT * FROM temp.big; CREATE INDEX kept_r ON kept(r); DELETE FROM kept WHERE i % 2 = 0; VACUUM`)
		must(t, err)
		none("after an index build and a VACUUM")
		if _, _, err := db.serialize(ctx); err != nil {
			t.Fatal(err)
		}
		none("after serializing")
		// The stores never ask for another journal mode on these databases.
		// If something did: a loaded image is a file in SQLite's in-memory
		// file system, and so is its journal, whatever the mode; a database
		// that started empty refuses the change. Either way a transaction
		// that is committed and one that is rolled back leave no file.
		for _, mode := range []string{"WAL", "DELETE", "TRUNCATE", "PERSIST", "MEMORY"} {
			var got string
			must(t, db.QueryRow("PRAGMA journal_mode = "+mode).Scan(&got))
			if got == "wal" || (image == nil && got != "memory") {
				t.Errorf("%s: asked for journal_mode %s and got %q", name, mode, got)
			}
			for _, end := range []func(*sql.Tx) error{(*sql.Tx).Commit, (*sql.Tx).Rollback} {
				tx, err := db.Begin()
				must(t, err)
				_, err = tx.Exec(`UPDATE kept SET r = r || ?`, mode)
				must(t, err)
				none("in a transaction with journal_mode " + got)
				must(t, end(tx))
				none("after a transaction with journal_mode " + got)
			}
		}
	}
}

// The whole life of a shadow, watched: nothing readable is written, by the
// store or by SQLite, at any point.
func TestStore_NothingReadableTouchesTheDisk(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	for _, k := range []string{"TMP", "TEMP", "TMPDIR", "SQLITE_TMPDIR"} {
		t.Setenv(k, tmp)
	}
	canary := "canary-" + token()
	needles := []string{canary, sqliteMagic[:15]}

	// Every byte the store hands to the file system.
	var mu sync.Mutex
	written := map[string][][]byte{}
	real := replaceFile
	replaceFile = func(path string, data []byte) error {
		mu.Lock()
		written[filepath.Base(path)] = append(written[filepath.Base(path)], bytes.Clone(data))
		mu.Unlock()
		return real(path, data)
	}
	t.Cleanup(func() { replaceFile = real })

	f := newFleet(t, Options{FlushAfter: 5 * time.Millisecond, IdleAfter: 80 * time.Millisecond})
	stop := watch(f.dir, tmp)
	atRest := func(stage string) {
		t.Helper()
		for _, hit := range append(readable(t, f.dir, needles...), readable(t, tmp, needles...)...) {
			t.Errorf("%s: %s", stage, hit)
		}
		if open, _ := sqliteTempFiles(); len(open) != 0 {
			t.Errorf("%s: SQLite has temporary files open: %v", stage, open)
		}
	}

	// Writes, and the snapshots that follow them.
	p := f.acquire(hostA)
	for i := range 30 {
		addEpisode(t, p.Episodic, canary+" asked", strings.Repeat(canary+" answered. ", 100))
		addFact(t, p.Semantic, fmt.Sprintf("%s fact %d", canary, i))
		if i%10 == 0 {
			time.Sleep(12 * time.Millisecond) // let a snapshot land between writes
		}
	}
	waitFor(t, "a snapshot", func() bool { return exists(f.path(hostA, shadowFile)) })
	atRest("after writes")
	// Recall, with a sort that needs working space, looked at mid-result.
	rows, err := p.Episodic.DB().Query(`SELECT e.response || f.value FROM episodes e, json_each('[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20]') f ORDER BY 1 DESC`)
	must(t, err)
	if !rows.Next() {
		t.Fatal("the sort returned nothing")
	}
	atRest("in the middle of a sorted read")
	must(t, rows.Close())
	hits, err := p.Semantic.Search(ctx, semantic.Query{Keywords: "fact", Embedding: testVec(2000), K: 10})
	must(t, err)
	for _, h := range hits {
		must(t, p.Semantic.BumpAccess(ctx, []int64{h.Fact.ID}))
	}
	// A generation for the child, made from a copy without vectors.
	c := f.mint(hostA)
	atRest("after a generation was made")
	p.Release()
	// Idle: dropped from RAM, then loaded again.
	waitFor(t, "the idle shadow to be dropped", func() bool { return len(f.Loaded()) == 0 })
	atRest("after the shadow was dropped")
	if got := f.facts(hostA); !strings.Contains(got, canary+" fact 29") {
		t.Fatalf("the reloaded shadow lost its facts: %q", got)
	}
	// Replaced by a copy (the child wins), the old one set aside; re-embedded.
	waitFor(t, "the idle shadow to be dropped again", func() bool { return len(f.Loaded()) == 0 })
	_, blob, err := f.Newest(hostA)
	must(t, err)
	got, err := f.Adopt(ctx, hostA, c, blob, nil)
	must(t, err)
	if got.Aside == "" {
		t.Fatal("nothing was set aside")
	}
	reembed(t, f, hostA)
	atRest("after a copy was adopted")
	// A second machine, and a restart.
	f.note(hostB, canary+" on another machine")
	f.restart()
	atRest("after a restart")
	must(t, f.Store.Close())
	atRest("closed")
	seen := stop()

	// What was written: only whole snapshots, encrypted, each several times.
	mu.Lock()
	defer mu.Unlock()
	for name, blobs := range written {
		if name != shadowFile && name != childFile {
			t.Errorf("the store wrote a file called %q", name)
		}
		for _, blob := range blobs {
			_, _, err := Peek(blob)
			rest := blob[bytes.IndexByte(blob, '\n')+1:]
			if err != nil || !bytes.HasPrefix(rest, []byte("age-encryption.org/v1\n")) {
				t.Errorf("%s was written with something that is not a snapshot: %q", name, blob[:min(len(blob), 80)])
			}
			for _, n := range needles {
				if bytes.Contains(blob, []byte(n)) {
					t.Errorf("%s was written with %q readable in it", name, n)
				}
			}
		}
	}
	if len(written[shadowFile]) < 6 || len(written[childFile]) < 2 {
		t.Errorf("the test did not exercise the store: %d snapshots and %d generations written", len(written[shadowFile]), len(written[childFile]))
	}
	// What existed, at any moment: the snapshots, Replace's temporary files
	// for them, the lock, and what was set aside. Nothing in the temporary
	// directory, ever.
	allowed := regexp.MustCompile(`^fleet/(\.shadows\.lock|m[0-9a-f]{16}(/(shadow|child)\.snap(\.[0-9]+\.tmp)?|/aside(/[0-9]+-shadow\.snap)?)?)$`)
	var names []string
	for name := range seen {
		names = append(names, name)
		if !allowed.MatchString(name) {
			t.Errorf("a file that should not exist was seen: %s", name)
		}
	}
	slices.Sort(names)
	if len(names) < 8 {
		t.Errorf("the watcher saw too little to mean anything: %v", names)
	}
	// And at rest, everything in a machine's directory is a snapshot.
	must(t, filepath.WalkDir(f.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || d.Name() == lockFile {
			return err
		}
		if _, _, err := Peek(readFile(t, path)); err != nil {
			t.Errorf("%s is not a snapshot", path)
		}
		return nil
	}))
}

// A crash between a write and its snapshot: the master process is killed
// with writes that exist only in its RAM. They are lost, the snapshot before
// them is not, and nothing readable is left behind anywhere.
func TestStore_ACrashLosesOnlyTheWritesSinceTheSnapshot(t *testing.T) {
	f := newFleet(t, quick)
	must(t, f.Store.Close()) // the other process owns the fleet's memory
	tmp := filepath.Join(t.TempDir(), "tmp")
	must(t, os.Mkdir(tmp, 0o700))
	saved, lost := "saved-"+token(), "lost-"+token()
	needles := []string{saved, lost, sqliteMagic[:15]}

	cmd := f.helper("crash", "TENANT_SHADOW_SAVED="+saved, "TENANT_SHADOW_LOST="+lost,
		"TMP="+tmp, "TEMP="+tmp, "TMPDIR="+tmp, "SQLITE_TMPDIR="+tmp)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	must(t, err)
	must(t, cmd.Start())
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	expect := func(want string) {
		t.Helper()
		select {
		case got, ok := <-lines:
			if !ok || got != want {
				t.Fatalf("the other process said %q (open: %v), want %q; its stderr: %s", got, ok, want, stderr.String())
			}
		case <-time.After(2 * time.Minute):
			t.Fatalf("the other process did not say %q; its stderr: %s", want, stderr.String())
		}
	}
	nothingReadable := func(stage string) {
		t.Helper()
		for _, hit := range append(readable(t, f.dir, needles...), readable(t, tmp, needles...)...) {
			t.Errorf("%s: %s", stage, hit)
		}
		if left := files(t, tmp); len(left) != 0 {
			t.Errorf("%s: files in the temporary directory: %v", stage, left)
		}
	}
	expect("flushed")
	expect("written")
	// It is alive, with fifty writes and a temporary table that no snapshot
	// holds. Only it can open the shadows.
	nothingReadable("while the writes are only in RAM")
	if _, err := OpenStore(f.dir, f.key, f.recipient, quick); !errors.Is(err, datalock.ErrBusy) {
		t.Fatalf("a second process opening the store: %v, want datalock.ErrBusy", err)
	}
	must(t, cmd.Process.Kill())
	_ = cmd.Wait()
	killed = true
	nothingReadable("after the crash")
	if got := files(t, filepath.Join(f.dir, hostA)); !slices.Equal(got, []string{shadowFile}) {
		t.Errorf("the machine's directory after the crash: %v", got)
	}

	// The next master to start has the snapshot, and not the writes after it.
	f.open()
	if got := f.facts(hostA); got != saved {
		t.Fatalf("after the crash the shadow holds %q, want only %q", got, saved)
	}
	f.with(hostA, func(p *Pair) {
		n, err := p.Episodic.Count(context.Background(), true)
		if err != nil || n != 1 {
			t.Fatalf("after the crash the shadow holds %d episodes (%v), want 1", n, err)
		}
	})
}

// database/sql throws a connection away when the driver calls it unusable,
// which this driver does while a cancelled query's interrupt is pending. For
// a database that lives in its one connection that is the loss of
// everything, so memDB does not pass that judgement on.
func TestMemDB_ACancelledQueryDoesNotLoseTheDatabase(t *testing.T) {
	slow := `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 5000000) SELECT count(*) FROM n`
	hammer := func(db *sql.DB) (lostAfter int) {
		must(t, func() error { _, err := db.Exec(`CREATE TABLE kept (x); INSERT INTO kept VALUES (1)`); return err }())
		for i := range 300 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%30)*50*time.Microsecond)
			var n int
			_ = db.QueryRowContext(ctx, slow).Scan(&n)
			cancel()
			if err := db.QueryRow(`SELECT count(*) FROM kept`).Scan(&n); err != nil || n != 1 {
				return i + 1
			}
		}
		return 0
	}
	plain, err := sql.Open("sqlite", ":memory:")
	must(t, err)
	defer plain.Close()
	plain.SetMaxOpenConns(1)
	if n := hammer(plain); n > 0 {
		t.Logf("for comparison: a plain in-memory database lost its table after %d cancelled queries", n)
	} else {
		t.Log("for comparison: a plain in-memory database survived 300 cancelled queries this time")
	}
	db, err := openMem(context.Background(), nil, false)
	must(t, err)
	defer db.Close()
	if n := hammer(db.DB); n > 0 {
		t.Fatalf("the database was lost after %d cancelled queries", n)
	}
}

// If the connection is lost all the same, the shadow is not carried on empty:
// every use fails, it is dropped, and the next use loads the last snapshot.
func TestStore_ALostDatabaseIsLoadedAgainFromItsSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t, Options{FlushAfter: 20 * time.Millisecond, IdleAfter: time.Hour})
	f.note(hostA, "on disk")
	must(t, f.Flush(ctx, hostA))
	p := f.acquire(hostA)
	// The loss: database/sql is told the connection is bad, and closes it.
	f.mu.Lock()
	sh := f.hosts[hostA]
	f.mu.Unlock()
	sh.mu.Lock()
	conn, err := sh.semantic.DB.Conn(ctx)
	must(t, err)
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
	sh.mu.Unlock()

	if _, err := p.Semantic.Count(ctx, true, true); !errors.Is(err, errMemLost) {
		t.Fatalf("a read of the lost database: %v, want errMemLost", err)
	}
	if _, err := p.Semantic.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: "into the void", EmbedderID: "e", Embedding: testVec(1)}); err == nil {
		t.Fatal("a write to the lost database succeeded")
	}
	waitFor(t, "the lost shadow to be dropped", func() bool { return len(f.Loaded()) == 0 })
	// The pass drops the shadow and then says why, so the report can be a
	// moment behind the drop (on CI's Windows runner it was).
	var reported []error
	waitFor(t, "the loss to be reported", func() bool {
		reported = append(reported, f.errs.take()...)
		return len(reported) > 0
	})
	if !errors.Is(reported[0], errMemLost) {
		t.Fatalf("the loss was reported as %v", reported)
	}
	p.Release()
	if got := f.facts(hostA); got != "on disk" {
		t.Fatalf("after the loss the shadow holds %q", got)
	}
	// And the empty database was never written over the snapshot.
	f.restart()
	if got := f.facts(hostA); got != "on disk" {
		t.Fatalf("after a restart the shadow holds %q", got)
	}
}

// The budget from the ticket: loading a machine's memory, at the size the
// measurement was taken on (683 facts and 286 episodes, 8.6 MB), cost about
// 40 ms and 26 MB.
func TestBudget_LoadingAMachinesMemory(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("a timing and memory budget: not under -short or the race detector")
	}
	f := newFleet(t, Options{FlushAfter: time.Hour, IdleAfter: time.Hour})
	f.with(hostA, func(p *Pair) { fillSynthetic(t, p.Episodic, p.Semantic, measuredFacts, measuredEpisodes) })
	must(t, f.Store.Close())
	onDisk := int64(len(readFile(t, f.path(hostA, shadowFile))))
	forChild := int64(len(readFile(t, f.path(hostA, childFile))))

	// Time: the fastest of several loads, which is the one least disturbed by
	// whatever else the machine is doing.
	var times []time.Duration
	for range 9 {
		f.open()
		start := time.Now()
		p := f.acquire(hostA)
		times = append(times, time.Since(start))
		p.Release()
		must(t, f.Store.Close())
	}
	slices.Sort(times)
	best, median := times[0], times[len(times)/2]

	// Memory: in a process that does nothing else, what a load adds at its
	// highest point, and what is still there once it is done.
	var added, kept int64 = -1, -1
	out, err := f.helper("load").Output()
	if err != nil {
		t.Fatalf("the loading process: %v", err)
	}
	var before, after, peak, nanos, records int64
	var ok bool
	if _, err := fmt.Sscan(string(out), &before, &after, &peak, &nanos, &records, &ok); err != nil {
		t.Fatalf("the loading process said %q: %v", out, err)
	}
	if records != measuredFacts+measuredEpisodes {
		t.Fatalf("the loading process found %d records", records)
	}
	if ok {
		added, kept = peak-before, after-before
	}
	ceiling := timeCeiling()
	t.Logf("%d records, %.1f MB snapshot on disk (%.2f MB for the child); load: best %v, median %v, in a fresh process %v (ceiling %v); RAM: %.1f MB more at the peak of the load, %.1f MB while loaded (%s/%s)",
		records, float64(onDisk)/1e6, float64(forChild)/1e6, best, median, time.Duration(nanos), ceiling,
		float64(added)/1e6, float64(kept)/1e6, runtime.GOOS, runtime.GOARCH)

	// The ceilings. Time: see timeCeiling. RAM while loaded: the budget's own
	// 26 MB, about twice what it is today. RAM at the peak of the load: twice
	// the budget, where today it is about the budget (the larger database is
	// in RAM twice while SQLite takes its copy, and the decryption leaves
	// garbage behind it). Three times today's cost fails each of them.
	if best > ceiling {
		t.Errorf("loading took %v at best, over the %v ceiling", best, ceiling)
	}
	const budget = 26 << 20
	if ok && kept > budget {
		t.Errorf("a loaded machine keeps %.1f MB, over the %d MB budget", float64(kept)/1e6, budget>>20)
	}
	if ok && added > 2*budget {
		t.Errorf("loading added %.1f MB at its peak, over the %d MB ceiling", float64(added)/1e6, 2*budget>>20)
	}
	if !ok {
		t.Log("this system gives no reading of the process's memory: the memory budget is UNVERIFIED here")
	}
}

// timeCeiling is the most a load may take: 55 ms on a machine as fast as the
// one the load was measured on, where it takes about 20 ms, and more in
// proportion on a slower one, never less. So three times today's cost fails
// anywhere, and a slow CI runner does not fail for being slow.
func timeCeiling() time.Duration {
	// A plain loop over 32 MB, the fastest of three runs: it measures the
	// machine, not this package. 28 ms where the 20 ms was measured.
	const here = 28 * time.Millisecond
	buf := make([]byte, 32<<20)
	ref := time.Hour
	for range 3 {
		start := time.Now()
		h := uint64(14695981039346656037)
		for _, b := range buf {
			h = (h ^ uint64(b)) * 1099511628211
		}
		runtime.KeepAlive(h)
		ref = min(ref, time.Since(start))
	}
	return 55 * time.Millisecond * max(ref, here) / here
}
