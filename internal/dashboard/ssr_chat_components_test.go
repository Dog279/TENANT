package dashboard

// Components in the chat (TEN-517): a ui_render call is drawn as the table or
// steps it names, in place of the tool card, at the call and again from the
// transcript; a call the validator refuses stays an ordinary card with the
// error; every string from the model is text.

import (
	"encoding/json"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

const tableArgs = `{"kind":"table","data":{"columns":["Model","Cost"],"rows":[["glm","$0"],["<b>claude</b>","$3"]],"caption":"Configured models"}}`

func TestChatStream_ComponentDrawnAtTheCallAndSettledInPlace(t *testing.T) {
	cs := newChatStream()
	call := cs.patches(agent.Event{Kind: agent.EventToolCall, Tool: "ui_render", ToolCallID: "c1", Args: tableArgs})
	if len(call) != 1 {
		t.Fatalf("call patches = %d", len(call))
	}
	got := string(call[0])
	for _, want := range []string{"mode append", `class="msg comp-wrap"`, `class="comp comp-table"`, "<caption>Configured models</caption>", "<th>Model</th>", "<td>glm</td>", "&lt;b&gt;claude&lt;/b&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("component at the call missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "details") || strings.Contains(got, "running…") || strings.Contains(got, "<b>claude") {
		t.Fatalf("a component is not a tool card and carries no raw markup:\n%s", got)
	}
	id := cardIDIn(t, got)
	done := cs.patches(agent.Event{Kind: agent.EventToolResult, Tool: "ui_render", ToolCallID: "c1", Args: tableArgs, Result: "shown: table c1, 2 rows", Duration: 1})
	if len(done) != 1 || !strings.Contains(string(done[0]), `id="`+id+`"`) || !strings.Contains(string(done[0]), "<td>glm</td>") || strings.Contains(string(done[0]), "details") {
		t.Fatalf("the result should settle the same component in place: %q", done)
	}
}

// A call the validator refuses is the ordinary card, open, with the error the
// model got; so is a result that failed.
func TestChatStream_BadComponentIsAnOrdinaryCard(t *testing.T) {
	cs := newChatStream()
	bad := `{"kind":"table","data":{"columns":["a","b"],"rows":[["1"]]}}`
	call := cs.patches(agent.Event{Kind: agent.EventToolCall, Tool: "ui_render", ToolCallID: "c2", Args: bad})
	if len(call) != 1 || !strings.Contains(string(call[0]), `<details class="tool running"`) || strings.Contains(string(call[0]), "comp-table") {
		t.Fatalf("a bad shape should be the running card: %q", call)
	}
	res := cs.patches(agent.Event{Kind: agent.EventToolResult, Tool: "ui_render", ToolCallID: "c2", Args: bad, Result: "table: row 1 has 1 cells, the table has 2 columns", IsErr: true, Duration: 1})
	if len(res) != 1 || !strings.Contains(string(res[0]), "the table has 2 columns") || !strings.Contains(string(res[0]), " open") {
		t.Fatalf("the error card should show the model's error, open: %q", res)
	}
}

// The transcript on load draws a saved component the same way.
func TestTranscript_RendersComponents(t *testing.T) {
	s := &Server{}
	s.SetTranscript(func() []working.Message {
		return []working.Message{
			{Role: "user", Content: "which models?"},
			{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "c9", Name: "ui_render", Arguments: json.RawMessage(tableArgs)}}},
			{Role: "tool", ToolCallID: "c9", Content: "shown: table c9, 2 rows"},
			{Role: "assistant", Content: "Two models are configured."},
		}
	})
	var all strings.Builder
	for _, h := range s.transcriptHTML(20) {
		all.WriteString(string(h))
	}
	got := all.String()
	if !strings.Contains(got, `id="tool-c9"`) || !strings.Contains(got, "<td>glm</td>") || strings.Contains(got, "<details") {
		t.Fatalf("transcript should draw the component, not a card:\n%s", got)
	}
}

// cardIDIn pulls the id of the one element a patch appends.
func cardIDIn(t *testing.T, patch string) string {
	t.Helper()
	i := strings.Index(patch, `id="`)
	if i < 0 {
		t.Fatalf("no id in %q", patch)
	}
	rest := patch[i+4:]
	return rest[:strings.Index(rest, `"`)]
}
