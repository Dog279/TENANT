package tui

// Components in the terminal (TEN-517): a ui_render call is drawn as the
// table or steps it names, always open, in place of the tool row; a call the
// validator refuses stays an ordinary row with the error.

import (
	"strings"
	"testing"

	"tenant/internal/agent"
)

const tuiTableArgs = `{"kind":"table","data":{"columns":["Model","Cost"],"rows":[["glm","$0"],["claude","$3"]],"caption":"Configured models","source":"config.json"}}`

func TestRenderChat_ComponentsAreDrawnOpen(t *testing.T) {
	m := readyChatModel(Config{})
	m.applyEvent(agent.Event{Kind: agent.EventToolCall, Tool: "ui_render", ToolCallID: "c1", Args: tuiTableArgs})
	out := stripANSI(m.renderChat())
	for _, want := range []string{"Configured models", "Model", "Cost", "glm", "$3", "Source: config.json"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ui_render") || strings.Contains(out, "⏺") {
		t.Fatalf("a component is the answer, not a tool row:\n%s", out)
	}
	m.applyEvent(agent.Event{Kind: agent.EventToolResult, Tool: "ui_render", ToolCallID: "c1", Args: tuiTableArgs, Result: "shown: table c1, 2 rows", Duration: 1})
	m.applyEvent(agent.Event{Kind: agent.EventToolCall, Tool: "ui_render", ToolCallID: "c2", Args: `{"kind":"steps","data":{"items":[{"title":"Back up","state":"done"},{"title":"Restore","detail":"after the copy","state":"doing"},{"title":"Verify"}]}}`})
	m.applyEvent(agent.Event{Kind: agent.EventToolResult, Tool: "ui_render", ToolCallID: "c2", Args: `{"kind":"steps","data":{"items":[{"title":"Back up","state":"done"},{"title":"Restore","detail":"after the copy","state":"doing"},{"title":"Verify"}]}}`, Result: "shown: steps c2, 3 items", Duration: 1})
	out = stripANSI(m.renderChat())
	for _, want := range []string{"✓ Back up", "◐ Restore after the copy", "○ Verify", "glm"} {
		if !strings.Contains(out, want) {
			t.Errorf("steps missing %q in:\n%s", want, out)
		}
	}
	// Ctrl+O folds tool rows, never a component.
	m.toolsOpen = !m.toolsOpen
	if again := stripANSI(m.renderChat()); !strings.Contains(again, "✓ Back up") || !strings.Contains(again, "glm") {
		t.Fatalf("components stay open either way:\n%s", again)
	}
}

func TestRenderChat_BadComponentIsAnOrdinaryRow(t *testing.T) {
	m := readyChatModel(Config{})
	bad := `{"kind":"table","data":{"columns":["a","b"],"rows":[["1"]]}}`
	m.applyEvent(agent.Event{Kind: agent.EventToolCall, Tool: "ui_render", ToolCallID: "c3", Args: bad})
	m.applyEvent(agent.Event{Kind: agent.EventToolResult, Tool: "ui_render", ToolCallID: "c3", Args: bad, Result: "table: row 1 has 1 cells, the table has 2 columns", IsErr: true, Duration: 1})
	out := stripANSI(m.renderChat())
	if !strings.Contains(out, "ui_render") || !strings.Contains(out, "the table has 2 columns") {
		t.Fatalf("a refused call is the ordinary row, open with its error:\n%s", out)
	}
}
