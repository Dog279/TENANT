package sandbox

// container_paths.go holds the PURE path mapping the container backend uses, so
// container_paths_test.go can pin the Windows mapping on any OS. The actual
// filesystem canonicalisation — which resolves and rejects reparse points, and
// so must make real OS calls — is in the build-tagged container_paths_windows.go
// and container_paths_other.go.

import (
	"strings"
)

// containerDest maps a canonical, absolute HOST path to the container path it is
// mounted at (plan §3.5). On Windows, C:\x\y becomes /mnt/c/x/y, the WSL
// convention the model already knows; the drive is lower-cased. Elsewhere the
// path is used unchanged. It is only ever a DESTINATION: a source is always the
// native host path, never this form (the probe showed /mnt/c as a source lands
// on the wrong disk).
func containerDest(hostAbs, goos string) string {
	if goos != "windows" {
		return hostAbs
	}
	// Parse the drive letter directly rather than with filepath.VolumeName, which
	// only recognises Windows drives when the host itself is Windows — this pure
	// mapping runs cross-OS (tests and the Linux/macOS CI runners).
	if len(hostAbs) >= 2 && hostAbs[1] == ':' && isDriveLetter(hostAbs[0]) {
		drive := strings.ToLower(hostAbs[:1])
		rest := strings.ReplaceAll(hostAbs[2:], `\`, "/")
		if !strings.HasPrefix(rest, "/") {
			rest = "/" + rest
		}
		return "/mnt/" + drive + rest
	}
	// A UNC or driveless path: the caller rejects these before mapping, but map
	// defensively under a distinct prefix so a leak is obvious, never silent.
	return "/mnt/host/" + strings.Trim(strings.ReplaceAll(hostAbs, `\`, "/"), "/")
}

func isDriveLetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// withinHost reports whether p is root or inside it, both absolute, matched the
// way the host filesystem does: case-insensitively on Windows and macOS. It is
// the deny-root check for a mount source.
func withinHost(p, root string) bool { return within(p, root) }
