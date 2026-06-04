package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHealthz: GET /healthz returns 200 JSON {"status":"ok"}.
func TestHealthz(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}, nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body = %v", body)
	}
}

// TestRootServesDashboard: after the TEN-110 cutover, GET / serves the SSR
// dashboard at the root (the JS SPA and its GET / file server are gone), and the
// legacy /page/ migration prefix no longer resolves.
func TestRootServesDashboard(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<!doctype html>", `href="/styles.css"`, "<h1>Dashboard</h1>", `src="/datastar.js"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard root missing %q", want)
		}
	}
	// The migration prefix is retired — no SPA file server backstops it anymore.
	old := httptest.NewRecorder()
	s.Handler().ServeHTTP(old, httptest.NewRequest(http.MethodGet, "/page/", nil))
	if old.Code != http.StatusNotFound {
		t.Errorf("legacy /page/ should 404 after cutover, got %d", old.Code)
	}
}

// TestRunGracefulShutdown: Run returns nil when ctx is canceled (clean
// shutdown), binding an ephemeral port so the test never collides.
func TestRunGracefulShutdown(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}, nil, nil, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()

	// Give ListenAndServe a moment to bind, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
