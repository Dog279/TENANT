package dashboard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEvalCtl changes as the real control does (a saved schedule is the one
// shown, a check run now is in progress at once, a grader set is the one
// named), under mu, so a browser test's server and the test can share it.
type fakeEvalCtl struct {
	mu        sync.Mutex
	sched     EvalScheduleView
	points    []EvalTrendPoint
	diff      string
	everyCall string
	atCall    string
	offCalls  int
	runCalls  int
	setErr    error
	judge     string   // QualityModel()
	judgeSet  []string // "kind|model|endpoint"
	judgeClr  int
}

func (f *fakeEvalCtl) Schedule() EvalScheduleView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sched
}
func (f *fakeEvalCtl) Trend() []EvalTrendPoint { return f.points }
func (f *fakeEvalCtl) Diff() (string, error)   { return f.diff, nil }
func (f *fakeEvalCtl) SetEvery(s string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.everyCall = s
	d, _ := time.ParseDuration(s)
	return f.saved("every "+d.String(), f.setErr)
}
func (f *fakeEvalCtl) SetAt(s string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.atCall = s
	return f.saved("daily at "+s, f.setErr)
}
func (f *fakeEvalCtl) Off() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offCalls++
	return f.saved("off", nil)
}

// saved makes desc the schedule shown unless err; f.mu is held.
func (f *fakeEvalCtl) saved(desc string, err error) (string, error) {
	if err == nil {
		f.sched.Desc = desc
	}
	return desc, err
}
func (f *fakeEvalCtl) RunNow() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCalls++
	f.sched.Running, f.sched.RunTotal = true, 10
	return "Quality check queued.", nil
}
func (f *fakeEvalCtl) QualityModel() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.judge
}
func (f *fakeEvalCtl) SetQualityModel(kind, model, endpoint string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.judgeSet = append(f.judgeSet, kind+"|"+model+"|"+endpoint)
	if f.setErr == nil {
		f.judge = model + " from " + kind + " grades the checks."
	}
	return "judge set", f.setErr
}
func (f *fakeEvalCtl) ClearQualityModel() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.judgeClr++
	return nil
}

// Every Quality form answers a Datastar post in place: the toast, the status
// strip and #eval-body, never a 303 the fetch would follow. With no control
// the page says so.
func TestEval_InlineAnswersEveryForm(t *testing.T) {
	forms := map[string]url.Values{
		"/eval/schedule":            {"mode": {"every"}, "spec_every": {"12h"}},
		"/eval/run":                 {},
		"/eval/quality-model":       {"kind": {"zai"}, "model": {"glm"}},
		"/eval/quality-model/clear": {},
	}
	for path, f := range forms {
		for _, on := range []bool{true, false} {
			var fc EvalControl
			if on {
				fc = &fakeEvalCtl{sched: EvalScheduleView{Desc: "off", Live: true}}
			}
			rec := sendForm(evalServer(fc).Handler(), http.MethodPost, path, f, datastar)
			body := rec.Body.String()
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" || rec.Header().Get("Location") != "" {
				t.Errorf("%s (control %v): %d %q to %q, want 200 text/event-stream", path, on, rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Location"))
			}
			for _, want := range []string{"data: mode replace\ndata: elements <div id=\"toast\"", `data: elements <div id="ui-status"`, `data: elements <div id="eval-body">`} {
				if !strings.Contains(body, want) {
					t.Errorf("%s (control %v) is missing %q:\n%s", path, on, want, body)
				}
			}
			if !on && !strings.Contains(body, "Quality checks aren&#39;t available on this Tenant.") {
				t.Errorf("%s with no control: the page doesn't say so:\n%s", path, body)
			}
		}
	}
}

// A setting saved in place: the toast says so and the page shows it; a
// check run now shows its progress, which refreshes itself; the grader's
// section stays open. An error is an alert with the plain message and
// nothing re-rendered, so what was typed stays. A plain post still gets the
// 303 and its flash.
func TestEval_Inline(t *testing.T) {
	c := installCapture(t)
	fc := &fakeEvalCtl{sched: EvalScheduleView{Desc: "off", Live: true}}
	s := evalServer(fc)
	post := func(path string, f url.Values) string {
		return sendForm(s.Handler(), http.MethodPost, path, f, datastar).Body.String()
	}

	body := post("/eval/schedule", url.Values{"mode": {"at"}, "spec_every": {"24h"}, "spec_at": {"04:30"}})
	for _, want := range []string{`<div id="toast" class="toast ok"><span class="toast-text">Schedule saved.</span>`, "Now: <strong>daily at 04:30</strong>", `value="at" checked`, `name="spec_at" value="04:30"`} {
		if !strings.Contains(body, want) {
			t.Errorf("a schedule saved in place is missing %q:\n%s", want, body)
		}
	}

	body = post("/eval/run", nil)
	for _, want := range []string{`<span class="toast-text">Quality check queued.</span>`, `<span class="loading">Check in progress</span>`, `data-on-interval__duration.20s="window.location.reload()"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Run now in place is missing %q:\n%s", want, body)
		}
	}

	body = post("/eval/quality-model", url.Values{"kind": {"zai"}, "model": {"glm-4.6"}})
	for _, want := range []string{`<span class="toast-text">Saved: glm-4.6 from zai grades the checks now.</span>`, "glm-4.6 from zai grades the checks.", `<details class="advanced" data-preserve-attr="open">`} {
		if !strings.Contains(body, want) {
			t.Errorf("a grader set in place is missing %q:\n%s", want, body)
		}
	}

	fc.mu.Lock()
	fc.setErr = errors.New("config file is locked")
	fc.mu.Unlock()
	body = post("/eval/schedule", url.Values{"mode": {"every"}, "spec_every": {"soon"}})
	if !strings.Contains(body, `<div id="toast" class="toast err" role="alert"><span class="toast-text">Couldn&#39;t update the schedule. Use a length like 24h or 12h, or a time like 03:15.</span>`) || strings.Contains(body, "locked") {
		t.Errorf("a failed save: want the plain alert and not the control's own words:\n%s", body)
	}
	if strings.Contains(body, `id="eval-body"`) {
		t.Errorf("a failed save re-rendered the page, which would undo what was typed:\n%s", body)
	}
	if !c.has("2201 ", "action=eval schedule") {
		t.Errorf("the failure isn't logged as ActionFailed: %q", c.lines())
	}

	rec := sendForm(s.Handler(), http.MethodPost, "/eval/quality-model/clear", nil, nil)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/eval?msg=") {
		t.Errorf("a plain post: %d to %q, want 303 to /eval with its flash", rec.Code, loc)
	}
}

// TEN-91: the Quality page renders the Quality model status + routes the set/clear forms.
func TestEval_QualityModelControl(t *testing.T) {
	fc := &fakeEvalCtl{judge: "Your main model grades the checks."}
	s := evalServer(fc)

	if body := get(t, s, "/eval").Body.String(); !strings.Contains(body, "Your main model grades the checks.") || !strings.Contains(body, "Set the Quality model") || strings.Contains(strings.ToLower(body), "judge") {
		t.Errorf("Quality page should render the Quality model status + form and never say judge (TEN-516); got:\n%s", body)
	}

	rec := submitForm(t, s, "/eval/quality-model", "kind=zai&model=glm-4.6&endpoint=https://api.z.ai/x")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("set judge should 303, got %d", rec.Code)
	}
	if len(fc.judgeSet) != 1 || fc.judgeSet[0] != "zai|glm-4.6|https://api.z.ai/x" {
		t.Fatalf("judge set didn't route: %v", fc.judgeSet)
	}

	// Missing model → validation, no SetJudge call.
	submitForm(t, s, "/eval/quality-model", "kind=anthropic")
	if len(fc.judgeSet) != 1 {
		t.Errorf("incomplete judge form must not call SetJudge: %v", fc.judgeSet)
	}

	submitForm(t, s, "/eval/quality-model/clear", "")
	if fc.judgeClr != 1 {
		t.Fatalf("clear judge should call ClearJudge once: %d", fc.judgeClr)
	}
}

func evalServer(fc EvalControl) *Server {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if fc != nil {
		s.SetEval(fc)
	}
	return s
}

// get is defined in ssr_test.go (same package). submitForm POSTs a urlencoded
// body — distinct from auth_pages_test.go's postForm(path, url.Values).
func submitForm(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestEval_PageNotConfigured(t *testing.T) {
	rec := get(t, evalServer(nil), "/eval")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Quality checks aren&#39;t available on this Tenant.") {
		t.Errorf("nil eval should render not-configured; got:\n%s", rec.Body.String())
	}
}

func TestEval_PageShowsScoreAndSchedule(t *testing.T) {
	fc := &fakeEvalCtl{sched: EvalScheduleView{
		Desc: "every 24h0m0s", Live: true, HasRun: true,
		LastWhen: "2026-06-12 17:13", LastScore: 89.2, LastPass: 28, LastTotal: 31,
		Skipped: 26, Trend: "up",
	}}
	rec := get(t, evalServer(fc), "/eval")
	body := rec.Body.String()
	for _, want := range []string{"89", "28", "/ 31 passed", "every 24h0m0s", "26 skipped"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q:\n%s", want, body)
		}
	}
}

func TestEval_ChartRendersWithEnoughPoints(t *testing.T) {
	fc := &fakeEvalCtl{
		sched: EvalScheduleView{HasRun: true, LastScore: 80},
		points: []EvalTrendPoint{
			{When: "06-11 01:03", Score: 31, HasBase: true, Regressed: true},
			{When: "06-12 17:13", Score: 89, HasBase: true},
		},
	}
	body := get(t, evalServer(fc), "/eval").Body.String()
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "trend-line") {
		t.Errorf("expected an inline trend SVG; got:\n%s", body)
	}
	if !strings.Contains(body, "dot reg") {
		t.Error("a regressed point should render with the reg class")
	}
}

func TestEval_ChartEmptyStateUnderTwoPoints(t *testing.T) {
	fc := &fakeEvalCtl{sched: EvalScheduleView{HasRun: true}, points: []EvalTrendPoint{{Score: 50}}}
	body := get(t, evalServer(fc), "/eval").Body.String()
	if strings.Contains(body, "<svg") {
		t.Error("one point should not draw a chart")
	}
	if !strings.Contains(body, "Not enough checks") {
		t.Errorf("expected the empty-chart state; got:\n%s", body)
	}
}

func TestEval_ScheduleFormRoutesByMode(t *testing.T) {
	fc := &fakeEvalCtl{}
	s := evalServer(fc)
	if rec := submitForm(t, s, "/eval/schedule", "mode=every&spec=12h"); rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	if fc.everyCall != "12h" {
		t.Errorf("SetEvery got %q, want 12h", fc.everyCall)
	}
	submitForm(t, s, "/eval/schedule", "mode=at&spec=03:15")
	if fc.atCall != "03:15" {
		t.Errorf("SetAt got %q, want 03:15", fc.atCall)
	}
	submitForm(t, s, "/eval/schedule", "mode=off")
	if fc.offCalls != 1 {
		t.Errorf("Off calls = %d, want 1", fc.offCalls)
	}
}

func TestEval_RunNowPostsToControl(t *testing.T) {
	fc := &fakeEvalCtl{}
	rec := submitForm(t, evalServer(fc), "/eval/run", "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	if fc.runCalls != 1 {
		t.Errorf("RunNow calls = %d, want 1", fc.runCalls)
	}
}

func TestEval_MutationsNilSafe(t *testing.T) {
	s := evalServer(nil) // no control
	for _, p := range []string{"/eval/run", "/eval/schedule"} {
		if rec := submitForm(t, s, p, "mode=off"); rec.Code != http.StatusSeeOther {
			t.Errorf("%s with nil control: want 303 redirect, got %d", p, rec.Code)
		}
	}
}

// UX-19, UX-20: the form shows the schedule there is, and the page's own
// form (both boxes sent, as a browser sends them) saves Daily at; with
// checks off the run button says so up front.
func TestEval_FormFollowsTheSchedule(t *testing.T) {
	for desc, want := range map[string][]string{
		"every 12h0m0s":  {`value="every" checked`, `name="spec_every" value="12h"`},
		"daily at 03:15": {`value="at" checked`, `name="spec_at" value="03:15"`},
		"off":            {`value="off" checked`, `name="spec_every" value="24h"`},
	} {
		body := get(t, evalServer(&fakeEvalCtl{sched: EvalScheduleView{Desc: desc, Live: true}}), "/eval").Body.String()
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: the form lacks %q", desc, w)
			}
		}
		if strings.Count(body, " checked") != 1 {
			t.Errorf("%s: %d options checked", desc, strings.Count(body, " checked"))
		}
	}
	fc := &fakeEvalCtl{}
	submitForm(t, evalServer(fc), "/eval/schedule", "mode=at&spec_every=24h&spec_at=03:15")
	if fc.atCall != "03:15" {
		t.Errorf("Daily at from the page's form: SetAt got %q", fc.atCall)
	}
	body := get(t, evalServer(&fakeEvalCtl{sched: EvalScheduleView{Desc: "off"}}), "/eval").Body.String()
	if !strings.Contains(body, "Quality checks are off on this Tenant") || strings.Contains(body, `action="/eval/run"`) {
		t.Errorf("checks off: the page should say so and offer no run")
	}
}
