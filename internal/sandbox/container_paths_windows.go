//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// probeDesktop reads the Docker Desktop product version from the uninstall key
// (HKCU first, then HKLM) and warns when it is below the floor (W1-1/H3). The
// obvious `docker desktop version` reports the CLI plugin's version instead, so
// it is not used. A missing key is not an error: a headless dockerd or Podman
// has no Desktop version, and the engine floor still applies.
func (b *containerBackend) probeDesktop(ctx context.Context) string {
	ver := desktopVersion()
	if ver == "" {
		return ""
	}
	if cmp, ok := compareVersions(ver, minDesktopVersion); ok && cmp < 0 {
		return fmt.Sprintf("Docker Desktop %s is below the floor %s (CVE-2025-9074: a networked container could take over the host)", ver, minDesktopVersion)
	}
	return ""
}

func desktopVersion() string {
	for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
		k, err := registry.OpenKey(root, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\Docker Desktop`, registry.QUERY_VALUE|registry.WOW64_64KEY)
		if err != nil {
			continue
		}
		v, _, err := k.GetStringValue("DisplayVersion")
		k.Close()
		if err == nil && v != "" {
			return v
		}
	}
	return ""
}

// containerMount canonicalises a Windows host path into a bind mount, rejecting
// everything the live probe showed Docker Desktop resolves in a way that escapes
// the declared roots (W1-3 / H5):
//
//   - a UNC or device path (\\server\share, \\?\, \\.\);
//   - a reparse point (junction or symlink) in ANY component of the path, since
//     Docker Desktop follows a junction named as a mount source to its target;
//   - an alternate data stream (a ':' past the drive), a trailing dot or space.
//
// The source is always a native, canonical Windows path; the destination is the
// /mnt/<drive>/... form (containerDest). It resolves against the nearest
// existing ancestor so a not-yet-created leaf under a real, safe parent is fine.
func containerMount(hostPath string, ro bool) (mount, error) {
	if hostPath == "" {
		return mount{}, fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return mount{}, fmt.Errorf("resolve: %w", err)
	}
	abs = filepath.Clean(abs)

	if strings.HasPrefix(abs, `\\`) {
		return mount{}, fmt.Errorf("UNC and device paths are not allowed as a mount source: %q", abs)
	}
	vol := filepath.VolumeName(abs)
	if !(len(vol) == 2 && vol[1] == ':') {
		return mount{}, fmt.Errorf("a mount source must be a drive path like C:\\...: %q", abs)
	}
	// An alternate data stream is a ':' after the drive colon.
	if strings.Contains(abs[len(vol):], ":") {
		return mount{}, fmt.Errorf("alternate data streams are not allowed: %q", abs)
	}

	// Reject a reparse point in any existing component. Walk from the drive root
	// down; stop at the first component that does not exist yet (a leaf to be
	// created), and require every existing ancestor to be a real directory.
	if err := rejectReparseWindows(abs, vol); err != nil {
		return mount{}, err
	}

	// A trailing dot or space in the leaf is a classic Windows name-confusion; Abs
	// keeps them, so reject explicitly.
	leaf := filepath.Base(abs)
	if strings.HasSuffix(leaf, ".") || strings.HasSuffix(leaf, " ") {
		return mount{}, fmt.Errorf("a trailing dot or space is not allowed in %q", leaf)
	}

	return mount{host: abs, ctr: containerDest(abs, "windows"), ro: ro}, nil
}

// rejectReparseWindows returns an error if any existing component of abs (from
// the drive root to the leaf) is a reparse point.
func rejectReparseWindows(abs, vol string) error {
	rest := strings.TrimPrefix(abs, vol)
	rest = strings.TrimPrefix(rest, `\`)
	cur := vol + `\`
	comps := strings.Split(rest, `\`)
	for i, comp := range comps {
		if comp == "" {
			continue
		}
		cur = filepath.Join(cur, comp)
		attrs, err := fileAttributes(cur)
		if err != nil {
			// This component does not exist yet: the remaining tail is to be
			// created under a real parent, which is fine.
			return nil
		}
		if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("a junction or symlink in the path is not allowed as a mount source: %q", cur)
		}
		if i < len(comps)-1 && attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return fmt.Errorf("%q is not a directory", cur)
		}
	}
	return nil
}

func fileAttributes(p string) (uint32, error) {
	p16, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, err
	}
	attrs, err := windows.GetFileAttributes(p16)
	if err != nil {
		return 0, err
	}
	return attrs, nil
}

// ensure os is referenced on Windows builds (kept for parity with the POSIX file).
var _ = os.Stat
