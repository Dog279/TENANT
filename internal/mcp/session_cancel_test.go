package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/mcp/transport"
)

// A peer cancels a request with notifications/cancelled, naming its id. The
// read loop handles that notification itself, and a request in a goroutine
// of its own: so whether a cancel finds its request is a question of what
// the read loop has done by then, and these tests put the two frames as
// close together, and in as many orders, as a peer can.

// scripted is a transport that receives the frames a test gives it, in
// order, and counts what is sent.
type scripted struct {
	in   chan []byte
	sent atomic.Int64
	done chan struct{}
	once sync.Once
}

func newScripted(frames ...string) *scripted {
	t := &scripted{in: make(chan []byte, 64), done: make(chan struct{})}
	t.feed(frames...)
	return t
}

func (t *scripted) feed(frames ...string) {
	for _, f := range frames {
		t.in <- []byte(f)
	}
}

func (t *scripted) Send(context.Context, []byte) error {
	t.sent.Add(1)
	return nil
}

func (t *scripted) Recv(ctx context.Context) ([]byte, error) {
	select {
	case f := <-t.in: // what is queued is read, whatever else is true
		return f, nil
	default:
	}
	select {
	case f := <-t.in:
		return f, nil
	case <-t.done:
		return nil, transport.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *scripted) Close() error {
	t.once.Do(func() { close(t.done) })
	return nil
}

// probe is a handler. "wait" runs until its context ends; "hold" until the
// test lets the request of that name go, or its context ends; "mark" says
// that the read loop has got as far as it, and so has dealt with every frame
// before it; anything else returns at once.
type probe struct {
	started, cancelled atomic.Int64
	marked             chan struct{}

	mu    sync.Mutex
	holds map[string]chan struct{}
	began map[string]chan struct{}
}

func newProbe() *probe {
	return &probe{marked: make(chan struct{}, 8), holds: map[string]chan struct{}{}, began: map[string]chan struct{}{}}
}

// gate is the channel of the named "hold" request in one of the two maps.
func (h *probe) gate(m map[string]chan struct{}, name string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m[name] == nil {
		m[name] = make(chan struct{})
	}
	return m[name]
}

func (h *probe) HandleRequest(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	h.started.Add(1)
	switch method {
	case "wait":
		<-ctx.Done()
		h.cancelled.Add(1)
		return nil, ctx.Err()
	case "hold":
		var name string
		_ = json.Unmarshal(params, &name)
		close(h.gate(h.began, name))
		select {
		case <-h.gate(h.holds, name):
		case <-ctx.Done():
			h.cancelled.Add(1)
			return nil, ctx.Err()
		}
	case "mark":
		h.marked <- struct{}{}
	}
	return json.RawMessage(`{}`), nil
}

func (*probe) HandleNotification(context.Context, string, json.RawMessage) {}

const (
	waitReq   = `{"jsonrpc":"2.0","id":1,"method":"wait"}`
	quickReq  = `{"jsonrpc":"2.0","id":1,"method":"quick"}`
	cancelOne = `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`
	markReq   = `{"jsonrpc":"2.0","id":"mark","method":"mark"}`
)

// held is how many requests the session has registered and not finished.
func held(s *Session) int {
	n := 0
	s.inflight.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// run starts a session over the frames and returns it once the read loop
// has dealt with every frame before the mark, which must be the last.
func run(t *testing.T, h *probe, frames ...string) (*Session, *scripted) {
	t.Helper()
	tr := newScripted(frames...)
	s := NewSession(tr, h)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(func() {
		stop()
		_ = s.Close()
	})
	s.Start(ctx)
	awaitMark(t, h)
	return s, tr
}

// awaitBegan waits until the named "hold" request is running.
func awaitBegan(t *testing.T, h *probe, name string) {
	t.Helper()
	select {
	case <-h.gate(h.began, name):
	case <-time.After(10 * time.Second):
		t.Fatalf("the %s request never started", name)
	}
}

func awaitMark(t *testing.T, h *probe) {
	t.Helper()
	select {
	case <-h.marked:
	case <-time.After(10 * time.Second):
		t.Fatal("the read loop never got to the mark")
	}
}

// settled waits until the session holds no request. A request whose cancel
// was lost is held for good, and that is what this fails on.
func settled(t *testing.T, s *Session, what string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); held(s) != 0; runtime.Gosched() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d request(s) still held; a cancel was lost", what, held(s))
		}
	}
}

// The regression: a cancel read right behind the request it cancels. Before
// the request was registered on the read loop, the cancel was looked up
// before the request's goroutine had registered it, found nothing, and the
// request ran on with nobody waiting for it: 1,753 times in 2,000.
func TestSession_CancelRightBehindItsRequest(t *testing.T) {
	const tries = 4000
	before := runtime.NumGoroutine()
	ran := 0
	for i := range tries {
		h := newProbe()
		tr := newScripted(waitReq, cancelOne, markReq)
		s := NewSession(tr, h)
		ctx, stop := context.WithCancel(context.Background())
		s.Start(ctx)
		awaitMark(t, h)
		settled(t, s, fmt.Sprintf("try %d, with the cancel right behind the request", i))
		// It either never started or was cancelled; it did not finish, and
		// a cancelled request is not answered: the one frame sent is the
		// mark's answer.
		if started, cancelled := h.started.Load()-1, h.cancelled.Load(); started != cancelled || tr.sent.Load() != 1 {
			t.Fatalf("try %d: the request started %d times and was cancelled %d times; %d frames were sent", i, started, cancelled, tr.sent.Load())
		}
		ran += int(h.cancelled.Load())
		stop()
		_ = s.Close()
	}
	t.Logf("of %d cancels read right behind their request, none was lost: %d found the request not yet started, %d stopped it after it had", tries, tries-ran, ran)
	for deadline := time.Now().Add(10 * time.Second); runtime.NumGoroutine() > before; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines before, %d after", before, runtime.NumGoroutine())
		}
	}
}

// The other orders a cancel and its request can come in.
func TestSession_CancelOrderings(t *testing.T) {
	t.Run("the cancel first", func(t *testing.T) {
		// A cancel names a request the peer has sent. One that names an id
		// not seen yet cancels nothing, and is not kept for a request that
		// comes later under that id.
		h := newProbe()
		s, tr := run(t, h, cancelOne, quickReq, markReq)
		settled(t, s, "a request that came after a cancel for its id")
		if h.started.Load() != 2 || h.cancelled.Load() != 0 || tr.sent.Load() != 2 {
			t.Errorf("%d requests started, %d cancelled, %d answered; want both run and answered", h.started.Load(), h.cancelled.Load(), tr.sent.Load())
		}
	})
	t.Run("a cancel for a request that never comes", func(t *testing.T) {
		h := newProbe()
		s, tr := run(t, h, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":99}}`,
			`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"never"}}`,
			`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`,
			`{"jsonrpc":"2.0","method":"notifications/cancelled"}`, markReq)
		settled(t, s, "cancels for requests that never came")
		if _, kept := s.inflight.Load("99"); kept || tr.sent.Load() != 1 {
			t.Errorf("a cancel for no request left something behind (kept=%v), or was answered (%d frames sent)", kept, tr.sent.Load())
		}
	})
	t.Run("an id used again while its first request runs", func(t *testing.T) {
		// A peer's bug, or its right once the first is answered and this end
		// has not quite finished with it: either way the entry under the id
		// is the later request's, and the first one finishing must not take
		// it away.
		h := newProbe()
		s, tr := run(t, h, `{"jsonrpc":"2.0","id":1,"method":"hold","params":"first"}`, markReq)
		awaitBegan(t, h, "first")
		tr.feed(`{"jsonrpc":"2.0","id":1,"method":"hold","params":"second"}`, markReq)
		awaitMark(t, h)
		awaitBegan(t, h, "second")
		close(h.gate(h.holds, "first")) // the first finishes, and is answered
		for deadline := time.Now().Add(2 * time.Second); tr.sent.Load() < 3; runtime.Gosched() {
			if time.Now().After(deadline) {
				t.Fatalf("the first request was not answered: %d frames sent", tr.sent.Load())
			}
		}
		for deadline := time.Now().Add(2 * time.Second); held(s) != 1; runtime.Gosched() {
			if time.Now().After(deadline) {
				t.Fatalf("with the second request running, %d are held", held(s))
			}
		}
		tr.feed(cancelOne, markReq)
		awaitMark(t, h)
		settled(t, s, "a cancel for an id that was used twice")
		if h.cancelled.Load() != 1 {
			t.Errorf("the cancel stopped %d requests, want the second one", h.cancelled.Load())
		}
	})
	t.Run("the request finishes as the cancel arrives", func(t *testing.T) {
		// The answer and the cancel cross. The request is answered or it is
		// not, and nothing is left of it either way.
		before := runtime.NumGoroutine()
		for i := range 2000 {
			h := newProbe()
			tr := newScripted(quickReq, cancelOne, markReq)
			s := NewSession(tr, h)
			ctx, stop := context.WithCancel(context.Background())
			s.Start(ctx)
			awaitMark(t, h)
			settled(t, s, "a request that finished as its cancel arrived")
			if sent := tr.sent.Load(); sent < 1 || sent > 2 || h.started.Load() > 2 {
				t.Fatalf("try %d: %d frames sent for one request and the mark", i, sent)
			}
			stop()
			_ = s.Close()
		}
		for deadline := time.Now().Add(10 * time.Second); runtime.NumGoroutine() > before; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%d goroutines before, %d after", before, runtime.NumGoroutine())
			}
		}
	})
	t.Run("the session ends with a request registered and not yet started", func(t *testing.T) {
		before := runtime.NumGoroutine()
		for i := range 2000 {
			h := newProbe()
			tr := newScripted(waitReq, `{"jsonrpc":"2.0","id":2,"method":"wait"}`, `{"jsonrpc":"2.0","id":3,"method":"wait"}`)
			s := NewSession(tr, h)
			ctx, stop := context.WithCancel(context.Background())
			s.Start(ctx)
			if i%2 == 0 {
				stop() // the context the session was started with ends
			}
			_ = s.Close()
			stop()
			// The read loop has stopped, with any of the three registered;
			// each is let go, started or not, and none is answered.
			<-s.Done()
			settled(t, s, "requests registered when the session ended")
			if h.started.Load() != h.cancelled.Load() || tr.sent.Load() != 0 {
				t.Fatalf("try %d: %d requests started and %d were cancelled; %d frames sent", i, h.started.Load(), h.cancelled.Load(), tr.sent.Load())
			}
		}
		for deadline := time.Now().Add(10 * time.Second); runtime.NumGoroutine() > before; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%d goroutines before, %d after", before, runtime.NumGoroutine())
			}
		}
	})
}
