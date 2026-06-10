package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestModelAdd_NoFormatOpensPicker: `/model add <name> <endpoint>` with no tool
// format must open the arrow picker (TEN-138), not silently default to gemma.
func TestModelAdd_NoFormatOpensPicker(t *testing.T) {
	f := &fakeModels{}
	m := newModelPickerModel(f)

	p := applyPicker(t, m, runMsg(m.handleModel("add dgx http://localhost:8000")))
	if strings.Join(p.items, ",") != "qwen,gemma,llama,mistral,openai" {
		t.Fatalf("tool-format picker items = %v", p.items)
	}
	if p.items[p.selected] != "gemma" {
		t.Errorf("default highlight = %q, want gemma", p.items[p.selected])
	}
	// Pick a non-default format and confirm it's what gets stored.
	selectItem(p, "qwen")
	runMsg(m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter}))
	if len(f.addCalls) != 1 || f.addCalls[0] != "dgx|http://localhost:8000|qwen" {
		t.Fatalf("AddModel calls = %v, want [dgx|http://localhost:8000|qwen]", f.addCalls)
	}
}

// TestModelAdd_ExplicitFormatNoPicker: an explicit format still adds directly.
func TestModelAdd_ExplicitFormatNoPicker(t *testing.T) {
	f := &fakeModels{}
	m := newModelPickerModel(f)

	m.handleModel("add dgx http://localhost:8000 mistral")
	if m.picker != nil {
		t.Fatal("explicit tool format should not open a picker")
	}
	if len(f.addCalls) != 1 || f.addCalls[0] != "dgx|http://localhost:8000|mistral" {
		t.Fatalf("AddModel calls = %v, want [dgx|http://localhost:8000|mistral]", f.addCalls)
	}
}

// TestModelAdd_CancelNoAdd: esc at the format picker adds nothing.
func TestModelAdd_CancelNoAdd(t *testing.T) {
	f := &fakeModels{}
	m := newModelPickerModel(f)
	applyPicker(t, m, runMsg(m.handleModel("add dgx http://localhost:8000")))
	runMsg(m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEsc}))
	if len(f.addCalls) != 0 {
		t.Fatalf("cancel should not add a backend, got %v", f.addCalls)
	}
}
