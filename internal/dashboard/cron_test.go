package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeCronCtl's jobs change as the real control's do (an added job is
// listed, a removed one goes, a job turned off shows off), under mu, so a
// browser test's server and the test can share it.
type fakeCronCtl struct {
	mu          sync.Mutex
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
	execOn      bool
	execCalls   []bool
	execErr     error
}

func (f *fakeCronCtl) Jobs() []CronJobView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.jobs)
}
func (f *fakeCronCtl) Add(s CronAddSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addCalls++
	f.addName, f.addSpec, f.addPrompt, f.addKind, f.addExec, f.addTZ = s.Name, s.Spec, s.Prompt, s.Kind, s.Exec, s.TZ
	if f.addErr == nil {
		f.jobs = append(f.jobs, CronJobView{ID: fmt.Sprintf("new%d", f.addCalls), Name: s.Name, Spec: s.Spec, Prompt: s.Prompt, Enabled: true})
	}
	return f.addErr
}
func (f *fakeCronCtl) Remove(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls = append(f.removeCalls, id)
	f.jobs = slices.DeleteFunc(f.jobs, func(j CronJobView) bool { return j.ID == id })
	return nil
}
func (f *fakeCronCtl) SetEnabled(id string, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enableCalls = append(f.enableCalls, fmt.Sprintf("%s:%t", id, on))
	for i := range f.jobs {
		if f.jobs[i].ID == id {
			f.jobs[i].Enabled = on
		}
	}
	return nil
}
func (f *fakeCronCtl) RunNow(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCalls = append(f.runCalls, id)
	return nil
}
func (f *fakeCronCtl) ExecEnabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execOn
}
func (f *fakeCronCtl) SetExec(on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.execErr != nil {
		return f.execErr
	}
	f.execCalls = append(f.execCalls, on)
	f.execOn = on
	return nil
}

// set runs change under the lock, for a test sharing f with a server.
func (f *fakeCronCtl) set(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

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
	if !strings.Contains(rec.Body.String(), "Scheduled jobs aren&#39;t available on this Tenant.") {
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
	for _, want := range []string{"nightly", "0 9 * * *", "run the tests", "abc", "Turn off", "Run now"} {
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
	fc := &fakeCronCtl{jobs: []CronJobView{{ID: "abc"}}}
	s := cronServer(fc)

	if rec := cronPostForm(t, s, "/cron/enable", "id=abc&enabled=false"); rec.Code != http.StatusSeeOther {
		t.Fatalf("enable: want 303, got %d", rec.Code)
	}
	if len(fc.enableCalls) != 1 || fc.enableCalls[0] != "abc:false" {
		t.Errorf("enable calls = %v", fc.enableCalls)
	}
	if rec := cronPostForm(t, s, "/cron/run", "id=abc"); rec.Code != http.StatusSeeOther {
		t.Fatalf("run: want 303, got %d", rec.Code)
	}
	if len(fc.runCalls) != 1 || fc.runCalls[0] != "abc" {
		t.Errorf("run calls = %v", fc.runCalls)
	}
	if rec := cronPostForm(t, s, "/cron/delete", "id=abc"); rec.Code != http.StatusPreconditionRequired || len(fc.removeCalls) != 0 {
		t.Fatalf("delete without its confirm step: %d, removed %v; want 428 and nothing removed", rec.Code, fc.removeCalls)
	}
	if rec := cronPostForm(t, s, "/cron/delete", "id=abc&confirm=yes"); rec.Code != http.StatusSeeOther {
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

// The global cron-exec kill-switch used to be reachable ONLY from the TUI, so an
// operator managing the box remotely could not turn it on — and with it off, an
// exec-opted-in job silently loses its gated tools (the daily brief could not
// call discord_send_message). Drive it from the dashboard instead.
func TestCron_ExecToggleFromDashboard(t *testing.T) {
	f := &fakeCronCtl{}
	s := cronServer(f)

	if rec := cronPostForm(t, s, "/cron/exec", "enabled=true&confirm=yes"); rec.Code != http.StatusSeeOther {
		t.Fatalf("enable: status = %d, want 303", rec.Code)
	}
	if !f.execOn || len(f.execCalls) != 1 || f.execCalls[0] != true {
		t.Fatalf("enable did not reach the control: on=%v calls=%v", f.execOn, f.execCalls)
	}

	if rec := cronPostForm(t, s, "/cron/exec", "enabled=false"); rec.Code != http.StatusSeeOther {
		t.Fatalf("disable: status = %d, want 303", rec.Code)
	}
	if f.execOn {
		t.Fatalf("disable did not reach the control: on=%v calls=%v", f.execOn, f.execCalls)
	}

	// The page must show the live state, or the operator cannot tell whether the
	// switch is on without reading config.
	f.execOn = true
	req := httptest.NewRequest("GET", "/cron", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `<strong>Scheduled jobs may make changes:</strong> <span class="on-tag">on</span>`) {
		t.Fatal("cron page does not render the cron-exec state")
	}
}

// Every Scheduled jobs form answers a Datastar post in place: the toast, the
// status strip, an emptied #jobs-flash and #jobs-body, never a 303 the fetch
// would follow. Only a job added sends #jobs-add. With no control the page
// says so.
func TestCron_InlineAnswersEveryForm(t *testing.T) {
	forms := map[string]url.Values{
		"/cron/add":    {"spec": {"@daily"}, "prompt": {"report"}},
		"/cron/enable": {"id": {"j1"}, "enabled": {"false"}},
		"/cron/delete": {"id": {"j1"}},
		"/cron/run":    {"id": {"j1"}},
		"/cron/exec":   {"enabled": {"true"}},
	}
	for path, f := range forms {
		for _, on := range []bool{true, false} {
			var fc CronControl
			if on {
				fc = &fakeCronCtl{jobs: []CronJobView{{ID: "j1", Spec: "@daily", Enabled: true}}}
			}
			rec := sendForm(cronServer(fc).Handler(), http.MethodPost, path, withField(f, "confirm", "yes"), datastar)
			body := rec.Body.String()
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" || rec.Header().Get("Location") != "" {
				t.Errorf("%s (control %v): %d %q to %q, want 200 text/event-stream", path, on, rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Location"))
			}
			for _, want := range []string{"data: mode replace\ndata: elements <div id=\"toast\"", `data: elements <div id="ui-status"`, `data: elements <div id="jobs-flash"></div><div id="jobs-body">`} {
				if !strings.Contains(body, want) {
					t.Errorf("%s (control %v) is missing %q:\n%s", path, on, want, body)
				}
			}
			if added := on && path == "/cron/add"; strings.Contains(body, `id="jobs-add"`) != added {
				t.Errorf("%s (control %v): #jobs-add sent = %v, want %v:\n%s", path, on, !added, added, body)
			}
			if !on && !strings.Contains(body, "Scheduled jobs aren&#39;t available on this Tenant.") {
				t.Errorf("%s with no control: the page doesn't say so:\n%s", path, body)
			}
		}
	}
}

// A job changed in place: the toast says so and its card shows it. An added
// job is listed and the add form comes back under a new id, so its boxes are
// empty. An error is an alert with the plain message and nothing
// re-rendered, so what was typed stays. A plain post still gets the 303 and
// its flash, and Delete without its confirm step is still 428. A job that
// has gone since the page was drawn is an alert, not a false success.
func TestCron_Inline(t *testing.T) {
	c := installCapture(t)
	fc := &fakeCronCtl{jobs: []CronJobView{{ID: "j1", Name: "nightly", Spec: "@daily", Enabled: true}, {ID: "j2", Spec: "@hourly", Enabled: true}}}
	s := cronServer(fc)
	post := func(path string, f url.Values, set func(*http.Request)) string {
		return sendForm(s.Handler(), http.MethodPost, path, f, set).Body.String()
	}

	body := post("/cron/enable", url.Values{"id": {"j1"}, "enabled": {"false"}}, datastar)
	_, card, _ := strings.Cut(body, `id="cron-j1"`)
	card, _, _ = strings.Cut(card, `id="cron-j2"`)
	if !strings.Contains(body, `<div id="toast" class="toast ok"><span class="toast-text">nightly is off. It won&#39;t run until you turn it on again.</span>`) ||
		!strings.Contains(card, `<span class="dim">off</span>`) || !strings.Contains(card, `<input type="hidden" name="enabled" value="true">`) || !strings.Contains(card, ">Turn on</button>") {
		t.Errorf("Turn off in place: want the toast and the card showing off:\n%s", body)
	}

	formID := regexp.MustCompile(`id="jobs-add-form-[A-Z2-7]+"`)
	var ids []string
	for _, name := range []string{"weekly", "monthly"} {
		body = post("/cron/add", url.Values{"name": {name}, "spec": {"@weekly"}, "prompt": {"report"}}, datastar)
		if !strings.Contains(body, `<span class="toast-text">Job scheduled.</span>`) || !strings.Contains(body, `<span class="nm">`+name+`</span>`) {
			t.Errorf("Add in place: want the toast and %s listed:\n%s", name, body)
		}
		ids = append(ids, formID.FindString(body))
	}
	if ids[0] == "" || ids[0] == ids[1] {
		t.Errorf("each job added gets a new add form id, so the morph replaces the form: %q", ids)
	}

	fc.set(func() { fc.execErr = errors.New("config file is locked") })
	body = post("/cron/exec", url.Values{"enabled": {"false"}}, datastar)
	if !strings.Contains(body, `<div id="toast" class="toast err" role="alert"><span class="toast-text">Couldn&#39;t change whether scheduled jobs may make changes.</span>`) || strings.Contains(body, "locked") {
		t.Errorf("a failed change: want the plain alert and not the control's own words:\n%s", body)
	}
	if !c.has("2201 ", "action=cron exec") {
		t.Errorf("the failure isn't logged as ActionFailed: %q", c.lines())
	}
	body += post("/cron/add", url.Values{"spec": {""}, "prompt": {"report"}}, datastar)
	if !strings.Contains(body, `<span class="toast-text">A job needs a schedule and something to do.</span>`) || strings.Contains(body, `id="jobs-body"`) || strings.Contains(body, `id="jobs-add"`) {
		t.Errorf("an error re-rendered the page, which would empty what was typed:\n%s", body)
	}

	rec := sendForm(s.Handler(), http.MethodPost, "/cron/run", url.Values{"id": {"j2"}}, nil)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/cron?msg=j2+is+running+now.") {
		t.Errorf("a plain post: %d to %q, want 303 to /cron with its flash", rec.Code, loc)
	}

	// A stale page: a job deleted elsewhere is named as gone, and nothing
	// is called or re-rendered, rather than a success the engine didn't do.
	for _, path := range []string{"/cron/enable", "/cron/run", "/cron/delete"} {
		body = post(path, url.Values{"id": {"gone"}, "enabled": {"false"}, "confirm": {"yes"}}, datastar)
		if !strings.Contains(body, `<div id="toast" class="toast err" role="alert"><span class="toast-text">That job isn&#39;t there any more.`) || strings.Contains(body, `id="jobs-body"`) {
			t.Errorf("%s for a job that has gone: want the alert and nothing re-rendered:\n%s", path, body)
		}
	}
	fc.set(func() {
		if slices.Contains(fc.enableCalls, "gone:false") || slices.Contains(fc.runCalls, "gone") || slices.Contains(fc.removeCalls, "gone") {
			t.Errorf("a job that has gone reached the control: %v %v %v", fc.enableCalls, fc.runCalls, fc.removeCalls)
		}
	})

	rec = sendForm(s.Handler(), http.MethodPost, "/cron/delete", url.Values{"id": {"j1"}}, datastar)
	if rec.Code != http.StatusPreconditionRequired || len(fc.Jobs()) != 4 {
		t.Errorf("Delete without its confirm step over Datastar: %d with %d jobs left, want 428 and all 4", rec.Code, len(fc.Jobs()))
	}
}
