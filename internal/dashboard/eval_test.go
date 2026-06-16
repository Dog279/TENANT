package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeEvalCtl struct {
	sched     EvalScheduleView
	points    []EvalTrendPoint
	diff      string
	everyCall string
	atCall    string
	offCalls  int
	runCalls  int
	setErr    error
	judge     string   // JudgeStatus()
	judgeSet  []string // "kind|model|endpoint"
	judgeClr  int
}

func (f *fakeEvalCtl) Schedule() EvalScheduleView { return f.sched }
func (f *fakeEvalCtl) Trend() []EvalTrendPoint    { return f.points }
func (f *fakeEvalCtl) Diff() (string, error)      { return f.diff, nil }
func (f *fakeEvalCtl) SetEvery(s string) (string, error) {
	f.everyCall = s
	return "every " + s, f.setErr
}
func (f *fakeEvalCtl) SetAt(s string) (string, error) {
	f.atCall = s
	return "daily at " + s, f.setErr
}
func (f *fakeEvalCtl) Off() (string, error)    { f.offCalls++; return "off", nil }
func (f *fakeEvalCtl) RunNow() (string, error) { f.runCalls++; return "queued", nil }
func (f *fakeEvalCtl) JudgeStatus() string     { return f.judge }
func (f *fakeEvalCtl) SetJudge(kind, model, endpoint string) (string, error) {
	f.judgeSet = append(f.judgeSet, kind+"|"+model+"|"+endpoint)
	return "judge set", f.setErr
}
func (f *fakeEvalCtl) ClearJudge() error { f.judgeClr++; return nil }

// TEN-91: the Quality page renders the judge status + routes the set/clear forms.
func TestEval_JudgeControl(t *testing.T) {
	fc := &fakeEvalCtl{judge: "judge: default — planner self-judging"}
	s := evalServer(fc)

	if body := get(t, s, "/eval").Body.String(); !strings.Contains(body, "self-judging") || !strings.Contains(body, "Set judge") {
		t.Errorf("Quality page should render the judge status + form; got:\n%s", body)
	}

	rec := submitForm(t, s, "/eval/judge", "kind=zai&model=glm-4.6&endpoint=https://api.z.ai/x")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("set judge should 303, got %d", rec.Code)
	}
	if len(fc.judgeSet) != 1 || fc.judgeSet[0] != "zai|glm-4.6|https://api.z.ai/x" {
		t.Fatalf("judge set didn't route: %v", fc.judgeSet)
	}

	// Missing model → validation, no SetJudge call.
	submitForm(t, s, "/eval/judge", "kind=anthropic")
	if len(fc.judgeSet) != 1 {
		t.Errorf("incomplete judge form must not call SetJudge: %v", fc.judgeSet)
	}

	submitForm(t, s, "/eval/judge/clear", "")
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
	if !strings.Contains(rec.Body.String(), "aren't configured") {
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
