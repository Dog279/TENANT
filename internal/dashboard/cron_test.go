package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeCronCtl struct {
	jobs        []CronJobView
	addCalls    int
	addName     string
	addSpec     string
	addPrompt   string
	addKind     string
	addExec     bool
	addTZ       string
	addErr      error
	removeCalls []string
	enableCalls []string // "id:true"/"id:false"
	runCalls    []string
}

func (f *fakeCronCtl) Jobs() []CronJobView { return f.jobs }
func (f *fakeCronCtl) Add(s CronAddSpec) error {
	f.addCalls++
	f.addName, f.addSpec, f.addPrompt, f.addKind, f.addExec, f.addTZ = s.Name, s.Spec, s.Prompt, s.Kind, s.Exec, s.TZ
	return f.addErr
}
func (f *fakeCronCtl) Remove(id string) error { f.removeCalls = append(f.removeCalls, id); return nil }
func (f *fakeCronCtl) SetEnabled(id string, on bool) error {
	v := "false"
	if on {
		v = "true"
	}
	f.enableCalls = append(f.enableCalls, id+":"+v)
	return nil
}
func (f *fakeCronCtl) RunNow(id string) error { f.runCalls = append(f.runCalls, id); return nil }

func cronServer(fc CronControl) *Server {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if fc != nil {
		s.SetCron(fc)
	}
	return s
}

func cronPostForm(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestCron_PageNotConfigured(t *testing.T) {
	s := cronServer(nil) // cron control never set
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/cron", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "isn't configured") {
		t.Errorf("nil cron should render a not-configured notice; got:\n%s", rec.Body.String())
	}
}

func TestCron_PageListsJobs(t *testing.T) {
	fc := &fakeCronCtl{jobs: []CronJobView{
		{ID: "abc", Name: "nightly", Spec: "0 9 * * *", Prompt: "run the tests", Enabled: true, NextRun: "2026-06-08 09:00", LastStatus: "ok"},
	}}
	s := cronServer(fc)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/cron", nil))
	body := rec.Body.String()
	for _, want := range []string{"nightly", "0 9 * * *", "run the tests", "abc", "Disable", "Run now"} {
		if !strings.Contains(body, want) {
			t.Errorf("cron page missing %q in body", want)
		}
	}
}

// A hostile job name/prompt must be HTML-escaped (html/template), never
// reflected as live markup.
func TestCron_PageEscapesJobFields(t *testing.T) {
	fc := &fakeCronCtl{jobs: []CronJobView{
		{ID: "x", Name: `<img src=x onerror=alert(1)>`, Spec: "@daily", Prompt: `</script><b>pwn</b>`, Enabled: true},
	}}
	s := cronServer(fc)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/cron", nil))
	body := rec.Body.String()
	if strings.Contains(body, "<img src=x onerror=") {
		t.Error("job name rendered as raw markup — XSS hole")
	}
	if strings.Contains(body, "<b>pwn</b>") {
		t.Error("job prompt rendered as raw markup — XSS hole")
	}
	if !strings.Contains(body, "&lt;img") {
		t.Errorf("expected escaped job name in body:\n%s", body)
	}
}

func TestCron_AddForm(t *testing.T) {
	fc := &fakeCronCtl{}
	s := cronServer(fc)
	rec := cronPostForm(t, s, "/cron/add", "name=nightly&spec=0+9+*+*+*&prompt=run+tests")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	if fc.addCalls != 1 || fc.addSpec != "0 9 * * *" || fc.addPrompt != "run tests" || fc.addName != "nightly" {
		t.Errorf("add not wired: calls=%d name=%q spec=%q prompt=%q", fc.addCalls, fc.addName, fc.addSpec, fc.addPrompt)
	}
}

func TestCron_AddFormThreadsKindExecTZ(t *testing.T) {
	fc := &fakeCronCtl{}
	s := cronServer(fc)
	rec := cronPostForm(t, s, "/cron/add", "spec=@daily&prompt=go+test&kind=shell&exec=true&tz=UTC")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	if fc.addKind != "shell" || !fc.addExec || fc.addTZ != "UTC" {
		t.Errorf("kind/exec/tz not threaded: kind=%q exec=%v tz=%q", fc.addKind, fc.addExec, fc.addTZ)
	}
}

func TestCron_AddFormRequiresFields(t *testing.T) {
	fc := &fakeCronCtl{}
	s := cronServer(fc)
	rec := cronPostForm(t, s, "/cron/add", "spec=&prompt=")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303 redirect, got %d", rec.Code)
	}
	if fc.addCalls != 0 {
		t.Error("empty spec/prompt must not reach the control")
	}
	if !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Errorf("expected an error flash redirect, got Location=%q", rec.Header().Get("Location"))
	}
}

func TestCron_EnableDeleteRunForms(t *testing.T) {
	fc := &fakeCronCtl{}
	s := cronServer(fc)

	if rec := cronPostForm(t, s, "/cron/abc/enable", "enabled=false"); rec.Code != http.StatusSeeOther {
		t.Fatalf("enable: want 303, got %d", rec.Code)
	}
	if len(fc.enableCalls) != 1 || fc.enableCalls[0] != "abc:false" {
		t.Errorf("enable calls = %v", fc.enableCalls)
	}
	if rec := cronPostForm(t, s, "/cron/abc/run", ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("run: want 303, got %d", rec.Code)
	}
	if len(fc.runCalls) != 1 || fc.runCalls[0] != "abc" {
		t.Errorf("run calls = %v", fc.runCalls)
	}
	if rec := cronPostForm(t, s, "/cron/abc/delete", ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: want 303, got %d", rec.Code)
	}
	if len(fc.removeCalls) != 1 || fc.removeCalls[0] != "abc" {
		t.Errorf("remove calls = %v", fc.removeCalls)
	}
}

// The nil-guarded form handlers must not panic when cron is unconfigured.
func TestCron_FormsNilSafe(t *testing.T) {
	s := cronServer(nil)
	if rec := cronPostForm(t, s, "/cron/add", "spec=@daily&prompt=x"); rec.Code != http.StatusSeeOther {
		t.Errorf("nil cron add: want 303, got %d", rec.Code)
	}
}
