package skills

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func names(hits []Hit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Skill.Name)
	}
	return out
}

// A new skill is trusted, and its provenance and requirements round-trip.
func TestTrustAndProvenanceRoundTrip(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	if _, err := s.Upsert(ctx, &Skill{AgentID: "main", Name: "native", Description: "d", Recipe: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(ctx, &Skill{
		AgentID: "main", Name: "pdf-forms", Description: "d", Recipe: "r",
		Trust: TrustUntrusted, Origin: OriginImport, Version: "1.2",
		RequiresTools: []string{"Bash(pdftk:*)", "Read"},
		Package: &PackageInfo{Source: "/pkgs/pdf-forms", Digest: "abc", License: "MIT",
			Metadata: map[string]string{"author": "x"}, Files: []string{"scripts/fill.py"}, ImportedAt: at},
	}); err != nil {
		t.Fatal(err)
	}
	native, _ := s.GetByName(ctx, "main", "native")
	if native.Trust != TrustTrusted || native.Origin != "" || native.Package != nil || native.RequiresTools != nil {
		t.Errorf("native = %+v", native)
	}
	got, _ := s.GetByName(ctx, "main", "pdf-forms")
	if got.Trust != TrustUntrusted || got.Origin != OriginImport || got.Version != "1.2" {
		t.Errorf("imported = %+v", got)
	}
	if !reflect.DeepEqual(got.RequiresTools, []string{"Bash(pdftk:*)", "Read"}) {
		t.Errorf("requires = %v", got.RequiresTools)
	}
	if got.Package == nil || got.Package.Source != "/pkgs/pdf-forms" || !got.Package.ImportedAt.Equal(at) || got.Package.Files[0] != "scripts/fill.py" {
		t.Errorf("package = %+v", got.Package)
	}
	// A write that leaves the new fields empty keeps them.
	if _, err := s.Upsert(ctx, &Skill{AgentID: "main", Name: "pdf-forms", Description: "d2", Recipe: "r2"}); err != nil {
		t.Fatal(err)
	}
	kept, _ := s.GetByName(ctx, "main", "pdf-forms")
	if kept.Trust != TrustUntrusted || kept.Origin != OriginImport || kept.Version != "1.2" || kept.Package == nil || len(kept.RequiresTools) != 2 {
		t.Errorf("an edit lost provenance: %+v", kept)
	}
	if _, err := s.Upsert(ctx, &Skill{AgentID: "main", Name: "bad", Description: "d", Recipe: "r", Trust: "sort of"}); err == nil {
		t.Error("an unknown trust state was stored")
	}
}

// Only trusted skills reach the agent; trusting one makes it retrievable.
func TestUntrustedSkillsAreNeverRetrieved(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	vec := []float32{1, 0, 0}
	for _, sk := range []*Skill{
		{AgentID: "main", Name: "native", Description: "d", Recipe: "r", Embedding: vec},
		{AgentID: "main", Name: "imported", Description: "d", Recipe: "r", Embedding: vec, Trust: TrustUntrusted, Origin: OriginImport},
	} {
		if _, err := s.Upsert(ctx, sk); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.Search(ctx, Query{AgentID: "main", Embedding: vec, K: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(hits); !reflect.DeepEqual(got, []string{"native"}) {
		t.Fatalf("retrieved %v; an untrusted skill must never be offered", got)
	}
	if ok, err := s.SetTrust(ctx, "main", "imported", TrustTrusted); !ok || err != nil {
		t.Fatalf("SetTrust: %v %v", ok, err)
	}
	hits, _ = s.Search(ctx, Query{AgentID: "main", Embedding: vec, K: 5})
	if len(hits) != 2 {
		t.Errorf("after trusting, retrieved %v", names(hits))
	}
	if ok, err := s.SetTrust(ctx, "main", "missing", TrustTrusted); ok || err != nil {
		t.Errorf("SetTrust on a missing skill: %v %v", ok, err)
	}
	if _, err := s.SetTrust(ctx, "main", "imported", "maybe"); err == nil {
		t.Error("SetTrust accepted an unknown state")
	}
}

// A skill needing a tool that is not available here is not retrieved.
func TestSearchHonoursRequiredTools(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	vec := []float32{0, 1, 0}
	if _, err := s.Upsert(ctx, &Skill{AgentID: "main", Name: "needs-shell", Description: "d", Recipe: "r", Embedding: vec, RequiresTools: []string{"os_exec"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(ctx, &Skill{AgentID: "main", Name: "plain", Description: "d", Recipe: "r", Embedding: vec}); err != nil {
		t.Fatal(err)
	}
	none := func(string) bool { return false }
	hits, _ := s.Search(ctx, Query{AgentID: "main", Embedding: vec, K: 5, Available: none})
	if got := names(hits); !reflect.DeepEqual(got, []string{"plain"}) {
		t.Errorf("without os_exec retrieved %v", got)
	}
	all := func(string) bool { return true }
	hits, _ = s.Search(ctx, Query{AgentID: "main", Embedding: vec, K: 5, Available: all})
	if len(hits) != 2 {
		t.Errorf("with every tool retrieved %v", names(hits))
	}
}

// Widening what a skill requires, without an explicit trust decision, makes it
// untrusted; narrowing it, or leaving it alone, keeps its trust; an explicit
// decision wins.
func TestWideningRequirementsDropsTrust(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	get := func() *Skill {
		sk, _ := s.GetByName(ctx, "main", "x")
		return sk
	}
	up := func(sk *Skill) {
		t.Helper()
		sk.AgentID, sk.Name, sk.Description, sk.Recipe = "main", "x", "d", "r"
		if _, err := s.Upsert(ctx, sk); err != nil {
			t.Fatal(err)
		}
	}
	up(&Skill{RequiresTools: []string{"Read"}})
	if get().Trust != TrustTrusted {
		t.Fatal("a new skill starts trusted")
	}
	up(&Skill{RequiresTools: []string{}})
	if sk := get(); sk.Trust != TrustTrusted || len(sk.RequiresTools) != 0 {
		t.Errorf("narrowing: %+v", sk)
	}
	up(&Skill{})
	if get().Trust != TrustTrusted {
		t.Error("an edit that leaves requirements alone must keep trust")
	}
	up(&Skill{RequiresTools: []string{"Bash(rm:*)"}})
	if sk := get(); sk.Trust != TrustUntrusted {
		t.Errorf("widening kept trust: %+v", sk)
	}
	up(&Skill{RequiresTools: []string{"Bash(rm:*)", "Write"}, Trust: TrustTrusted})
	if get().Trust != TrustTrusted {
		t.Error("an explicit trust decision must win")
	}
}

// Trust decisions are recorded in the history, and a revert restores the trust
// and requirements the version had.
func TestTrustHistoryAndRevert(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	if _, err := s.Upsert(ctx, &Skill{AgentID: "main", Name: "x", Description: "v1", Recipe: "r1", Trust: TrustUntrusted, RequiresTools: []string{"Read"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTrust(ctx, "main", "x", TrustTrusted); err != nil {
		t.Fatal(err)
	}
	// A re-import brings new content and wider requirements, untrusted.
	if _, err := s.UpsertWithSource(ctx, &Skill{AgentID: "main", Name: "x", Description: "v2", Recipe: "r2", Trust: TrustUntrusted, RequiresTools: []string{"Read", "Bash(curl:*)"}}, "import"); err != nil {
		t.Fatal(err)
	}
	hs, err := s.History(ctx, "main", "x")
	if err != nil {
		t.Fatal(err)
	}
	var sources []string
	for _, h := range hs {
		sources = append(sources, h.ChangeSource)
	}
	if got := strings.Join(sources, ","); got != "import,trust" {
		t.Fatalf("history sources = %s", got)
	}
	if hs[0].PriorTrust != TrustTrusted || !reflect.DeepEqual(hs[0].PriorRequiresTools, []string{"Read"}) {
		t.Errorf("the import snapshot = %+v", hs[0])
	}
	// Reverting to the trusted v1 brings back its trust and requirements.
	cur, err := s.RevertTo(ctx, "main", "x", hs[0].Version)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Description != "v1" || cur.Trust != TrustTrusted || !reflect.DeepEqual(cur.RequiresTools, []string{"Read"}) {
		t.Errorf("reverted = %+v", cur)
	}
	if again, _ := s.GetByName(ctx, "main", "x"); again.Trust != TrustTrusted || len(again.RequiresTools) != 1 {
		t.Errorf("stored after revert = %+v", again)
	}
}

func TestEligibleReasons(t *testing.T) {
	base := func() *Skill {
		return &Skill{Status: StatusLive, Enabled: true, Trust: TrustTrusted, RequiresTools: []string{"os_exec", "web_read"}}
	}
	onlyWeb := func(tool string) bool { return tool == "web_read" }
	for name, tc := range map[string]struct {
		mutate func(*Skill)
		want   string
	}{
		"eligible":  {func(*Skill) {}, ""},
		"proposed":  {func(sk *Skill) { sk.Status = StatusProposed }, "proposed"},
		"removed":   {func(sk *Skill) { sk.Status = StatusTombstoned }, "removed"},
		"disabled":  {func(sk *Skill) { sk.Enabled = false }, "disabled"},
		"untrusted": {func(sk *Skill) { sk.Trust = TrustUntrusted }, "untrusted"},
	} {
		sk := base()
		tc.mutate(sk)
		ok, reason := sk.Eligible(nil)
		if ok != (tc.want == "") || !strings.Contains(reason, tc.want) {
			t.Errorf("%s: ok=%v reason=%q", name, ok, reason)
		}
	}
	if ok, reason := base().Eligible(onlyWeb); ok || !strings.Contains(reason, "os_exec") || strings.Contains(reason, "web_read") {
		t.Errorf("missing tool: ok=%v reason=%q", ok, reason)
	}
}

// A skills.db from before TEN-298 opens with its skills trusted, as they were.
func TestOldDatabasesMigrateTrusted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skills.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE skills (id INTEGER PRIMARY KEY AUTOINCREMENT, agent_id TEXT NOT NULL, name TEXT NOT NULL,
			description TEXT NOT NULL, recipe TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'live',
			enabled INTEGER NOT NULL DEFAULT 1, success_count INTEGER NOT NULL DEFAULT 0, embedder_id TEXT,
			embedding TEXT, created_at INTEGER NOT NULL)`,
		`CREATE TABLE skill_history (id INTEGER PRIMARY KEY AUTOINCREMENT, skill_id INTEGER NOT NULL,
			agent_id TEXT NOT NULL, name TEXT NOT NULL, version INTEGER NOT NULL, prior_description TEXT NOT NULL,
			prior_recipe TEXT NOT NULL, prior_status TEXT NOT NULL, change_source TEXT NOT NULL DEFAULT 'operator',
			changed_at INTEGER NOT NULL)`,
		`INSERT INTO skills (agent_id, name, description, recipe, created_at) VALUES ('main', 'old', 'd', 'r', 1)`,
		`INSERT INTO skill_history (skill_id, agent_id, name, version, prior_description, prior_recipe, prior_status, changed_at)
			VALUES (1, 'main', 'old', 1, 'd0', 'r0', 'live', 1)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	old, err := s.GetByName(ctx, "main", "old")
	if err != nil || old == nil {
		t.Fatalf("old skill: %v %v", old, err)
	}
	if old.Trust != TrustTrusted || old.Origin != "" || old.RequiresTools != nil || old.Package != nil {
		t.Errorf("migrated = %+v", old)
	}
	// Its pre-TEN-298 history still reverts, keeping the current trust.
	cur, err := s.RevertTo(ctx, "main", "old", 1)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Description != "d0" || cur.Trust != TrustTrusted {
		t.Errorf("reverted = %+v", cur)
	}
}
