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
)
