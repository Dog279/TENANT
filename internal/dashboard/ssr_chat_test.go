package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
)

type chatRunner struct{ turns chan string }

func (f *chatRunner) Turn(_ context.Context, req agent.TurnRequest) (*agent.TurnResult, error) {
	if f.turns != nil {
		f.turns <- req.UserQuery
	}
	return &agent.TurnResult{}, nil
}
func (f *chatRunner) Interject(string) {}

func TestSSR_ChatPage(t *testing.T) {
	rec := get(t, mustServer(), "/chat")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat page want 200, got %d", rec.Code)
	}
	for _, want := range []string{`id="chat-log"`, "/events", "/chat/send", "/chat/stop"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("chat page missing %q", want)
		}
	}
}

func TestSSR_ChatSend_AppendsUserAndStartsTurn(t *testing.T) {
	fr := &chatRunner{turns: make(chan string, 1)}
	s := New(Config{}, fr, &fakeTools{}, nil, nil, nil)
	r := httptest.NewRequest("POST", "/chat/send", strings.NewReader(`{"text":"hello there"}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	body := rec.Body.String()
	if !strings.Contains(body, "datastar-patch-elements") || !strings.Contains(body, "selector #chat-log") {
		t.Errorf("expected a #chat-log append patch:\n%s", body)
	}
	if !strings.Contains(body, "hello there") {
		t.Errorf("expected the user message echoed:\n%s", body)
	}
	select {
	case q := <-fr.turns:
		if q != "hello there" {
			t.Errorf("turn query = %q", q)
		}
	case <-time.After(2 * time.Second):
		t.Error("background Turn was not started")
	}
}

func TestSSR_ChatSend_Empty(t *testing.T) {
	s := New(Config{}, &chatRunner{}, &fakeTools{}, nil, nil, nil)
	r := httptest.NewRequest("POST", "/chat/send", strings.NewReader(`{"text":"  "}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Errorf("empty chat send want 204, got %d", rec.Code)
	}
}

func TestSSR_ChatStop(t *testing.T) {
	s := New(Config{}, &chatRunner{}, &fakeTools{}, nil, nil, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/chat/stop", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("chat stop want 204, got %d", rec.Code)
	}
}

func TestSSR_Events_ReturnsOnCancel(t *testing.T) {
	s := New(Config{}, nil, &fakeTools{}, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled → handler should return promptly
	r := httptest.NewRequest("GET", "/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.handleEventsSSE(rec, r); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleEventsSSE did not return on a cancelled context")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("events should set SSE content-type, got %q", ct)
	}
}

func TestEventFragments(t *testing.T) {
	if b := chatBubble(agent.Event{Kind: agent.EventFinal, Text: "<b>hi</b>"}); !strings.Contains(b, "&lt;b&gt;hi&lt;/b&gt;") {
		t.Errorf("final bubble should html-escape text: %q", b)
	}
	if chatBubble(agent.Event{Kind: agent.EventToken, Text: "x"}) != "" {
		t.Error("token events should not render a chat bubble")
	}
	if tc := chatBubble(agent.Event{Kind: agent.EventToolCall, Tool: "web_read", Args: "{}"}); !strings.Contains(tc, "web_read") {
		t.Errorf("tool_call bubble missing tool name: %q", tc)
	}
	if row := activityRow(agent.Event{Kind: agent.EventError, Text: "boom <x>"}); !strings.Contains(row, "error") || !strings.Contains(row, "boom &lt;x&gt;") {
		t.Errorf("activity row wrong: %q", row)
	}
	// TEN-232: offsite ingest gets the inbox tag; the channel prefix rides in Text
	// and an ingest event renders no chat bubble (activity-feed only).
	if row := activityRow(agent.Event{Kind: agent.EventIngest, Text: "Discord: hey there"}); !strings.Contains(row, "inbox") || !strings.Contains(row, "Discord: hey there") {
		t.Errorf("ingest activity row wrong: %q", row)
	}
	if chatBubble(agent.Event{Kind: agent.EventIngest, Text: "Discord: hey"}) != "" {
		t.Error("ingest events should not render a chat bubble")
	}
}

// TEN-234: the activity feed shows meaningful activity, NOT token-by-token noise;
// cross-agent + bus events are attributed and never enter the chat transcript.
func TestActivityFeed_FiltersNoiseAndAttributesAgents(t *testing.T) {
	// Token / usage / assistant / memory are filtered OUT of the activity feed.
	for _, k := range []agent.EventKind{agent.EventToken, agent.EventUsage, agent.EventAssistant, agent.EventMemory} {
		if activityRelevant(agent.Event{Kind: k}) {
			t.Errorf("%s must be filtered out of the activity feed", k)
		}
	}
	// Activity-worthy kinds stay (denylist → meaningful + future kinds show).
	for _, k := range []agent.EventKind{agent.EventToolCall, agent.EventToolResult, agent.EventFinal, agent.EventBus, agent.EventIngest, agent.EventTurnStart} {
		if !activityRelevant(agent.Event{Kind: k}) {
			t.Errorf("%s must stay in the activity feed", k)
		}
	}

	// A bus message renders with the bus tag, the sender attribution, and the routing text.
	row := activityRow(agent.Event{Kind: agent.EventBus, Agent: "researcher", Text: "→ writer: draft ready"})
	if !strings.Contains(row, "bus") || !strings.Contains(row, "[researcher]") || !strings.Contains(row, "→ writer: draft ready") {
		t.Errorf("bus row missing tag/agent/detail: %q", row)
	}

	// A sub-agent tool call is attributed by agent id in the activity feed...
	subRow := activityRow(agent.Event{Kind: agent.EventToolCall, Agent: "writer", Tool: "os_write"})
	if !strings.Contains(subRow, "[writer]") || !strings.Contains(subRow, "os_write") {
		t.Errorf("sub-agent row should be attributed: %q", subRow)
	}
	// ...but cross-agent + bus events never enter the operator's chat transcript.
	if chatBubble(agent.Event{Kind: agent.EventToolCall, Agent: "writer", Tool: "os_write"}) != "" {
		t.Error("sub-agent events must not render a chat bubble")
	}
	if chatBubble(agent.Event{Kind: agent.EventBus, Agent: "researcher", Text: "→ writer: hi"}) != "" {
		t.Error("bus events must not render a chat bubble")
	}
}
