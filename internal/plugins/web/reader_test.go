package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func usePublicReaderDNS(t *testing.T) {
	t.Helper()
	old := readerLookupIP
	readerLookupIP = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	t.Cleanup(func() { readerLookupIP = old })
}

func TestLooksBlocked(t *testing.T) {
	for _, s := range []string{"", "   ", "Just a moment...", "Please enable JavaScript to continue", "Verify you are human", "Access Denied"} {
		if !looksBlocked(s) {
			t.Errorf("%q should look blocked", s)
		}
	}
	for _, s := range []string{"Here is a normal article about Go and its history.", "Short but real."} {
		if looksBlocked(s) {
			t.Errorf("%q should NOT look blocked", s)
		}
	}
}

// fetchViaReader prepends the target URL to the reader base, sends a realistic
// UA + the Bearer key when present, and returns the body.
func TestFetchViaReader(t *testing.T) {
	usePublicReaderDNS(t)
	var gotPath, gotAuth, gotUA string
	var activities []ReaderActivity
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotUA = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("# Title\n\nClean markdown body."))
	}))
	defer srv.Close()
	old := readerBase
	readerBase = srv.URL + "/"
	defer func() { readerBase = old }()

	cfg := Config{
		ReaderEnabled:  true,
		JinaKey:        "jk",
		ReaderObserver: func(a ReaderActivity) { activities = append(activities, a) },
	}
	txt, err := fetchViaReader(context.Background(), cfg, "https://example.com/page#local-fragment")
	if err != nil {
		t.Fatalf("fetchViaReader: %v", err)
	}
	if !strings.Contains(txt, "Clean markdown body") {
		t.Errorf("body wrong: %q", txt)
	}
	if !strings.Contains(gotPath, "https://example.com/page") {
		t.Errorf("target URL not forwarded in path: %q", gotPath)
	}
	if gotAuth != "Bearer jk" {
		t.Errorf("auth header = %q, want Bearer jk", gotAuth)
	}
	if !strings.Contains(gotUA, "Mozilla") {
		t.Errorf("UA not realistic: %q", gotUA)
	}
	if strings.Contains(gotPath, "local-fragment") {
		t.Errorf("URL fragment must be stripped before relay: %q", gotPath)
	}
	if len(activities) != 1 || activities[0].Provider != "r.jina.ai" || activities[0].TargetHost != "example.com" {
		t.Fatalf("activity = %+v, want one host-only Jina event", activities)
	}
}

func TestFetchViaReader_Non200(t *testing.T) {
	usePublicReaderDNS(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	old := readerBase
	readerBase = srv.URL + "/"
	defer func() { readerBase = old }()
	if _, err := fetchViaReader(context.Background(), Config{ReaderEnabled: true, ReaderObserver: func(ReaderActivity) {}}, "https://example.com/"); err == nil {
		t.Fatal("non-2xx should error so the caller can keep the original browser error")
	}
}

// navigate falls back to the reader when the browser fails (timeout/blocked);
// read then serves the cached reader-sourced text.
func TestNavigate_ReaderFallback(t *testing.T) {
	usePublicReaderDNS(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Reader markdown for the blocked page."))
	}))
	defer srv.Close()
	old := readerBase
	readerBase = srv.URL + "/"
	defer func() { readerBase = old }()

	fb := &fakeBrowser{navErr: errors.New("context deadline exceeded")}
	d := disp(fb, Policy{})
	d.cfg.ReaderEnabled = true
	d.cfg.ReaderObserver = func(ReaderActivity) {}
	out, isErr, _ := d.Dispatch(context.Background(), call("web_navigate", map[string]any{"url": "https://hard.example/"}))
	if isErr || !strings.Contains(out, "reader fallback") {
		t.Fatalf("navigate should fall back to reader: isErr=%v out=%q", isErr, out)
	}
	rout, rErr, _ := d.Dispatch(context.Background(), call("web_read", nil))
	if rErr || !strings.Contains(rout, "Reader markdown") {
		t.Fatalf("read should serve the reader text: isErr=%v out=%q", rErr, rout)
	}
}

// read falls back to the reader when the browser "loaded" a bot-wall.
func TestRead_ReaderFallbackOnBlock(t *testing.T) {
	usePublicReaderDNS(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Real article content via reader."))
	}))
	defer srv.Close()
	old := readerBase
	readerBase = srv.URL + "/"
	defer func() { readerBase = old }()

	fb := &fakeBrowser{navTitle: "Just a moment", text: "Just a moment… verifying you are human"}
	d := disp(fb, Policy{})
	d.cfg.ReaderEnabled = true
	d.cfg.ReaderObserver = func(ReaderActivity) {}
	d.Dispatch(context.Background(), call("web_navigate", map[string]any{"url": "https://walled.example/"}))
	out, isErr, _ := d.Dispatch(context.Background(), call("web_read", nil))
	if isErr || !strings.Contains(out, "Real article content") {
		t.Fatalf("read should fall back to reader on a bot-wall: isErr=%v out=%q", isErr, out)
	}
}

// The zero-value config keeps the remote fallback off — an empty page just
// reports no text and no third-party request.
func TestRead_ReaderDefaultOff(t *testing.T) {
	fb := &fakeBrowser{text: ""}
	d := newDispatcherWithBrowser(fb, Policy{}, ".")
	d.lastURL = "https://x/"
	out, isErr, _ := d.Dispatch(context.Background(), call("web_read", nil))
	if isErr || !strings.Contains(out, "no visible text") {
		t.Fatalf("reader default-off + empty page → no-text msg: isErr=%v out=%q", isErr, out)
	}
}

func TestFetchViaReader_RequiresExplicitOptIn(t *testing.T) {
	requests, activities := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer srv.Close()
	oldBase := readerBase
	readerBase = srv.URL + "/"
	defer func() { readerBase = oldBase }()

	_, err := fetchViaReader(context.Background(), Config{ReaderObserver: func(ReaderActivity) { activities++ }}, "https://example.com/")
	if err == nil || !strings.Contains(err.Error(), "explicit opt-in") {
		t.Fatalf("default config error = %v, want explicit opt-in refusal", err)
	}
	if requests != 0 || activities != 0 {
		t.Fatalf("default-off reader leaked activity/request: activities=%d requests=%d", activities, requests)
	}
	_, err = fetchViaReader(context.Background(), Config{ReaderEnabled: true}, "https://example.com/")
	if err == nil || !strings.Contains(err.Error(), "activity observer") {
		t.Fatalf("enabled reader without audit sink error = %v, want refusal", err)
	}
	if requests != 0 {
		t.Fatalf("reader without audit sink made %d request(s)", requests)
	}
}

func TestFetchViaReader_BlocksPrivateAndSensitiveTargets(t *testing.T) {
	requests, activities := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer srv.Close()
	oldBase := readerBase
	readerBase = srv.URL + "/"
	defer func() { readerBase = oldBase }()
	oldLookup := readerLookupIP
	readerLookupIP = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "private.example" {
			return []net.IP{net.ParseIP("10.2.3.4")}, nil
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	defer func() { readerLookupIP = oldLookup }()

	cfg := Config{ReaderEnabled: true, ReaderObserver: func(ReaderActivity) { activities++ }}
	targets := []string{
		"http://127.0.0.1/admin",
		"http://[::1]/admin",
		"http://10.0.0.8/admin",
		"http://100.64.0.1/metadata",
		"http://localhost/admin",
		"http://printer.internal/status",
		"http://intranet/status",
		"https://private.example/status",
		"https://user:password@public.example/private",
		"https://public.example/file?sig=abc",
		"https://public.example/file?access_token=abc",
		"https://public.example/file?api_key=abc",
		"https://public.example/file?X-Amz-Signature=abc",
		"https://public.example/file?X-Goog-Credential=abc",
	}
	for _, target := range targets {
		if _, err := fetchViaReader(context.Background(), cfg, target); err == nil || !strings.Contains(err.Error(), "reader blocked") {
			t.Errorf("target %q error = %v, want privacy refusal", target, err)
		}
	}
	if requests != 0 || activities != 0 {
		t.Fatalf("blocked targets leaked activity/request: activities=%d requests=%d", activities, requests)
	}
}

func TestFetchViaReader_AllowsOrdinaryQuery(t *testing.T) {
	usePublicReaderDNS(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	old := readerBase
	readerBase = srv.URL + "/"
	defer func() { readerBase = old }()

	got, err := fetchViaReader(context.Background(), Config{ReaderEnabled: true, ReaderObserver: func(ReaderActivity) {}}, "https://example.com/article?page=2&utm_source=test&monkey=banana")
	if err != nil || got != "ok" {
		t.Fatalf("ordinary query rejected: got=%q err=%v", got, err)
	}
}
