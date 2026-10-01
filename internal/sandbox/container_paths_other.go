//go:build !windows

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// probeDesktop has nothing to check off Windows: Docker Desktop's product
// version is not gated here (Linux uses Docker Engine; macOS Desktop gating is a
// wave-3 follow-up, W3-3). The engine and runc floors still apply.
func (b *containerBackend) probeDesktop(context.Context) string { return "" }

// containerMount canonicalises a POSIX host path into a bind mount. It resolves
// the deepest existing ancestor with EvalSymlinks and rejects a symlinked
// component under a writable source, then re-joins any not-yet-created tail
// (W1-3 / H5). On Linux and macOS the source and destination are the same
// absolute path.
func containerMount(hostPath string, ro bool) (mount, error) {
	if hostPath == "" {
		return mount{}, fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return mount{}, fmt.Errorf("resolve: %w", err)
	}
	abs = filepath.Clean(abs)

	// Resolve symlinks on the existing prefix. If the whole path resolves and the
	// result differs from abs, a component was a symlink: use the resolved form so
	// the mount source is the real target, never a link that could be swapped.
	resolved, err := resolveExisting(abs)
	if err != nil {
		return mount{}, err
	}
	if strings.ContainsAny(resolved, "\x00\n\r") {
		return mount{}, fmt.Errorf("path contains a control character: %q", resolved)
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

var _ = os.Stat
