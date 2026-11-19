package main

// A forget on the fleet's machines' memory, its record, and distillation
// machine by machine (fleetforget.go, TEN-532), over a real link to a child
// in this process. fleet_e2e_memory_test.go does the forget with every piece
// a process of its own.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"tenant/internal/fleet"
	"tenant/internal/fleet/shadow"
	"tenant/internal/memory/distill"
	"tenant/internal/memory/episodic"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
	"tenant/internal/privacy"
)

// remember writes a turn's extract holding text into the lab machine's
// memory, as a turn would, and a fact drawn from it.
func (l *memLab) remember(text string) {
	l.t.Helper()
	ep := episodic.Episode{AgentID: "main", SessionID: "s1", SourceRunID: "run-" + text, Prompt: "look at the disks",
		Response: "fleet_read {}\n" + text + " is in the output", Tags: []string{fleetExtractTag}, EmbedderID: "e", Embedding: []float32{1, 0.5, 0.25}}
	if err := fleetWriteEpisode(context.Background(), l.ch.id, &ep); err != nil {
		l.t.Fatal(err)
	}
	l.note(text + " is a fact")
}

func (l *memLab) holds(text string) bool {
	l.t.Helper()
	return strings.Contains(memoryText(l.t, l.ch.id), text)
}

// forgetOf is where the forgets stand on the lab machine.
func (l *memLab) forgetOf() []forgetOn {
	m := l.mem()
	m.fgMu.Lock()
	defer m.fgMu.Unlock()
	var out []forgetOn
	for _, f := range m.forgets {
		if on := f.Machines[l.ch.id]; on != nil {
			out = append(out, *on)
		}
	}
	return out
}

func (l *memLab) forget(sel privacy.Selector) string {
	l.t.Helper()
	said, err := recordFleetForget(l.cfg, l.data, sel)
	if err != nil {
		l.t.Fatal(err)
	}
	return said
}

// A forget deletes from a machine's shadow what it selects and nothing else,
// along with the shadows kept aside; the machine gets a new copy and drops
// its older ones. Nothing readable of it is left on the master's disk.
func TestAForgetReachesTheMachineAndItsCopies(t *testing.T) {
	l := newMemLab(t)
	gone, kept := mark("forget-me"), mark("keep-me")
	l.remember(gone)
	l.remember(kept)
	l.mint()
	l.compare(memoryInStep, "generation 1 on both sides")
	aside := filepath.Join(fleetShadowDir(l.data), l.ch.id, "aside")
	if err := os.MkdirAll(aside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aside, "1-shadow.snap"), []byte("an older shadow"), 0o600); err != nil {
		t.Fatal(err)
	}

	said := l.forget(privacy.Selector{Match: gone})
	if !strings.Contains(said, "every fleet machine's memory") || !strings.Contains(said, "earlier fleet key cannot be deleted from here") {
		t.Errorf("the operator is told: %q", said)
	}
	assertNowhere(t, gone, l.data, l.cfg) // the request names what is forgotten: it is sealed
	l.mem().sweep()
	if l.holds(gone) || !l.holds(kept) {
		t.Fatalf("after the forget the machine's memory:\n%s", memoryText(t, l.ch.id))
	}
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Errorf("the shadows kept aside are still there: %v", err)
	}
	l.compare(memoryInStep, "generation 2 on both sides")
	if got := l.gens(); got != "2" {
		t.Fatalf("after the purge the machine keeps %q", got)
	}
	if on := l.forgetOf(); len(on) != 1 || on[0] != (forgetOn{Applied: true, Gen: 2, Purged: true}) {
		t.Fatalf("the record of the forget: %+v", on)
	}
	entries, _ := os.ReadDir(fleetShadowDir(l.data))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), fleetForgetPrefix) {
			t.Errorf("the request %s was left", e.Name())
		}
	}
	assertNowhere(t, gone, l.data, l.cfg, l.ch.dir)
}

// A machine that is offline at the forget keeps its older copies until it is
// back, and is told then.
func TestAForgetReachesAMachineThatWasOffline(t *testing.T) {
	l := newMemLab(t)
	gone := mark("forget-me")
	l.remember(gone)
	l.mint()
	l.compare(memoryInStep, "generation 1 on both sides")
	l.disconnect()
	l.forget(privacy.Selector{Match: gone})
	l.mem().sweep()
	if on := l.forgetOf(); len(on) != 1 || on[0] != (forgetOn{Applied: true, Gen: 2}) {
		t.Fatalf("the record of a forget the machine has not had: %+v", on)
	}
	if l.holds(gone) || l.gens() != "1" {
		t.Fatalf("the shadow holds it %v, the machine keeps %q", l.holds(gone), l.gens())
	}
	// The master restarts meanwhile: the record is read back.
	l.reopen(nil)
	if on := l.forgetOf(); len(on) != 1 || on[0].Purged {
		t.Fatalf("after a restart the record is %+v", on)
	}
	l.connect()
	l.await(memoryInStep, "generation 2 on both sides")
	eventually(t, "the machine drops its copy from before the forget", func() bool { return l.gens() == "2" })
	if on := l.forgetOf(); on[0] != (forgetOn{Applied: true, Gen: 2, Purged: true}) {
		t.Fatalf("the record after the machine was told: %+v", on)
	}
}

// A forget survives a rebuild: a shadow replaced by a machine's copy from
// before the forget has it applied again, and the machine drops that copy.
func TestAForgetSurvivesARebuild(t *testing.T) {
	l := newMemLab(t)
	gone, kept := mark("forget-me"), mark("keep-me")
	l.remember(gone)
	l.remember(kept)
	l.mint()
	l.compare(memoryInStep, "generation 1 on both sides")
	l.disconnect() // the machine keeps generation 1, which holds it
	l.forget(privacy.Selector{Match: gone})
	l.mem().sweep()
	if l.holds(gone) {
		t.Fatal("the forget was not applied")
	}
	// The master loses its own memory of the machine (its snapshot and its
	// generation file), and is started again.
	dir := filepath.Join(fleetShadowDir(l.data), l.ch.id)
	l.reopen(func() {
		for _, f := range []string{"shadow.snap", "child.snap"} {
			if err := os.Remove(filepath.Join(dir, f)); err != nil {
				t.Fatal(err)
			}
		}
	})
	if l.forgetOf()[0] != (forgetOn{Applied: true, Gen: 2}) {
		t.Fatalf("the record before the rebuild: %+v", l.forgetOf())
	}
	l.connect() // the machine's generation 1 wins, and is taken as the shadow
	l.await(memoryInStep, "generation 2 on both sides")
	// Generation 1 held it: what the shadow holds now is that copy with the
	// forget applied again.
	if l.holds(gone) || !l.holds(kept) {
		t.Fatalf("after the rebuild from a copy older than the forget:\n%s", memoryText(t, l.ch.id))
	}
	eventually(t, "the machine drops the copy that holds it", func() bool { return l.gens() == "2" })
	if on := l.forgetOf(); on[0] != (forgetOn{Applied: true, Gen: 2, Purged: true}) {
		t.Fatalf("the record after the rebuild: %+v", on)
	}
	assertNowhere(t, gone, l.data, l.ch.dir)
}

// Copies a machine keeps under another fleet key are not its master's to
// delete: they stay, and the operator is told.
func TestAForgetCannotPurgeCopiesUnderAnotherKey(t *testing.T) {
	l := newMemLab(t)
	gone := mark("forget-me")
	l.remember(gone)
	l.mint()
	l.compare(memoryInStep, "generation 1 on both sides")
	l.note("two")
	l.mint()
	l.compare(memoryInStep, "generation 2 on both sides")
	// A copy made by another master's key, as one the machine kept from
	// before the fleet key was replaced.
	ctx := context.Background()
	other, err := shadow.CreateKey(filepath.Join(t.TempDir(), "other.key"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	identity, recipient, err := shadow.NewRecovery()
	if err != nil {
		t.Fatal(err)
	}
	_ = identity.Release()
	elsewhere, err := shadow.OpenStore(t.TempDir(), other, recipient, shadow.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := elsewhere.Acquire(ctx, l.ch.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Episodic.Insert(ctx, &episodic.Episode{AgentID: "main", Prompt: "p", Response: "r", EmbedderID: "e", Embedding: []float32{1}}); err != nil {
		t.Fatal(err)
	}
	p.Release()
	if _, _, err := elsewhere.Mint(ctx, l.ch.id); err != nil {
		t.Fatal(err)
	}
	c, blob, err := elsewhere.Newest(l.ch.id)
	if err != nil {
		t.Fatal(err)
	}
	_ = elsewhere.Close()
	if err := l.copies.Put(c, blob); err != nil {
		t.Fatal(err)
	}

	l.forget(privacy.Selector{Match: gone})
	l.mem().sweep()
	l.compare(memoryInStep, "generation 3 on both sides")
	if got := l.gens(); got != "3 1" {
		t.Fatalf("the machine keeps %q, want its newest and the copy under the other key", got)
	}
	if on := l.forgetOf(); len(on) != 1 || !on[0].Foreign || !on[0].Purged {
		t.Fatalf("the record: %+v", on)
	}
	if told := strings.Join(l.told(), "\n"); !strings.Contains(told, "keeps copies of its memory made under another fleet key") {
		t.Errorf("the operator was not told:\n%s", told)
	}
}

// The record of forgets is read by one parser, which refuses what is not
// one, whatever is in the file.
func FuzzFleetForgets(f *testing.F) {
	good, _ := json.Marshal([]*fleetForget{{At: time.Unix(1, 0).UTC(), Match: "x", Machines: map[string]*forgetOn{"m0123456789abcdef": {Applied: true, Gen: 3}}}})
	f.Add(good)
	f.Add([]byte(`[{"at":"2026-11-19T00:00:00Z","sessions":["s"],"runs":["r"],"before":"2026-01-01T00:00:00Z","agent":"main"}]`))
	f.Add([]byte(`[{"match":"x","machines":{"../../etc":{"applied":true}}}]`))
	f.Add([]byte(`[{"match":"x","machines":{"m0123456789abcdef":null}}]`))
	f.Add([]byte(`[null]`))
	f.Add([]byte(`[{}]`))
	f.Add([]byte(`{"forgets":[]}`))
	f.Add([]byte(`[{"match":"` + strings.Repeat("x", fleetForgetTextMax+1) + `"}]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		all, err := parseFleetForgets(data)
		if err != nil {
			return
		}
		for _, fg := range all {
			if fg == nil || fg.selector().Empty() || len(fg.Match) > fleetForgetTextMax || len(fg.Sessions) > fleetForgetListMax || len(fg.Runs) > fleetForgetListMax {
				t.Fatalf("accepted %+v", fg)
			}
			for id, on := range fg.Machines {
				if !fleetMachineID.MatchString(id) || on == nil || strings.ContainsAny(id, `/\.`) {
					t.Fatalf("accepted machine %q %v", id, on)
				}
			}
		}
	})
}

func storedMachine(t *testing.T, id string) fleet.Machine {
	t.Helper()
	m, err := fleetStore().Machine(id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// --- distillation, machine by machine ---

// Facts about a machine come from distilling that machine's episodes only,
// into its own memory, as drawn from its output; a second run starts where
// the first ended.
func TestFleetDistillationKeepsToEachMachine(t *testing.T) {
	l := newRecallLab(t)
	l.agent("work").turn(t,
		fleetCall("c1", "fleet_read", map[string]any{"host": "web1", "path": "/a"}),
		fleetCall("c2", "fleet_read", map[string]any{"host": "db1", "path": "/b"}))

	summarizer := testllm.New()
	var prompts []string
	summarizer.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
		user := req.Messages[len(req.Messages)-1].Content
		prompts = append(prompts, user)
		fact := "distilled from nothing"
		for _, m := range []string{l.webOut, l.dbOut} {
			if strings.Contains(user, m) {
				fact = "distilled from " + m
			}
		}
		raw, _ := json.Marshal(map[string]any{"facts": []map[string]any{{"fact": fact, "confidence": 0.9}}})
		return &model.GenerateResponse{Text: string(raw)}, nil
	}
	reg := model.NewEmptyRegistry()
	for _, p := range []model.Profile{
		{ID: "sum", Role: model.RoleSummarizer, Backend: "sum", ProviderKind: "vllm", Endpoint: "http://sum", ContextLength: 64000},
		{ID: "embed", Role: model.RoleEmbedder, Backend: "embed", ProviderKind: "vllm", Endpoint: "http://embed", ContextLength: 8000},
	} {
		if err := reg.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	router := model.NewRouter(reg, nil)
	router.RegisterBackend("sum", func(context.Context, model.Profile, *slog.Logger) (any, error) { return summarizer, nil })
	router.RegisterBackend("embed", func(context.Context, model.Profile, *slog.Logger) (any, error) { return testllm.New(), nil })

	job := fleetDistillJob{d: &distill.Distiller{Router: router, AgentID: "main"}}
	res, err := job.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 || !res.Changed {
		t.Fatalf("%d prompts, %+v", len(prompts), res)
	}
	for _, p := range prompts {
		if strings.Contains(p, l.webOut) == strings.Contains(p, l.dbOut) {
			t.Fatalf("one distillation saw both machines' output, or neither:\n%s", p)
		}
		if strings.Contains(p, l.masterMark) {
			t.Fatal("a distillation saw the master's own memory")
		}
	}
	web, db := memoryText(t, l.web.id), memoryText(t, l.db.id)
	if !strings.Contains(web, "distilled from "+l.webOut+" (distill)") || strings.Contains(web, "distilled from "+l.dbOut) ||
		!strings.Contains(db, "distilled from "+l.dbOut+" (distill)") || strings.Contains(db, "distilled from "+l.webOut) {
		t.Fatalf("the facts went elsewhere:\nweb1: %s\ndb1: %s", web, db)
	}
	if _, err := job.Run(context.Background()); err != nil || len(prompts) != 2 {
		t.Fatalf("a second run distilled again: %d prompts, %v", len(prompts), err)
	}
	// A distilled fact is the machine's: it comes back inside its frame.
	recalled, err := fleetRecallOf(context.Background(), storedMachine(t, l.web.id), "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if head, _, _ := strings.Cut(recalled, "\n[fleet web1 "); strings.Contains(head, "distilled from") {
		t.Fatalf("a distilled fact was given back as the master's:\n%s", recalled)
	}
}

// `tenant privacy forget --episode <id>` reaches a machine's extract of that
// turn by the run the master's episode was recorded in, and nothing else.
func TestAForgetOfAnEpisodeReachesTheMachinesExtractOfIt(t *testing.T) {
	l := newMemLab(t)
	ctx := context.Background()
	gone, kept := mark("forget-me"), mark("keep-me")
	master, err := episodic.Open(filepath.Join(l.data, "episodes.db"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, text := range []string{gone, kept} {
		run := "run-of-" + text
		id, err := master.Insert(ctx, &episodic.Episode{AgentID: "main", SessionID: "s1", SourceRunID: run, Prompt: "look at the disks",
			Response: "done", Tags: []string{fleetHostTag + l.ch.id}, EmbedderID: "e", Embedding: []float32{1, 0.5, 0.25}})
		if err != nil {
			t.Fatal(err)
		}
		ids[text] = id
		ep := episodic.Episode{AgentID: "main", SessionID: "s1", SourceRunID: run, Prompt: "look at the disks",
			Response: "fleet_read {}\n" + text + " is in the output", Tags: []string{fleetExtractTag}, EmbedderID: "e", Embedding: []float32{1, 0.5, 0.25}}
		if err := fleetWriteEpisode(ctx, l.ch.id, &ep); err != nil {
			t.Fatal(err)
		}
	}
	_ = master.Close()
	var out bytes.Buffer
	if err := cmdPrivacyForget(ctx, []string{"--episode", strconv.FormatInt(ids[gone], 10), "--yes", "--config", l.cfg, "--data", l.data}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "fleet: The same is forgotten from every fleet machine's memory") {
		t.Fatalf("tenant privacy forget:\n%s", out.String())
	}
	l.mem().sweep()
	if text := memoryText(t, l.ch.id); strings.Contains(text, gone) || !strings.Contains(text, kept) {
		t.Fatalf("after forgetting the master's episode %d:\n%s", ids[gone], text)
	}
}
