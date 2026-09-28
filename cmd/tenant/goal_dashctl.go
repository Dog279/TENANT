package main

// goal_dashctl.go: the dashboard's Autopilot page over the terminal app's
// goalControl (TEN-212). WP-goal fills it in.

import "tenant/internal/dashboard"

// newDashGoal adapts gc; notify tells the terminal feed about a Stop. nil
// (an untyped nil interface) leaves the page showing "isn't available".
func newDashGoal(gc *goalControl, notify func(string)) dashboard.GoalControl {
	return nil
}
