package dashboard

// Regression guard for the live dashboard update path. The TEN-110 QA fix noted
// the original Datastar break was "invisible to the (server-only) tests" because
// nothing asserted an actual agent event streams out of GET /events as a patch.
// TestSSR_Events_ReturnsOnCancel only covers shutdown. This closes that gap:
// publish a real Event to the broker the dashboard subscribes to, and assert the
// handler emits a datastar-patch-elements append to #chat-log.

import (
	"bufio"
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
			strings.Contains(out, "selector #chat-log") &&
			strings.Contains(out, "mode append") {
			streamed = true
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no #chat-log patch streamed after publishing events:\n%q", out)
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

// openEvents runs GET path on s's /events handler until the test ends, and
// returns what it has streamed so far.
func openEvents(t *testing.T, s *Server, path string) *syncResponse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rec := &syncResponse{}
	done := make(chan struct{})
	go func() {
		s.handleEventsSSE(rec, httptest.NewRequest("GET", path, nil).WithContext(ctx))
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return rec
}

// streamed waits until the stream has sent something ok accepts, calling
// poke (nil = nothing) while it waits, and returns the stream so far.
func streamed(t *testing.T, rec *syncResponse, what string, ok func(string) bool, poke func()) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if poke != nil {
			poke()
		}
		if out := rec.String(); ok(out) {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stream never sent %s:\n%s", what, rec.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The approval queue rides /events (TEN-207): a snapshot as soon as the
// stream opens, then the whole queue again from Pending() on each approval
// event, so a new request appears and an answered one goes.
func TestEvents_ApprovalQueueLive(t *testing.T) {
	broker := agent.NewBroker(0)
	fq := &fakeQueue{}
	s := New(Config{}, nil, nil, nil, broker, nil)
	s.SetApprovals(fq)
	rec := openEvents(t, s, "/events")
	streamed(t, rec, "a snapshot", func(out string) bool {
		return strings.HasPrefix(out, "event: datastar-patch-elements\ndata: elements <div id=\"approval-queue\">") &&
			strings.Contains(out, "Nothing is waiting for you.")
	}, nil)

	// One event each: the snapshot came after Subscribe, so none is missed,
	// and no Pending left queued can stand in for the Resolved one.
	fq.add(PendingApproval{ID: "ap-7", Category: "exec", Action: "os_exec", Detail: "uname -a", Origin: "local"})
	broker.Publish(agent.Event{Kind: agent.EventApprovalPending})
	streamed(t, rec, "the new request", func(out string) bool { return strings.Contains(out, `id="q-ap-7"`) }, nil)

	if err := fq.Decide("ap-7", "deny"); err != nil {
		t.Fatal(err)
	}
	at := len(rec.String())
	broker.Publish(agent.Event{Kind: agent.EventApprovalResolved})
	streamed(t, rec, "the empty queue", func(out string) bool { return strings.Contains(out[at:], "Nothing is waiting for you.") }, nil)
}

// With no event at all, the queue still catches up every queueResync: the
// event broker drops events for a slow subscriber.
func TestEvents_ApprovalQueueResyncs(t *testing.T) {
	old := queueResync
	queueResync = 30 * time.Millisecond
	t.Cleanup(func() { queueResync = old })
	fq := &fakeQueue{}
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetApprovals(fq)
	rec := openEvents(t, s, "/events?only=approvals")
	streamed(t, rec, "a snapshot", func(out string) bool { return strings.Contains(out, `id="approval-queue"`) }, nil)
	fq.add(PendingApproval{ID: "ap-9", Category: "exec", Action: "os_exec", Origin: "local"})
	streamed(t, rec, "the request, unannounced", func(out string) bool { return strings.Contains(out, `id="q-ap-9"`) }, nil)
}

// ?only=approvals: the Safety page takes the queue and nothing else.
func TestEvents_OnlyApprovals(t *testing.T) {
	broker := agent.NewBroker(0)
	fq := &fakeQueue{}
	s := New(Config{}, nil, nil, nil, broker, nil)
	s.SetApprovals(fq)
	only := openEvents(t, s, "/events?only=approvals")
	all := openEvents(t, s, "/events")
	fq.add(PendingApproval{ID: "ap-3", Category: "exec", Action: "os_exec", Origin: "local"})
	poke := func() {
		broker.Publish(agent.Event{Kind: agent.EventToolCall, Tool: "web_search", Args: "{}"})
		broker.Publish(agent.Event{Kind: agent.EventApprovalPending})
	}
	streamed(t, all, "the chat and the queue", func(out string) bool {
		return strings.Contains(out, "selector #chat-log") && strings.Contains(out, `id="q-ap-3"`)
	}, poke)
	out := streamed(t, only, "the queue", func(out string) bool { return strings.Contains(out, `id="q-ap-3"`) }, poke)
	if strings.Contains(out, "#chat-log") {
		t.Errorf("?only=approvals streamed the chat:\n%s", out)
	}
}

// The snapshot is now the stream's first write, which the write-timeout
// guard in server_limits_test.go reads as its first line: this one reads
// past it, so an event written after the finite-response deadline must
// still arrive.
func TestEvents_OutlivesWriteTimeoutPastSnapshot(t *testing.T) {
	broker := agent.NewBroker(0)
	s := New(Config{}, nil, nil, nil, broker, nil)
	const timeout = 40 * time.Millisecond
	hs := httptest.NewServer(s.handlerWithWriteTimeout(timeout))
	defer hs.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, hs.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	time.Sleep(3 * timeout)
	broker.Publish(agent.Event{Kind: agent.EventToolCall, Tool: "web_search", Args: "{}"})
	got := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "selector #chat-log") {
				got <- true
				return
			}
		}
		got <- false
	}()
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("the stream closed before the event after the deadline")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event after the write deadline")
	}
}

// A model call in flight shows as a thinking row the stream keeps counting
// up; its end removes the row and says how long it took, and the turn's end
// splits its time (TEN-342). Another agent's calls don't show.
func TestEvents_ThinkingRowThenHowLong(t *testing.T) {
	old := thinkingEvery
	thinkingEvery = 20 * time.Millisecond
	t.Cleanup(func() { thinkingEvery = old })
	broker := agent.NewBroker(0)
	s := New(Config{}, nil, nil, nil, broker, nil)
	rec := openEvents(t, s, "/events")
	row := `id="chat-thinking"`
	streamed(t, rec, "the thinking row", func(out string) bool { return strings.Contains(out, row) }, func() {
		if !strings.Contains(rec.String(), row) {
			broker.Publish(agent.Event{Kind: agent.EventThinking, Iter: 1})
		}
	})
	streamed(t, rec, "a tick", func(out string) bool { return strings.Count(out, row) >= 3 }, nil)

	at := len(rec.String())
	broker.Publish(agent.Event{Kind: agent.EventThought, Iter: 1, Duration: 1500 * time.Millisecond})
	out := streamed(t, rec, "the call's end", func(out string) bool { return strings.Contains(out[at:], "thought for 1.5s") }, nil)
	if end := out[at:]; !strings.Contains(end, "mode remove\ndata: selector #chat-thinking\n") {
		t.Fatalf("the thinking row was not removed:\n%s", end)
	}
	// A tick may land before the call's end is read; none may after.
	after := len(out)
	time.Sleep(3 * thinkingEvery)
	if n := strings.Count(rec.String()[after:], row); n != 0 {
		t.Fatalf("the row kept ticking after the call ended (%d patches)", n)
	}

	at = len(rec.String())
	broker.Publish(agent.Event{Kind: agent.EventTurnEnd, Duration: 3 * time.Second, Think: 2 * time.Second, Tools: time.Second})
	streamed(t, rec, "the turn's split", func(out string) bool { return strings.Contains(out[at:], "turn 3s — thinking 2s · tools 1s") }, nil)

	at = len(rec.String())
	broker.Publish(agent.Event{Kind: agent.EventThinking, Agent: "writer"})
	broker.Publish(agent.Event{Kind: agent.EventToolCall, Tool: "marker", Args: "{}"})
	out = streamed(t, rec, "the marker", func(out string) bool { return strings.Contains(out[at:], "marker") }, nil)
	if strings.Contains(out[at:], row) {
		t.Fatalf("a sub-agent's call showed as the chat's:\n%s", out[at:])
	}
}

func TestThinkingRow(t *testing.T) {
	if got := thinkingRow(0); !strings.Contains(got, ">thinking…<") {
		t.Errorf("row at 0 = %s", got)
	}
	if got := thinkingRow(75 * time.Second); !strings.Contains(got, ">thinking… 1m 15s<") {
		t.Errorf("row at 75s = %s", got)
	}
}
