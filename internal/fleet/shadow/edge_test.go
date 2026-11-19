package shadow

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/secrets"
)

// The hash tells apart every kind of value SQLite stores and the driver
// hands over, so that a value cannot change kind and hash the same.
func TestContentHash_ValueKinds(t *testing.T) {
	ctx := context.Background()
	hashOf := func(insert string) string {
		db, err := openMem(ctx, nil, false)
		must(t, err)
		defer db.Close()
		_, err = db.Exec(`CREATE TABLE v (id INTEGER PRIMARY KEY, x, b BLOB, d DATETIME, ok BOOLEAN); CREATE TABLE loose (x, y)`)
		must(t, err)
		_, err = db.Exec(insert)
		must(t, err)
		h, err := contentHash(ctx, db.DB, db.DB)
		must(t, err)
		return h
	}
	seen := map[string]string{}
	for _, insert := range []string{
		`SELECT 1`, // nothing
		`INSERT INTO v (x) VALUES (NULL)`,
		`INSERT INTO v (x) VALUES (1)`,
		`INSERT INTO v (x) VALUES (1.0)`,
		`INSERT INTO v (x) VALUES ('1')`,
		`INSERT INTO v (x) VALUES (x'31')`,
		`INSERT INTO v (x) VALUES ('')`,
		`INSERT INTO v (x) VALUES (x'')`,
		`INSERT INTO v (x) VALUES (0)`,
		`INSERT INTO v (x) VALUES ('a' || char(0) || 'b')`,
		`INSERT INTO v (x) VALUES ('a')`,
		`INSERT INTO v (b) VALUES (x'6162')`,
		`INSERT INTO v (b) VALUES ('ab')`,
		`INSERT INTO v (d) VALUES ('2026-11-18 10:00:00')`,
		`INSERT INTO v (d) VALUES ('2026-11-18 10:00:01')`,
		`INSERT INTO v (ok) VALUES (1)`,
		`INSERT INTO v (ok) VALUES (0)`,
		`INSERT INTO v (x) VALUES ('ab'), ('c')`,
		`INSERT INTO v (x) VALUES ('a'), ('bc')`,
		// A table without a key is hashed in the order of its rows.
		`INSERT INTO loose VALUES ('ab', 'c')`,
		`INSERT INTO loose VALUES ('a', 'bc')`,
		`INSERT INTO loose VALUES ('ab', 'c'), ('d', 'e')`,
	} {
		h := hashOf(insert)
		if other, dup := seen[h]; dup {
			t.Errorf("%q hashes the same as %q", insert, other)
		}
		seen[h] = insert
		if again := hashOf(insert); again != h {
			t.Errorf("%q hashes differently the second time", insert)
		}
	}
	// A database that cannot be read has no hash.
	db, err := openMem(ctx, nil, false)
	must(t, err)
	must(t, db.Close())
	if h, err := contentHash(ctx, db.DB, db.DB); err == nil || h != "" {
		t.Errorf("the hash of a closed database: %q, %v", h, err)
	}
	if _, _, err := db.serialize(ctx); err == nil {
		t.Error("serializing a closed database: no error")
	}
	if db.Driver() == nil {
		t.Error("an in-memory database with no driver")
	}
}

type failingProtector struct{ protect, unprotect error }

func (failingProtector) Name() string { return "failing" }
func (f failingProtector) Protect(p []byte) ([]byte, error) {
	if f.protect != nil {
		return nil, f.protect
	}
	return xorProtector{}.Protect(p)
}
func (f failingProtector) Unprotect(c []byte) ([]byte, error) { return nil, f.unprotect }

// A protector that fails: no key file is left half made, and none is opened.
func TestKeyFile_ProtectorFailures(t *testing.T) {
	boom := errors.New("the protector is unwell")
	path := filepath.Join(t.TempDir(), "fleet.key")

	old := protector
	protector = func() (secrets.Protector, error) { return nil, boom }
	if _, err := CreateKey(path); !errors.Is(err, boom) {
		t.Errorf("CreateKey when the protector cannot be had: %v", err)
	}
	protector = old

	useProtector(t, failingProtector{protect: boom})
	if _, err := CreateKey(path); !errors.Is(err, boom) {
		t.Errorf("CreateKey when sealing fails: %v", err)
	}
	if exists(path) {
		t.Fatal("a failed CreateKey left a key file")
	}
	useProtector(t, failingProtector{unprotect: boom})
	k, err := CreateKey(path)
	must(t, err)
	must(t, k.Close())
	if _, err := OpenKey(path); !errors.Is(err, boom) {
		t.Errorf("OpenKey when unsealing fails: %v", err)
	}
	// Not a file at all.
	if _, err := OpenKey(filepath.Dir(path)); err == nil {
		t.Error("OpenKey of a directory: accepted")
	}
}

// A snapshot that is everything a snapshot must be, of databases that are
// sound, and that are not a machine's memory: it is damaged like any other,
// before it replaces anything.
func TestStore_ASoundDatabaseThatIsNotAMemory(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t, quick)
	f.note(hostA, "the shadow as it is")
	f.mint(hostA)
	must(t, f.Flush(ctx, hostA))
	before := readFile(t, f.path(hostA, shadowFile))

	odd, err := openMem(ctx, nil, false)
	must(t, err)
	_, err = odd.Exec(`CREATE VIEW episodes AS SELECT 1 AS id; CREATE VIEW facts AS SELECT 1 AS id`)
	must(t, err)
	image, _, err := odd.serialize(ctx)
	must(t, err)
	must(t, odd.Close())
	m := meta{Host: hostA, KeyID: f.key.ID(), Gen: 5}
	blob := craft(t, f.key, string(appendHeader(nil, m.KeyID, m.Gen)), f.to, gzip.DefaultCompression, metaLine(t, m, image, image), image, image)
	c := Copy{KeyID: m.KeyID, Gen: m.Gen, Hash: Hash(blob), OK: true}

	if got, err := f.Adopt(ctx, hostA, c, blob, nil); !errors.Is(err, ErrDamaged) || got != (Adopted{}) {
		t.Fatalf("Adopt: %+v, %v", got, err)
	}
	if got := f.facts(hostA); got != "the shadow as it is" {
		t.Fatalf("the shadow after the refused Adopt: %q", got)
	}
	if !bytes.Equal(readFile(t, f.path(hostA, shadowFile)), before) || exists(f.path(hostA, asideDir)) {
		t.Fatal("the refused Adopt changed the files")
	}
	// The same thing as the master's own snapshot.
	must(t, f.Store.Close())
	m.Embeddings = true
	must(t, os.WriteFile(f.path(hostA, shadowFile),
		craft(t, f.key, string(appendHeader(nil, m.KeyID, m.Gen)), f.to, gzip.NoCompression, metaLine(t, m, image, image), image, image), 0o600))
	f.open()
	if _, err := f.Acquire(ctx, hostA); !errors.Is(err, ErrDamaged) {
		t.Fatalf("Acquire: %v", err)
	}
	if st, _ := f.State(hostA); st.ShadowOK {
		t.Fatalf("State: %+v", st)
	}
	// Nothing can be numbered past a shadow that is not there.
	if _, err := f.Skip(ctx, hostA, 3); !errors.Is(err, ErrDamaged) {
		t.Fatalf("Skip: %v", err)
	}
	// A generation file that cannot be read is an error, not "none was made".
	must(t, os.Remove(f.path(hostA, childFile)))
	must(t, os.Mkdir(f.path(hostA, childFile), 0o700))
	if _, _, err := f.Newest(hostA); err == nil || errors.Is(err, ErrNoCopy) {
		t.Fatalf("Newest with a directory in the file's place: %v", err)
	}
}

// A snapshot that will not write is reported and tried again at the next
// look, and nothing claims to be on disk that is not.
func TestStore_AFailedWriteIsTriedAgain(t *testing.T) {
	ctx := context.Background()
	var failShadow, failChild, denied atomic.Int32
	diskFull := errors.New("no space left on device")
	real := replaceFile
	replaceFile = func(path string, data []byte) error {
		switch name := filepath.Base(path); {
		case denied.Load() > 0:
			denied.Add(-1)
			return &fs.PathError{Op: "rename", Path: path, Err: fs.ErrPermission}
		case name == shadowFile && failShadow.Load() > 0:
			failShadow.Add(-1)
			return diskFull
		case name == childFile && failChild.Load() > 0:
			failChild.Add(-1)
			return diskFull
		}
		return real(path, data)
	}
	t.Cleanup(func() { replaceFile = real })

	f := newFleet(t, quick)
	failShadow.Store(3)
	f.note(hostA, "web1 runs nginx")
	waitFor(t, "the snapshot, after three failures", func() bool { return exists(f.path(hostA, shadowFile)) })
	if failShadow.Load() != 0 {
		t.Fatal("the snapshot was written without the failures being used up")
	}
	reported := f.errs.take()
	if len(reported) != 3 || !errors.Is(reported[0], diskFull) {
		t.Fatalf("reported: %v", reported)
	}
	// A generation that cannot be written is not counted as made.
	failChild.Store(1)
	if c, made, err := f.Mint(ctx, hostA); !errors.Is(err, diskFull) || made || c != (Copy{}) {
		t.Fatalf("Mint with a failing disk: %+v, %v, %v", c, made, err)
	}
	if c := f.mint(hostA); c.Gen != 1 {
		t.Fatalf("the generation after the failed one is %d, want 1", c.Gen)
	}
	// Its snapshot fails after the generation file was written: the number
	// is taken, and the snapshot that records it is written at the next
	// chance, with nothing else having changed.
	f.note(hostA, "web1's /var fills up")
	must(t, f.Flush(ctx, hostA)) // so that the next write of the snapshot is Mint's own
	failShadow.Store(1)
	if _, _, err := f.Mint(ctx, hostA); !errors.Is(err, diskFull) {
		t.Fatalf("Mint whose snapshot fails: %v", err)
	}
	must(t, f.Flush(ctx, hostA))
	f.restart()
	if c, made, _ := f.Mint(ctx, hostA); made || c.Gen != 2 {
		t.Fatalf("after a restart: generation %d, made %v", c.Gen, made)
	}
	f.note(hostA, "a third thing")
	if c := f.mint(hostA); c.Gen != 3 {
		t.Fatalf("the next generation is %d, want 3", c.Gen)
	}
	// Refused because someone has the file open (Windows): tried again at
	// once, a few times, and the caller never hears of it.
	denied.Store(3)
	f.note(hostA, "a fourth thing")
	must(t, f.Flush(ctx, hostA))
	if denied.Load() != 0 {
		t.Fatal("the refusals were not used up")
	}
	denied.Store(50)
	f.note(hostA, "a fifth thing")
	if err := f.Flush(ctx, hostA); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Flush refused for good: %v", err)
	}
	denied.Store(0)
	f.restart()
	if got := f.facts(hostA); got != "a fifth thing | a fourth thing | a third thing | web1 runs nginx | web1's /var fills up" {
		t.Fatalf("after all that the shadow holds %q", got)
	}
	f.errs.take() // the passes that met the refusals said so
}

// A shadow whose database is in the middle of something is left for the next
// look; nobody waits on it and nothing is reported.
func TestStore_ABusyShadowIsLookedAtLater(t *testing.T) {
	f := newFleet(t, quick)
	p := f.acquire(hostA)
	defer p.Release()
	tx, err := p.Semantic.DB().Begin()
	must(t, err)
	_, err = tx.Exec(`INSERT INTO facts (agent_id, fact, first_seen, last_confirmed, embedder_id, embedding) VALUES ('main', 'in a transaction', 1, 1, 'e', x'00')`)
	must(t, err)
	time.Sleep(busyWait + 150*time.Millisecond) // several looks come and go
	if exists(f.path(hostA, shadowFile)) {
		t.Fatal("a snapshot was written in the middle of a transaction")
	}
	must(t, tx.Commit())
	waitFor(t, "the snapshot, once the transaction is done", func() bool { return exists(f.path(hostA, shadowFile)) })
	if errs := f.errs.take(); len(errs) != 0 {
		t.Fatalf("a busy database was reported: %v", errs)
	}
}

// A master holding another fleet key than the one its files were written
// under: it reads nothing, guesses at nothing and replaces nothing. With the
// recovery identity the memory comes back whole, vectors and all, and the old
// key's files are set aside, not written over.
func TestStore_FilesUnderAnotherFleetKey(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t, quick)
	f.note(hostA, "web1 runs nginx")
	made := f.mint(hostA)
	must(t, f.Store.Close())
	oldShadow, oldChild := readFile(t, f.path(hostA, shadowFile)), readFile(t, f.path(hostA, childFile))

	// The key file was lost and a new key made; the recovery identity is the
	// same one the operator kept.
	newKey := testKey(t)
	s, err := OpenStore(f.dir, newKey, f.recipient, quick)
	must(t, err)
	defer func() { _ = s.Close() }()
	if _, err := s.Acquire(ctx, hostA); !errors.Is(err, ErrForeignKey) {
		t.Fatalf("Acquire under another key: %v, want ErrForeignKey", err)
	}
	st, err := s.State(hostA)
	if err != nil || st.ShadowOK || !st.ShadowForeign || st.Gen != 0 || st.Hash != "" {
		t.Fatalf("State under another key: %+v, %v", st, err)
	}
	// Not "lost", and not "rebuild from a copy", even with a child that has
	// nothing or has copies this key reads: the memory is there, unread.
	for _, child := range [][]Copy{nil, {cp(st.KeyID, 9, hashY, true)}} {
		if d := Decide(st, child); d != (Decision{Action: NeedsRecoveryKey}) {
			t.Fatalf("Decide under another key: %+v", d)
		}
	}
	if _, _, err := s.Newest(hostA); !errors.Is(err, ErrNoCopy) {
		t.Fatalf("Newest under another key: %v, want ErrNoCopy", err)
	}
	if _, _, err := s.Mint(ctx, hostA); err != nil {
		t.Fatalf("Mint of a shadow that is not loaded: %v", err)
	}
	if !bytes.Equal(readFile(t, f.path(hostA, shadowFile)), oldShadow) || !bytes.Equal(readFile(t, f.path(hostA, childFile)), oldChild) {
		t.Fatal("the other key's files were changed")
	}

	// The operator brings the recovery identity, and the master's own
	// snapshot is read with it.
	recovery, err := ParseRecovery(f.recovery)
	must(t, err)
	defer recovery.Close()
	keyID, gen, err := Peek(oldShadow)
	must(t, err)
	got, err := s.Adopt(ctx, hostA, Copy{KeyID: keyID, Gen: gen, Hash: Hash(oldShadow)}, oldShadow, recovery)
	must(t, err)
	if got.Reembed || got.Gen != made.Gen+1 || got.Aside == "" {
		t.Fatalf("Adopt of the master's own snapshot: %+v", got)
	}
	p, err := s.Acquire(ctx, hostA)
	must(t, err)
	var facts, withVectors int
	must(t, p.Semantic.DB().QueryRow(`SELECT count(*), sum(length(embedding) > 0) FROM facts`).Scan(&facts, &withVectors))
	p.Release()
	if facts != 1 || withVectors != 1 {
		t.Fatalf("the recovered shadow has %d facts, %d with vectors", facts, withVectors)
	}
	// Both of the old key's files are in the aside directory, byte for byte.
	aside := files(t, f.path(hostA, asideDir))
	if len(aside) != 2 {
		t.Fatalf("set aside: %v", aside)
	}
	var kept [][]byte
	for _, name := range aside {
		kept = append(kept, readFile(t, filepath.Join(f.path(hostA, asideDir), name)))
	}
	if !(bytes.Equal(kept[0], oldShadow) && bytes.Equal(kept[1], oldChild)) && !(bytes.Equal(kept[0], oldChild) && bytes.Equal(kept[1], oldShadow)) {
		t.Fatal("what was set aside is not the old key's two files")
	}
	// And the memory goes on under the new key.
	c, _, err := s.Newest(hostA)
	if err != nil || c.Gen != made.Gen+1 || c.KeyID == made.KeyID {
		t.Fatalf("the newest generation after the recovery: %+v, %v", c, err)
	}
	must(t, s.Close())
	s, err = OpenStore(f.dir, newKey, f.recipient, quick)
	must(t, err)
	if st, err := s.State(hostA); err != nil || st != (Master{KeyID: c.KeyID, Gen: c.Gen, Hash: c.Hash, ShadowOK: true}) {
		t.Fatalf("State after the recovery: %+v, %v", st, err)
	}
}
