package main

import (
	"strings"
	"testing"
)

func ghCtl(t *testing.T) githubTUIControl {
	t.Helper()
	return githubTUIControl{cfgDir: t.TempDir()}
}

// The whole point of /github is that a write flag is a per-repo safety setting
// that survives being written to config and read back. A round trip that
// silently downgrades a repo to read-only would be a safety regression; one
// that silently UPGRADES it would be worse.
func TestGitHubTUIControl_RoundTripPreservesPerRepoWrite(t *testing.T) {
	g := ghCtl(t)

	if _, err := g.Add("Dog279/TENANT", true); err != nil {
		t.Fatalf("add writable: %v", err)
	}
	if _, err := g.Add("Dog279/bitematch", false); err != nil {
		t.Fatalf("add watch-only: %v", err)
	}

	repos, err := g.Repos()
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("want 2 repos, got %+v", repos)
	}
	if !repos[0].Write || !repos[0].Primary {
		t.Errorf("first repo should be writable and primary: %+v", repos[0])
	}
	if repos[1].Write || repos[1].Primary {
		t.Errorf("second repo should be watch-only and not primary: %+v", repos[1])
	}
}

// Flipping write must persist, and must be reversible - the operator changes
// this far more often than the token or the repo set.
func TestGitHubTUIControl_SetWriteBothDirections(t *testing.T) {
	g := ghCtl(t)
	if _, err := g.Add("Dog279/bitematch", false); err != nil {
		t.Fatalf("add: %v", err)
	}

	if info, err := g.SetWrite("dog279/BITEMATCH", true); err != nil { // case-insensitive
		t.Fatalf("set write: %v", err)
	} else if !info.Write {
		t.Fatal("expected writable after set")
	}
	repos, _ := g.Repos()
	if !repos[0].Write {
		t.Fatal("write flag did not persist")
	}

	if _, err := g.SetWrite("Dog279/bitematch", false); err != nil {
		t.Fatalf("set readonly: %v", err)
	}
	repos, _ = g.Repos()
	if repos[0].Write {
		t.Fatal("readonly did not persist")
	}
}

// Flipping a repo that is not watched must fail loudly. Silently adding it
// would grant write to something the operator never chose to watch.
func TestGitHubTUIControl_SetWriteOnUnwatchedRepoFails(t *testing.T) {
	g := ghCtl(t)
	if _, err := g.Add("Dog279/TENANT", false); err != nil {
		t.Fatalf("add: %v", err)
	}
	_, err := g.SetWrite("someoneelse/private", true)
	if err == nil {
		t.Fatal("expected an error for an unwatched repo")
	}
	if !strings.Contains(err.Error(), "not watched") {
		t.Errorf("error should say it is not watched: %v", err)
	}
	if repos, _ := g.Repos(); len(repos) != 1 {
		t.Errorf("a failed SetWrite must not add the repo: %+v", repos)
	}
}

func TestGitHubTUIControl_AddRejectsDuplicateAndBadSlug(t *testing.T) {
	g := ghCtl(t)
	if _, err := g.Add("Dog279/TENANT", false); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := g.Add("Dog279/TENANT", true); err == nil {
		t.Error("duplicate add should fail rather than silently flip write")
	}
	if _, err := g.Add("not-a-slug", false); err == nil {
		t.Error("a bare name should be rejected")
	}
	// The :write suffix in the argument should win, so the command reads how it
	// behaves.
	if info, err := g.Add("Dog279/nebula:write", false); err != nil {
		t.Fatalf("add with suffix: %v", err)
	} else if !info.Write {
		t.Error("an explicit :write suffix should make the repo writable")
	}
}

func TestGitHubTUIControl_Remove(t *testing.T) {
	g := ghCtl(t)
	if _, err := g.Add("Dog279/TENANT", true); err != nil {
		t.Fatalf("add: %v", err)
	}
	ok, err := g.Remove("Dog279/TENANT")
	if err != nil || !ok {
		t.Fatalf("remove: ok=%v err=%v", ok, err)
	}
	if repos, _ := g.Repos(); len(repos) != 0 {
		t.Errorf("repo should be gone: %+v", repos)
	}
	if ok, err := g.Remove("Dog279/TENANT"); err != nil || ok {
		t.Errorf("removing an unwatched repo should report false, not error: ok=%v err=%v", ok, err)
	}
}

// An unconfigured install must report an empty list, not an error - the TUI
// turns that into "add one with /github add".
func TestGitHubTUIControl_EmptyWhenUnconfigured(t *testing.T) {
	g := ghCtl(t)
	repos, err := g.Repos()
	if err != nil {
		t.Fatalf("unconfigured should not error: %v", err)
	}
	if len(repos) != 0 {
		t.Errorf("expected no repos, got %+v", repos)
	}
}
