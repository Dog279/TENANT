package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func newBareModel() *model { return newModel(context.Background(), Config{}) }

// TestHistory_RecallCycle: ↑ walks back through past entries, ↓ walks forward and
// restores the in-progress draft past the newest (readline behavior). (TEN-181)
func TestHistory_RecallCycle(t *testing.T) {
	m := newBareModel()
	m.pushHistory("first")
	m.pushHistory("second")
	m.ta.SetValue("draft") // a half-typed line before navigating

	if !m.historyPrev() || m.ta.Value() != "second" {
		t.Fatalf("first ↑ should recall 'second', got %q", m.ta.Value())
	}
	if !m.historyPrev() || m.ta.Value() != "first" {
		t.Fatalf("second ↑ should recall 'first', got %q", m.ta.Value())
	}
	if m.historyPrev() { // already at oldest → not consumed
		t.Errorf("↑ past oldest should return false")
	}
	if m.ta.Value() != "first" {
		t.Errorf("value should stay 'first' at the oldest, got %q", m.ta.Value())
	}
	if !m.historyNext() || m.ta.Value() != "second" {
		t.Fatalf("↓ should go to 'second', got %q", m.ta.Value())
	}
	if !m.historyNext() || m.ta.Value() != "draft" {
		t.Fatalf("↓ past newest should restore the draft, got %q", m.ta.Value())
	}
	if m.historyNext() { // already at draft → not consumed
		t.Errorf("↓ past the draft should return false")
	}
}

func TestHistory_DedupConsecutive(t *testing.T) {
	m := newBareModel()
	m.pushHistory("x")
	m.pushHistory("x") // consecutive dup ignored
	m.pushHistory("y")
	if len(m.history) != 2 || m.history[0] != "x" || m.history[1] != "y" {
		t.Fatalf("history = %v, want [x y]", m.history)
	}
	if m.pushHistory(""); len(m.history) != 2 { // empty ignored
		t.Fatalf("empty push changed history: %v", m.history)
	}
}

func TestMouseToggle(t *testing.T) {
	m := newBareModel()
	if !m.mouseOn {
		t.Fatal("mouse capture should default ON (wheel scrolls the TUI; ⌥/Shift-drag selects)")
	}
	if cmd := m.handleSlash("/mouse on"); cmd == nil || !m.mouseOn {
		t.Fatalf("/mouse on: mouseOn=%v cmd=%v", m.mouseOn, cmd)
	}
	if cmd := m.handleSlash("/mouse off"); cmd == nil || m.mouseOn {
		t.Fatalf("/mouse off: mouseOn=%v cmd=%v", m.mouseOn, cmd)
	}
	if cmd := m.handleSlash("/mouse"); cmd == nil || !m.mouseOn { // bare = toggle → on
		t.Fatalf("/mouse (toggle): mouseOn=%v cmd=%v", m.mouseOn, cmd)
	}
}

func TestCls_ClearsScreenNotContext(t *testing.T) {
	m := newBareModel()
	m.sysChat("UNIQUE-SEED-LINE")
	m.handleSlash("/cls")
	for _, msg := range m.msgs {
		if strings.Contains(msg.content, "UNIQUE-SEED-LINE") {
			t.Fatal("/cls did not clear the prior screen content")
		}
	}
}

// TestSelectMode_CtrlS: Ctrl+S enters select mode (capture dropped → plain drag
// highlights in any terminal), Ctrl+S or Esc exits and restores capture.
func TestSelectMode_CtrlS(t *testing.T) {
	m := newBareModel()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS}); cmd == nil || !m.selectMode {
		t.Fatalf("Ctrl+S should enter select mode with a DisableMouse cmd; selectMode=%v", m.selectMode)
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS}); cmd == nil || m.selectMode {
		t.Fatalf("second Ctrl+S should exit select mode; selectMode=%v", m.selectMode)
	}
	// Esc also exits.
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if !m.selectMode {
		t.Fatal("re-enter select mode failed")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.selectMode {
		t.Fatal("Esc should exit select mode")
	}
	// /mouse commands clear select mode too.
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m.handleSlash("/mouse on")
	if m.selectMode {
		t.Fatal("/mouse on should clear select mode")
	}
}

func TestClear_NilAgentGraceful(t *testing.T) {
	m := newBareModel() // Config{} has a nil Agent
	m.handleSlash("/clear")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "not available") {
		t.Fatalf("/clear with no agent should be graceful, got %q", last)
	}
}
