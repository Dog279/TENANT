package main

// fleet_e2e_memory_test.go: a machine's memory with every piece a process of
// its own (TEN-532; docs/plans/gap6-fleet-plan.md section 6 and the "Phases"
// acceptance list). The master is a real `tenant serve`, the machine a real
// `tenant child run`; the master is stopped, killed and restarted, and the
// files are changed between, as an operator or a disk would.
//
// Two things stand in: the operator at init's terminal saves the recovery
// identity and gives it back (fleetmemory_test.go), and what goes into a
// machine's memory is written straight into its shadow through a line on the
// master process's standard input (e2eMemory), since the agent's way in
// (fleet_note, attribution) is the second half of TEN-532.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/fleet"
	"tenant/internal/memory/semantic"
)

// e2eMemory runs one memory command in a master process:
//
//	note <machine> <text>   write a fact into the machine's memory
//	facts <machine>         {"facts": "a | b", "bare": facts without a vector}
//	pass                    the five-minute pass, now
//	state <machine>         {"state", "detail"}
func e2eMemory(cmd string) string {
	verb, rest, _ := strings.Cut(cmd, " ")
	id, text, _ := strings.Cut(rest, " ")
	ctx := context.Background()
	out := map[string]any{}
	switch verb {
	case "note", "facts":
		p, err := fleetShadow(ctx, id)
		if err != nil {
			out["error"] = err.Error()
			break
		}
		if verb == "note" {
			long := time.Unix(1_600_000_000, 0)
			_, err = p.Semantic.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: text, FirstSeen: long, LastConfirmed: long,
				EmbedderID: "e2e", Embedding: []float32{1, 2, 3}})
		} else {
			var all []*semantic.Fact
			if all, err = p.Semantic.List(ctx, semantic.ListFilter{}); err == nil {
				var facts []string
				bare := 0
				for _, f := range all {
					facts = append(facts, f.Fact)
					if len(f.Embedding) == 0 {
						bare++
					}
				}
				slices.Sort(facts)
				out["facts"], out["bare"] = strings.Join(facts, " | "), bare
			}
		}
		p.Release()
		if err != nil {
			out["error"] = err.Error()
		}
	case "pass":
		h := theFleet.Load()
		h.mu.Lock()
		mem := h.mem
		h.mu.Unlock()
		if mem == nil {
			out["error"] = "no memory open"
			break
		}
		mem.pass()
	case "state":
		out["state"], out["detail"] = fleetMemoryState(id)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// memory sends a memory command to a master process and returns its answer.
func (p *e2eProc) memory(cmd string) map[string]any {
	p.t.Helper()
	from := p.out.count()
	if _, err := io.WriteString(p.in, "memory "+cmd+"\n"); err != nil {
		p.t.Fatalf("%s: %v", p.what, err)
	}
	_, line := p.await(from, "E2E-MEMORY ", "the master answers "+cmd)
	var out map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "E2E-MEMORY ")), &out); err != nil {
		p.t.Fatalf("%q: %v", line, err)
	}
	if e, ok := out["error"]; ok {
		p.t.Fatalf("memory %s: %v\n%s", cmd, e, p.said())
	}
	return out
}

// memoryState waits until the master says the machine's memory is want,
// with a detail that holds detail.
func (m *e2eMaster) memoryState(id, want, detail string) {
	m.t.Helper()
	var got map[string]any
	for deadline := time.Now().Add(e2eWait); ; time.Sleep(100 * time.Millisecond) {
		got = m.proc.memory("state " + id)
		if got["state"] == want && strings.Contains(got["detail"].(string), detail) {
			return
		}
		if time.Now().After(deadline) {
			m.t.Fatalf("the machine's memory is %v, want %q with %q\n%s", got, want, detail, m.proc.said())
		}
	}
}

func (m *e2eMaster) facts(id string) (string, int) {
	m.t.Helper()
	got := m.proc.memory("facts " + id)
	return got["facts"].(string), int(got["bare"].(float64))
}

// filesWith lists every file under the dirs whose bytes hold text.
func filesWith(text string, dirs ...string) []string {
	var found []string
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte(text)) {
				found = append(found, path)
			}
			return nil
		})
	}
	return found
}

func dirBytes(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		out[e.Name()] = readRaw(t, filepath.Join(dir, e.Name()))
	}
	return out
}

// The acceptance list of the memory pull request for this half (TEN-532),
// with a real master and a real machine.
func TestFleetMemoryEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a master and a machine as processes")
	}
	h := thisHost(t)
	m := h.master(false)
	m.serve()
	// A fixed loopback port, so that the machine finds its master again after
	// each restart.
	from := m.proc.out.count()
	m.must(nil, "fleet", "init", "--listen", freeLoopbackAddr(t), "--wait", "60s")
	m.master = true
	m.listening(from)
	m.must(nil, "fleet", "init", "--address", m.fleet)
	c := m.join(h, "web1")
	copies := filepath.Join(c.dir, "memory")
	var marker string
	{
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		marker = "e2e-marker-" + hex.EncodeToString(b)
	}
	readable := func(when string) {
		t.Helper()
		if found := filesWith(marker, c.dir, childDataDir(c.dir), m.data, m.cfg); len(found) > 0 {
			t.Fatalf("%s: the marker is readable in %v", when, found)
		}
	}
	restart := func(between func()) {
		t.Helper()
		m.proc.stop()
		if between != nil {
			between()
		}
		m.serve()
		c.state("connected to the master")
	}
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		t.Run(name, fn)
		if t.Failed() {
			t.FailNow()
		}
	}

	step("a machine's memory goes to it, and nothing on either side is readable", func(t *testing.T) {
		m.memoryState(c.id, memoryInStep, "no memory of this machine is kept yet")
		m.proc.memory("note " + c.id + " " + marker + " web1 runs nginx")
		readable("after a write")
		m.proc.memory("pass")
		m.memoryState(c.id, memoryInStep, "generation 1 on both sides")
		readable("after a snapshot")
		if entries, _ := os.ReadDir(copies); len(entries) != 1 {
			t.Fatalf("the machine keeps %d copies", len(entries))
		}
	})

	step("delete the machine's copies and they return", func(t *testing.T) {
		if err := os.RemoveAll(copies); err != nil {
			t.Fatal(err)
		}
		c.proc.stop()
		c.run()
		c.state("connected to the master")
		m.memoryState(c.id, memoryInStep, "generation 1 on both sides")
		eventually(t, "the copy is back on the machine", func() bool {
			entries, _ := os.ReadDir(copies)
			return len(entries) == 1
		})
	})

	step("a crash loses at most the writes since the last snapshot", func(t *testing.T) {
		snap := filepath.Join(fleetShadowDir(m.data), c.id, "shadow.snap")
		before, _ := os.Stat(snap)
		m.proc.memory("note " + c.id + " " + marker + " written before the snapshot")
		eventually(t, "the snapshot is written a few seconds after the write", func() bool {
			now, err := os.Stat(snap)
			return err == nil && now.ModTime().After(before.ModTime())
		})
		m.proc.memory("note " + c.id + " " + marker + " written after the last snapshot")
		m.proc.kill() // between a write and its snapshot
		readable("after the master was killed between a write and its snapshot")
		m.serve()
		c.state("connected to the master")
		facts, _ := m.facts(c.id)
		if !strings.Contains(facts, "written before the snapshot") {
			t.Fatalf("a write the snapshot had was lost in the crash: %q", facts)
		}
		t.Logf("the write after the last snapshot was %s", map[bool]string{true: "kept", false: "lost"}[strings.Contains(facts, "written after the last snapshot")])
		m.proc.memory("pass")
		m.memoryState(c.id, memoryInStep, "on both sides")
	})

	step("delete the master's snapshot and the shadow is rebuilt from the machine's copy, and re-embedded", func(t *testing.T) {
		want, _ := m.facts(c.id)
		restart(func() {
			if err := os.Remove(filepath.Join(fleetShadowDir(m.data), c.id, "shadow.snap")); err != nil {
				t.Fatal(err)
			}
		})
		m.memoryState(c.id, memoryInStep, "on both sides")
		got, bare := m.facts(c.id)
		if got != want {
			t.Fatalf("the rebuilt shadow holds %q, want %q", got, want)
		}
		if bare != 0 {
			t.Errorf("after the rebuild %d facts have no vector: not re-embedded", bare)
		}
		readable("after the rebuild")
	})

	step("restore the master from an older backup and the machine's newer memory comes back", func(t *testing.T) {
		backup := filepath.Join(h.root, "fleet-backup")
		restart(func() { copyTree(t, fleetShadowDir(m.data), backup) })
		m.proc.memory("note " + c.id + " " + marker + " written after the backup")
		m.proc.memory("pass")
		m.memoryState(c.id, memoryInStep, "on both sides")
		restart(func() { restoreTree(t, backup, fleetShadowDir(m.data)) })
		m.memoryState(c.id, memoryInStep, "on both sides")
		if got, _ := m.facts(c.id); !strings.Contains(got, "written after the backup") {
			t.Fatalf("the machine's newer memory did not come back: %q", got)
		}
		readable("after the restore")
	})

	step("a master with the wrong key marks the machine and overwrites nothing", func(t *testing.T) {
		onChild := dirBytes(t, copies)
		var onMaster map[string][]byte
		restart(func() {
			if err := os.Remove(fleet.KeyPath(m.cfg)); err != nil {
				t.Fatal(err)
			}
			if err := ensureFleetKey(m.cfg); err != nil {
				t.Fatal(err)
			}
			onMaster = treeBytes(t, fleetShadowDir(m.data))
		})
		m.memoryState(c.id, memoryNeedsKey, "another fleet key")
		// And again on the next reconnect, with time for anything to be written.
		c.proc.stop()
		c.run()
		c.state("connected to the master")
		m.memoryState(c.id, memoryNeedsKey, "another fleet key")
		m.proc.memory("pass")
		time.Sleep(time.Second)
		if !sameFiles(dirBytes(t, copies), onChild) {
			t.Fatal("the machine's copies changed")
		}
		if !sameFiles(treeBytes(t, fleetShadowDir(m.data)), onMaster) {
			t.Fatal("the master's snapshots under the old key changed")
		}
		out := m.must(nil, "fleet", "show", "web1")
		if !strings.Contains(out, "memory      needs recovery key") {
			t.Fatalf("tenant fleet show does not say so:\n%s", out)
		}
		readable("at the end")
	})
}
