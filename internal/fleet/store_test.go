package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/secrets"
)

func TestOpenWithoutAFleetLeavesNothingBehind(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	s := Open(dir)
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	if list, err := s.Machines(); err != nil || len(list) != 0 {
		t.Errorf("Machines = %v, %v", list, err)
	}
	if _, err := s.Machine("m0000000000000000"); !errors.Is(err, ErrUnknown) {
		t.Errorf("Machine = %v", err)
	}
	if err := s.Check("m0000000000000000"); !errors.Is(err, ErrUnknown) {
		t.Errorf("Check = %v", err)
	}
	s.states(true) // what Watch does every second
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("looking at a fleet that does not exist created %s", dir)
	}
}

func TestStoreKeepsNoToken(t *testing.T) {
	m := newMaster(t)
	m.add(t, "box", StateConfirmed)
	code := must(m.store.CreateEnrollment(m.id.Fingerprint, "next", 0, ""))(t)
	b := must(os.ReadFile(StorePath(m.dir)))(t)
	// The file names no credential a reader could use: no token, and not
	// the secret of the code that is still good.
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(b), []byte("token")) {
		t.Errorf("fleet.json has a token field:\n%s", b)
	}
	if secret := secretOf(t, code); bytes.Contains(b, []byte(secret)) {
		t.Error("fleet.json holds the secret of an enrollment code")
	}
	if report := secrets.Probe(StorePath(m.dir)); !report.Protected {
		t.Errorf("fleet.json is not owner-only: %+v", report)
	}
}

func TestMachineLifecycle(t *testing.T) {
	m := newMaster(t)
	s := m.store
	box, boxID := m.add(t, "box", StatePending)
	if box.State != StatePending || box.Fingerprint != boxID.Fingerprint || box.Reported != linuxBox || box.EnrolledAt.IsZero() || !validID(box.ID) {
		t.Fatalf("enrolled machine = %+v", box)
	}

	// Pending: nothing but confirm, revoke and rename applies.
	if err := s.Pause(box.ID); err == nil {
		t.Error("paused a pending machine")
	}
	if err := s.Resume(box.ID); err == nil {
		t.Error("resumed a pending machine, which would confirm it without its code")
	}
	if err := s.Remove(box.ID); err == nil {
		t.Error("removed a machine that is not revoked")
	}
	if got := must(s.Machine(box.ID))(t); got.State != StatePending {
		t.Fatalf("state = %s after refused changes", got.State)
	}

	right := ConfirmCode(m.id.Fingerprint, boxID.Fingerprint)
	if _, err := s.Confirm(m.id.Fingerprint, box.ID, right); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(m.id.Fingerprint, box.ID, right); err == nil {
		t.Error("confirmed a machine twice")
	}
	if err := s.Check(box.ID); err != nil {
		t.Errorf("Check of a confirmed machine = %v", err)
	}

	if err := s.Pause(box.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Pause(box.ID); err != nil {
		t.Errorf("pausing twice = %v", err)
	}
	if err := s.Check(box.ID); !errors.Is(err, ErrPaused) {
		t.Errorf("Check of a paused machine = %v", err)
	}
	if err := s.Resume(box.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(box.ID); err != nil {
		t.Errorf("Check after resume = %v", err)
	}

	next := Reported{OS: "linux", User: "root", Version: "1.2.4", Sandbox: "host", Ceiling: "no-exec"}
	if err := s.SetReported(box.ID, next); err != nil {
		t.Fatal(err)
	}
	if got := must(s.Machine(box.ID))(t); got.Reported != next {
		t.Errorf("Reported = %+v", got.Reported)
	}
	for _, bad := range []Reported{
		{OS: "plan9", Ceiling: "full"},
		{OS: "linux", Ceiling: "everything"},
		{OS: "linux", Ceiling: "full", User: "root\x1b[2J"},
		{OS: "linux", Ceiling: "full", User: "a\u202eb"},
		{OS: "linux", Ceiling: "full", Version: strings.Repeat("9", maxReportedLen+1)},
		{OS: "linux", Ceiling: "full", Sandbox: "\xff"},
	} {
		if err := s.SetReported(box.ID, bad); err == nil {
			t.Errorf("SetReported(%+v) accepted", bad)
		}
	}

	if err := s.Revoke(box.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(box.ID); err != nil {
		t.Errorf("revoking twice = %v", err)
	}
	got := must(s.Machine(box.ID))(t)
	if got.State != StateRevoked || got.RevokedAt.IsZero() {
		t.Errorf("revoked machine = %+v", got)
	}
	if err := s.Check(box.ID); !errors.Is(err, ErrRevoked) {
		t.Errorf("Check of a revoked machine = %v", err)
	}
	// Revoked is the end: nothing brings the certificate back.
	for name, err := range map[string]error{
		"Resume":      s.Resume(box.ID),
		"Pause":       s.Pause(box.ID),
		"Rename":      s.Rename(box.ID, "again"),
		"SetReported": s.SetReported(box.ID, linuxBox),
	} {
		if err == nil {
			t.Errorf("%s worked on a revoked machine", name)
		}
	}
	if _, err := s.Confirm(m.id.Fingerprint, box.ID, right); err == nil {
		t.Error("confirmed a revoked machine")
	}
	if got := must(s.Machine(box.ID))(t); got.State != StateRevoked {
		t.Fatalf("state = %s", got.State)
	}

	if err := s.Remove(box.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Machine(box.ID); !errors.Is(err, ErrUnknown) {
		t.Errorf("a removed machine: %v", err)
	}
	for name, err := range map[string]error{"Revoke": s.Revoke(box.ID), "Remove": s.Remove(box.ID), "Pause": s.Pause(box.ID), "Rename": s.Rename(box.ID, "x")} {
		if !errors.Is(err, ErrUnknown) {
			t.Errorf("%s of an unknown machine = %v", name, err)
		}
	}
}

func TestNames(t *testing.T) {
	for _, good := range []string{"a", "box", "DISPATCH-01", "a.b_c-d", "9lives", strings.Repeat("a", maxNameLen)} {
		if !validName(good) {
			t.Errorf("validName(%q) = false", good)
		}
	}
	for _, bad := range []string{"", "-a", ".a", "_a", "a b", "a/b", `a\b`, "a\n", "é", "..", strings.Repeat("a", maxNameLen+1),
		"m0123456789abcdef"} { // an id is not a name; "m0123456789abcdeg" is not an id
		if validName(bad) {
			t.Errorf("validName(%q) = true", bad)
		}
	}

	if !validName("m0123456789abcdeg") || validID("m0123456789abcdeg") || validID("n0123456789abcdef") || !validID("m0123456789abcdef") {
		t.Error("validID does not accept exactly m and sixteen lowercase hex digits")
	}

	m := newMaster(t)
	s := m.store
	one, _ := m.add(t, "one", StateConfirmed)
	two, _ := m.add(t, "two", StatePending)
	if err := s.Rename(one.ID, "Two"); err == nil {
		t.Error("renamed a machine to another's name, differing only in case")
	}
	if err := s.Rename(one.ID, "not a name"); err == nil {
		t.Error("an invalid name was accepted")
	}
	if err := s.Rename(one.ID, "ONE"); err != nil {
		t.Errorf("changing only the case of a name: %v", err)
	}
	if err := s.Rename(one.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if got := must(s.ByName("FIRST"))(t); got.ID != one.ID {
		t.Errorf("ByName found %s, want %s: the id does not change with the name", got.ID, one.ID)
	}
	if _, err := s.ByName("one"); !errors.Is(err, ErrUnknown) {
		t.Errorf("the old name still resolves: %v", err)
	}

	// A name a code is out for is not free either.
	must(s.CreateEnrollment(m.id.Fingerprint, "three", 0, ""))(t)
	if err := s.Rename(one.ID, "three"); err == nil {
		t.Error("renamed a machine to a name an enrollment code holds")
	}
	if _, err := s.CreateEnrollment(m.id.Fingerprint, "two", 0, ""); err == nil {
		t.Error("made a code for a name a pending machine has")
	}

	// A revoked machine gives its name up.
	if err := s.Revoke(two.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ByName("two"); !errors.Is(err, ErrUnknown) {
		t.Errorf("a revoked machine still answers to its name: %v", err)
	}
	if err := s.Rename(one.ID, "two"); err != nil {
		t.Errorf("a revoked machine's name could not be reused: %v", err)
	}
	list := must(s.Machines())(t)
	if len(list) != 2 {
		t.Errorf("Machines lists %d, want both the live and the revoked one", len(list))
	}
}

func TestAddresses(t *testing.T) {
	for _, good := range []string{"10.0.0.1:7443", "master.lan:7443", "[::1]:7443", "[fe80::1%eth0]:1", "a_b:65535"} {
		if !validAddress(good) {
			t.Errorf("validAddress(%q) = false", good)
		}
	}
	for _, bad := range []string{"", "host", "host:", ":7443", "host:0", "host:65536", "host:+80", "host:80 ", "host:999999",
		"http://host:80", "user@host:80", "host/path:80", "ho st:80", "host:80/x", strings.Repeat("a", maxAddressLen) + ":80"} {
		if validAddress(bad) {
			t.Errorf("validAddress(%q) = true", bad)
		}
	}
	m := newMaster(t)
	want := []string{"10.0.0.1:7443", "master.lan:7443"}
	if err := m.store.SetAddresses(want); err != nil {
		t.Fatal(err)
	}
	got := must(m.store.Addresses())(t)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Addresses = %v", got)
	}
	got[0] = "changed"
	if again := must(m.store.Addresses())(t); again[0] != want[0] {
		t.Error("the caller's copy of the addresses is the store's own")
	}
	if err := m.store.SetAddresses([]string{"no port"}); err == nil {
		t.Error("an address without a port was accepted")
	}
	if err := m.store.SetAddresses(make([]string, maxAddresses+1)); err == nil {
		t.Error("too many addresses were accepted")
	}
}

// fleet.json that cannot be read: the store says so and nothing is allowed.
// It never carries on with what it read before, and never writes over the
// file it could not read.
func TestUnreadableStoreFailsClosed(t *testing.T) {
	m := newMaster(t)
	box, boxID := m.add(t, "box", StateConfirmed)
	path := StorePath(m.dir)
	good := must(os.ReadFile(path))(t)
	link := must(MasterLinkTLS(m.id, m.store))(t)
	client := must(ChildTLS(boxID, m.id.Fingerprint))(t)

	twoPins := strings.Replace(string(good), `"machines": {`, `"machines": {
    "m00000000000000aa": {"name": "twin", "fingerprint": "`+boxID.Fingerprint+`", "state": "confirmed", "reported": {"os": "linux", "ceiling": "full"}, "enrolled_at": "2026-01-01T00:00:00Z"},`, 1)
	if twoPins == string(good) {
		t.Fatal("the test did not change the file")
	}
	for name, bad := range map[string]string{
		"empty":                "",
		"zeros":                strings.Repeat("\x00", 512),
		"garbage":              "not json at all",
		"half written":         string(good[:len(good)/2]),
		"two values":           string(good) + string(good),
		"newer version":        strings.Replace(string(good), `"version": 1`, `"version": 2`, 1),
		"unknown key":          strings.Replace(string(good), `"version": 1`, `"version": 1, "token": "x"`, 1),
		"unknown state":        strings.Replace(string(good), `"state": "confirmed"`, `"state": "trusted"`, 1),
		"bad fingerprint":      strings.Replace(string(good), boxID.Fingerprint, strings.ToUpper(boxID.Fingerprint), 1),
		"one pin two machines": twoPins,
		"a machine id twice": strings.Replace(string(good), `"machines": {`, `"machines": {
    "`+box.ID+`": {"name": "other", "fingerprint": "`+strings.Repeat("ab", 32)+`", "state": "revoked", "reported": {"os": "linux", "ceiling": "full"}, "enrolled_at": "2026-01-01T00:00:00Z"},`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			writeRaw(t, path, []byte(bad))
			defer func() {
				writeRaw(t, path, good)
				if err := m.store.Check(box.ID); err != nil {
					t.Errorf("the store did not recover once the file was readable: %v", err)
				}
			}()

			if err := m.store.Err(); !errors.Is(err, ErrUnreadable) {
				t.Fatalf("Err = %v, want ErrUnreadable", err)
			}
			if _, err := m.store.Machines(); !errors.Is(err, ErrUnreadable) {
				t.Errorf("Machines = %v", err)
			}
			if _, err := m.store.Machine(box.ID); !errors.Is(err, ErrUnreadable) {
				t.Errorf("Machine = %v", err)
			}
			if _, err := m.store.ByName("box"); !errors.Is(err, ErrUnreadable) {
				t.Errorf("ByName = %v", err)
			}
			if err := m.store.Check(box.ID); !errors.Is(err, ErrUnreadable) {
				t.Errorf("Check = %v: a machine was allowed from memory", err)
			}
			if _, err := m.store.Addresses(); !errors.Is(err, ErrUnreadable) {
				t.Errorf("Addresses = %v", err)
			}
			if m.store.EnrollOpen() {
				t.Error("enrollment is open on a fleet that cannot be read")
			}
			if state, serverErr, _ := handshake(t, link, client); state != nil || !errors.Is(serverErr, ErrUnreadable) {
				t.Errorf("handshake: connected %v, err %v", state != nil, serverErr)
			}
			if m.store.states(true) != nil {
				t.Error("the watcher was given states")
			}

			// No change goes through, and the file is left as it was found.
			for op, err := range map[string]error{
				"Revoke":        m.store.Revoke(box.ID),
				"Pause":         m.store.Pause(box.ID),
				"SetAddresses":  m.store.SetAddresses([]string{"10.0.0.9:1"}),
				"SetEnrollOpen": m.store.SetEnrollOpen(false),
			} {
				if !errors.Is(err, ErrUnreadable) {
					t.Errorf("%s = %v, want ErrUnreadable", op, err)
				}
			}
			if _, err := m.store.CreateEnrollment(m.id.Fingerprint, "new", 0, ""); !errors.Is(err, ErrUnreadable) {
				t.Errorf("CreateEnrollment = %v", err)
			}
			if _, err := m.store.enroll("c2VjcmV0", boxID.Fingerprint, linuxBox); !errors.Is(err, ErrUnreadable) {
				t.Errorf("enroll = %v", err)
			}
			if now := must(os.ReadFile(path))(t); string(now) != bad {
				t.Error("a file that could not be read was written over")
			}
			// A second process that starts now is no better off.
			if err := Open(m.dir).Check(box.ID); !errors.Is(err, ErrUnreadable) {
				t.Errorf("a fresh store: Check = %v", err)
			}
		})
	}
}

// A fleet.json over the size limit is unreadable, so the store never writes
// one: a change that would take the file past the limit is refused and the
// fleet stays as it was.
func TestStoreRefusesToOutgrowItsLimit(t *testing.T) {
	path := StorePath(t.TempDir())
	if err := os.WriteFile(path, bytes.Repeat([]byte(" "), maxStoreBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Open(filepath.Dir(path)).Err(); !errors.Is(err, ErrUnreadable) {
		t.Errorf("Err = %v", err)
	}
	if _, err := parseStore(bytes.Repeat([]byte(" "), maxStoreBytes+1)); err == nil {
		t.Error("parseStore read past its limit")
	}

	// A fleet a few records short of the limit.
	m := newMaster(t)
	box, _ := m.add(t, "box", StateConfirmed)
	var d fileData
	if err := json.Unmarshal(must(os.ReadFile(StorePath(m.dir)))(t), &d); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour).Unix()
	record := func(i int) *enrollment {
		return &enrollment{Name: fmt.Sprintf("filler-%07d", i), Secret: fmt.Sprintf("%064x", i), Expires: later}
	}
	size := func() int { return len(must(json.MarshalIndent(&d, "", "  "))(t)) }
	empty := size()
	d.Enrollments = append(d.Enrollments, record(0))
	each := size() - empty
	for i := 1; i < (maxStoreBytes-empty)/each-2; i++ {
		d.Enrollments = append(d.Enrollments, record(i))
	}
	full := must(json.MarshalIndent(&d, "", "  "))(t)
	writeRaw(t, StorePath(m.dir), full)
	if err := m.store.Err(); err != nil {
		t.Fatalf("a fleet under the limit: %v", err)
	}
	var err error
	for i := 0; i < 8 && err == nil; i++ {
		_, err = m.store.CreateEnrollment(m.id.Fingerprint, fmt.Sprint("one-more-", i), 0, "")
	}
	if err == nil || !strings.Contains(err.Error(), "grow past") {
		t.Fatalf("growing past the limit: %v", err)
	}
	if now := must(os.Stat(StorePath(m.dir)))(t); now.Size() > maxStoreBytes {
		t.Errorf("fleet.json is %d bytes, past the limit", now.Size())
	}
	// Still a working fleet, and a revoke, which matters more than a new
	// code, still goes through.
	if err := m.store.Revoke(box.ID); err != nil {
		t.Errorf("a revoke on a full fleet.json: %v", err)
	}
	if err := Open(m.dir).Check(box.ID); !errors.Is(err, ErrRevoked) {
		t.Errorf("Check = %v", err)
	}
}

// A change that changes nothing does not write the file.
func TestNoChangeNoWrite(t *testing.T) {
	m := newMaster(t)
	box, _ := m.add(t, "box", StateConfirmed)
	before := must(os.Stat(StorePath(m.dir)))(t)
	for name, err := range map[string]error{
		"SetAddresses":  m.store.SetAddresses([]string{"127.0.0.1:1"}),
		"SetEnrollOpen": m.store.SetEnrollOpen(true),
		"Rename":        m.store.Rename(box.ID, "box"),
		"SetReported":   m.store.SetReported(box.ID, linuxBox),
		"Resume":        m.store.Resume(box.ID),
	} {
		if err != nil {
			t.Errorf("%s to what already is: %v", name, err)
		}
	}
	if after := must(os.Stat(StorePath(m.dir)))(t); !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Error("fleet.json was written with nothing to change")
	}
}

// A change made by another process (the command line, while the master
// runs) is seen at the next lookup, and Watch reports it.
func TestAnotherProcessesChangeIsSeen(t *testing.T) {
	m := newMaster(t)
	running := m.store
	running.watchEvery = 10 * time.Millisecond
	cli := Open(m.dir)

	a, aID := m.add(t, "a", StateConfirmed)
	b, _ := m.add(t, "b", StateConfirmed)

	type event struct {
		id    string
		state State
	}
	events := make(chan event, 16)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		running.Watch(ctx, func(id string, state State) { events <- event{id, state} })
	}()
	next := func(want event) {
		t.Helper()
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("Watch reported %+v, want %+v", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("Watch never reported %+v", want)
		}
	}
	// It starts by saying where every machine stands.
	first := map[event]bool{}
	for range 2 {
		select {
		case got := <-events:
			first[got] = true
		case <-time.After(10 * time.Second):
			t.Fatal("Watch did not report the machines it started with")
		}
	}
	if !first[event{a.ID, StateConfirmed}] || !first[event{b.ID, StateConfirmed}] {
		t.Fatalf("Watch started with %v", first)
	}

	link := must(MasterLinkTLS(m.id, running))(t)
	client := must(ChildTLS(aID, m.id.Fingerprint))(t)
	if state, _, _ := handshake(t, link, client); state == nil {
		t.Fatal("a confirmed machine could not connect")
	}

	if err := cli.Revoke(a.ID); err != nil {
		t.Fatal(err)
	}
	// With no wait at all: the running master's very next handshake and
	// check see the revoke.
	if err := running.Check(a.ID); !errors.Is(err, ErrRevoked) {
		t.Errorf("Check right after another process revoked = %v", err)
	}
	if state, serverErr, _ := handshake(t, link, client); state != nil || !errors.Is(serverErr, ErrRevoked) {
		t.Errorf("handshake right after another process revoked: connected %v, err %v", state != nil, serverErr)
	}
	next(event{a.ID, StateRevoked})

	if err := cli.Pause(b.ID); err != nil {
		t.Fatal(err)
	}
	next(event{b.ID, StatePaused})
	if err := cli.Resume(b.ID); err != nil {
		t.Fatal(err)
	}
	next(event{b.ID, StateConfirmed})

	// A machine that enrolls shows up as pending; one confirmed, as confirmed.
	c, cID := m.add(t, "c", StatePending)
	next(event{c.ID, StatePending})
	must(cli.Confirm(m.id.Fingerprint, c.ID, ConfirmCode(m.id.Fingerprint, cID.Fingerprint)))(t)
	next(event{c.ID, StateConfirmed})

	// A record that disappears is a revoke to whoever holds its socket; the
	// record of a machine already revoked goes quietly.
	if err := cli.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	raw := must(os.ReadFile(StorePath(m.dir)))(t)
	var d fileData
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	delete(d.Machines, c.ID)
	writeRaw(t, StorePath(m.dir), must(json.Marshal(d))(t))
	next(event{c.ID, StateRevoked})

	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Watch did not stop with its context")
	}
	select {
	case got := <-events:
		t.Errorf("an event nobody caused: %+v", got)
	default:
	}
}

// Something that is not a file where fleet.json should be: unreadable, like
// any other fleet.json that cannot be read.
func TestADirectoryInPlaceOfTheStore(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(StorePath(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	s := Open(dir)
	if err := s.Err(); !errors.Is(err, ErrUnreadable) {
		t.Errorf("Err = %v", err)
	}
	if err := s.SetAddresses([]string{"10.0.0.1:1"}); !errors.Is(err, ErrUnreadable) {
		t.Errorf("SetAddresses = %v", err)
	}
	if fi := must(os.Stat(StorePath(dir)))(t); !fi.IsDir() {
		t.Error("the directory was replaced")
	}
}

// A rewrite that leaves the file's time and size as they were is not seen
// by the stat a lookup makes. Watch does not rely on the stat: it reads the
// file every tick, so such a change is seen within one.
func TestWatchReadSeesAChangeAStatMisses(t *testing.T) {
	m := newMaster(t)
	box, _ := m.add(t, "box", StateConfirmed)
	path := StorePath(m.dir)
	if err := m.store.Check(box.ID); err != nil {
		t.Fatal(err)
	}

	before := must(os.Stat(path))(t)
	raw := must(os.ReadFile(path))(t)
	changed := strings.Replace(string(raw), `"state": "confirmed"`, `"state": "revoked"  `, 1)
	if len(changed) != len(raw) || changed == string(raw) {
		t.Fatal("the rewrite is not the same size")
	}
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if state := m.store.states(true)[box.ID]; state != StateRevoked { // what Watch does each tick
		t.Fatalf("the watcher's read gave %q, want revoked", state)
	}
	if err := m.store.Check(box.ID); !errors.Is(err, ErrRevoked) {
		t.Errorf("Check = %v", err)
	}
}

// The store's own saves never leave the stat blind: a version is dated
// after the one it replaced, whatever the clock's tick.
func TestEverySaveMovesTheFileTimeOn(t *testing.T) {
	m := newMaster(t)
	box, _ := m.add(t, "box", StateConfirmed)
	path := StorePath(m.dir)
	last := must(os.Stat(path))(t).ModTime()
	for i := range 50 {
		// Same size every time, as fast as the disk allows.
		if err := m.store.Rename(box.ID, []string{"aaa", "bbb"}[i%2]); err != nil {
			t.Fatal(err)
		}
		now := must(os.Stat(path))(t).ModTime()
		if !now.After(last) {
			t.Fatalf("save %d left the file's time at %s, not after %s", i, now, last)
		}
		last = now
	}
	// Even when the clock was set back since the last save.
	ahead := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, time.Time{}, ahead); err != nil {
		t.Fatal(err)
	}
	if err := m.store.Rename(box.ID, "ccc"); err != nil {
		t.Fatal(err)
	}
	if now := must(os.Stat(path))(t).ModTime(); !now.After(ahead) {
		t.Errorf("a save with the clock set back dated the file %s, not after %s", now, ahead)
	}
	if got := must(Open(m.dir).Machine(box.ID))(t); got.Name != "ccc" {
		t.Errorf("name = %q", got.Name)
	}
}

// writeRaw replaces a file the way a hand or a fault would, not through the
// store, at a later time than the file had.
func writeRaw(t *testing.T, path string, b []byte) {
	t.Helper()
	prev, statErr := os.Stat(path)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if statErr == nil && !must(os.Stat(path))(t).ModTime().After(prev.ModTime()) {
		if err := os.Chtimes(path, time.Time{}, prev.ModTime().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestParseStoreRejects(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	machine := func(extra string) string {
		return `{"version":1,"machines":{"m0123456789abcdef":{"name":"box","fingerprint":"` + fp +
			`","state":"confirmed","reported":{"os":"linux","ceiling":"full"},"enrolled_at":"2026-01-01T00:00:00Z"` + extra + `}}}`
	}
	if _, err := parseStore([]byte(machine(""))); err != nil {
		t.Fatalf("the valid file: %v", err)
	}
	if _, err := parseStore([]byte(`{"version":1,"machines":null}`)); err != nil {
		t.Errorf("an empty fleet: %v", err)
	}
	enrollment := func(e string) string { return `{"version":1,"machines":{},"enrollments":[` + e + `]}` }
	if _, err := parseStore([]byte(enrollment(`{"name":"a","secret_sha256":"` + fp + `","expires":1}`))); err != nil {
		t.Fatalf("the valid enrollment: %v", err)
	}
	for name, bad := range map[string]string{
		"token field":        machine(`,"token":"abc"`),
		"bad id":             strings.Replace(machine(""), "m0123456789abcdef", "../../etc", 1),
		"null machine":       `{"version":1,"machines":{"m0123456789abcdef":null}}`,
		"bad name":           strings.Replace(machine(""), `"box"`, `"b ox"`, 1),
		"bad os":             strings.Replace(machine(""), `"linux"`, `"beos"`, 1),
		"control in user":    strings.Replace(machine(""), `"os":"linux"`, `"os":"linux","user":"a\u0007"`, 1),
		"no version":         `{"machines":{}}`,
		"bad address":        `{"version":1,"addresses":["nope"],"machines":{}}`,
		"null enrollment":    enrollment(`null`),
		"enrollment name":    enrollment(`{"name":"","secret_sha256":"` + fp + `","expires":1}`),
		"enrollment secret":  enrollment(`{"name":"a","secret_sha256":"plain-secret","expires":1}`),
		"enrollment expiry":  enrollment(`{"name":"a","secret_sha256":"` + fp + `","expires":0}`),
		"enrollment adopt":   enrollment(`{"name":"a","secret_sha256":"` + fp + `","expires":1,"adopt":"x"}`),
		"enrollment used_by": enrollment(`{"name":"a","secret_sha256":"` + fp + `","expires":1,"used_by":"x"}`),
		"same name twice": `{"version":1,"machines":{` +
			`"m0000000000000001":{"name":"box","fingerprint":"` + fp + `","state":"confirmed","reported":{"os":"linux","ceiling":"full"},"enrolled_at":"2026-01-01T00:00:00Z"},` +
			`"m0000000000000002":{"name":"BOX","fingerprint":"` + strings.Repeat("cd", 32) + `","state":"pending","reported":{"os":"linux","ceiling":"full"},"enrolled_at":"2026-01-01T00:00:00Z"}}}`,
	} {
		if _, err := parseStore([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
