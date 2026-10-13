package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"tenant/internal/agent"
	"tenant/internal/uicomp"
)

// componentLines draws a ui_render call as the component it names (TEN-517):
// a table through lipgloss's table with the Markdown renderer's styles, steps
// as marked lines. The component is the answer, not a tool detail, so it is
// always open and Ctrl+O does not fold it; it is drawn at the call, from the
// arguments (they arrive complete). A call whose arguments do not validate,
// or that failed, is not a component: the caller draws the ordinary row,
// which shows the error the model got.
func (m *model) componentLines(t toolRow, w int) (string, bool) {
	if t.name != uicomp.ToolName || t.isErr {
		return "", false
	}
	c, _, err := uicomp.Parse([]byte(t.args))
	if err != nil {
		return "", false
	}
	th := defaultMDTheme()
	w = max(20, w)
	armed := m.comp != nil && m.comp.id == t.id
	var lines []string
	switch c.Kind {
	case "choices":
		for i, o := range c.Choices.Options {
			lines = append(lines, wrapStyled(cUser.Render(fmt.Sprintf("%d.", i+1))+" "+o.Label, w)...)
		}
		if armed {
			lines = append(lines, cDim.Render("type a number and Enter to send it · anything else is a normal message"))
		}
	case "form":
		f := c.Form
		var names []string
		for _, fl := range f.Fields {
			names = append(names, fl.Label)
		}
		submit := f.Submit
		if submit == "" {
			submit = "Send"
		}
		lines = append(lines, wrapStyled(cUser.Render("form")+" · "+submit+cDim.Render(" · "+strings.Join(names, ", ")), w)...)
		if armed {
			lines = append(lines, cDim.Render("answer each field below, Enter to confirm · /cancel sends nothing"))
		}
	case "table":
		tb := c.Table
		if tb.Caption != "" {
			lines = append(lines, wrapStyled(th.TableHead.Render(tb.Caption), w)...)
		}
		tbl := table.New().Border(lipgloss.NormalBorder()).BorderStyle(th.TableLine).Width(w).Wrap(true).
			Headers(tb.Columns...).Rows(tb.Rows...).
			StyleFunc(func(row, _ int) lipgloss.Style {
				if row == table.HeaderRow {
					return th.TableHead.Padding(0, 1)
				}
				return lipgloss.NewStyle().Padding(0, 1)
			})
		lines = append(lines, strings.Split(tbl.Render(), "\n")...)
		if tb.Source != "" {
			lines = append(lines, wrapStyled(cDim.Render("Source: "+tb.Source), w)...)
		}
	case "steps":
		for _, it := range c.Steps.Items {
			mark := cDim.Render(uicomp.Marks[it.State])
			title := it.Title
			switch it.State {
			case "done":
				mark = cOK.Render(uicomp.Marks[it.State])
				title = cDim.Render(it.Title)
			case "doing":
				mark = cSys.Render(uicomp.Marks[it.State])
			}
			line := mark + " " + title
			if it.Detail != "" {
				line += " " + cDim.Render(it.Detail)
			}
			lines = append(lines, wrapStyled(line, w)...)
		}
	default:
		return "", false
	}
	return strings.Join(lines, "\n"), true
}

// componentPrompt is the latest turn's choices or form while it waits for the
// operator (TEN-518): a number answers choices; a form asks for one field at
// a time and sends the filled template. Only the latest turn's component is
// interactive; older ones render static.
type componentPrompt struct {
	c    uicomp.Component
	id   string
	vals map[string]string
	i    int // the next form field
}

// armComponent runs when a turn ends: the newest ui_render call that
// validated and is a choices or a form becomes the one waiting for an
// answer. Tables and steps wait for nothing.
func (m *model) armComponent() {
	m.comp = nil
	n := len(m.msgs)
	if n == 0 || m.msgs[n-1].role != "assistant" {
		return
	}
	last := &m.msgs[n-1]
	for i := len(last.tools) - 1; i >= 0; i-- {
		t := last.tools[i]
		if t.name != uicomp.ToolName || !t.done || t.isErr {
			continue
		}
		c, _, err := uicomp.Parse([]byte(t.args))
		if err != nil || (c.Kind != "choices" && c.Kind != "form") {
			continue
		}
		m.comp = &componentPrompt{c: c, id: t.id, vals: map[string]string{}}
		if c.Kind == "form" {
			m.formNext()
		}
		return
	}
}

// formNext asks for the next field, or sends the filled form. A select is an
// arrow-key picker; the rest are typed. /cancel (slashCancel) and the
// picker's Esc leave with nothing sent.
func (m *model) formNext() tea.Cmd {
	cp := m.comp
	if cp == nil {
		return nil
	}
	f := cp.c.Form
	if cp.i >= len(f.Fields) {
		text, err := uicomp.Fill(cp.c, uicomp.Inputs{Fields: cp.vals})
		m.comp = nil
		if err != nil {
			m.sysChat(err.Error() + " — the form was not sent; ask again")
			return nil
		}
		var none agent.RouteOverride
		return m.startTurn(text, none, "form", uicomp.Meta(cp.c, cp.id))
	}
	fl := f.Fields[cp.i]
	record := func(v string) tea.Cmd {
		if m.comp != cp {
			return nil // cancelled or replaced meanwhile
		}
		cp.vals[fl.Name] = strings.TrimSpace(v)
		cp.i++
		return m.formNext()
	}
	if fl.Type == "select" {
		m.picker = &listPicker{
			title: "form · " + fl.Label, hint: "↑/↓ pick · enter · esc cancels the form",
			items: fl.Options, currentMark: fl.Default,
			onSelect: record,
			onCancel: func() tea.Cmd {
				m.comp = nil
				m.sysChat("form: cancelled — nothing sent")
				return nil
			},
		}
		return nil
	}
	hint := "form · " + fl.Label
	switch fl.Type {
	case "number":
		hint += " (a number"
		if fl.Min != nil || fl.Max != nil {
			hint += " between " + numOr(fl.Min, "…") + " and " + numOr(fl.Max, "…")
		}
		hint += ")"
	case "range":
		hint += " (" + numOr(fl.Min, "") + " to " + numOr(fl.Max, "") + ")"
	}
	if fl.Default != "" {
		hint += " · Enter keeps " + fl.Default
	}
	m.setupEntry = &setupEntryState{label: fl.Label, apply: record}
	m.sysChat(hint + " · /cancel sends nothing")
	return nil
}

func numOr(f *float64, dflt string) string {
	if f == nil {
		return dflt
	}
	return strconv.FormatFloat(*f, 'f', -1, 64)
}
