package dashboard

// goal.go is the "Autopilot" page (TEN-212): the goal the agent is working on
// by itself, and a Stop. Only the terminal app runs goals, so serve leaves the
// control nil (D16). The page shows the state as of each load; Stop answers
// in place.

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
	Busy       *BusyView // the running goal as a busy line; goal.html adds its Stop
	Msg, Err   string
}

func (s *Server) handleGoalPage(w http.ResponseWriter, r *http.Request) {
	d := s.goalData()
	d.Msg, d.Err = r.URL.Query().Get("msg"), r.URL.Query().Get("err")
	s.render(w, s.tmpl.goal, d)
}

// goalData is the goal as the control says now, with no flash.
func (s *Server) goalData() goalPageData {
	d := goalPageData{layoutData: layoutData{Title: "Autopilot", Page: "goal"}, Configured: s.goal != nil}
	if s.goal != nil {
		d.Goal = s.goal.Show()
		if d.Goal.Active && !d.Goal.Met {
			d.Busy = &BusyView{Title: "Your agent is working on its own", Since: d.Goal.Elapsed,
				Detail: "Stopping lets the step in progress finish, then starts no new one."}
		}
	}
	return d
}

// handleGoalStopForm is POST /goal/stop: the step in progress finishes, and
// the terminal app starts no new one (goalControl.Continue returns "" once
// cleared). The control's Clear can't fail, so there is no raw error to keep
// off the page; "not running" is the one unhappy answer. A Datastar post is
// answered in place, with #goal-body re-rendered.
func (s *Server) handleGoalStopForm(w http.ResponseWriter, r *http.Request) {
	msg, errMsg := "", ""
	switch {
	case s.goal == nil:
	case !s.goal.Clear():
		errMsg = "Autopilot isn't running."
	default:
		msg = "Stopped. The step in progress finishes, then no new step starts."
	}
	s.formDone(w, r, "/goal", msg, errMsg, func() (string, error) { return part(s.tmpl.goal, "goal-body", s.goalData()) })
}
