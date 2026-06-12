package vllm

// Internal test (package vllm) so it can inspect the unexported http client's
// transport — verifies ForceHTTP11 actually disables HTTP/2 (TEN-218).

import (
	"context"
	"net/http"
	"testing"

	"tenant/internal/model"
)

func TestNew_ForceHTTP11DisablesH2(t *testing.T) {
	transportFor := func(force bool) *http.Transport {
		t.Helper()
		b, err := New(context.Background(), model.Profile{Backend: "vllm", Endpoint: "https://example", ForceHTTP11: force}, nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		tr, ok := b.(*Backend).client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("transport is %T, want *http.Transport", b.(*Backend).client.Transport)
		}
		return tr
	}

	// Forced: a non-nil, empty TLSNextProto is the canonical way to stop the
	// transport from auto-negotiating h2.
	if got := transportFor(true).TLSNextProto; got == nil {
		t.Error("ForceHTTP11=true must set a non-nil TLSNextProto to disable HTTP/2")
	} else if len(got) != 0 {
		t.Errorf("TLSNextProto should be empty (h2 disabled), has %d entries", len(got))
	}

	// Default: nil TLSNextProto leaves Go's HTTP/2 auto-negotiation intact.
	if got := transportFor(false).TLSNextProto; got != nil {
		t.Errorf("ForceHTTP11=false must leave TLSNextProto nil (h2 auto), got non-nil")
	}
}
