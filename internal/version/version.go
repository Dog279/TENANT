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
	// ReleaseKeys is the project's Ed25519 public key(s) the main releases'
	// checksums.txt is signed with (TEN-542): standard base64 of each 32-byte
	// key, up to three, comma-separated (selfupdate.ParseKeys). Set by -X at
	// release time from the repository variable TENANT_RELEASE_PUBKEYS; empty
	// in a fork's release and every development build, which then update as
	// they did before. Nothing at run time sets or overrides it.
	ReleaseKeys = ""
)
