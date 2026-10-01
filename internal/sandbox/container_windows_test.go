//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestSpecForDenyRoots_ShortName is the regression test for the Windows CI break:
// the runner's temp lives under an 8.3 short name (RUNNER~1), and comparing a
// short-named path on one side against an EvalSymlinks-expanded long name on the
// other let the deny-root guard silently miss. specFor must fire regardless of
// which spelling each side uses.
func TestSpecForDenyRoots_ShortName(t *testing.T) {
	b := Container(ContainerOptions{})
	base := t.TempDir()
	deny := filepath.Join(base, "has-a-long-enough-name-for-8dot3")
	if err := os.MkdirAll(deny, 0o755); err != nil {
		t.Fatal(err)
	}
	short := shortPath(t, deny) // e.g. ...\HAS-AL~1
	if short == deny {
		t.Skip("8.3 short names are disabled on this volume; nothing to test")
	}

	// The exact CI shape: the work dir equals the deny root, but named by its 8.3
	// SHORT form. containerMount keeps the short form; the deny side gets
	// EvalSymlinks-expanded to the long form. Unless both sides are canonicalised
	// the same way, the guard misses and this run is NOT refused (the bug).
	if _, err := b.specFor(Command{Shell: "echo hi"}, Policy{WorkDir: short, DenyRoots: []string{short}}); err == nil {
		t.Error("a work dir equal to a deny root (named by its 8.3 short name) must fail closed")
	}
}

func shortPath(t *testing.T, p string) string {
	t.Helper()
	long, err := windows.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, 512)
	n, err := windows.GetShortPathName(long, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || int(n) > len(buf) {
		return p // no short name available
	}
	return windows.UTF16ToString(buf[:n])
}
