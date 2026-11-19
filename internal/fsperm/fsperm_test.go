//go:build !windows

package fsperm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateWritersTightenExistingPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	assertAtMost(t, dir, PrivateDirMode)

	path := filepath.Join(dir, "artifact")
	if err := os.WriteFile(path, []byte("old"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	assertAtMost(t, path, PrivateFileMode)
}

func TestPrepareFileCreatesPrivateDatabaseWithoutTighteningExistingParent(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "existing")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "tenant.db")
	if err := PrepareFile(path); err != nil {
		t.Fatal(err)
	}
	assertAtMost(t, path, PrivateFileMode)
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o055 == 0 {
		t.Fatalf("existing database parent was unexpectedly tightened: %v", info.Mode().Perm())
	}

	newParent := filepath.Join(root, "new", "nested")
	if err := PrepareFile(filepath.Join(newParent, "tenant.db")); err != nil {
		t.Fatal(err)
	}
	assertAtMost(t, newParent, PrivateDirMode)
}

func TestRepairTreeTightensWithoutFollowingSymlinks(t *testing.T) {
	if !Supported() {
		t.Skip("POSIX modes are not enforceable on this platform")
	}
	root := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(root, 0o777); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "report.md")
	if err := os.WriteFile(path, []byte("private"), 0o666); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(external, []byte("outside"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	fixed, err := RepairTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if fixed != 2 {
		t.Fatalf("fixed = %d, want 2", fixed)
	}
	assertAtMost(t, root, PrivateDirMode)
	assertAtMost(t, path, PrivateFileMode)
	info, err := os.Stat(external)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 == 0 {
		t.Fatalf("external symlink target was tightened: %v", info.Mode().Perm())
	}
}

func assertAtMost(t *testing.T, path string, maximum os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if extra := info.Mode().Perm() &^ maximum; extra != 0 {
		t.Errorf("%s mode = %v, exceeds %v", path, info.Mode().Perm(), maximum)
	}
}

// MkdirAll tightens the final directory when it was there already (TEN-328).
// That takes permission bits away from that one directory and never adds
// one, and it touches nothing else: not a file in it, not a folder in it,
// not the folder above. (TEN-535 round three asked the same of Windows,
// where the first version of the access-list change widened a file.)
func TestMkdirAllOnADirectoryThatWasThereOnlyTakesBitsFromIt(t *testing.T) {
	above := filepath.Join(t.TempDir(), "above")
	dir := filepath.Join(above, "was there")
	sub, file := filepath.Join(dir, "sub"), filepath.Join(dir, "file")
	for _, d := range []string{above, dir, sub} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	modes := map[string]os.FileMode{above: 0o755, dir: 0o750, sub: 0o755, file: 0o640}
	for p, m := range modes {
		if err := os.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	for p, was := range modes {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		got := info.Mode().Perm()
		switch {
		case p == dir && (got&^was != 0 || got&^PrivateDirMode != 0):
			t.Errorf("%s: mode %v, was %v; want bits taken away only, down to %v", p, got, was, PrivateDirMode)
		case p != dir && got != was:
			t.Errorf("%s: mode %v, was %v; MkdirAll of the directory beside or above it must not touch it", p, got, was)
		}
	}
	// What it makes itself is private from the first instant, parents too.
	made := filepath.Join(above, "new", "nested")
	if err := MkdirAll(made); err != nil {
		t.Fatal(err)
	}
	assertAtMost(t, made, PrivateDirMode)
	assertAtMost(t, filepath.Dir(made), PrivateDirMode)
}
