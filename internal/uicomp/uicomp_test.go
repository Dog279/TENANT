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
	attrRe = regexp.MustCompile(`(?i)(href|src|on[a-z]+)=`)
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
		`{"kind":"chart","data":{}}`:                                                              `ui_render: unknown kind "chart"; kinds: table, steps`,
		`{"data":{}}`:                                                                             "ui_render: kind is missing; kinds: table, steps",
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
	h := HTML(c, nil)
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
	h = HTML(c, func(s string) string { seen += s; return "T" })
	if !strings.Contains(h, "<td>T</td>") || !strings.Contains(seen, "javascript:alert(1)") {
		t.Fatalf("the text filter should see every model string: %q", h)
	}
	s, _, _ := Parse([]byte(`{"kind":"steps","data":{"items":[{"title":"Back up","state":"done"},{"title":"Restore","detail":"<i>later</i>"}]}}`))
	h = HTML(s, nil)
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
