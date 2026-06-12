package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

type fakeSkillCtl struct {
	skills      []SkillView
	autoMode    string
	acceptCalls []string
	enableCalls []string // "name:true"/"name:false"
	forgetCalls []string
	autoSet     string
}

func (f *fakeSkillCtl) Skills() []SkillView { return f.skills }
func (f *fakeSkillCtl) Accept(name string) (bool, error) {
	f.acceptCalls = append(f.acceptCalls, name)
	return true, nil
}
func (f *fakeSkillCtl) SetEnabled(name string, on bool) (bool, error) {
	v := "false"
	if on {
		v = "true"
	}
	f.enableCalls = append(f.enableCalls, name+":"+v)
	return true, nil
}
func (f *fakeSkillCtl) Forget(name string) (bool, error) {
	f.forgetCalls = append(f.forgetCalls, name)
	return true, nil
}
func (f *fakeSkillCtl) AutoAcceptMode() string { return f.autoMode }
func (f *fakeSkillCtl) SetAutoAccept(m string) error {
	f.autoSet = m
	return nil
}

func skillServer(fc SkillControl) *Server {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if fc != nil {
		s.SetSkills(fc)
	}
	return s
}

func TestSkills_PageNotConfigured(t *testing.T) {
	rec := get(t, skillServer(nil), "/skills")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "isn't configured") {
		t.Errorf("nil skills should render not-configured; got %d:\n%s", rec.Code, rec.Body.String())
	}
}

func TestSkills_GroupsProposedAndLive_HidesTombstoned(t *testing.T) {
	fc := &fakeSkillCtl{
		autoMode: "trusted",
		skills: []SkillView{
			{Name: "summarize-pr", Description: "summarize a pull request", Status: "live", Enabled: true},
			{Name: "triage-bug", Description: "triage an incoming bug", Status: "proposed"},
			{Name: "old-thing", Description: "dead", Status: "tombstoned"},
		},
	}
	body := get(t, skillServer(fc), "/skills").Body.String()
	if !strings.Contains(body, "Waiting for your OK (1)") {
		t.Errorf("proposed skill should appear in the waiting section:\n%s", body)
	}
	if !strings.Contains(body, "summarize-pr") {
		t.Error("live skill should be listed")
	}
	if strings.Contains(body, "old-thing") {
		t.Error("tombstoned skill must not render")
	}
	if !strings.Contains(body, `value="trusted" checked`) {
		t.Error("current auto-accept mode should be pre-selected")
	}
}

func TestSkills_AcceptEnableForget(t *testing.T) {
	fc := &fakeSkillCtl{}
	s := skillServer(fc)
	submitForm(t, s, "/skills/triage-bug/accept", "")
	if len(fc.acceptCalls) != 1 || fc.acceptCalls[0] != "triage-bug" {
		t.Errorf("accept calls = %v", fc.acceptCalls)
	}
	submitForm(t, s, "/skills/summarize-pr/enable", "enabled=false")
	if len(fc.enableCalls) != 1 || fc.enableCalls[0] != "summarize-pr:false" {
		t.Errorf("enable calls = %v", fc.enableCalls)
	}
	submitForm(t, s, "/skills/old/forget", "")
	if len(fc.forgetCalls) != 1 {
		t.Errorf("forget calls = %v", fc.forgetCalls)
	}
}

func TestSkills_AutoAcceptForm(t *testing.T) {
	fc := &fakeSkillCtl{}
	rec := submitForm(t, skillServer(fc), "/skills/auto", "mode=trusted")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	if fc.autoSet != "trusted" {
		t.Errorf("SetAutoAccept got %q, want trusted", fc.autoSet)
	}
}

func TestSkills_MutationsNilSafe(t *testing.T) {
	s := skillServer(nil)
	for _, p := range []string{"/skills/x/accept", "/skills/x/enable", "/skills/x/forget", "/skills/auto"} {
		if rec := submitForm(t, s, p, "mode=on&enabled=true"); rec.Code != http.StatusSeeOther {
			t.Errorf("%s with nil control: want 303, got %d", p, rec.Code)
		}
	}
}
