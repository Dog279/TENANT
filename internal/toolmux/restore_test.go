package toolmux

import (
	"context"
	"encoding/json"
	"testing"

	"tenant/internal/model"
)

// A toggle must fire onChange with a full snapshot, and that snapshot,
// fed to a fresh mux's restore, must reproduce the curation — the core
// persist-across-restart contract.
func TestToolMux_PersistAndRestore(t *testing.T) {
	var hit string
	build := func() *Mux {
		m := New()
		m.Add("wiki", fakePlugin{name: "wiki", lastHit: &hit}) // wiki_do
		m.Add("os", fakePlugin{name: "os", lastHit: &hit})     // os_do
		return m
	}

	// Session 1: capture saves, disable os_do.
	m1 := build()
	var saved map[string]bool
	m1.SetOnChange(func(snap map[string]bool) { saved = snap })
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
	notes := m2.Restore(saved)
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
	m := New()
	m.Add("web", Stub{Specs: []model.ToolSpec{{Name: "web_do", Parameters: json.RawMessage(`{}`)}}, Hint: "stub"})
	m.SetEnabled("web", false)
	var hit string
	m.RegisterActivator("web", func() (Plugin, func(), error) {
		return fakePlugin{name: "web", lastHit: &hit}, nil, nil
	})

	m.Restore(map[string]bool{"web_do": true})

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
	m := New()
	m.Add("os", fakePlugin{name: "os", lastHit: &hit})
	saveCount := 0
	m.SetOnChange(func(map[string]bool) { saveCount++ })

	// onChange IS set here (unlike production order) to prove restore's
	// own SetEnabled calls still notify — production avoids re-saving by
	// setting the hook AFTER restore. We just assert unknown names are
	// skipped and known ones apply.
	notes := m.Restore(map[string]bool{"os_do": false, "ghost_do": true})
	if _, ok := m.Get("os_do"); ok {
		t.Fatal("os_do should be disabled")
	}
	if saveCount != 1 { // only os_do applied; ghost_do skipped (not registered)
		t.Fatalf("only the known tool should toggle: saves=%d notes=%v", saveCount, notes)
	}
}

// Remote-MCP plugins publish their tools from an activator that runs AFTER
// restore, so restore can only skip them. If add() ignored the restored
// curation, every bounce silently re-enabled tools the operator had turned off
// — observed in production as 29 Atlassian MCP tools coming back on after a
// restart while settings.main.json still read false.
func TestToolMux_RestoreSurvivesLateRegistration(t *testing.T) {
	var hit string
	m := New()

	// Operator disabled it last session; this session it isn't registered yet.
	notes := m.Restore(map[string]bool{"remote_do": false})
	if _, ok := m.Get("remote_do"); ok {
		t.Fatalf("remote_do should not be registered yet; notes=%v", notes)
	}

	// The activator finally registers it, long after restore ran.
	m.Add("mcp:remote", fakePlugin{name: "remote", lastHit: &hit})

	if _, ok := m.Get("remote_do"); ok {
		t.Fatal("late-registered tool must honor the restored disable, not add()'s enabled default")
	}
}

// The path an already-connected remote MCP actually takes at boot: the reconnect
// goroutine calls adoptLiveMCP, which merges tools ENABLED, and it runs AFTER
// restore. Without honoring the restored curation there, a disabled remote tool
// came back on every single bounce even though settings.json still read false.
func TestToolMux_AdoptLiveMCPHonorsRestoredCuration(t *testing.T) {
	var hit string
	m := New()

	m.Restore(map[string]bool{"remote_do": false})

	// Reconnect goroutine adopts the live server well after restore ran.
	m.adoptLiveMCP("mcp:remote", fakePlugin{name: "remote", lastHit: &hit}, nil)

	if _, ok := m.Get("remote_do"); ok {
		t.Fatal("adoptLiveMCP must not re-enable a tool the operator disabled")
	}
}

// A toggle that lands BEFORE a late-registering plugin loads must not drop that
// plugin's tools from the persisted snapshot — otherwise the operator's choice
// for them is erased from settings and cannot come back.
func TestToolMux_SnapshotKeepsUnregisteredCuration(t *testing.T) {
	var hit string
	m := New()
	m.Add("os", fakePlugin{name: "os", lastHit: &hit})
	m.Restore(map[string]bool{"os_do": true, "remote_do": false})

	var saved map[string]bool
	m.SetOnChange(func(snap map[string]bool) { saved = snap })
	if _, _, err := m.SetEnabled("os_do", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if saved == nil {
		t.Fatal("expected a snapshot")
	}
	if v, ok := saved["remote_do"]; !ok || v {
		t.Fatalf("unregistered remote_do curation must survive the snapshot: %+v", saved)
	}
	if saved["os_do"] {
		t.Fatalf("live state must override the seeded curation: %+v", saved)
	}
}
