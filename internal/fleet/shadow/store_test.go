package shadow

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/memory/semantic"
	"tenant/internal/secrets"
)

const hostA, hostB = "m00000000000000aa", "m00000000000000bb"

// fleet is a master's side for a test: its key, its recovery identity and
// its store of shadows.
type fleet struct {
	t         testing.TB
	dir       string // <data>/fleet
	keyPath   string
	key       *Key
	recovery  *secrets.LockedBuffer
	recipient string
	opt       Options
	*Store
	errs *problems
}

// problems collects what the background pass reports.
type problems struct {
	mu  sync.Mutex
	all []error
}

func (p *problems) add(_ string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.all = append(p.all, err)
}

func (p *problems) take() []error {
	p.mu.Lock()
	defer p.mu.Unlock()
	all := p.all
	p.all = nil
	return all
}

// quick is a store that looks at its shadows every few milliseconds and
// drops none.
var quick = Options{FlushAfter: 10 * time.Millisecond, IdleAfter: time.Hour}

func newFleet(t testing.TB, opt Options) *fleet {
	t.Helper()
	root := t.TempDir()
	f := &fleet{t: t, dir: filepath.Join(root, "data", "fleet"), keyPath: filepath.Join(root, "config", "fleet.key"), opt: opt, errs: &problems{}}
	var err error
	if f.key, err = CreateKey(f.keyPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.key.Close() })
	f.recovery, f.recipient = testRecovery(t)
	f.open()
	t.Cleanup(func() {
		_ = f.Store.Close()
		if errs := f.errs.take(); len(errs) > 0 {
			t.Errorf("the background pass reported: %v", errs)
		}
	})
	return f
}

func (f *fleet) open() {
	f.t.Helper()
	opt := f.opt
	opt.OnError = f.errs.add
	s, err := OpenStore(f.dir, f.key, f.recipient, opt)
	if err != nil {
		f.t.Fatal(err)
	}
	f.Store = s
}

// restart is the master going down in good order and coming back.
func (f *fleet) restart() {
	f.t.Helper()
	if err := f.Store.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.open()
}

func (f *fleet) path(host, name string) string { return filepath.Join(f.dir, host, name) }

func (f *fleet) acquire(host string) *Pair {
	f.t.Helper()
	p, err := f.Acquire(context.Background(), host)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

// with runs fn on host's stores and releases them.
func (f *fleet) with(host string, fn func(p *Pair)) {
	f.t.Helper()
	p := f.acquire(host)
	defer p.Release()
	fn(p)
}

func (f *fleet) mint(host string) Copy {
	f.t.Helper()
	c, _, err := f.Mint(context.Background(), host)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// texts is every fact in host's shadow, in order.
func (f *fleet) facts(host string) string {
	f.t.Helper()
	var out []string
	f.with(host, func(p *Pair) {
		all, err := p.Semantic.List(context.Background(), semantic.ListFilter{})
		must(f.t, err)
		for _, fact := range all {
			out = append(out, fact.Fact)
		}
	})
	slices.Sort(out)
	return strings.Join(out, " | ")
}

func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// readFile reads a file the store may be replacing at this moment, which on
// Windows fails for an instant; it tries again.
func readFile(t testing.TB, path string) []byte {
	t.Helper()
	var b []byte
	var err error
	for range 200 {
		if b, err = os.ReadFile(path); err == nil {
			return b
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(err)
	return nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// A machine with no memory: empty stores, and nothing is written for it.
func TestStore_AMachineWithNoMemory(t *testing.T) {
	f := newFleet(t, quick)
	f.with(hostA, func(p *Pair) {
		n, err := p.Episodic.Count(context.Background(), true)
		if err != nil || n != 0 {
			t.Fatalf("a new machine has %d episodes (%v)", n, err)
		}
		if _, err := p.Semantic.Search(context.Background(), semantic.Query{Keywords: "anything"}); err != nil {
			t.Fatal(err)
		}
	})
	m, err := f.State(hostA)
	if err != nil || m != (Master{KeyID: f.key.ID(), ShadowOK: true}) {
		t.Fatalf("State: %+v, %v", m, err)
	}
	if d := Decide(m, nil); d.Action != Nothing {
		t.Fatalf("Decide for a machine with nothing on either side: %+v", d)
	}
	if c, made, err := f.Mint(context.Background(), hostA); err != nil || made || c != (Copy{}) {
		t.Fatalf("Mint of nothing: %+v, %v, %v", c, made, err)
	}
	if _, _, err := f.Newest(hostA); !errors.Is(err, ErrNoCopy) {
		t.Fatalf("Newest of nothing: %v", err)
	}
	f.restart()
	if exists(filepath.Join(f.dir, hostA)) {
		t.Errorf("files were written for a machine with no memory: %v", files(t, filepath.Join(f.dir, hostA)))
	}
}

// A write is on disk within FlushAfter, encrypted, and a restarted master
// reads it back with its vectors.
func TestStore_AWriteReachesTheDisk(t *testing.T) {
	f := newFleet(t, quick)
	p := f.acquire(hostA)
	addEpisode(t, p.Episodic, "is nginx up on web1", "active (running)")
	id := addFact(t, p.Semantic, "web1 runs nginx behind haproxy")
	// Not released: a shadow in use is written out all the same.
	waitFor(t, "the snapshot", func() bool { return exists(f.path(hostA, shadowFile)) })
	first := readFile(t, f.path(hostA, shadowFile))
	if bytes.Contains(first, []byte("nginx")) || bytes.Contains(first, []byte("SQLite format 3")) {
		t.Fatal("the snapshot on disk is readable")
	}
	if keyID, gen, err := Peek(first); err != nil || keyID != f.key.ID() || gen != 0 {
		t.Fatalf("the snapshot's first line: %q %d %v", keyID, gen, err)
	}
	// A second write replaces it.
	addFact(t, p.Semantic, "web1's /var fills up")
	waitFor(t, "the second snapshot", func() bool { return !bytes.Equal(readFile(t, f.path(hostA, shadowFile)), first) })
	p.Release()
	p.Release() // twice is once

	f.restart()
	if got := f.facts(hostA); got != "web1 runs nginx behind haproxy | web1's /var fills up" {
		t.Fatalf("after a restart the shadow holds: %q", got)
	}
	f.with(hostA, func(p *Pair) {
		fact, err := p.Semantic.Get(context.Background(), id)
		if err != nil || len(fact.Embedding) != 32 || fact.Embedding[1] != testVec(2000)[1] {
			t.Fatalf("the vector did not come back: %v, %v", fact, err)
		}
		// The stores are the ordinary ones, on a database that lives in RAM.
		for pragma, want := range map[string]string{"journal_mode": "memory", "temp_store": "2", "foreign_keys": "1", "secure_delete": "1", "trusted_schema": "0", "cache_size": "-512"} {
			for _, db := range []*sql.DB{p.Episodic.DB(), p.Semantic.DB()} {
				var got string
				if err := db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
					t.Errorf("PRAGMA %s = %q (%v), want %q", pragma, got, err, want)
				}
			}
		}
		var file string
		must(t, p.Episodic.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&file))
		if file != "" {
			t.Errorf("the episodic database has a file: %q", file)
		}
	})
}

// Recall counts accesses, which is a write. It does not rewrite the snapshot
// on its own; the counts go to disk with the next write, Mint, Flush or drop.
func TestStore_RecallAloneWritesNoSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t, quick)
	p := f.acquire(hostA)
	defer p.Release()
	id := addFact(t, p.Semantic, "web1 runs nginx behind haproxy")
	waitFor(t, "the snapshot", func() bool { return exists(f.path(hostA, shadowFile)) })
	// The first look after a load does not know what is on disk and writes
	// once more; from then on it knows.
	must(t, f.Flush(ctx, hostA))
	before := readFile(t, f.path(hostA, shadowFile))
	for range 3 {
		must(t, p.Semantic.BumpAccess(ctx, []int64{id}))
		time.Sleep(5 * quick.FlushAfter)
	}
	if !bytes.Equal(readFile(t, f.path(hostA, shadowFile)), before) {
		t.Fatal("recall alone rewrote the snapshot")
	}
	// Re-embedding is the same: no content changed.
	must(t, p.Semantic.UpdateEmbedding(ctx, id, testVec(7), "another-embedder"))
	time.Sleep(5 * quick.FlushAfter)
	if !bytes.Equal(readFile(t, f.path(hostA, shadowFile)), before) {
		t.Fatal("a re-embed alone rewrote the snapshot")
	}
	must(t, f.Flush(ctx, hostA))
	if bytes.Equal(readFile(t, f.path(hostA, shadowFile)), before) {
		t.Fatal("Flush did not write the counts and the vector out")
	}
	must(t, f.Flush(ctx, hostA)) // nothing changed since: nothing to do
	p.Release()
	f.restart()
	f.with(hostA, func(p *Pair) {
		sig, err := p.Semantic.GetSignals(ctx, id)
		fact, err2 := p.Semantic.Get(ctx, id)
		if err != nil || err2 != nil || sig.AccessCount != 3 || fact.EmbedderID != "another-embedder" {
			t.Fatalf("after a restart: %d accesses, embedder %q (%v, %v)", sig.AccessCount, fact.EmbedderID, err, err2)
		}
	})
	if err := f.Flush(ctx, hostB); err != nil {
		t.Errorf("Flush of a machine that is not loaded: %v", err)
	}
}

// An idle shadow leaves RAM, with everything written and a generation made;
// the next use loads it again. One that is held is never dropped.
func TestStore_AnIdleShadowIsDropped(t *testing.T) {
	f := newFleet(t, Options{FlushAfter: 5 * time.Millisecond, IdleAfter: 40 * time.Millisecond})
	held := f.acquire(hostB)
	defer held.Release()
	addFact(t, held.Semantic, "db1 is the postgres primary")

	// Both are loaded while both are held. Looked at after hostA's release,
	// the idle pass (40ms) could drop it first: a macOS runner saw only hostB.
	f.with(hostA, func(p *Pair) {
		addFact(t, p.Semantic, "web1 runs nginx")
		if got := f.Loaded(); !slices.Equal(got, []string{hostA, hostB}) {
			t.Fatalf("Loaded: %v", got)
		}
	})
	waitFor(t, "the idle shadow to be dropped", func() bool { return slices.Equal(f.Loaded(), []string{hostB}) })
	// Nothing is left that was only in RAM: the snapshot is there, and so is
	// a generation for the child.
	c, blob, err := f.Newest(hostA)
	if err != nil || c.Gen != 1 || c.Hash != Hash(blob) {
		t.Fatalf("after the drop, the newest generation: %+v, %v", c, err)
	}
	if !exists(f.path(hostA, shadowFile)) {
		t.Fatal("after the drop there is no snapshot")
	}
	// The held one outlived several idle periods and still works.
	time.Sleep(3 * f.opt.IdleAfter)
	addFact(t, held.Semantic, "db1 has a replica, db2")
	if got := f.Loaded(); !slices.Equal(got, []string{hostB}) {
		t.Fatalf("Loaded while one is held: %v", got)
	}
	// Used again, the dropped one comes back from its snapshot.
	if got := f.facts(hostA); got != "web1 runs nginx" {
		t.Fatalf("reloaded shadow holds %q", got)
	}
	// Nothing new: dropping it again makes no second generation.
	waitFor(t, "the second drop", func() bool { return slices.Equal(f.Loaded(), []string{hostB}) })
	if c2, _, _ := f.Newest(hostA); c2 != c {
		t.Fatalf("an unchanged shadow got a new generation: %+v, was %+v", c2, c)
	}
}

func TestStore_OneProcessHoldsTheShadows(t *testing.T) {
	f := newFleet(t, quick)
	if _, err := OpenStore(f.dir, f.key, f.recipient, quick); !errors.Is(err, datalock.ErrBusy) {
		t.Fatalf("a second OpenStore: %v, want datalock.ErrBusy", err)
	}
	must(t, f.Store.Close())
	must(t, f.Store.Close()) // twice is once
	f.open()                 // and now it can be opened again
	if _, err := OpenStore(filepath.Join(f.dir, "other"), f.key, "age1nope", quick); err == nil {
		t.Fatal("a recovery recipient that is not one was accepted")
	}
	if _, err := OpenStore(filepath.Join(f.dir, "other"), f.key, "", quick); err == nil {
		t.Fatal("no recovery recipient was accepted")
	}
	if _, err := OpenStore(filepath.Join(f.keyPath, "under-a-file"), f.key, f.recipient, quick); err == nil {
		t.Fatal("a directory under a file was accepted")
	}
	// With no options it runs as Tenant runs it.
	plain, err := OpenStore(filepath.Join(t.TempDir(), "fleet"), f.key, f.recipient, Options{})
	must(t, err)
	if plain.opt.FlushAfter != 3*time.Second || plain.opt.IdleAfter != 10*time.Minute {
		t.Errorf("the defaults are %v and %v", plain.opt.FlushAfter, plain.opt.IdleAfter)
	}
	must(t, plain.Close())
}

func TestStore_ClosedAndBadMachineIDs(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t, quick)
	c, blob := opaque(f.key.ID(), 1, "x")
	for _, host := range []string{"", "..", "../other", `..\other`, "a/b", "A", "con", "m0000000000000000 "} {
		_, err1 := f.Acquire(ctx, host)
		_, _, err2 := f.Mint(ctx, host)
		_, _, err3 := f.Newest(host)
		_, err4 := f.State(host)
		_, err5 := f.Adopt(ctx, host, c, blob, nil)
		for i, err := range []error{err1, err2, err3, err4, err5, f.Flush(ctx, host), f.Remove(host), f.RemoveAside(host)} {
			if err == nil {
				t.Errorf("machine id %q accepted by call %d", host, i)
			}
		}
	}
	if got := files(t, filepath.Dir(f.dir)); !slices.Equal(got, []string{"fleet"}) {
		t.Fatalf("a bad machine id wrote outside the fleet directory: %v", got)
	}
	held := f.acquire(hostA)
	addFact(t, held.Semantic, "written before the close")
	must(t, f.Store.Close())
	_, err1 := f.Acquire(ctx, hostA)
	_, _, err2 := f.Mint(ctx, hostA)
	_, err3 := f.Adopt(ctx, hostA, c, blob, nil)
	for i, err := range []error{err1, err2, err3, f.Flush(ctx, hostA), f.Remove(hostA)} {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("call %d after Close: %v, want ErrClosed", i, err)
		}
	}
	// A pair still held at Close stops working; it does not write to a store
	// that has been saved and freed.
	if _, err := held.Semantic.Count(ctx, false, false); err == nil {
		t.Error("a pair held across Close still works")
	}
	held.Release()
	f.open()
	if got := f.facts(hostA); got != "written before the close" {
		t.Fatalf("Close did not save what a held pair had written: %q", got)
	}
}

// Many holders, the background pass, idle drops and reloads, generations and
// statements, all at once: every write that was acknowledged is there at the
// end. (Under the race detector, this is also the test of the locking.)
func TestStore_ConcurrentUse(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t, Options{FlushAfter: 2 * time.Millisecond, IdleAfter: 6 * time.Millisecond})
	hosts := []string{hostA, hostB, "m00000000000000cc"}
	const workers, rounds = 8, 25
	var wg sync.WaitGroup
	wrote := make([]int, len(hosts))
	var mu sync.Mutex
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				h := (w + i) % len(hosts)
				p, err := f.Acquire(ctx, hosts[h])
				if err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				id, err := p.Semantic.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: fmt.Sprintf("worker %d round %d", w, i), EmbedderID: "e", Embedding: testVec(1)})
				if err != nil {
					t.Errorf("Insert: %v", err)
				} else {
					mu.Lock()
					wrote[h]++
					mu.Unlock()
					_ = p.Semantic.BumpAccess(ctx, []int64{id})
				}
				if i%3 == 0 {
					time.Sleep(time.Duration(w) * time.Millisecond) // long enough, sometimes, to be dropped in between
				}
				p.Release()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 60 {
			h := hosts[i%len(hosts)]
			if _, _, err := f.Mint(ctx, h); err != nil {
				t.Errorf("Mint: %v", err)
			}
			if _, err := f.State(h); err != nil {
				t.Errorf("State: %v", err)
			}
			if _, _, err := f.Newest(h); err != nil && !errors.Is(err, ErrNoCopy) {
				t.Errorf("Newest: %v", err)
			}
			if err := f.Flush(ctx, h); err != nil {
				t.Errorf("Flush: %v", err)
			}
			_ = f.Loaded()
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	f.restart()
	for h, host := range hosts {
		var n int
		f.with(host, func(p *Pair) {
			must(t, p.Semantic.DB().QueryRow(`SELECT count(*) FROM facts`).Scan(&n))
		})
		if n != wrote[h] {
			t.Errorf("%s: %d facts after a restart, %d were written", host, n, wrote[h])
		}
		// And each machine's last generation is its memory as it ended.
		c, blob, err := f.Newest(host)
		must(t, err)
		snap, err := openSnapshot(ctx, bytes.NewReader(blob), f.key, host)
		must(t, err)
		must(t, snap.semantic.QueryRow(`SELECT count(*) FROM facts`).Scan(&n))
		snap.close()
		if n != wrote[h] {
			t.Errorf("%s: generation %d holds %d facts, %d were written", host, c.Gen, n, wrote[h])
		}
	}
}
