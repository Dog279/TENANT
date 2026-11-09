//go:build !windows

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// desktopVersionOS has nothing to read off Windows: Docker Desktop's product
// version is not gated here (Linux uses Docker Engine; macOS Desktop gating is a
// wave-3 follow-up, W3-3). The engine and runc floors still apply.
func desktopVersionOS() string { return "" }

// containerMount canonicalises a POSIX host path into a bind mount. It resolves
// the deepest existing ancestor with EvalSymlinks, so the mount source is the
// real target and never a link that could be swapped, then re-joins any
// not-yet-created tail (W1-3 / H5). A source that is a socket, a named pipe or
// a device is refused: binding one hands the container a channel to a host
// process (the engine socket is the obvious one; W1-2's forbidden list catches
// it by name, this catches it by kind). On Linux and macOS the source and
// destination are the same absolute path.
//
// TODO(W3-2/W3-3): the plan also rejects a symlinked component under a
// WRITABLE root rather than resolving it. macOS puts every temp dir under
// /var -> /private/var, so a blanket refusal would break the work dir there;
// the rule needs the per-OS shape the Linux and macOS builds settle.
func containerMount(hostPath string, ro bool) (mount, error) {
	if hostPath == "" {
		return mount{}, fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return mount{}, fmt.Errorf("resolve: %w", err)
	}
	abs = filepath.Clean(abs)

	resolved, err := resolveExisting(abs)
	if err != nil {
		return mount{}, err
	}
	if strings.ContainsAny(resolved, "\x00\n\r") {
		return mount{}, fmt.Errorf("path contains a control character: %q", resolved)
	}
	if fi, err := os.Lstat(resolved); err == nil {
		if fi.Mode()&(os.ModeSocket|os.ModeNamedPipe|os.ModeDevice|os.ModeCharDevice) != 0 {
			return mount{}, fmt.Errorf("a socket, pipe or device is not allowed as a mount source: %q", resolved)
		}
	}
	return mount{host: resolved, ctr: containerDest(resolved, runtime.GOOS), ro: ro}, nil
}

// resolveExisting resolves symlinks on the deepest existing ancestor of p and
// re-joins the remaining tail, so a not-yet-created leaf under a real parent
// resolves to <real parent>/<leaf>.
func resolveExisting(p string) (string, error) {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r, nil
	}
	var tail []string
	cur := p
	for {
		parent := filepath.Dir(cur)
		tail = append([]string{filepath.Base(cur)}, tail...)
		if parent == cur {
			return p, nil // reached root resolving nothing
		}
		if r, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(append([]string{r}, tail...)...), nil
		}
		cur = parent
	}
}
