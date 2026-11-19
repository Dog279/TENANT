package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/fleet"
	"tenant/internal/fleet/shadow"
)

// The rest of "who wins" (fleetmemory_test.go has the harness), revoke,
// doctor and backup.

// A master with another fleet key: the machine is marked "needs recovery
// key" and nothing is overwritten or deleted on either side.
func TestFleetMemoryAMasterWithTheWrongKeyOverwritesNothing(t *testing.T) {
	for name, rebuilt := range map[string]bool{"its key file replaced": false, "rebuilt from nothing": true} {
		t.Run(name, func(t *testing.T) {
			l := newMemLab(t)
			dir := fleetShadowDir(l.data)
			l.note("web1 runs nginx")
			l.mint()
			l.note("web1's /var fills up")
			l.mint()
			l.compare(memoryInStep, "generation 2 on both sides")
			onChild := l.childFiles()
			var onMaster map[string][]byte
			l.reopen(func() {
				if err := os.Remove(fleet.KeyPath(l.cfg)); err != nil {
					t.Fatal(err)
				}
				if err := ensureFleetKey(l.cfg); err != nil {
					t.Fatal(err)
				}
				if rebuilt {
					if err := os.RemoveAll(dir); err != nil {
						t.Fatal(err)
					}
				}
				onMaster = treeBytes(t, dir)
			})
			l.await(memoryNeedsKey, "another fleet key")
			if rebuilt {
				// The new master writes and makes a generation of its own:
				// still nothing goes to the machine.
				l.note("noted by the new master")
				l.mint()
				onMaster = nil
			}
			l.compare(memoryNeedsKey, "another fleet key")
			if got := l.childFiles(); !sameFiles(got, onChild) {
				t.Fatalf("the machine's copies changed: %d files, were %d", len(got), len(onChild))
			}
			if onMaster != nil && !sameFiles(treeBytes(t, dir), onMaster) {
				t.Fatal("the master's own snapshots under the old key changed")
			}
			if told := strings.Join(l.told(), "\n"); !strings.Contains(told, "needs the recovery key") {
				t.Fatalf("the operator was not told:\n%s", told)
			}
		})
	}
}

// treeBytes is every snapshot file under dir, by path: what the shadow store
// keeps, without the states record and the lock.
func treeBytes(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".snap") {
			out[path] = readRaw(t, path)
		}
		return nil
	})
	return out
}

func sameFiles(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !bytes.Equal(w, v) {
			return false
		}
	}
	return true
}

// The machine's newest copy is damaged: its next older one is used. A copy
// planted with a right-looking name and first line is passed over by the
// master. None verifies: the master pushes, its generation numbered past
// the planted one.
func TestFleetMemoryTheChildsNewestIsDamaged(t *testing.T) {
	l := newMemLab(t)
	dir := fleetShadowDir(l.data)
	l.note("one")
	c1 := l.mint()
	l.compare(memoryInStep, "generation 1")
	l.note("two")
	c2 := l.mint()
	l.compare(memoryInStep, "generation 2")
	damage := func(c shadow.Copy) {
		p := filepath.Join(l.ch.dir, "memory", childCopyName(t, l.copies, c.Gen))
		b := readRaw(t, p)
		b[len(b)-3] ^= 1
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The master lost this machine's memory altogether, and the machine's
	// newest copy rotted.
	l.reopen(func() {
		if err := os.RemoveAll(filepath.Join(dir, l.ch.id)); err != nil {
			t.Fatal(err)
		}
		damage(c2)
	})
	l.await(memoryInStep, "generation 1 on both sides")
	if got, bare := l.facts(); got != "one" || bare != 0 {
		t.Fatalf("the next older copy was not the one used: %q (%d without a vector)", got, bare)
	}
	// A forged newest copy: the child cannot tell it from a real one.
	head := readRaw(t, filepath.Join(l.ch.dir, "memory", childCopyName(t, l.copies, c1.Gen)))
	forged := append(bytes.Clone(head[:bytes.IndexByte(head, '\n')-1]), "9\nnot something this master encrypted"...)
	if err := l.copies.Put(shadow.Copy{KeyID: c1.KeyID, Gen: 9, Hash: shadow.Hash(forged)}, forged); err != nil {
		t.Fatal(err)
	}
	// And none of the real ones verifies.
	damage(c1)
	l.note("three")
	l.mint()
	l.compare(memoryInStep, "generation 10 on both sides")
	if got, _ := l.facts(); got != "one | three" {
		t.Fatalf("the master's shadow: %q", got)
	}
	if got := l.gens(); !strings.HasPrefix(got, "10 9 ") {
		t.Fatalf("the machine keeps %q", got)
	}
}

// A shadow a call holds is not waited for: the comparison says so at once,
// and the next one takes the machine's copy.
func TestFleetMemoryNeverWaitsForAShadowInUse(t *testing.T) {
	l := newMemLab(t)
	l.note("one")
	l.mint()
	l.compare(memoryInStep, "generation 1")
	backup := t.TempDir()
	dir := fleetShadowDir(l.data)
	l.reopen(func() { copyTree(t, dir, backup) })
	l.note("two")
	l.mint()
	l.compare(memoryInStep, "generation 2")
	l.disconnect()
	l.reopen(func() { restoreTree(t, backup, dir) })
	held, err := l.mem().shadows.Acquire(context.Background(), l.ch.id)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	l.connect()
	l.await(memoryBehind, "in use")
	if waited := time.Since(start); waited > 10*time.Second {
		t.Fatalf("the comparison waited %s", waited)
	}
	held.Release()
	l.compare(memoryInStep, "generation 2 on both sides")
	if got, _ := l.facts(); got != "one | two" {
		t.Fatalf("after the wait the shadow holds %q", got)
	}
}

// Revoking a machine with delete deletes its memory: at once in the process
// that holds the fleet, and asked from another process, at the holder's
// next look. Keep keeps it.
func TestFleetRevokeDeletesTheMachinesMemory(t *testing.T) {
	l := newMemLab(t)
	l.note("one")
	l.mint()
	l.compare(memoryInStep, "generation 1")
	dir := filepath.Join(fleetShadowDir(l.data), l.ch.id)
	l.disconnect()
	if out := l.must("revoke", "web1", "--keep-scheme"); !strings.Contains(out, "kept") {
		t.Fatalf("revoke keep:\n%s", out)
	}
	if !hasFiles(dir) {
		t.Fatal("revoke keep deleted the machine's memory")
	}
	// Another process asks; this one holds the memory and deletes it.
	h := theFleet.Swap(nil)
	said, err := removeFleetMemory(l.cfg, l.data, l.ch.id)
	theFleet.Store(h)
	if err != nil || !strings.Contains(said, "holds the fleet") {
		t.Fatalf("removal from another process: %q %v", said, err)
	}
	eventually(t, "the holder deletes the machine's memory", func() bool {
		_, err := os.Lstat(dir)
		return errors.Is(err, fs.ErrNotExist)
	})
}

func TestFleetRevokeDeleteInTheHoldersProcess(t *testing.T) {
	l := newMemLab(t)
	l.note("one")
	l.mint()
	l.compare(memoryInStep, "generation 1")
	l.disconnect()
	out := l.must("revoke", "web1", "--delete-scheme")
	if _, err := os.Lstat(filepath.Join(fleetShadowDir(l.data), l.ch.id)); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(out, "its memory is deleted") {
		t.Fatalf("revoke delete left the memory (%v):\n%s", err, out)
	}
}

// Doctor reports the fleet key's locked memory and whether the recovery key
// was proven, and changes nothing.
func TestDoctorChecksTheFleetKeys(t *testing.T) {
	l := newFleetLab(t)
	env := &doctorEnv{c: l.c}
	if r := checkFleetKeys(context.Background(), env); r.Status != statusSkip {
		t.Fatalf("not a master: %+v", r)
	}
	withOperator(t, func() (*recoveryTerminal, error) { return nil, errors.New("no terminal") })
	_ = notes(t)
	_, _ = l.run("init", "--listen", "127.0.0.1:0")
	r := checkFleetKeys(context.Background(), env)
	if r.Status != statusWarn || !strings.Contains(r.Detail, "no recovery key") || !strings.Contains(r.Detail, "locked memory") {
		t.Fatalf("no recovery key: %+v", r)
	}
	withOperator(t, operator(nil).open)
	l.must("init")
	key := readRaw(t, fleet.KeyPath(l.cfg))
	if r := checkFleetKeys(context.Background(), env); !strings.Contains(r.Detail, "recovery key was proven") {
		t.Fatalf("a proven recovery key: %+v", r)
	}
	if !bytes.Equal(key, readRaw(t, fleet.KeyPath(l.cfg))) {
		t.Fatal("doctor changed the key file")
	}
	if err := os.Remove(fleet.KeyPath(l.cfg)); err != nil {
		t.Fatal(err)
	}
	if r := checkFleetKeys(context.Background(), env); r.Status != statusWarn || !strings.Contains(r.Detail, "no fleet key") {
		t.Fatalf("no key: %+v", r)
	}
}

// A fleet master's plain backup warns; the fleet key is a managed secret,
// which the log scrubber never reads into a string.
func TestFleetBackupWarningAndTheKeyIsNeverText(t *testing.T) {
	l := newFleetLab(t)
	l.must("init", "--listen", "127.0.0.1:0")
	var out bytes.Buffer
	runBackup(t, func() error {
		return cmdBackupCreate(context.Background(), []string{"--config", l.cfg, "--data", l.data, "--out", t.TempDir()}, &out)
	})
	if !strings.Contains(out.String(), "fleet master") || !strings.Contains(out.String(), "--encrypt") {
		t.Fatalf("a plain backup of a fleet master:\n%s", out.String())
	}
	managed, _ := managedSecrets(l.cfg, l.data)
	if !slices.ContainsFunc(managed, func(m managedSecret) bool { return m.path == fleet.KeyPath(l.cfg) }) {
		t.Fatal("the fleet key is not a managed secret")
	}
	for name := range logSecretValues(l.cfg, l.data, nil, nil) {
		if strings.HasPrefix(name, "fleet-key") {
			t.Fatalf("the log scrubber read the fleet key: %s", name)
		}
	}
}

// A machine that holds a copy it calls a generation far past the master's,
// and that is not the master's, cannot make the master use up its
// generation numbers: the master does not number past it, says why, and
// changes nothing.
func TestFleetMemoryAMachineCannotUseUpTheNumbers(t *testing.T) {
	l := newMemLab(t)
	l.note("one")
	c1 := l.mint()
	l.compare(memoryInStep, "generation 1")
	head := readRaw(t, filepath.Join(l.ch.dir, "memory", childCopyName(t, l.copies, c1.Gen)))
	const huge = 1<<62 + 7
	forged := append(bytes.Clone(head[:bytes.IndexByte(head, '\n')-1]), "4611686018427387911\nnot the master's"...)
	if err := l.copies.Put(shadow.Copy{KeyID: c1.KeyID, Gen: huge, Hash: shadow.Hash(forged)}, forged); err != nil {
		t.Fatal(err)
	}
	l.note("two")
	l.mint()
	l.compare(memoryBehind, "far past the master's")
	if c, _, err := l.mem().shadows.Newest(l.ch.id); err != nil || c.Gen != 2 {
		t.Fatalf("the master's newest generation is %+v, %v; want 2", c, err)
	}
	if got, _ := l.facts(); got != "one | two" {
		t.Fatalf("the master's shadow: %q", got)
	}
}

// A machine whose child does not know the memory methods (an older Tenant)
// keeps its link and its calls; its memory is "not known", saying why, and
// nothing is sent to it. Offline, a machine behind the master says so.
func TestFleetMemoryAnOlderChild(t *testing.T) {
	l := newMemLab(t)
	l.note("one")
	l.mint()
	l.compare(memoryInStep, "generation 1")
	l.disconnect()
	l.note("two")
	l.mint()
	l.mem().kick(l.ch.id) // the machine is offline
	l.await(memoryBehind, "offline")
	l.copies = nil // the same machine, now running a child that keeps no copies
	l.connect()
	l.await(memoryUnknown, "keeps no copy of its memory")
	if state, detail := fleetMemoryState(l.ch.id); state != memoryUnknown || !strings.Contains(detail, "update it") {
		t.Fatalf("what the dashboard is told: %q %q", state, detail)
	}
	if !online(fleetManager(), l.ch.id) {
		t.Fatal("the link to the older child ended")
	}
	if res, err := fleetManager().Call(context.Background(), l.ch.id, fleet.Call{Tool: "os_exec",
		Confirm: func(context.Context, string, string) bool { return true }}); err != nil || res.Text != "Linux" {
		t.Fatalf("a call to the older child: %+v %v", res, err)
	}
}
