package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// failRunner fails every turn the way the serve turn gate rejects one: no
// event, only the returned error.
type failRunner struct{}

func (failRunner) Turn(ctx context.Context, _ agent.TurnRequest) (*agent.TurnResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("turn scheduler queue full (depth 32)")
}
func (failRunner) Interject(string) {}

// A turn that fails before it emits anything is logged and shown in the chat
// the operator sent it from, instead of vanishing.
func TestSSR_ChatSend_TurnFailureReachesChatAndLog(t *testing.T) {
	c := installCapture(t)
	broker := agent.NewBroker(0)
	ch, unsub := broker.Subscribe()
	defer unsub()
	s := New(Config{}, failRunner{}, &fakeTools{}, nil, broker, nil)
	r := httptest.NewRequest("POST", "/chat/send", strings.NewReader(`{"text":"hello"}`))
	r.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(httptest.NewRecorder(), r)
	select {
	case ev := <-ch:
		// The chat says it plainly (D21); the log keeps the reason.
		if ev.Kind != agent.EventError || ev.Text != "Your agent couldn't answer that message. The Logs page has the reason." {
			t.Fatalf("event = %+v, want a plain error event", ev)
		}
		if b := chatBubble(ev); !strings.Contains(b, "couldn&#39;t answer") && !strings.Contains(b, "couldn't answer") {
			t.Errorf("the chat does not show it: %q", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a failed turn posted nothing to the chat")
	}
	// The event is written before the chat's is published.
	if !c.has("2206 ", "Chat turn failed", "queue full") {
		t.Errorf("not logged: %q", c.lines())
	}
}

// A turn the operator stopped did not fail: nothing is posted.
func TestChatTurnStoppedIsNotReported(t *testing.T) {
	broker := agent.NewBroker(0)
	ch, unsub := broker.Subscribe()
	defer unsub()
	co := newWSCoordinator(failRunner{}, broker)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	co.runTurn(ctx, "hello")
	select {
	case ev := <-ch:
		t.Fatalf("a stopped turn was reported: %+v", ev)
	default:
	}
}

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
	// TEN-232: an offsite ingest event renders no chat bubble.
	if chatBubble(agent.Event{Kind: agent.EventIngest, Text: "Discord: hey"}) != "" {
		t.Error("ingest events should not render a chat bubble")
	}
}

// TEN-234: cross-agent and bus events never enter the operator's chat
// transcript.
func TestChatBubble_LeavesOutOtherAgents(t *testing.T) {
	if chatBubble(agent.Event{Kind: agent.EventToolCall, Agent: "writer", Tool: "os_write"}) != "" {
		t.Error("sub-agent events must not render a chat bubble")
	}
	if chatBubble(agent.Event{Kind: agent.EventBus, Agent: "researcher", Text: "→ writer: hi"}) != "" {
		t.Error("bus events must not render a chat bubble")
	}
}

// REVSEC-8: hidden characters show as markers in the chat transcript, as on
// the approval card, and a cut never splits a character.
func TestFeedRows_ShowHiddenCharacters(t *testing.T) {
	cmd := "run echo safe ‮ evil ⁦ done"
	rows := []string{
		chatBubble(agent.Event{Kind: agent.EventToolCall, Tool: "os_exec", Args: cmd}),
		chatBubble(agent.Event{Kind: "final", Text: cmd}),
	}
	for _, row := range rows {
		if strings.ContainsAny(row, "‮⁦") || !strings.Contains(row, "⟨U+202E⟩") {
			t.Errorf("a row shows the raw characters: %q", row)
		}
	}
	long := strings.Repeat("é", 150) // 2 bytes each: an odd cut lands mid-character
	if got := snippetStr(long, 201); !utf8.ValidString(got) {
		t.Errorf("snippetStr cut a character: %q", got)
	}
}
