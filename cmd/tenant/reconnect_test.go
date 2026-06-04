package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The monitor probes the active backend on its cascade and reports a
// reconnect once the endpoint answers.
func TestReconnectMonitor_ReportsReconnect(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // reachable
	}))
	defer srv.Close()

	lc := &launchConfig{
		Provider:  "vllm",
		Providers: map[string]*providerConfig{"vllm": {Kind: "vllm", Endpoint: srv.URL, Model: "m"}},
	}
	if err := lc.save(dir); err != nil {
		t.Fatal(err)
	}

	feed := make(chan string, 16)
	rm := &reconnectMonitor{cfgDir: dir, feed: feed, ctx: context.Background(), fastEvery: 5 * time.Millisecond}

	rm.OnGenerationDown()
	// Idempotent: a second call while running must not start a second loop.
	rm.OnGenerationDown()

	deadline := time.After(2 * time.Second)
	reconnected := false
	for !reconnected {
		select {
		case line := <-feed:
			if strings.Contains(line, "reconnected") {
				reconnected = true
			}
		case <-deadline:
			t.Fatal("monitor never reported a reconnect")
		}
	}
	// The loop should have stopped (running flag cleared) shortly after.
	time.Sleep(20 * time.Millisecond)
	if rm.running.Load() {
		t.Fatal("monitor should stop after reconnecting")
	}
}

// With no active provider configured, the loop reports + stops rather than
// spinning forever.
func TestReconnectMonitor_NoActiveProvider(t *testing.T) {
	dir := t.TempDir()
	(&launchConfig{}).save(dir) // empty config: no active provider
	feed := make(chan string, 16)
	rm := &reconnectMonitor{cfgDir: dir, feed: feed, ctx: context.Background(), fastEvery: 5 * time.Millisecond}
	rm.OnGenerationDown()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case line := <-feed:
			if strings.Contains(line, "no active model") {
				return // good
			}
		case <-deadline:
			t.Fatal("monitor did not stop on missing provider")
		}
	}
}
