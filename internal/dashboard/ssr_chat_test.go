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
}
