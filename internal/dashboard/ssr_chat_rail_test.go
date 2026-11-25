package dashboard

// The activity rail (TEN-520): the events stream appends one line per event
// the terminal app's feed would show, keeps the last 200, and leaves the
// chat's own events (tokens, the answer) out.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
)

func TestChatStream_RailLinesAndCap(t *testing.T) {
	cs := newChatStream()
	if p := cs.railPatches(agent.Event{Kind: agent.EventToken, Text: "hi"}); p != nil {
		t.Fatalf("tokens are the chat's, not the rail's: %q", p)
	}
	ev := agent.Event{Kind: agent.EventToolCall, Tool: "web_search", ToolCallID: "c1", Args: `{"q":"<b>tenant</b>"}`}
	ps := cs.railPatches(ev)
	if len(ps) != 1 {
		t.Fatalf("one rail line per event: %d", len(ps))
	}
	rail := string(ps[0])
	for _, want := range []string{"mode append", "selector #activity-rail", `class="line k-tool" id="ar-1"`, "⚡ web_search(", "&lt;b&gt;tenant&lt;/b&gt;", "<time>"} {
		if !strings.Contains(rail, want) {
			t.Errorf("rail line missing %q in:\n%s", want, rail)
		}
	}
	if chat := cs.patches(ev); len(chat) != 1 || strings.Contains(string(chat[0]), "activity-rail") {
		t.Fatalf("the chat's own patches are unchanged by the rail: %q", chat)
	}
	res := cs.railPatches(agent.Event{Kind: agent.EventToolResult, Tool: "web_search", IsErr: true, Result: "timeout", Duration: 1200 * time.Millisecond})
	if len(res) != 1 || !strings.Contains(string(res[0]), `k-err" id="ar-2"`) || !strings.Contains(string(res[0]), "✗ web_search · 1.2s · timeout") {
		t.Fatalf("a failed result is a red line: %q", res)
	}
	for i := 0; i < railCap-2; i++ {
		cs.railPatches(agent.Event{Kind: agent.EventJudge, Text: "done ✓"})
	}
	over := cs.railPatches(agent.Event{Kind: agent.EventTurnEnd, Duration: 3 * time.Second})
	if len(over) != 2 || !strings.Contains(string(over[0]), `id="ar-`+strconv.Itoa(railCap+1)+`"`) || !strings.Contains(string(over[1]), "mode remove") || !strings.Contains(string(over[1]), "selector #ar-1") {
		t.Fatalf("the line past the cap removes the oldest: %q", over)
	}
}

// A fleet call's rail lines name the machine it works on (TEN-531), escaped
// like the rest of the line: the name is what the model asked for.
func TestChatStream_RailNamesAFleetCallsMachine(t *testing.T) {
	cs := newChatStream()
	for _, c := range []struct {
		ev   agent.Event
		want string
	}{
		{agent.Event{Kind: agent.EventToolCall, Tool: "fleet_exec", Host: "web1", Args: `{"host":"web1"}`}, "⚡ fleet_exec@web1("},
		{agent.Event{Kind: agent.EventToolResult, Tool: "fleet_exec", Host: "web1", Duration: time.Second}, "✓ fleet_exec@web1 · 1s"},
		{agent.Event{Kind: agent.EventToolResult, Tool: "fleet_read", Host: "<b>", IsErr: true, Result: "offline"}, "✗ fleet_read@&lt;b&gt; · "},
	} {
		if ps := cs.railPatches(c.ev); len(ps) != 1 || !strings.Contains(string(ps[0]), c.want) {
			t.Errorf("rail line for %+v: %q, want %q in it", c.ev, ps, c.want)
		}
	}
}

func TestChatPage_HasTheActivityToggle(t *testing.T) {
	s := New(Config{}, &chatRunner{}, &fakeTools{}, nil, nil, nil)
	body := get(t, s, "/chat").Body.String()
	for _, want := range []string{`<details class="activity"><summary>Show activity</summary>`, `id="activity-rail"`} {
		if !strings.Contains(body, want) {
			t.Errorf("chat page missing %q", want)
		}
	}
	if strings.Contains(body, "<script") && strings.Count(body, "<script") > strings.Count(body, "datastar.js")+strings.Count(body, "theme.js") {
		t.Fatal("the rail must add no script")
	}
}
