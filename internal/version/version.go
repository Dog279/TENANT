// Package version provides build-time-injected version information.
package version

import "runtime"

var (
	// These values are replaced with -ldflags during release builds.
	Version   = "0.0.1"
	Commit    = "unknown"
	BuildTime = "unknown"
	Target    = runtime.GOOS + "/" + runtime.GOARCH
	Dirty     = "false"
	// Repo is the owner/name of the GitHub repository releases come from; set
	// by -X at release time (GITHUB_REPOSITORY); empty in dev builds, which is
	// how `tenant update` tells a dev build from a release.
	Repo = ""
)
