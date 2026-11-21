//go:build !linux && !windows && !darwin

package procprotect

// No controls on this system, so nothing to attack.

var (
	controlIDs   []string
	attempts     []attempt
	stillAllowed map[string]bool
)

func attackerTooStrong() string     { return "" }
func cleanupMachine()               {}
func victimReady()                  {}
func selfState() map[string]string  { return nil }
func childState() map[string]string { return nil }
