package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is an OpenAI-compatible server for the model check: owned is
// each listed model's owned_by (the kind's signature); paths it also answers
// make it Ollama (/api/version) or an older llama.cpp (/props); chat answers
// the test call.
type fakeServer struct {
	models []string
	owned  string
	paths  map[string]string // extra GET path → JSON body
	key    string            // non-empty: every request must carry Bearer key
	chat   func(w http.ResponseWriter, req map[string]any)

	mu   sync.Mutex
	seen []string // "METHOD path auth"
}

func (f *fakeServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		f.mu.Unlock()
		if f.key != "" && r.Header.Get("Authorization") != "Bearer "+f.key {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			data := []map[string]any{}
			for _, m := range f.models {
				data = append(data, map[string]any{"id": m, "object": "model", "owned_by": f.owned})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" && f.chat != nil:
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.chat(w, req)
		case r.Method == http.MethodGet && f.paths[r.URL.Path] != "":
			_, _ = io.WriteString(w, f.paths[r.URL.Path])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeServer) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// chatAnswer answers a chat completion with message.
func chatAnswer(message map[string]any) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, _ map[string]any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": "stop"}}})
	}
}

var nativeAdd = chatAnswer(map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
	map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "add", "arguments": `{"a":2,"b":3}`}},
}})

func plainOf(t *testing.T, err error) string {
	t.Helper()
	var pe *plainError
	if !errors.As(err, &pe) {
		t.Fatalf("want a plain error, got %T %v", err, err)
	}
	return pe.plain
}

func TestNormalizeServerAddress(t *testing.T) {
	for in, want := range map[string]string{
		"http://my-server:8080":                     "http://my-server:8080",
		"  http://my-server:8080/  ":                "http://my-server:8080",
		"http://my-server:8080/v1":                  "http://my-server:8080",
		"http://my-server:8080/v1/":                 "http://my-server:8080",
		"http://my-server:8080/V1/chat/completions": "http://my-server:8080",
		"https://proxy.example/api/v1":              "https://proxy.example/api",
		"http://my-server:8080/v10":                 "http://my-server:8080/v10",
		"HTTP://My-Server:8080/v1":                  "http://My-Server:8080",
		"http://[::1]:8080/v1/models":               "http://[::1]:8080",
	} {
		got, err := normalizeServerAddress(in)
		if err != nil || got != want {
			t.Errorf("normalizeServerAddress(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]string{
		"":                               "Enter the server's address",
		"my-server:8080":                 "That isn't a server address",
		"ftp://my-server":                "That isn't a server address",
		"sk-live-0123456789abcdef":       "That isn't a server address",
		"http://":                        "That isn't a server address",
		"http://user:zz9pw@my-server:80": "Put a key in the API key box",
		"http://my-server:8080/?key=abc": "Leave out anything after ? or #",
		"http://my-server:8080/#frag":    "Leave out anything after ? or #",
	} {
		_, err := normalizeServerAddress(in)
		p := plainOf(t, err)
		if !strings.HasPrefix(p, want) {
			t.Errorf("normalizeServerAddress(%q): %q, want %q…", in, p, want)
		}
		for _, secret := range []string{"zz9pw", "sk-live", "abc"} {
			if strings.Contains(in, secret) && strings.Contains(err.Error(), secret) {
				t.Errorf("normalizeServerAddress(%q) echoed %q: %v", in, secret, err)
			}
		}
	}
}

// llama.cpp names itself in owned_by, and a Qwen model's structured
// tool_calls keep its family's format, which the name gives at each start:
// saved as auto.
func TestDiscover_LlamaCppNative(t *testing.T) {
	fs := &fakeServer{models: []string{"qwen3.8-27b"}, owned: "llamacpp", chat: nativeAdd}
	srv := fs.start(t)
	f, err := discoverServer(context.Background(), srv.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != "llamacpp" || f.Model != "qwen3.8-27b" || !f.Auto || f.Tools != toolsNative || f.ToolFmt != "qwen" {
		t.Errorf("found %+v", f)
	}
	// The test call offered one tool and let the model choose to call it.
	var sawChat bool
	for _, r := range fs.requests() {
		sawChat = sawChat || strings.HasPrefix(r, "POST /v1/chat/completions")
	}
	if !sawChat {
		t.Errorf("no test call: %q", fs.requests())
	}
}

// vLLM names itself in owned_by; a Hermes call written as text is read by
// the qwen adapter.
func TestDiscover_VLLMTextCall(t *testing.T) {
	fs := &fakeServer{models: []string{"Qwen3-32B"}, owned: "vllm",
		chat: chatAnswer(map[string]any{"role": "assistant", "content": "<tool_call>\n{\"name\": \"add\", \"arguments\": {\"a\": 2, \"b\": 3}}\n</tool_call>"})}
	f, err := discoverServer(context.Background(), fs.start(t).URL, "", "qwen3-32b") // typed in another case
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != "vllm" || f.Model != "Qwen3-32B" || f.Auto || f.Tools != toolsText || f.ToolFmt != "qwen" {
		t.Errorf("found %+v", f)
	}
}

// A model whose name says nothing writes Mistral's [TOOL_CALLS]: the check
// finds the adapter that reads it.
func TestDiscover_UnknownFamilyTextCall(t *testing.T) {
	fs := &fakeServer{models: []string{"acme-coder-7b"}, owned: "organization_owner",
		chat: chatAnswer(map[string]any{"role": "assistant", "content": `[TOOL_CALLS] [{"name": "add", "arguments": {"a": 2, "b": 3}}]`})}
	f, err := discoverServer(context.Background(), fs.start(t).URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != "openai-compat" || f.Tools != toolsText || f.ToolFmt != "mistral" {
		t.Errorf("found %+v", f)
	}
}

// Ollama answers /api/version; one that lists several models needs a name,
// and a model that refuses tools is refused with the server's reason.
func TestDiscover_Ollama(t *testing.T) {
	fs := &fakeServer{models: []string{"llama3.1:8b", "nomic-embed-text:latest"}, owned: "library",
		paths: map[string]string{"/api/version": `{"version":"0.32.9"}`}, chat: nativeAdd}
	srv := fs.start(t)
	_, err := discoverServer(context.Background(), srv.URL, "", "")
	if p := plainOf(t, err); p != "The server offers several models: llama3.1:8b, nomic-embed-text:latest. Type the one you want in Model." {
		t.Errorf("several models: %q", p)
	}
	_, err = discoverServer(context.Background(), srv.URL, "", "mistral")
	if p := plainOf(t, err); !strings.HasPrefix(p, "The server doesn't offer mistral. It offers llama3.1:8b, nomic-embed-text:latest.") {
		t.Errorf("not served: %q", p)
	}
	f, err := discoverServer(context.Background(), srv.URL, "", "LLAMA3.1:8B")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != "ollama" || f.Model != "llama3.1:8b" || f.Tools != toolsNative || f.ToolFmt != "llama" {
		t.Errorf("found %+v", f)
	}
	fs.chat = func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"nomic-embed-text:latest does not support tools‮!","type":"api_error"}}`)
	}
	_, err = discoverServer(context.Background(), srv.URL, "", "nomic-embed-text:latest")
	if p := plainOf(t, err); p != "nomic-embed-text:latest refused a test message (it said: nomic-embed-text:latest does not support tools), so your agent can't use it as the server is set up." {
		t.Errorf("refused: %q", p)
	}
}

// A server that is none of the three, whose model answers without the tool.
func TestDiscover_GenericNoToolCall(t *testing.T) {
	fs := &fakeServer{models: []string{"phi-4"}, owned: "organization_owner",
		chat: chatAnswer(map[string]any{"role": "assistant", "content": "2 + 3 = 5"})}
	f, err := discoverServer(context.Background(), fs.start(t).URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != "openai-compat" || f.Tools != toolsNone || f.ToolFmt != "openai" || !strings.Contains(f.Why, "2 + 3 = 5") {
		t.Errorf("found %+v", f)
	}
}

// An older llama.cpp that doesn't say so in owned_by still has /props.
func TestDiscover_LlamaCppByProps(t *testing.T) {
	fs := &fakeServer{models: []string{"model.gguf"}, owned: "",
		paths: map[string]string{"/props": `{"default_generation_settings":{"n_ctx":8192},"total_slots":1}`}, chat: nativeAdd}
	f, err := discoverServer(context.Background(), fs.start(t).URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != "llamacpp" || f.ToolFmt != "openai" {
		t.Errorf("found %+v", f)
	}
}

// A key goes as a Bearer on every request of the check; without it, or with
// the wrong one, the page says which.
func TestDiscover_APIKey(t *testing.T) {
	fs := &fakeServer{models: []string{"m"}, owned: "vllm", key: "sk-right", chat: nativeAdd}
	srv := fs.start(t)
	_, err := discoverServer(context.Background(), srv.URL, "", "")
	if p := plainOf(t, err); !strings.HasSuffix(p, "wants an API key. Add it under More options.") {
		t.Errorf("no key: %q", p)
	}
	_, err = discoverServer(context.Background(), srv.URL, "sk-wrong", "")
	if p := plainOf(t, err); !strings.HasSuffix(p, "turned down that API key.") || strings.Contains(err.Error(), "sk-wrong") {
		t.Errorf("wrong key: %q / %v", p, err)
	}
	before := len(fs.requests())
	if _, err := discoverServer(context.Background(), srv.URL, "sk-right", ""); err != nil {
		t.Fatal(err)
	}
	for _, r := range fs.requests()[before:] {
		if !strings.HasSuffix(r, " Bearer sk-right") {
			t.Errorf("a request of the check went without the key: %q", r)
		}
	}
}

// A server that echoes the request back in a refusal doesn't get the key
// into the page or the log.
func TestDiscover_KeyNeverEchoed(t *testing.T) {
	fs := &fakeServer{models: []string{"m"}, owned: "vllm", key: "sk-echo-9",
		chat: func(w http.ResponseWriter, _ map[string]any) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"bad request from Authorization: Bearer sk-echo-9"}}`)
		}}
	_, err := discoverServer(context.Background(), fs.start(t).URL, "sk-echo-9", "")
	if err == nil || strings.Contains(err.Error(), "sk-echo-9") || !strings.Contains(plainOf(t, err), "[the API key]") {
		t.Errorf("err = %v", err)
	}
}

// The check talks to the address typed and nowhere else: a redirect is an
// answer, not a place to go.
func TestDiscover_NoRedirects(t *testing.T) {
	elsewhere := &fakeServer{models: []string{"m"}, owned: "vllm", chat: nativeAdd}
	other := elsewhere.start(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()
	_, err := discoverServer(context.Background(), srv.URL, "sk-x", "")
	if p := plainOf(t, err); !strings.Contains(p, "doesn't answer like an OpenAI-compatible server") {
		t.Errorf("redirect: %q", p)
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Errorf("the redirect was followed: %q", elsewhere.requests())
	}
}

func TestDiscover_UnreachableAndNotOpenAI(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	_, err := discoverServer(context.Background(), url, "", "")
	if p := plainOf(t, err); !strings.Contains(p, "doesn't answer like an OpenAI-compatible server") {
		t.Errorf("404: %q", p)
	}
	srv.Close()
	_, err = discoverServer(context.Background(), url, "", "")
	if p := plainOf(t, err); !strings.HasPrefix(p, "Tenant couldn't reach "+url+".") {
		t.Errorf("closed: %q", p)
	}
	empty := &fakeServer{owned: "vllm"}
	_, err = discoverServer(context.Background(), empty.start(t).URL, "", "")
	if p := plainOf(t, err); !strings.Contains(p, "isn't offering a model yet") {
		t.Errorf("no models: %q", p)
	}
}

// A test call that doesn't finish in the budget, or a server error, leaves
// the model added with its format from its name.
func TestDiscover_UncheckedToolCalls(t *testing.T) {
	old := discoverBudget
	discoverBudget = 300 * time.Millisecond
	defer func() { discoverBudget = old }()
	release := make(chan struct{})
	defer close(release)
	slow := &fakeServer{models: []string{"gemma-3-27b"}, owned: "vllm", chat: func(w http.ResponseWriter, _ map[string]any) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}}
	f, err := discoverServer(context.Background(), slow.start(t).URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Tools != toolsUnchecked || f.ToolFmt != "gemma" {
		t.Errorf("slow: %+v", f)
	}
	busy := &fakeServer{models: []string{"m"}, owned: "vllm", chat: func(w http.ResponseWriter, _ map[string]any) {
		http.Error(w, "loading model", http.StatusServiceUnavailable)
	}}
	f, err = discoverServer(context.Background(), busy.start(t).URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Tools != toolsUnchecked || f.ToolFmt != "openai" || !strings.Contains(f.Why, "503") {
		t.Errorf("busy: %+v", f)
	}
}

// TestDiscover_Live checks a real server when TENANT_LIVE_MODEL_SERVER names
// one (http://host:port), with TENANT_LIVE_MODEL and TENANT_LIVE_MODEL_KEY
// when it needs them, and logs what the Model page would find.
func TestDiscover_Live(t *testing.T) {
	addr := os.Getenv("TENANT_LIVE_MODEL_SERVER")
	if addr == "" {
		t.Skip("set TENANT_LIVE_MODEL_SERVER=http://host:port to check a real server")
	}
	endpoint, err := normalizeServerAddress(addr)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	f, err := discoverServer(context.Background(), endpoint, os.Getenv("TENANT_LIVE_MODEL_KEY"), os.Getenv("TENANT_LIVE_MODEL"))
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		t.Fatalf("after %v: %v", took, err)
	}
	t.Logf("after %v: kind=%s model=%s auto=%v tools=%s format=%s why=%q served=%v", took, f.Kind, f.Model, f.Auto, f.Tools, f.ToolFmt, f.Why, f.Served)
}

// Text a server sends reaches the page as one clean line.
func TestDisplayText(t *testing.T) {
	if got := displayText("evil‮model​\nname\x07", 80); got != "evilmodel name" {
		t.Errorf("displayText = %q", got)
	}
	if got := displayText(strings.Repeat("x", 100), 10); got != strings.Repeat("x", 10)+"…" {
		t.Errorf("clip = %q", got)
	}
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	if got := listForPage(ids); got != "a, b, c, d, e, f and 2 more" {
		t.Errorf("listForPage = %q", got)
	}
}
