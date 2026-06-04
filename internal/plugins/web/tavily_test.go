package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// NewSearcher precedence: Tavily > Brave > DuckDuckGo, with whitespace-only keys
// treated as "no key" so a stray space can't silently route to an unauthed API.
func TestNewSearcher_Precedence(t *testing.T) {
	if s := NewSearcher(Config{}); s.Name() != "duckduckgo" {
		t.Errorf("no key → %s, want duckduckgo", s.Name())
	}
	if s := NewSearcher(Config{BraveKey: "b"}); s.Name() != "brave" {
		t.Errorf("brave key → %s, want brave", s.Name())
	}
	if s := NewSearcher(Config{TavilyKey: "t"}); s.Name() != "tavily" {
		t.Errorf("tavily key → %s, want tavily", s.Name())
	}
	if s := NewSearcher(Config{TavilyKey: "t", BraveKey: "b"}); s.Name() != "tavily" {
		t.Errorf("tavily+brave → %s, want tavily (precedence)", s.Name())
	}
	if s := NewSearcher(Config{TavilyKey: "   "}); s.Name() != "duckduckgo" {
		t.Errorf("blank tavily → %s, want duckduckgo", s.Name())
	}
}

// tavilySearcher.search POSTs JSON with a Bearer token and parses
// results[].{title,url,content}; httptest stands in for api.tavily.com.
func TestTavilySearch_EndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q, want Bearer test-key", got)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["query"] != "go modules" {
			t.Errorf("query passthrough = %v", body["query"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"title":"Modules","url":"https://go.dev/ref/mod","content":"about <b>modules</b>"},
			{"title":"Two","url":"https://x/2","content":"second"}
		]}`))
	}))
	defer srv.Close()

	s := &tavilySearcher{key: "test-key", http: srv.Client()}
	results, err := s.search(context.Background(), "go modules", 5, srv.URL)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 || results[0].URL != "https://go.dev/ref/mod" {
		t.Fatalf("results wrong: %+v", results)
	}
	if strings.Contains(results[0].Snippet, "<b>") {
		t.Errorf("content HTML not stripped: %q", results[0].Snippet)
	}
}

// A 401/403 surfaces a message pointing at the key env var (so a misconfigured
// key is diagnosable from the tool error).
func TestTavilySearch_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	s := &tavilySearcher{key: "bad", http: srv.Client()}
	if _, err := s.search(context.Background(), "q", 5, srv.URL); err == nil ||
		!strings.Contains(err.Error(), "TAVILY_API_KEY") {
		t.Fatalf("want auth-hint error, got %v", err)
	}
}

func TestParseTavilyResults_CapsK(t *testing.T) {
	body := `{"results":[
		{"title":"a","url":"u1","content":"c"},
		{"title":"b","url":"u2","content":"c"},
		{"title":"c","url":"u3","content":"c"}]}`
	got, err := parseTavilyResults(strings.NewReader(body), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("k=2 cap not honored: %d", len(got))
	}
}
