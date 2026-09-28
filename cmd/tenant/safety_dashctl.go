package main

// safety_dashctl.go: the facts the dashboard's Approvals & safety page needs
// beside the broker (D11). WP-safety adds whether commands are sandboxed.

import "tenant/internal/dashboard"

// newSafetyFacts reads the facts fresh on each call, so a profile edited
// while Tenant runs shows on the next page load.
func newSafetyFacts() func() dashboard.SafetyFacts {
	return func() dashboard.SafetyFacts { return dashboard.SafetyFacts{Account: runAs()} }
}
