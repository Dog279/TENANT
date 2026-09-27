package config

import (
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
