package peering

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tenant/internal/logs/event"
)

// captured keeps the events written while it is installed.
type captured struct {
	mu     sync.Mutex
	events []*event.Event
}

func (c *captured) Emit(_ context.Context, e *event.Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}
func (c *captured) Audit(ctx context.Context, e *event.Event) error { c.Emit(ctx, e); return nil }

func capture(t *testing.T) *captured {
	c := &captured{}
	event.Install(c)
	t.Cleanup(func() { event.Install(nil) })
	return c
}

// find returns the events of d with opcode op, waiting a moment for ones
// written off the test's goroutine.
func (c *captured) find(d *event.Def, op string) []*event.Event {
	id := d.Spec().ID
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var out []*event.Event
		c.mu.Lock()
		for _, e := range c.events {
			if e.Provider == "peering" && e.ID == id && e.Opcode == op {
				out = append(out, e)
			}
		}
		c.mu.Unlock()
		if len(out) > 0 || time.Now().After(deadline) {
			return out
		}
	}
}

func field(e *event.Event, key string) string {
	for _, f := range e.Data {
		if f.Key == key {
			return f.Text()
		}
	}
	return ""
}

// A peer's session and each tool it calls are activities, a failed call
// fails its activity, and a request with a bad token is logged.
func TestListenerLogsSessionsCallsAndRejections(t *testing.T) {
	c := capture(t)
	srv, token, _ := pairedListener(t, func(s *mcp.Server, _ PeerContext) {
		mcp.AddTool(s, &mcp.Tool{Name: "broken"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return nil, nil, errors.New("index unavailable")
		})
	})
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if bad, err := dialClient(t, ctx, srv.URL, "nope"); err == nil {
		bad.Close()
		t.Fatal("a bad token connected")
	}
	if got := c.find(TokenRejected, ""); len(got) == 0 || field(got[0], "remote") == "" {
		t.Errorf("the rejected token was not logged with its remote address: %v", got)
	}

	sess, err := dialClient(t, ctx, srv.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "peer_hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "broken"}); err != nil {
		t.Fatal(err)
	}
	sess.Close()

	if got := c.find(Connection, event.OpStart); len(got) != 1 || field(got[0], "peer") != "spoke" {
		t.Errorf("session start: %v", got)
	}
	if got := c.find(Connection, event.OpStop); len(got) != 1 {
		t.Errorf("session stop: %v", got)
	}
	if got := c.find(RPC, event.OpStop); len(got) != 1 || field(got[0], "tool") != "peer_hello" || field(got[0], "duration") == "" {
		t.Errorf("peer_hello's stop: %v", got)
	}
	fail := c.find(RPC, event.OpFail)
	if len(fail) != 1 || field(fail[0], "tool") != "broken" || !strings.Contains(field(fail[0], "error"), "index unavailable") || fail[0].Level != event.Error {
		t.Errorf("the failed call: %v", fail)
	}
}

// The listener's lifetime is an activity: start when it serves, stop when
// it shuts down.
func TestServeIsAnActivity(t *testing.T) {
	c := capture(t)
	store, _ := LoadStore(t.TempDir())
	l, err := NewListener(ListenerConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := l.Bind("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- l.Serve(ctx, ln) }()
	if got := c.find(Listening, event.OpStart); len(got) != 1 || field(got[0], "addr") != ln.Addr().String() {
		t.Fatalf("start: %v", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := c.find(Listening, event.OpStop); len(got) != 1 {
		t.Errorf("stop: %v", got)
	}
}

// Both sides of a pairing are logged, and the PIN never is.
func TestPairingIsLogged(t *testing.T) {
	c := capture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, approve := range []bool{true, false} {
		srv := httptest.NewServer(pairListener(t, t.TempDir(), approve, nil).Handler())
		_, _ = RequestPair(ctx, srv.URL, PairRequest{Name: "laptop", InstanceID: "laptop-id", PIN: "418207"}, true)
		srv.Close()
	}
	if got := c.find(Paired, ""); len(got) != 1 || field(got[0], "peer") != "laptop" {
		t.Errorf("paired: %v", got)
	}
	if got := c.find(PairDenied, ""); len(got) != 1 {
		t.Errorf("denied: %v", got)
	}
	if got := c.find(PairInvite, event.OpStop); len(got) != 1 {
		t.Errorf("the approved request's stop: %v", got)
	}
	if got := c.find(PairInvite, event.OpFail); len(got) != 1 || !strings.Contains(field(got[0], "error"), "DENIED") {
		t.Errorf("the denied request's fail: %v", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		for _, f := range e.Data {
			if strings.Contains(f.Text(), "418207") || strings.Contains(f.Text(), "418 207") {
				t.Errorf("the PIN reached %s/%d: %s=%s", e.Provider, e.ID, f.Key, f.Text())
			}
		}
	}
}
