package soul_test

import (
	"errors"
	"strings"
	"testing"

	"tenant/internal/memory/soul"
)

// Add/edit/remove by derived ID must mutate the right list element and
// persist through Save/Load — the core contract the dashboard editor uses.
func TestSoul_DeriveIDRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := soul.NewDefault("main")
	s.User.Facts = []string{"lives in NYC", "prefers Go"}

	// IDs are stable: deriving twice for the same text matches what Items returns.
	items, err := s.Items(soul.SectionUserFact)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("Items len = %d, want 2", len(items))
	}
	if items[0].ID != soul.DeriveItemID(soul.SectionUserFact, "lives in NYC") {
		t.Fatalf("derived ID mismatch: %q", items[0].ID)
	}

	// Edit the first fact by its ID.
	nycID := items[0].ID
	if _, err := s.EditItem(string(soul.SectionUserFact), nycID, "lives in Boston"); err != nil {
		t.Fatalf("EditItem: %v", err)
	}
	// Add an instruction.
	if _, err := s.AddItem(string(soul.SectionInstruction), "always cite sources"); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	// Remove the second fact ("prefers Go") by its ID.
	goID := soul.DeriveItemID(soul.SectionUserFact, "prefers Go")
	if err := s.RemoveItem(string(soul.SectionUserFact), goID); err != nil {
		t.Fatalf("RemoveItem: %v", err)
	}

	if err := s.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := soul.Load(dir, "main")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Join(loaded.User.Facts, "|"); got != "lives in Boston" {
		t.Fatalf("facts after edit+remove = %q, want \"lives in Boston\"", got)
	}
	if !contains(loaded.Instructions.Items, "always cite sources") {
		t.Fatalf("instruction not persisted: %v", loaded.Instructions.Items)
	}
}

// Edit/remove against an ID that no longer matches must error, not silently
// clobber a neighbor (the positional-clobber bug derived IDs prevent).
func TestSoul_EditUnknownIDErrors(t *testing.T) {
	s := soul.NewDefault("main")
	s.User.Facts = []string{"one"}
	if _, err := s.EditItem(string(soul.SectionUserFact), "user_fact-deadbeefdeadbeef", "two"); !errors.Is(err, soul.ErrItemNotFound) {
		t.Fatalf("EditItem unknown id err = %v, want ErrItemNotFound", err)
	}
	if err := s.RemoveItem(string(soul.SectionInstruction), "instruction-0000000000000000"); !errors.Is(err, soul.ErrItemNotFound) {
		t.Fatalf("RemoveItem unknown id err = %v, want ErrItemNotFound", err)
	}
}

// Blank add/edit text and unknown sections are rejected.
func TestSoul_EditValidation(t *testing.T) {
	s := soul.NewDefault("main")
	if _, err := s.AddItem(string(soul.SectionUserFact), "   "); !errors.Is(err, soul.ErrEmptyItem) {
		t.Fatalf("AddItem blank err = %v, want ErrEmptyItem", err)
	}
	if _, err := s.AddItem("bogus", "x"); err == nil {
		t.Fatal("AddItem unknown section should error")
	}
}

// A fact and an instruction with identical text get DISTINCT IDs (the
// section is part of the hash) so editing one never touches the other.
func TestSoul_IDsNamespacedBySection(t *testing.T) {
	same := "be concise"
	if soul.DeriveItemID(soul.SectionUserFact, same) == soul.DeriveItemID(soul.SectionInstruction, same) {
		t.Fatal("user_fact and instruction IDs collided for identical text")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
