package soul_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/memory/soul"
)

func TestProposeEdit_WritesToProposedDir(t *testing.T) {
	dir := t.TempDir()
	s := soul.NewDefault("main")
	s.User.Facts = []string{"prefers black coffee"}

	id, err := soul.ProposeEdit(dir, "main", "noticed coffee preference", s)
	if err != nil {
		t.Fatalf("ProposeEdit: %v", err)
	}
	if id == "" {
		t.Fatal("ProposeEdit returned empty id")
	}
	// File must exist under proposed/.
	want := filepath.Join(dir, "soul", "proposed", id+".toml")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("proposal file missing at %s: %v", want, err)
	}
}

func TestProposeEdit_IDIncludesReasonSlug(t *testing.T) {
	dir := t.TempDir()
	id, err := soul.ProposeEdit(dir, "main", "Add User Timezone!!", soul.NewDefault("main"))
	if err != nil {
		t.Fatalf("ProposeEdit: %v", err)
	}
	if !strings.Contains(id, "add-user-timezone") {
		t.Errorf("id = %q, want slugified reason 'add-user-timezone'", id)
	}
}

func TestProposeEdit_RejectsEmptyAgentID(t *testing.T) {
	dir := t.TempDir()
	_, err := soul.ProposeEdit(dir, "", "reason", soul.NewDefault("main"))
	if err == nil {
		t.Fatal("expected error on empty agentID")
	}
}

func TestProposeEdit_RejectsNilSoul(t *testing.T) {
	dir := t.TempDir()
	_, err := soul.ProposeEdit(dir, "main", "reason", nil)
	if err == nil {
		t.Fatal("expected error on nil soul")
	}
}

func TestListProposals_ReturnsOldestFirst(t *testing.T) {
	dir := t.TempDir()
	for _, reason := range []string{"first", "second", "third"} {
		if _, err := soul.ProposeEdit(dir, "main", reason, soul.NewDefault("main")); err != nil {
			t.Fatalf("ProposeEdit %s: %v", reason, err)
		}
	}
	got, err := soul.ListProposals(dir, "main")
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d proposals, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].ProposedAt.Before(got[i-1].ProposedAt) {
			t.Errorf("proposals not in oldest-first order at index %d", i)
		}
	}
}

func TestListProposals_FiltersByAgentID(t *testing.T) {
	dir := t.TempDir()
	if _, err := soul.ProposeEdit(dir, "alice", "x", soul.NewDefault("alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := soul.ProposeEdit(dir, "bob", "y", soul.NewDefault("bob")); err != nil {
		t.Fatal(err)
	}
	got, err := soul.ListProposals(dir, "alice")
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(got) != 1 || got[0].AgentID != "alice" {
		t.Fatalf("filter by agent broken: got %+v", got)
	}
}

func TestListProposals_MissingDirReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := soul.ListProposals(dir, "main")
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d proposals from empty dir, want 0", len(got))
	}
}

func TestAccept_AppliesAndRemovesProposal(t *testing.T) {
	dir := t.TempDir()
	// First save a baseline live soul.
	live := soul.NewDefault("main")
	if err := live.Save(dir); err != nil {
		t.Fatal(err)
	}
	// Propose an edit.
	edited := soul.NewDefault("main")
	edited.User.Name = "Ada"
	edited.User.Facts = []string{"prefers Go"}
	id, err := soul.ProposeEdit(dir, "main", "set user name", edited)
	if err != nil {
		t.Fatal(err)
	}
	// Accept.
	if err := soul.Accept(dir, id); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	// Live soul now has the new user name.
	updated, err := soul.Load(dir, "main")
	if err != nil {
		t.Fatalf("Load after Accept: %v", err)
	}
	if updated.User.Name != "Ada" {
		t.Fatalf("User.Name = %q, want Ada", updated.User.Name)
	}
	// Proposal file gone.
	if _, err := os.Stat(filepath.Join(dir, "soul", "proposed", id+".toml")); !os.IsNotExist(err) {
		t.Fatalf("proposal not cleaned up: %v", err)
	}
}

func TestReject_RemovesProposalLeavesLiveAlone(t *testing.T) {
	dir := t.TempDir()
	live := soul.NewDefault("main")
	live.User.Name = "Original"
	if err := live.Save(dir); err != nil {
		t.Fatal(err)
	}
	edited := soul.NewDefault("main")
	edited.User.Name = "Changed"
	id, err := soul.ProposeEdit(dir, "main", "change name", edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := soul.Reject(dir, id); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	got, err := soul.Load(dir, "main")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.User.Name != "Original" {
		t.Fatalf("Reject changed live soul: User.Name = %q", got.User.Name)
	}
	if _, err := os.Stat(filepath.Join(dir, "soul", "proposed", id+".toml")); !os.IsNotExist(err) {
		t.Fatal("proposal not removed by Reject")
	}
}
