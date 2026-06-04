package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"tenant/internal/model"
)

func TestSettings_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Missing file => empty, no error (first run).
	s, err := loadSettings(dir, "main")
	if err != nil || len(s.Tools) != 0 {
		t.Fatalf("first-run load: s=%+v err=%v", s, err)
	}

	s.Tools = map[string]bool{"web_navigate": true, "os_exec": false}
	if err := s.save(dir, "main"); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadSettings(dir, "main")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !got.Tools["web_navigate"] || got.Tools["os_exec"] {
		t.Fatalf("round-trip wrong: %+v", got.Tools)
	}

	// Per-agent isolation: a different agent has its own file.
	if other, _ := loadSettings(dir, "other"); len(other.Tools) != 0 {
		t.Fatalf("agent isolation broken: %+v", other.Tools)
	}

	// Corrupt file => error surfaced, defaults returned (not a wipe).
	if err := os.WriteFile(settingsPath(dir, "main"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, cerr := loadSettings(dir, "main"); cerr == nil || len(c.Tools) != 0 {
		t.Fatalf("corrupt should error + empty: c=%+v err=%v", c, cerr)
	}
}

// A toggle must fire onChange with a full snapshot, and that snapshot,
// fed to a fresh mux's restore, must reproduce the curation — the core
// persist-across-restart contract.
func TestToolMux_PersistAndRestore(t *testing.T) {
	var hit string
	build := func() *toolMux {
		m := newToolMux()
		m.add("wiki", fakePlugin{name: "wiki", lastHit: &hit}) // wiki_do
		m.add("os", fakePlugin{name: "os", lastHit: &hit})     // os_do
		return m
	}

	// Session 1: capture saves, disable os_do.
	m1 := build()
	var saved map[string]bool
	m1.setOnChange(func(snap map[string]bool) { saved = snap })
	if _, _, err := m1.SetEnabled("os_do", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if saved == nil || saved["os_do"] || !saved["wiki_do"] {
		t.Fatalf("snapshot wrong: %+v", saved)
	}

	// Session 2: a fresh mux (defaults all-enabled) restores the snapshot.
	m2 := build()
	if _, ok := m2.Get("os_do"); !ok {
		t.Fatal("os_do should start enabled by default")
	}
	notes := m2.restore(saved)
	if _, ok := m2.Get("os_do"); ok {
		t.Fatalf("os_do should be disabled after restore; notes=%v", notes)
	}
	if _, ok := m2.Get("wiki_do"); !ok {
		t.Fatal("wiki_do should remain enabled after restore")
	}
}

// Restoring an enabled state for an activator-backed stub (web) must
// activate it for real, so a persisted `/enable web` comes back live —
// not stuck as a "needs setup" stub.
func TestToolMux_RestoreActivatesStub(t *testing.T) {
	m := newToolMux()
	m.add("web", stubPlugin{specs: []model.ToolSpec{{Name: "web_do", Parameters: json.RawMessage(`{}`)}}, hint: "stub"})
	m.SetEnabled("web", false)
	var hit string
	m.registerActivator("web", func() (plugin, func(), error) {
		return fakePlugin{name: "web", lastHit: &hit}, nil, nil
	})

	m.restore(map[string]bool{"web_do": true})

	if _, ok := m.Get("web_do"); !ok {
		t.Fatal("web_do should be enabled after restore")
	}
	if out, isErr, _ := m.Dispatch(context.Background(), model.ToolCall{Name: "web_do"}); isErr || out != "ok:web" || hit != "web" {
		t.Fatalf("restore should have activated the real plugin: out=%q isErr=%v", out, isErr)
	}
}

// restore must not save during startup, and must ignore stale tool names.
func TestToolMux_RestoreIsSilentAndIgnoresUnknown(t *testing.T) {
	var hit string
	m := newToolMux()
	m.add("os", fakePlugin{name: "os", lastHit: &hit})
	saveCount := 0
	m.setOnChange(func(map[string]bool) { saveCount++ })

	// onChange IS set here (unlike production order) to prove restore's
	// own SetEnabled calls still notify — production avoids re-saving by
	// setting the hook AFTER restore. We just assert unknown names are
	// skipped and known ones apply.
	notes := m.restore(map[string]bool{"os_do": false, "ghost_do": true})
	if _, ok := m.Get("os_do"); ok {
		t.Fatal("os_do should be disabled")
	}
	if saveCount != 1 { // only os_do applied; ghost_do skipped (not registered)
		t.Fatalf("only the known tool should toggle: saves=%d notes=%v", saveCount, notes)
	}
}
