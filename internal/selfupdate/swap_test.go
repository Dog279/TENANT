package selfupdate

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPrevPath(t *testing.T) {
	for exe, want := range map[string]string{
		"/usr/local/bin/tenant":  "/usr/local/bin/tenant.prev",
		`C:\Tenant\tenant.exe`:   `C:\Tenant\tenant.prev.exe`,
		"tenant.EXE":             "tenant.prev.EXE",
		"/opt/tenant-1.2/tenant": "/opt/tenant-1.2/tenant.prev",
	} {
		if got := PrevPath(exe); got != want {
			t.Errorf("PrevPath(%q) = %q, want %q", exe, got, want)
		}
	}
}

// TestSwapNoGap runs real shell scripts as the binaries, so "runnable" is
// proven by running them, not by reading bytes.
func TestSwapNoGap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Unix swap path")
	}
	t.Cleanup(func() { linkFn, renameFn = os.Link, os.Rename })
	dir := t.TempDir()
	exe, prev := filepath.Join(dir, "tenant"), PrevPath(filepath.Join(dir, "tenant"))
	run := func(path string) string {
		t.Helper()
		out, err := exec.Command(path).Output()
		if err != nil {
			t.Fatalf("run %s: %v", path, err)
		}
		return strings.TrimSpace(string(out))
	}
	stage := func(says string) string {
		t.Helper()
		p := filepath.Join(dir, "staged")
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho "+says+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	absent := func(path, why string) {
		t.Helper()
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s exists: %s", path, why)
		}
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho old\n"), 0o750); err != nil {
		t.Fatal(err)
	}

	// A failure between the link and the rename: exe untouched, still old,
	// still runnable, and no .prev left to mislead a rollback.
	renameFn = func(string, string) error { return errors.New("injected") }
	if err := Swap(exe, stage("new")); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("Swap with a failing rename = %v", err)
	}
	renameFn = os.Rename
	if got := run(exe); got != "old" {
		t.Fatalf("after a failed swap exe says %q, want old", got)
	}
	absent(prev, "a failed swap must not leave a prev")

	// The hard link holds the old binary under both names before the rename.
	linked := false
	linkFn = func(oldname, newname string) error {
		if err := os.Link(oldname, newname); err != nil {
			return err
		}
		if a, b := run(oldname), run(newname); a != "old" || b != "old" {
			t.Errorf("after Link: exe says %q, prev says %q; want both old", a, b)
		}
		linked = true
		return nil
	}
	staged := stage("new")
	if err := Swap(exe, staged); err != nil {
		t.Fatal(err)
	}
	linkFn = os.Link
	if !linked {
		t.Fatal("the hard-link path was not taken")
	}
	if a, b := run(exe), run(prev); a != "new" || b != "old" {
		t.Fatalf("after Swap: exe says %q, prev says %q; want new and old", a, b)
	}
	absent(staged, "the staged file must be renamed in, not copied")
	if info, _ := os.Stat(exe); info.Mode().Perm() != 0o750 {
		t.Fatalf("exe mode = %o, want the old binary's 0750", info.Mode().Perm())
	}

	// Rollback restores the old binary and consumes prev.
	if err := Rollback(exe); err != nil {
		t.Fatal(err)
	}
	if got := run(exe); got != "old" {
		t.Fatalf("after Rollback exe says %q, want old", got)
	}
	absent(prev, "rollback must consume prev")
	if err := Rollback(exe); err == nil {
		t.Fatal("a second Rollback has nothing to restore and must say so")
	}

	// No hard links (the fallback): two renames, and a failed second rename
	// puts the old binary back.
	linkFn = func(string, string) error { return errors.New("operation not permitted") }
	renameFn = func(string, string) error { return errors.New("injected") }
	if err := Swap(exe, stage("new")); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("fallback Swap with a failing rename = %v", err)
	}
	if got := run(exe); got != "old" {
		t.Fatalf("after a failed fallback swap exe says %q, want old", got)
	}
	absent(prev, "a failed fallback swap must put the old binary back under its own name")
	renameFn = os.Rename
	if err := Swap(exe, stage("new")); err != nil {
		t.Fatal(err)
	}
	if a, b := run(exe), run(prev); a != "new" || b != "old" {
		t.Fatalf("after fallback Swap: exe says %q, prev says %q; want new and old", a, b)
	}
}

// TestSwapWindows is compile-checked everywhere and runs on Windows CI only.
func TestSwapWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the Windows swap path")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "tenant.exe")
	prev := PrevPath(exe)
	if filepath.Base(prev) != "tenant.prev.exe" {
		t.Fatalf("prev = %s, want tenant.prev.exe", prev)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(b)
	}
	staged := filepath.Join(dir, "tenant.new.exe")
	write(exe, "old")
	write(staged, "new")

	// An old .prev held open (as the still-running previous version holds its
	// image) cannot be removed: the distinct error, and nothing touched.
	write(prev, "older")
	h, err := os.Open(prev)
	if err != nil {
		t.Fatal(err)
	}
	err = Swap(exe, staged)
	if !errors.Is(err, ErrPrevInUse) || !strings.Contains(err.Error(), "restart Tenant, then retry") {
		t.Fatalf("Swap over a held prev = %v, want ErrPrevInUse", err)
	}
	if read(exe) != "old" || read(staged) != "new" {
		t.Fatal("a refused swap must touch nothing")
	}
	_ = h.Close()

	// The two-rename path.
	if err := Swap(exe, staged); err != nil {
		t.Fatal(err)
	}
	if read(exe) != "new" || read(prev) != "old" {
		t.Fatalf("after Swap: exe = %q, prev = %q; want new and old", read(exe), read(prev))
	}
	if _, err := os.Stat(staged); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the staged file must be renamed in, not copied")
	}

	// Rollback parks the new binary and puts the old one back.
	if err := Rollback(exe); err != nil {
		t.Fatal(err)
	}
	if read(exe) != "old" {
		t.Fatalf("after Rollback exe = %q, want old", read(exe))
	}
	if _, err := os.Stat(prev); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("rollback must consume prev")
	}
}

// TestSwapWaitsForAHeldBinary: on Windows a binary that has just run can
// still be open in another handle for an instant (its own process on the way
// out, a virus scanner), and a rename of it is refused meanwhile. Seen on
// windows-latest, 2026-11-27: a fleet machine's update rolled back at this
// rename, right after the trial run of the new binary. Swap and Rollback
// wait for the handle to go.
func TestSwapWaitsForAHeldBinary(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows refuses to rename a file another handle has open")
	}
	dir := t.TempDir()
	exe, staged := filepath.Join(dir, "tenant.exe"), filepath.Join(dir, "staged.exe")
	for path, content := range map[string]string{exe: "old", staged: "new"} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hold := func(path string) {
		t.Helper()
		h, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		time.AfterFunc(300*time.Millisecond, func() { _ = h.Close() })
	}
	hold(staged)
	if err := Swap(exe, staged); err != nil {
		t.Fatalf("Swap of a staged binary held for an instant = %v", err)
	}
	hold(exe) // the new binary, as it is just after its process ended
	if err := Rollback(exe); err != nil {
		t.Fatalf("Rollback of a binary held for an instant = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("after Rollback exe = %q, want old", b)
	}
}
