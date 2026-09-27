package mcpremote

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func portFree(addr string) bool {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// The callback port is bound only while a sign-in runs: free before, free
// after, and the code the browser delivers comes back. A long-running hub can
// therefore add connectors one after another indefinitely.
func TestLazyFetcher_HoldsPortOnlyDuringSignIn(t *testing.T) {
	addr := freePort(t)
	// "open the browser" = hit the callback ourselves, like the consent page's redirect would.
	open := func(u string) error {
		go func() {
			time.Sleep(50 * time.Millisecond)
			_, _ = http.Get("http://" + addr + "/callback?code=c0de&state=st8")
		}()
		return nil
	}
	fetch := newLazyCodeFetcher(addr, open)
	if !portFree(addr) {
		t.Fatal("building the fetcher must not bind the port")
	}
	res, err := fetch(context.Background(), &auth.AuthorizationArgs{URL: "https://as.example/authorize?state=st8"})
	if err != nil || res.Code != "c0de" || res.State != "st8" {
		t.Fatalf("fetch = %+v, %v", res, err)
	}
	if !portFree(addr) {
		t.Fatal("the port must be released after the sign-in completes")
	}
	// A second sign-in on the same port works (the old failure mode).
	if _, err := fetch(context.Background(), &auth.AuthorizationArgs{URL: "https://as.example/authorize?state=st8"}); err != nil {
		t.Fatalf("second sign-in: %v", err)
	}
}

// Two connectors needing a sign-in at once queue on the shared port instead of
// one of them failing to bind.
func TestLazyFetcher_SerializesConcurrentSignIns(t *testing.T) {
	addr := freePort(t)
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	open := func(u string) error {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		go func() {
			time.Sleep(60 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			_, _ = http.Get("http://" + addr + "/callback?code=ok&state=s")
		}()
		return nil
	}
	fetch := newLazyCodeFetcher(addr, open)
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := fetch(context.Background(), &auth.AuthorizationArgs{URL: "https://as.example/authorize?state=s"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a queued sign-in failed: %v", err)
		}
	}
	if maxInFlight != 1 {
		t.Errorf("sign-ins must serialize (max in flight = %d)", maxInFlight)
	}
	if !portFree(addr) {
		t.Error("port must be free once every sign-in is done")
	}
}

// A cancelled context is honored before binding anything.
func TestLazyFetcher_CancelledBeforeBind(t *testing.T) {
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newLazyCodeFetcher(addr, nil)(ctx, &auth.AuthorizationArgs{}); err == nil {
		t.Fatal("expected ctx error")
	}
	if !portFree(addr) {
		t.Fatal("nothing should be bound")
	}
}

// The opener shows the operator the consent URL; when it fails the URL goes to
// the process logger (the log book, a one-shot command's terminal), never
// straight to stderr over the TUI.
func TestLazyFetcher_LogsTheURLWhenNoBrowserOpens(t *testing.T) {
	var buf bytes.Buffer
	prev, prevW, prevF := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev); log.SetOutput(prevW); log.SetFlags(prevF) })

	addr := freePort(t)
	open := func(u string) error {
		go func() {
			time.Sleep(50 * time.Millisecond)
			_, _ = http.Get("http://" + addr + "/callback?code=c&state=s")
		}()
		return errors.New("no browser here")
	}
	if _, err := newLazyCodeFetcher(addr, open)(context.Background(), &auth.AuthorizationArgs{URL: "https://as.example/authorize?state=s"}); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "https://as.example/authorize?state=s") {
		t.Fatalf("the consent URL was not logged:\n%s", out)
	}
}
