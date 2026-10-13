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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"tenant/internal/model"
)

// ToolName is the tool the model calls.
const ToolName = "ui_render"

// The caps. A table's are the ones A2UI uses; the total keeps one call from
// swallowing the model's context on the way back and every transcript load.
const (
	MaxArgs     = 32 << 10
	MaxColumns  = 12
	MaxRows     = 200
	MaxText     = 200 // one cell, caption, source, title or detail
	MaxSteps    = 50
	MinOptions  = 2
	MaxOptions  = 6
	MaxLabel    = 80
	MaxSend     = 200
	MaxFields   = 8
	MaxTemplate = 1024
	MaxSubmit   = 40
)

// Kinds lists the component kinds this build draws, in the order the model
// sees them.
var Kinds = []string{"table", "steps", "choices", "form"}

// MetaSource marks a user message a component sent (TEN-518): the message's
// Meta carries {"source": MetaSource, "component": <call id>, "kind": <kind>},
// and both surfaces show it as "via choices" or "via form".
const MetaSource = "ui_render"

// Component is one validated ui_render call. Exactly one of the kind fields
// is set.
type Component struct {
	Kind    string
	Table   *Table
	Steps   *Steps
	Choices *Choices
	Form    *Form
}

// Choices is a row of likely next messages; a tap sends one as the user.
type Choices struct {
	Options []Choice `json:"options"`
}

// Choice is one option: its label, and the message a tap sends (the label
// when Send is empty).
type Choice struct {
	Label string `json:"label"`
	Send  string `json:"send,omitempty"`
}

// Form asks the user for values and sends Template with {name} filled in.
type Form struct {
	Fields   []Field `json:"fields"`
	Submit   string  `json:"submit,omitempty"`
	Template string  `json:"template"`
}

// Field is one input: text, number, select (Options) or range (Min, Max).
type Field struct {
	Name    string   `json:"name"`
	Label   string   `json:"label"`
	Type    string   `json:"type"`
	Options []string `json:"options,omitempty"`
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Default string   `json:"default,omitempty"`
}

// fieldName is what a field may be called: it becomes a Datastar signal and
// a {placeholder}, so it is an identifier.
var fieldName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)

// placeholder finds {name} in a template.
var placeholder = regexp.MustCompile(`\{([a-z][a-z0-9_]{0,30})\}`)

// safeID is what a tool call id may look like to ride in a page attribute.
var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

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
"kind":{"type":"string","enum":["table","steps","choices","form"],"description":"what to draw"},
"data":{"type":"object","description":"the kind's shape. table: {columns:[string], rows:[[string]], caption?, source?} with at most %d columns, %d rows and %d characters per cell; source is plain text, not a link. steps: {items:[{title, detail?, state: todo|doing|done}]} with at most %d items. choices: {options:[{label, send?}]}, %d to %d options the user can tap to send the next message (send defaults to the label; never a slash command). form: {fields:[{name, label, type: text|number|select|range, options?, min?, max?, default?}], submit?, template} with at most %d fields; the user fills it and template is sent with {name} replaced by each value."}}}`,
	MaxColumns, MaxRows, MaxText, MaxSteps, MinOptions, MaxOptions, MaxFields))

// Spec is the tool. Its description is the only prompting there is: it says
// when to reach for a component and when to stay in prose.
func Spec() model.ToolSpec {
	return model.ToolSpec{
		Name:        ToolName,
		Description: "Show the answer as a component Tenant draws for the user instead of prose: a table (a comparison, a list of things with attributes), steps (a plan, a checklist, progress), choices (the likely next messages, as buttons the user taps) or a form (values you need from the user; the filled template comes back as their next message). Use it when the answer IS the table, the list, the next step or the question; answer in prose otherwise. Hand over data only, never markup. The result says what was shown; you need not repeat the data in text, one line of context is enough.",
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
	case "choices":
		warnings = append(warnings, unknown(fields, "options")...)
		var ch Choices
		if err := json.Unmarshal(data, &ch); err != nil {
			return Component{}, nil, fmt.Errorf("choices: %s", shape(err))
		}
		if err := ch.validate(); err != nil {
			return Component{}, nil, err
		}
		c.Choices = &ch
	case "form":
		warnings = append(warnings, unknown(fields, "fields", "submit", "template")...)
		var f Form
		if err := json.Unmarshal(data, &f); err != nil {
			return Component{}, nil, fmt.Errorf("form: %s", shape(err))
		}
		if err := f.validate(); err != nil {
			return Component{}, nil, err
		}
		c.Form = &f
	default:
		return Component{}, nil, fmt.Errorf("%s: unknown kind %q; kinds: %s", ToolName, kind, strings.Join(Kinds, ", "))
	}
	return c, warnings, nil
}

// sendable is the rule for text a component sends as the user: one line,
// never a slash command (the terminal would run it).
func sendable(kind, what, s string, limit int) error {
	switch {
	case strings.HasPrefix(strings.TrimSpace(s), "/"):
		return fmt.Errorf("%s: %s must not start with /; a component never sends a command", kind, what)
	case strings.ContainsAny(s, "\n\r"):
		return fmt.Errorf("%s: %s must be one line", kind, what)
	case utf8.RuneCountInString(s) > limit:
		return fmt.Errorf("%s: %s is %d characters, the limit is %d", kind, what, utf8.RuneCountInString(s), limit)
	}
	return nil
}

func (ch *Choices) validate() error {
	switch {
	case len(ch.Options) < MinOptions:
		return fmt.Errorf("choices: %d options, at least %d are needed", len(ch.Options), MinOptions)
	case len(ch.Options) > MaxOptions:
		return fmt.Errorf("choices: %d options, the limit is %d", len(ch.Options), MaxOptions)
	}
	for i := range ch.Options {
		o := &ch.Options[i]
		o.Label = strings.TrimSpace(o.Label)
		if o.Label == "" {
			return fmt.Errorf("choices: option %d has no label", i+1)
		}
		if n := utf8.RuneCountInString(o.Label); n > MaxLabel {
			return fmt.Errorf("choices: option %d's label is %d characters, the limit is %d", i+1, n, MaxLabel)
		}
		if o.Send == "" {
			o.Send = o.Label
		}
		if err := sendable("choices", fmt.Sprintf("option %d's message", i+1), o.Send, MaxSend); err != nil {
			return err
		}
	}
	return nil
}

func (f *Form) validate() error {
	switch {
	case len(f.Fields) == 0:
		return fmt.Errorf("form: needs at least one field")
	case len(f.Fields) > MaxFields:
		return fmt.Errorf("form: %d fields, the limit is %d", len(f.Fields), MaxFields)
	case strings.TrimSpace(f.Template) == "":
		return fmt.Errorf("form: template is missing; it is the message sent with {name} filled in")
	}
	if err := sendable("form", "template", f.Template, MaxTemplate); err != nil {
		return err
	}
	if n := utf8.RuneCountInString(f.Submit); n > MaxSubmit {
		return fmt.Errorf("form: submit is %d characters, the limit is %d", n, MaxSubmit)
	}
	seen := map[string]bool{}
	for i := range f.Fields {
		fl := &f.Fields[i]
		if !fieldName.MatchString(fl.Name) {
			return fmt.Errorf("form: field %d's name %q must be a lowercase identifier (a-z, 0-9, _), at most 31 characters", i+1, fl.Name)
		}
		if seen[fl.Name] {
			return fmt.Errorf("form: field %q appears twice", fl.Name)
		}
		seen[fl.Name] = true
		if strings.TrimSpace(fl.Label) == "" {
			fl.Label = fl.Name
		}
		if n := utf8.RuneCountInString(fl.Label); n > MaxLabel {
			return fmt.Errorf("form: field %q's label is %d characters, the limit is %d", fl.Name, n, MaxLabel)
		}
		switch fl.Type = strings.ToLower(strings.TrimSpace(fl.Type)); fl.Type {
		case "", "text":
			fl.Type = "text"
		case "number":
		case "select":
			if len(fl.Options) == 0 || len(fl.Options) > MaxColumns {
				return fmt.Errorf("form: field %q is a select and needs 1 to %d options", fl.Name, MaxColumns)
			}
		case "range":
			if fl.Min == nil || fl.Max == nil || *fl.Min >= *fl.Max {
				return fmt.Errorf("form: field %q is a range and needs min below max", fl.Name)
			}
		default:
			return fmt.Errorf("form: field %q's type is %q; types: text, number, select, range", fl.Name, fl.Type)
		}
		if n := utf8.RuneCountInString(fl.Default); n > MaxText {
			return fmt.Errorf("form: field %q's default is %d characters, the limit is %d", fl.Name, n, MaxText)
		}
	}
	for _, m := range placeholder.FindAllStringSubmatch(f.Template, -1) {
		if !seen[m[1]] {
			return fmt.Errorf("form: the template names {%s}, which is not a field", m[1])
		}
	}
	return nil
}

// Inputs is what the user gave back: the option tapped (choices) or the
// field values (form), as strings, as a page or a terminal collects them.
type Inputs struct {
	Choice int
	Fields map[string]string
}

// Fill validates the user's inputs against c and returns the message to send
// as the user. The same function serves the dashboard and the terminal, so
// the two cannot drift. Errors are one line for the person who typed.
func Fill(c Component, in Inputs) (string, error) {
	switch c.Kind {
	case "choices":
		if in.Choice < 0 || in.Choice >= len(c.Choices.Options) {
			return "", fmt.Errorf("choices: option %d does not exist (there are %d)", in.Choice+1, len(c.Choices.Options))
		}
		return c.Choices.Options[in.Choice].Send, nil
	case "form":
		vals := map[string]string{}
		for _, fl := range c.Form.Fields {
			v, ok := in.Fields[fl.Name]
			v = strings.TrimSpace(v)
			if (!ok || v == "") && fl.Default != "" {
				v = fl.Default
			}
			if v == "" {
				return "", fmt.Errorf("form: %s is missing", fl.Label)
			}
			if strings.ContainsAny(v, "\n\r") {
				return "", fmt.Errorf("form: %s must be one line", fl.Label)
			}
			if n := utf8.RuneCountInString(v); n > MaxText {
				return "", fmt.Errorf("form: %s is %d characters, the limit is %d", fl.Label, n, MaxText)
			}
			switch fl.Type {
			case "number", "range":
				n, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return "", fmt.Errorf("form: %s must be a number", fl.Label)
				}
				if (fl.Min != nil && n < *fl.Min) || (fl.Max != nil && n > *fl.Max) {
					return "", fmt.Errorf("form: %s must be between %s and %s", fl.Label, num(fl.Min), num(fl.Max))
				}
				v = strconv.FormatFloat(n, 'f', -1, 64)
			case "select":
				ok := false
				for _, o := range fl.Options {
					if o == v {
						ok = true
						break
					}
				}
				if !ok {
					return "", fmt.Errorf("form: %s must be one of %s", fl.Label, strings.Join(fl.Options, ", "))
				}
			}
			vals[fl.Name] = v
		}
		text := placeholder.ReplaceAllStringFunc(c.Form.Template, func(m string) string {
			return vals[strings.Trim(m, "{}")]
		})
		if err := sendable("form", "the filled message", text, MaxTemplate+MaxFields*MaxText); err != nil {
			return "", err
		}
		return text, nil
	}
	return "", fmt.Errorf("%s: a %s sends nothing", ToolName, c.Kind)
}

func num(f *float64) string {
	if f == nil {
		return "…"
	}
	return strconv.FormatFloat(*f, 'f', -1, 64)
}

// Meta is the tag on a message a component sent (MetaSource).
func Meta(c Component, id string) map[string]any {
	return map[string]any{"source": MetaSource, "component": id, "kind": c.Kind}
}

// Via reads the tag back: "choices" or "form" for a message a component
// sent, "" otherwise.
func Via(meta map[string]any) string {
	if meta == nil || meta["source"] != MetaSource {
		return ""
	}
	kind, _ := meta["kind"].(string)
	return kind
}

// SignalID is the call id as a Datastar signal fragment: the id with every
// character that is not a letter, digit or underscore replaced, or "" when
// the id cannot ride in a page attribute at all (then the component is drawn
// static).
func SignalID(id string) string {
	if !safeID.MatchString(id) {
		return ""
	}
	return strings.ReplaceAll(id, "-", "_")
}

// FieldSignal is the Datastar signal a form field binds to.
func FieldSignal(id, field string) string { return "f_" + SignalID(id) + "_" + field }

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
	case "choices":
		fmt.Fprintf(&b, ", %d options; the user's tap arrives as their next message", len(c.Choices.Options))
	case "form":
		fmt.Fprintf(&b, ", %d fields; the filled template arrives as the user's next message", len(c.Form.Fields))
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
// image and no script in a component. The only attribute values that are
// not fixed are the call id (checked against safeID) and field names
// (checked against fieldName) in the Datastar signals a choices or a form
// posts; a call whose id cannot ride in an attribute is drawn static.
func HTML(c Component, id string, text func(string) string) string {
	if text == nil {
		text = func(s string) string { return s }
	}
	esc := func(s string) string { return html.EscapeString(text(s)) }
	sid := SignalID(id)
	const post = `@post('/chat/send',{requestCancellation:'disabled'})`
	var b strings.Builder
	switch c.Kind {
	case "choices":
		b.WriteString(`<div class="comp comp-choices"`)
		if sid != "" {
			b.WriteString(` data-signals__ifmissing="{component:'',choice:-1}"`)
		}
		b.WriteString(`>`)
		for i, o := range c.Choices.Options {
			if sid != "" {
				fmt.Fprintf(&b, `<button type="button" class="btn sm" data-on:click="$component='%s';$choice=%d;%s">%s</button>`, id, i, post, esc(o.Label))
			} else {
				fmt.Fprintf(&b, `<span class="btn sm off">%s</span>`, esc(o.Label))
			}
		}
		b.WriteString(`</div>`)
	case "form":
		f := c.Form
		if sid == "" {
			b.WriteString(`<div class="comp comp-form static">`)
			for _, fl := range f.Fields {
				b.WriteString(`<div class="row"><span>` + esc(fl.Label) + `</span></div>`)
			}
			b.WriteString(`</div>`)
			break
		}
		b.WriteString(`<form class="comp comp-form" data-signals__ifmissing="{component:''`)
		for _, fl := range f.Fields {
			fmt.Fprintf(&b, `,%s:'%s'`, FieldSignal(id, fl.Name), jsString(fl.Default))
		}
		fmt.Fprintf(&b, `}" data-on:submit__prevent="$component='%s';%s">`, id, post)
		for _, fl := range f.Fields {
			sig := FieldSignal(id, fl.Name)
			b.WriteString(`<label class="field"><span class="lbl">` + esc(fl.Label) + `</span>`)
			switch fl.Type {
			case "select":
				fmt.Fprintf(&b, `<select class="cinput" data-bind:%s>`, sig)
				for _, o := range fl.Options {
					b.WriteString(`<option>` + esc(o) + `</option>`)
				}
				b.WriteString(`</select>`)
			case "range":
				fmt.Fprintf(&b, `<input type="range" min="%s" max="%s" data-bind:%s> <output data-text="$%s"></output>`, num(fl.Min), num(fl.Max), sig, sig)
			case "number":
				fmt.Fprintf(&b, `<input type="number" class="cinput"`)
				if fl.Min != nil {
					fmt.Fprintf(&b, ` min="%s"`, num(fl.Min))
				}
				if fl.Max != nil {
					fmt.Fprintf(&b, ` max="%s"`, num(fl.Max))
				}
				fmt.Fprintf(&b, ` data-bind:%s>`, sig)
			default:
				fmt.Fprintf(&b, `<input type="text" class="cinput" autocomplete="off" data-bind:%s>`, sig)
			}
			b.WriteString(`</label>`)
		}
		submit := f.Submit
		if submit == "" {
			submit = "Send"
		}
		b.WriteString(`<button type="submit" class="btn sm on">` + esc(submit) + `</button></form>`)
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

// jsString makes s safe inside a single-quoted literal in a Datastar
// expression attribute: quotes, backslashes, angle brackets and ampersands
// become escapes the attribute carries without ever closing it.
func jsString(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\', r == '\'', r == '"', r == '<', r == '>', r == '&', r == '\n', r == '\r', r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
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
