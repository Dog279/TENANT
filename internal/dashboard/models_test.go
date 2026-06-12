package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

type fakeModelCtl struct {
	models      []ModelView
	ceiling     int
	useCalls    []string
	addCalls    []string // "kind|key"
	removeCalls []string
	ceilingSet  int
	reloadCalls int
}

func (f *fakeModelCtl) Models() []ModelView { return f.models }
func (f *fakeModelCtl) Use(name string) (string, error) {
	f.useCalls = append(f.useCalls, name)
	return "switched to " + name, nil
}
func (f *fakeModelCtl) AddCloud(kind, key string) (string, error) {
	f.addCalls = append(f.addCalls, kind+"|"+key)
	return "added " + kind, nil
}
func (f *fakeModelCtl) Remove(name string) (string, error) {
	f.removeCalls = append(f.removeCalls, name)
	return "removed " + name, nil
}
func (f *fakeModelCtl) LoopCeiling() int { return f.ceiling }
func (f *fakeModelCtl) SetLoopCeiling(n int) (string, error) {
	f.ceilingSet = n
	return "ceiling set", nil
}
func (f *fakeModelCtl) ReloadKeys() (string, error) { f.reloadCalls++; return "keys reloaded", nil }

func modelServer(fc ModelControl) *Server {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if fc != nil {
		s.SetModels(fc)
	}
	return s
}

func TestModels_PageNotConfigured(t *testing.T) {
	rec := get(t, modelServer(nil), "/models")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "aren't configured") {
		t.Errorf("nil models should render not-configured; got %d:\n%s", rec.Code, rec.Body.String())
	}
}

func TestModels_ListsBackendsAndMarksActive(t *testing.T) {
	fc := &fakeModelCtl{ceiling: 30, models: []ModelView{
		{Name: "zai", Kind: "openai", Model: "glm-4.6", Active: true},
		{Name: "local", Kind: "vllm", Model: "qwen", Active: false},
	}}
	body := get(t, modelServer(fc), "/models").Body.String()
	for _, want := range []string{"zai", "glm-4.6", "active", "local", "qwen", "Add a cloud model", "30"} {
		if !strings.Contains(body, want) {
			t.Errorf("models page missing %q:\n%s", want, body)
		}
	}
	// the active model must NOT offer a "Use this model" button or a Remove
	if strings.Count(body, "Use this model") != 1 {
		t.Errorf("exactly the one inactive model should offer Use; got %d", strings.Count(body, "Use this model"))
	}
}

func TestModels_SwitchRequiresConfirm(t *testing.T) {
	fc := &fakeModelCtl{}
	s := modelServer(fc)
	// Without confirm=yes, the swap must NOT happen.
	submitForm(t, s, "/models/use", "name=local")
	if len(fc.useCalls) != 0 {
		t.Errorf("switch without confirm should not call Use; got %v", fc.useCalls)
	}
	// With confirm=yes it goes through.
	submitForm(t, s, "/models/use", "name=local&confirm=yes")
	if len(fc.useCalls) != 1 || fc.useCalls[0] != "local" {
		t.Errorf("confirmed switch calls = %v", fc.useCalls)
	}
}

func TestModels_AddRemoveCeilingReload(t *testing.T) {
	fc := &fakeModelCtl{}
	s := modelServer(fc)
	submitForm(t, s, "/models/add", "kind=anthropic&key=sk-test")
	if len(fc.addCalls) != 1 || fc.addCalls[0] != "anthropic|sk-test" {
		t.Errorf("add calls = %v", fc.addCalls)
	}
	submitForm(t, s, "/models/remove", "name=old")
	if len(fc.removeCalls) != 1 || fc.removeCalls[0] != "old" {
		t.Errorf("remove calls = %v", fc.removeCalls)
	}
	submitForm(t, s, "/models/ceiling", "ceiling=42")
	if fc.ceilingSet != 42 {
		t.Errorf("ceiling set = %d, want 42", fc.ceilingSet)
	}
	submitForm(t, s, "/models/reload", "")
	if fc.reloadCalls != 1 {
		t.Errorf("reload calls = %d, want 1", fc.reloadCalls)
	}
}

func TestModels_AddRejectsEmpty(t *testing.T) {
	fc := &fakeModelCtl{}
	submitForm(t, modelServer(fc), "/models/add", "kind=&key=")
	if len(fc.addCalls) != 0 {
		t.Errorf("empty add should be rejected; got %v", fc.addCalls)
	}
}

func TestModels_CeilingRejectsNonPositive(t *testing.T) {
	fc := &fakeModelCtl{}
	s := modelServer(fc)
	submitForm(t, s, "/models/ceiling", "ceiling=0")
	submitForm(t, s, "/models/ceiling", "ceiling=abc")
	if fc.ceilingSet != 0 {
		t.Errorf("invalid ceiling should be rejected; got %d", fc.ceilingSet)
	}
}

func TestModels_MutationsNilSafe(t *testing.T) {
	s := modelServer(nil)
	for _, p := range []string{"/models/use", "/models/add", "/models/remove", "/models/ceiling", "/models/reload"} {
		if rec := submitForm(t, s, p, "name=x&kind=zai&key=k&ceiling=5&confirm=yes"); rec.Code != http.StatusSeeOther {
			t.Errorf("%s with nil control: want 303, got %d", p, rec.Code)
		}
	}
}

func TestHome_ShowsActiveModel(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetModels(&fakeModelCtl{models: []ModelView{{Name: "zai", Model: "glm-4.6", Active: true}}})
	body := get(t, s, "/").Body.String()
	if !strings.Contains(body, "Model") || !strings.Contains(body, "zai · glm-4.6") {
		t.Errorf("home should show the active model:\n%s", body)
	}
}
