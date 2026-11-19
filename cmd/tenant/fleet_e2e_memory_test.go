package main

// fleet_e2e_memory_test.go: a machine's memory with every piece a process of
// its own (TEN-532; docs/plans/gap6-fleet-plan.md section 6 and the "Phases"
// acceptance list). The master is a real `tenant serve`, the machine a real
// `tenant child run`; the master is stopped, killed and restarted, and the
// files are changed between, as an operator or a disk would.
//
// Two things stand in: the operator at init's terminal saves the recovery
// identity and gives it back (fleetmemory_test.go), and, where a test is
// about the memory's life rather than what fills it, a fact is written
// straight into a shadow through a line on the master process's standard
// input (e2eMemory). TestFleetMemoryTwoMachinesEndToEnd fills it the way the
// agent does: a turn of a real master on its scripted model.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/fleet"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
)

// e2eMemory runs one memory command in a master process:
//
//	note <machine> <text>   write a fact into the machine's memory
//	facts <machine>         {"facts": "a | b", "bare": facts without a vector}
//	memory <machine>        {"text": every episode and fact, as text}
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
					if o, _, _ := p.Semantic.Origin(ctx, f.ID); o.Job == originInventory {
						continue // taken at confirmation, when there is an embedder (TEN-532)
					}
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
	case "memory": // everything in the machine's memory, as text
		p, err := fleetShadow(ctx, id)
		if err != nil {
			out["error"] = err.Error()
			break
		}
		var b strings.Builder
		eps, err := p.Episodic.List(ctx, episodic.ListFilter{IncludeTombstoned: true})
		for _, e := range eps {
			fmt.Fprintf(&b, "episode %s | %s | %v\n", e.Prompt, e.Response, e.ToolCalls)
		}
		if err == nil {
			var all []*semantic.Fact
			all, err = p.Semantic.List(ctx, semantic.ListFilter{IncludeTombstoned: true, IncludeSuperseded: true})
			for _, f := range all {
				fmt.Fprintf(&b, "fact %s\n", f.Fact)
			}
		}
		p.Release()
		out["text"] = b.String()
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

// copiesOn counts the copies of its memory a machine keeps.
func copiesOn(c *e2eChild) int {
	entries, _ := os.ReadDir(filepath.Join(c.dir, "memory"))
	return len(entries)
}

// The second half of TEN-532 with every piece a process of its own: a real
// `tenant serve` on the scripted model, which embeds, and two real `tenant
// child run`. A turn reads a file on each machine: neither machine's output
// goes into the other's memory or into the master's own. `tenant privacy
// forget`, in a process of its own, reaches the machine that is online at
// once, and the one that is offline when it is back.
func TestFleetMemoryTwoMachinesEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a master and two machines as processes")
	}
	h := thisHost(t)
	m := h.master(true, "--os")
	m.env = map[string]string{e2eEmbedEnv: "1"}
	m.serve()
	m.init("")
	web, db := m.join(h, "web1"), m.join(h, "db1")
	webMark, dbMark := mark("e2e-web1"), mark("e2e-db1")
	webFile, dbFile := h.join("web1", "label.txt"), h.join("db1", "label.txt")
	for file, text := range map[string]string{webFile: webMark + " is the disk label", dbFile: dbMark + " is the disk label"} {
		if err := os.WriteFile(file, []byte(text+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	memory := func(c *e2eChild) string { return m.proc.memory("memory " + c.id)["text"].(string) }
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		t.Run(name, fn)
		if t.Failed() {
			t.FailNow()
		}
	}

	step("a turn on two machines leaves neither machine's output in the other's memory", func(t *testing.T) {
		turn := m.turn(e2eScript(e2eCall("fleet_read", map[string]any{"host": "web1", "path": webFile}),
			e2eCall("fleet_read", map[string]any{"host": "db1", "path": dbFile})), nil)
		if len(turn.Results) != 2 || !strings.Contains(turn.Results[0], webMark) || !strings.Contains(turn.Results[1], dbMark) {
			t.Fatalf("the turn: %q", turn.Results)
		}
		var w, d string
		eventually(t, "each machine's memory has its part of the turn", func() bool {
			w, d = memory(web), memory(db)
			return strings.Contains(w, webMark) && strings.Contains(d, dbMark)
		})
		if strings.Contains(w, dbMark) || strings.Contains(d, webMark) {
			t.Fatalf("a machine's output is in the other's memory:\nweb1: %s\ndb1: %s", w, d)
		}
		st, err := episodic.OpenExisting(filepath.Join(m.data, "episodes.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		eps, err := st.List(context.Background(), episodic.ListFilter{IncludeTombstoned: true})
		if err != nil {
			t.Fatal(err)
		}
		var mine string
		for _, e := range eps {
			mine += e.Prompt + e.Response + strings.Join(e.Tags, ",") + fmt.Sprint(e.ToolCalls) + "\n"
		}
		if strings.Contains(mine, webMark) || strings.Contains(mine, dbMark) || !strings.Contains(mine, fleetHostTag+web.id) || !strings.Contains(mine, fleetHostTag+db.id) {
			t.Fatalf("the master's own episodes:\n%s", mine)
		}
		t.Logf("files that hold web1's marker after the turn: %v", filesWith(webMark, m.data, m.cfg, web.dir, db.dir))
		if found := filesWith(webMark, fleetShadowDir(m.data), web.dir, db.dir); len(found) > 0 {
			t.Fatalf("the marker is readable in the machines' memory on disk: %v", found)
		}
		m.proc.memory("pass")
		m.memoryState(web.id, memoryInStep, "on both sides")
		m.memoryState(db.id, memoryInStep, "on both sides")
	})

	step("a forget reaches the machine that is online, and its copies", func(t *testing.T) {
		// A second generation on web1, so that a purge has something to drop.
		m.proc.memory("note " + web.id + " a second generation for web1")
		m.proc.memory("pass")
		m.memoryState(web.id, memoryInStep, "generation 2 on both sides")
		before := copiesOn(web)
		if before != 2 {
			t.Fatalf("web1 keeps %d copies before the forget, want 2", before)
		}
		out := m.must(nil, "privacy", "forget", "--match", webMark, "--yes")
		if !strings.Contains(out, "fleet: The same is forgotten from every fleet machine's memory") {
			t.Fatalf("tenant privacy forget:\n%s", out)
		}
		eventually(t, "the forget reaches web1's memory", func() bool { return !strings.Contains(memory(web), webMark) })
		if !strings.Contains(memory(db), dbMark) {
			t.Fatal("db1's memory lost what was not forgotten")
		}
		eventually(t, "web1 keeps only the copy made after the forget", func() bool { return copiesOn(web) == 1 })
		t.Logf("web1 kept %d copies before the forget, 1 after", before)
		if found := filesWith(webMark, fleetShadowDir(m.data), web.dir); len(found) > 0 {
			t.Fatalf("after the forget the marker is in %v", found)
		}
		t.Logf("files that hold web1's marker after the forget: %v", filesWith(webMark, m.data, m.cfg, web.dir))
	})

	step("a forget reaches a machine that was offline when it is back", func(t *testing.T) {
		m.proc.memory("note " + db.id + " a second generation for db1")
		m.proc.memory("pass")
		m.memoryState(db.id, memoryInStep, "generation 2 on both sides")
		db.proc.stop()
		m.memoryState(db.id, memoryInStep, "generation 2 on both sides")
		held := copiesOn(db)
		m.must(nil, "privacy", "forget", "--match", dbMark, "--yes")
		eventually(t, "the forget reaches db1's memory on the master", func() bool { return !strings.Contains(memory(db), dbMark) })
		if copiesOn(db) != held {
			t.Fatal("the copies of a machine that is offline changed")
		}
		db.run()
		db.state("connected to the master")
		eventually(t, "db1 drops its copies from before the forget once it is back", func() bool { return copiesOn(db) == 1 })
		m.memoryState(db.id, memoryInStep, "generation 3 on both sides")
	})
}
