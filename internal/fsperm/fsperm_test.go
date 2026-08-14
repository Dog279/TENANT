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
