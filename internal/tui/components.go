package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

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
	var lines []string
	switch c.Kind {
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
