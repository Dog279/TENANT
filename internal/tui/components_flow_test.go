package tui

// Choices and forms in the terminal (TEN-518): the latest turn's component
// takes input once the turn is complete; a number answers choices, a form
// asks for one field at a time (a picker for a select), and the message it
// sends is tagged "via"; /cancel or the picker's Esc sends nothing.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"tenant/internal/agent"
)

const (
	tuiChoices = `{"kind":"choices","data":{"options":[{"label":"Show by price"},{"label":"Fastest","send":"Switch to the fastest model"}]}}`
	tuiForm    = `{"kind":"form","data":{"fields":[{"name":"total","label":"Bill total","type":"number","min":0},{"name":"tip","type":"select","options":["10%","15%","20%"],"default":"15%"},{"name":"note","type":"text","default":"none"}],"template":"Split {total} with a {tip} tip; note: {note}"}}`
)

// settle feeds one ui_render call and its result into a running turn, then
// ends the turn.
func settle(m *model, id, args string) {
	m.busy = true
	m.applyEvent(agent.Event{Kind: agent.EventToolCall, Tool: "ui_render", ToolCallID: id, Args: args})
	m.applyEvent(agent.Event{Kind: agent.EventToolResult, Tool: "ui_render", ToolCallID: id, Args: args, Result: "shown", Duration: 1})
	if m.comp != nil {
		panic("a component took input while the turn was still running")
	}
	m.Update(turnDoneMsg{res: &agent.TurnResult{Response: "Pick one."}})
}

func lastUser(m *model) *chatMsg {
	for i := len(m.msgs) - 1; i >= 0; i-- {
		if m.msgs[i].role == "user" {
			return &m.msgs[i]
		}
	}
	return nil
}

func TestChoices_NumberSendsTaggedMessage(t *testing.T) {
	m := readyChatModel(Config{})
	settle(m, "c1", tuiChoices)
	if m.comp == nil || m.comp.c.Kind != "choices" {
		t.Fatalf("choices should wait for an answer once the turn is done: %+v", m.comp)
	}
	out := stripANSI(m.renderChat())
	if !strings.Contains(out, "1. Show by price") || !strings.Contains(out, "2. Fastest") || !strings.Contains(out, "type a number") {
		t.Fatalf("choices render numbered with the hint:\n%s", out)
	}
	m.ta.SetValue("9")
	if cmd := m.submit(); cmd != nil || m.comp == nil || lastUser(m) != nil {
		t.Fatalf("an option that does not exist sends nothing: cmd=%v comp=%v", cmd != nil, m.comp != nil)
	}
	m.ta.SetValue("2")
	if cmd := m.submit(); cmd == nil {
		t.Fatal("a valid number starts the turn")
	}
	u := lastUser(m)
	if u == nil || u.content != "Switch to the fastest model" || u.via != "choices" || m.comp != nil || !m.busy {
		t.Fatalf("sent %+v comp=%v busy=%v", u, m.comp != nil, m.busy)
	}
	if h := stripANSI(m.blockHeader(u)); !strings.Contains(h, "via choices") {
		t.Fatalf("header: %q", h)
	}
}

func TestChoices_OtherMessageLeavesThemBehind(t *testing.T) {
	m := readyChatModel(Config{})
	settle(m, "c1", tuiChoices)
	m.ta.SetValue("something else")
	m.submit()
	if u := lastUser(m); u == nil || u.content != "something else" || u.via != "" || m.comp != nil {
		t.Fatalf("a typed message is itself and drops the choices: %+v comp=%v", u, m.comp != nil)
	}
}

func TestForm_PromptsEachFieldThenSends(t *testing.T) {
	m := readyChatModel(Config{})
	settle(m, "f1", tuiForm)
	if m.comp == nil || m.setupEntry == nil || m.setupEntry.label != "Bill total" {
		t.Fatalf("the form should ask for its first field: comp=%v entry=%+v", m.comp != nil, m.setupEntry)
	}
	m.input.SetValue("120.5")
	m.submit()
	if m.picker == nil || m.picker.title != "form · tip" || len(m.picker.items) != 3 {
		t.Fatalf("the select field should be a picker: %+v", m.picker)
	}
	m.picker.selected = 2 // 20%
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.setupEntry == nil || m.setupEntry.label != "note" {
		t.Fatalf("then the text field: %+v", m.setupEntry)
	}
	m.input.SetValue("")
	cmd := m.submit() // blank keeps the default
	u := lastUser(m)
	if cmd == nil || u == nil || u.content != "Split 120.5 with a 20% tip; note: none" || u.via != "form" || m.comp != nil || m.setupEntry != nil {
		t.Fatalf("filled: %+v comp=%v entry=%v", u, m.comp != nil, m.setupEntry != nil)
	}
	if m.input.EchoMode != textinput.EchoNormal {
		t.Fatal("the input should be back to normal")
	}
}

func TestForm_CancelSendsNothing(t *testing.T) {
	m := readyChatModel(Config{})
	settle(m, "f1", tuiForm)
	m.input.SetValue("12")
	m.submit() // now on the select picker
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.comp != nil || lastUser(m) != nil || m.picker != nil {
		t.Fatalf("Esc on the picker cancels the form: comp=%v user=%v", m.comp != nil, lastUser(m))
	}
	m = readyChatModel(Config{})
	settle(m, "f1", tuiForm)
	m.handleSlash("/cancel")
	if m.comp != nil || m.setupEntry != nil || lastUser(m) != nil || m.input.EchoMode != textinput.EchoNormal {
		t.Fatalf("/cancel mid-form sends nothing: comp=%v entry=%v user=%v", m.comp != nil, m.setupEntry != nil, lastUser(m))
	}
	if last := stripANSI(m.msgs[len(m.msgs)-1].content); !strings.Contains(last, "nothing sent") {
		t.Fatalf("it says so: %q", last)
	}
}

func TestForm_BadValueIsNotSent(t *testing.T) {
	m := readyChatModel(Config{})
	settle(m, "f1", tuiForm)
	m.input.SetValue("abc")
	m.submit()
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	m.input.SetValue("")
	if cmd := m.submit(); cmd != nil || lastUser(m) != nil || m.comp != nil {
		t.Fatalf("a bad number sends nothing: cmd=%v user=%v", cmd != nil, lastUser(m))
	}
	if last := stripANSI(m.msgs[len(m.msgs)-1].content); !strings.Contains(last, "Bill total must be a number") {
		t.Fatalf("it says why: %q", last)
	}
}

// Tables and steps wait for nothing; a refused component is not armed.
func TestArm_OnlyChoicesAndForm(t *testing.T) {
	m := readyChatModel(Config{})
	settle(m, "t1", tuiTableArgs)
	if m.comp != nil {
		t.Fatal("a table takes no input")
	}
	m = readyChatModel(Config{})
	bad := `{"kind":"choices","data":{"options":[{"label":"a"},{"label":"/exit"}]}}`
	m.busy = true
	m.applyEvent(agent.Event{Kind: agent.EventToolCall, Tool: "ui_render", ToolCallID: "x", Args: bad})
	m.applyEvent(agent.Event{Kind: agent.EventToolResult, Tool: "ui_render", ToolCallID: "x", Args: bad, Result: "choices: option 2's message must not start with /", IsErr: true, Duration: 1})
	m.Update(turnDoneMsg{res: &agent.TurnResult{Response: "x"}})
	if m.comp != nil {
		t.Fatal("a refused component is never armed, so a slash command can never be sent")
	}
}
