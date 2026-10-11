package dashboard

// The Chat page's answer streams and settles (TEN-515): tokens gather into
// one bubble that is sent at most every streamEvery, the final answer replaces
// it rendered from Markdown, a tool call is a card that settles in place, and
// an opened page starts with the conversation so far.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

func TestChatStream_TokensThenMarkdownFinal(t *testing.T) {
	cs := newChatStream()
	first := cs.patches(agent.Event{Kind: agent.EventToken, Text: "Hello **wor"})
	// The first token clears a bubble an earlier stream may have left (a
	// reconnect), then appends this one.
	if len(first) != 2 || !strings.Contains(string(first[0]), "mode remove") || !strings.Contains(string(first[0]), "selector #chat-cur") ||
		!strings.Contains(string(first[1]), "selector #chat-log") || !strings.Contains(string(first[1]), `id="chat-cur"`) || !strings.Contains(string(first[1]), "Hello **wor") {
		t.Fatalf("first token should clear then append the streaming bubble: %q", first)
	}
	if more := cs.patches(agent.Event{Kind: agent.EventToken, Text: "ld**"}); more != nil {
		t.Fatalf("a later token should wait for the tick, got %q", more)
	}
	if p := string(cs.flush()); !strings.Contains(p, "Hello **world**") || strings.Contains(p, "selector") {
		t.Fatalf("the tick should morph #chat-cur with the text so far: %q", p)
	}
	if cs.flush() != nil {
		t.Fatal("nothing new, nothing sent")
	}
	fin := cs.patches(agent.Event{Kind: agent.EventFinal, Text: "Hello **world**\n\n- one\n- two"})
	if len(fin) != 1 {
		t.Fatalf("final patches = %d", len(fin))
	}
	got := string(fin[0])
	for _, want := range []string{"mode replace", "selector #chat-cur", `class="bubble md"`, "<strong>world</strong>", "<li>one</li>"} {
		if !strings.Contains(got, want) {
			t.Errorf("final answer missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, `id="chat-cur"`) {
		t.Error("the settled answer must not keep the streaming id")
	}
	// The next answer starts a fresh bubble.
	if p := cs.patches(agent.Event{Kind: agent.EventToken, Text: "again"}); len(p) != 2 || !strings.Contains(string(p[1]), "mode append") {
		t.Fatalf("a new answer should append a new bubble: %q", p)
	}
}

// An answer with no tokens (a non-streaming backend) appends the final bubble;
// text written before a tool call settles as its own bubble so the card sits
// where the call happened.
func TestChatStream_FinalWithoutTokensAndTextBeforeTool(t *testing.T) {
	cs := newChatStream()
	if p := cs.patches(agent.Event{Kind: agent.EventFinal, Text: "done"}); len(p) != 1 || !strings.Contains(string(p[0]), "mode append") || !strings.Contains(string(p[0]), "done") {
		t.Fatalf("final without tokens: %q", p)
	}
	cs.patches(agent.Event{Kind: agent.EventToken, Text: "Let me look."})
	ps := cs.patches(agent.Event{Kind: agent.EventToolCall, Tool: "web_search", ToolCallID: "c1", Args: `{"q":"x"}`})
	if len(ps) != 2 || !strings.Contains(string(ps[0]), "mode replace") || !strings.Contains(string(ps[0]), "Let me look.") || !strings.Contains(string(ps[1]), `id="tool-`+cs.cardID("c1")+`"`) {
		t.Fatalf("text before a call should settle, then the card appends: %q", ps)
	}
}

// A tool card appears running and settles in place by the call's id; a failed
// call settles open with its error; a call with no id is matched by name.
func TestChatStream_ToolCardsSettleInPlace(t *testing.T) {
	cs := newChatStream()
	cs.patches(agent.Event{Kind: agent.EventTurnStart})
	c1 := cs.cardID("c1")
	call := string(cs.patches(agent.Event{Kind: agent.EventToolCall, Tool: "web_search", ToolCallID: "c1", Args: `{"q":"tenant docs"}`, TraceID: "tr1"})[0])
	for _, want := range []string{"mode append", `<details class="tool" id="tool-` + c1 + `">`, "⚡ web_search", `pill run`, "running", `{&#34;q&#34;:&#34;tenant docs&#34;}`} {
		if !strings.Contains(call, want) {
			t.Errorf("running card missing %q in:\n%s", want, call)
		}
	}
	res := string(cs.patches(agent.Event{Kind: agent.EventToolResult, Tool: "web_search", ToolCallID: "c1", Result: "three results", Duration: 1200 * time.Millisecond, TraceID: "tr1"})[0])
	for _, want := range []string{`id="tool-` + c1 + `"`, `pill ok`, "ok 1.2s", "three results", `href="/logs?`, "trace", "open in Logs"} {
		if !strings.Contains(res, want) {
			t.Errorf("settled card missing %q in:\n%s", want, res)
		}
	}
	if strings.Contains(res, "selector") || strings.Contains(res, "mode ") {
		t.Errorf("a settled card morphs by id, no selector or mode: %s", res)
	}
	cs.patches(agent.Event{Kind: agent.EventToolCall, Tool: "os_exec", ToolCallID: "c2"})
	errCard := string(cs.patches(agent.Event{Kind: agent.EventToolResult, Tool: "os_exec", ToolCallID: "c2", Result: "blocked: needs your OK", IsErr: true})[0])
	if !strings.Contains(errCard, `id="tool-`+cs.cardID("c2")+`" open>`) || !strings.Contains(errCard, "pill err") || !strings.Contains(errCard, "blocked: needs your OK") {
		t.Errorf("failed call not open with its error:\n%s", errCard)
	}
	// No id from the model: the card gets one, and the result finds it by name.
	noID := string(cs.patches(agent.Event{Kind: agent.EventToolCall, Tool: "os_read"})[0])
	if !strings.Contains(noID, `id="tool-`+cs.cardID("n1")+`"`) {
		t.Fatalf("a call without an id should get one: %s", noID)
	}
	if got := string(cs.patches(agent.Event{Kind: agent.EventToolResult, Tool: "os_read", Result: "x"})[0]); !strings.Contains(got, `id="tool-`+cs.cardID("n1")+`"`) || strings.Contains(got, "mode append") {
		t.Errorf("the result should settle the card it matches by name: %s", got)
	}
	// A result whose call this stream never saw (dropped, or the page opened
	// mid-turn) is appended on its own, after clearing a stale card.
	lost := cs.patches(agent.Event{Kind: agent.EventToolResult, Tool: "web_read", ToolCallID: "c7", Result: "page text"})
	if len(lost) != 2 || !strings.Contains(string(lost[0]), "mode remove") || !strings.Contains(string(lost[1]), "mode append") || !strings.Contains(string(lost[1]), "page text") {
		t.Errorf("a result without its call should still show: %q", lost)
	}
	// The same call id in the next turn is a different card.
	cs.patches(agent.Event{Kind: agent.EventTurnStart})
	again := string(cs.patches(agent.Event{Kind: agent.EventToolCall, Tool: "web_search", ToolCallID: "c1"})[0])
	if strings.Contains(again, `id="tool-`+c1+`"`) || !strings.Contains(again, `id="tool-`+cs.cardID("c1")+`"`) {
		t.Errorf("a reused call id must not reuse the card id: %s", again)
	}
	if newChatStream().tag == "" {
		t.Error("a stream needs a tag")
	}
}

// Raw HTML in an answer is text on the page; a script link is dropped.
func TestAnswerBubble_RawHTMLIsText(t *testing.T) {
	got := answerBubble("hi <script>alert(1)</script> [x](javascript:alert(1)) <b>bold</b>", "")
	for _, bad := range []string{"<script", "javascript:", "<b>"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q reached the page:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Errorf("the tag should show as text:\n%s", got)
	}
}

// Opening Chat shows the last exchanges: each message sent, answers as
// Markdown, tool calls as settled cards, a compaction summary as a dim row;
// only the last n exchanges when the conversation is longer.
func TestChatPage_ShowsTranscript(t *testing.T) {
	s := mustServer()
	s.SetTranscript(func() []working.Message {
		msgs := []working.Message{{Role: "system", Kind: "summary", Content: "earlier"}, {Role: "system", Kind: "summary", Content: "earlier too"}}
		for i := 0; i < 25; i++ {
			msgs = append(msgs, working.Message{Role: "user", Content: "question " + string(rune('A'+i))},
				working.Message{Role: "assistant", Content: "answer " + string(rune('A'+i))})
		}
		msgs = append(msgs,
			working.Message{Role: "user", Content: "back it up <i>now</i>"},
			working.Message{Role: "assistant", Content: "Checking.", ToolCalls: []model.ToolCall{{ID: "c9", Name: "os_exec", Arguments: json.RawMessage(`{"cmd":"tenant backup --help"}`)}}},
			working.Message{Role: "tool", ToolCallID: "c9", Content: "Usage: tenant backup"},
			working.Message{Role: "assistant", Content: "## Steps\n\n1. `tenant backup create`"},
			working.Message{Role: "tool", ToolCallID: "gone", Content: "a result whose call was compacted away"},
		)
		return msgs
	})
	rec := get(t, s, "/chat")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	// 25 pairs (A..Y) plus the backup exchange: the last 20 start at G.
	for _, want := range []string{"back it up &lt;i&gt;now&lt;/i&gt;", `class="bubble md"`, "<h2>Steps</h2>", "<code>tenant backup create</code>", `id="tool-c9"`, "Usage: tenant backup", "pill ok", "question Y", "question G", "a result whose call was compacted away"} {
		if !strings.Contains(body, want) {
			t.Errorf("chat page missing %q", want)
		}
	}
	if strings.Contains(body, "question F") || strings.Contains(body, "summarized") {
		t.Errorf("the page should show the last 20 exchanges only (F and the summary rows are older)")
	}
	// Inside the window, two summaries are one row.
	s2 := mustServer()
	s2.SetTranscript(func() []working.Message {
		return []working.Message{{Role: "user", Content: "q"}, {Role: "system", Kind: "summary"}, {Role: "system", Kind: "summary"}, {Role: "assistant", Content: "a"}}
	})
	if b := get(t, s2, "/chat").Body.String(); strings.Count(b, "summarized to save room") != 1 {
		t.Errorf("summary rows = %d, want 1", strings.Count(b, "summarized to save room"))
	}
	if !strings.Contains(get(t, mustServer(), "/chat").Body.String(), "conversation so far") {
		t.Error("the page's note is missing")
	}
}
