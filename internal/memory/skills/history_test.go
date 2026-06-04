package skills

import (
	"context"
	"strings"
	"testing"
)

// First Upsert of a never-seen skill records NO history (it's v1; the
// current row IS the first version).
func TestHistory_FirstUpsertHasNoHistory(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	if _, err := s.Upsert(ctx, &Skill{
		AgentID: "main", Name: "researcher", Description: "v1 desc", Recipe: "v1 recipe",
	}); err != nil {
		t.Fatal(err)
	}
	hs, err := s.History(ctx, "main", "researcher")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hs) != 0 {
		t.Errorf("first insert should produce 0 history entries, got %d", len(hs))
	}
}

// Each subsequent Upsert snapshots the PRIOR row into history. Versions
// are sequential per (agent, name).
func TestHistory_TracksEveryEdit(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	// v1
	if _, err := s.Upsert(ctx, &Skill{AgentID: "main", Name: "x", Description: "d1", Recipe: "r1"}); err != nil {
		t.Fatal(err)
	}
	// v2 — describing this means we snapshot v1 (the prior row).
	if _, err := s.Upsert(ctx, &Skill{AgentID: "main", Name: "x", Description: "d2", Recipe: "r2"}); err != nil {
		t.Fatal(err)
	}
	// v3 — snapshot v2.
	if _, err := s.Upsert(ctx, &Skill{AgentID: "main", Name: "x", Description: "d3", Recipe: "r3"}); err != nil {
		t.Fatal(err)
	}
	hs, err := s.History(ctx, "main", "x")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hs) != 2 {
		t.Fatalf("want 2 history entries (v1 + v2 as predecessors), got %d", len(hs))
	}
	// Newest first → v2 then v1.
	if hs[0].Version != 2 || hs[0].PriorDescription != "d2" {
		t.Errorf("hs[0] wrong: %+v", hs[0])
	}
	if hs[1].Version != 1 || hs[1].PriorDescription != "d1" {
		t.Errorf("hs[1] wrong: %+v", hs[1])
	}
	// Current live row is d3/r3.
	cur, _ := s.GetByName(ctx, "main", "x")
	if cur.Description != "d3" || cur.Recipe != "r3" {
		t.Errorf("current row wrong: %+v", cur)
	}
}

// changeSource is recorded so /skills history can show "who did this".
func TestHistory_ChangeSourceRecorded(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	_, _ = s.Upsert(ctx, &Skill{AgentID: "main", Name: "x", Description: "d1", Recipe: "r1"})
	// Replace via "seed" — that should be recorded as the change_source on
	// the v1 history snapshot.
	_, _ = s.UpsertWithSource(ctx, &Skill{AgentID: "main", Name: "x", Description: "d2", Recipe: "r2"}, "seed")
	hs, _ := s.History(ctx, "main", "x")
	if len(hs) != 1 {
		t.Fatalf("want 1 entry, got %d", len(hs))
	}
	if hs[0].ChangeSource != "seed" {
		t.Errorf("change_source = %q, want %q", hs[0].ChangeSource, "seed")
	}
}

// RevertTo restores the prior values + snapshots the current state into
// a NEW history entry (with change_source = "revert"). Reverting is
// itself a recorded edit.
func TestHistory_Revert(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	_, _ = s.Upsert(ctx, &Skill{AgentID: "main", Name: "x", Description: "good v1", Recipe: "good r1"})
	_, _ = s.Upsert(ctx, &Skill{AgentID: "main", Name: "x", Description: "bad v2", Recipe: "bad r2"})

	hs, _ := s.History(ctx, "main", "x")
	if len(hs) != 1 || hs[0].Version != 1 {
		t.Fatalf("setup: expected single v1 prior entry, got %+v", hs)
	}
	v1 := hs[0]

	// Revert to v1.
	reverted, err := s.RevertTo(ctx, "main", "x", v1.Version)
	if err != nil {
		t.Fatalf("RevertTo: %v", err)
	}
	if reverted.Description != "good v1" || reverted.Recipe != "good r1" {
		t.Errorf("revert didn't restore: %+v", reverted)
	}
	// Live row now matches the v1 snapshot.
	cur, _ := s.GetByName(ctx, "main", "x")
	if cur.Description != "good v1" {
		t.Errorf("live row not restored: %q", cur.Description)
	}
	// History now has 2 entries: v1 (the original prior) AND v2 (the
	// "bad v2" state we just snapshotted before reverting). v2 carries
	// change_source = "revert".
	hs, _ = s.History(ctx, "main", "x")
	if len(hs) != 2 {
		t.Fatalf("after revert want 2 entries, got %d", len(hs))
	}
	if hs[0].Version != 2 || hs[0].ChangeSource != "revert" || hs[0].PriorDescription != "bad v2" {
		t.Errorf("revert entry wrong: %+v", hs[0])
	}
	// Revert to a missing version → useful error.
	if _, err := s.RevertTo(ctx, "main", "x", 99); err == nil {
		t.Error("revert to missing version should error")
	}
}

// GetHistoryEntry: targeted lookup for a specific prior version. Returns
// (nil, nil) when not found — caller distinguishes from error.
func TestHistory_GetHistoryEntry(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	_, _ = s.Upsert(ctx, &Skill{AgentID: "main", Name: "x", Description: "d1", Recipe: "r1"})
	_, _ = s.Upsert(ctx, &Skill{AgentID: "main", Name: "x", Description: "d2", Recipe: "r2"})

	got, err := s.GetHistoryEntry(ctx, "main", "x", 1)
	if err != nil {
		t.Fatalf("GetHistoryEntry: %v", err)
	}
	if got == nil {
		t.Fatal("v1 should exist after a single edit")
	}
	if got.PriorDescription != "d1" {
		t.Errorf("wrong entry: %+v", got)
	}
	// Missing version → (nil, nil).
	missing, err := s.GetHistoryEntry(ctx, "main", "x", 99)
	if err != nil || missing != nil {
		t.Errorf("missing version: got (%v, %v), want (nil, nil)", missing, err)
	}
}

// History tx atomicity: a failed Upsert (e.g. bad inputs) must NOT leave
// a phantom history snapshot. We can't easily force the SQL update to fail
// post-snapshot, but we CAN verify that validation failures (caught before
// the tx) leave history untouched.
func TestHistory_TxAtomicity_ValidationFailure(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	_, _ = s.Upsert(ctx, &Skill{AgentID: "main", Name: "x", Description: "d1", Recipe: "r1"})

	// Invalid update (empty name) — Upsert returns an error BEFORE any tx
	// starts. History stays empty.
	_, err := s.Upsert(ctx, &Skill{AgentID: "main", Name: "", Description: "d2", Recipe: "r2"})
	if err == nil {
		t.Fatal("empty name should error")
	}
	hs, _ := s.History(ctx, "main", "x")
	if len(hs) != 0 {
		t.Errorf("validation failure left phantom history: %+v", hs)
	}
}

// History per-agent isolation: a skill named "x" on agent A is independent
// of "x" on agent B. Editing one doesn't write history for the other.
func TestHistory_AgentIsolation(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	_, _ = s.Upsert(ctx, &Skill{AgentID: "a", Name: "shared", Description: "a-v1", Recipe: "a-r1"})
	_, _ = s.Upsert(ctx, &Skill{AgentID: "b", Name: "shared", Description: "b-v1", Recipe: "b-r1"})
	// Edit a only.
	_, _ = s.Upsert(ctx, &Skill{AgentID: "a", Name: "shared", Description: "a-v2", Recipe: "a-r2"})

	if hs, _ := s.History(ctx, "a", "shared"); len(hs) != 1 || !strings.Contains(hs[0].PriorDescription, "a-v1") {
		t.Errorf("agent a should have 1 entry with a-v1, got %+v", hs)
	}
	if hs, _ := s.History(ctx, "b", "shared"); len(hs) != 0 {
		t.Errorf("agent b should have 0 entries, got %+v", hs)
	}
}

// GetByName: returns the live row for (agent, name); nil + nil when
// missing.
func TestGetByName(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	_, _ = s.Upsert(ctx, &Skill{AgentID: "main", Name: "x", Description: "d", Recipe: "r"})

	got, err := s.GetByName(ctx, "main", "x")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got == nil || got.Description != "d" {
		t.Errorf("got %+v, want skill with desc 'd'", got)
	}
	missing, err := s.GetByName(ctx, "main", "ghost")
	if err != nil || missing != nil {
		t.Errorf("missing: got (%v, %v), want (nil, nil)", missing, err)
	}
}
