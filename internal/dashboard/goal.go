package dashboard

// goal.go is the "Autopilot" page (TEN-212): the goal the agent is working on
// by itself, and a Stop. Only the terminal app runs goals, so serve leaves the
// control nil (D16). The page is static: it shows the state as of each load.

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
	// Stop moves toward safety, so it has no confirm step (D3, D17).
	mux.HandleFunc("POST /goal/stop", s.handleGoalStopForm)
}

type goalPageData struct {
	layoutData
	Configured bool
	Goal       GoalStatusView
	Busy       *BusyView // the running goal as a ui-busy line, with its Stop
	Msg, Err   string
}

func (s *Server) handleGoalPage(w http.ResponseWriter, r *http.Request) {
	d := goalPageData{
		layoutData: layoutData{Title: "Autopilot", Page: "goal"},
		Configured: s.goal != nil,
		Msg:        r.URL.Query().Get("msg"),
		Err:        r.URL.Query().Get("err"),
	}
	if s.goal != nil {
		d.Goal = s.goal.Show()
		if d.Goal.Active && !d.Goal.Met {
			d.Busy = &BusyView{Title: "Your agent is working on its own", Since: d.Goal.Elapsed,
				Detail: "Stopping lets the step in progress finish, then starts no new one.",
				Stop:   "/goal/stop", StopLabel: "Stop after this step"}
		}
	}
	s.render(w, s.tmpl.goal, d)
}

// handleGoalStopForm is POST /goal/stop: the step in progress finishes, and
// the terminal app starts no new one (goalControl.Continue returns "" once
// cleared). The control's Clear can't fail, so there is no raw error to keep
// off the page; "not running" is the one unhappy answer.
func (s *Server) handleGoalStopForm(w http.ResponseWriter, r *http.Request) {
	switch {
	case s.goal == nil:
		redirectFlash(w, r, "/goal", "", "")
	case !s.goal.Clear():
		redirectFlash(w, r, "/goal", "", "Autopilot isn't running.")
	default:
		redirectFlash(w, r, "/goal", "Stopped. The step in progress finishes, then no new step starts.", "")
	}
}
