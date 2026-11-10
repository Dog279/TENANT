package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// bridgeCfgDirs maps a test bridge's address to its config dir, where its
// token file is.
var bridgeCfgDirs sync.Map

// bridgeToken is the bearer token of the bridge at addr, as a client would
// read it from gateway-auth-token. The bridge writes it before it listens.
func bridgeToken(t *testing.T, addr string) string {
	t.Helper()
	dir, ok := bridgeCfgDirs.Load(addr)
	if !ok {
		t.Fatalf("no config dir recorded for the bridge at %s", addr)
	}
	b, err := os.ReadFile(gatewayAuthTokenPath(dir.(string)))
	if err != nil {
		t.Fatalf("read the bridge's token: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// bridgeDo sends one request to the bridge at addr the way a configured
// client does: with the bridge's token, and JSON for a body.
func bridgeDo(t *testing.T, ctx context.Context, method, addr, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, method, "http://"+addr+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bridgeToken(t, addr))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// oneSession opens a client session against the bridge: it starts the GET
// stream, initializes, lists tools, and then drops the stream — exactly what a
// `claude -p` spawn does over its lifetime.
func oneSession(t *testing.T, addr string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/sse", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+bridgeToken(t, addr))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open sse stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sse stream status = %d, want 200", resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)
	msgPath := ""
	for msgPath == "" {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read endpoint event: %v", err)
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			msgPath = strings.TrimSpace(v)
		}
	}

	post := func(body any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := bridgeDo(t, ctx, http.MethodPost, addr, msgPath, string(raw))
		r.Body.Close()
		if r.StatusCode != http.StatusAccepted {
			t.Fatalf("post status = %d, want 202", r.StatusCode)
		}
	}
	post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2024-11-05", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"}}})
	post(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized", "params": map[string]any{}})
	post(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}})

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read tools/list response: %v", err)
		}
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok || !strings.Contains(v, `"id":2`) {
			continue
		}
		var env struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(v)), &env); err != nil {
			t.Fatalf("decode tools/list: %v", err)
		}
		names := make([]string, 0, len(env.Result.Tools))
		for _, tl := range env.Result.Tools {
			names = append(names, tl.Name)
		}
		return names
	}
}

// The bridge exists to serve many SEQUENTIAL `claude -p` spawns. The SSE
// transport is single-session by contract, so the naive wiring exited as soon
// as the first client's stream dropped: the bridge answered exactly one probe
// and every spawn after that got connection-refused, which read as "the bridge
// never started" rather than "the bridge already served someone".
func TestMCPBridge_SurvivesClientDisconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	bridgeCfgDirs.Store(addr, cfgDir)
	go func() {
		done <- cmdMCPMemory(ctx, []string{
			"--backend", "echo", "--tools", "--sse-addr", addr,
			"--config", cfgDir, "--data", dataDir,
		})
	}()

	waitUp(t, addr)

	first := oneSession(t, addr)
	if len(first) == 0 {
		t.Fatal("first session listed no tools")
	}

	// The bug: everything below this line used to fail with connection-refused.
	second := oneSession(t, addr)
	if len(second) != len(first) {
		t.Errorf("second session saw %d tools, first saw %d", len(second), len(first))
	}
	third := oneSession(t, addr)
	if len(third) == 0 {
		t.Fatal("third session listed no tools")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bridge returned an error on shutdown: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("bridge did not shut down when its context was cancelled")
	}
}

// Sub-agent spawning has to be reachable THROUGH the bridge, not merely
// registered in some mux: serve and the TUI register the team tools on a
// per-agent local mux, and mcp-memory built no team runtime at all, so
// `orchestrator` and `team` were absent over MCP no matter which allow flags
// were passed. Reading the registration code proves nothing here; only the
// wire does.
func TestMCPBridge_ExposesTeamTools(t *testing.T) {
	addr := startBridge(t)

	got := map[string]bool{}
	for _, n := range oneSession(t, addr) {
		got[n] = true
	}
	for _, want := range []string{"spawn_agent", "team_await", "team_send", "team_roster"} {
		if !got[want] {
			t.Errorf("tool %q missing over the bridge; a claudecode fallback could read everything and delegate nothing", want)
		}
	}
}

// A second client while a session is live is refused, and the refusal must
// not end the live session: its POSTs still reach it.
func TestMCPBridge_RefusedSecondClientKeepsTheLiveSession(t *testing.T) {
	addr := startBridge(t)
	ctx := context.Background()
	live := bridgeDo(t, ctx, http.MethodGet, addr, "/sse", "")
	defer live.Body.Close()
	br := bufio.NewReader(live.Body)
	msgPath := ""
	for msgPath == "" {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read endpoint event: %v", err)
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			msgPath = strings.TrimSpace(v)
		}
	}

	second := bridgeDo(t, ctx, http.MethodGet, addr, "/sse", "")
	second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second concurrent stream = %d, want 409", second.StatusCode)
	}

	r := bridgeDo(t, ctx, http.MethodPost, addr, msgPath,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	r.Body.Close()
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("live session's post after the refusal = %d, want 202", r.StatusCode)
	}
}

// startBridge runs the MCP bridge on a free loopback port until the test ends.
func startBridge(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	cfgDir, dataDir := t.TempDir(), t.TempDir()
	bridgeCfgDirs.Store(addr, cfgDir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// Registered after the temp dirs, so it runs first: the server must be
	// down before its dirs are removed, or its shutdown writes race RemoveAll.
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		_ = cmdMCPMemory(ctx, []string{
			"--backend", "echo", "--tools", "--sse-addr", addr,
			"--config", cfgDir, "--data", dataDir,
		})
	}()
	waitUp(t, addr)
	return addr
}

func waitUp(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("bridge never bound %s", addr))
}
