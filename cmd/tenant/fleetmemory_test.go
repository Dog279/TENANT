package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"tenant/internal/fleet"
	"tenant/internal/fleet/shadow"
	"tenant/internal/memory/semantic"
	"tenant/internal/secrets"
)

// The master's memory of its machines, wired to the link (fleetmemory.go):
// `tenant fleet init` and its recovery key, the shadows' life in the process
// that holds the fleet, and every row of "who wins" over a real link to a
// child in this process. fleet_e2e_memory_test.go does the same with every
// piece a process of its own.

// shippedRecoveryTerminal is the terminal init uses in a shipped Tenant.
var shippedRecoveryTerminal = openRecoveryTerminal

// Every init in this test binary, helper processes included, meets an
// operator who saves the recovery identity and gives it back: the fleet
// tests that are not about the recovery key need a fleet with memory.
func init() {
	openRecoveryTerminal = func() (*recoveryTerminal, error) { return operator(nil).open() }
}

var identityOnScreen = regexp.MustCompile(`AGE-SECRET-KEY-1[0-9A-Z]+`)

// fakeOperator is a person at init's terminal. answers are what they type,
// in turn: "saved" is the identity as the screen showed it, "" is Enter
// alone; with no answers left they type what they saved.
type fakeOperator struct {
	mu      sync.Mutex
	answers []string
	screen  bytes.Buffer
	opened  int
}

func operator(answers []string) *fakeOperator { return &fakeOperator{answers: answers} }

func (o *fakeOperator) open() (*recoveryTerminal, error) {
	o.mu.Lock()
	o.opened++
	o.mu.Unlock()
	return &recoveryTerminal{out: writerFunc(func(p []byte) (int, error) {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.screen.Write(p)
	}), read: func() (*secrets.LockedBuffer, error) {
		o.mu.Lock()
		defer o.mu.Unlock()
		typed := "saved"
		if len(o.answers) > 0 {
			typed, o.answers = o.answers[0], o.answers[1:]
		}
		if typed == "saved" { // the one shown last
			all := identityOnScreen.FindAllString(o.screen.String(), -1)
			typed = ""
			if len(all) > 0 {
				typed = all[len(all)-1]
			}
		}
		if typed == "" {
			return nil, nil
		}
		return secrets.NewLockedBuffer([]byte(typed))
	}}, nil
}

func (o *fakeOperator) shown() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.screen.String()
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// withOperator puts o at init's terminal for one test.
func withOperator(t *testing.T, open func() (*recoveryTerminal, error)) {
	prev := openRecoveryTerminal
	openRecoveryTerminal = open
	t.Cleanup(func() { openRecoveryTerminal = prev })
}

// notes captures what a command writes for a person (fleetNotes).
func notes(t *testing.T) *bytes.Buffer {
	var b bytes.Buffer
	prev := fleetNotes
	fleetNotes = &b
	t.Cleanup(func() { fleetNotes = prev })
	return &b
}

// --- init and the recovery key: refusals first ---

// Init with no terminal makes the fleet and its key, shows no secret
// anywhere, records no recovery key, and does not end as if it were done.
func TestFleetInitWithNoTerminal(t *testing.T) {
	l := newFleetLab(t)
	said := notes(t)
	withOperator(t, shippedRecoveryTerminal) // standard output is a pipe here: captureTenantStdout
	out, err := l.run("init", "--listen", "127.0.0.1:0", "--address", "127.0.0.1:9")
	if !errors.Is(err, errRecoveryNotSetUp) {
		t.Fatalf("init with no terminal: %v, want errRecoveryNotSetUp\n%s", err, out)
	}
	if strings.Contains(out+said.String(), "AGE-SECRET-KEY") {
		t.Fatal("init with no terminal printed a recovery identity")
	}
	if !strings.Contains(said.String(), "not both a terminal") || !strings.Contains(out, "this Tenant is now a fleet master") {
		t.Fatalf("what init said:\n%s\n%s", out, said)
	}
	if _, err := os.Stat(fleet.KeyPath(l.cfg)); err != nil {
		t.Fatalf("the fleet key was not made: %v", err)
	}
	if rec, _, err := fleet.Open(l.cfg).Recovery(); err != nil || rec != "" {
		t.Fatalf("a recovery key was recorded: %q %v", rec, err)
	}
	// The fleet runs; its machines' memory is unavailable, and says why.
	l.stop = startFleetHub(context.Background(), l.c, "serve", newApprovalBroker(quietLog), l.tell, quietLog)
	if fleetManager() == nil {
		t.Fatalf("the hub did not take the fleet: %v", l.told())
	}
	if state, detail := fleetMemoryState("m0123456789abcdef"); state != memoryUnknown || !strings.Contains(detail, "no recovery key") {
		t.Fatalf("memory state: %q %q", state, detail)
	}
	// Enroll, which runs init, says so too, and still gives its code.
	code, err := l.run("enroll", "web1")
	if err != nil || !strings.HasPrefix(code, "tntfleet1_") || !strings.Contains(said.String(), "no recovery key yet") {
		t.Fatalf("enroll: %v\n%s\n%s", err, code, said)
	}
}

// The operator gives back something else, or nothing: nothing is recorded,
// and the identity shown is never shown again.
func TestFleetInitWrongRecoveryIdentity(t *testing.T) {
	other, _, err := shadow.NewRecovery()
	if err != nil {
		t.Fatal(err)
	}
	var stranger string
	_ = other.With(func(p []byte) error { stranger = string(p); return nil })
	_ = other.Release()
	for name, answers := range map[string][]string{
		"another recovery identity":            {stranger, stranger, stranger},
		"not an identity":                      {"AGE-SECRET-KEY-1NOTONE", "age1publichalf", "password"},
		"the identity cut short":               {"cut", "cut", "cut"},
		"Enter alone":                          {""},
		"wrong, then Enter alone":              {stranger, ""},
		"the identity in lower case, and more": {"lower", "lower", "lower"},
	} {
		t.Run(name, func(t *testing.T) {
			l := newFleetLab(t)
			said := notes(t)
			op := operator(answers)
			withOperator(t, func() (*recoveryTerminal, error) {
				term, err := op.open()
				read := term.read
				term.read = func() (*secrets.LockedBuffer, error) {
					// "cut" and "lower" are made from what the screen shows.
					op.mu.Lock()
					next := ""
					if len(op.answers) > 0 {
						next = op.answers[0]
					}
					shownID := identityOnScreen.FindString(op.screen.String())
					op.mu.Unlock()
					switch next {
					case "cut":
						op.mu.Lock()
						op.answers[0] = shownID[:len(shownID)-1]
						op.mu.Unlock()
					case "lower":
						op.mu.Lock()
						op.answers[0] = strings.ToLower(shownID) + "x"
						op.mu.Unlock()
					}
					return read()
				}
				return term, err
			})
			out, err := l.run("init", "--listen", "127.0.0.1:0")
			if !errors.Is(err, errRecoveryNotSetUp) {
				t.Fatalf("init: %v\n%s", err, out)
			}
			if rec, _, _ := fleet.Open(l.cfg).Recovery(); rec != "" {
				t.Fatalf("a recovery key was recorded: %q", rec)
			}
			if n := len(identityOnScreen.FindAllString(op.shown(), -1)); n != 1 {
				t.Fatalf("the identity was shown %d times", n)
			}
			if strings.Contains(out+said.String(), "AGE-SECRET-KEY") {
				t.Fatal("the identity reached standard output or the notes")
			}
			if !strings.Contains(said.String(), "is void") {
				t.Fatalf("the operator was not told: %s", said)
			}
		})
	}
}

// The operator saves the identity and gives it back: its public half and
// the proof are recorded, the key is made once, and a later init asks
// nothing and replaces nothing.
func TestFleetInitMakesTheKeysOnce(t *testing.T) {
	l := newFleetLab(t)
	op := operator(nil)
	withOperator(t, op.open)
	before := time.Now().Add(-time.Second)
	out := l.must("init", "--listen", "127.0.0.1:0")
	rec, proven, err := fleet.Open(l.cfg).Recovery()
	if err != nil || !strings.HasPrefix(rec, "age1") || proven.Before(before) {
		t.Fatalf("recorded: %q %v %v", rec, proven, err)
	}
	if !strings.Contains(out, "recovery key   proven") || strings.Contains(out, "AGE-SECRET-KEY") {
		t.Fatalf("init said:\n%s", out)
	}
	if n := len(identityOnScreen.FindAllString(op.shown(), -1)); n != 1 {
		t.Fatalf("the identity was shown %d times", n)
	}
	// Nothing Tenant keeps holds the identity: it was shown and given back.
	shownID := identityOnScreen.FindString(op.shown())
	assertNowhere(t, shownID, l.cfg, l.data)
	key := readRaw(t, fleet.KeyPath(l.cfg))
	again := l.must("init")
	if op.opened != 1 || !bytes.Equal(key, readRaw(t, fleet.KeyPath(l.cfg))) || !strings.Contains(again, "recovery key   proven") {
		t.Fatalf("a second init opened the terminal %d times or replaced the key\n%s", op.opened, again)
	}
	if r2, _, _ := fleet.Open(l.cfg).Recovery(); r2 != rec {
		t.Fatal("a second init replaced the recovery key")
	}
}

// A recovery key that was recorded and never proven (a fleet.json from before
// the proof, or put right by hand) is doctor's warning, with the command that
// proves it; init at a terminal then asks for the identity the operator kept,
// records the proof only for the right one, and makes nothing new.
func TestFleetRecoveryKeyNeverProven(t *testing.T) {
	l := newFleetLab(t)
	op := operator(nil)
	withOperator(t, op.open)
	l.must("init", "--listen", "127.0.0.1:0")
	kept := identityOnScreen.FindString(op.shown())
	store := fleet.Open(l.cfg)
	rec, _, err := store.Recovery()
	if err != nil || kept == "" {
		t.Fatal(err)
	}
	if err := store.SetRecovery(rec, time.Time{}); err != nil {
		t.Fatal(err)
	}
	e := &doctorEnv{c: l.c}
	r := checkFleetKeys(context.Background(), e)
	if r.Status != statusWarn || !strings.Contains(r.Detail, "the recovery key was never proven") ||
		r.Fix != "tenant fleet init, at a terminal: it asks for the recovery identity you kept and proves it" {
		t.Fatalf("doctor on an unproven recovery key: %s %q fix %q", r.Status.tag(), r.Detail, r.Fix)
	}
	other, _, err := shadow.NewRecovery()
	if err != nil {
		t.Fatal(err)
	}
	var stranger string
	_ = other.With(func(p []byte) error { stranger = string(p); return nil })
	_ = other.Release()
	said := notes(t)
	asked := operator([]string{stranger, stranger, stranger})
	withOperator(t, asked.open)
	if _, err := l.run("init"); !errors.Is(err, errRecoveryNotSetUp) || !strings.Contains(said.String(), "still not proven") {
		t.Fatalf("init given another identity: %v, %s", err, said)
	}
	if _, proven, _ := store.Recovery(); !proven.IsZero() {
		t.Fatal("another identity was taken as the proof")
	}
	asked = operator([]string{stranger, kept})
	withOperator(t, asked.open)
	key := readRaw(t, fleet.KeyPath(l.cfg))
	out := l.must("init")
	if r2, proven, _ := store.Recovery(); r2 != rec || proven.IsZero() || !strings.Contains(out, "recovery key   proven") {
		t.Fatalf("after the identity was given: %q %v\n%s", r2, proven, out)
	}
	if identityOnScreen.MatchString(asked.shown()) || !bytes.Equal(key, readRaw(t, fleet.KeyPath(l.cfg))) {
		t.Fatal("proving the recovery key showed an identity or made a new key")
	}
	if r := checkFleetKeys(context.Background(), e); r.Status == statusWarn && strings.Contains(r.Detail, "never proven") || !strings.Contains(r.Detail, "the recovery key was proven") {
		t.Fatalf("doctor after the proof: %s %q", r.Status.tag(), r.Detail)
	}
}

// A fleet made before there was memory (an identity, no fleet key, no
// recovery key) keeps working, and gets both at its next init.
func TestFleetMadeBeforeMemory(t *testing.T) {
	l := newFleetLab(t)
	lc, err := loadLaunchConfig(l.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistLaunchConfig(l.cfg, lc, func(lc *launchConfig) { lc.Fleet.Listen = "127.0.0.1:0" }); err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.MintIdentity(fleet.MasterIdentityPath(l.cfg)); err != nil {
		t.Fatal(err)
	}
	if err := fleet.Open(l.cfg).SetAddresses([]string{"127.0.0.1:9"}); err != nil {
		t.Fatal(err)
	}
	if out := l.must("list"); !strings.Contains(out, "no machines yet") {
		t.Fatalf("list on an old fleet:\n%s", out)
	}
	l.stop = startFleetHub(context.Background(), l.c, "serve", newApprovalBroker(quietLog), l.tell, quietLog)
	if fleetManager() == nil {
		t.Fatal("an old fleet was not taken")
	}
	if _, detail := fleetMemoryState("m0123456789abcdef"); !strings.Contains(detail, "unavailable") {
		t.Fatalf("an old fleet's memory: %q", detail)
	}
	l.must("init")
	if _, err := os.Stat(fleet.KeyPath(l.cfg)); err != nil {
		t.Fatal("init did not make the fleet key of an old fleet")
	}
	eventually(t, "the running master opens the memory once init made the keys", func() bool {
		_, detail := fleetMemoryState("m0123456789abcdef")
		return strings.Contains(detail, "not compared yet")
	})
}

// --- who wins, over a real link to a child in this process ---

// memLab is a lab master whose hub keeps its machines' memory, and one
// confirmed machine that keeps its copies in its own directory.
type memLab struct {
	*fleetLab
	ch     labChild
	copies *shadow.ChildStore
	stop   func() // the child
}

type countingEmbedder struct{}

func (countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0.5, 0.25}
	}
	return out, nil
}

func newMemLab(t *testing.T) *memLab {
	t.Helper()
	prevEvery, prevOpt, prevEmb := fleetMemoryEvery, fleetShadowOptions, fleetEmbedder.Load()
	fleetMemoryEvery, fleetShadowOptions = time.Hour, shadow.Options{FlushAfter: 50 * time.Millisecond}
	setFleetEmbedder(countingEmbedder{})
	t.Cleanup(func() {
		fleetMemoryEvery, fleetShadowOptions = prevEvery, prevOpt
		fleetEmbedder.Store(prevEmb)
	})
	l := &memLab{fleetLab: newFleetLab(t)}
	l.master()
	l.ch = l.enroll("web1", fleet.Reported{})
	l.confirm("web1", l.ch)
	var err error
	if l.copies, err = shadow.OpenChildStore(filepath.Join(l.ch.dir, "memory")); err != nil {
		t.Fatal(err)
	}
	l.connect()
	// The inventory confirmation asked for (TEN-532) is written before the
	// test counts generations.
	eventually(t, "the inventory taken at confirmation is recorded", func() bool {
		_, err := os.Lstat(filepath.Join(fleetShadowDir(l.data), l.ch.id, inventoryMark))
		return errors.Is(err, fs.ErrNotExist)
	})
	return l
}

// connect runs the machine's child until disconnect or the end of the test.
func (l *memLab) connect() {
	l.t.Helper()
	client, err := fleet.NewClient(fleet.ClientConfig{Dir: l.ch.dir, Tools: &labTools{}, Version: "1.2.3", User: "root", Memory: l.copies, Log: quietLog})
	if err != nil {
		l.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = client.Run(ctx)
		close(done)
	}()
	l.stop = func() {
		cancel()
		<-done
	}
	l.t.Cleanup(l.disconnect)
	eventually(l.t, "the machine is online", func() bool { return online(fleetManager(), l.ch.id) })
}

func (l *memLab) disconnect() {
	if l.stop != nil {
		l.stop()
		l.stop = nil
		eventually(l.t, "the machine is offline", func() bool { return !online(fleetManager(), l.ch.id) })
	}
}

// mem is the hub's memory, which must be open.
func (l *memLab) mem() *fleetMemory {
	l.t.Helper()
	h := theFleet.Load()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mem == nil {
		l.t.Fatalf("the hub's memory is not open: %s", h.memProblem)
	}
	return h.mem
}

// note writes a fact into the machine's memory, as fleet_note will.
func (l *memLab) note(text string) {
	l.t.Helper()
	ctx := context.Background()
	p, err := l.mem().shadows.Acquire(ctx, l.ch.id)
	if err != nil {
		l.t.Fatal(err)
	}
	defer p.Release()
	long := time.Unix(1_600_000_000, 0)
	if _, err := p.Semantic.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: text, FirstSeen: long, LastConfirmed: long,
		EmbedderID: "embedder", Embedding: []float32{1, 2, 3}}); err != nil {
		l.t.Fatal(err)
	}
}

// facts is the machine's memory, and how many of its facts have no vector.
func (l *memLab) facts() (string, int) {
	l.t.Helper()
	p, err := l.mem().shadows.Acquire(context.Background(), l.ch.id)
	if err != nil {
		l.t.Fatal(err)
	}
	defer p.Release()
	all, err := p.Semantic.List(context.Background(), semantic.ListFilter{})
	if err != nil {
		l.t.Fatal(err)
	}
	var out []string
	bare := 0
	for _, f := range all {
		// The inventory taken at confirmation (TEN-532, fleetrecall.go) is in
		// every confirmed machine's memory; these tests are about the rest.
		if o, _, _ := p.Semantic.Origin(context.Background(), f.ID); strings.TrimPrefix(o.Job, shadow.RecoveredOrigin) == originInventory {
			continue
		}
		out = append(out, f.Fact)
		if len(f.Embedding) == 0 {
			bare++
		}
	}
	slices.Sort(out)
	return strings.Join(out, " | "), bare
}

// mint makes the next generation now, as the five-minute pass does.
func (l *memLab) mint() shadow.Copy {
	l.t.Helper()
	c, _, err := l.mem().shadows.Mint(context.Background(), l.ch.id)
	if err != nil {
		l.t.Fatal(err)
	}
	return c
}

// compare runs who wins now and waits until the machine's memory state is
// want with a detail that holds detail.
func (l *memLab) compare(want, detail string) {
	l.t.Helper()
	l.mem().kick(l.ch.id)
	l.await(want, detail)
}

func (l *memLab) await(want, detail string) {
	l.t.Helper()
	var state, got string
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if state, got = fleetMemoryState(l.ch.id); state == want && strings.Contains(got, detail) {
			// and the comparison that said so is over
			m := l.mem()
			m.mu.Lock()
			busy := m.syncing[l.ch.id]
			m.mu.Unlock()
			if !busy {
				return
			}
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("the machine's memory is %q (%s), want %q with %q\n%v", state, got, want, detail, l.told())
		}
	}
}

// reopen closes the hub's memory, lets mutate change the files, and opens
// it again the way a restarted master would; the machine stays linked.
func (l *memLab) reopen(mutate func()) {
	l.t.Helper()
	h := theFleet.Load()
	// The hub's lock is held until the files are changed: its tick, which
	// opens the memory again, waits for it.
	h.mu.Lock()
	mem := h.mem
	h.mem, h.memInputs, h.memProblem = nil, "", ""
	if mem != nil {
		mem.close()
	}
	if mutate != nil {
		mutate()
	}
	h.mu.Unlock()
	eventually(l.t, "the hub opens the memory again", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.mem != nil
	})
}

// childFiles is every file the machine keeps of its memory, name and bytes.
func (l *memLab) childFiles() map[string][]byte {
	l.t.Helper()
	out := map[string][]byte{}
	dir := filepath.Join(l.ch.dir, "memory")
	entries, err := os.ReadDir(dir)
	if err != nil {
		l.t.Fatal(err)
	}
	for _, e := range entries {
		out[e.Name()] = readRaw(l.t, filepath.Join(dir, e.Name()))
	}
	return out
}

func (l *memLab) gens() string {
	l.t.Helper()
	copies, err := l.copies.List()
	if err != nil {
		l.t.Fatal(err)
	}
	var out []string
	for _, c := range copies {
		s := strconv.FormatUint(c.Gen, 10)
		if !c.OK {
			s += "!"
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func readRaw(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// assertNowhere fails if text is in any file under the dirs.
func assertNowhere(t *testing.T, text string, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte(text)) {
				t.Errorf("%s holds %q", path, text)
			}
			return nil
		})
	}
}

// Equal, and master newer: the newest generation goes to the machine, and
// then nothing is done.
func TestFleetMemoryEqualAndMasterNewer(t *testing.T) {
	l := newMemLab(t)
	l.compare(memoryInStep, "no memory of this machine is kept yet")
	l.note("web1 runs nginx")
	c1 := l.mint()
	l.compare(memoryInStep, "generation 1 on both sides")
	if got := l.gens(); got != "1" {
		t.Fatalf("the machine keeps %q", got)
	}
	if _, err := l.copies.Get(c1); err != nil {
		t.Fatalf("the machine's copy: %v", err)
	}
	before := l.childFiles()
	l.compare(memoryInStep, "generation 1 on both sides")
	if after := l.childFiles(); len(after) != len(before) {
		t.Fatal("an equal comparison changed the machine's copies")
	}
	// A generation made while the machine is offline: behind, then pushed
	// when it is back.
	l.disconnect()
	l.note("web1's /var fills up")
	l.mem().pass() // the five-minute pass: a generation is made without the machine
	l.await(memoryBehind, "generation 2")
	l.connect()
	l.await(memoryInStep, "generation 2 on both sides")
	if got := l.gens(); got != "2 1" {
		t.Fatalf("the machine keeps %q", got)
	}
	assertNowhere(t, "nginx", l.ch.dir, l.data)
}

// Delete the machine's copies and they return.
func TestFleetMemoryTheChildsCopiesReturn(t *testing.T) {
	l := newMemLab(t)
	l.note("web1 runs nginx")
	l.mint()
	l.compare(memoryInStep, "generation 1 on both sides")
	for name := range l.childFiles() {
		if err := os.Remove(filepath.Join(l.ch.dir, "memory", name)); err != nil {
			t.Fatal(err)
		}
	}
	l.disconnect()
	l.connect() // a reconnect compares
	l.await(memoryInStep, "generation 1 on both sides")
	if got := l.gens(); got != "1" {
		t.Fatalf("after its copies were deleted the machine keeps %q", got)
	}
}

// The master's snapshot is gone or damaged: it is rebuilt from the
// machine's newest copy that verifies, and re-embedded.
func TestFleetMemoryTheShadowIsRebuilt(t *testing.T) {
	for name, lose := range map[string]func(dir string){
		"deleted": func(dir string) { _ = os.Remove(filepath.Join(dir, "shadow.snap")) },
		"damaged": func(dir string) {
			p := filepath.Join(dir, "shadow.snap")
			b, _ := os.ReadFile(p)
			b[len(b)/2] ^= 1
			_ = os.WriteFile(p, b, 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := newMemLab(t)
			l.note("web1 runs nginx")
			l.note("web1's /var fills up")
			l.mint()
			l.compare(memoryInStep, "generation 1 on both sides")
			l.reopen(func() { lose(filepath.Join(fleetShadowDir(l.data), l.ch.id)) })
			// A snapshot damaged further in than its first line is found when
			// the shadow is first used: that use fails, and asks for a rebuild.
			if _, err := fleetShadow(context.Background(), l.ch.id); name == "damaged" && !errors.Is(err, shadow.ErrDamaged) {
				t.Fatalf("the damaged shadow: %v", err)
			}
			eventually(t, "the shadow is rebuilt", func() bool {
				p, err := fleetShadow(context.Background(), l.ch.id)
				if err == nil {
					p.Release()
				}
				return err == nil
			})
			l.await(memoryInStep, "generation 1 on both sides")
			if got, bare := l.facts(); got != "web1 runs nginx | web1's /var fills up" || bare != 0 {
				t.Fatalf("the rebuilt shadow holds %q, %d facts without a vector", got, bare)
			}
			if name == "damaged" && !hasFiles(filepath.Join(fleetShadowDir(l.data), l.ch.id, "aside")) {
				t.Fatal("the damaged snapshot was not kept aside")
			}
			// The vectors reached the disk: they survive another reopen.
			l.reopen(nil)
			if _, bare := l.facts(); bare != 0 {
				t.Fatalf("after a reopen %d facts have no vector", bare)
			}
		})
	}
}

// The master was restored from an older backup: the machine's newer memory
// comes back, and the shadow it replaces is kept aside. Then the restored
// master writes before the machine is back: the same generation, other
// content; the machine's stands, the master's is kept aside, and the
// operator is told.
func TestFleetMemoryRestoreFromAnOlderBackup(t *testing.T) {
	l := newMemLab(t)
	dir := fleetShadowDir(l.data)
	l.note("one")
	l.mint()
	l.compare(memoryInStep, "generation 1")
	backup := t.TempDir()
	l.reopen(func() { copyTree(t, dir, backup) })
	l.note("two")
	l.mint()
	l.compare(memoryInStep, "generation 2")
	l.reopen(func() { restoreTree(t, backup, dir) })
	l.await(memoryInStep, "generation 2 on both sides")
	if got, _ := l.facts(); got != "one | two" {
		t.Fatalf("after the restore the shadow holds %q", got)
	}
	if aside := filepath.Join(dir, l.ch.id, "aside"); !hasFiles(aside) {
		var tree []string
		_ = filepath.WalkDir(dir, func(p string, _ fs.DirEntry, _ error) error { tree = append(tree, p); return nil })
		t.Fatalf("the shadow the restore had was not kept aside: %v", tree)
	}

	// Restored again, and this time the master writes before the machine is
	// back: the same generation number with other content.
	l.disconnect()
	l.reopen(func() { restoreTree(t, backup, dir) })
	l.note("written after the restore")
	if c := l.mint(); c.Gen != 2 {
		t.Fatalf("the restored master's next generation is %d", c.Gen)
	}
	l.connect()
	l.await(memoryInStep, "generation 2 on both sides")
	if got, _ := l.facts(); got != "one | two" {
		t.Fatalf("the machine's generation 2 did not stand: %q", got)
	}
	told := strings.Join(l.told(), "\n")
	if !strings.Contains(told, "the machine's copy stands") || !strings.Contains(told, "kept aside") {
		t.Fatalf("the operator was not told:\n%s", told)
	}
}

// copyTree copies the files of from into to.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	if err := os.CopyFS(to, os.DirFS(from)); err != nil {
		t.Fatal(err)
	}
}

// restoreTree replaces dir with the copy in from.
func restoreTree(t *testing.T, from, dir string) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	copyTree(t, from, dir)
	_ = os.Remove(filepath.Join(dir, ".shadows.lock")) // the backup's copy of a lock file is no lock
}

func hasFiles(dir string) bool {
	entries, _ := os.ReadDir(dir)
	return len(entries) > 0
}

func childCopyName(t *testing.T, s *shadow.ChildStore, gen uint64) string {
	t.Helper()
	copies, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range copies {
		if c.Gen == gen {
			return c.KeyID + "." + strconv.FormatUint(c.Gen, 10) + "." + c.Hash + ".snap"
		}
	}
	t.Fatalf("no copy of generation %d", gen)
	return ""
}

// FuzzRecordedMemoryState: the record of every machine's memory state the
// holder keeps for other processes, as `tenant fleet show` reads it: what it
// shows is a known state and one line of printable text, whatever the file.
func FuzzRecordedMemoryState(f *testing.F) {
	for _, seed := range []string{
		`{"m0123456789abcdef":{"state":"in_step","detail":"generation 2 on both sides","at":"2026-11-19T10:00:00Z"}}`,
		`{"m0123456789abcdef":{"state":"\u001b[2Jowned","detail":"\u001b]0;title\u0007\nsecond line","at":"x"}}`,
		`{"m0123456789abcdef":null}`, `[]`, `null`, ``, `{`, strings.Repeat("A", 5<<20),
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		data := t.TempDir()
		if err := os.MkdirAll(fleetShadowDir(data), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fleetMemoryStatesPath(data), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		state, detail := recordedMemoryState(data, "m0123456789abcdef")
		switch state {
		case memoryUnknown, memoryInStep, memoryBehind, memoryNeedsKey:
		default:
			t.Fatalf("an unknown state is shown: %q", state)
		}
		if strings.ContainsFunc(detail, func(r rune) bool { return !unicode.IsPrint(r) }) || !utf8.ValidString(detail) {
			t.Fatalf("the detail is not one line of printable text: %q", detail)
		}
	})
}
