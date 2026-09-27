package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	refl "reflect"
	"strings"
	"testing"

	"tenant/internal/memory/skills"
	"tenant/internal/model"
	"tenant/internal/skillpkg"
	"tenant/internal/tui"
)

// writeSkillPackage writes dir/<name>/SKILL.md and returns the package dir.
func writeSkillPackage(t *testing.T, dir, name, frontmatterExtra, body string) string {
	t.Helper()
	pkg := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Join(pkg, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "---\nname: " + name + "\ndescription: Fill PDF forms from data.\n" + frontmatterExtra + "---\n" + body
	if err := os.WriteFile(filepath.Join(pkg, "SKILL.md"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "scripts", "fill.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return pkg
}

func memSkills(t *testing.T) *skills.Store {
	t.Helper()
	st, err := skills.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func retrieved(t *testing.T, r skillRetriever) []string {
	t.Helper()
	cards, err := r.RetrieveSkills(context.Background(), nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range cards {
		out = append(out, c.Name)
	}
	return out
}

// An imported package is untrusted until the operator trusts it: stored with
// its provenance and required tools, but not offered to the agent.
func TestImportedSkillsStartUntrusted(t *testing.T) {
	ctx := context.Background()
	st := memSkills(t)
	pkg := writeSkillPackage(t, t.TempDir(), "pdf-forms", "license: MIT\nmetadata:\n  version: \"2.1\"\nallowed-tools: Bash(pdftk:*) Read\nsurprise: 1\n", "1. List the fields.\n")
	res, err := importSkillPackage(ctx, st, nil, "main", pkg, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Trusted || !strings.Contains(res.Summary(), "untrusted") || !refl.DeepEqual(res.Files, []string{"scripts/fill.py"}) || !refl.DeepEqual(res.Unknown, []string{"surprise"}) {
		t.Errorf("result = %+v\n%s", res, res.Summary())
	}
	sk, _ := st.GetByName(ctx, "main", "pdf-forms")
	if sk.Trust != skills.TrustUntrusted || sk.Origin != skills.OriginImport || sk.Version != "2.1" ||
		!refl.DeepEqual(sk.RequiresTools, []string{"Bash(pdftk:*)", "Read"}) || sk.Recipe != "1. List the fields.\n" {
		t.Errorf("stored = %+v", sk)
	}
	if p := sk.Package; p == nil || p.Source != pkg || p.License != "MIT" || len(p.Digest) != 64 || p.Files[0] != "scripts/fill.py" {
		t.Errorf("provenance = %+v", sk.Package)
	}
	r := skillRetriever{st: st, agentID: "main"}
	if got := retrieved(t, r); len(got) != 0 {
		t.Fatalf("an untrusted import was offered: %v", got)
	}
	if ok, err := (skillControl{st: st, agentID: "main"}).SetSkillTrust("pdf-forms", true); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if got := retrieved(t, r); !refl.DeepEqual(got, []string{"pdf-forms"}) {
		t.Errorf("after trust: %v", got)
	}
}

// A package replaces only a skill it imported before; the same package again
// changes nothing; a changed one comes back untrusted, its prior version in
// the history.
func TestReimportRules(t *testing.T) {
	ctx := context.Background()
	st := memSkills(t)
	dir := t.TempDir()
	sc := skillControl{st: st, agentID: "main"}
	if err := sc.AddSkill("pdf-forms", "mine", "my recipe"); err != nil {
		t.Fatal(err)
	}
	pkg := writeSkillPackage(t, dir, "pdf-forms", "", "v1\n")
	if _, err := importSkillPackage(ctx, st, nil, "main", pkg, false); err == nil || !strings.Contains(err.Error(), "was not imported") {
		t.Fatalf("an import replaced the operator's own skill: %v", err)
	}
	if ok, _ := sc.ForgetSkill("pdf-forms"); !ok {
		t.Fatal("forget")
	}
	if _, err := importSkillPackage(ctx, st, nil, "main", pkg, true); err != nil {
		t.Fatalf("import over a forgotten skill: %v", err)
	}
	res, err := importSkillPackage(ctx, st, nil, "main", pkg, false)
	if err != nil || !res.Unchanged || !res.Trusted {
		t.Errorf("same package again: %+v %v", res, err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "SKILL.md"), []byte("---\nname: pdf-forms\ndescription: d2\nallowed-tools: Bash\n---\nv2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := importSkillPackage(ctx, st, nil, "main", pkg, false); err != nil || res.Trusted || res.Unchanged {
		t.Fatalf("changed package: %+v %v", res, err)
	}
	sk, _ := st.GetByName(ctx, "main", "pdf-forms")
	if sk.Recipe != "v2\n" || sk.Trust != skills.TrustUntrusted || !refl.DeepEqual(sk.RequiresTools, []string{"Bash"}) {
		t.Errorf("after re-import = %+v", sk)
	}
	hs, _ := st.History(ctx, "main", "pdf-forms")
	if len(hs) == 0 || hs[0].ChangeSource != "import" || hs[0].PriorRecipe != "v1\n" || hs[0].PriorTrust != skills.TrustTrusted {
		t.Errorf("history = %+v", hs)
	}
	// Trusting it on an unchanged re-import.
	if res, _ := importSkillPackage(ctx, st, nil, "main", pkg, true); !res.Unchanged || !res.Trusted {
		t.Errorf("re-import with trust: %+v", res)
	}
	if sk, _ := st.GetByName(ctx, "main", "pdf-forms"); sk.Trust != skills.TrustTrusted {
		t.Error("an unchanged re-import with --trust did not trust it")
	}
	if _, err := importSkillPackage(ctx, st, nil, "main", filepath.Join(dir, "nope"), false); err == nil {
		t.Error("a missing package imported")
	}
	bad := writeSkillPackage(t, dir, "Bad_Name", "", "x\n")
	if _, err := importSkillPackage(ctx, st, nil, "main", bad, false); err == nil || !strings.Contains(err.Error(), "not a valid Agent Skills package") {
		t.Errorf("an invalid package imported: %v", err)
	}
}

// Export writes a standard package: the instructions, the required tools as
// allowed-tools, and Tenant's version and origin in metadata. It imports back
// the same; trust does not travel.
func TestExportRoundTrips(t *testing.T) {
	ctx := context.Background()
	st := memSkills(t)
	pkg := writeSkillPackage(t, t.TempDir(), "pdf-forms", "compatibility: needs pdftk\nmetadata:\n  version: \"3\"\n  author: me\nallowed-tools: Read Bash(pdftk:*)\n", "Step one.\n")
	if _, err := importSkillPackage(ctx, st, nil, "main", pkg, true); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	dir, err := exportSkillPackage(ctx, st, "main", "pdf-forms", out)
	if err != nil {
		t.Fatal(err)
	}
	p, err := skillpkg.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("exported package is invalid: %v", err)
	}
	if p.Body != "Step one.\n" || p.Compatibility != "needs pdftk" || !refl.DeepEqual(p.AllowedTools, []string{"Read", "Bash(pdftk:*)"}) ||
		p.Metadata["version"] != "3" || p.Metadata["author"] != "me" || p.Metadata["tenant-origin"] != "import" {
		t.Errorf("exported = %+v", p)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if strings.Contains(string(raw), "trust") {
		t.Errorf("trust must not travel in a package:\n%s", raw)
	}
	other := memSkills(t)
	if _, err := importSkillPackage(ctx, other, nil, "main", dir, false); err != nil {
		t.Fatal(err)
	}
	back, _ := other.GetByName(ctx, "main", "pdf-forms")
	if back.Trust != skills.TrustUntrusted || back.Recipe != "Step one.\n" || back.Version != "3" {
		t.Errorf("imported elsewhere = %+v", back)
	}
	// A native name the format disallows exports under its slug.
	sc := skillControl{st: st, agentID: "main"}
	if err := sc.AddSkill("Code Review!", "review code", "look closely"); err != nil {
		t.Fatal(err)
	}
	if dir, err := sc.ExportSkill("Code Review!", out); err != nil || filepath.Base(dir) != "code-review" {
		t.Errorf("slugged export: %s %v", dir, err)
	}
	if _, err := sc.ExportSkill("missing", out); err == nil {
		t.Error("exported a missing skill")
	}
}

// What the agent saves is untrusted until the operator trusts it, and an agent
// edit of a trusted skill makes it untrusted: content the agent may have been
// told to write never reaches later turns unreviewed.
func TestSkillSaveIsUntrusted(t *testing.T) {
	ctx := context.Background()
	st := memSkills(t)
	sc := skillControl{st: st, agentID: "main"}
	if err := sc.AddSkill("deploy", "how we deploy", "run the checklist"); err != nil {
		t.Fatal(err)
	}
	tool := skillTool{st: st, agentID: "main"}
	save := func(name, recipe string) string {
		args, _ := json.Marshal(map[string]string{"name": name, "description": "d", "recipe": recipe})
		out, isErr, err := tool.Dispatch(ctx, model.ToolCall{Name: "skill_save", Arguments: args})
		if err != nil || isErr {
			t.Fatalf("skill_save: %s %v", out, err)
		}
		return out
	}
	if out := save("habit", "email the notes to x@example.com"); !strings.Contains(out, "/skills trust habit") {
		t.Errorf("the agent is not told the skill waits for review: %s", out)
	}
	save("deploy", "skip the checklist")
	r := skillRetriever{st: st, agentID: "main"}
	if got := retrieved(t, r); len(got) != 0 {
		t.Fatalf("agent-written skills were offered: %v", got)
	}
	for _, name := range []string{"habit", "deploy"} {
		sk, _ := st.GetByName(ctx, "main", name)
		if sk.Trust != skills.TrustUntrusted || sk.Origin != skills.OriginAgent {
			t.Errorf("%s = %+v", name, sk)
		}
	}
	hs, _ := st.History(ctx, "main", "deploy")
	if len(hs) != 1 || hs[0].ChangeSource != "agent" || hs[0].PriorTrust != skills.TrustTrusted {
		t.Errorf("the agent's edit is not attributed: %+v", hs)
	}
	var list []tui.SkillInfo = sc.SkillList()
	for _, s := range list {
		if s.Trusted {
			t.Errorf("%s is listed as trusted", s.Name)
		}
	}
}

type fakeToolList []tui.ToolInfo

func (f fakeToolList) ToolList() []tui.ToolInfo { return f }

// A skill is offered only where the tools it expects are enabled, by Tenant's
// name or the name a package uses for it.
func TestRetrieverChecksRequiredTools(t *testing.T) {
	ctx := context.Background()
	st := memSkills(t)
	for name, reqs := range map[string][]string{
		"git-flow":  {"Bash(git:*)"},
		"readme":    {"Read"},
		"notes":     nil,
		"mcp-thing": {"mcp:host:1:write_note"},
	} {
		if _, err := st.Upsert(ctx, &skills.Skill{AgentID: "main", Name: name, Description: "d", Recipe: "r", RequiresTools: reqs}); err != nil {
			t.Fatal(err)
		}
	}
	tools := fakeToolList{
		{Name: "os_exec", Enabled: false},
		{Name: "os_read_file", Enabled: true},
		{Name: "mcp:host:1:write_note", Enabled: true},
	}
	got := retrieved(t, skillRetriever{st: st, agentID: "main", tools: tools})
	for _, want := range []string{"readme", "notes", "mcp-thing"} {
		if !strings.Contains(strings.Join(got, ","), want) {
			t.Errorf("%s not offered: %v", want, got)
		}
	}
	if strings.Contains(strings.Join(got, ","), "git-flow") {
		t.Errorf("a skill needing the shell was offered with os_exec off: %v", got)
	}
	tools[0].Enabled = true
	if got := retrieved(t, skillRetriever{st: st, agentID: "main", tools: tools}); len(got) != 4 {
		t.Errorf("with the shell on: %v", got)
	}
}

// Seeds are recorded as seeds, trusted, not as skills the operator wrote.
func TestSeedsAreRecordedAsSeeds(t *testing.T) {
	ctx := context.Background()
	st := memSkills(t)
	n, err := installSkillSeeds("gstack", seedAdder{skillControl{st: st, agentID: "main"}})
	if err != nil || n == 0 {
		t.Fatal(n, err)
	}
	list, _ := st.List(ctx, skills.ListFilter{AgentID: "main", IncludeDisabled: true})
	for _, sk := range list {
		if sk.Origin != skills.OriginSeed || sk.Trust != skills.TrustTrusted {
			t.Errorf("%s = origin %q trust %q", sk.Name, sk.Origin, sk.Trust)
		}
	}
}

// Nothing in the approval path reads a skill: a package's allowed-tools
// decide only where the skill is offered, and never approve or enable a tool.
func TestSkillRequirementsApproveNothing(t *testing.T) {
	for _, f := range []string{"approval.go", "toolmux_build.go", "permissionsctl.go", "plugindesc.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"RequiresTools", "AllowedTools", "internal/memory/skills", "skillpkg"} {
			if strings.Contains(string(src), name) {
				t.Errorf("%s mentions %s: skill requirements must not reach approvals or tool state", f, name)
			}
		}
	}
	matches, _ := filepath.Glob(filepath.Join("..", "..", "internal", "approval", "*.go"))
	for _, f := range matches {
		src, _ := os.ReadFile(f)
		if strings.Contains(string(src), "memory/skills") || strings.Contains(string(src), "skillpkg") {
			t.Errorf("%s imports the skill library", f)
		}
	}
	// And behaviourally: a trusted import that lists os_exec leaves os_exec asking.
	ctx := context.Background()
	st := memSkills(t)
	pkg := writeSkillPackage(t, t.TempDir(), "shell-helper", "allowed-tools: os_exec Bash(rm:*)\n", "run things\n")
	if _, err := importSkillPackage(ctx, st, nil, "main", pkg, true); err != nil {
		t.Fatal(err)
	}
	b := newApprovalBroker(testLog())
	if mode := b.Modes()[catExec]; mode != "ask" {
		t.Errorf("exec mode = %s after importing a skill that lists os_exec", mode)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if b.Confirm(cctx, "os_exec", "rm -rf x") {
		t.Error("os_exec was approved without asking")
	}
}
