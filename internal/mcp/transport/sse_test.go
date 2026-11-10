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

	// 2. First event must name the POST endpoint, with this stream's session id.
	ev := readEvent(t, br)
	if ev.event != "endpoint" || !strings.HasPrefix(ev.data, "/message?sessionId=") || len(ev.data) < len("/message?sessionId=")+32 {
		t.Fatalf("first event = %q/%q, want endpoint//message?sessionId=<id>", ev.event, ev.data)
	}

	// 3. Client → server: POST a frame, expect it from Recv.
	go func() {
		preq, _ := http.NewRequest(http.MethodPost, hs.URL+ev.data,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		preq.Header.Set("Content-Type", "application/json")
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

// openStream opens the GET stream of sse on a test server and returns the
// POST endpoint its first event names (path and session id).
func openStream(t *testing.T, sse *transport.SSE) (base, endpoint string) {
	t.Helper()
	hs := httptest.NewServer(sse)
	t.Cleanup(hs.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, hs.URL+"/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return hs.URL, readEvent(t, bufio.NewReader(resp.Body)).data
}

func postStatus(t *testing.T, url, contentType, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestSSE_OversizedMessageRejected(t *testing.T) {
	const max = 32
	sse := transport.NewSSE(transport.SSEConfig{MaxFrameSize: max})
	defer sse.Close()
	base, endpoint := openStream(t, sse)
	if got := postStatus(t, base+endpoint, "application/json", strings.Repeat("x", max+1)); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", got, http.StatusRequestEntityTooLarge)
	}
}

// A message is accepted only at the endpoint the stream named: without the
// session id, or with another one, it is refused and never reaches Recv. Only
// whoever read the stream can send into its session.
func TestSSE_MessageNeedsTheStreamsSession(t *testing.T) {
	sse := transport.NewSSE(transport.SSEConfig{})
	defer sse.Close()
	base, endpoint := openStream(t, sse)
	const frame = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for _, url := range []string{
		base + "/message",
		base + "/message?sessionId=",
		base + "/message?sessionId=00000000000000000000000000000000",
	} {
		if got := postStatus(t, url, "application/json", frame); got != http.StatusForbidden {
			t.Errorf("POST %s = %d, want 403", url, got)
		}
	}
	if got := postStatus(t, base+endpoint, "application/json", frame); got != http.StatusAccepted {
		t.Fatalf("POST to the named endpoint = %d, want 202", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if got, err := sse.Recv(ctx); err != nil || !strings.Contains(string(got), `"ping"`) {
		t.Fatalf("Recv = %s, %v; want the one accepted frame", got, err)
	}
}

// A message must be application/json: the content types a web page can send
// across origins without the browser asking first are refused.
func TestSSE_MessageMustBeJSON(t *testing.T) {
	sse := transport.NewSSE(transport.SSEConfig{})
	defer sse.Close()
	base, endpoint := openStream(t, sse)
	const frame = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "text/plain; charset=application/json"} {
		if got := postStatus(t, base+endpoint, ct, frame); got != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q = %d, want 415", ct, got)
		}
	}
	if got := postStatus(t, base+endpoint, "application/json; charset=utf-8", frame); got != http.StatusAccepted {
		t.Fatalf("application/json with a charset = %d, want 202", got)
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
