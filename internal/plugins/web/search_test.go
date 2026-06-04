package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// NewSearcher picks Brave when a key is present, DDG otherwise. A whitespace-
// only key counts as "no key" — so a stray space in credentials.json doesn't
// silently route to Brave without auth.
func TestNewSearcher_PicksByKey(t *testing.T) {
	if s := NewSearcher(Config{}); s.Name() != "duckduckgo" {
		t.Fatalf("no key: want duckduckgo, got %s", s.Name())
	}
	if s := NewSearcher(Config{BraveKey: "abc"}); s.Name() != "brave" {
		t.Fatalf("key set: want brave, got %s", s.Name())
	}
	if s := NewSearcher(Config{BraveKey: "   "}); s.Name() != "duckduckgo" {
		t.Fatalf("blank key: want duckduckgo, got %s", s.Name())
	}
}

// parseDDGResults must extract title/url/snippet from DDG's lite HTML layout,
// decode the uddg redirect param, strip HTML tags, and honor the k cap.
func TestParseDDGResults(t *testing.T) {
	const html = `
<html><body>
<div class="result">
  <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2F&rut=x">The <b>Go</b> Programming Language</a>
  <a class="result__snippet" href="https://go.dev/doc/">Go is an open source <b>programming</b> language.</a>
</div>
<div class="result">
  <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com">Example Domain</a>
  <a class="result__snippet" href="https://example.com">For use in illustrative examples.</a>
</div>
<div class="result">
  <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fthird.example">Third</a>
  <a class="result__snippet" href="https://third.example">Third snippet.</a>
</div>
</body></html>`
	got := parseDDGResults(html, 2)
	if len(got) != 2 {
		t.Fatalf("k=2 cap not honored: got %d results", len(got))
	}
	if got[0].URL != "https://go.dev/doc/" {
		t.Fatalf("uddg decode failed: %q", got[0].URL)
	}
	if !strings.Contains(got[0].Title, "Go Programming Language") {
		t.Fatalf("title not stripped of <b>: %q", got[0].Title)
	}
	if !strings.Contains(got[0].Snippet, "programming language") {
		t.Fatalf("snippet not extracted: %q", got[0].Snippet)
	}
	if got[1].URL != "https://example.com" {
		t.Fatalf("second result URL wrong: %q", got[1].URL)
	}
}

// decodeDDGHref handles both the "//duckduckgo.com/l/?uddg=..." redirect form
// and a direct http(s) link. Anything else (mailto:, javascript:, malformed)
// must be dropped — we do NOT want the agent following weird schemes.
func TestDecodeDDGHref(t *testing.T) {
	cases := []struct{ in, want string }{
		{"//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F", "https://go.dev/"},
		{"https://example.com/page", "https://example.com/page"},
		{"http://example.com/page", "http://example.com/page"},
		{"mailto:x@y.z", ""},
		{"javascript:alert(1)", ""},
	}
	for _, c := range cases {
		if got := decodeDDGHref(c.in); got != c.want {
			t.Errorf("decodeDDGHref(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// stripTags removes HTML, collapses whitespace, decodes the entities DDG
// actually emits. Regression-guards the "<b>foo</b>" + "&amp;" cases.
func TestStripTags(t *testing.T) {
	in := `<a href="x"><b>Foo</b>&amp;Bar    baz</a>`
	if got := stripTags(in); got != "Foo&Bar baz" {
		t.Fatalf("stripTags = %q", got)
	}
}

// FormatSearchResults must produce a numbered list the model can act on —
// title, URL, optional snippet on the next indented line.
func TestFormatSearchResults(t *testing.T) {
	out := FormatSearchResults("duckduckgo", "go", []SearchResult{
		{Title: "Go", URL: "https://go.dev", Snippet: "the lang"},
		{Title: "Spec", URL: "https://go.dev/ref/spec"}, // no snippet
	})
	if !strings.Contains(out, "2 result(s) from duckduckgo for \"go\":") {
		t.Fatalf("header wrong: %q", out)
	}
	if !strings.Contains(out, "1. Go — https://go.dev") {
		t.Fatalf("missing numbered entry: %q", out)
	}
	if !strings.Contains(out, "   the lang") {
		t.Fatalf("snippet not indented: %q", out)
	}
	if strings.Contains(out, "2. Spec — https://go.dev/ref/spec\n   ") {
		t.Fatalf("empty snippet line should be suppressed: %q", out)
	}
}

// parseBraveResults decodes the documented JSON shape and honors k.
func TestParseBraveResults(t *testing.T) {
	const body = `{"web":{"results":[
		{"title":"Go","url":"https://go.dev","description":"The <b>Go</b> language"},
		{"title":"Spec","url":"https://go.dev/ref/spec","description":"Lang ref"},
		{"title":"Third","url":"https://third.example","description":"third"}
	]}}`
	got, err := parseBraveResults(strings.NewReader(body), 2)
	if err != nil {
		t.Fatalf("parseBraveResults: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("k=2 cap not honored: %d", len(got))
	}
	if got[0].URL != "https://go.dev" || !strings.Contains(got[0].Title, "Go") {
		t.Fatalf("first hit wrong: %+v", got[0])
	}
	if strings.Contains(got[0].Snippet, "<b>") {
		t.Fatalf("snippet HTML not stripped: %q", got[0].Snippet)
	}
}

// HandleSearch validates args (empty query → tool-level error), clamps k to a
// sane range, calls the configured searcher, and formats the result. We
// stand up a fake Brave endpoint via httptest so this stays hermetic — no
// network, no DDG flakiness.
func TestHandleSearch_BraveEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Subscription-Token") != "test-key" {
			t.Errorf("missing/wrong auth header: %q", r.Header.Get("X-Subscription-Token"))
		}
		if q := r.URL.Query().Get("q"); q != "go modules" {
			t.Errorf("query passthrough wrong: %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[{"title":"Modules","url":"https://go.dev/ref/mod","description":"about modules"}]}}`))
	}))
	defer srv.Close()

	// Swap the package-level client + override the endpoint via a key/test-only
	// constructor would be cleaner — but the simplest hermetic seam is to point
	// the brave searcher at the test server directly.
	s := &braveSearcher{key: "test-key", http: srv.Client()}
	// Re-implement the request against our test URL: we exercise the parser +
	// auth header contract; the URL builder is straightforward string concat
	// already covered by Search itself when reachable.
	results, err := s.search(context.Background(), "go modules", 5, srv.URL)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 || results[0].URL != "https://go.dev/ref/mod" {
		t.Fatalf("results wrong: %+v", results)
	}
}

// HandleSearch surface contract: missing query is a tool-level error (isErr
// true), not a transport error (err nil) — the model sees the message and can
// fix its call, the run continues.
func TestHandleSearch_RejectsEmptyQuery(t *testing.T) {
	out, isErr, err := HandleSearch(context.Background(), Config{}, json.RawMessage(`{"query":""}`))
	if err != nil {
		t.Fatalf("err should be nil for tool-level rejection: %v", err)
	}
	if !isErr {
		t.Fatal("isErr should be true")
	}
	if !strings.Contains(out, "query is required") {
		t.Fatalf("unhelpful message: %q", out)
	}
}
