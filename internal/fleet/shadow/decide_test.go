package shadow

import (
	"encoding/json"
	"strings"
	"testing"
)

var (
	hashX = strings.Repeat("11", 32)
	hashY = strings.Repeat("22", 32)
	hashZ = strings.Repeat("33", 32)
)

func cp(keyID string, gen uint64, hash string, ok bool) Copy {
	return Copy{KeyID: keyID, Gen: gen, Hash: hash, OK: ok}
}

// Every row of the "who wins" table in gap6-fleet-plan.md section 6, and the
// paragraph under it, as Decide answers it.
func TestDecide(t *testing.T) {
	master := func(gen uint64, hash string) Master { return Master{KeyID: keyA, Gen: gen, Hash: hash, ShadowOK: true} }
	broken := func(gen uint64, hash string) Master { return Master{KeyID: keyA, Gen: gen, Hash: hash} }
	for _, tc := range []struct {
		row    string
		master Master
		child  []Copy
		want   Decision
	}{
		// Equal: nothing.
		{"equal", master(7, hashX), []Copy{cp(keyA, 7, hashX, true), cp(keyA, 6, hashY, true), cp(keyA, 5, hashZ, true)},
			Decision{Action: Nothing}},
		{"equal, neither side has anything", master(0, ""), nil, Decision{Action: Nothing}},

		// Master newer: push.
		{"master newer", master(8, hashX), []Copy{cp(keyA, 7, hashY, true), cp(keyA, 6, hashZ, true)},
			Decision{Action: Push}},
		{"master newer, the child has nothing (its copies were deleted)", master(3, hashX), nil,
			Decision{Action: Push}},

		// Child newer (the master was restored from an older backup): pull,
		// verify, swap in as the shadow, re-embed; the replaced shadow is
		// kept aside.
		{"child newer", master(5, hashX), []Copy{cp(keyA, 9, hashY, true), cp(keyA, 8, hashZ, true)},
			Decision{Action: Pull, Copy: cp(keyA, 9, hashY, true)}},
		{"child newer, the master never made a generation", master(0, ""), []Copy{cp(keyA, 2, hashY, true)},
			Decision{Action: Pull, Copy: cp(keyA, 2, hashY, true)}},
		{"child newer, listed oldest first", master(5, hashX), []Copy{cp(keyA, 7, hashZ, true), cp(keyA, 9, hashY, true)},
			Decision{Action: Pull, Copy: cp(keyA, 9, hashY, true)}},

		// Same generation, different hash: the child's stands; the master's
		// is kept aside (for thirty days, and the operator is told).
		{"same generation, different hash", master(7, hashX), []Copy{cp(keyA, 7, hashY, true), cp(keyA, 6, hashZ, true)},
			Decision{Action: Pull, Copy: cp(keyA, 7, hashY, true), Diverged: true}},

		// A child cannot hold this (it refuses the second), but if one says it
		// does, the answer is still one answer: the lower hash.
		{"same generation twice on the child, neither the master's", master(7, hashX), []Copy{cp(keyA, 7, hashZ, true), cp(keyA, 7, hashY, true)},
			Decision{Action: Pull, Copy: cp(keyA, 7, hashY, true), Diverged: true}},

		// Key ids differ: the machine is marked "needs recovery key";
		// nothing is overwritten.
		{"key ids differ", master(3, hashX), []Copy{cp(keyB, 9, hashY, true), cp(keyB, 8, hashZ, true)},
			Decision{Action: NeedsRecoveryKey}},
		{"key ids differ, the master has nothing", master(0, ""), []Copy{cp(keyB, 9, hashY, true)},
			Decision{Action: NeedsRecoveryKey}},
		{"key ids differ, and the master's generation is the higher number", master(50, hashX), []Copy{cp(keyB, 9, hashY, true)},
			Decision{Action: NeedsRecoveryKey}},
		{"key ids differ, and the master's shadow is gone", broken(3, hashX), []Copy{cp(keyB, 9, hashY, true)},
			Decision{Action: NeedsRecoveryKey}},
		{"key ids differ on the child's newest; older copies are ours", master(3, hashX),
			[]Copy{cp(keyB, 9, hashY, true), cp(keyA, 3, hashX, true)},
			Decision{Action: NeedsRecoveryKey}},
		{"the same generation under both keys", master(9, hashX), []Copy{cp(keyA, 9, hashX, true), cp(keyB, 9, hashY, true)},
			Decision{Action: NeedsRecoveryKey}},
		{"the same generation under both keys, the other way round", master(9, hashX), []Copy{cp(keyB, 9, hashY, true), cp(keyA, 9, hashX, true)},
			Decision{Action: NeedsRecoveryKey}},
		{"two other keys at the same generation", master(3, hashX),
			[]Copy{cp(keyB, 9, hashY, true), cp(strings.Repeat("c3", keyIDBytes), 9, hashY, true)},
			Decision{Action: NeedsRecoveryKey}},
		// The master's own snapshot is under another key (its key file was
		// replaced). It holds a memory it cannot read: no copy is put in its
		// place and it is not called lost, whatever the child has.
		{"the master's own snapshot is under another key", Master{KeyID: keyA, ShadowForeign: true}, nil,
			Decision{Action: NeedsRecoveryKey}},
		{"the master's own snapshot is under another key, the child has copies under this one",
			Master{KeyID: keyA, Gen: 3, Hash: hashX, ShadowForeign: true}, []Copy{cp(keyA, 9, hashY, true)},
			Decision{Action: NeedsRecoveryKey}},
		// After a recovery the master's generations are the newest, and the
		// old key's copies, which the child still keeps, no longer decide.
		{"old-key copies under newer ones of ours", master(10, hashX),
			[]Copy{cp(keyA, 10, hashX, true), cp(keyB, 9, hashY, true), cp(keyB, 8, hashZ, true)},
			Decision{Action: Nothing}},

		// The child's newest copy is damaged: its next older one is used; if
		// none verifies, the master pushes.
		{"child's newest damaged, the next older is newer than the master's", master(5, hashX),
			[]Copy{cp(keyA, 9, hashY, false), cp(keyA, 8, hashZ, true)},
			Decision{Action: Pull, Copy: cp(keyA, 8, hashZ, true)}},
		{"child's newest damaged, the next older is the master's", master(8, hashZ),
			[]Copy{cp(keyA, 9, hashY, false), cp(keyA, 8, hashZ, true)},
			Decision{Action: Nothing}},
		{"child's newest damaged, the next older is behind the master", master(9, hashY),
			[]Copy{cp(keyA, 9, hashY, false), cp(keyA, 8, hashZ, true)},
			Decision{Action: Push}},
		{"none of the child's copies verifies", master(5, hashX),
			[]Copy{cp(keyA, 9, hashY, false), cp(keyA, 8, hashZ, false), cp(keyA, 7, hashX, false)},
			Decision{Action: Push}},
		{"a damaged copy under another key does not mark the machine", master(5, hashX),
			[]Copy{cp(keyB, 9, hashY, false), cp(keyA, 5, hashX, true)},
			Decision{Action: Nothing}},
		{"copies with fields no master wrote count as damaged", master(5, hashX),
			[]Copy{cp(keyA, 1<<64-1, hashY, true), cp("../etc", 9, hashY, true), cp(keyA, 9, "zz", true), cp(keyA, 0, hashY, true), cp(keyA, 4, hashZ, true)},
			Decision{Action: Push}},

		// The master's snapshot of the shadow is damaged or gone: rebuilt
		// from the child's newest copy that verifies, and re-embedded.
		{"master's shadow gone", broken(7, hashX), []Copy{cp(keyA, 7, hashX, true), cp(keyA, 6, hashY, true)},
			Decision{Action: Rebuild, Copy: cp(keyA, 7, hashX, true)}},
		{"master's shadow gone, the child's newest is damaged", broken(7, hashX), []Copy{cp(keyA, 7, hashX, false), cp(keyA, 6, hashY, true)},
			Decision{Action: Rebuild, Own: true}},
		{"master's shadow gone, the child is ahead", broken(5, hashX), []Copy{cp(keyA, 9, hashY, true)},
			Decision{Action: Rebuild, Copy: cp(keyA, 9, hashY, true)}},
		{"master's shadow gone, same generation, different hash: the child's", broken(7, hashX), []Copy{cp(keyA, 7, hashY, true)},
			Decision{Action: Rebuild, Copy: cp(keyA, 7, hashY, true)}},
		{"master's shadow gone, no generation file either", broken(0, ""), []Copy{cp(keyA, 4, hashY, true)},
			Decision{Action: Rebuild, Copy: cp(keyA, 4, hashY, true)}},
		// Not in the table: the child has nothing as new, and the master
		// still has the generation it last made. That is a copy too.
		{"master's shadow gone, the child has nothing", broken(7, hashX), nil,
			Decision{Action: Rebuild, Own: true}},
		{"master's shadow gone, the child is behind", broken(7, hashX), []Copy{cp(keyA, 6, hashY, true)},
			Decision{Action: Rebuild, Own: true}},
		{"master's shadow gone and nothing verifies anywhere", broken(0, ""), []Copy{cp(keyA, 4, hashY, false)},
			Decision{Action: Lost}},
		{"master's shadow gone and there is nothing anywhere", broken(0, ""), nil,
			Decision{Action: Lost}},
	} {
		if got := Decide(tc.master, tc.child); got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.row, got, tc.want)
		}
	}
	for a, want := range map[Action]string{Nothing: "nothing", Push: "push", Pull: "pull", Rebuild: "rebuild",
		NeedsRecoveryKey: "needs recovery key", Lost: "lost", Action(99): "Action(99)"} {
		if a.String() != want {
			t.Errorf("Action %d prints %q, want %q", int(a), a, want)
		}
	}
}

// checkDecision holds Decide to what must be true of any answer.
func checkDecision(t *testing.T, m Master, child []Copy, d Decision) {
	t.Helper()
	if m.ShadowForeign {
		// The master's own files are under another key: that alone decides.
		if d != (Decision{Action: NeedsRecoveryKey}) {
			t.Fatalf("the master's snapshot is under another key, and the answer is %+v", d)
		}
		return
	}
	var newest uint64
	foreignOnTop, listed := false, false
	for _, c := range child {
		if c.OK && c.valid() {
			newest = max(newest, c.Gen)
		}
	}
	for _, c := range child {
		if c.OK && c.valid() && c.Gen == newest && c.KeyID != m.KeyID {
			foreignOnTop = true
		}
		if c == d.Copy {
			listed = true
		}
	}
	switch d.Action {
	case Pull, Rebuild:
		if d.Own {
			if d.Action == Pull || m.Gen == 0 || m.ShadowOK || d.Copy != (Copy{}) {
				t.Fatalf("a rebuild from the master's own generation it does not have, or does not need: %+v", d)
			}
			break
		}
		// What is fetched is a copy the child said it has intact, under the
		// master's key, and no intact copy of the child's is newer.
		if !listed || !d.Copy.OK || !d.Copy.valid() || d.Copy.KeyID != m.KeyID || d.Copy.Gen != newest {
			t.Fatalf("fetches %+v, which is not the child's newest intact copy under the master's key", d.Copy)
		}
		if (d.Action == Rebuild) == m.ShadowOK {
			t.Fatalf("%v with ShadowOK %v", d.Action, m.ShadowOK)
		}
		if d.Diverged && (d.Copy.Gen != m.Gen || d.Copy.Hash == m.Hash) {
			t.Fatalf("diverged without the same generation and another hash: %+v", d)
		}
		if d.Action == Pull && d.Copy.Gen < m.Gen {
			t.Fatalf("pulls generation %d over the master's newer %d", d.Copy.Gen, m.Gen)
		}
	case Push:
		// Only a master with a shadow and a generation pushes, and never
		// over something the child has that is as new.
		if !m.ShadowOK || m.Gen == 0 || newest >= m.Gen {
			t.Fatalf("pushes generation %d with ShadowOK %v over the child's %d", m.Gen, m.ShadowOK, newest)
		}
	case Lost:
		if m.ShadowOK || m.Gen != 0 || newest != 0 {
			t.Fatalf("lost with something left: %+v, child's newest %d", m, newest)
		}
	case Nothing:
		if !m.ShadowOK {
			t.Fatalf("nothing to do for a master without its shadow")
		}
	case NeedsRecoveryKey:
	default:
		t.Fatalf("an action that does not exist: %d", d.Action)
	}
	// The rule nothing may break: when the child's newest intact memory is
	// under another key, the machine is marked and nothing else happens.
	if foreignOnTop != (d.Action == NeedsRecoveryKey) {
		t.Fatalf("the child's newest intact copy is under another key: %v, and the answer is %v", foreignOnTop, d.Action)
	}
	if d.Action != Pull && d.Action != Rebuild && (d.Copy != (Copy{}) || d.Own || d.Diverged) {
		t.Fatalf("%v carries a copy or a flag: %+v", d.Action, d)
	}
}

func TestDecide_HoldsForEveryCombination(t *testing.T) {
	keys, hashes := []string{keyA, keyB, "bad"}, []string{hashX, hashY}
	var states []Copy
	for _, k := range keys {
		for _, gen := range []uint64{0, 1, 2, 3} {
			for _, h := range hashes {
				for _, ok := range []bool{true, false} {
					states = append(states, cp(k, gen, h, ok))
				}
			}
		}
	}
	n := 0
	for _, state := range []struct{ ok, foreign bool }{{true, false}, {false, false}, {false, true}, {true, true}} {
		for _, gen := range []uint64{0, 1, 2, 3} {
			m := Master{KeyID: keyA, Gen: gen, Hash: hashX, ShadowOK: state.ok, ShadowForeign: state.foreign}
			checkDecision(t, m, nil, Decide(m, nil))
			for i, a := range states {
				checkDecision(t, m, []Copy{a}, Decide(m, []Copy{a}))
				for _, b := range states[i:] {
					checkDecision(t, m, []Copy{a, b}, Decide(m, []Copy{a, b}))
					if d1, d2 := Decide(m, []Copy{a, b}), Decide(m, []Copy{b, a}); d1.Action != d2.Action || d1.Copy.Gen != d2.Copy.Gen {
						t.Fatalf("the order of the child's copies changes the answer: %+v, %+v", d1, d2)
					}
					n++
				}
			}
		}
	}
	t.Logf("%d pairs of copies checked", n)
}

func TestParseCopies(t *testing.T) {
	want := []Copy{cp(keyA, 9, hashX, true), cp(keyB, 1, hashY, false)}
	data, err := json.Marshal(want)
	must(t, err)
	got, err := ParseCopies(data)
	if err != nil || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	for _, empty := range []string{"[]", "null", " [ ] "} {
		if got, err := ParseCopies([]byte(empty)); err != nil || len(got) != 0 {
			t.Errorf("%q: %+v, %v", empty, got, err)
		}
	}
	one := `{"key":"` + keyA + `","gen":1,"hash":"` + hashX + `","ok":true}`
	for name, data := range map[string]string{
		"empty":                    "",
		"not JSON":                 "copies: none",
		"an object":                one,
		"two values":               "[" + one + "][" + one + "]",
		"text after":               "[" + one + "] and more",
		"an unknown field":         `[{"key":"` + keyA + `","gen":1,"hash":"` + hashX + `","ok":true,"path":"/etc"}]`,
		"generation zero":          `[{"key":"` + keyA + `","gen":0,"hash":"` + hashX + `","ok":true}]`,
		"a negative generation":    `[{"key":"` + keyA + `","gen":-1,"hash":"` + hashX + `","ok":true}]`,
		"a generation past int64":  `[{"key":"` + keyA + `","gen":9223372036854775808,"hash":"` + hashX + `","ok":true}]`,
		"a generation past uint64": `[{"key":"` + keyA + `","gen":18446744073709551616,"hash":"` + hashX + `","ok":true}]`,
		"a fractional generation":  `[{"key":"` + keyA + `","gen":1.5,"hash":"` + hashX + `","ok":true}]`,
		"a generation as text":     `[{"key":"` + keyA + `","gen":"1","hash":"` + hashX + `","ok":true}]`,
		"a key id that is a path":  `[{"key":"../../x","gen":1,"hash":"` + hashX + `","ok":true}]`,
		"a hash in capitals":       `[{"key":"` + keyA + `","gen":1,"hash":"` + strings.Repeat("AB", 32) + `","ok":true}]`,
		"no hash":                  `[{"key":"` + keyA + `","gen":1,"ok":true}]`,
		"ok as text":               `[{"key":"` + keyA + `","gen":1,"hash":"` + hashX + `","ok":"yes"}]`,
		"too many copies":          "[" + strings.Repeat(one+",", maxStatementCopies) + one + "]",
		"too large":                "[" + strings.Repeat(" ", maxStatement) + "]",
	} {
		if got, err := ParseCopies([]byte(data)); err == nil {
			t.Errorf("%s: accepted as %+v", name, got)
		}
	}
	// What a child store lists, the master reads.
	s := newChildStore(t)
	put(t, s, keyA, 1, "one")
	damage(t, s, put(t, s, keyA, 2, "two"))
	listed, err := s.List()
	must(t, err)
	data, err = json.Marshal(listed)
	must(t, err)
	read, err := ParseCopies(data)
	if err != nil || len(read) != 2 || read[0] != listed[0] || read[1] != listed[1] || read[0].OK || !read[1].OK {
		t.Fatalf("a child store's listing: %+v, %v", read, err)
	}
}

func FuzzParseCopies(f *testing.F) {
	one := `{"key":"` + keyA + `","gen":1,"hash":"` + hashX + `","ok":true}`
	f.Add([]byte("[" + one + "]"))
	f.Add([]byte("[" + one + "," + one + "]"))
	f.Add([]byte(`[{"key":"../x","gen":18446744073709551615,"hash":"","ok":true}]`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`null`))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		copies, err := ParseCopies(data)
		if err != nil {
			if copies != nil {
				t.Fatal("copies came back with an error")
			}
			return
		}
		if len(copies) > maxStatementCopies {
			t.Fatalf("%d copies accepted", len(copies))
		}
		for _, c := range copies {
			if !c.valid() {
				t.Fatalf("accepted %+v", c)
			}
			if got, ok := parseCopyName(c.fileName()); !ok || got.KeyID != c.KeyID || got.Gen != c.Gen || got.Hash != c.Hash {
				t.Fatalf("%+v has no file name of its own", c)
			}
		}
		// And whatever was accepted, Decide answers within its rules, for a
		// master in any state.
		for _, m := range []Master{
			{KeyID: keyA, ShadowOK: true}, {KeyID: keyA}, {KeyID: keyA, Gen: 1, Hash: hashX, ShadowOK: true}, {KeyID: keyA, Gen: 1 << 40, Hash: hashY},
			{KeyID: keyA, Gen: 2, Hash: hashX, ShadowForeign: true},
		} {
			checkDecision(t, m, copies, Decide(m, copies))
		}
	})
}

// FuzzDecide builds both sides' statements from raw bytes, with fields that
// no parser has vetted, and holds the answer to the rules.
func FuzzDecide(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1, 7, 0, 1, 0, 7, 0, 1})
	f.Add([]byte{0, 3, 1, 0, 1, 9, 1, 1, 0, 9, 0, 0, 2, 255, 2, 1})
	f.Add([]byte{1, 0, 0, 1, 3, 5, 3, 1})
	f.Fuzz(func(t *testing.T, raw []byte) {
		keys := []string{keyA, keyB, "", "../../x", strings.ToUpper(keyA)}
		hashes := []string{hashX, hashY, "", "zz"}
		next := func() byte {
			if len(raw) == 0 {
				return 0
			}
			b := raw[0]
			raw = raw[1:]
			return b
		}
		gen := func() uint64 {
			switch b := next(); {
			case b == 255:
				return 1<<64 - 1
			case b == 254:
				return maxGen
			case b == 253:
				return maxGen + 1
			default:
				return uint64(b % 8)
			}
		}
		m := Master{KeyID: keyA, ShadowOK: next()%2 == 1, Gen: gen()}
		m.Hash = hashes[int(next())%2]
		m.ShadowForeign = next()%8 == 7
		var child []Copy
		for len(raw) > 0 && len(child) < 12 {
			child = append(child, Copy{
				KeyID: keys[int(next())%len(keys)], Gen: gen(), Hash: hashes[int(next())%len(hashes)], OK: next()%2 == 1,
			})
		}
		d := Decide(m, child)
		checkDecision(t, m, child, d)
		// The answer does not depend on the order the child listed them in.
		for i, j := 0, len(child)-1; i < j; i, j = i+1, j-1 {
			child[i], child[j] = child[j], child[i]
		}
		if r := Decide(m, child); r.Action != d.Action || r.Copy.Gen != d.Copy.Gen || r.Own != d.Own || r.Diverged != d.Diverged {
			t.Fatalf("reversed, the answer changes: %+v, then %+v", d, r)
		}
	})
}
