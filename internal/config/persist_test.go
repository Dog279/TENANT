package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The lost-update regression (found live 2026-08-12): a component holding the
// BOOT-TIME *launchConfig persisted a field change by saving that stale copy,
// wiping out everything other writers (e.g. /skill configure) had saved to
// disk since boot — configured skills silently vanished on the next launch.
// persistLaunchConfig must merge: fresh-load, apply, save.
func TestPersistLaunchConfigPreservesConcurrentWrites(t *testing.T) {
	dir := t.TempDir()

	// Boot: a component captures the config as it looked at launch.
	boot := &Launch{}
	if err := boot.Save(dir); err != nil {
		t.Fatal(err)
	}

	// Later, ANOTHER writer (skill configure, model add) saves new state via
	// the fresh-load discipline.
	fresh, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Provider = "zai"
	fresh.MCPRemotes = []string{"https://example.com/mcp"}
	if err := fresh.Save(dir); err != nil {
		t.Fatal(err)
	}

	// The stale-copy holder now persists its own field the SAFE way.
	if err := Persist(dir, boot, func(lc *Launch) {
		lc.Relay.Enabled = true
		lc.Relay.OperatorID = "op1"
	}); err != nil {
		t.Fatal(err)
	}

	// Both writers' fields must survive.
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "zai" || len(got.MCPRemotes) != 1 {
		t.Fatalf("other writer's fields clobbered: provider=%q remotes=%v", got.Provider, got.MCPRemotes)
	}
	if !got.Relay.Enabled || got.Relay.OperatorID != "op1" {
		t.Fatalf("persisted change missing: %+v", got.Relay)
	}
	// And the in-memory boot copy stays coherent for live readers.
	if !boot.Relay.Enabled {
		t.Fatal("in-memory copy not updated")
	}
}

// leftovers lists the temporary files AtomicWrite left in dir.
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
func TestAtomicWriteFlushesBeforeRename(t *testing.T) {
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
	if err := AtomicWrite(path, []byte("new")); err != nil {
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
}

// A flush that fails means the bytes may not be on the disk, so the old file
// keeps its name and the caller hears about it.
func TestAtomicWriteKeepsTheOldFileWhenTheFlushFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	real := syncFile
	t.Cleanup(func() { syncFile = real })
	gone := errors.New("the disk went away")
	syncFile = func(*os.File) error { return gone }
	if err := AtomicWrite(path, []byte("new")); !errors.Is(err, gone) {
		t.Fatalf("AtomicWrite = %v, want the flush's error", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Errorf("the file holds %q after a failed flush, want the old contents", b)
	}
	if m := leftovers(t, dir); len(m) != 0 {
		t.Errorf("temporary files left behind: %v", m)
	}
}
