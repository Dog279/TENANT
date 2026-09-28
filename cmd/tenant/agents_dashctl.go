package main

// agents_dashctl.go: the dashboard's Helpers page over the terminal app's
// agentControl (TEN-209). WP-agents fills it in.

import "tenant/internal/dashboard"

// newDashAgents adapts ac; nil (an untyped nil interface) leaves the page
// showing "isn't available".
func newDashAgents(ac *agentControl) dashboard.AgentControl {
	return nil
}
