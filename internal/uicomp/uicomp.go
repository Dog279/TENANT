// Package uicomp is the components the model can answer with (TEN-517): the
// ui_render tool's schema, the validation and caps on its arguments, the
// one-line result the model reads back, and the dashboard's HTML. The
// terminal app draws the same Component with its own styles (internal/tui).
// The model never writes markup: it names a kind and hands over data, and
// Tenant draws it from a fixed template, so nothing here is ever more than
// escaped text.
package uicomp

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"sort"
	"strings"
	"unicode/utf8"

	"tenant/internal/model"
)

// ToolName is the tool the model calls.
const ToolName = "ui_render"

// The caps. A table's are the ones A2UI uses; the total keeps one call from
// swallowing the model's context on the way back and every transcript load.
const (
	MaxArgs    = 32 << 10
	MaxColumns = 12
	MaxRows    = 200
	MaxText    = 200 // one cell, caption, source, title or detail
	MaxSteps   = 50
)

// Kinds lists the component kinds this build draws, in the order the model
// sees them.
var Kinds = []string{"table", "steps"}

// Component is one validated ui_render call. Exactly one of the kind fields
// is set.
type Component struct {
	Kind  string
	Table *Table
	Steps *Steps
}

// Table is rows under columns, with an optional caption and a plain-text
// source line (never a link).
type Table struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
	Caption string     `json:"caption,omitempty"`
	Source  string     `json:"source,omitempty"`
}

// Steps is a checklist.
type Steps struct {
	Items []Step `json:"items"`
}

// Step is one checklist item; State is todo (the default), doing or done.
type Step struct {
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	State  string `json:"state,omitempty"`
}

// schema is the tool's parameters as the model sees them. The caps are in
// here too, so a strict model corrects itself before calling.
var schema = json.RawMessage(fmt.Sprintf(`{"type":"object","required":["kind","data"],"properties":{
"kind":{"type":"string","enum":["table","steps"],"description":"what to draw"},
"data":{"type":"object","description":"the kind's shape. table: {columns:[string], rows:[[string]], caption?, source?} with at most %d columns, %d rows and %d characters per cell; source is plain text, not a link. steps: {items:[{title, detail?, state: todo|doing|done}]} with at most %d items."}}}`,
	MaxColumns, MaxRows, MaxText, MaxSteps))

// Spec is the tool. Its description is the only prompting there is: it says
// when to reach for a component and when to stay in prose.
func Spec() model.ToolSpec {
	return model.ToolSpec{
		Name:        ToolName,
		Description: "Show the answer as a component Tenant draws for the user instead of prose: a table (a comparison, a list of things with attributes) or steps (a plan, a checklist, progress). Use it when the answer IS the table or the list; answer in prose otherwise. Hand over data only, never markup. The result says what was shown; you need not repeat the data in text, one line of context is enough.",
		Parameters:  schema,
	}
}

// Parse validates one call's arguments. warnings name the fields that were
// ignored, so a live call learns about a typo; err is one line the model can
// act on ("table: 14 columns, the limit is 12"). An unknown kind is an
// error for a live call; the renderers treat it as "draw the ordinary card".
func Parse(args []byte) (Component, []string, error) {
	var warnings []string
	if len(args) > MaxArgs {
		return Component{}, nil, fmt.Errorf("%s: the arguments are %d KB, the limit is %d KB", ToolName, len(args)>>10, MaxArgs>>10)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(args, &top); err != nil {
		return Component{}, nil, fmt.Errorf("%s: the arguments are not a JSON object", ToolName)
	}
	var kind string
	if raw, ok := top["kind"]; ok {
		if err := json.Unmarshal(raw, &kind); err != nil {
			return Component{}, nil, fmt.Errorf("%s: kind must be a string", ToolName)
		}
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	data, hasData := top["data"]
	warnings = append(warnings, unknown(top, "kind", "data")...)
	if kind == "" {
		return Component{}, nil, fmt.Errorf("%s: kind is missing; kinds: %s", ToolName, strings.Join(Kinds, ", "))
	}
	if !hasData || len(data) == 0 || string(data) == "null" {
		return Component{}, nil, fmt.Errorf("%s: data is missing", kind)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Component{}, nil, fmt.Errorf("%s: data must be an object", kind)
	}
	c := Component{Kind: kind}
	switch kind {
	case "table":
		warnings = append(warnings, unknown(fields, "columns", "rows", "caption", "source")...)
		var t Table
		if err := json.Unmarshal(data, &t); err != nil {
			return Component{}, nil, fmt.Errorf("table: %s", shape(err))
		}
		if err := t.validate(); err != nil {
			return Component{}, nil, err
		}
		c.Table = &t
	case "steps":
		warnings = append(warnings, unknown(fields, "items")...)
		var s Steps
		if err := json.Unmarshal(data, &s); err != nil {
			return Component{}, nil, fmt.Errorf("steps: %s", shape(err))
		}
		if err := s.validate(); err != nil {
			return Component{}, nil, err
		}
		c.Steps = &s
	default:
		return Component{}, nil, fmt.Errorf("%s: unknown kind %q; kinds: %s", ToolName, kind, strings.Join(Kinds, ", "))
	}
	return c, warnings, nil
}

// unknown lists the keys of m that are not among known, as warnings, sorted.
func unknown(m map[string]json.RawMessage, known ...string) []string {
	var out []string
	for k := range m {
		ok := false
		for _, n := range known {
			if k == n {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, "ignored unknown field: "+k)
		}
	}
	sort.Strings(out)
	return out
}

// shape turns a JSON type error into words the model can act on.
func shape(err error) string {
	if te, ok := err.(*json.UnmarshalTypeError); ok {
		return fmt.Sprintf("%s should be %s, not %s", te.Field, want(te.Type.String()), te.Value)
	}
	return "the data has the wrong shape"
}

func want(goType string) string {
	switch {
	case strings.HasPrefix(goType, "[][]"):
		return "a list of lists"
	case strings.HasPrefix(goType, "[]"):
		return "a list"
	case goType == "string":
		return "a string"
	}
	return "an object"
}

func (t *Table) validate() error {
	switch {
	case len(t.Columns) == 0:
		return fmt.Errorf("table: needs at least one column")
	case len(t.Columns) > MaxColumns:
		return fmt.Errorf("table: %d columns, the limit is %d", len(t.Columns), MaxColumns)
	case len(t.Rows) > MaxRows:
		return fmt.Errorf("table: %d rows, the limit is %d", len(t.Rows), MaxRows)
	}
	for i, col := range t.Columns {
		if n := utf8.RuneCountInString(col); n > MaxText {
			return fmt.Errorf("table: column %d's name is %d characters, the limit is %d", i+1, n, MaxText)
		}
	}
	for r, row := range t.Rows {
		if len(row) != len(t.Columns) {
			return fmt.Errorf("table: row %d has %d cells, the table has %d columns", r+1, len(row), len(t.Columns))
		}
		for c, cell := range row {
			if n := utf8.RuneCountInString(cell); n > MaxText {
				return fmt.Errorf("table: the cell at row %d, column %d is %d characters, the limit is %d", r+1, c+1, n, MaxText)
			}
		}
	}
	if n := utf8.RuneCountInString(t.Caption); n > MaxText {
		return fmt.Errorf("table: the caption is %d characters, the limit is %d", n, MaxText)
	}
	if n := utf8.RuneCountInString(t.Source); n > MaxText {
		return fmt.Errorf("table: the source is %d characters, the limit is %d", n, MaxText)
	}
	return nil
}

func (s *Steps) validate() error {
	switch {
	case len(s.Items) == 0:
		return fmt.Errorf("steps: needs at least one item")
	case len(s.Items) > MaxSteps:
		return fmt.Errorf("steps: %d items, the limit is %d", len(s.Items), MaxSteps)
	}
	for i := range s.Items {
		it := &s.Items[i]
		if strings.TrimSpace(it.Title) == "" {
			return fmt.Errorf("steps: item %d has no title", i+1)
		}
		if n := utf8.RuneCountInString(it.Title); n > MaxText {
			return fmt.Errorf("steps: item %d's title is %d characters, the limit is %d", i+1, n, MaxText)
		}
		if n := utf8.RuneCountInString(it.Detail); n > MaxText {
			return fmt.Errorf("steps: item %d's detail is %d characters, the limit is %d", i+1, n, MaxText)
		}
		switch it.State = strings.ToLower(strings.TrimSpace(it.State)); it.State {
		case "":
			it.State = "todo"
		case "todo", "doing", "done":
		default:
			return fmt.Errorf("steps: item %d's state is %q; states: todo, doing, done", i+1, it.State)
		}
	}
	return nil
}

// Note is the tool result the model reads: what was shown, under which id,
// and what was ignored. id is the tool call's own id ("" when the provider
// gave none).
func Note(c Component, id string, warnings []string) string {
	var b strings.Builder
	b.WriteString("shown: " + c.Kind)
	if id != "" {
		b.WriteString(" " + id)
	}
	switch c.Kind {
	case "table":
		fmt.Fprintf(&b, ", %d rows", len(c.Table.Rows))
	case "steps":
		fmt.Fprintf(&b, ", %d items", len(c.Steps.Items))
	}
	if len(warnings) > 0 {
		b.WriteString("; " + strings.Join(warnings, "; "))
	}
	return b.String()
}

// Marks are the step states as glyphs, shared by both surfaces.
var Marks = map[string]string{"todo": "○", "doing": "◐", "done": "✓"}

// HTML draws c for the dashboard. Every string from the model goes through
// text (the dashboard's visibleText, or an identity) and then HTML escaping;
// no attribute is ever built from model data, so there is no link, no
// image and no script in a component.
func HTML(c Component, text func(string) string) string {
	if text == nil {
		text = func(s string) string { return s }
	}
	esc := func(s string) string { return html.EscapeString(text(s)) }
	var b strings.Builder
	switch c.Kind {
	case "table":
		t := c.Table
		b.WriteString(`<div class="comp comp-table"><div class="table-wrap"><table>`)
		if t.Caption != "" {
			b.WriteString(`<caption>` + esc(t.Caption) + `</caption>`)
		}
		b.WriteString(`<thead><tr>`)
		for _, col := range t.Columns {
			b.WriteString(`<th>` + esc(col) + `</th>`)
		}
		b.WriteString(`</tr></thead><tbody>`)
		for _, row := range t.Rows {
			b.WriteString(`<tr>`)
			for _, cell := range row {
				b.WriteString(`<td>` + esc(cell) + `</td>`)
			}
			b.WriteString(`</tr>`)
		}
		b.WriteString(`</tbody></table></div>`)
		if t.Source != "" {
			b.WriteString(`<div class="dim comp-source">Source: ` + esc(t.Source) + `</div>`)
		}
		b.WriteString(`</div>`)
	case "steps":
		b.WriteString(`<ol class="comp comp-steps">`)
		for _, it := range c.Steps.Items {
			b.WriteString(`<li class="step ` + it.State + `"><span class="mark" aria-label="` + it.State + `">` + Marks[it.State] + `</span> <span class="title">` + esc(it.Title) + `</span>`)
			if it.Detail != "" {
				b.WriteString(` <span class="dim">` + esc(it.Detail) + `</span>`)
			}
			b.WriteString(`</li>`)
		}
		b.WriteString(`</ol>`)
	}
	return b.String()
}

// Dispatcher is the ui plugin: one tool, no state. Drawing happens in the
// surfaces, from the call's arguments; the handler only validates and
// answers the model.
type Dispatcher struct{}

// Tools lists ui_render.
func (Dispatcher) Tools() []model.ToolSpec { return []model.ToolSpec{Spec()} }

// Dispatch validates the call. A bad shape is a logical error the model can
// fix (isError true, the message says how); the result of a good one is the
// Note.
func (Dispatcher) Dispatch(_ context.Context, call model.ToolCall) (string, bool, error) {
	if call.Name != ToolName {
		return fmt.Sprintf("unknown tool %q", call.Name), true, nil
	}
	c, warnings, err := Parse(call.Arguments)
	if err != nil {
		return err.Error(), true, nil
	}
	return Note(c, call.ID, warnings), false, nil
}
