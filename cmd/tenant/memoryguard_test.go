package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSemanticMemoryReady: the predicate that gates TUI/serve startup (TEN-254).
func TestSemanticMemoryReady(t *testing.T) {
	ctx := context.Background()

	if semanticMemoryReady(ctx, &commonFlags{embedEndpoint: "", embedModel: "m"}) {
		t.Error("empty endpoint must be not-ready")
	}
	if semanticMemoryReady(ctx, &commonFlags{embedEndpoint: "http://x:1", embedModel: ""}) {
		t.Error("empty model must be not-ready")
	}

	// Reachable /v1/models (status < 500) ⇒ ready.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if !semanticMemoryReady(ctx, &commonFlags{embedEndpoint: srv.URL, embedModel: "m"}) {
		t.Error("reachable endpoint must be ready")
	}
	srv.Close()

	// A now-closed server ⇒ connection refused ⇒ not-ready.
	if semanticMemoryReady(ctx, &commonFlags{embedEndpoint: srv.URL, embedModel: "m"}) {
		t.Error("unreachable endpoint must be not-ready")
	}
}

// TestMemoryDownError: the refuse-to-start message names the fix + the override.
func TestMemoryDownError(t *testing.T) {
	withEndpoint := memoryDownError(&commonFlags{embedEndpoint: "http://h:11434", embedModel: "nomic"}).Error()
	for _, want := range []string{"EMBEDDINGS DOWN", "http://h:11434", "--allow-no-memory", "--backend echo"} {
		if !strings.Contains(withEndpoint, want) {
			t.Errorf("error (endpoint set) missing %q:\n%s", want, withEndpoint)
		}
	}

	noEndpoint := memoryDownError(&commonFlags{}).Error()
	for _, want := range []string{"No embedding endpoint", "--allow-no-memory"} {
		if !strings.Contains(noEndpoint, want) {
			t.Errorf("error (no endpoint) missing %q:\n%s", want, noEndpoint)
		}
	}
}
