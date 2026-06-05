package dashboard

// Regression guard for the live dashboard update path. The TEN-110 QA fix noted
// the original Datastar break was "invisible to the (server-only) tests" because
// nothing asserted an actual agent event streams out of GET /events as a patch.
// TestSSR_Events_ReturnsOnCancel only covers shutdown. This closes that gap:
// publish a real Event to the broker the dashboard subscribes to, and assert the
// handler emits a datastar-patch-elements append to #activity-feed.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
)

// syncResponse is a concurrency-safe ResponseWriter+Flusher so the test can read
// the streamed bytes while the handler goroutine is still writing them (a plain
// httptest.ResponseRecorder would race).
type syncResponse struct {
	mu  sync.Mutex
	buf bytes.Buffer
	hdr http.Header
}

func (s *syncResponse) Header() http.Header {
	if s.hdr == nil {
		s.hdr = http.Header{}
	}
	return s.hdr
}
func (s *syncResponse) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncResponse) WriteHeader(int) {}
func (s *syncResponse) Flush()          {}
func (s *syncResponse) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestSSR_Events_StreamsPublishedEvent(t *testing.T) {
	broker := agent.NewBroker(0)
	s := New(Config{}, nil, &fakeTools{}, nil, broker, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &syncResponse{}
	done := make(chan struct{})
	go func() {
		s.handleEventsSSE(rec, httptest.NewRequest("GET", "/events", nil).WithContext(ctx))
		close(done)
	}()

	// The handler subscribes on entry; publish in a short loop so we don't race
	// the subscription, and so a single buffered delivery is enough to pass.
	deadline := time.After(2 * time.Second)
	for streamed := false; !streamed; {
		broker.Publish(agent.Event{Kind: agent.EventToolCall, Tool: "web_search", Args: "{}"})
		out := rec.String()
		if strings.Contains(out, "event: datastar-patch-elements") &&
			strings.Contains(out, "selector #activity-feed") &&
			strings.Contains(out, "mode append") {
			streamed = true
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no #activity-feed patch streamed after publishing events:\n%q", out)
		case <-time.After(20 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after cancel")
	}
}
