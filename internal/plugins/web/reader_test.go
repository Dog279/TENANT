package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	readerBase = srv.URL + "/"
	defer func() { readerBase = old }()

	txt, err := fetchViaReader(context.Background(), Config{JinaKey: "jk"}, "https://example.com/page")
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
	readerBase = srv.URL + "/"
	defer func() { readerBase = old }()
	if _, err := fetchViaReader(context.Background(), Config{}, "https://x"); err == nil {
		t.Fatal("non-2xx should error so the caller can keep the original browser error")
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
	readerBase = srv.URL + "/"
	defer func() { readerBase = old }()

	fb := &fakeBrowser{navErr: errors.New("context deadline exceeded")}
	d := disp(fb, Policy{})
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
	readerBase = srv.URL + "/"
	defer func() { readerBase = old }()

	fb := &fakeBrowser{navTitle: "Just a moment", text: "Just a moment… verifying you are human"}
	d := disp(fb, Policy{})
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
