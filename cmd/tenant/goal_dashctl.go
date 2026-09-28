package main

// goal_dashctl.go: the dashboard's Autopilot page over the terminal app's
// goalControl (TEN-212). It is the same goalControl the TUI drives (D16), so
// the page shows the real loop, and a Stop there is the TUI's /goal clear.

import (
	"strings"

	"tenant/internal/dashboard"
)

// dashGoal adapts goalControl to dashboard.GoalControl.
type dashGoal struct {
	gc     *goalControl
	notify func(string)
}

// newDashGoal adapts gc; notify tells the terminal feed about a Stop. nil
// (an untyped nil interface) leaves the page showing "isn't available".
func newDashGoal(gc *goalControl, notify func(string)) dashboard.GoalControl {
	if gc == nil {
		return nil
	}
	return dashGoal{gc: gc, notify: notify}
}

// Show maps the TUI snapshot. A met goal stays set only until the TUI clears
// it; Met is reported only while the condition is still there.
func (d dashGoal) Show() dashboard.GoalStatusView {
	st := d.gc.Show()
	return dashboard.GoalStatusView{
		Active:    st.Active && !st.Met,
		Met:       st.Active && st.Met,
		Condition: st.Condition,
		Turns:     st.Turns,
		MaxTurns:  st.MaxTurns,
		Elapsed:   st.ElapsedFmt,
		LastCheck: plainCheck(st.LastJudge),
	}
}

// Clear stops a running goal after its step in progress (D17): Continue
// returns "" once cleared, so the TUI starts nothing more. The TUI feed hears
// about it, since the operator there didn't do it.
func (d dashGoal) Clear() bool {
	s, ok := d.gc.stopIfRunning()
	if ok && d.notify != nil {
		d.notify(s + " (stopped from the dashboard)")
	}
	return ok
}

// plainCheck keeps goalctl's own status texts off the page (D21); the judge's
// reason, the model's words, passes through for visibleText.
func plainCheck(s string) string {
	switch {
	case s == "judge returned empty response", s == "judge response was unparseable":
		return "The check gave no clear answer."
	case s == "goal achieved", strings.HasPrefix(s, "turn cap reached"):
		return "" // the loop is ending; the page says so on its next load
	}
	return s
}
