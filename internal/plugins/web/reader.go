package web

// reader.go is the HTTP "reader" fallback (TEN-113). When the headless browser
// is blocked (Cloudflare/DataDome interstitial, "verify you are human"), times
// out, or returns empty text, we refetch the URL through Jina Reader
// (https://r.jina.ai/<url>), which renders the page server-side and returns
// clean markdown. The privacy boundary is explicit: ReaderEnabled must be true,
// and private-network or credential-bearing targets are rejected before any
// request is made. Pure net/http — no browser, no SDK.
//
// When enabled, the fallback only fires AFTER local browser extraction fails.
// It sends the target URL to a third party and emits a host-only activity event
// immediately before every outbound request.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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

// readerLookupIP is a seam for deterministic tests. Production resolves the
// target locally so hostnames that point at loopback/private space are refused
// instead of being disclosed to the remote reader.
var readerLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// ReaderActivity is the privacy-safe audit payload for a Jina request. The
// target path and query are deliberately absent so the event cannot become a
// second disclosure channel.
type ReaderActivity struct {
	Provider   string
	TargetHost string
}

// fetchViaReader retrieves targetURL through Jina Reader and returns the page as
// markdown. Returns an error on a non-2xx or transport failure so the caller can
// decide whether to surface the original browser error instead.
func fetchViaReader(ctx context.Context, cfg Config, targetURL string) (string, error) {
	if !cfg.ReaderEnabled {
		return "", fmt.Errorf("reader disabled: explicit opt-in required")
	}
	if cfg.ReaderObserver == nil {
		return "", fmt.Errorf("reader disabled: activity observer is required")
	}
	safeURL, targetHost, err := validateReaderTarget(ctx, targetURL)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, readerBase+safeURL, nil)
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
	cfg.ReaderObserver(ReaderActivity{Provider: "r.jina.ai", TargetHost: targetHost})
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

// validateReaderTarget enforces the privacy boundary before the target is
// included in a request to Jina. It returns a URL with its fragment stripped
// (fragments are local-only browser state) and a host-only value for auditing.
func validateReaderTarget(ctx context.Context, raw string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", fmt.Errorf("reader blocked: target must be an absolute http/https URL")
	}
	if u.User != nil {
		return "", "", fmt.Errorf("reader blocked: target URL contains credentials")
	}
	if hasSensitiveReaderQuery(u.RawQuery) {
		return "", "", fmt.Errorf("reader blocked: target URL contains a signature, token, or credential query parameter")
	}

	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || isLocalReaderName(host) {
		return "", "", fmt.Errorf("reader blocked: target host is local or private")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicReaderIP(ip) {
			return "", "", fmt.Errorf("reader blocked: target host is local or private")
		}
	} else {
		ips, lookupErr := readerLookupIP(ctx, host)
		if lookupErr != nil || len(ips) == 0 {
			return "", "", fmt.Errorf("reader blocked: target host could not be resolved safely")
		}
		for _, ip := range ips {
			if !isPublicReaderIP(ip) {
				return "", "", fmt.Errorf("reader blocked: target host resolves to local or private space")
			}
		}
	}

	u.Fragment = ""
	return u.String(), host, nil
}

func isLocalReaderName(host string) bool {
	if host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".home.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

func isPublicReaderIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	// Go's IsPrivate intentionally excludes shared carrier-grade NAT space.
	// It is still non-public and may expose provider/internal infrastructure.
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return !cgnat.Contains(ip)
}

func hasSensitiveReaderQuery(raw string) bool {
	if raw == "" {
		return false
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return true // malformed queries are not safe to relay
	}
	for name := range values {
		lower := strings.ToLower(strings.TrimSpace(name))
		if strings.HasPrefix(lower, "x-amz-") || strings.HasPrefix(lower, "x-goog-") || strings.HasPrefix(lower, "x-ms-") {
			return true
		}
		normalized := strings.NewReplacer("-", "_", ".", "_", "[", "_", "]", "_").Replace(lower)
		for _, part := range strings.FieldsFunc(normalized, func(r rune) bool { return r == '_' }) {
			switch part {
			case "sig", "signature", "token", "jwt", "key", "secret", "password", "passwd", "credential", "credentials", "auth", "authorization", "bearer", "session", "sessionid", "ticket":
				return true
			}
		}
		switch normalized {
		case "apikey", "accesstoken", "refreshtoken", "idtoken", "clientsecret", "keypairid":
			return true
		}
	}
	return false
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
