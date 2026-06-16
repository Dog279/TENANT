package dashboard

// eval.go is the server-rendered Eval & Quality page (TEN-201): the web view
// of the nightly regression gate. It mirrors the TUI's /eval — show the
// schedule, run a check now, read the score trend, see what moved — but for a
// non-technical operator: a chart instead of a text table, "Run a quality
// check now" instead of `/eval now`, plain words instead of slash commands.
//
// Same shape as cron.go: an EvalControl interface (satisfied in cmd/tenant by
// dashEval), unconditional route mounting with nil-guarded handlers, and
// form/303 mutations so every view is a plain GET. The trend chart is a
// server-rendered inline SVG (the ssr_memory_svg.go precedent) — no JS.

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
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

func (s *Server) handleEvalPage(w http.ResponseWriter, r *http.Request) {
	d := evalPageData{layoutData: layoutData{Title: "Quality", Page: "eval"}}
	d.Err = r.URL.Query().Get("err")
	d.Msg = r.URL.Query().Get("msg")
	if s.eval == nil {
		s.render(w, s.tmpl.eval, d)
		return
	}
	d.Configured = true
	d.Judge = s.eval.JudgeStatus()
	d.Sched = s.eval.Schedule()
	d.Points = s.eval.Trend()
	d.Chart = template.HTML(renderTrendSVG(d.Points)) //nolint:gosec // server-built SVG, numbers only
	if diff, err := s.eval.Diff(); err == nil {
		d.Diff = diff
	}
	s.render(w, s.tmpl.eval, d)
}

func (s *Server) handleEvalScheduleForm(w http.ResponseWriter, r *http.Request) {
	if s.eval == nil {
		http.Redirect(w, r, "/eval", http.StatusSeeOther)
		return
	}
	mode := strings.TrimSpace(r.FormValue("mode"))
	spec := strings.TrimSpace(r.FormValue("spec"))
	var status string
	var err error
	switch mode {
	case "every":
		status, err = s.eval.SetEvery(spec)
	case "at":
		status, err = s.eval.SetAt(spec)
	case "off":
		status, err = s.eval.Off()
	default:
		redirectEval(w, r, "", "Pick a schedule option.")
		return
	}
	if err != nil {
		redirectEval(w, r, "", "Couldn't update the schedule: "+err.Error())
		return
	}
	redirectEval(w, r, status, "")
}

func (s *Server) handleEvalRunForm(w http.ResponseWriter, r *http.Request) {
	if s.eval == nil {
		http.Redirect(w, r, "/eval", http.StatusSeeOther)
		return
	}
	status, err := s.eval.RunNow()
	if err != nil {
		redirectEval(w, r, "", "Couldn't start a quality check: "+err.Error())
		return
	}
	redirectEval(w, r, status, "")
}

// handleEvalJudgeForm pins the eval LLM-judge model (TEN-91). The API key is
// never entered here — it's read from the kind's env var at eval time.
func (s *Server) handleEvalJudgeForm(w http.ResponseWriter, r *http.Request) {
	if s.eval == nil {
		http.Redirect(w, r, "/eval", http.StatusSeeOther)
		return
	}
	kind := strings.TrimSpace(r.FormValue("kind"))
	mdl := strings.TrimSpace(r.FormValue("model"))
	endpoint := strings.TrimSpace(r.FormValue("endpoint"))
	if kind == "" || mdl == "" {
		redirectEval(w, r, "", "Pick a provider kind and a model id for the judge.")
		return
	}
	status, err := s.eval.SetJudge(kind, mdl, endpoint)
	if err != nil {
		s.log.Warn("dashboard: set judge", "kind", kind, "err", err)
		redirectEval(w, r, "", "Couldn't set the judge: "+err.Error())
		return
	}
	redirectEval(w, r, status, "")
}

// handleEvalJudgeClearForm reverts to the planner-default judge (TEN-91).
func (s *Server) handleEvalJudgeClearForm(w http.ResponseWriter, r *http.Request) {
	if s.eval == nil {
		http.Redirect(w, r, "/eval", http.StatusSeeOther)
		return
	}
	if err := s.eval.ClearJudge(); err != nil {
		s.log.Warn("dashboard: clear judge", "err", err)
		redirectEval(w, r, "", "Couldn't clear the judge: "+err.Error())
		return
	}
	redirectEval(w, r, "Judge reverted to the planner model (self-judging).", "")
}

func redirectEval(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	target := "/eval"
	switch {
	case errMsg != "":
		target += "?err=" + url.QueryEscape(errMsg)
	case msg != "":
		target += "?msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
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
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" class="trend-svg" role="img" aria-label="quality score over time">`, w, h)
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
