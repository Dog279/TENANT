package vllm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tenant/internal/model"
)

// A profile's request_timeout bounds one request and reports the endpoint as
// down (so fallbacks and health gating act), never as a cancel; without one
// the request waits for the server.
func TestPostJSON_RequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	build := func(d time.Duration) *Backend {
		inst, err := New(context.Background(), model.Profile{ID: "t", Backend: "vllm", Endpoint: srv.URL, Model: "m", RequestTimeout: d}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return inst.(*Backend)
	}
	var out map[string]any
	t0 := time.Now()
	err := build(50*time.Millisecond).postJSON(context.Background(), "/x", map[string]any{}, &out)
	if !errors.Is(err, model.ErrEndpointDown) || errors.Is(err, model.ErrCancelled) || time.Since(t0) > 250*time.Millisecond {
		t.Fatalf("with request_timeout 50ms: err=%v after %s, want endpoint down well before the server's 300ms", err, time.Since(t0))
	}
	if err := build(0).postJSON(context.Background(), "/x", map[string]any{}, &out); err != nil {
		t.Fatalf("without a request_timeout the request should wait for the server: %v", err)
	}
	// The caller's own cancel is still a cancel, not an endpoint failure.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := build(time.Second).postJSON(ctx, "/x", map[string]any{}, &out); !errors.Is(err, model.ErrCancelled) {
		t.Fatalf("the caller's deadline: err=%v, want cancelled", err)
	}
}
