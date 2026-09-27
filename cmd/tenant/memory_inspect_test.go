package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/dashboard"
	"tenant/internal/memory/archive"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/sme"
)

// inspectRig is the dashboard memory adapter over real stores, able to embed
// new versions.
func inspectRig(t *testing.T) (dashMemory, *semantic.Store, *episodic.Store) {
	t.Helper()
	d, ss, es, _ := dashMemTestRig(t)
	d.c.embedder = stubEmbedder{vec: []float32{0, 1}}
	d.c.embedderID = "t2"
	d.c.dataDir = t.TempDir()
	return d, ss, es
}

func TestDashMemory_FactDetailExplainsAFact(t *testing.T) {
	d, ss, es := inspectRig(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	epID, err := es.Insert(ctx, &episodic.Episode{
		AgentID: "main", SessionID: "s1", Timestamp: at, Prompt: "I moved to Munich", Response: "noted",
		EmbedderID: "t", Embedding: []float32{1, 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := mkDashFact("User lives in Berlin", []int64{epID, 999})
	f.OriginJob, f.OriginModel = "distill", "glm-5"
	oldID, err := ss.Insert(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if err := ss.SetPinned(ctx, oldID, true); err != nil {
		t.Fatal(err)
	}
	if err := ss.BumpAccess(ctx, []int64{oldID}); err != nil {
		t.Fatal(err)
	}

	newID, err := d.EditFact(oldID, "User lives in Munich")
	if err != nil {
		t.Fatalf("EditFact: %v", err)
	}
	st, err := sme.New(ss.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertSection(ctx, sme.Section{AgentID: "main", Section: "About the user", Body: "Munich", SourceFactIDs: []int64{newID}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ss.BumpAccess(ctx, []int64{newID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ss.DB().ExecContext(ctx, `INSERT INTO fact_source_tombstones (fact_id, source_kind, source_id, removed_at, reason) VALUES (?, 'episode', '7', ?, 'forget')`, newID, at.Unix()); err != nil {
		t.Fatal(err)
	}

	det, err := d.FactDetail(newID)
	if err != nil {
		t.Fatalf("FactDetail: %v", err)
	}
	if det.Fact.ID != newID || det.Fact.Text != "User lives in Munich" || det.Fact.Status != dashboard.FactStatusLive {
		t.Fatalf("fact = %+v", det.Fact)
	}
	if det.Origin == nil || det.Origin.Job != "edit" || det.Origin.Model != "" {
		t.Fatalf("origin = %+v", det.Origin)
	}
	if !det.Signals.Pinned || det.Signals.AccessCount != 2 || det.Signals.LastAccessed == nil || det.Signals.ValidTo != nil {
		t.Fatalf("signals = %+v (the pin carries over, the access count starts again)", det.Signals)
	}
	if len(det.Sources) != 2 || det.Sources[0].Prompt != "I moved to Munich" || !det.Sources[1].Missing {
		t.Fatalf("sources = %+v", det.Sources)
	}
	if rs := det.RemovedSources; len(rs) != 1 || rs[0].Kind != "episode" || rs[0].ID != "7" || !rs[0].RemovedAt.Equal(at) || rs[0].Reason != "forget" {
		t.Fatalf("removed sources = %+v", det.RemovedSources)
	}
	if len(det.Versions) != 2 || det.Versions[0].ID != oldID || det.Versions[0].Status != dashboard.FactStatusSuperseded ||
		det.Versions[0].Job != "distill" || det.Versions[1].ID != newID || det.Versions[1].Job != "edit" {
		t.Fatalf("versions = %+v", det.Versions)
	}
	if len(det.Replaced) != 1 || det.Replaced[0].ID != oldID || det.Replaced[0].Job != "edit" || det.ReplacedBy != nil {
		t.Fatalf("replaced = %+v, replaced by %+v", det.Replaced, det.ReplacedBy)
	}
	if !slices.Equal(det.Downstream.SMESections, []string{"About the user"}) || det.Downstream.Retrievals != 2 {
		t.Fatalf("downstream = %+v", det.Downstream)
	}

	old, err := d.FactDetail(oldID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Fact.Status != dashboard.FactStatusSuperseded || old.ReplacedBy == nil || old.ReplacedBy.ID != newID || old.ReplacedBy.Job != "edit" {
		t.Fatalf("old version = %+v, replaced by %+v", old.Fact, old.ReplacedBy)
	}
	if old.Origin == nil || old.Origin.Job != "distill" || old.Origin.Model != "glm-5" || old.Signals.AccessCount != 1 {
		t.Fatalf("old version origin %+v, signals %+v", old.Origin, old.Signals)
	}
	nf, err := ss.Get(ctx, newID)
	if err != nil {
		t.Fatal(err)
	}
	if nf.EmbedderID != "t2" || !slices.Equal(nf.Embedding, []float32{0, 1}) {
		t.Fatalf("new version embedded with %q %v; want the live embedder", nf.EmbedderID, nf.Embedding)
	}
}

func TestDashMemory_FactDetailOfAnOldFact(t *testing.T) {
	d, ss, _ := inspectRig(t)
	id, err := ss.Insert(context.Background(), mkDashFact("User likes tea", nil))
	if err != nil {
		t.Fatal(err)
	}
	det, err := d.FactDetail(id)
	if err != nil {
		t.Fatal(err)
	}
	if det.Origin != nil || len(det.Versions) != 1 || det.Replaced != nil || det.ReplacedBy != nil || det.Downstream.SMESections != nil {
		t.Fatalf("a fact with no history = %+v", det)
	}
	if _, err := d.FactDetail(12345); err == nil {
		t.Fatal("a missing fact explained itself")
	}
}

func TestDashMemory_EditNeedsAnEmbedder(t *testing.T) {
	d, ss, _ := inspectRig(t)
	d.c.embedder = nil
	id, err := ss.Insert(context.Background(), mkDashFact("User lives in Berlin", nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.EditFact(id, "User lives in Munich"); err == nil || !strings.Contains(err.Error(), "embedder") {
		t.Fatalf("edit without an embedder = %v", err)
	}
	if _, err := d.RestoreFactVersion(id); err == nil {
		t.Fatal("restore without an embedder succeeded")
	}
	vs, err := ss.Versions(context.Background(), id)
	if err != nil || len(vs) != 1 || vs[0].SupersededBy != 0 {
		t.Fatalf("a refused edit left versions %v (%v)", vs, err)
	}
}

func TestDashMemory_RestoreFactVersion(t *testing.T) {
	d, ss, _ := inspectRig(t)
	ctx := context.Background()
	d.c.embedderID = "" // falls back to the fact's own embedder
	oldID, err := ss.Insert(ctx, mkDashFact("User lives in Berlin", nil))
	if err != nil {
		t.Fatal(err)
	}
	midID, err := d.EditFact(oldID, "User lives in Munich")
	if err != nil {
		t.Fatal(err)
	}
	headID, err := d.RestoreFactVersion(oldID)
	if err != nil {
		t.Fatalf("RestoreFactVersion: %v", err)
	}
	det, err := d.FactDetail(headID)
	if err != nil {
		t.Fatal(err)
	}
	if det.Fact.Text != "User lives in Berlin" || det.Origin == nil || det.Origin.Job != "restore" || len(det.Versions) != 3 {
		t.Fatalf("restored = %+v origin %+v versions %d", det.Fact, det.Origin, len(det.Versions))
	}
	if len(det.Replaced) != 1 || det.Replaced[0].ID != midID || det.Replaced[0].Job != "restore" {
		t.Fatalf("the restore replaced %+v", det.Replaced)
	}
	head, err := ss.Get(ctx, headID)
	if err != nil {
		t.Fatal(err)
	}
	if head.EmbedderID != "t" {
		t.Fatalf("restored version embedder = %q, want the fact's own", head.EmbedderID)
	}
	if _, err := d.RestoreFactVersion(headID); err == nil {
		t.Fatal("restoring the current version succeeded")
	}
}

func TestDashMemory_InvalidateAndRevalidate(t *testing.T) {
	d, ss, _ := inspectRig(t)
	id, err := ss.Insert(context.Background(), mkDashFact("User works at Acme", nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.InvalidateFact(id); err != nil {
		t.Fatal(err)
	}
	det, err := d.FactDetail(id)
	if err != nil {
		t.Fatal(err)
	}
	if det.Signals.ValidTo == nil || time.Since(*det.Signals.ValidTo) > time.Minute {
		t.Fatalf("invalidated fact valid_to = %v", det.Signals.ValidTo)
	}
	if det.Fact.Status != dashboard.FactStatusLive {
		t.Fatalf("an invalidated fact is %s; it stays in memory for as-of recall", det.Fact.Status)
	}
	if err := d.RevalidateFact(id); err != nil {
		t.Fatal(err)
	}
	if det, err = d.FactDetail(id); err != nil || det.Signals.ValidTo != nil {
		t.Fatalf("revalidated fact valid_to = %v (%v)", det.Signals.ValidTo, err)
	}
}

func TestDashMemory_FactTurnsRebuildsTheConversation(t *testing.T) {
	d, ss, es := inspectRig(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	w := archive.NewWriter(d.c.dataDir)
	for _, ev := range []archive.Event{
		{Timestamp: at.Add(-time.Minute), SessionID: "s1", Role: "user", Content: "hello"},
		{Timestamp: at.Add(-50 * time.Second), SessionID: "s1", Role: "assistant", Content: "hi"},
		{Timestamp: at, SessionID: "s1", Role: "user", Content: "I moved to Munich"},
		{Timestamp: at.Add(time.Second), SessionID: "s1", Role: "assistant", ToolCalls: []archive.ToolCall{{ID: "c1", Name: "memory_search", Arguments: json.RawMessage(`{"q":"home"}`)}}},
		{Timestamp: at.Add(2 * time.Second), SessionID: "s1", Role: "tool", ToolResult: &archive.ToolResult{CallID: "c1", Content: "no match"}},
		{Timestamp: at.Add(3 * time.Second), SessionID: "s1", Role: "assistant", Content: "Noted: Munich."},
		{Timestamp: at.Add(time.Hour), SessionID: "s1", Role: "user", Content: "what's the weather"},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	ep := func(session, prompt string) int64 {
		id, err := es.Insert(ctx, &episodic.Episode{AgentID: "main", SessionID: session, Timestamp: at.Add(5 * time.Second), Prompt: prompt, Response: "r", EmbedderID: "t", Embedding: []float32{1, 0}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	archived, unarchived := ep("s1", "I moved to Munich"), ep("s2", "never archived")
	id, err := ss.Insert(ctx, mkDashFact("User lives in Munich", []int64{archived, unarchived, 999}))
	if err != nil {
		t.Fatal(err)
	}

	turns, err := d.FactTurns(id)
	if err != nil {
		t.Fatalf("FactTurns: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("turns = %+v", turns)
	}
	var got []string
	for _, ev := range turns[0].Events {
		got = append(got, ev.Role+": "+ev.Content)
	}
	want := []string{"user: I moved to Munich", `assistant: → memory_search {"q":"home"}`, "tool: no match", "assistant: Noted: Munich."}
	if turns[0].Missing || turns[0].SessionID != "s1" || !slices.Equal(got, want) {
		t.Fatalf("archived turn = %+v\n%q", turns[0], got)
	}
	if !turns[1].Missing || !strings.Contains(turns[1].Why, "no record") || turns[1].SessionID != "s2" {
		t.Fatalf("unarchived turn = %+v", turns[1])
	}
	if !turns[2].Missing || turns[2].EpisodeID != 999 || !strings.Contains(turns[2].Why, "no longer in memory") {
		t.Fatalf("forgotten turn = %+v", turns[2])
	}

	d.c.dataDir = ""
	if turns, err = d.FactTurns(id); err != nil || !turns[0].Missing || !strings.Contains(turns[0].Why, "isn't available") {
		t.Fatalf("turns without an archive = %+v (%v)", turns, err)
	}
}

func TestTurnEventText(t *testing.T) {
	for _, tc := range []struct {
		ev   archive.Event
		want string
	}{
		{archive.Event{Content: "plain"}, "plain"},
		{archive.Event{Content: "let me look", ToolCalls: []archive.ToolCall{{Name: "a", Arguments: json.RawMessage(`{}`)}, {Name: "b", Arguments: json.RawMessage(`{"x":1}`)}}}, "let me look\n→ a {}\n→ b {\"x\":1}"},
		{archive.Event{ToolResult: &archive.ToolResult{Content: "boom", IsError: true}}, "error: boom"},
		{archive.Event{Content: "said", ToolResult: &archive.ToolResult{Content: "raw"}}, "said"},
	} {
		if got := turnEventText(tc.ev); got != tc.want {
			t.Errorf("turnEventText(%+v) = %q, want %q", tc.ev, got, tc.want)
		}
	}
}
