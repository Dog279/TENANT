package vllm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"tenant/internal/config"
	"tenant/internal/model"
	"tenant/internal/model/backend/vllm"
)

// platformTokenizer emulates one platform's /tokenize as the real servers
// behave: llama.cpp reads "content" and answers {"tokens": [...]}, ignoring
// anything else, so a vLLM-shaped request gets an empty list; vLLM reads
// "prompt" and answers {"count": N}, rejecting a request without it; Ollama
// has no such route (0.32).
func platformTokenizer(t *testing.T, platform string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/tokenize" || platform == "ollama" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch platform {
		case "llamacpp":
			if _, ok := body["content"]; !ok {
				_, _ = io.WriteString(w, `{"tokens":[]}`)
				return
			}
			_, _ = io.WriteString(w, `{"tokens":[9707,1879,15]}`)
		case "vllm":
			if _, ok := body["prompt"]; !ok {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"object":"error","message":"Field required: prompt","type":"BadRequestError","code":400}`)
				return
			}
			_, _ = io.WriteString(w, `{"count":3,"max_model_len":32768,"tokens":[9707,1879,15]}`)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

func newTokenizeBackend(t *testing.T, endpoint, kind string, estimateOnly bool) *vllm.Backend {
	t.Helper()
	b, err := vllm.New(context.Background(), model.Profile{
		ID: kind, Backend: "vllm", Endpoint: endpoint, Model: "m",
		ProviderKind: kind, EstimateTokensOnly: estimateOnly,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b.(*vllm.Backend)
}

func TestTokenizeDialect(t *testing.T) {
	for _, tc := range []struct {
		kind         string
		estimateOnly bool
		want         string
	}{
		{"vllm", false, vllm.DialectVLLM},
		{"", false, vllm.DialectVLLM},            // a bare --backend vllm
		{"llamacpp", true, vllm.DialectLlamaCpp}, // estimate-only in the catalog, tokenizes anyway
		{"llamacpp", false, vllm.DialectLlamaCpp},
		{"ollama", true, ""},
		{"openai", true, ""},
	} {
		if got := vllm.TokenizeDialect(tc.kind, tc.estimateOnly); got != tc.want {
			t.Errorf("TokenizeDialect(%q, %v) = %q, want %q", tc.kind, tc.estimateOnly, got, tc.want)
		}
	}
	// Against the catalog itself: of the OpenAI-compatible kinds, only vLLM
	// and llama.cpp have a /tokenize; Ollama and the hosted APIs estimate.
	for id, pk := range config.ProviderKinds {
		if pk.Backend != "vllm" {
			continue
		}
		want := ""
		switch id {
		case "vllm":
			want = vllm.DialectVLLM
		case "llamacpp":
			want = vllm.DialectLlamaCpp
		}
		if got := vllm.TokenizeDialect(id, pk.EstimateTokens); got != want {
			t.Errorf("catalog kind %q: dialect %q, want %q", id, got, want)
		}
	}
}

// Tokenize asks in the dialect it's given and reports what the server
// answered, where TokenCount would quietly estimate: doctor tells the
// platforms apart this way.
func TestVLLM_TokenizeByPlatform(t *testing.T) {
	for _, tc := range []struct {
		server, dialect string
		want            int
		wantErr         error
	}{
		{"llamacpp", vllm.DialectLlamaCpp, 3, nil},
		{"llamacpp", vllm.DialectVLLM, 0, nil}, // ignored fields: an empty list, not an error
		{"vllm", vllm.DialectVLLM, 3, nil},
		{"vllm", vllm.DialectLlamaCpp, 0, model.ErrInvalidRequest},
		{"ollama", vllm.DialectVLLM, 0, model.ErrInvalidRequest},
		{"ollama", vllm.DialectLlamaCpp, 0, model.ErrInvalidRequest},
	} {
		ts, _ := platformTokenizer(t, tc.server)
		n, err := newTokenizeBackend(t, ts.URL, tc.server, false).Tokenize(context.Background(), tc.dialect, "hello world")
		if n != tc.want || !errors.Is(err, tc.wantErr) {
			t.Errorf("%s server, %s dialect: Tokenize = %d, %v; want %d, %v", tc.server, tc.dialect, n, err, tc.want, tc.wantErr)
		}
	}
	ts, _ := platformTokenizer(t, "vllm")
	if _, err := newTokenizeBackend(t, ts.URL, "vllm", false).Tokenize(context.Background(), "tgi", "x"); !errors.Is(err, model.ErrInternal) {
		t.Errorf("unknown dialect: err = %v, want ErrInternal", err)
	}
}

// Ollama and the hosted kinds budget by estimate without asking the server.
func TestVLLM_TokenCountEstimateOnlyKindsNeverCallTokenize(t *testing.T) {
	for _, kind := range []string{"ollama", "openai"} {
		ts, hits := platformTokenizer(t, "vllm")
		n, err := newTokenizeBackend(t, ts.URL, kind, true).TokenCount(context.Background(), "twelve chars")
		if err != nil || n != 3 {
			t.Errorf("%s: TokenCount = %d, %v; want the 3-token estimate", kind, n, err)
		}
		if got := hits.Load(); got != 0 {
			t.Errorf("%s: %d requests to the server, want none", kind, got)
		}
	}
}
