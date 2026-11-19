package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gobwas/ws"

	"tenant/internal/mcp"
	"tenant/internal/mcp/transport"
)

// The link is two mcp.Sessions over this transport, each calling the other.
// These tests run that, over an in-memory pipe, a loopback TCP socket and TLS.

// eachLink runs fn over every kind of connection, with both ends upgraded.
func eachLink(t *testing.T, cfg transport.WSConfig, fn func(t *testing.T, dialing, accepting *transport.WS)) {
	t.Run("pipe", func(t *testing.T) {
		a, b := net.Pipe()
		dialing, accepting := transport.NewWSClient(a, cfg), transport.NewWSServer(b, cfg)
		t.Cleanup(func() {
			_, _ = a.Close(), b.Close() // first, so that neither goodbye waits on a reader
			_, _ = dialing.Close(), accepting.Close()
		})
		fn(t, dialing, accepting)
	})
	t.Run("tcp", func(t *testing.T) {
		dialing, accepting := newWSServer(t, cfg, false).link(t, cfg, nil)
		fn(t, dialing, accepting)
	})
	t.Run("tls", func(t *testing.T) {
		dialing, accepting := newWSServer(t, cfg, true).link(t, cfg, nil)
		fn(t, dialing, accepting)
	})
}

// side is one end's handler. "echo" returns its params with the end's name,
// "wait" blocks until the caller cancels, "big" returns a result of the size asked for.
type side struct {
	name      string
	waiting   chan struct{} // a "wait" request has arrived
	cancelled chan struct{} // and has been cancelled
}

func newSide(name string) *side {
	return &side{name: name, waiting: make(chan struct{}, 8), cancelled: make(chan struct{}, 8)}
}

type echoed struct {
	From string `json:"from"`
	Data string `json:"data"`
}

func (s *side) HandleRequest(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	switch method {
	case "echo":
		var data string
		if err := json.Unmarshal(params, &data); err != nil {
			return nil, err
		}
		return json.Marshal(echoed{From: s.name, Data: data})
	case "wait":
		s.waiting <- struct{}{}
		<-ctx.Done()
		s.cancelled <- struct{}{}
		return nil, ctx.Err()
	case "big":
		var n int
		_ = json.Unmarshal(params, &n)
		return json.Marshal(string(bytes.Repeat([]byte("r"), n)))
	}
	return nil, mcp.NewError(mcp.ErrMethodNotFound, method)
}

func (s *side) HandleNotification(context.Context, string, json.RawMessage) {}

// sessions starts a session on each end and closes both when the test ends.
func sessions(t *testing.T, dialing, accepting transport.Transport) (child, master *mcp.Session, childSide, masterSide *side) {
	t.Helper()
	childSide, masterSide = newSide("child"), newSide("master")
	child, master = mcp.NewSession(dialing, childSide), mcp.NewSession(accepting, masterSide)
	child.Start(bg)
	master.Start(bg)
	t.Cleanup(func() {
		_ = child.Close()
		_ = master.Close()
	})
	return child, master, childSide, masterSide
}

// echo calls "echo" on the far end of from and checks who answered with what.
func echo(ctx context.Context, from *mcp.Session, farEnd, data string) error {
	got, err := mcp.CallTyped[echoed](ctx, from, "echo", data)
	if err != nil {
		return err
	}
	if got.From != farEnd || got.Data != data {
		return fmt.Errorf("echo of %d bytes came back from %q with %d bytes", len(data), got.From, len(got.Data))
	}
	return nil
}

func TestWS_Sessions(t *testing.T) {
	eachLink(t, transport.WSConfig{}, func(t *testing.T, dialing, accepting *transport.WS) {
		child, master, childSide, _ := sessions(t, dialing, accepting)
		ctx, cancel := context.WithTimeout(bg, 30*time.Second)
		defer cancel()

		// A request from each end, answered by the other.
		if err := echo(ctx, master, "child", "to the child"); err != nil {
			t.Fatalf("master calls child: %v", err)
		}
		if err := echo(ctx, child, "master", "to the master"); err != nil {
			t.Fatalf("child calls master: %v", err)
		}

		// A message of a megabyte, each way: many writes, one frame.
		large := string(bytes.Repeat([]byte("L"), 1<<20))
		if err := echo(ctx, master, "child", large); err != nil {
			t.Fatalf("a megabyte to the child and back: %v", err)
		}
		if err := echo(ctx, child, "master", large); err != nil {
			t.Fatalf("a megabyte to the master and back: %v", err)
		}

		// Many calls at once from both ends, small and large mixed.
		var wg sync.WaitGroup
		for i := 0; i < 48; i++ {
			from, farEnd := master, "child"
			if i%2 == 1 {
				from, farEnd = child, "master"
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 12; j++ {
					data := string(bytes.Repeat([]byte{byte('a' + i%26)}, (i*j*977)%90000))
					if err := echo(ctx, from, farEnd, data); err != nil {
						t.Errorf("caller %d, call %d: %v", i, j, err)
						return
					}
				}
			}()
		}
		wg.Wait()

		// A call the caller gives up on: the far end's handler is told, and the
		// link carries on.
		callCtx, giveUp := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			_, err := master.Call(callCtx, "wait", nil)
			done <- err
		}()
		await(t, childSide.waiting, "the child's handler to start")
		giveUp()
		if err := await(t, done, "the cancelled call"); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled call = %v, want context.Canceled", err)
		}
		await(t, childSide.cancelled, "the child's handler to be cancelled")
		if err := echo(ctx, master, "child", "after a cancel"); err != nil {
			t.Fatalf("call after a cancelled call: %v", err)
		}

		// An ordinary goodbye from one end is a clean end for the other.
		_ = child.Close()
		await(t, master.Done(), "the master's session to end")
		if err := master.Err(); !errors.Is(err, mcp.ErrSessionClosed) {
			t.Errorf("master's session ended with %v, want ErrSessionClosed", err)
		}
	})
}

// A result too large for the link is refused by Send on the way out and the
// link stays up for everything else. The session does not report that refusal
// to anyone (see session.go, handleRequest), so the caller's own deadline is
// what ends the call: whoever serves a link must keep results under the limit.
func TestWS_SessionOversizeResultLeavesTheLinkUp(t *testing.T) {
	const max = 64 << 10
	eachLink(t, transport.WSConfig{MaxFrameSize: max}, func(t *testing.T, dialing, accepting *transport.WS) {
		_, master, _, _ := sessions(t, dialing, accepting)
		short, cancel := context.WithTimeout(bg, 300*time.Millisecond)
		defer cancel()
		_, err := master.Call(short, "big", json.RawMessage(fmt.Sprint(max)))
		t.Logf("a call whose result is over the limit ended with: %v", err)
		if err == nil {
			t.Fatal("a result over the limit was delivered")
		}
		if err := echo(bg, master, "child", "still here"); err != nil {
			t.Fatalf("the link did not survive an oversize result: %v", err)
		}
	})
}

// When the peer dies without a word, the session ends within two intervals
// and a call in flight returns then, with the reason.
func TestWS_SessionSeesADeadPeer(t *testing.T) {
	const interval = 150 * time.Millisecond
	tr, far := rawPipe(t, false, transport.WSConfig{PingInterval: interval})
	p := newPeer(t, far, false) // reads what it is sent, answers nothing
	sess := mcp.NewSession(tr, nil)
	start := time.Now()
	sess.Start(bg)
	defer sess.Close()

	_, err := sess.Call(bg, "tools/call", json.RawMessage(`{}`))
	took := time.Since(start)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("call to a dead peer = %v, want the transport's deadline error", err)
	}
	near(t, "the call to a dead peer", took, 2*interval-interval/10, 2*interval)
	if !errors.Is(sess.Err(), os.ErrDeadlineExceeded) {
		t.Errorf("session ended with %v, want the transport's deadline error", sess.Err())
	}
	// The request did go out before the silence was noticed.
	if f := p.next(); f.Header.OpCode != ws.OpBinary {
		t.Errorf("first frame to the peer: opcode %v, want the request", f.Header.OpCode)
	}
}

// Links opened, used and ended every way they can end leave nothing behind:
// no goroutine and no open connection.
func TestWS_NoLeakAfterClose(t *testing.T) {
	before := runtime.NumGoroutine()
	var open atomic.Int32
	cfg := transport.WSConfig{PingInterval: 50 * time.Millisecond}
	s := newWSServer(t, cfg, false)

	use := func(dialing, accepting *transport.WS) (child, master *mcp.Session) {
		child, master = mcp.NewSession(dialing, newSide("child")), mcp.NewSession(accepting, newSide("master"))
		child.Start(bg)
		master.Start(bg)
		if err := echo(bg, master, "child", "x"); err != nil {
			t.Fatalf("echo: %v", err)
		}
		if err := echo(bg, child, "master", "y"); err != nil {
			t.Fatalf("echo: %v", err)
		}
		return child, master
	}
	for i := 0; i < 10; i++ {
		// Over a pipe, closed from one end and then the other.
		a, b := net.Pipe()
		child, master := use(transport.NewWSClient(track(a, &open), transport.WSConfig{}), transport.NewWSServer(track(b, &open), transport.WSConfig{}))
		_ = []*mcp.Session{child, master}[i%2].Close()
		<-child.Done()
		<-master.Done()

		// Over TCP, the same.
		dialing, accepting := s.link(t, cfg, &open)
		child, master = use(dialing, accepting)
		_ = []*mcp.Session{child, master}[i%2].Close()
		<-child.Done()
		<-master.Done()

		// A peer that dies silently: the session ends itself.
		a, b = net.Pipe()
		dead := mcp.NewSession(transport.NewWSClient(track(a, &open), cfg), nil)
		dead.Start(bg)
		go func() { _, _ = io.Copy(io.Discard, b) }() // drain until the transport hangs up
		<-dead.Done()
		_ = b.Close()

		// A peer that sends something forbidden.
		a, b = net.Pipe()
		refusing := mcp.NewSession(transport.NewWSServer(track(a, &open), cfg), nil)
		refusing.Start(bg)
		go func() { _, _ = io.Copy(io.Discard, b) }()
		_, _ = b.Write(encode(ws.NewTextFrame([]byte("no")), true))
		<-refusing.Done()
		_ = b.Close()
	}
	s.Close()

	waitClosed(t, &open, "client and pipe connections")
	waitClosed(t, &s.open, "server connections")
	deadline := time.Now().Add(waitLong)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("%d goroutines before, %d after:\n%s", before, runtime.NumGoroutine(), buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
