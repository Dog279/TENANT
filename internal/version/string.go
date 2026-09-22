package version

import "fmt"

// String returns a human-readable version string containing all build provenance.
func String() string {
	return fmt.Sprintf("%s (commit=%s, built=%s, target=%s, dirty=%s)",
		Version, shortCommit(Commit), BuildTime, Target, Dirty)
}

func shortCommit(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}
