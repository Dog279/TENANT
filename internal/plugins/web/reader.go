package web

// reader.go is the HTTP "reader" fallback (TEN-113). When the headless browser
// is blocked (Cloudflare/DataDome interstitial, "verify you are human"), times
// out, or returns empty text, we refetch the URL through Jina Reader
// (https://r.jina.ai/<url>), which renders the page server-side and returns
// clean markdown. Keyless by default (low rate limit, $0); a JINA key raises
// the limit. Pure net/http — no browser, no SDK.
//
// Privacy note: the fallback only fires AFTER the local browser fails, and it
// sends the (already-failed) URL to a third party. Operators who don't want
// that set Config.ReaderDisabled.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// readerBase is the Jina Reader endpoint prefix. A var (not const) so tests can
// point it at an httptest server.
var readerBase = "https://r.jina.ai/"

// readerUA is a realistic desktop UA for the reader request (some origins Jina
// proxies still sniff the forwarded UA).
const readerUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

var readerHTTPClient = &http.Client{Timeout: 30 * time.Second}

// fetchViaReader retrieves targetURL through Jina Reader and returns the page as
// markdown. Returns an error on a non-2xx or transport failure so the caller can
// decide whether to surface the original browser error instead.
func fetchViaReader(ctx context.Context, cfg Config, targetURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, readerBase+targetURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", readerUA)
	req.Header.Set("Accept", "text/plain, text/markdown, */*")
	// Ask Jina for markdown explicitly.
	req.Header.Set("X-Return-Format", "markdown")
	if k := strings.TrimSpace(cfg.jinaKey()); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := readerHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("reader HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// blockPhrases are substrings that mark a bot-wall / interstitial / error page
// the browser "loaded" but that carries no real content — the signal to try the
// reader fallback.
var blockPhrases = []string{
	"verify you are human", "are you a robot", "checking your browser",
	"just a moment", "enable javascript", "please enable js",
	"access denied", "attention required", "unusual traffic",
	"captcha", "cf-browser-verification", "ddos protection",
}

// looksBlocked reports whether page text is empty or a known interstitial — in
// which case the reader fallback should be tried.
func looksBlocked(txt string) bool {
	t := strings.TrimSpace(txt)
	if t == "" {
		return true
	}
	low := strings.ToLower(t)
	for _, p := range blockPhrases {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}
