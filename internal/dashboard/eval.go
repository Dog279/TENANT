package dashboard

// eval.go is the server-rendered Eval & Quality page (TEN-201): the web view
// of the nightly regression gate. It mirrors the TUI's /eval — show the
// schedule, run a check now, read the score trend, see what moved — but for a
// non-technical operator: a chart instead of a text table, "Run a quality
// check now" instead of `/eval now`, plain words instead of slash commands.
//
// Same shape as cron.go: an EvalControl interface (satisfied in cmd/tenant by
// dashEval), unconditional route mounting with nil-guarded handlers, and
// form mutations answered by evalDone: in place for a Datastar post, else a
// 303. The trend chart is a server-rendered inline SVG (the
// ssr_memory_svg.go precedent) — no JS.

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"strings"
)

// EvalControl is the runtime eval surface the dashboard drives. cmd/tenant
// adapts evalTUIControl + the trend/diff readers to it (see dashEval there).
// A nil control renders an "isn't configured" state. Read methods return
// STRUCTURED views (never the TUI's preformatted strings); mutating methods
// return a human status line surfaced as a flash message.
type EvalControl interface {
	Schedule() EvalScheduleView // current cadence + last recorded run
	Trend() []EvalTrendPoint    // score series, oldest first
	Diff() (string, error)      // per-task movers (preformatted block)
	SetEvery(spec string) (string, error)
	SetAt(spec string) (string, error)
	Off() (string, error)
	RunNow() (string, error)
	// Judge surface (TEN-91): the eval LLM-judge model, persisted + shared with
	// `tenant eval`, the TUI /judge command, and the nightly gate.
	JudgeStatus() string                                   // render-ready current judge
	SetJudge(kind, model, endpoint string) (string, error) // pin a judge override
	ClearJudge() error                                     // revert to the planner default
}

// EvalScheduleView is the render-ready schedule state. Trend is "up" /
// "steady" / "down" / "" (no data), derived from the latest run's regression
// verdict — so the home badge and this page agree.
type EvalScheduleView struct {
	Desc      string  // "off" | "every 24h0m0s" | "daily at 03:15"
	Live      bool    // applies this session (vs persist-only)
	HasRun    bool    // any recorded run yet
	LastWhen  string  // pre-formatted local time, "" if none
	LastScore float64 // 0-100
	LastPass  int
	LastTotal int
	Skipped   int
	Ungraded  int
	Trend     string // "up" | "steady" | "down" | ""
	// Live progress of a check running RIGHT NOW (zero values = idle).
	Running    bool
	RunDone    int
	RunTotal   int
	RunTask    string // last task to finish
	RunMins    int    // minutes since the run started
	RunPercent int    // 0-100, precomputed for the bar width
}

// Mode is the schedule form's choice that Desc is: "every", "at" or "off"
// (UX-20: the form showed Every 24h whatever the schedule was).
func (v EvalScheduleView) Mode() string {
	switch {
	case strings.HasPrefix(v.Desc, "every "):
		return "every"
	case strings.HasPrefix(v.Desc, "daily at "):
		return "at"
	}
	return "off"
}

// Spec is the form's value for Mode: "12h" for "every 12h0m0s", "03:15" for
// "daily at 03:15", "" otherwise.
func (v EvalScheduleView) Spec() string {
	switch v.Mode() {
	case "every":
		d := strings.TrimPrefix(v.Desc, "every ")
		if strings.HasSuffix(d, "m0s") {
			d = strings.TrimSuffix(d, "0s")
		}
		if strings.HasSuffix(d, "h0m") {
			d = strings.TrimSuffix(d, "0m")
		}
		return d
	case "at":
		return strings.TrimPrefix(v.Desc, "daily at ")
	}
	return ""
}

// EvalTrendPoint is one run on the chart. Score is 0-100.
type EvalTrendPoint struct {
	When      string
	Score     float64
	Regressed bool
	HasBase   bool
}

// SetEval installs the eval control after construction (mirrors SetCron).
func (s *Server) SetEval(c EvalControl) { s.eval = c }

func (s *Server) mountEvalSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /eval", s.handleEvalPage)
	mux.HandleFunc("POST /eval/schedule", s.handleEvalScheduleForm)
	mux.HandleFunc("POST /eval/run", s.handleEvalRunForm)
	mux.HandleFunc("POST /eval/judge", s.handleEvalJudgeForm)            // TEN-91
	mux.HandleFunc("POST /eval/judge/clear", s.handleEvalJudgeClearForm) // TEN-91
}

type evalPageData struct {
	layoutData
	Configured bool
	Sched      EvalScheduleView
	Points     []EvalTrendPoint
	Chart      template.HTML // server-built inline SVG; numbers only, safe to not escape
	Diff       string
	Judge      string // TEN-91: render-ready current eval judge
	Err        string
	Msg        string
}

func (s *Server) evalData() evalPageData {
	d := evalPageData{layoutData: layoutData{Title: "Quality", Page: "eval"}}
	if s.eval == nil {
		return d
	}
	d.Configured = true
	d.Judge = s.eval.JudgeStatus()
	d.Sched = s.eval.Schedule()
	d.Points = s.eval.Trend()
	d.Chart = template.HTML(renderTrendSVG(d.Points)) //nolint:gosec // server-built SVG, numbers only
	if diff, err := s.eval.Diff(); err == nil {
		d.Diff = diff
	}
	return d
}

func (s *Server) handleEvalPage(w http.ResponseWriter, r *http.Request) {
	d := s.evalData()
	d.Err = r.URL.Query().Get("err")
	d.Msg = r.URL.Query().Get("msg")
	s.render(w, s.tmpl.eval, d)
}

// evalDone is every Quality form's answer: in place for a Datastar post,
// with #eval-body re-rendered; a 303 with the flash otherwise.
func (s *Server) evalDone(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	s.formDone(w, r, "/eval", msg, errMsg, func() (string, error) {
		var buf bytes.Buffer
		if err := s.tmpl.eval.ExecuteTemplate(&buf, "eval-body", s.evalData()); err != nil {
			return "", err
		}
		return buf.String(), nil
	})
}

func (s *Server) handleEvalScheduleForm(w http.ResponseWriter, r *http.Request) {
	if s.eval == nil {
		s.evalDone(w, r, "", "")
		return
	}
	mode := strings.TrimSpace(r.FormValue("mode"))
	// Each option has its own box (spec_every, spec_at): with one name for
	// both, the Every box's 24h was read for Daily at too, which then failed.
	spec := strings.TrimSpace(r.FormValue("spec_" + mode))
	if spec == "" {
		spec = strings.TrimSpace(r.FormValue("spec"))
	}
	var err error
	switch mode {
	case "every":
		_, err = s.eval.SetEvery(spec)
	case "at":
		_, err = s.eval.SetAt(spec)
	case "off":
		_, err = s.eval.Off()
	default:
		s.evalDone(w, r, "", "Pick a schedule option.")
		return
	}
	if err != nil {
		s.evalDone(w, r, "", s.flashFor(r.Context(), "eval schedule", err,
			"Couldn't update the schedule. Use a length like 24h or 12h, or a time like 03:15."))
		return
	}
	s.evalDone(w, r, "Schedule saved.", "")
}

func (s *Server) handleEvalRunForm(w http.ResponseWriter, r *http.Request) {
	if s.eval == nil {
		s.evalDone(w, r, "", "")
		return
	}
	status, err := s.eval.RunNow()
	if err != nil {
		s.evalDone(w, r, "", s.flashFor(r.Context(), "eval run", err, "Couldn't start a quality check."))
		return
	}
	s.evalDone(w, r, status, "")
}

// handleEvalJudgeForm pins the eval LLM-judge model (TEN-91). The API key is
// never entered here — it's read from the kind's env var at eval time.
func (s *Server) handleEvalJudgeForm(w http.ResponseWriter, r *http.Request) {
	if s.eval == nil {
		s.evalDone(w, r, "", "")
		return
	}
	kind := strings.TrimSpace(r.FormValue("kind"))
	mdl := strings.TrimSpace(r.FormValue("model"))
	endpoint := strings.TrimSpace(r.FormValue("endpoint"))
	if kind == "" || mdl == "" {
		s.evalDone(w, r, "", "Enter a provider and a model name for the grader.")
		return
	}
	if _, err := s.eval.SetJudge(kind, mdl, endpoint); err != nil {
		s.evalDone(w, r, "", s.flashFor(r.Context(), "set judge", err, "Couldn't set the grader. Check the provider's name (anthropic, zai, openai…)."))
		return
	}
	s.evalDone(w, r, "Saved: "+mdl+" from "+kind+" grades the checks now.", "")
}

// handleEvalJudgeClearForm reverts to the planner-default judge (TEN-91).
func (s *Server) handleEvalJudgeClearForm(w http.ResponseWriter, r *http.Request) {
	if s.eval == nil {
		s.evalDone(w, r, "", "")
		return
	}
	if err := s.eval.ClearJudge(); err != nil {
		s.evalDone(w, r, "", s.flashFor(r.Context(), "clear judge", err, "Couldn't change the grader."))
		return
	}
	s.evalDone(w, r, "Your main model grades the checks again.", "")
}

// renderTrendSVG draws the score-over-time line as an inline SVG (0-100 on a
// fixed scale). Regression points are marked red. Returns "" for <2 points
// (the page shows an empty-state instead). Pure string building — no JS, no
// external lib, matching ssr_memory_svg.go. All numbers are server-computed,
// so the output is safe to mark template.HTML at render.
func renderTrendSVG(points []EvalTrendPoint) string {
	if len(points) < 2 {
		return ""
	}
	const (
		w, h    = 640, 160
		padL    = 34
		padR    = 12
		padT    = 12
		padB    = 22
		plotW   = w - padL - padR
		plotH   = h - padT - padB
		scaleHi = 100.0
	)
	x := func(i int) float64 {
		return float64(padL) + float64(i)*float64(plotW)/float64(len(points)-1)
	}
	y := func(score float64) float64 {
		if score < 0 {
			score = 0
		}
		if score > scaleHi {
			score = scaleHi
		}
		return float64(padT) + (1-score/scaleHi)*float64(plotH)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" class="trend-svg" role="img" aria-label="Quality score over time">`, w, h)
	// gridlines at 0/50/100
	for _, gv := range []float64{0, 50, 100} {
		gy := y(gv)
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="grid"/>`, padL, gy, w-padR, gy)
		fmt.Fprintf(&b, `<text x="%d" y="%.1f" class="axis">%d</text>`, 6, gy+3, int(gv))
	}
	// the line path
	var path strings.Builder
	for i, p := range points {
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&path, "%s%.1f %.1f ", cmd, x(i), y(p.Score))
	}
	fmt.Fprintf(&b, `<path d="%s" class="trend-line"/>`, strings.TrimSpace(path.String()))
	// points (regressions red)
	for i, p := range points {
		cls := "dot"
		if p.HasBase && p.Regressed {
			cls = "dot reg"
		}
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3" class="%s"/>`, x(i), y(p.Score), cls)
	}
	b.WriteString(`</svg>`)
	return b.String()
}
