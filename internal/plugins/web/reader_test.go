package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

type staticResolver map[string][]netip.Addr

func (r staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := r[host]
	if !ok {
		return nil, fmt.Errorf("unexpected DNS lookup for %s", host)
	}
	return addrs, nil
}

func readerTestConfig(t *testing.T, srv *httptest.Server, targets ...string) Config {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	readerBase = "http://reader.test:" + u.Port() + "/"
	resolver := staticResolver{"reader.test": {netip.MustParseAddr("127.0.0.1")}}
	for _, target := range targets {
		resolver[target] = []netip.Addr{netip.MustParseAddr("93.184.216.34")}
	}
	return Config{PrivateNetwork: []string{"reader.test"}, resolver: resolver}
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
	var gotPath, gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotUA = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("# Title\n\nClean markdown body."))
	}))
	defer srv.Close()
	old := readerBase
	cfg := readerTestConfig(t, srv, "example.com")
	defer func() { readerBase = old }()

	cfg.JinaKey = "jk"
	txt, err := fetchViaReader(context.Background(), cfg, "https://example.com/page")
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
}

func TestFetchViaReader_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	old := readerBase
	cfg := readerTestConfig(t, srv, "x")
	defer func() { readerBase = old }()
	if _, err := fetchViaReader(context.Background(), cfg, "https://x"); err == nil {
		t.Fatal("non-2xx should error so the caller can keep the original browser error")
	}
}

func TestFetchViaReader_RejectsPrivateTargetBeforeRelay(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte("must not be reached"))
	}))
	defer srv.Close()
	old := readerBase
	cfg := readerTestConfig(t, srv)
	cfg.resolver.(staticResolver)["private.test"] = []netip.Addr{netip.MustParseAddr("10.0.0.8")}
	defer func() { readerBase = old }()
	if _, err := fetchViaReader(context.Background(), cfg, "http://private.test/secret"); err == nil {
		t.Fatal("reader must reject a private target before relaying it")
	}
	if requests != 0 {
		t.Fatalf("reader relay received %d request(s), want zero", requests)
	}
}

// navigate falls back to the reader when the browser fails (timeout/blocked);
// read then serves the cached reader-sourced text.
func TestNavigate_ReaderFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Reader markdown for the blocked page."))
	}))
	defer srv.Close()
	old := readerBase
	cfg := readerTestConfig(t, srv, "hard.example")
	defer func() { readerBase = old }()

	fb := &fakeBrowser{navErr: errors.New("context deadline exceeded")}
	d := disp(fb, Policy{})
	d.cfg = cfg
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Real article content via reader."))
	}))
	defer srv.Close()
	old := readerBase
	cfg := readerTestConfig(t, srv, "walled.example")
	defer func() { readerBase = old }()

	fb := &fakeBrowser{navTitle: "Just a moment", text: "Just a moment… verifying you are human"}
	d := disp(fb, Policy{})
	d.cfg = cfg
	d.Dispatch(context.Background(), call("web_navigate", map[string]any{"url": "https://walled.example/"}))
	out, isErr, _ := d.Dispatch(context.Background(), call("web_read", nil))
	if isErr || !strings.Contains(out, "Real article content") {
		t.Fatalf("read should fall back to reader on a bot-wall: isErr=%v out=%q", isErr, out)
	}
}

// ReaderDisabled turns the fallback off entirely — an empty page just reports
// no text, no third-party request.
func TestRead_ReaderDisabled(t *testing.T) {
	fb := &fakeBrowser{text: ""}
	d := newDispatcherWithBrowser(fb, Policy{}, ".")
	d.cfg.ReaderDisabled = true
	d.lastURL = "https://x/"
	out, isErr, _ := d.Dispatch(context.Background(), call("web_read", nil))
	if isErr || !strings.Contains(out, "no visible text") {
		t.Fatalf("reader disabled + empty page → no-text msg: isErr=%v out=%q", isErr, out)
	}
}
