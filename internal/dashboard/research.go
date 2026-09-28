package dashboard

// research.go is the "Research" page (TEN-210): start a deep-research run,
// watch it live (GET /research/live), and read, re-run or delete past runs.
// The run lives in cmd/tenant's adapter under the process lifetime, so a
// closed tab or a dropped stream never stops it; the stream sends the whole
// panel on every connect, which is how a tab re-attaches. Changes are form
// POSTs answered with a redirect; only the live panel streams.

import (
	"bytes"
	"context"
	"html"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"tenant/internal/logs/event"
)

// ResearchControl is the research surface. cmd/tenant adapts the terminal
// app's researchControl (or serve's research-only runtime); nil renders the
// "isn't available" state.
type ResearchControl interface {
	Watch() (ResearchLiveView, <-chan struct{}) // current or last run, plus a channel closed at its next change
	Start(question string, skipQuestions bool) error
	Clarify(answer string) error // "" = research it as typed
	Replay(id string) error
	Stop() bool // false when nothing is running
	History(limit int) ([]ResearchRunRow, error)
	Report(id string) (ResearchReportView, error)
	Delete(id string) error
}

// ResearchLiveView is the live panel's state.
type ResearchLiveView struct {
	SavesToNotes, WebOff, HistoryOn bool
	WebOffWhy                       string // D19: "" or the plain reason
	Phase                           string // "" checking planning researching reflecting writing saving clarify done failed stopped
	Running, Elsewhere              bool
	RunID, Question, ReplayOf       string
	Round, Rounds                   int
	Questions                       []string
	Progress, Elapsed               string
	Clarify                         []string
	Report                          *ResearchReportView
	Err                             string
	Lines                           []string
}

// ResearchRunRow is one past run in the history list.
type ResearchRunRow struct {
	ID, Question, State, StateText, When, Took, Model, ReplayOf string
	Sources                                                     int
	Live                                                        bool
}

// ResearchReportView is one finished run's report.
type ResearchReportView struct {
	ID, Question, StateText, When, Took, Model, ReplayOf, Err string
	Body                                                      string
	Refs                                                      []ResearchRef
	Findings                                                  []ResearchFinding
	Live                                                      bool // still going: no Delete or Run again
}

// ResearchRef is one numbered source of a report.
type ResearchRef struct {
	N   int
	URL string
	own bool // on a name this dashboard answers to (Server.researchReport)
}

// Web reports whether the source may be a link: only an http or https URL,
// in any letter case, to a public host. A report's sources come from web
// pages, so anything else (javascript:, data:, a note path) renders as plain
// text, and so does an address on this machine, its network or the tailnet:
// cookies aren't kept apart by port, so a click there would hand whatever
// listens (a server the agent started, say) the dashboard's sign-in cookie.
func (r ResearchRef) Web() bool {
	u, err := url.Parse(r.URL)
	return err == nil && (strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")) &&
		!r.own && publicHost(u.Hostname())
}

// tailnetRange is Tailscale's address range (100.64.0.0/10, CGNAT).
var tailnetRange = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// publicHost reports whether h is an internet host, not this machine, its
// network or the tailnet. A name without a dot is a local one, and a last
// label that is a number means a browser reads the name as an IPv4 address
// (127.1, 0x7f.0.0.1), so both count as local.
func publicHost(h string) bool {
	// A browser folds fullwidth dots and digits to ASCII (127.0.0.1。 and
	// 127.0.0.１ both reach 127.0.0.1), so a non-ASCII host is never linked;
	// an international name shows as text, the safe direction.
	if strings.IndexFunc(h, func(r rune) bool { return r > 0x7f }) >= 0 {
		return false
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if ip := net.ParseIP(h); ip != nil {
		return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() && !tailnetRange.Contains(ip)
	}
	dot := strings.LastIndex(h, ".")
	if last := h[dot+1:]; dot < 0 || strings.TrimLeft(last, "0123456789") == "" || strings.HasPrefix(last, "0x") {
		return false
	}
	for _, local := range []string{".localhost", ".local", ".internal", ".lan", ".home.arpa", ".ts.net"} {
		if strings.HasSuffix(h, local) {
			return false
		}
	}
	return true
}

// ownHost reports whether h is a host name this dashboard answers to, on
// any port.
func (s *Server) ownHost(h string) bool {
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	s.trustedMu.RLock()
	names := append(append([]string{s.cfg.Addr}, s.cfg.AllowedHosts...), s.trusted...)
	s.trustedMu.RUnlock()
	for _, a := range names {
		if ah, _, ok := splitAuthority(a); ok && ah == h {
			return true
		}
	}
	return false
}

// ResearchFinding is one helper's finding under a report.
type ResearchFinding struct{ Question, State, Body string }

// SetResearch installs the research control after construction.
func (s *Server) SetResearch(c ResearchControl) { s.research = c }

func (s *Server) mountResearchSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /research", s.handleResearchPage)
	mux.HandleFunc("GET /research/live", s.handleResearchLive) // wins over {id}
	mux.HandleFunc("GET /research/{id}", s.handleResearchRunPage)
	mux.HandleFunc("POST /research/start", s.handleResearchStart)
	mux.HandleFunc("POST /research/clarify", s.handleResearchClarify)
	mux.HandleFunc("POST /research/stop", needConfirm(nil, s.handleResearchStop))
	mux.HandleFunc("POST /research/{id}/replay", s.handleResearchReplay)
	mux.HandleFunc("POST /research/{id}/delete", needConfirm(nil, s.handleResearchDelete))
}

// Page data: the views plus what the templates can't build themselves (the
// busy line, escaped-first report HTML, escaped paths).
type researchPageData struct {
	layoutData
	Configured bool
	Msg, Err   string
	Live       researchLiveData
	History    researchHistoryData
	Run        *researchReportData // the run page's report
}

type researchLiveData struct {
	ResearchLiveView
	Busy    *BusyView
	Waiting int                 // approvals pending while a run is going
	Done    *researchReportData // Report, ready to render
}

type researchHistoryData struct {
	Rows []researchRow
	Err  string
}

type researchRow struct {
	ResearchRunRow
	Path string // /research/<id>, path-escaped
}

type researchReportData struct {
	ResearchReportView
	BodyHTML template.HTML
	Path     string
}

// researchHistoryLimit is how many past runs the page lists.
const researchHistoryLimit = 50

// researchQuestionMax is the longest question or answer the page accepts, in
// characters.
const researchQuestionMax = 2000

func (s *Server) researchData(r *http.Request) researchPageData {
	return researchPageData{
		layoutData: layoutData{Title: "Research", Page: "research"},
		Configured: s.research != nil,
		Msg:        r.URL.Query().Get("msg"),
		Err:        r.URL.Query().Get("err"),
	}
}

func (s *Server) handleResearchPage(w http.ResponseWriter, r *http.Request) {
	d := s.researchData(r)
	if s.research != nil {
		v, _ := s.research.Watch()
		d.Live, d.History = s.researchLive(v), s.researchHistory(r.Context())
	}
	s.render(w, s.tmpl.research, d)
}

func (s *Server) handleResearchRunPage(w http.ResponseWriter, r *http.Request) {
	d := s.researchData(r)
	if s.research != nil {
		if v, err := s.research.Report(r.PathValue("id")); err != nil {
			d.Err = s.flashFor(r.Context(), "research show", err, "Couldn't open that research.")
		} else {
			d.Run = s.researchReport(&v)
		}
	}
	s.render(w, s.tmpl.researchRun, d)
}

// researchLive is the live panel's data for v.
func (s *Server) researchLive(v ResearchLiveView) researchLiveData {
	d := researchLiveData{ResearchLiveView: v, Done: s.researchReport(v.Report)}
	switch {
	case v.Running:
		d.Busy = &BusyView{Title: "Research is running", Since: v.Elapsed}
		if s.approvals != nil {
			d.Waiting = len(s.approvals.Pending())
		}
	case v.Elsewhere:
		d.Busy = &BusyView{Title: "Research is running from the terminal", Detail: "Start another when it finishes."}
	}
	return d
}

func (s *Server) researchHistory(ctx context.Context) researchHistoryData {
	rows, err := s.research.History(researchHistoryLimit)
	if err != nil {
		return researchHistoryData{Err: s.flashFor(ctx, "research history", err, "Couldn't read the list of past research.")}
	}
	var h researchHistoryData
	for _, row := range rows {
		h.Rows = append(h.Rows, researchRow{ResearchRunRow: row, Path: "/research/" + url.PathEscape(row.ID)})
	}
	return h
}

func (s *Server) researchReport(v *ResearchReportView) *researchReportData {
	if v == nil {
		return nil
	}
	d := &researchReportData{ResearchReportView: *v, BodyHTML: reportHTML(v.Body), Path: "/research/" + url.PathEscape(v.ID)}
	d.Refs = append([]ResearchRef(nil), v.Refs...)
	for i, ref := range d.Refs {
		if u, err := url.Parse(ref.URL); err == nil {
			d.Refs[i].own = s.ownHost(u.Hostname())
		}
	}
	return d
}

var (
	reportHead = regexp.MustCompile(`(?m)^#{1,6} +(.+)$\n?`) // the line break too: the heading is a block
	reportCite = regexp.MustCompile(`\[(\d{1,3})\]`)
)

// reportHTML is the report body as HTML, and the XSS boundary: the text
// comes from web pages. It is escaped first (after visibleText); only then
// do heading lines become bold and [n] markers links to #ref-n, so the only
// tags in the result are those two fixed ones. The page's pre-wrap keeps the
// rest of the layout.
func reportHTML(body string) template.HTML {
	h := html.EscapeString(visibleText(body))
	h = reportHead.ReplaceAllString(h, `<strong class="rh">$1</strong>`)
	h = reportCite.ReplaceAllString(h, `<a href="#ref-$1" class="cite">[$1]</a>`)
	return template.HTML(h) //nolint:gosec // escaped above
}

// handleResearchLive streams the live panel: the whole #research-live on
// open (the paused banner clears only on a patch) and at every change, and
// #research-history whenever a run starts or ends. While a run is going it
// also re-renders every 15s so the elapsed time moves; otherwise nothing
// overwrites an answer being typed into the clarify box.
func (s *Server) handleResearchLive(w http.ResponseWriter, r *http.Request) {
	if s.research == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	setSSEHeaders(w)
	flush(w)
	ctx, act := event.Start(r.Context(), Stream, "stream", "research", "remote", r.RemoteAddr)
	defer act.End(nil)
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	lastHistory := "" // never a real key: the first frame carries the history too
	for {
		v, changed := s.research.Watch()
		out, err := s.researchPart("research-live", s.researchLive(v))
		// The list changes when a run starts (its row appears once it has
		// an id) and when it ends, here or in the terminal.
		if key := v.RunID + "|" + strconv.FormatBool(v.Running) + "|" + strconv.FormatBool(v.Elsewhere); err == nil && key != lastHistory {
			lastHistory = key
			var hist string
			if hist, err = s.researchPart("research-history", s.researchHistory(ctx)); err == nil {
				out += "\n" + hist
			}
		}
		if err != nil {
			RenderFailed.Log(ctx, "page", "research live", "err", err)
			return
		}
		if _, err := w.Write(patchElements("", "", out)); err != nil {
			return
		}
		flush(w)
		var every <-chan time.Time
		if v.Running || v.Elsewhere {
			every = tick.C
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-every:
		}
	}
}

// researchPart renders one research partial on its own (an SSE patch).
func (s *Server) researchPart(name string, data any) (string, error) {
	var buf bytes.Buffer
	if err := s.tmpl.researchParts.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// researchText reads a question or answer field: trimmed, and "" with a
// reason when it's too long.
func researchText(r *http.Request, field string) (string, string) {
	t := strings.TrimSpace(r.FormValue(field))
	if utf8.RuneCountInString(t) > researchQuestionMax {
		return "", "That's too long. Keep it to 2,000 characters or fewer."
	}
	return t, ""
}

func (s *Server) handleResearchStart(w http.ResponseWriter, r *http.Request) {
	if s.research == nil {
		redirectFlash(w, r, "/research", "", "")
		return
	}
	q, bad := researchText(r, "question")
	if bad == "" && q == "" {
		bad = "Type a question first."
	}
	if bad != "" {
		redirectFlash(w, r, "/research", "", bad)
		return
	}
	if err := s.research.Start(q, r.FormValue("skip_questions") != ""); err != nil {
		redirectFlash(w, r, "/research", "", s.flashFor(r.Context(), "research start", err, "Couldn't start the research."))
		return
	}
	redirectFlash(w, r, "/research", "Research started. Its progress shows below.", "")
}

func (s *Server) handleResearchClarify(w http.ResponseWriter, r *http.Request) {
	if s.research == nil {
		redirectFlash(w, r, "/research", "", "")
		return
	}
	a, bad := researchText(r, "answer")
	if bad != "" {
		redirectFlash(w, r, "/research", "", bad)
		return
	}
	if err := s.research.Clarify(a); err != nil {
		redirectFlash(w, r, "/research", "", s.flashFor(r.Context(), "research clarify", err, "Couldn't carry on with the research."))
		return
	}
	redirectFlash(w, r, "/research", "Research started. Its progress shows below.", "")
}

func (s *Server) handleResearchStop(w http.ResponseWriter, r *http.Request) {
	switch {
	case s.research == nil:
		redirectFlash(w, r, "/research", "", "")
	case s.research.Stop():
		redirectFlash(w, r, "/research", "Stopping the research. What the helpers found so far stays in the list.", "")
	default:
		redirectFlash(w, r, "/research", "", "Nothing is running, so there's nothing to stop.")
	}
}

func (s *Server) handleResearchReplay(w http.ResponseWriter, r *http.Request) {
	if s.research == nil {
		redirectFlash(w, r, "/research", "", "")
		return
	}
	if err := s.research.Replay(r.PathValue("id")); err != nil {
		redirectFlash(w, r, "/research", "", s.flashFor(r.Context(), "research replay", err, "Couldn't run that again."))
		return
	}
	redirectFlash(w, r, "/research", "Running it again with the current model. Its progress shows below.", "")
}

func (s *Server) handleResearchDelete(w http.ResponseWriter, r *http.Request) {
	if s.research == nil {
		redirectFlash(w, r, "/research", "", "")
		return
	}
	if err := s.research.Delete(r.PathValue("id")); err != nil {
		redirectFlash(w, r, "/research", "", s.flashFor(r.Context(), "research delete", err, "Couldn't delete that research."))
		return
	}
	redirectFlash(w, r, "/research", "Deleted from the list.", "")
}
