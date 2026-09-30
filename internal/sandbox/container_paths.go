package sandbox

// container_paths.go holds the PURE path mapping the container backend uses, so
// container_paths_test.go can pin the Windows mapping on any OS. The actual
// filesystem canonicalisation — which resolves and rejects reparse points, and
// so must make real OS calls — is in the build-tagged container_paths_windows.go
// and container_paths_other.go.

import (
	"path/filepath"
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
	vol := filepath.VolumeName(hostAbs) // "C:" for C:\x, `\\srv\share` for UNC
	if len(vol) == 2 && vol[1] == ':' {
		drive := strings.ToLower(vol[:1])
		rest := filepath.ToSlash(hostAbs[len(vol):])
		if !strings.HasPrefix(rest, "/") {
			rest = "/" + rest
		}
		return "/mnt/" + drive + rest
	}
	// A UNC or driveless path: the caller rejects these before mapping, but map
	// defensively under a distinct prefix so a leak is obvious, never silent.
	return "/mnt/host/" + strings.Trim(filepath.ToSlash(hostAbs), "/")
}

// withinHost reports whether p is root or inside it, both absolute, matched the
// way the host filesystem does: case-insensitively on Windows and macOS. It is
// the deny-root check for a mount source.
func withinHost(p, root string) bool { return within(p, root) }
