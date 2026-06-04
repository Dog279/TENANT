package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"tenant/internal/dashboard"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/soul"
	"tenant/internal/memory/working"
)

// dashMemTestRig opens real stores in a temp dir and returns a wired
// dashMemory adapter plus the underlying stores for assertions.
func dashMemTestRig(t *testing.T) (dashMemory, *semantic.Store, *episodic.Store, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	ss, err := semantic.Open(filepath.Join(dir, "facts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	es, err := episodic.Open(filepath.Join(dir, "eps.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })

	c := memControl{
		semantic: ss, episodic: es, agentID: "main", cfgDir: cfg,
		soulLive: soul.NewLive(soul.NewDefault("main")),
		working:  working.New(),
	}
	return dashMemory{c}, ss, es, cfg
}

func mkDashFact(claim string, src []int64) *semantic.Fact {
	return &semantic.Fact{
		AgentID: "main", Visibility: semantic.VisibilityPrivate, Fact: claim,
		Confidence: 0.9, EmbedderID: "t", Embedding: []float32{1, 0}, SourceEpisodes: src,
	}
}

// SoulEdit must round-trip through the live holder AND persist to disk so
// the agent sees it next turn and a restart sees it too.
func TestDashMemory_SoulEditRoundTrip(t *testing.T) {
	d, _, _, cfg := dashMemTestRig(t)

	if err := d.SoulEdit(dashboard.SoulEditOp{
		Section: dashboard.SoulSectionUserFact, Action: dashboard.SoulActionAdd, Text: "lives in NYC",
	}); err != nil {
		t.Fatalf("SoulEdit add: %v", err)
	}

	// Live holder reflects it immediately.
	view, err := d.Soul()
	if err != nil {
		t.Fatalf("Soul: %v", err)
	}
	if len(view.UserFacts) != 1 || view.UserFacts[0].Text != "lives in NYC" {
		t.Fatalf("soul view after add = %+v", view.UserFacts)
	}

	// Persisted to disk.
	loaded, err := soul.Load(cfg, "main")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.User.Facts) != 1 || loaded.User.Facts[0] != "lives in NYC" {
		t.Fatalf("soul not persisted: %v", loaded.User.Facts)
	}

	// Edit then remove by derived ID.
	id := view.UserFacts[0].ID
	if err := d.SoulEdit(dashboard.SoulEditOp{
		Section: dashboard.SoulSectionUserFact, Action: dashboard.SoulActionEdit, ID: id, Text: "lives in Boston",
	}); err != nil {
		t.Fatalf("SoulEdit edit: %v", err)
	}
	view, _ = d.Soul()
	newID := view.UserFacts[0].ID
	if view.UserFacts[0].Text != "lives in Boston" {
		t.Fatalf("edit not applied: %+v", view.UserFacts)
	}
	if err := d.SoulEdit(dashboard.SoulEditOp{
		Section: dashboard.SoulSectionUserFact, Action: dashboard.SoulActionRemove, ID: newID,
	}); err != nil {
		t.Fatalf("SoulEdit remove: %v", err)
	}
	view, _ = d.Soul()
	if len(view.UserFacts) != 0 {
		t.Fatalf("remove failed: %+v", view.UserFacts)
	}
}

func TestDashMemory_SoulEditRejectsBadAction(t *testing.T) {
	d, _, _, _ := dashMemTestRig(t)
	if err := d.SoulEdit(dashboard.SoulEditOp{Section: dashboard.SoulSectionUserFact, Action: "nuke"}); err == nil {
		t.Fatal("bad action should error")
	}
}

// Delete tombstones (gone from Facts, present in RemovedFacts); Restore
// brings it back. Resolve supersedes the discard so it leaves Facts too.
func TestDashMemory_DeleteRestoreResolve(t *testing.T) {
	d, ss, _, _ := dashMemTestRig(t)
	ctx := context.Background()
	keep, _ := ss.Insert(ctx, mkDashFact("user prefers Go", nil))
	discard, _ := ss.Insert(ctx, mkDashFact("user prefers Python", nil))

	// Delete the keep fact, then confirm it's removed + restorable.
	if err := d.DeleteFact(keep); err != nil {
		t.Fatalf("DeleteFact: %v", err)
	}
	facts, _, _ := d.Facts("", 0, "")
	if findFact(facts, keep) != nil {
		t.Fatal("deleted fact still in Facts")
	}
	removed, _ := d.RemovedFacts(0)
	if findFact(removed, keep) == nil {
		t.Fatal("deleted fact not in RemovedFacts")
	}
	if err := d.RestoreFact(keep); err != nil {
		t.Fatalf("RestoreFact: %v", err)
	}
	facts, _, _ = d.Facts("", 0, "")
	if findFact(facts, keep) == nil {
		t.Fatal("restored fact not back in Facts")
	}

	// Resolve: keep supersedes discard → discard leaves the live list.
	if err := d.ResolveFacts(keep, discard); err != nil {
		t.Fatalf("ResolveFacts: %v", err)
	}
	got, _ := ss.Get(ctx, discard)
	if got.SupersededBy != keep {
		t.Fatalf("discard.SupersededBy = %d, want %d", got.SupersededBy, keep)
	}
	facts, _, _ = d.Facts("", 0, "")
	if findFact(facts, discard) != nil {
		t.Fatal("superseded discard still in Facts")
	}
}

// Provenance resolves real source episodes and marks missing ones — without
// failing the whole call when one episode was forgotten.
func TestDashMemory_ProvenanceTolerantToMissingEpisode(t *testing.T) {
	d, ss, es, _ := dashMemTestRig(t)
	ctx := context.Background()
	epID, err := es.Insert(ctx, &episodic.Episode{
		AgentID: "main", Prompt: "what do I prefer?", Response: "Go", EmbedderID: "t", Embedding: []float32{1, 0},
	})
	if err != nil {
		t.Fatalf("episode insert: %v", err)
	}
	// One real episode + one that doesn't exist (999).
	factID, _ := ss.Insert(ctx, mkDashFact("user prefers Go", []int64{epID, 999}))

	prov, err := d.FactProvenance(factID)
	if err != nil {
		t.Fatalf("FactProvenance must not fail on a missing episode: %v", err)
	}
	if len(prov) != 2 {
		t.Fatalf("provenance len = %d, want 2", len(prov))
	}
	var sawReal, sawMissing bool
	for _, e := range prov {
		switch e.ID {
		case epID:
			sawReal = e.Prompt == "what do I prefer?" && !e.Missing
		case 999:
			sawMissing = e.Missing
		}
	}
	if !sawReal {
		t.Error("real episode not resolved with its prompt")
	}
	if !sawMissing {
		t.Error("missing episode not marked Missing")
	}
}

// Facts keyset pagination walks the whole store across pages.
func TestDashMemory_FactsPaginate(t *testing.T) {
	d, ss, _, _ := dashMemTestRig(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		ss.Insert(ctx, mkDashFact("fact "+string(rune('a'+i)), nil))
	}
	seen := map[int64]bool{}
	cursor := ""
	pages := 0
	for {
		page, next, err := d.Facts("", 2, cursor)
		if err != nil {
			t.Fatalf("Facts: %v", err)
		}
		for _, f := range page {
			if seen[f.ID] {
				t.Fatalf("fact %d returned twice across pages", f.ID)
			}
			seen[f.ID] = true
		}
		pages++
		if next == "" {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("paged %d distinct facts, want 5", len(seen))
	}
}

func TestDashMemory_WorkingCount(t *testing.T) {
	d, _, _, _ := dashMemTestRig(t)
	if d.WorkingCount() != 0 {
		t.Fatalf("empty working count = %d, want 0", d.WorkingCount())
	}
	d.c.working.Append(working.Message{Role: "user", Content: "hi"})
	d.c.working.Append(working.Message{Role: "assistant", Content: "hello"})
	if d.WorkingCount() != 2 {
		t.Fatalf("working count = %d, want 2", d.WorkingCount())
	}
}

// TemporalFacts enumerates every fact — live, superseded, tombstoned — onto
// the knowledge-time axis with a matching count summary. Status precedence is
// tombstoned > superseded > live, and the stats must always agree with the
// rows (both come from the same enumeration).
func TestDashMemory_TemporalFacts(t *testing.T) {
	d, ss, _, _ := dashMemTestRig(t)
	ctx := context.Background()

	live, _ := ss.Insert(ctx, mkDashFact("user lives in NYC", nil))
	keep, _ := ss.Insert(ctx, mkDashFact("user prefers Go", nil))
	superseded, _ := ss.Insert(ctx, mkDashFact("user prefers Python", nil))
	tombstoned, _ := ss.Insert(ctx, mkDashFact("user uses vim", nil))
	both, _ := ss.Insert(ctx, mkDashFact("user uses emacs", nil))

	// keep supersedes the Python + emacs claims; vim + emacs get tombstoned —
	// so "emacs" is both superseded AND tombstoned (exercises precedence).
	if err := d.ResolveFacts(keep, superseded); err != nil {
		t.Fatalf("ResolveFacts superseded: %v", err)
	}
	if err := d.ResolveFacts(keep, both); err != nil {
		t.Fatalf("ResolveFacts both: %v", err)
	}
	if err := d.DeleteFact(tombstoned); err != nil {
		t.Fatalf("DeleteFact tombstoned: %v", err)
	}
	if err := d.DeleteFact(both); err != nil {
		t.Fatalf("DeleteFact both: %v", err)
	}

	views, stats, err := d.TemporalFacts()
	if err != nil {
		t.Fatalf("TemporalFacts: %v", err)
	}

	// Stats and rows come from one enumeration, so they must never disagree.
	if len(views) != stats.Total {
		t.Fatalf("len(views)=%d != stats.Total=%d", len(views), stats.Total)
	}
	if want := (dashboard.MemStats{Total: 5, Live: 2, Superseded: 1, Tombstoned: 2}); stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}

	byID := map[int64]dashboard.TemporalFactView{}
	for _, v := range views {
		byID[v.ID] = v
	}
	if got := byID[live].Status; got != dashboard.FactStatusLive {
		t.Errorf("live status = %q, want %q", got, dashboard.FactStatusLive)
	}
	if got := byID[keep].Status; got != dashboard.FactStatusLive {
		t.Errorf("superseder status = %q, want %q", got, dashboard.FactStatusLive)
	}
	if v := byID[superseded]; v.Status != dashboard.FactStatusSuperseded || v.SupersededBy != keep {
		t.Errorf("superseded fact = %+v, want status=superseded SupersededBy=%d", v, keep)
	}
	if v := byID[tombstoned]; v.Status != dashboard.FactStatusTombstoned || !v.Tombstoned {
		t.Errorf("tombstoned fact = %+v, want status=tombstoned Tombstoned=true", v)
	}
	// Precedence: a fact that is BOTH superseded and tombstoned reports tombstoned.
	if v := byID[both]; v.Status != dashboard.FactStatusTombstoned {
		t.Errorf("both-superseded-and-tombstoned status = %q, want %q (tombstoned wins)", v.Status, dashboard.FactStatusTombstoned)
	}

	// Knowledge-time axis populated for every row; effective confidence equals
	// the base within the 30-day decay grace (facts were just inserted).
	for _, v := range views {
		if v.FirstSeen <= 0 || v.LastConfirmed <= 0 {
			t.Errorf("fact %d missing knowledge-time stamps: FirstSeen=%d LastConfirmed=%d", v.ID, v.FirstSeen, v.LastConfirmed)
		}
		if v.EffectiveConfidence != v.Confidence {
			t.Errorf("fact %d fresh EffectiveConfidence=%v, want base Confidence=%v", v.ID, v.EffectiveConfidence, v.Confidence)
		}
	}
}

func findFact(fs []dashboard.FactView, id int64) *dashboard.FactView {
	for i := range fs {
		if fs[i].ID == id {
			return &fs[i]
		}
	}
	return nil
}
