package dashboard

// research.go is the "Research" page (TEN-210): start a deep-research run,
// watch it live (GET /research/live), and read, re-run or delete past runs.
// The foundation lays the contract (control, views, routes); WP-research
// builds the page, the stream and reportHTML on it.

import (
	"net/http"
	"net/url"
	"strings"
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
}

// ResearchRef is one numbered source of a report.
type ResearchRef struct {
	N   int
	URL string
}

// Web reports whether the source may be a link: only an http or https URL,
// in any letter case. A report's sources come from web pages, so anything
// else (javascript:, data:, a note path) renders as plain text.
func (r ResearchRef) Web() bool {
	u, err := url.Parse(r.URL)
	return err == nil && (strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https"))
}

// ResearchFinding is one helper's finding under a report.
type ResearchFinding struct{ Question, State, Body string }

// SetResearch installs the research control after construction.
func (s *Server) SetResearch(c ResearchControl) { s.research = c }

func (s *Server) mountResearchSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /research", s.handleResearchPage)
	mux.HandleFunc("GET /research/live", s.handleResearchLive) // wins over {id}
	mux.HandleFunc("GET /research/{id}", s.handleResearchRunPage)
}

type researchPageData struct {
	layoutData
	Configured bool
	Msg, Err   string
}

func (s *Server) researchData(r *http.Request) researchPageData {
	return researchPageData{
		layoutData: layoutData{Title: "Research", Page: "research"},
		Configured: s.research != nil,
		Msg:        r.URL.Query().Get("msg"),
		Err:        r.URL.Query().Get("err"),
	}
}

func (s *Server) handleResearchPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, s.tmpl.research, s.researchData(r))
}

func (s *Server) handleResearchRunPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, s.tmpl.researchRun, s.researchData(r))
}

// handleResearchLive is the live panel's stream; nothing to stream yet.
func (s *Server) handleResearchLive(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}
