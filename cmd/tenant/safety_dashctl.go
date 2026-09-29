package main

// safety_dashctl.go: the facts the dashboard's Approvals & safety page needs
// beside the broker (D11): who commands run as, and whether they run in the
// sandbox. They decide the Allow consequence for Run commands and whether a
// request offers "Allow for the rest of this session".

import (
	"os/user"
	"strings"

	"tenant/internal/dashboard"
)

// newSafetyFacts reads the facts fresh on each call, so a profile edited
// while Tenant runs shows on the next page load.
func newSafetyFacts() func() dashboard.SafetyFacts {
	return func() dashboard.SafetyFacts {
		return dashboard.SafetyFacts{Account: runAs(), System: runsAsSystem(), Sandboxed: execSandboxed()}
	}
}

// execSandboxed reports whether commands the main broker asks about run in
// the sandbox: the dashboard's, the terminal's and serve's chat all run
// under the "local" exec profile, resolved as the spawn site resolves it.
// Scheduled jobs without exec have no command tool. Sandboxed means the
// restricted backend with enforce on, so a refused command asks before it
// leaves the box instead of re-running outside it; a dry run is not.
func execSandboxed() bool {
	p := resolveExecProfile(surfaceLocal, "", "", "")
	return p.Backend == "restricted" && p.Policy.Enforce
}

// runsAsSystem reports whether this is Windows' SYSTEM account: by SID,
// which reads the same in every language, or by its English name.
func runsAsSystem() bool {
	u, err := user.Current()
	return err == nil && (u.Uid == "S-1-5-18" || strings.EqualFold(u.Username, `NT AUTHORITY\SYSTEM`))
}
