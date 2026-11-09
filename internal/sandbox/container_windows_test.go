//go:build windows

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// W1-3: a junction in ANY component of a mount source is refused — Docker
// Desktop follows a junction named as a source to its target, so one planted
// in the work dir's path would mount somewhere else. Junctions need no
// elevation, so this runs on any Windows box.
func TestContainerMount_RejectsJunction(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	link := filepath.Join(base, "link")
	if err := os.MkdirAll(filepath.Join(target, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J failed (%v): %s", err, out)
	}
	t.Cleanup(func() { os.Remove(link) })
	for _, p := range []string{link, filepath.Join(link, "sub"), filepath.Join(link, "sub", "not-yet-created")} {
		if _, err := containerMount(p, false); err == nil || !strings.Contains(err.Error(), "junction") {
			t.Errorf("containerMount(%q) = %v, want a junction refusal", p, err)
		}
	}
	// The real target is fine.
	if _, err := containerMount(target, false); err != nil {
		t.Errorf("the junction's target must be accepted: %v", err)
	}
}

// W1-3: UNC and device paths, alternate data streams, and a trailing dot or
// space in ANY component are refused before the filesystem is consulted.
func TestContainerMount_RejectsNameConfusions(t *testing.T) {
	for p, want := range map[string]string{
		`\\server\share\x`:         "UNC",
		`\\?\C:\x`:                 "UNC",
		`\\.\pipe\docker_engine`:   "UNC",
		`C:\x\file.txt:stream`:     "alternate data stream",
		`C:\Users\x.\y`:            "trailing dot or space",
		`C:\Users\x \y`:            "trailing dot or space",
		`C:\Users\y\leaf.`:         "trailing dot or space",
	} {
		_, err := containerMount(p, false)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("containerMount(%q) = %v, want a %q refusal", p, err, want)
		}
	}
}

// W1-3: an 8.3 short name is expanded to the long name, so the engine, the
// deny-root comparison and the operator all see one spelling.
func TestContainerMount_ExpandsShortName(t *testing.T) {
	base := t.TempDir()
	long := filepath.Join(base, "a-long-enough-directory-name")
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatal(err)
	}
	short := shortPath(t, long)
	if strings.EqualFold(short, long) {
		t.Skip("8.3 short names are disabled on this volume")
	}
	m, err := containerMount(short, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(m.host, longPathWindows(long)) || strings.Contains(m.host, "~") {
		t.Errorf("mount source %q, want the long form of %q", m.host, long)
	}
	// A not-yet-created leaf under a short-named parent still expands the parent.
	m, err = containerMount(filepath.Join(short, "new-leaf"), false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.host, "~") || !strings.HasSuffix(m.host, `\new-leaf`) {
		t.Errorf("mount source %q, want the long parent plus new-leaf", m.host)
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
