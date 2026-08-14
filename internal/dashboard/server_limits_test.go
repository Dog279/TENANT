package dashboard

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/httplimits"
)

func TestDashboardHTTPServerLimitsConfigured(t *testing.T) {
	srv := newDashboardHTTPServer("127.0.0.1:0", http.NotFoundHandler(), nil)
	if srv.ReadHeaderTimeout != httplimits.ReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, httplimits.ReadHeaderTimeout)
	}
	if srv.IdleTimeout != httplimits.IdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, httplimits.IdleTimeout)
	}
	if srv.MaxHeaderBytes != httplimits.MaxHeaderBytes {
		t.Errorf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, httplimits.MaxHeaderBytes)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want zero so streams are not globally bounded", srv.WriteTimeout)
	}
}

func TestDashboardOversizedBodyRejected(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(strings.Repeat("x", maxDashboardRequestBodyBytes+1)))
	req.ContentLength = -1 // exercise the MaxBytesReader path used for chunked bodies
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestDashboardOversizedHeaderRejected(t *testing.T) {
	srv := newDashboardHTTPServer("", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	addr := serveHTTPServer(t, srv)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	request := "GET / HTTP/1.1\r\nHost: localhost\r\nX-Fill: " + strings.Repeat("a", httplimits.MaxHeaderBytes*2) + "\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write oversized request: %v", err)
	}
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read rejection: %v", err)
	}
	if !strings.Contains(status, " 431 ") {
		t.Fatalf("status line = %q, want 431", strings.TrimSpace(status))
	}
}

func TestDashboardSlowHeaderConnectionDropped(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := newDashboardHTTPServer("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called <- struct{}{}
	}), nil)
	const testTimeout = 40 * time.Millisecond
	srv.ReadHeaderTimeout = testTimeout
	addr := serveHTTPServer(t, srv)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\nX-Slow: "); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * testTimeout)
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, err = io.ReadAll(conn)
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatal("slow-header connection remained open past ReadHeaderTimeout")
	}
	select {
	case <-called:
		t.Fatal("handler ran for an incomplete slow header")
	default:
	}
}

func TestDashboardSSEOutlivesNonStreamingWriteTimeout(t *testing.T) {
	broker := agent.NewBroker(0)
	s := New(Config{}, nil, nil, nil, broker, nil)
	const testTimeout = 40 * time.Millisecond
	hs := httptest.NewServer(s.handlerWithWriteTimeout(testTimeout))
	defer hs.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hs.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)

	// If the finite-response deadline leaked onto this route, the next write
	// after this pause would fail and close the stream.
	time.Sleep(3 * testTimeout)
	broker.Publish(agent.Event{Kind: agent.EventToolCall, Tool: "web_search", Args: "{}"})
	linec := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		line, readErr := reader.ReadString('\n')
		linec <- line
		errc <- readErr
	}()
	select {
	case line := <-linec:
		if readErr := <-errc; readErr != nil {
			t.Fatalf("read SSE after timeout: %v", readErr)
		}
		if !strings.Contains(line, "event: datastar-patch-elements") {
			t.Fatalf("first streamed line = %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SSE did not remain writable past the non-streaming timeout")
	}
}

func serveHTTPServer(t *testing.T, srv *http.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	})
	return ln.Addr().String()
}
