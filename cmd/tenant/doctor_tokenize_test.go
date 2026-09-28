package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeTokenizeServer emulates one platform's /tokenize the way the real
// servers behave: llama.cpp reads "content" and ignores other fields (a
// vLLM-shaped request gets {"tokens":[]}); vLLM reads "prompt" and rejects a
// request without it; Ollama has no such route (0.32). A non-empty key makes
// it demand that bearer token, like vLLM run with --api-key.
func fakeTokenizeServer(t *testing.T, platform, key string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if key != "" && r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
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
	t.Cleanup(srv.Close)
	return srv, &hits
}

// The tokenize check knows vLLM, llama.cpp and Ollama: each gets the right
// verdict, a kind that doesn't match its server is named with the fix, and
// a hosted API is never probed.
func TestCheckTokenize_ByPlatform(t *testing.T) {
	for _, tc := range []struct {
		name, kind, server, key string
		want                    checkStatus
		detail, fix             string
	}{
		{"vLLM", "vllm", "vllm", "", statusOK, "/tokenize OK (vLLM, probe=3 tok)", ""},
		{"vLLM behind --api-key", "vllm", "vllm", "sk-local", statusOK, "(vLLM, probe=3 tok)", ""},
		{"a bare --backend vllm", "", "vllm", "", statusOK, "(vLLM, probe=3 tok)", ""},
		{"llama.cpp", "llamacpp", "llamacpp", "", statusOK, "/tokenize OK (llama.cpp, probe=3 tok)", ""},
		{"Ollama", "ollama", "ollama", "", statusSkip, "has no tokenize endpoint", ""},
		{"llama.cpp declared vllm", "vllm", "llamacpp", "", statusWarn,
			"answers llama.cpp's /tokenize, not vLLM's", `kind to "llamacpp"`},
		{"vLLM declared llamacpp", "llamacpp", "vllm", "", statusWarn,
			"answers vLLM's /tokenize, not llama.cpp's", `kind to "vllm"`},
		{"llama.cpp declared ollama", "ollama", "llamacpp", "", statusWarn,
			`answers llama.cpp's /tokenize, but kind "ollama" budgets with`, `kind to "llamacpp"`},
		{"a vLLM without /tokenize", "vllm", "ollama", "", statusWarn,
			"vLLM's /tokenize failed", "vLLM build with /tokenize"},
		{"a llama.cpp endpoint that isn't one", "llamacpp", "ollama", "", statusWarn,
			"llama.cpp's /tokenize failed", "check that this endpoint is one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fakeTokenizeServer(t, tc.server, tc.key)
			e := &doctorEnv{httpc: http.DefaultClient, c: &commonFlags{backend: "vllm", vllmEndpoint: srv.URL, vllmModel: "m", genKind: tc.kind, genAPIKey: tc.key}}
			r := checkTokenize(context.Background(), e)
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) || !strings.Contains(r.Fix, tc.fix) {
				t.Fatalf("got %s %q (fix %q), want %s containing %q (fix %q)", r.Status.tag(), r.Detail, r.Fix, tc.want.tag(), tc.detail, tc.fix)
			}
		})
	}
}

func TestCheckTokenize_HostedIsNeverProbed(t *testing.T) {
	srv, hits := fakeTokenizeServer(t, "vllm", "")
	e := &doctorEnv{httpc: http.DefaultClient, c: &commonFlags{backend: "vllm", vllmEndpoint: srv.URL, vllmModel: "gpt-4o", genKind: "openai", genAPIKey: "sk-test"}}
	r := checkTokenize(context.Background(), e)
	if r.Status != statusSkip || !strings.Contains(r.Detail, "is hosted") {
		t.Fatalf("got %s %q, want SKIP for a hosted API", r.Status.tag(), r.Detail)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d requests to a hosted API, want none", n)
	}
}

// An unreachable server is a warning about the configured dialect, not a
// guess at another platform.
func TestCheckTokenize_UnreachableServer(t *testing.T) {
	srv, _ := fakeTokenizeServer(t, "vllm", "")
	url := srv.URL
	srv.Close()
	e := &doctorEnv{httpc: http.DefaultClient, c: &commonFlags{backend: "vllm", vllmEndpoint: url, vllmModel: "m", genKind: "vllm"}}
	r := checkTokenize(context.Background(), e)
	if r.Status != statusWarn || !strings.Contains(r.Detail, "vLLM's /tokenize failed") {
		t.Fatalf("got %s %q, want WARN naming the failed vLLM /tokenize", r.Status.tag(), r.Detail)
	}
}
