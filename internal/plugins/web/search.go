package web

// web_search — discovery layer for agentic browsing. Without it, the model
// can only navigate to URLs it already knows (or hallucinates). DDG is the
// always-on default (no API key, scrapes the lite HTML endpoint); Brave is
// opt-in when an API key is configured and delivers cleaner, rate-stable
// results. Both speak the same Searcher interface so a third backend
// (Tavily / SearXNG / etc.) can drop in later.
//
// Search is HTTP-only — no Chrome — so it works even when the per-agent
// browser failed to launch. The agent's normal flow becomes:
//   web_search(query)  →  pick links  →  web_navigate(url)  →  web_read.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SearchResult is one web search hit.
type SearchResult struct {
	Title, URL, Snippet string
}

// Searcher backs the web_search tool. Implementations: DDG (default,
// keyless), Brave (API key required).
type Searcher interface {
	Name() string
	Search(ctx context.Context, query string, k int) ([]SearchResult, error)
}

// NewSearcher picks the best configured backend: Tavily (LLM-ready, free
// tier) > Brave (now metered) > DuckDuckGo (keyless default).
func NewSearcher(cfg Config) Searcher {
	if k := strings.TrimSpace(cfg.TavilyKey); k != "" {
		return &tavilySearcher{key: k, http: searchHTTPClient}
	}
	if k := strings.TrimSpace(cfg.BraveKey); k != "" {
		return &braveSearcher{key: k, http: searchHTTPClient}
	}
	return &ddgSearcher{http: searchHTTPClient}
}

// braveEndpoint is the public Brave API base URL. Overridable for tests.
const braveEndpoint = "https://api.search.brave.com/res/v1/web/search"

// HandleSearch implements the web_search tool entry point — shared by the
// regular Dispatcher and the per-agent lazyWeb (which routes web_search around
// its Chrome-launch step).
func HandleSearch(ctx context.Context, cfg Config, argsJSON json.RawMessage) (string, bool, error) {
	var a struct {
		Query string `json:"query"`
		K     int    `json:"k"`
	}
	if err := json.Unmarshal(argsJSON, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	q := strings.TrimSpace(a.Query)
	if q == "" {
		return "query is required", true, nil
	}
	k := a.K
	if k <= 0 || k > 25 {
		k = 8
	}
	s := NewSearcher(cfg)
	results, err := s.Search(ctx, q, k)
	if err != nil {
		return fmt.Sprintf("web_search (%s) failed: %v", s.Name(), err), true, nil
	}
	if len(results) == 0 {
		return fmt.Sprintf("web_search (%s) returned no results for %q", s.Name(), q), false, nil
	}
	return FormatSearchResults(s.Name(), q, results), false, nil
}

// FormatSearchResults renders results as a numbered list the model can act on
// directly: pick a URL, then web_navigate to it.
func FormatSearchResults(engine, query string, results []SearchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d result(s) from %s for %q:\n", len(results), engine, query)
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s — %s\n", i+1, strings.TrimSpace(r.Title), r.URL)
		if s := strings.TrimSpace(r.Snippet); s != "" {
			fmt.Fprintf(&b, "   %s\n", s)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- shared HTTP client ---

var searchHTTPClient = &http.Client{Timeout: 15 * time.Second}

// --- DuckDuckGo (HTML, keyless) ---

type ddgSearcher struct{ http *http.Client }

func (ddgSearcher) Name() string { return "duckduckgo" }

func (s *ddgSearcher) Search(ctx context.Context, query string, k int) ([]SearchResult, error) {
	endpoint := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	// DDG returns an empty body without a realistic UA.
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d from DuckDuckGo", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return nil, err
	}
	return parseDDGResults(string(body), k), nil
}

// DDG html-only layout (stable for years): one .result block per hit, with
// a.result__a (title + redirect href) and a.result__snippet next to it.
var (
	ddgTitleRE   = regexp.MustCompile(`(?s)<a[^>]*class="[^"]*result__a[^"]*"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRE = regexp.MustCompile(`(?s)<a[^>]*class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>`)
	htmlTagRE    = regexp.MustCompile(`<[^>]*>`)
	htmlWSRE     = regexp.MustCompile(`\s+`)
)

func parseDDGResults(html string, k int) []SearchResult {
	titles := ddgTitleRE.FindAllStringSubmatch(html, -1)
	snippets := ddgSnippetRE.FindAllStringSubmatch(html, -1)
	out := make([]SearchResult, 0, len(titles))
	for i, m := range titles {
		if len(out) >= k {
			break
		}
		realURL := decodeDDGHref(m[1])
		if realURL == "" {
			continue
		}
		title := stripTags(m[2])
		snippet := ""
		if i < len(snippets) {
			snippet = stripTags(snippets[i][1])
		}
		out = append(out, SearchResult{Title: title, URL: realURL, Snippet: snippet})
	}
	return out
}

// decodeDDGHref unwraps DDG's redirect URL. Real link sits in the `uddg`
// query param; supports both "//duckduckgo.com/l/?uddg=..." and the rare
// already-direct form.
func decodeDDGHref(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if dest := u.Query().Get("uddg"); dest != "" {
		if d, derr := url.QueryUnescape(dest); derr == nil {
			return d
		}
		return dest
	}
	// Direct link — accept only http(s).
	if u.Scheme == "http" || u.Scheme == "https" {
		return u.String()
	}
	return ""
}

func stripTags(s string) string {
	s = htmlTagRE.ReplaceAllString(s, "")
	s = htmlWSRE.ReplaceAllString(s, " ")
	// Minimal entity decode for the common ones we see in DDG output.
	s = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&nbsp;", " ").Replace(s)
	return strings.TrimSpace(s)
}

// --- Brave Search API (keyed) ---

type braveSearcher struct {
	key  string
	http *http.Client
}

func (braveSearcher) Name() string { return "brave" }

func (s *braveSearcher) Search(ctx context.Context, query string, k int) ([]SearchResult, error) {
	return s.search(ctx, query, k, braveEndpoint)
}

// search is the test seam: same as Search but with the endpoint URL injected,
// so httptest can stand in for api.search.brave.com without monkey-patching.
func (s *braveSearcher) search(ctx context.Context, query string, k int, endpoint string) ([]SearchResult, error) {
	full := endpoint + "?count=" + strconv.Itoa(k) +
		"&q=" + url.QueryEscape(query) + "&safesearch=moderate"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", s.key)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("HTTP %d (check BRAVE_SEARCH_API_KEY)", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d from Brave", resp.StatusCode)
	}
	return parseBraveResults(resp.Body, k)
}

func parseBraveResults(r io.Reader, k int) ([]SearchResult, error) {
	var raw struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode brave response: %w", err)
	}
	out := make([]SearchResult, 0, len(raw.Web.Results))
	for _, r := range raw.Web.Results {
		if len(out) >= k {
			break
		}
		out = append(out, SearchResult{
			Title:   stripTags(r.Title),
			URL:     r.URL,
			Snippet: stripTags(r.Description),
		})
	}
	return out, nil
}

// --- Tavily (keyed, LLM-ready) ---

// tavilyEndpoint is the Tavily search API. Overridable for tests.
const tavilyEndpoint = "https://api.tavily.com/search"

type tavilySearcher struct {
	key  string
	http *http.Client
}

func (tavilySearcher) Name() string { return "tavily" }

func (s *tavilySearcher) Search(ctx context.Context, query string, k int) ([]SearchResult, error) {
	return s.search(ctx, query, k, tavilyEndpoint)
}

// search is the test seam: same as Search but with the endpoint injected so
// httptest can stand in for api.tavily.com.
func (s *tavilySearcher) search(ctx context.Context, query string, k int, endpoint string) ([]SearchResult, error) {
	if k > 20 {
		k = 20 // Tavily caps max_results at 20
	}
	body, err := json.Marshal(map[string]any{
		"query":        query,
		"max_results":  k,
		"search_depth": "basic",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.key)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("HTTP %d (check TAVILY_API_KEY)", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d from Tavily", resp.StatusCode)
	}
	return parseTavilyResults(resp.Body, k)
}

func parseTavilyResults(r io.Reader, k int) ([]SearchResult, error) {
	var raw struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode tavily response: %w", err)
	}
	out := make([]SearchResult, 0, len(raw.Results))
	for _, r := range raw.Results {
		if len(out) >= k {
			break
		}
		out = append(out, SearchResult{
			Title:   stripTags(r.Title),
			URL:     r.URL,
			Snippet: stripTags(r.Content),
		})
	}
	return out, nil
}
