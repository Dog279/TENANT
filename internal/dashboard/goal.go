package dashboard

// goal.go is the "Autopilot" page (TEN-212): the goal the agent is working on
// by itself, and a Stop. Only the terminal app runs goals, so serve leaves the
// control nil. The foundation lays the contract; WP-goal builds the page.

import "net/http"

// GoalControl is the read-and-stop goal surface.
type GoalControl interface {
	Show() GoalStatusView
	Clear() bool // true when a running goal was stopped
}

// GoalStatusView is the goal loop's state, render-ready.
type GoalStatusView struct {
	Active, Met bool
	Condition   string
	Turns       int // steps finished
	MaxTurns    int
	Elapsed     string // e.g. "12m30s"
	LastCheck   string // latest progress check, "" = none yet
}

// SetGoal installs the goal control after construction.
func (s *Server) SetGoal(c GoalControl) { s.goal = c }

func (s *Server) mountGoalSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /goal", s.handleGoalPage)
}

type goalPageData struct {
	layoutData
	Configured bool
	Msg, Err   string
}

func (s *Server) handleGoalPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, s.tmpl.goal, goalPageData{
		layoutData: layoutData{Title: "Autopilot", Page: "goal"},
		Configured: s.goal != nil,
		Msg:        r.URL.Query().Get("msg"),
		Err:        r.URL.Query().Get("err"),
	})
}
