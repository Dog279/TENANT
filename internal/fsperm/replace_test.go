package fsperm

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// leftovers lists the temporary files Replace left in dir.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The power-cut regression (found on Windows Server 2025, 2026-11-17): a
// save renamed its temporary file into place without flushing it, the power
// went within seconds, and config.json came back full of zero bytes. The new
// bytes must reach the disk while the old file still has its name.
func TestReplaceFlushesBeforeRename(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(path string) error
	}{
		{"Replace", func(p string) error { return Replace(p, []byte("new")) }},
		{"ReplaceWithMode", func(p string) error { return ReplaceWithMode(p, []byte("new"), 0o644) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			real := syncFile
			t.Cleanup(func() { syncFile = real })
			flushes := 0
			syncFile = func(f *os.File) error {
				flushes++
				if b, _ := os.ReadFile(path); string(b) != "old" {
					t.Errorf("at the flush, %s holds %q; the rename came first", filepath.Base(path), b)
				}
				if b, _ := os.ReadFile(f.Name()); string(b) != "new" {
					t.Errorf("at the flush, the temporary file holds %q, want the new bytes", b)
				}
				return real(f)
			}
			if err := tc.write(path); err != nil {
				t.Fatal(err)
			}
			if flushes != 1 {
				t.Errorf("flushed %d times, want 1", flushes)
			}
			if b, _ := os.ReadFile(path); string(b) != "new" {
				t.Errorf("after the write the file holds %q", b)
			}
			if m := leftovers(t, dir); len(m) != 0 {
				t.Errorf("temporary files left behind: %v", m)
			}
		})
	}
}

// A flush that fails means the bytes may not be on the disk, so the old file
// keeps its name and the caller hears about it.
func TestReplaceKeepsTheOldFileWhenTheFlushFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	real := syncFile
	t.Cleanup(func() { syncFile = real })
	gone := errors.New("the disk went away")
	syncFile = func(*os.File) error { return gone }
	if err := Replace(path, []byte("new")); !errors.Is(err, gone) {
		t.Fatalf("Replace = %v, want the flush's error", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Errorf("the file holds %q after a failed flush, want the old contents", b)
	}
	if m := leftovers(t, dir); len(m) != 0 {
		t.Errorf("temporary files left behind: %v", m)
	}
}

// Replace makes a private file and creates one that was not there;
// ReplaceWithMode gives the file the mode it was asked for.
func TestReplaceModes(t *testing.T) {
	dir := t.TempDir()
	private, shared := filepath.Join(dir, "private"), filepath.Join(dir, "shared")
	if err := Replace(private, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceWithMode(shared, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return // no Unix modes to compare
	}
	for path, want := range map[string]os.FileMode{private: PrivateFileMode, shared: 0o644} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o", filepath.Base(path), got, want)
		}
	}
}

// A power cut between the write and the rename leaves the temporary file
// behind (seen on Linux, 2026-11-17). The next save of the same path removes
// it once it is old; a young one may be another writer's and stays, and so
// does any file that is not this path's.
func TestReplaceRemovesStaleTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	old := time.Now().Add(-2 * staleAfter)
	stale := filepath.Join(dir, "config.json.123.tmp")
	young := filepath.Join(dir, "config.json.456.tmp")
	other := filepath.Join(dir, "settings.main.json.789.tmp")
	for _, f := range []string{stale, young, other} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{stale, other} {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := Replace(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale temporary file is still there (%v)", err)
	}
	for _, f := range []string{young, other, path} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s is gone: %v", filepath.Base(f), err)
		}
	}
}
