package transport_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tenant/internal/mcp/transport"
)

// TestSSE_RoundTrip drives the SSE transport through a real HTTP server:
// open the GET stream, read the endpoint event, POST an inbound frame
// (→ Recv), Send an outbound frame (→ stream `message` event), then drop
// the stream and confirm Recv reports io.EOF.
func TestSSE_RoundTrip(t *testing.T) {
	sse := transport.NewSSE(transport.SSEConfig{})
	hs := httptest.NewServer(sse)
	defer hs.Close()
	defer sse.Close()

	ctx := context.Background()

	// 1. Open the GET event stream.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	req, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, hs.URL+"/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	br := bufio.NewReader(resp.Body)

	// 2. First event must name the POST endpoint.
	ev := readEvent(t, br)
	if ev.event != "endpoint" || ev.data != "/message" {
		t.Fatalf("first event = %q/%q, want endpoint//message", ev.event, ev.data)
	}

	// 3. Client → server: POST a frame, expect it from Recv.
	go func() {
		preq, _ := http.NewRequest(http.MethodPost, hs.URL+"/message",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		pr, perr := http.DefaultClient.Do(preq)
		if perr == nil {
			pr.Body.Close()
		}
	}()
	got, err := sse.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if !strings.Contains(string(got), `"method":"ping"`) {
		t.Fatalf("Recv frame = %s", got)
	}

	// 4. Server → client: Send a frame, expect a `message` event.
	if err := sse.Send(ctx, []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev = readEvent(t, br)
	if ev.event != "message" || !strings.Contains(ev.data, `"result"`) {
		t.Fatalf("message event = %q/%q", ev.event, ev.data)
	}

	// 5. Drop the stream → Recv reports io.EOF (clean session end).
	cancelStream()
	resp.Body.Close()
	done := make(chan error, 1)
	go func() { _, e := sse.Recv(ctx); done <- e }()
	select {
	case e := <-done:
		if !errors.Is(e, io.EOF) {
			t.Fatalf("Recv after disconnect = %v, want io.EOF", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv did not return after stream drop")
	}
}

// TestSSE_SecondStreamRejected enforces the single-connection contract.
func TestSSE_SecondStreamRejected(t *testing.T) {
	sse := transport.NewSSE(transport.SSEConfig{})
	hs := httptest.NewServer(sse)
	defer hs.Close()
	defer sse.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, hs.URL+"/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("first GET: %v", err)
	}
	defer resp.Body.Close()
	// Read the endpoint event so we know the stream is established.
	readEvent(t, bufio.NewReader(resp.Body))

	resp2, err := http.Get(hs.URL + "/sse")
	if err != nil {
		t.Fatalf("second GET: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("second stream status = %d, want 409", resp2.StatusCode)
	}
}

// TestSSE_AfterClose rejects new streams once closed.
func TestSSE_AfterClose(t *testing.T) {
	sse := transport.NewSSE(transport.SSEConfig{})
	hs := httptest.NewServer(sse)
	defer hs.Close()
	sse.Close()

	resp, err := http.Get(hs.URL + "/sse")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status after Close = %d, want 410", resp.StatusCode)
	}
	if _, err := sse.Recv(context.Background()); !errors.Is(err, transport.ErrClosed) {
		t.Fatalf("Recv after Close = %v, want ErrClosed", err)
	}
}

type sseEvent struct{ event, data string }

// readEvent parses one SSE event (event:/data: lines, blank-line terminated).
func readEvent(t *testing.T, br *bufio.Reader) sseEvent {
	t.Helper()
	var ev sseEvent
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read event: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			return ev
		case strings.HasPrefix(line, "event: "):
			ev.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		}
	}
}
