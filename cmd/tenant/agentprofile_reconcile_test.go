package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A casing typo in a stored agent profile ("...-nvfp4" vs the served
// "...-NVFP4") must self-heal to the endpoint's served name — vLLM's model
// lookup is case-sensitive and otherwise 404s every generate of every spawn
// (the bug behind TEN-341's masked sub-agent failure).
func TestReconcileServedModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "" && got != "Bearer k" {
			t.Errorf("unexpected auth header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"Qwen3.6-35B-A3B-NVFP4"},{"id":"other-model"}]}`))
	}))
	defer srv.Close()

	cases := []struct {
		name, in, want string
	}{
		{"exact match unchanged", "Qwen3.6-35B-A3B-NVFP4", "Qwen3.6-35B-A3B-NVFP4"},
		{"casing typo adopts served name", "qwen3.6-35b-a3b-nvfp4", "Qwen3.6-35B-A3B-NVFP4"},
		{"total miss kept as configured", "not-served-at-all", "not-served-at-all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reconcileServedModel(srv.URL, "k", tc.in, "Programmer"); got != tc.want {
				t.Fatalf("reconcile(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	t.Run("unreachable endpoint keeps configured name", func(t *testing.T) {
		if got := reconcileServedModel("http://127.0.0.1:1", "", "any-model", "Programmer"); got != "any-model" {
			t.Fatalf("unreachable reconcile = %q, want any-model", got)
		}
	})
}
