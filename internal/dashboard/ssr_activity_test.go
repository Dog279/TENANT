package dashboard

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"tenant/internal/agent"
)

// TEN-238: /activity backfills the retained log server-side; /activity/events
// replays only events AFTER the cursor (gap-free reconnect), tagged with the
// Seq as the SSE id.
func TestActivity_BackfillAndReplay(t *testing.T) {
	l := agent.NewEventLog(100)
	l.Append(agent.Event{Kind: agent.EventToolCall, Tool: "web_search"})
	l.Append(agent.Event{Kind: agent.EventError, Text: "boom-marker"})
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetEventLog(l)

	// Page render backfills BOTH events + stamps the head cursor (2) for the tail.
	body := get(t, s, "/activity").Body.String()
	if !strings.Contains(body, "web_search") || !strings.Contains(body, "boom-marker") {
		t.Errorf("/activity should backfill retained events; got:\n%s", body)
	}
	if !strings.Contains(body, "cursor=2") {
		t.Errorf("/activity should stamp head cursor=2; got:\n%s", body)
	}

	// SSE from cursor=1 replays ONLY Seq 2 (id: 2), then the tail blocks until we
	// cancel. The replay runs before the tail loop, so it's written deterministically.
	r := httptest.NewRequest("GET", "/activity/events?cursor=1", nil)
	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.handleActivitySSE(rec, r); close(done) }()
	cancel()
	<-done

	out := rec.Body.String()
	if !strings.Contains(out, "id: 2") {
		t.Errorf("SSE from cursor=1 should replay Seq 2 with id: 2; got:\n%s", out)
	}
	if strings.Contains(out, "id: 1") {
		t.Errorf("SSE from cursor=1 must NOT replay Seq 1 (already-seen); got:\n%s", out)
	}
	if !strings.Contains(out, "boom-marker") {
		t.Errorf("replayed row should carry the event content; got:\n%s", out)
	}
}

// Last-Event-ID header wins over ?cursor= (gap-free resume on reconnect).
func TestActivity_LastEventIDResumes(t *testing.T) {
	r := httptest.NewRequest("GET", "/activity/events?cursor=0", nil)
	r.Header.Set("Last-Event-ID", "7")
	if got := activityStartCursor(r); got != 7 {
		t.Fatalf("Last-Event-ID should win over ?cursor; got %d, want 7", got)
	}
	r2 := httptest.NewRequest("GET", "/activity/events?cursor=3", nil)
	if got := activityStartCursor(r2); got != 3 {
		t.Fatalf("?cursor should be used when no Last-Event-ID; got %d, want 3", got)
	}
}

// Nil event log → page renders "unavailable", stream is a no-op (no panic).
func TestActivity_NilLogGraceful(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil) // no SetEventLog
	if body := get(t, s, "/activity").Body.String(); !strings.Contains(body, "unavailable") {
		t.Errorf("nil evlog should render an unavailable notice; got:\n%s", body)
	}
	r := httptest.NewRequest("GET", "/activity/events", nil)
	rec := httptest.NewRecorder()
	s.handleActivitySSE(rec, r) // must return immediately, no panic/block
}
