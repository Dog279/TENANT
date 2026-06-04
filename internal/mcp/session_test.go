package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"tenant/internal/mcp"
	"tenant/internal/mcp/transport"
)

// pipePair wires two Stdio transports back-to-back via in-memory
// pipes. What one sends, the other receives. Round-trip tests
// without spawning subprocesses.
func pipePair(t *testing.T) (a, b transport.Transport) {
	t.Helper()
	aRead, bWrite := io.Pipe()
	bRead, aWrite := io.Pipe()
	a = transport.NewStdioStreams(aRead, aWrite)
	b = transport.NewStdioStreams(bRead, bWrite)
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})
	return a, b
}

// echoHandler returns params unchanged.
type echoHandler struct{}

func (echoHandler) HandleRequest(_ context.Context, _ string, params json.RawMessage) (json.RawMessage, error) {
	return params, nil
}
func (echoHandler) HandleNotification(_ context.Context, _ string, _ json.RawMessage) {}

func TestSession_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	clientT, serverT := pipePair(t)

	server := mcp.NewSession(serverT, echoHandler{})
	server.Start(ctx)
	defer server.Close()

	client := mcp.NewSession(clientT, nil)
	client.Start(ctx)
	defer client.Close()

	type payload struct {
		Hello string `json:"hello"`
	}
	got, err := mcp.CallTyped[payload](ctx, client, "echo", payload{Hello: "world"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Hello != "world" {
		t.Fatalf("round-trip mismatch: got %+v", got)
	}
}

func TestSession_MethodNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	clientT, serverT := pipePair(t)

	// nil handler => nopHandler => method-not-found for any request.
	server := mcp.NewSession(serverT, nil)
	server.Start(ctx)
	defer server.Close()

	client := mcp.NewSession(clientT, nil)
	client.Start(ctx)
	defer client.Close()

	_, err := client.Call(ctx, "no.such.method", nil)
	var rpcErr *mcp.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *mcp.Error, got %T: %v", err, err)
	}
	if rpcErr.Code != mcp.ErrMethodNotFound {
		t.Fatalf("expected ErrMethodNotFound (%d), got %d", mcp.ErrMethodNotFound, rpcErr.Code)
	}
}

func TestSession_NotificationDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	clientT, serverT := pipePair(t)

	delivered := make(chan struct{}, 1)
	server := mcp.NewSession(serverT, &notifyHandler{ch: delivered})
	server.Start(ctx)
	defer server.Close()

	client := mcp.NewSession(clientT, nil)
	client.Start(ctx)
	defer client.Close()

	if err := client.Notify(ctx, "ping", nil); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	select {
	case <-delivered:
	case <-ctx.Done():
		t.Fatal("notification not delivered before timeout")
	}
}

type notifyHandler struct{ ch chan struct{} }

func (h *notifyHandler) HandleRequest(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (h *notifyHandler) HandleNotification(_ context.Context, _ string, _ json.RawMessage) {
	select {
	case h.ch <- struct{}{}:
	default:
	}
}

// cancelHandler blocks until its per-request ctx is cancelled, then
// signals — proving the full bidirectional cancel path (client sends
// notifications/cancelled → server cancels the in-flight handler).
type cancelHandler struct {
	started   chan struct{}
	cancelled chan struct{}
}

func (h *cancelHandler) HandleRequest(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	h.started <- struct{}{}
	<-ctx.Done()
	close(h.cancelled)
	return nil, ctx.Err()
}
func (h *cancelHandler) HandleNotification(context.Context, string, json.RawMessage) {}

func TestSession_CancelPropagatesToHandler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	clientT, serverT := pipePair(t)

	h := &cancelHandler{started: make(chan struct{}, 1), cancelled: make(chan struct{})}
	server := mcp.NewSession(serverT, h)
	server.Start(ctx)
	defer server.Close()
	client := mcp.NewSession(clientT, nil)
	client.Start(ctx)
	defer client.Close()

	callCtx, callCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := client.Call(callCtx, "slow", nil)
		done <- err
	}()

	select {
	case <-h.started:
	case <-ctx.Done():
		t.Fatal("handler never started")
	}
	callCancel() // client ctx done → should emit notifications/cancelled

	select {
	case <-h.cancelled: // server cancelled the in-flight handler ctx
	case <-ctx.Done():
		t.Fatal("server handler was not cancelled by notifications/cancelled")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Call should return context.Canceled, got %v", err)
	}
}

// progressHandler reports two progress updates against the request's
// token, then returns a result.
type progressHandler struct{ sess *mcp.Session }

func (h *progressHandler) HandleRequest(ctx context.Context, _ string, params json.RawMessage) (json.RawMessage, error) {
	if tok, ok := mcp.ProgressTokenFrom(params); ok {
		_ = h.sess.SendProgress(ctx, tok, 0.5, nil, "halfway")
		_ = h.sess.SendProgress(ctx, tok, 1.0, nil, "done")
	}
	return json.RawMessage(`{"ok":true}`), nil
}
func (h *progressHandler) HandleNotification(context.Context, string, json.RawMessage) {}

func TestSession_ProgressStreaming(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	clientT, serverT := pipePair(t)

	h := &progressHandler{}
	server := mcp.NewSession(serverT, h)
	h.sess = server // handler reports progress via the server session
	server.Start(ctx)
	defer server.Close()
	client := mcp.NewSession(clientT, nil)
	client.Start(ctx)
	defer client.Close()

	var mu sync.Mutex
	var got []mcp.ProgressParams
	res, err := client.CallWithProgress(ctx, "work", json.RawMessage(`{"x":1}`), func(p mcp.ProgressParams) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("CallWithProgress: %v", err)
	}
	if string(res) != `{"ok":true}` {
		t.Fatalf("result wrong: %s", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("expected 2 progress updates, got %d: %+v", len(got), got)
	}
	if got[0].Message != "halfway" || got[1].Message != "done" || got[1].Progress != 1.0 {
		t.Errorf("progress payloads wrong: %+v", got)
	}
}
