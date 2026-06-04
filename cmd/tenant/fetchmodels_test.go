package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestFetchModels_V1PathHappyPath — primary OpenAI/vLLM/Ollama path:
// /v1/models returns the catalog on first try. Fallback never fires.
func TestFetchModels_V1PathHappyPath(t *testing.T) {
	var fallbackHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"glm-4.6","max_model_len":128000}]}`))
		case "/models":
			atomic.AddInt32(&fallbackHits, 1)
			http.Error(w, "should not fall back", 500)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	models, err := fetchModels(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "glm-4.6" {
		t.Errorf("v1 path returned wrong models: %+v", models)
	}
	if atomic.LoadInt32(&fallbackHits) != 0 {
		t.Errorf("fallback path was hit when /v1/models succeeded — wastes a network call per swap")
	}
}

// TestFetchModels_FallbackOn404 — load-bearing Z.ai case: /v1/models
// 404s because the base already includes /v4; /models works. Without
// this fallback, swap-probes lie about reachability and operators
// don't trust the swap (verified live 2026-05-26).
func TestFetchModels_FallbackOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			http.NotFound(w, r)
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"glm-4.6"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	models, err := fetchModels(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("fallback should have succeeded after /v1/models 404: %v", err)
	}
	if len(models) != 1 || models[0].ID != "glm-4.6" {
		t.Errorf("fallback returned wrong models: %+v", models)
	}
}

// TestFetchModels_BothFail — when neither path serves, surface the
// error. The error message must mention both paths so the operator
// understands what was tried.
func TestFetchModels_BothFail(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	_, err := fetchModels(context.Background(), srv.URL, "")
	if err == nil {
		t.Fatal("expected error when both paths 404")
	}
	// Both attempts went out; message must show "404 at both" so the
	// operator sees we tried the standard path AND the version-in-base
	// variant before giving up.
	if !strings.Contains(err.Error(), "404 at both") {
		t.Errorf("error message should signal both attempted paths 404'd; got %q", err.Error())
	}
}

// TestFetchModels_NoFallbackFor401 — drift guard for the narrow
// fallback policy. Only 404 triggers the alternate path; other 4xx
// (e.g. 401 auth-missing) MUST surface immediately because the
// alternate path won't fix them.
func TestFetchModels_NoFallbackFor401(t *testing.T) {
	var v1Hits, modelsHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			atomic.AddInt32(&v1Hits, 1)
			http.Error(w, "missing api key", 401)
		case "/models":
			atomic.AddInt32(&modelsHits, 1)
			http.Error(w, "should not reach here on 401", 500)
		}
	}))
	defer srv.Close()

	_, err := fetchModels(context.Background(), srv.URL, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should preserve the 401 status; got %q", err.Error())
	}
	if atomic.LoadInt32(&v1Hits) != 1 {
		t.Errorf("v1 path should be hit exactly once on 401, got %d", v1Hits)
	}
	if atomic.LoadInt32(&modelsHits) != 0 {
		t.Errorf("/models must NOT be retried on 401 — alternate path won't fix auth; got %d hits", modelsHits)
	}
}

// TestFetchModels_AuthHeader — every variant sends the bearer token
// when configured (used by Z.ai, OpenAI, etc).
func TestFetchModels_AuthHeader(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	_, _ = fetchModels(context.Background(), srv.URL, "test-token-xyz")
	if sawAuth != "Bearer test-token-xyz" {
		t.Errorf("Authorization header malformed: got %q", sawAuth)
	}
}
