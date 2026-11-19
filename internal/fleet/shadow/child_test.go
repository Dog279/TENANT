package shadow

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var (
	keyA = strings.Repeat("a1", keyIDBytes)
	keyB = strings.Repeat("b2", keyIDBytes)
)

// opaque is what a child sees of a snapshot: a first line and bytes it
// cannot read.
func opaque(keyID string, gen uint64, body string) (Copy, []byte) {
	blob := append(appendHeader(nil, keyID, gen), "age-encryption.org/v1\n"+body...)
	return Copy{KeyID: keyID, Gen: gen, Hash: Hash(blob), OK: true}, blob
}

func newChildStore(t testing.TB) *ChildStore {
	t.Helper()
	s, err := OpenChildStore(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func put(t testing.TB, s *ChildStore, keyID string, gen uint64, body string) Copy {
	t.Helper()
	c, blob := opaque(keyID, gen, body)
	if err := s.Put(c, blob); err != nil {
		t.Fatalf("Put %s generation %d: %v", keyID[:2], gen, err)
	}
	return c
}

// gens lists the generations kept under a key, newest first, with a "!" on
// each one that is damaged.
func gens(t testing.TB, s *ChildStore, keyID string) string {
	t.Helper()
	copies, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range copies {
		if c.KeyID == keyID {
			out = append(out, fmt.Sprint(c.Gen)+map[bool]string{false: "!"}[c.OK])
		}
	}
	return strings.Join(out, " ")
}

func files(t testing.TB, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func TestChildStore_KeepsThreeUnderAKey(t *testing.T) {
	s := newChildStore(t)
	if got := gens(t, s, keyA); got != "" {
		t.Fatalf("a new store lists %q", got)
	}
	for gen := uint64(1); gen <= 3; gen++ {
		put(t, s, keyA, gen, "memory")
	}
	if got := gens(t, s, keyA); got != "3 2 1" {
		t.Fatalf("after three: %q", got)
	}
	// The fourth arrives, its hash checks, the oldest goes.
	put(t, s, keyA, 4, "memory")
	if got := gens(t, s, keyA); got != "4 3 2" {
		t.Fatalf("after the fourth: %q", got)
	}
	// Generations need not be consecutive.
	put(t, s, keyA, 9, "memory")
	put(t, s, keyA, 40, "memory")
	if got := gens(t, s, keyA); got != "40 9 4" {
		t.Fatalf("after two more: %q", got)
	}
	if n := len(files(t, s.dir)); n != 3 {
		t.Fatalf("%d files on disk, want 3: %v", n, files(t, s.dir))
	}

	// What it keeps is what it was given, byte for byte.
	c, blob := opaque(keyA, 40, "memory")
	got, err := s.Get(c)
	if err != nil || !bytes.Equal(got, blob) {
		t.Fatalf("Get: %v", err)
	}
	// Sent again, it is accepted and nothing changes.
	before := files(t, s.dir)
	if err := s.Put(c, blob); err != nil {
		t.Fatalf("the same copy again: %v", err)
	}
	if after := files(t, s.dir); !slices.Equal(after, before) {
		t.Fatalf("the same copy again changed the store: %v", after)
	}
	for _, missing := range []Copy{
		{KeyID: keyA, Gen: 2, Hash: c.Hash},       // pruned
		{KeyID: keyA, Gen: 40, Hash: keyA + keyA}, // that generation, another hash
		{KeyID: keyB, Gen: 40, Hash: c.Hash},
		{},
	} {
		if _, err := s.Get(missing); !errors.Is(err, ErrNoCopy) {
			t.Errorf("Get %+v: %v, want ErrNoCopy", missing, err)
		}
	}
}

// Copies under another key are not counted among the three, and nothing done
// under one key touches them.
func TestChildStore_AnotherKeysCopiesAreLeftAlone(t *testing.T) {
	s := newChildStore(t)
	old1 := put(t, s, keyB, 7, "the old master's")
	old2 := put(t, s, keyB, 8, "the old master's")
	for gen := uint64(1); gen <= 6; gen++ {
		put(t, s, keyA, gen, "the new master's")
	}
	if a, b := gens(t, s, keyA), gens(t, s, keyB); a != "6 5 4" || b != "8 7" {
		t.Fatalf("keys A and B keep %q and %q", a, b)
	}
	// A lower generation under A than B's newest is no step backwards: the
	// keys do not share a count.
	keep := put(t, s, keyA, 7, "the new master's")
	if err := s.Purge(keep); err != nil {
		t.Fatal(err)
	}
	if a, b := gens(t, s, keyA), gens(t, s, keyB); a != "7" || b != "8 7" {
		t.Fatalf("after a purge under A, keys A and B keep %q and %q", a, b)
	}
	for _, c := range []Copy{old1, old2} {
		if _, err := s.Get(c); err != nil {
			t.Errorf("the copy under the other key is gone: %v", err)
		}
	}
	// Same generation number under both keys: two copies, neither replaces
	// the other.
	if _, err := s.Get(keep); err != nil {
		t.Error(err)
	}
}

func TestChildStore_PutRefusals(t *testing.T) {
	s := newChildStore(t)
	put(t, s, keyA, 5, "five")
	stands := put(t, s, keyA, 6, "six")
	put(t, s, keyA, 7, "seven")
	before := files(t, s.dir)

	good, blob := opaque(keyA, 8, "eight")
	_, sixAgain := opaque(keyA, 6, "six, as another master remembers it")
	_, two := opaque(keyA, 2, "two")
	_, six := opaque(keyA, 6, "six")
	for name, tc := range map[string]struct {
		c    Copy
		blob []byte
		want error
	}{
		"an older generation":            {Copy{KeyID: keyA, Gen: 2, Hash: Hash(two)}, two, ErrStale},
		"older than the newest only":     {Copy{KeyID: keyA, Gen: 6, Hash: Hash(six)}, six, nil}, // held: accepted, unchanged
		"a held generation, other bytes": {Copy{KeyID: keyA, Gen: 6, Hash: Hash(sixAgain)}, sixAgain, ErrConflict},
		"the hash is of other bytes":     {Copy{KeyID: keyA, Gen: 8, Hash: Hash([]byte("x"))}, blob, ErrDamaged},
		"one byte changed on the way":    {good, append(bytes.Clone(blob[:len(blob)-1]), 'X'), ErrDamaged},
		"cut short on the way":           {good, blob[:len(blob)-3], ErrDamaged},
		"stated as another generation":   {Copy{KeyID: keyA, Gen: 9, Hash: good.Hash}, blob, ErrDamaged},
		"stated as under another key":    {Copy{KeyID: keyB, Gen: 8, Hash: good.Hash}, blob, ErrDamaged},
		"not a snapshot":                 {Copy{KeyID: keyA, Gen: 8, Hash: Hash([]byte("hello"))}, []byte("hello"), ErrDamaged},
		"empty":                          {Copy{KeyID: keyA, Gen: 8, Hash: Hash(nil)}, nil, nil},
		"generation zero":                {Copy{KeyID: keyA, Gen: 0, Hash: good.Hash}, blob, nil},
		"a generation past the range":    {Copy{KeyID: keyA, Gen: maxGen + 1, Hash: good.Hash}, blob, nil},
		"the largest uint64":             {Copy{KeyID: keyA, Gen: 1<<64 - 1, Hash: good.Hash}, blob, nil},
		"a key id that is a path":        {Copy{KeyID: "../../../../etc/passwd", Gen: 8, Hash: good.Hash}, blob, nil},
		"a key id in capitals":           {Copy{KeyID: strings.ToUpper(keyA), Gen: 8, Hash: good.Hash}, blob, nil},
		"a short key id":                 {Copy{KeyID: keyA[:30], Gen: 8, Hash: good.Hash}, blob, nil},
		"a hash that is a path":          {Copy{KeyID: keyA, Gen: 8, Hash: `..\..\x`}, blob, nil},
		"a short hash":                   {Copy{KeyID: keyA, Gen: 8, Hash: good.Hash[:62]}, blob, nil},
		"no hash":                        {Copy{KeyID: keyA, Gen: 8}, blob, nil},
		"nothing stated":                 {Copy{}, blob, nil},
	} {
		err := s.Put(tc.c, tc.blob)
		switch {
		case name == "older than the newest only":
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
		case err == nil:
			t.Errorf("%s: accepted", name)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
		if after := files(t, s.dir); !slices.Equal(after, before) {
			t.Fatalf("%s: a refused Put changed the store: %v", name, after)
		}
	}
	// The child's copy of generation 6 stands.
	if _, err := s.Get(stands); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(good, blob); err != nil {
		t.Fatalf("and the good one is still accepted: %v", err)
	}
}

func damage(t testing.TB, s *ChildStore, c Copy) {
	t.Helper()
	path := filepath.Join(s.dir, c.fileName())
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A damaged copy is listed as such, is not handed out, gives way to the next
// older one, and is the first to go.
func TestChildStore_DamagedCopies(t *testing.T) {
	s := newChildStore(t)
	put(t, s, keyA, 4, "four")
	five := put(t, s, keyA, 5, "five")
	six := put(t, s, keyA, 6, "six")
	damage(t, s, six)
	if got := gens(t, s, keyA); got != "6! 5 4" {
		t.Fatalf("with the newest damaged: %q", got)
	}
	if _, err := s.Get(six); !errors.Is(err, ErrNoCopy) {
		t.Fatalf("Get of the damaged copy: %v", err)
	}
	// The master is told, and uses the next older.
	copies, _ := s.List()
	if d := Decide(Master{KeyID: keyA, ShadowOK: true}, copies); d.Action != Pull || d.Copy != five {
		t.Fatalf("Decide with the newest damaged: %+v", d)
	}
	// A purge is refused when what it would keep is not intact.
	if err := s.Purge(six); !errors.Is(err, ErrNoCopy) {
		t.Fatalf("Purge keeping the damaged copy: %v", err)
	}
	if got := gens(t, s, keyA); got != "6! 5 4" {
		t.Fatalf("the refused purge removed something: %q", got)
	}
	// Sent again, the same generation repairs it in place.
	_, blob := opaque(keyA, 6, "six")
	if err := s.Put(six, blob); err != nil {
		t.Fatal(err)
	}
	if got := gens(t, s, keyA); got != "6 5 4" {
		t.Fatalf("after the repair: %q", got)
	}

	// A damaged copy does not make a newer intact generation "stale", and it
	// goes before any intact one does.
	damage(t, s, six)
	put(t, s, keyA, 7, "seven")
	if got := gens(t, s, keyA); got != "7 5 4" {
		t.Fatalf("with a damaged copy and a fourth: %q", got)
	}
	// A damaged copy with a number above everything does not block the next
	// real generation either.
	nine := put(t, s, keyA, 9, "nine")
	damage(t, s, nine)
	put(t, s, keyA, 8, "eight")
	if got := gens(t, s, keyA); got != "8 7 5" {
		t.Fatalf("after a damaged 9 and then 8: %q", got)
	}
	// The same generation with other content replaces a damaged one: there
	// is nothing of the child's to stand.
	eight, _ := opaque(keyA, 8, "eight")
	damage(t, s, eight)
	put(t, s, keyA, 8, "eight, made again")
	if got := gens(t, s, keyA); got != "8 7 5" {
		t.Fatalf("a remade 8 in place of the damaged 8: %q", got)
	}
	if _, err := s.Get(eight); !errors.Is(err, ErrNoCopy) {
		t.Fatalf("the damaged 8 is still handed out: %v", err)
	}
}

func TestChildStore_Purge(t *testing.T) {
	s := newChildStore(t)
	put(t, s, keyA, 3, "before the forget")
	four := put(t, s, keyA, 4, "before the forget")
	put(t, s, keyB, 1, "another key")
	// A purge for a generation the child was not sent: nothing happens.
	five, blob := opaque(keyA, 5, "after the forget")
	for name, keep := range map[string]Copy{
		"not held":          five,
		"another hash":      {KeyID: keyA, Gen: 4, Hash: five.Hash},
		"another key":       {KeyID: keyB, Gen: 4, Hash: four.Hash},
		"not a copy at all": {KeyID: "x", Gen: 0},
	} {
		if err := s.Purge(keep); !errors.Is(err, ErrNoCopy) {
			t.Errorf("purge keeping %s: %v, want ErrNoCopy", name, err)
		}
	}
	if a := gens(t, s, keyA); a != "4 3" {
		t.Fatalf("a refused purge removed something: %q", a)
	}
	must(t, s.Put(five, blob))
	must(t, s.Purge(five))
	if a, b := gens(t, s, keyA), gens(t, s, keyB); a != "5" || b != "1" {
		t.Fatalf("after the purge: A %q, B %q", a, b)
	}
	must(t, s.Purge(five)) // again: nothing left to remove
	// Purging at an older generation leaves the newer ones.
	put(t, s, keyA, 6, "later")
	must(t, s.Purge(five))
	if a := gens(t, s, keyA); a != "6 5" {
		t.Fatalf("a purge at 5 with a 6 present: %q", a)
	}
}

// A directory someone else has written into: nothing in it is trusted,
// followed, counted or removed unless it is exactly a copy.
func TestChildStore_HostileDirectory(t *testing.T) {
	s := newChildStore(t)
	real := put(t, s, keyA, 3, "real")
	_, realBlob := opaque(keyA, 3, "real")
	h := real.Hash
	outside := filepath.Join(t.TempDir(), "outside.snap")
	must(t, os.WriteFile(outside, realBlob, 0o600))

	strays := []string{
		"notes.txt", ".snap", "x.snap", keyA + ".snap", keyA + ".4.snap", keyA + ".4." + h, // no extension
		keyA + ".4." + h + ".snap.tmp", keyA + ".4." + h + ".snap.123.tmp",
		keyA + ".04." + h + ".snap", keyA + ".+4." + h + ".snap", keyA + ".0." + h + ".snap",
		keyA + ".4.5." + h + ".snap", keyA + ".-1." + h + ".snap", keyA + ".4e0." + h + ".snap",
		keyA + ".9223372036854775808." + h + ".snap",  // one past the largest generation
		keyA + ".18446744073709551616." + h + ".snap", // overflows a uint64
		keyA + ".99999999999999999999999999999999." + h + ".snap",
		strings.ToUpper(keyA) + ".4." + h + ".snap", keyA + ".4." + strings.ToUpper(h) + ".snap",
		keyA[:30] + ".4." + h + ".snap", keyA + ".4." + h[:62] + ".snap", keyA + "zz.4." + h + ".snap",
		" " + keyA + ".4." + h + ".snap", keyA + ".4." + h + ".snapshot",
		keyA + ".٤." + h + ".snap", // an Arabic-Indic four
		strings.Repeat("a", 200) + ".snap",
	}
	for _, name := range strays {
		if err := os.WriteFile(filepath.Join(s.dir, name), realBlob, 0o600); err != nil {
			t.Logf("this system will not create %q: %v", name, err)
		}
	}
	// Right names, wrong things behind them. A plain file is a copy that is
	// damaged. A directory or a link is not a copy at all.
	_, linked := opaque(keyB, 3, "linked")
	must(t, os.WriteFile(outside, linked, 0o600))
	damagedFiles := map[string]func(path string) error{
		"an empty file":                        func(p string) error { return os.WriteFile(p, nil, 0o600) },
		"other bytes":                          func(p string) error { return os.WriteFile(p, []byte("not what the name says"), 0o600) },
		"a real snapshot under the wrong name": func(p string) error { return os.WriteFile(p, realBlob, 0o600) },
		"larger than any snapshot": func(p string) error {
			f, err := os.Create(p)
			if err != nil {
				return err
			}
			defer f.Close()
			return f.Truncate(MaxSnapshotBytes + 1) // no gigabyte is written
		},
	}
	notFiles := map[Copy]func(path string) error{
		{KeyID: keyA, Gen: 70, Hash: h}: func(p string) error { return os.Mkdir(p, 0o700) },
		{KeyID: keyA, Gen: 71, Hash: h}: func(p string) error {
			return errors.Join(os.Mkdir(p, 0o700), os.WriteFile(filepath.Join(p, "x"), realBlob, 0o600))
		},
		{KeyID: keyA, Gen: 72, Hash: h}: func(p string) error { return os.Symlink(filepath.Join(s.dir, "gone"), p) },
		// Followed, this one would verify: its target is the very snapshot
		// the name describes.
		{KeyID: keyB, Gen: 3, Hash: Hash(linked)}: func(p string) error { return os.Symlink(outside, p) },
	}
	var planted, squatters []Copy
	gen := uint64(50)
	for name, plant := range damagedFiles {
		gen++
		c := Copy{KeyID: keyA, Gen: gen, Hash: h}
		if err := plant(filepath.Join(s.dir, c.fileName())); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		planted = append(planted, c)
	}
	for c, plant := range notFiles {
		if err := plant(filepath.Join(s.dir, c.fileName())); err != nil {
			t.Logf("this system will not create %s: %v", c.fileName(), err)
			continue
		}
		squatters = append(squatters, c)
	}
	before := files(t, s.dir)

	copies, err := s.List()
	must(t, err)
	if len(copies) != 1+len(planted) {
		t.Errorf("List has %d entries, want the real copy and the %d damaged files: %+v", len(copies), len(planted), copies)
	}
	for _, c := range copies {
		if c.OK != (c == real) {
			t.Errorf("listed %+v", c)
		}
	}
	for _, c := range append(slices.Clone(planted), squatters...) {
		if _, err := s.Get(c); !errors.Is(err, ErrNoCopy) {
			t.Errorf("Get of planted %s generation %d: %v", c.KeyID[:2], c.Gen, err)
		}
		if err := s.Purge(c); !errors.Is(err, ErrNoCopy) {
			t.Errorf("Purge keeping planted %s generation %d: %v", c.KeyID[:2], c.Gen, err)
		}
	}
	if after := files(t, s.dir); !slices.Equal(after, before) {
		t.Fatalf("reading the directory changed it: %v, then %v", before, after)
	}
	// The planted generations are all "newer" than the real ones. They are
	// damaged, so they do not make the next real generation stale; and being
	// damaged copies under this key, they are the first to go.
	four := put(t, s, keyA, 4, "the next real one")
	if _, err := s.Get(real); err != nil {
		t.Errorf("the real copy is gone: %v", err)
	}
	must(t, s.Purge(four))
	if got := gens(t, s, keyA); got != "4" {
		t.Errorf("after a purge: %q", got)
	}
	// Nothing that was not a copy was removed, whatever its name.
	now := files(t, s.dir)
	for _, name := range before {
		c, isCopy := parseCopyName(name)
		if (!isCopy || slices.Contains(squatters, c)) && !slices.Contains(now, name) {
			t.Errorf("%q was removed", name)
		}
	}
	if raw, err := os.ReadFile(outside); err != nil || !bytes.Equal(raw, linked) {
		t.Errorf("the file a link pointed at was touched: %v", err)
	}
	// A directory squatting on the very name the next copy needs: the copy
	// cannot be kept, and that is an error, not a silent loss.
	next, blob := opaque(keyA, 5, "five")
	must(t, os.Mkdir(filepath.Join(s.dir, next.fileName()), 0o700))
	if err := s.Put(next, blob); err == nil {
		t.Error("a Put onto a directory reported success")
	}
}

// A child keeps copies under a bounded number of keys.
func TestChildStore_BoundsTheKeys(t *testing.T) {
	s := newChildStore(t)
	for i := range maxKeys {
		put(t, s, fmt.Sprintf("%032x", i+1), 1, "memory")
	}
	c, blob := opaque(fmt.Sprintf("%032x", maxKeys+1), 1, "memory")
	if err := s.Put(c, blob); err == nil {
		t.Fatal("a copy under one key too many was accepted")
	}
	put(t, s, fmt.Sprintf("%032x", 1), 2, "memory") // a key it already keeps
	if n := len(files(t, s.dir)); n != maxKeys+1 {
		t.Fatalf("%d files, want %d", n, maxKeys+1)
	}
}

// A Put leaves nothing behind but the copy, and a refused one leaves nothing.
func TestChildStore_WritesWholeFiles(t *testing.T) {
	s := newChildStore(t)
	c := put(t, s, keyA, 1, strings.Repeat("memory ", 10_000))
	if got := files(t, s.dir); !slices.Equal(got, []string{c.fileName()}) {
		t.Fatalf("files after a Put: %v", got)
	}
	bad, blob := opaque(keyA, 2, "x")
	bad.Hash = c.Hash
	if err := s.Put(bad, blob); err == nil {
		t.Fatal("accepted")
	}
	if got := files(t, s.dir); !slices.Equal(got, []string{c.fileName()}) {
		t.Fatalf("files after a refused Put: %v", got)
	}
	// The directory is gone from under the store: an error, not a panic.
	must(t, os.RemoveAll(s.dir))
	if _, err := s.List(); err == nil {
		t.Error("List of a missing directory: no error")
	}
	if err := s.Put(bad, blob); err == nil {
		t.Error("Put into a missing directory: no error")
	}
	if _, err := OpenChildStore(filepath.Join(s.dir, "\x00")); err == nil {
		t.Error("OpenChildStore of an impossible path: no error")
	}
}

func FuzzCopyName(f *testing.F) {
	h := strings.Repeat("0f", 32)
	f.Add(keyA + ".1." + h + ".snap")
	f.Add(keyA + ".9223372036854775807." + h + ".snap")
	f.Add(keyA + ".9223372036854775808." + h + ".snap")
	f.Add(keyA + ".01." + h + ".snap")
	f.Add("../" + keyA + ".1." + h + ".snap")
	f.Add(keyA + ".1." + h + ".snap.tmp")
	f.Add("")
	f.Fuzz(func(t *testing.T, name string) {
		c, ok := parseCopyName(name)
		if !ok {
			return
		}
		// A name that is read as a copy is that copy's one name: no two files
		// can be the same copy, and no copy's name leaves the directory.
		if c.fileName() != name {
			t.Fatalf("%q was read as the copy that is written %q", name, c.fileName())
		}
		if !c.valid() || strings.ContainsAny(name, `/\`) || filepath.Base(name) != name {
			t.Fatalf("%q accepted as %+v", name, c)
		}
	})
}
