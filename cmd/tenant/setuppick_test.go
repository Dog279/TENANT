package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestPickModelNavigation(t *testing.T) {
	opts := []pickOption{{Label: "a", Value: "a"}, {Label: "b", Value: "b"}, {Label: "c", Value: "c"}}
	m := pickModel{opts: opts, cursor: 0, chosen: -1}

	step := func(mm pickModel, key tea.KeyType) pickModel {
		nm, _ := mm.Update(tea.KeyMsg{Type: key})
		return nm.(pickModel)
	}

	if m = step(m, tea.KeyDown); m.cursor != 1 {
		t.Fatalf("down: cursor=%d want 1", m.cursor)
	}
	m.cursor = 0
	if m = step(m, tea.KeyUp); m.cursor != 2 {
		t.Fatalf("up should wrap to last: cursor=%d want 2", m.cursor)
	}
	if m = step(m, tea.KeyDown); m.cursor != 0 {
		t.Fatalf("down should wrap to first: cursor=%d want 0", m.cursor)
	}

	// Enter selects the current row and quits.
	m.cursor = 1
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(pickModel)
	if m.chosen != 1 {
		t.Fatalf("enter: chosen=%d want 1", m.chosen)
	}
	if cmd == nil {
		t.Error("enter should return a quit cmd")
	}

	// Esc cancels (chosen stays -1).
	c := pickModel{opts: opts, cursor: 2, chosen: -1}
	nm, _ = c.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if nm.(pickModel).chosen != -1 {
		t.Error("esc should cancel (chosen=-1)")
	}
}

// In `go test`, stdin/stderr are pipes (not TTYs), so selectOne must degrade to
// returning the current value rather than blocking on a bubbletea program.
func TestSelectOneNonTTYReturnsCurrent(t *testing.T) {
	opts := []pickOption{{Label: "a", Value: "a"}, {Label: "b", Value: "b"}}
	if got := selectOne("pick", opts, "b"); got != "b" {
		t.Errorf("non-TTY selectOne should return cur, got %q", got)
	}
	if got := selectOne("pick", nil, "x"); got != "x" {
		t.Errorf("empty options should return cur, got %q", got)
	}
}

func TestProviderAndToolFormatOpts(t *testing.T) {
	po := providerOpts()
	if len(po) != len(providerOrder) {
		t.Fatalf("providerOpts len=%d want %d", len(po), len(providerOrder))
	}
	if po[0].Value != providerOrder[0] {
		t.Errorf("providerOpts[0]=%q want %q", po[0].Value, providerOrder[0])
	}
	for _, o := range po {
		if o.Desc == "" {
			t.Errorf("provider %q missing label/desc", o.Value)
		}
	}
	if len(toolFormatOpts()) != 5 {
		t.Errorf("toolFormatOpts len=%d want 5", len(toolFormatOpts()))
	}
}
