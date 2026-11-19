package shadow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
)

// wire stands in for the fleet link (TEN-531): it carries statements and
// snapshots between a master's Store and one child's ChildStore, and acts on
// what Decide says, as the wiring will.
type wire struct {
	t      testing.TB
	master *fleet
	child  *ChildStore
	host   string

	needsRecoveryKey bool
	told             []string // what the operator is told
	asides           []string
}

func newWire(t testing.TB, master *fleet, host string) *wire {
	return &wire{t: t, master: master, child: newChildStore(t), host: host}
}

// reconnect runs "who wins" until there is nothing left to do, and returns
// the actions it took.
func (w *wire) reconnect() []Action {
	w.t.Helper()
	ctx := context.Background()
	var done []Action
	refused := map[Copy]bool{} // copies the child offered that did not verify on the master
	for range 12 {
		m, err := w.master.State(w.host)
		must(w.t, err)
		listed, err := w.child.List()
		must(w.t, err)
		// The child's statement crosses the wire as bytes.
		data, err := json.Marshal(listed)
		must(w.t, err)
		stated, err := ParseCopies(data)
		must(w.t, err)
		for i := range stated {
			if refused[stated[i]] {
				stated[i].OK = false
			}
		}
		d := Decide(m, stated)
		done = append(done, d.Action)
		switch d.Action {
		case Nothing, Lost:
			return done
		case NeedsRecoveryKey:
			w.needsRecoveryKey = true
			return done
		case Push:
			c, blob, err := w.master.Newest(w.host)
			must(w.t, err)
			err = w.child.Put(c, blob)
			if errors.Is(err, ErrStale) {
				// The child believes it holds something newer, which the
				// master has just found is not a copy of its making. The
				// master numbers its next generation past it.
				var past uint64
				for _, have := range stated {
					if have.KeyID == m.KeyID {
						past = max(past, have.Gen)
					}
				}
				_, err = w.master.Skip(ctx, w.host, past)
			}
			must(w.t, err)
		case Pull, Rebuild:
			c, blob := d.Copy, []byte(nil)
			if d.Own {
				c, blob, err = w.master.Newest(w.host)
			} else {
				blob, err = w.child.Get(c)
			}
			must(w.t, err)
			got, err := w.master.Adopt(ctx, w.host, c, blob, nil)
			if errors.Is(err, ErrDamaged) && !d.Own {
				refused[d.Copy] = true
				continue
			}
			must(w.t, err)
			if got.Aside != "" {
				w.asides = append(w.asides, got.Aside)
			}
			if d.Diverged {
				w.told = append(w.told, "the master's own generation was set aside: "+got.Aside)
			}
			if got.Reembed {
				reembed(w.t, w.master, w.host)
			}
		}
	}
	w.t.Fatalf("who wins did not settle: %v", done)
	return nil
}

// reembed is the existing re-embed command's work on one shadow: a vector for
// every row that has none.
func reembed(t testing.TB, f *fleet, host string) {
	t.Helper()
	ctx := context.Background()
	f.with(host, func(p *Pair) {
		eps, err := p.Episodic.List(ctx, episodic.ListFilter{IncludeTombstoned: true})
		must(t, err)
		for _, e := range eps {
			if len(e.Embedding) == 0 {
				must(t, p.Episodic.UpdateEmbedding(ctx, e.ID, testVec(float32(e.ID)), "embedder"))
			}
		}
		facts, err := p.Semantic.List(ctx, semantic.ListFilter{})
		must(t, err)
		for _, fact := range facts {
			if len(fact.Embedding) == 0 {
				must(t, p.Semantic.UpdateEmbedding(ctx, fact.ID, testVec(float32(fact.ID)), "embedder"))
			}
		}
	})
	must(t, f.Flush(ctx, host))
}

// vectors counts the facts in host's shadow that have a vector.
func vectors(f *fleet, host string) (with, without int) {
	f.with(host, func(p *Pair) {
		must(f.t, p.Semantic.DB().QueryRow(`SELECT coalesce(sum(length(embedding) > 0), 0), coalesce(sum(length(embedding) = 0), 0) FROM facts`).Scan(&with, &without))
	})
	return with, without
}

func (f *fleet) note(host, text string) {
	f.t.Helper()
	f.with(host, func(p *Pair) { addFact(f.t, p.Semantic, text) })
}

func copyDir(t testing.TB, from, to string) {
	t.Helper()
	must(t, os.CopyFS(to, os.DirFS(from)))
}

func actions(a ...Action) []Action { return a }

func TestMint(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t, quick)
	f.note(hostA, "web1 runs nginx")
	c1, made, err := f.Mint(ctx, hostA)
	if err != nil || !made || c1.Gen != 1 || c1.KeyID != f.key.ID() || !c1.OK {
		t.Fatalf("the first generation: %+v, %v, %v", c1, made, err)
	}
	// What was made for the child is what Newest hands out and State states.
	n, blob, err := f.Newest(hostA)
	if err != nil || n != c1 || Hash(blob) != c1.Hash {
		t.Fatalf("Newest: %+v, %v", n, err)
	}
	if m, err := f.State(hostA); err != nil || m != (Master{KeyID: f.key.ID(), Gen: 1, Hash: c1.Hash, ShadowOK: true}) {
		t.Fatalf("State: %+v, %v", m, err)
	}
	// It is the memory without its vectors, and its content hash is inside.
	snap, err := openSnapshot(ctx, bytes.NewReader(blob), f.key, hostA)
	must(t, err)
	var facts, withVectors int
	must(t, snap.semantic.QueryRow(`SELECT count(*), sum(length(embedding) > 0) FROM facts`).Scan(&facts, &withVectors))
	content, err := contentHash(ctx, snap.episodic.DB, snap.semantic.DB)
	must(t, err)
	if facts != 1 || withVectors != 0 || snap.Embeddings || snap.Gen != 1 || snap.Content != content {
		t.Fatalf("the generation holds %d facts, %d with vectors; meta %+v; content %s", facts, withVectors, snap.meta, content)
	}
	snap.close()
	// The shadow itself kept its vectors.
	if with, without := vectors(f, hostA); with != 1 || without != 0 {
		t.Fatalf("the shadow has %d facts with vectors and %d without", with, without)
	}

	// Nothing changed: no new generation. Recall and a re-embed change nothing.
	if c, made, err := f.Mint(ctx, hostA); err != nil || made || c != c1 {
		t.Fatalf("Mint with nothing new: %+v, %v, %v", c, made, err)
	}
	f.with(hostA, func(p *Pair) {
		must(t, p.Semantic.BumpAccess(ctx, []int64{1}))
		must(t, p.Semantic.UpdateEmbedding(ctx, 1, testVec(9), "another-embedder"))
	})
	if c, made, err := f.Mint(ctx, hostA); err != nil || made || c != c1 {
		t.Fatalf("Mint after a recall and a re-embed: %+v, %v, %v", c, made, err)
	}
	// Content changed: the next number, another file in place of the first.
	f.note(hostA, "web1's /var fills up")
	c2 := f.mint(hostA)
	if c2.Gen != 2 || c2.Hash == c1.Hash {
		t.Fatalf("the second generation: %+v", c2)
	}
	if got := files(t, filepath.Join(f.dir, hostA)); !slices.Equal(got, []string{childFile, shadowFile}) {
		t.Fatalf("the machine's directory holds %v", got)
	}

	// The numbers go on after a restart, and a shadow that is not in RAM has
	// nothing to mint and is not loaded to find that out.
	f.restart()
	if c, made, err := f.Mint(ctx, hostA); err != nil || made || c != c2 || len(f.Loaded()) != 0 {
		t.Fatalf("Mint of an unloaded shadow: %+v, %v, %v; loaded %v", c, made, err, f.Loaded())
	}
	f.note(hostA, "web1 was rebooted on Tuesday")
	if c3 := f.mint(hostA); c3.Gen != 3 {
		t.Fatalf("after a restart the next generation is %d, want 3", c3.Gen)
	}
	// Two machines: each its own numbers, files and content.
	f.note(hostB, "db1 is the postgres primary")
	if c := f.mint(hostB); c.Gen != 1 {
		t.Fatalf("another machine's first generation is %d", c.Gen)
	}
	if got := f.facts(hostB); got != "db1 is the postgres primary" {
		t.Fatalf("machine B's shadow holds %q", got)
	}
}

// A generation made just before a crash is on disk without the snapshot that
// records it. Its number is not used twice.
func TestMint_AfterACrashBetweenTheTwoFiles(t *testing.T) {
	f := newFleet(t, quick)
	f.note(hostA, "web1 runs nginx")
	f.mint(hostA)
	must(t, f.Store.Close())
	before := readFile(t, f.path(hostA, shadowFile)) // records generation 1
	f.open()
	f.note(hostA, "web1's /var fills up")
	c2 := f.mint(hostA)
	must(t, f.Store.Close())
	// The crash: generation 2's file is there, the snapshot is the older one.
	must(t, os.WriteFile(f.path(hostA, shadowFile), before, 0o600))
	f.open()
	if m, _ := f.State(hostA); m.Gen != 2 || m.Hash != c2.Hash {
		t.Fatalf("State after the crash: %+v", m)
	}
	if got := f.facts(hostA); got != "web1 runs nginx" {
		t.Fatalf("the shadow after the crash: %q", got)
	}
	// The shadow is at generation 1's content, and 2 is taken: the next is 3.
	if c := f.mint(hostA); c.Gen != 3 {
		t.Fatalf("the generation after the crash is %d, want 3", c.Gen)
	}
	f.restart()
	if c := f.mint(hostA); c.Gen != 3 {
		t.Fatalf("and it is recorded: after a restart the newest is %d", c.Gen)
	}
}

// The acceptance list of the spec, with function calls for the link.
func TestWhoWins_DeleteTheChildsCopiesAndTheyReturn(t *testing.T) {
	f := newFleet(t, quick)
	w := newWire(t, f, hostA)
	f.note(hostA, "web1 runs nginx")
	if got := w.reconnect(); !slices.Equal(got, actions(Nothing)) {
		t.Fatalf("before anything was minted: %v", got)
	}
	c1 := f.mint(hostA)
	if got := w.reconnect(); !slices.Equal(got, actions(Push, Nothing)) {
		t.Fatalf("the first push: %v", got)
	}
	if got := w.reconnect(); !slices.Equal(got, actions(Nothing)) {
		t.Fatalf("equal: %v", got)
	}
	for _, name := range files(t, w.child.dir) {
		must(t, os.Remove(filepath.Join(w.child.dir, name)))
	}
	if got := w.reconnect(); !slices.Equal(got, actions(Push, Nothing)) {
		t.Fatalf("after the child's copies were deleted: %v", got)
	}
	if _, err := w.child.Get(c1); err != nil {
		t.Fatalf("the copy did not return: %v", err)
	}
	// The child's files hold nothing readable.
	for _, name := range files(t, w.child.dir) {
		raw := readFile(t, filepath.Join(w.child.dir, name))
		for _, plain := range []string{"nginx", "web1", "SQLite format 3", hostA} {
			if bytes.Contains(raw, []byte(plain)) {
				t.Errorf("the child's copy shows %q", plain)
			}
		}
	}
}

func TestWhoWins_DeleteTheShadowAndItIsRebuilt(t *testing.T) {
	ctx := context.Background()
	for name, lose := range map[string]func(f *fleet){
		"the snapshot is deleted": func(f *fleet) { must(t, os.Remove(f.path(hostA, shadowFile))) },
		"the snapshot is damaged": func(f *fleet) {
			raw := readFile(t, f.path(hostA, shadowFile))
			raw[len(raw)/2] ^= 1
			must(t, os.WriteFile(f.path(hostA, shadowFile), raw, 0o600))
		},
		"the snapshot is cut short": func(f *fleet) {
			raw := readFile(t, f.path(hostA, shadowFile))
			must(t, os.WriteFile(f.path(hostA, shadowFile), raw[:len(raw)-40], 0o600))
		},
		"the whole directory is deleted": func(f *fleet) { must(t, os.RemoveAll(filepath.Join(f.dir, hostA))) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFleet(t, quick)
			w := newWire(t, f, hostA)
			f.note(hostA, "web1 runs nginx")
			f.note(hostA, "web1's /var fills up")
			f.mint(hostA)
			w.reconnect()
			must(t, f.Store.Close())
			lose(f)
			f.open()

			deleted := name == "the whole directory is deleted"
			if !deleted {
				// The shadow is not there, and the master does not start an
				// empty one in its place.
				if _, err := f.Acquire(ctx, hostA); !errors.Is(err, ErrDamaged) {
					t.Fatalf("Acquire of the lost shadow: %v, want ErrDamaged", err)
				}
				if m, _ := f.State(hostA); m.ShadowOK {
					t.Fatalf("State still says the shadow is there: %+v", m)
				}
			}
			got := w.reconnect()
			want := actions(Rebuild, Nothing)
			if deleted {
				want = actions(Pull, Nothing) // nothing on the master says there ever was one
			}
			if !slices.Equal(got, want) {
				t.Fatalf("who wins: %v, want %v", got, want)
			}
			if got := f.facts(hostA); got != "web1 runs nginx | web1's /var fills up" {
				t.Fatalf("the rebuilt shadow holds %q", got)
			}
			// It came without vectors and was re-embedded.
			if with, without := vectors(f, hostA); with != 2 || without != 0 {
				t.Fatalf("after the rebuild %d facts have vectors and %d do not", with, without)
			}
			// What was there, readable or not, was moved aside, not overwritten.
			if name == "the snapshot is damaged" || name == "the snapshot is cut short" {
				if len(w.asides) != 1 || !exists(w.asides[0]) {
					t.Fatalf("the damaged snapshot was not kept aside: %v", w.asides)
				}
			}
			// The rebuilt shadow is the generation both sides have: no new one.
			if c, made, _ := f.Mint(ctx, hostA); made || c.Gen != 1 {
				t.Fatalf("a rebuild made generation %d", c.Gen)
			}
			f.restart()
			if with, _ := vectors(f, hostA); with != 2 {
				t.Fatalf("the re-embedded vectors did not reach the disk: %d", with)
			}
		})
	}
}

func TestWhoWins_RestoreTheMasterFromAnOldBackup(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t, quick)
	w := newWire(t, f, hostA)
	f.note(hostA, "one")
	f.mint(hostA)
	w.reconnect()
	must(t, f.Store.Close())
	backup := filepath.Join(t.TempDir(), "backup")
	copyDir(t, f.dir, backup)
	f.open()

	f.note(hostA, "two")
	f.mint(hostA)
	w.reconnect()
	f.note(hostA, "three")
	c3 := f.mint(hostA)
	w.reconnect()

	restore := func() {
		must(t, f.Store.Close())
		must(t, os.RemoveAll(f.dir))
		copyDir(t, backup, f.dir)
		f.open()
	}
	// The child is ahead: its memory comes back, and what the restored
	// master had is kept aside.
	restore()
	if got := f.facts(hostA); got != "one" {
		t.Fatalf("the restored master's shadow: %q", got)
	}
	if got := w.reconnect(); !slices.Equal(got, actions(Pull, Nothing)) {
		t.Fatalf("who wins after the restore: %v", got)
	}
	if got := f.facts(hostA); got != "one | three | two" {
		t.Fatalf("after the pull the shadow holds %q", got)
	}
	if m, _ := f.State(hostA); m.Gen != 3 || m.Hash != c3.Hash {
		t.Fatalf("after the pull the master states %+v", m)
	}
	if len(w.asides) != 1 || len(w.told) != 0 {
		t.Fatalf("asides %v, told %v", w.asides, w.told)
	}
	// The aside is a snapshot of what was replaced, and reads with the key.
	aside, err := openSnapshot(ctx, bytes.NewReader(readFile(t, w.asides[0])), f.key, hostA)
	must(t, err)
	var kept string
	must(t, aside.semantic.QueryRow(`SELECT group_concat(fact, ' | ') FROM facts`).Scan(&kept))
	aside.close()
	if kept != "one" {
		t.Fatalf("the aside holds %q", kept)
	}

	// Restored again, and this time the master writes before the child is
	// back: the same generation number with other content. The child's
	// stands, the master's is kept aside, and the operator is told.
	w.asides = nil
	for _, name := range files(t, w.child.dir) { // the child kept only up to generation 2
		if c, _ := parseCopyName(name); c.Gen == 3 {
			must(t, os.Remove(filepath.Join(w.child.dir, name)))
		}
	}
	restore()
	f.note(hostA, "written after the restore")
	if c := f.mint(hostA); c.Gen != 2 {
		t.Fatalf("the restored master's next generation is %d", c.Gen)
	}
	if got := w.reconnect(); !slices.Equal(got, actions(Pull, Nothing)) {
		t.Fatalf("who wins with the same generation and another hash: %v", got)
	}
	if got := f.facts(hostA); got != "one | two" {
		t.Fatalf("the child's generation 2 did not stand: %q", got)
	}
	if len(w.told) != 1 || len(w.asides) != 1 {
		t.Fatalf("told %v, asides %v", w.told, w.asides)
	}
	aside, err = openSnapshot(ctx, bytes.NewReader(readFile(t, w.asides[0])), f.key, hostA)
	must(t, err)
	must(t, aside.semantic.QueryRow(`SELECT group_concat(fact, ' | ') FROM facts`).Scan(&kept))
	aside.close()
	if kept != "one | written after the restore" {
		t.Fatalf("the master's side kept aside holds %q", kept)
	}
	// Thirty days: an aside older than that goes at the next one; this one stays.
	old := filepath.Join(filepath.Dir(w.asides[0]), "1-"+shadowFile)
	must(t, os.WriteFile(old, []byte("x"), 0o600))
	must(t, os.Chtimes(old, time.Now().Add(-asideKeep-time.Hour), time.Now().Add(-asideKeep-time.Hour)))
	recent := filepath.Join(filepath.Dir(w.asides[0]), "2-"+shadowFile)
	must(t, os.WriteFile(recent, []byte("x"), 0o600))
	must(t, os.Chtimes(recent, time.Now().Add(-asideKeep+time.Hour), time.Now().Add(-asideKeep+time.Hour)))
	must(t, f.Store.Close())
	must(t, os.Remove(f.path(hostA, shadowFile)))
	f.open()
	if got := w.reconnect(); !slices.Equal(got, actions(Rebuild, Nothing)) {
		t.Fatalf("who wins with the snapshot deleted: %v", got)
	}
	if exists(old) || !exists(recent) || !exists(w.asides[0]) {
		t.Fatalf("after the sweep: thirty-one days old %v, twenty-nine days old %v, today's %v", exists(old), exists(recent), exists(w.asides[0]))
	}
	// A forget removes them all.
	must(t, f.RemoveAside(hostA))
	if exists(w.asides[0]) || exists(recent) {
		t.Fatal("RemoveAside left something")
	}
	must(t, f.RemoveAside(hostA))
}

func TestWhoWins_AMasterWithTheWrongKeyOverwritesNothing(t *testing.T) {
	ctx := context.Background()
	old := newFleet(t, quick)
	w := newWire(t, old, hostA)
	old.note(hostA, "web1 runs nginx")
	old.mint(hostA)
	old.note(hostA, "web1's /var fills up")
	c2 := old.mint(hostA)
	w.reconnect()
	must(t, old.Store.Close())
	onChild := files(t, w.child.dir)

	// The master is rebuilt from nothing: a new fleet key, the same recovery
	// identity (the operator kept it), the same child.
	fresh := newFleet(t, quick)
	must(t, fresh.Store.Close())
	fresh.recovery, fresh.recipient = old.recovery, old.recipient
	fresh.open()
	w.master = fresh
	if got := w.reconnect(); !slices.Equal(got, actions(NeedsRecoveryKey)) || !w.needsRecoveryKey {
		t.Fatalf("who wins under another key: %v", got)
	}
	// Even after the new master has written and minted its own.
	fresh.note(hostA, "noted by the new master")
	fresh.mint(hostA)
	if got := w.reconnect(); !slices.Equal(got, actions(NeedsRecoveryKey)) {
		t.Fatalf("who wins under another key, with a generation of its own: %v", got)
	}
	if got := files(t, w.child.dir); !slices.Equal(got, onChild) {
		t.Fatalf("the child's copies changed: %v, were %v", got, onChild)
	}
	// Without the recovery identity the copy is not read, and not guessed at.
	blob, err := w.child.Get(c2)
	must(t, err)
	if _, err := fresh.Adopt(ctx, hostA, c2, blob, nil); !errors.Is(err, ErrForeignKey) {
		t.Fatalf("Adopt under the wrong key: %v, want ErrForeignKey", err)
	}
	wrong, _ := testRecovery(t)
	wrongKey, err := ParseRecovery(wrong)
	must(t, err)
	defer wrongKey.Close()
	if _, err := fresh.Adopt(ctx, hostA, c2, blob, wrongKey); !errors.Is(err, ErrNotRecovery) {
		t.Fatalf("Adopt with another recovery identity: %v, want ErrNotRecovery", err)
	}
	if got := fresh.facts(hostA); got != "noted by the new master" {
		t.Fatalf("a refused Adopt changed the shadow: %q", got)
	}

	// The operator brings the recovery identity.
	recovery, err := ParseRecovery(old.recovery)
	must(t, err)
	defer recovery.Close()
	got, err := fresh.Adopt(ctx, hostA, c2, blob, recovery)
	must(t, err)
	if !got.Reembed || got.Aside == "" || got.Gen != 3 {
		t.Fatalf("Adopt with the recovery identity: %+v", got)
	}
	if facts := fresh.facts(hostA); facts != "web1 runs nginx | web1's /var fills up" {
		t.Fatalf("the recovered shadow holds %q", facts)
	}
	// The memory goes on under the new key, numbered after the old key's
	// newest. Until the child has that generation its newest is still the
	// old key's, so it is sent at once, and the old key's copies stay where
	// they are.
	if got := w.reconnect(); !slices.Equal(got, actions(NeedsRecoveryKey)) {
		t.Fatalf("who wins after the recovery, before the push: %v", got)
	}
	c3, blob3, err := fresh.Newest(hostA)
	must(t, err)
	must(t, w.child.Put(c3, blob3))
	if got := w.reconnect(); !slices.Equal(got, actions(Nothing)) {
		t.Fatalf("who wins after the recovery: %v", got)
	}
	now := files(t, w.child.dir)
	for _, name := range onChild {
		if !slices.Contains(now, name) {
			t.Errorf("the old key's copy %s is gone", name)
		}
	}
	if len(now) != len(onChild)+1 || gens(t, w.child, fresh.key.ID()) != "3" || gens(t, w.child, old.key.ID()) != "2" {
		t.Fatalf("the child keeps %v", now)
	}
}

// A copy that passes the child's check and fails the master's is passed
// over, as a damaged one is.
func TestWhoWins_TheChildsNewestIsDamaged(t *testing.T) {
	f := newFleet(t, quick)
	w := newWire(t, f, hostA)
	f.note(hostA, "one")
	c1 := f.mint(hostA)
	w.reconnect()
	f.note(hostA, "two")
	c2 := f.mint(hostA)
	w.reconnect()
	must(t, f.Store.Close())
	must(t, os.RemoveAll(filepath.Join(f.dir, hostA))) // the master lost this machine's memory
	f.open()

	// Bit rot on the child, which the child itself sees.
	damage(t, w.child, c2)
	if got := w.reconnect(); !slices.Equal(got, actions(Pull, Nothing)) {
		t.Fatalf("who wins with the child's newest damaged: %v", got)
	}
	if got := f.facts(hostA); got != "one" {
		t.Fatalf("the next older copy was not the one used: %q", got)
	}
	if m, _ := f.State(hostA); m.Gen != 1 || m.Hash != c1.Hash {
		t.Fatalf("the master states %+v", m)
	}
	// A file planted on the child with a right-looking name and a first line
	// to match: the child cannot tell, the master can.
	must(t, f.Store.Close())
	must(t, os.RemoveAll(filepath.Join(f.dir, hostA)))
	f.open()
	forged, blob := opaque(f.key.ID(), 9, "not something this master encrypted")
	must(t, os.WriteFile(filepath.Join(w.child.dir, forged.fileName()), blob, 0o600))
	if got := w.reconnect(); !slices.Equal(got, actions(Pull, Pull, Nothing)) {
		t.Fatalf("who wins with a forged newest copy: %v", got)
	}
	if got := f.facts(hostA); got != "one" {
		t.Fatalf("after the forged copy was passed over: %q", got)
	}
	if m, _ := f.State(hostA); m.Gen != 1 || m.Hash != c1.Hash {
		t.Fatalf("the master states %+v", m)
	}
	// None verifies: the master pushes what it has.
	must(t, f.Store.Close())
	damage(t, w.child, c1)
	f.open()
	f.note(hostA, "three")
	if c := f.mint(hostA); c.Gen != 2 {
		t.Fatalf("generation %d", c.Gen)
	}
	// The child holds 1 and 2 damaged and 9 forged; none counts, so the
	// master pushes. The child, which believes it holds generation 9, takes
	// nothing lower, so the master numbers its generation past it.
	if got := w.reconnect(); !slices.Equal(got, actions(Pull, Push, Push, Nothing)) {
		t.Fatalf("who wins when none of the child's copies verifies: %v", got)
	}
	if got := gens(t, w.child, f.key.ID()); got != "10 9 2!" {
		t.Fatalf("the child keeps %q", got)
	}
	if got := f.facts(hostA); got != "one | three" {
		t.Fatalf("the master's shadow: %q", got)
	}
	if _, err := f.Skip(context.Background(), hostA, maxGen); err == nil {
		t.Fatal("Skip past the last generation number: accepted")
	}
}

// After a forget: a fresh generation, a purge on the child, the asides gone.
// Nothing is left that a rebuild could bring the forgotten text back from.
func TestWhoWins_AForgetSurvivesARebuild(t *testing.T) {
	f := newFleet(t, quick)
	w := newWire(t, f, hostA)
	f.note(hostA, "the door code is 4711")
	f.mint(hostA)
	w.reconnect()
	f.note(hostA, "web1 runs nginx")
	f.mint(hostA)
	w.reconnect()

	// The forget, as tenant privacy does it: the row is deleted, and the
	// full-text index is optimized so that the deleted words leave it.
	f.with(hostA, func(p *Pair) {
		mustExec(t, p.Semantic.DB(), `DELETE FROM facts WHERE fact LIKE '%4711%'`)
		_, err := p.Semantic.DB().Exec(`INSERT INTO facts_fts(facts_fts) VALUES ('optimize')`)
		must(t, err)
	})
	keep := f.mint(hostA)
	w.reconnect()
	must(t, w.child.Purge(keep))
	must(t, f.RemoveAside(hostA))
	if got := gens(t, w.child, f.key.ID()); got != "3" {
		t.Fatalf("after the purge the child keeps %q", got)
	}
	// The master loses the shadow and rebuilds it.
	must(t, f.Store.Close())
	must(t, os.Remove(f.path(hostA, shadowFile)))
	f.open()
	w.reconnect()
	if got := f.facts(hostA); got != "web1 runs nginx" {
		t.Fatalf("the rebuilt shadow holds %q", got)
	}
	// And the deleted row's text is not lying in a freed page of anything
	// the master or the child keeps: each file, decrypted and searched as
	// bytes. (The shadow's databases zero what they free.)
	must(t, f.Flush(context.Background(), hostA))
	_, blob, err := f.Newest(hostA)
	must(t, err)
	for name, raw := range map[string][]byte{"the generation": blob, "the master's snapshot": readFile(t, f.path(hostA, shadowFile))} {
		snap, err := openSnapshot(context.Background(), bytes.NewReader(raw), f.key, hostA)
		must(t, err)
		image, _, err := snap.semantic.serialize(context.Background())
		must(t, err)
		snap.close()
		if bytes.Contains(image, []byte("4711")) {
			t.Errorf("%s still holds the forgotten text", name)
		}
		if !bytes.Contains(image, []byte("nginx")) {
			t.Errorf("%s does not hold the text that was kept: the search proves nothing", name)
		}
	}
}

func TestAdopt_Refusals(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t, quick)
	other := newFleet(t, quick) // another fleet, with its own key
	f.note(hostA, "the shadow as it is")
	f.mint(hostA)
	// A generation this master made for another machine, and one for this
	// machine at a later number, made by a second store with the same key.
	f.note(hostB, "machine B's memory")
	bCopy := f.mint(hostB)
	_, bBlob, err := f.Newest(hostB)
	must(t, err)
	aCopy, aBlob, err := f.Newest(hostA)
	must(t, err)
	other.note(hostA, "another master's")
	oCopy := other.mint(hostA)
	_, oBlob, err := other.Newest(hostA)
	must(t, err)
	must(t, f.Flush(ctx, hostA))
	before := readFile(t, f.path(hostA, shadowFile))

	flipped := bytes.Clone(aBlob)
	flipped[len(flipped)-5] ^= 1
	for name, tc := range map[string]struct {
		c    Copy
		blob []byte
		want error
	}{
		"the bytes are not the stated hash's": {aCopy, flipped, ErrDamaged},
		"altered, with a hash to match":       {Copy{KeyID: aCopy.KeyID, Gen: aCopy.Gen, Hash: Hash(flipped)}, flipped, ErrDamaged},
		"stated as another generation":        {Copy{KeyID: aCopy.KeyID, Gen: 9, Hash: aCopy.Hash}, aBlob, ErrDamaged},
		"stated as under another key":         {Copy{KeyID: keyB, Gen: aCopy.Gen, Hash: aCopy.Hash}, aBlob, ErrDamaged},
		"another machine's memory":            {bCopy, bBlob, ErrDamaged},
		"another fleet key's":                 {oCopy, oBlob, ErrForeignKey},
		"not a snapshot":                      {Copy{KeyID: aCopy.KeyID, Gen: 1, Hash: Hash([]byte("x"))}, []byte("x"), ErrDamaged},
		"empty":                               {Copy{KeyID: aCopy.KeyID, Gen: 1, Hash: Hash(nil)}, nil, ErrDamaged},
	} {
		got, err := f.Adopt(ctx, hostA, tc.c, tc.blob, nil)
		if !errors.Is(err, tc.want) || got != (Adopted{}) {
			t.Errorf("%s: %+v, %v; want %v", name, got, err, tc.want)
		}
	}
	// While the stores are held, nothing replaces them.
	held := f.acquire(hostA)
	if _, err := f.Adopt(ctx, hostA, aCopy, aBlob, nil); !errors.Is(err, ErrInUse) {
		t.Errorf("Adopt of a shadow in use: %v, want ErrInUse", err)
	}
	if err := f.Remove(hostA); !errors.Is(err, ErrInUse) {
		t.Errorf("Remove of a shadow in use: %v, want ErrInUse", err)
	}
	held.Release()
	// None of that changed anything.
	if got := f.facts(hostA); got != "the shadow as it is" {
		t.Fatalf("the shadow now holds %q", got)
	}
	if !bytes.Equal(readFile(t, f.path(hostA, shadowFile)), before) || exists(f.path(hostA, asideDir)) {
		t.Fatal("a refused Adopt changed the files")
	}
	// And the real one is taken.
	if got, err := f.Adopt(ctx, hostA, aCopy, aBlob, nil); err != nil || got.Gen != 1 || !got.Reembed || got.Aside == "" {
		t.Fatalf("Adopt of the master's own generation: %+v, %v", got, err)
	}
}

func TestRemove(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t, quick)
	f.note(hostA, "web1 runs nginx")
	f.note(hostB, "db1 is the postgres primary")
	f.mint(hostA)
	must(t, f.Remove(hostA))
	if exists(filepath.Join(f.dir, hostA)) {
		t.Fatal("Remove left the machine's directory")
	}
	if got := f.facts(hostA); got != "" {
		t.Fatalf("after Remove the machine's memory is %q", got)
	}
	if m, _ := f.State(hostA); m != (Master{KeyID: f.key.ID(), ShadowOK: true}) {
		t.Fatalf("State after Remove: %+v", m)
	}
	if got := f.facts(hostB); got != "db1 is the postgres primary" {
		t.Fatalf("Remove of one machine touched another: %q", got)
	}
	// Remove is also the way out of Lost: a shadow that cannot be read and
	// has no copy anywhere.
	f.note(hostA, "again")
	f.mint(hostA)
	must(t, f.Store.Close())
	must(t, os.WriteFile(f.path(hostA, shadowFile), []byte("garbage"), 0o600))
	must(t, os.WriteFile(f.path(hostA, childFile), []byte("garbage"), 0o600))
	f.open()
	if _, err := f.Acquire(ctx, hostA); !errors.Is(err, ErrDamaged) {
		t.Fatalf("Acquire: %v", err)
	}
	m, _ := f.State(hostA)
	if d := Decide(m, nil); d.Action != Lost {
		t.Fatalf("Decide: %+v for %+v", d, m)
	}
	must(t, f.Remove(hostA))
	if got := f.facts(hostA); got != "" {
		t.Fatalf("after Remove: %q", got)
	}
	must(t, f.Remove("m00000000000000cc")) // a machine that has nothing
}

// A machine re-homed to a master rebuilt from nothing has another id there
// (tenant child rehome): its copies, made under the old master, name the id
// it had. The recovery identity reads them as that machine's, and nothing
// else does: not the new master's own key, and not a recovery identity told
// the wrong old id.
func TestAdoptAs_ACopyMadeUnderTheMachinesOldID(t *testing.T) {
	ctx := context.Background()
	old := newFleet(t, quick)
	w := newWire(t, old, hostA)
	old.note(hostA, "web1 runs nginx")
	c := old.mint(hostA)
	w.reconnect()
	must(t, old.Store.Close())
	blob, err := w.child.Get(c)
	must(t, err)

	fresh := newFleet(t, quick)
	must(t, fresh.Store.Close())
	fresh.recovery, fresh.recipient = old.recovery, old.recipient
	fresh.open()
	recovery, err := ParseRecovery(old.recovery)
	must(t, err)
	defer recovery.Close()

	if _, err := fresh.AdoptAs(ctx, hostB, hostA, c, blob, nil); !errors.Is(err, ErrDamaged) {
		t.Fatalf("another machine's copy without the recovery identity: %v, want ErrDamaged", err)
	}
	if _, err := fresh.Adopt(ctx, hostB, c, blob, recovery); !errors.Is(err, ErrDamaged) {
		t.Fatalf("a copy that names another machine, as this one's: %v, want ErrDamaged", err)
	}
	if _, err := fresh.AdoptAs(ctx, hostB, "m00000000000000cc", c, blob, recovery); !errors.Is(err, ErrDamaged) {
		t.Fatalf("the wrong old id: %v, want ErrDamaged", err)
	}
	got, err := fresh.AdoptAs(ctx, hostB, hostA, c, blob, recovery)
	must(t, err)
	if facts := fresh.facts(hostB); facts != "web1 runs nginx" || got.Gen != c.Gen+1 {
		t.Fatalf("adopted as the new id: %+v, facts %q", got, facts)
	}
	// What it is now is a generation of the new machine's own, under the
	// new key: it opens as hostB's, and as nothing else's.
	c2, blob2, err := fresh.Newest(hostB)
	must(t, err)
	if _, err := fresh.Adopt(ctx, hostB, c2, blob2, nil); err != nil {
		t.Fatalf("the new generation does not open as the new id's: %v", err)
	}
	if facts := fresh.facts(hostA); facts != "" {
		t.Fatalf("the old id has a memory on the new master: %q", facts)
	}
}
