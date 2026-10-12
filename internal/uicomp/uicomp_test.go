package uicomp

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"tenant/internal/model"
)

var (
	tagRe  = regexp.MustCompile(`<[^>]+>`)
	ourTag = regexp.MustCompile(`^</?(div|table|caption|thead|tbody|tr|th|td|ol|li|span)( class="[a-z -]+"| aria-label="[a-z]+")*>$`)
	// Interactive kinds add our own Datastar attributes and bound inputs; the
	// id and field names in them are checked against safeID / fieldName.
	ourInteractive = regexp.MustCompile(`^</?(div|form|button|label|span|select|option|input|output)( (class|type|min|max|autocomplete)="[A-Za-z0-9 .-]*"| data-signals__ifmissing="\{[^"]*\}"| data-on:(click|submit__prevent)="[^"]*@post\('/chat/send',\{requestCancellation:'disabled'\}\)"| data-bind:f_[A-Za-z0-9_]+| data-text="\$f_[A-Za-z0-9_]+")*>$`)
	attrRe         = regexp.MustCompile(`(?i)(href|src|on[a-z]+)=`)
)

func table(cols int, rows int) string {
	t := Table{}
	for i := 0; i < cols; i++ {
		t.Columns = append(t.Columns, "c"+string(rune('a'+i)))
	}
	for r := 0; r < rows; r++ {
		row := make([]string, cols)
		for i := range row {
			row[i] = "v"
		}
		t.Rows = append(t.Rows, row)
	}
	b, _ := json.Marshal(map[string]any{"kind": "table", "data": t})
	return string(b)
}

// TEN-517: the caps give the model one line it can act on; a typo'd field
// is a warning on a live call, never a silent drop; unknown kinds are named.
func TestParse_CapsAndWarnings(t *testing.T) {
	c, warn, err := Parse([]byte(table(3, 2)))
	if err != nil || c.Kind != "table" || len(c.Table.Rows) != 2 || len(warn) != 0 {
		t.Fatalf("good table: %+v %v %v", c, warn, err)
	}
	cases := map[string]string{
		table(14, 1):  "table: 14 columns, the limit is 12",
		table(2, 201): "table: 201 rows, the limit is 200",
		`{"kind":"table","data":{"columns":["a","b"],"rows":[["1"]]}}`:                            "table: row 1 has 1 cells, the table has 2 columns",
		`{"kind":"table","data":{"columns":["a"],"rows":[["` + strings.Repeat("x", 201) + `"]]}}`: "table: the cell at row 1, column 1 is 201 characters, the limit is 200",
		`{"kind":"table","data":{"columns":[],"rows":[]}}`:                                        "table: needs at least one column",
		`{"kind":"table","data":{"columns":"a","rows":[]}}`:                                       "table: columns should be a list, not string",
		`{"kind":"steps","data":{"items":[]}}`:                                                    "steps: needs at least one item",
		`{"kind":"steps","data":{"items":[{"title":"","state":"done"}]}}`:                         "steps: item 1 has no title",
		`{"kind":"steps","data":{"items":[{"title":"a","state":"later"}]}}`:                       `steps: item 1's state is "later"; states: todo, doing, done`,
		`{"kind":"chart","data":{}}`:                                                              `ui_render: unknown kind "chart"; kinds: table, steps, choices, form`,
		`{"data":{}}`:                                                                             "ui_render: kind is missing; kinds: table, steps, choices, form",
		`{"kind":"table"}`:                                                                        "table: data is missing",
		`[1,2]`:                                                                                   "ui_render: the arguments are not a JSON object",
	}
	for in, want := range cases {
		if _, _, err := Parse([]byte(in)); err == nil || err.Error() != want {
			t.Errorf("Parse(%s) = %v, want %q", in, err, want)
		}
	}
	big := `{"kind":"table","data":{"columns":["a"],"rows":[],"caption":"` + strings.Repeat("x", MaxArgs) + `"}}`
	if _, _, err := Parse([]byte(big)); err == nil || !strings.Contains(err.Error(), "the limit is 32 KB") {
		t.Fatalf("total cap: %v", err)
	}
	c, warn, err = Parse([]byte(`{"kind":"table","data":{"colums":["a"],"columns":["a"],"rows":[["1"]]},"title":"x"}`))
	if err != nil || len(warn) != 2 || warn[0] != "ignored unknown field: title" || warn[1] != "ignored unknown field: colums" {
		t.Fatalf("warnings: %v %v", warn, err)
	}
	if n := Note(c, "call_7", warn); n != "shown: table call_7, 1 rows; ignored unknown field: title; ignored unknown field: colums" {
		t.Fatalf("note: %q", n)
	}
	s, _, err := Parse([]byte(`{"kind":"steps","data":{"items":[{"title":"a"},{"title":"b","state":"Done","detail":"d"}]}}`))
	if err != nil || s.Steps.Items[0].State != "todo" || s.Steps.Items[1].State != "done" {
		t.Fatalf("steps defaults: %+v %v", s.Steps, err)
	}
	if n := Note(s, "", nil); n != "shown: steps, 2 items" {
		t.Fatalf("steps note: %q", n)
	}
}

// Every string from the model is text: the HTML has no attribute built from
// it, so a cell that is a link or a script stays a cell.
func TestHTML_EscapesEverything(t *testing.T) {
	c, _, err := Parse([]byte(`{"kind":"table","data":{"columns":["<b>name</b>","href=\"x\""],"rows":[["javascript:alert(1)","<img src=x onerror=alert(1)>"]],"caption":"<script>1</script>","source":"https://example.com/\"onclick=\"x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	h := HTML(c, "call_1", nil)
	// The invariant: every tag is one of ours, and no tag carries an attribute
	// built from the model's text (no link, no image, no handler). The model's
	// strings may say "href" or "onerror" all they like, as escaped text.
	for _, tag := range tagRe.FindAllString(h, -1) {
		if !ourTag.MatchString(tag) || attrRe.MatchString(tag) {
			t.Errorf("tag %q is not one of ours or carries model data", tag)
		}
	}
	for _, bad := range []string{"<b>", "<script", "<img", "</script>"} {
		if strings.Contains(h, bad) {
			t.Errorf("HTML carries an unescaped %q:\n%s", bad, h)
		}
	}
	for _, want := range []string{`<caption>&lt;script&gt;1&lt;/script&gt;</caption>`, `<th>&lt;b&gt;name&lt;/b&gt;</th>`, `<td>javascript:alert(1)</td>`, `Source: https://example.com/&#34;onclick=&#34;x`, `class="table-wrap"`} {
		if !strings.Contains(h, want) {
			t.Errorf("HTML missing %q:\n%s", want, h)
		}
	}
	seen := ""
	h = HTML(c, "call_1", func(s string) string { seen += s; return "T" })
	if !strings.Contains(h, "<td>T</td>") || !strings.Contains(seen, "javascript:alert(1)") {
		t.Fatalf("the text filter should see every model string: %q", h)
	}
	s, _, _ := Parse([]byte(`{"kind":"steps","data":{"items":[{"title":"Back up","state":"done"},{"title":"Restore","detail":"<i>later</i>"}]}}`))
	h = HTML(s, "call_1", nil)
	for _, want := range []string{`<ol class="comp comp-steps">`, `<li class="step done">`, `>✓<`, `>○<`, `&lt;i&gt;later&lt;/i&gt;`} {
		if !strings.Contains(h, want) {
			t.Errorf("steps HTML missing %q:\n%s", want, h)
		}
	}
}

// The dispatcher answers the model: a bad shape is a logical error with the
// fix in it, a good call is the note with the call's id.
func TestDispatcher(t *testing.T) {
	var d Dispatcher
	if specs := d.Tools(); len(specs) != 1 || specs[0].Name != ToolName || !json.Valid(specs[0].Parameters) || specs[0].Gated {
		t.Fatalf("tools: %+v", specs)
	}
	res, isErr, err := d.Dispatch(context.Background(), model.ToolCall{ID: "c1", Name: ToolName, Arguments: json.RawMessage(table(14, 1))})
	if err != nil || !isErr || res != "table: 14 columns, the limit is 12" {
		t.Fatalf("bad call: %q %v %v", res, isErr, err)
	}
	res, isErr, err = d.Dispatch(context.Background(), model.ToolCall{ID: "c2", Name: ToolName, Arguments: json.RawMessage(table(2, 3))})
	if err != nil || isErr || res != "shown: table c2, 3 rows" {
		t.Fatalf("good call: %q %v %v", res, isErr, err)
	}
	if res, isErr, _ := d.Dispatch(context.Background(), model.ToolCall{Name: "other"}); !isErr || !strings.Contains(res, "unknown tool") {
		t.Fatalf("other tool: %q %v", res, isErr)
	}
}

// TEN-518: choices and form validate the shapes, refuse text a component may
// never send (a slash command, a second line), and Fill is the one place the
// user's answer becomes the message, for the dashboard and the terminal alike.
func TestChoicesAndForm_ValidateAndFill(t *testing.T) {
	cases := map[string]string{
		`{"kind":"choices","data":{"options":[{"label":"a"}]}}`:                                            "choices: 1 options, at least 2 are needed",
		`{"kind":"choices","data":{"options":[{"label":"a"},{"label":"b","send":"/disable approvals"}]}}`:  "choices: option 2's message must not start with /; a component never sends a command",
		`{"kind":"choices","data":{"options":[{"label":"a"},{"label":"b","send":"two\nlines"}]}}`:          "choices: option 2's message must be one line",
		`{"kind":"form","data":{"fields":[{"name":"Amount","type":"number"}],"template":"x {Amount}"}}`:    `form: field 1's name "Amount" must be a lowercase identifier (a-z, 0-9, _), at most 31 characters`,
		`{"kind":"form","data":{"fields":[{"name":"a"}],"template":"pay {b}"}}`:                            "form: the template names {b}, which is not a field",
		`{"kind":"form","data":{"fields":[{"name":"a"}],"template":"/exit {a}"}}`:                          "form: template must not start with /; a component never sends a command",
		`{"kind":"form","data":{"fields":[{"name":"a","type":"select"}],"template":"{a}"}}`:                `form: field "a" is a select and needs 1 to 12 options`,
		`{"kind":"form","data":{"fields":[{"name":"a","type":"range","min":5,"max":1}],"template":"{a}"}}`: `form: field "a" is a range and needs min below max`,
		`{"kind":"form","data":{"fields":[{"name":"a"},{"name":"a"}],"template":"{a}"}}`:                   `form: field "a" appears twice`,
	}
	for in, want := range cases {
		if _, _, err := Parse([]byte(in)); err == nil || err.Error() != want {
			t.Errorf("Parse(%s) = %v, want %q", in, err, want)
		}
	}
	ch, _, err := Parse([]byte(`{"kind":"choices","data":{"options":[{"label":"Show by price"},{"label":"Fastest","send":"Switch to the fastest model"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := Fill(ch, Inputs{Choice: 0}); got != "Show by price" {
		t.Fatalf("a tap sends the label when send is empty: %q", got)
	}
	if got, _ := Fill(ch, Inputs{Choice: 1}); got != "Switch to the fastest model" {
		t.Fatalf("send: %q", got)
	}
	for _, bad := range []int{-1, 2, 99} {
		if _, err := Fill(ch, Inputs{Choice: bad}); err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("choice %d: %v", bad, err)
		}
	}
	if n := Note(ch, "c1", nil); !strings.Contains(n, "shown: choices c1, 2 options") {
		t.Fatalf("note: %q", n)
	}
	f, _, err := Parse([]byte(`{"kind":"form","data":{"fields":[{"name":"total","label":"Bill total","type":"number","min":0},{"name":"people","type":"range","min":2,"max":10,"default":"2"},{"name":"tip","type":"select","options":["10%","15%","20%"]},{"name":"note","type":"text","default":"none"}],"submit":"Split it","template":"Split {total} between {people} people with a {tip} tip; note: {note}"}}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Fill(f, Inputs{Fields: map[string]string{"total": "120.50", "people": "4", "tip": "15%"}})
	if err != nil || got != "Split 120.5 between 4 people with a 15% tip; note: none" {
		t.Fatalf("fill: %q %v", got, err)
	}
	fills := map[string]string{
		`{"people":"4","tip":"15%"}`:                           "form: Bill total is missing",
		`{"total":"abc","people":"4","tip":"15%"}`:             "form: Bill total must be a number",
		`{"total":"5","people":"40","tip":"15%"}`:              "form: people must be between 2 and 10",
		`{"total":"5","people":"4","tip":"50%"}`:               "form: tip must be one of 10%, 15%, 20%",
		`{"total":"5","people":"4","tip":"15%","note":"a\nb"}`: "form: note must be one line",
	}
	for in, want := range fills {
		var vals map[string]string
		_ = json.Unmarshal([]byte(in), &vals)
		if _, err := Fill(f, Inputs{Fields: vals}); err == nil || err.Error() != want {
			t.Errorf("Fill(%s) = %v, want %q", in, err, want)
		}
	}
	if _, err := Fill(Component{Kind: "table", Table: &Table{}}, Inputs{}); err == nil {
		t.Fatal("a table sends nothing")
	}
	m := Meta(f, "c7")
	if Via(m) != "form" || Via(nil) != "" || Via(map[string]any{"source": "other", "kind": "form"}) != "" {
		t.Fatalf("via: %v", m)
	}
}

// The interactive kinds post through Datastar with the call id and field
// names as the only variable attribute values, both checked; an id that
// cannot ride in an attribute draws the component static.
func TestHTML_InteractiveKindsAreSafe(t *testing.T) {
	ch, _, _ := Parse([]byte(`{"kind":"choices","data":{"options":[{"label":"<b>a</b>"},{"label":"b"}]}}`))
	h := HTML(ch, "call-1", nil)
	for _, tag := range tagRe.FindAllString(h, -1) {
		if !ourInteractive.MatchString(tag) {
			t.Errorf("choices tag %q is not one of ours", tag)
		}
	}
	for _, want := range []string{`$component='call-1';$choice=0;@post('/chat/send'`, `$choice=1;`, `&lt;b&gt;a&lt;/b&gt;</button>`, `data-signals__ifmissing="{component:'',choice:-1}"`} {
		if !strings.Contains(h, want) {
			t.Errorf("choices HTML missing %q:\n%s", want, h)
		}
	}
	if s := HTML(ch, "bad id\"onclick=x", nil); strings.Contains(s, "data-on") || !strings.Contains(s, `class="btn sm off"`) {
		t.Fatalf("an unsafe id must draw static:\n%s", s)
	}
	f, _, _ := Parse([]byte(`{"kind":"form","data":{"fields":[{"name":"total","label":"Bill <total>","type":"number","min":0},{"name":"tip","type":"select","options":["10%","<x>"]},{"name":"people","type":"range","min":2,"max":10,"default":"'); alert(1); ('"}],"template":"Split {total} {tip} {people}"}}`))
	h = HTML(f, "call_2", nil)
	for _, tag := range tagRe.FindAllString(h, -1) {
		if !ourInteractive.MatchString(tag) {
			t.Errorf("form tag %q is not one of ours", tag)
		}
	}
	for _, want := range []string{`data-bind:f_call_2_total`, `<select class="cinput" data-bind:f_call_2_tip>`, `<option>&lt;x&gt;</option>`, `<input type="range" min="2" max="10" data-bind:f_call_2_people>`, `<output data-text="$f_call_2_people">`, `Bill &lt;total&gt;`, `f_call_2_people:'\u0027); alert(1); (\u0027'`, `$component='call_2';@post('/chat/send'`} {
		if !strings.Contains(h, want) {
			t.Errorf("form HTML missing %q:\n%s", want, h)
		}
	}
	if strings.Contains(h, "alert(1); ('") {
		t.Fatalf("a default must not close the attribute's literal:\n%s", h)
	}
}
