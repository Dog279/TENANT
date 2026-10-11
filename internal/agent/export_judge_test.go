package agent

import "time"

// SetJudgeCooldownForTest shortens the judge's outage pause; it returns the
// restore.
func SetJudgeCooldownForTest(d time.Duration) func() {
	old := judgeCooldown
	judgeCooldown = d
	return func() { judgeCooldown = old }
}
