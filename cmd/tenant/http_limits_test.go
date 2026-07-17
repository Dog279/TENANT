package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tenant/internal/httplimits"
	"tenant/internal/mcp/transport"
)

func TestSSEHTTPServerLimitsConfigured(t *testing.T) {
	srv := newSSEHTTPServer("127.0.0.1:0", http.NotFoundHandler())
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
		t.Errorf("WriteTimeout = %v, want zero for the SSE stream", srv.WriteTimeout)
	}

	req := httptest.NewRequest(http.MethodPost, "/message", strings.NewReader(strings.Repeat("x", transport.DefaultMaxFrameSize+1)))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized message status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}
