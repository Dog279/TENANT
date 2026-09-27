package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeSkills is a SkillControl that records the library calls /skills makes.
type fakeSkills struct {
	list       []SkillInfo
	detail     *SkillDetail
	imports    []string // "path:trust"
	exports    []string // "name:dir"
	trustCalls []string // "name:trusted"
	importErr  error
}

func (f *fakeSkills) SkillList() []SkillInfo                           { return f.list }
func (f *fakeSkills) AddSkill(string, string, string) error            { return nil }
func (f *fakeSkills) SetSkillEnabled(string, bool) (bool, error)       { return true, nil }
func (f *fakeSkills) ForgetSkill(string) (bool, error)                 { return true, nil }
func (f *fakeSkills) AcceptSkill(string) (bool, error)                 { return true, nil }
func (f *fakeSkills) SkillHistory(string) ([]SkillHistoryEntry, error) { return nil, nil }
func (f *fakeSkills) SkillCurrent(string) (*SkillSnapshot, error)      { return nil, nil }
func (f *fakeSkills) SkillRevert(string, int) error                    { return nil }
func (f *fakeSkills) AutoAcceptMode() string                           { return "off" }
func (f *fakeSkills) SetAutoAccept(string) error                       { return nil }
func (f *fakeSkills) ImportSkill(path string, trust bool) (string, error) {
	if f.importErr != nil {
		return "", f.importErr
	}
	f.imports = append(f.imports, path+":"+map[bool]string{true: "trust", false: "untrusted"}[trust])
	return "imported", nil
}
func (f *fakeSkills) ExportSkill(name, dir string) (string, error) {
	f.exports = append(f.exports, name+":"+dir)
	return dir + "/" + name, nil
}
func (f *fakeSkills) SetSkillTrust(name string, trusted bool) (bool, error) {
	f.trustCalls = append(f.trustCalls, name+":"+map[bool]string{true: "trust", false: "untrust"}[trusted])
	return name != "missing", nil
}
func (f *fakeSkills) SkillDetail(name string) (*SkillDetail, error) {
	if f.detail == nil || f.detail.Name != name {
		return nil, nil
	}
	return f.detail, nil
}

func skillsModel(f *fakeSkills) *model {
	return newModel(context.Background(), Config{Skills: f})
}

// The list marks untrusted skills and says who wrote them, and how to act.
func TestSkillsListMarksUntrusted(t *testing.T) {
	f := &fakeSkills{list: []SkillInfo{
		{Name: "deploy", Description: "how we deploy", Enabled: true, Status: "live", Trusted: true, Origin: "operator"},
		{Name: "habit", Description: "a habit", Enabled: true, Status: "live", Origin: "agent"},
		{Name: "pdf-forms", Description: "pdf", Enabled: true, Status: "live", Origin: "import"},
	}}
	out, _ := skillsModel(f).handleSkills("")
	for _, want := range []string{"habit [untrusted, saved by the agent]", "pdf-forms [untrusted, imported]",
		"2 untrusted: not offered to the agent until you review and trust them"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "deploy [untrusted") {
		t.Errorf("a trusted skill is marked untrusted:\n%s", out)
	}
}

func TestSkillsImportExportTrust(t *testing.T) {
	f := &fakeSkills{}
	m := skillsModel(f)
	cases := []struct{ arg, want string }{
		{"import", "usage: /skills import"},
		{"import ./pkgs/pdf-forms", "imported"},
		{"import ./pkgs/pdf-forms --trust", "imported"},
		{"export pdf-forms", "usage: /skills export"},
		{"export pdf-forms ./out", "exported skill pdf-forms to ./out/pdf-forms"},
		{"trust habit", "trusted skill habit"},
		{"untrust habit", "skill habit is untrusted"},
		{"trust missing", "no skill named missing"},
		{"trust", "usage: /skills trust"},
		{"frobnicate", "import/export/trust/untrust/show"},
	}
	for _, tc := range cases {
		out, _ := m.handleSkills(tc.arg)
		if !strings.Contains(out, tc.want) {
			t.Errorf("/skills %s = %q, want %q", tc.arg, out, tc.want)
		}
	}
	if got := strings.Join(f.imports, ","); got != "./pkgs/pdf-forms:untrusted,./pkgs/pdf-forms:trust" {
		t.Errorf("imports = %s", got)
	}
	if got := strings.Join(f.exports, ","); got != "pdf-forms:./out" {
		t.Errorf("exports = %s", got)
	}
	if got := strings.Join(f.trustCalls, ","); got != "habit:trust,habit:untrust,missing:trust" {
		t.Errorf("trust calls = %s", got)
	}
	f.importErr = errors.New("no SKILL.md")
	if out, _ := m.handleSkills("import ./x"); !strings.Contains(out, "import failed: no SKILL.md") {
		t.Errorf("import error = %q", out)
	}
}

// /skills show gives what the operator needs to decide: trust, origin, the
// package, the tools it expects (which approve nothing) and the instructions.
func TestSkillsShow(t *testing.T) {
	f := &fakeSkills{detail: &SkillDetail{
		Name: "pdf-forms", Description: "fill PDF forms", Recipe: "1. list fields", Status: "live", Enabled: true,
		Origin: "import", Version: "2.1", RequiresTools: []string{"Bash(pdftk:*)"}, NotOffered: "untrusted: review it, then trust it",
		Source: "/pkgs/pdf-forms", Digest: "abc123", License: "MIT", Files: []string{"scripts/fill.py"},
		ImportedAt: time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC),
	}}
	m := skillsModel(f)
	out, _ := m.handleSkills("show pdf-forms")
	for _, want := range []string{"trust: untrusted", "origin: import", "version 2.1", "not offered to the agent: untrusted",
		"expects tools: Bash(pdftk:*)", "approves nothing", "imported from /pkgs/pdf-forms", "sha256 abc123",
		"license: MIT", "not installed: scripts/fill.py", "1. list fields", "/skills trust pdf-forms"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	if out, _ := m.handleSkills("show nope"); out != "no skill named nope" {
		t.Errorf("show unknown = %q", out)
	}
	f.detail.Trusted, f.detail.NotOffered = true, ""
	if out, _ := m.handleSkills("show pdf-forms"); !strings.Contains(out, "offered to the agent when relevant") || strings.Contains(out, "/skills trust") {
		t.Errorf("trusted show:\n%s", out)
	}
}
