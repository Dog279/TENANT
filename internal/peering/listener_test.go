package peering

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// dialClient connects a go-sdk client to a peer listener test server using a
// static bearer (the TEN-186 StaticTokenHandler shape).
func dialClient(t *testing.T, ctx context.Context, url, token string) (*mcp.ClientSession, error) {
	t.Helper()
	httpClient := &http.Client{Transport: bearerRoundTripper{token: token, base: http.DefaultTransport}}
	transport := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: httpClient, DisableStandaloneSSE: true}
	client := mcp.NewClient(&mcp.Implementation{Name: "tenant", Version: "test"}, nil)
	return client.Connect(ctx, transport, nil)
}

// pairedListener sets up a hub store with one authenticated peer + a listener,
// returning the test server, the peer's token, and the store.
func pairedListener(t *testing.T, reg ToolRegistrar) (*httptest.Server, string, *Store) {
	t.Helper()
	dir := t.TempDir()
	store, _ := LoadStore(dir)
	if _, err := store.CreateInvite("hub", "hub-id", "https://hub", "", time.Hour, "spoke"); err != nil {
		t.Fatal(err)
	}
	p, _ := store.Get("spoke")
	l, err := NewListener(ListenerConfig{Store: store, SelfID: "hub-id", SelfVersion: "1.0", Registrar: reg})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(l.Handler()), p.Token, store
}

func TestListener_AuthAndHandshake(t *testing.T) {
	srv, token, _ := pairedListener(t, nil)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Good token connects; peer_hello returns our stamp + echoes the caller.
	sess, err := dialClient(t, ctx, srv.URL, token)
	if err != nil {
		t.Fatalf("connect with good token: %v", err)
	}
	defer sess.Close()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "peer_hello"})
	if err != nil {
		t.Fatalf("peer_hello: %v", err)
	}
	hr, _ := res.StructuredContent.(map[string]any)
	if hr["instance_id"] != "hub-id" {
		t.Errorf("hello instance_id = %v, want hub-id", hr["instance_id"])
	}
	if pv, _ := hr["protocol_version"].(float64); int(pv) != PeerProtocolVersion {
		t.Errorf("hello protocol_version = %v, want %d", hr["protocol_version"], PeerProtocolVersion)
	}
	if hr["you"] != "spoke" {
		t.Errorf("hello should echo the caller name 'spoke', got %v", hr["you"])
	}

	// Wrong token is refused at connect.
	if bad, err := dialClient(t, ctx, srv.URL, "nope"); err == nil {
		bad.Close()
		t.Error("connect with wrong token must fail")
	}
}

// TestListener_RevokeLandsNextCall is the load-bearing security test: an
// out-of-process revoke (peers.json rewritten) must reject the NEXT request,
// not the next restart — via the mtime-cached reload in verify().
func TestListener_RevokeLandsNextCall(t *testing.T) {
	srv, token, store := pairedListener(t, nil)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First connection works.
	sess, err := dialClient(t, ctx, srv.URL, token)
	if err != nil {
		t.Fatalf("precondition connect: %v", err)
	}
	sess.Close()

	// Revoke via the store (simulating the `tenant peer revoke` CLI writing
	// peers.json). Bump mtime explicitly — filesystem mtime resolution can be
	// coarse, and the listener shares this *Store in-test, but the reload path
	// is what production (separate process) exercises; force a fresh mtime so
	// ReloadIfChanged definitely re-reads.
	if _, err := store.Revoke("spoke"); err != nil {
		t.Fatal(err)
	}
	touch(t, store.path)

	// A fresh connection with the now-revoked token must be refused.
	if bad, err := dialClient(t, ctx, srv.URL, token); err == nil {
		bad.Close()
		t.Error("revoked token must be rejected on the next call")
	}
}

// TestListener_ScopedShareGate proves the per-peer scoped server reflects the
// peer's share policy: a registrar that only adds a tool when memory=true sees
// the policy through PeerContext.
func TestListener_ScopedShareGate(t *testing.T) {
	dir := t.TempDir()
	store, _ := LoadStore(dir)
	store.CreateInvite("hub", "hub-id", "https://hub", "", time.Hour, "spoke")
	store.SetShare("spoke", "memory", true)
	p, _ := store.Get("spoke")

	reg := func(s *mcp.Server, pc PeerContext) {
		if pc.Share.Memory {
			mcp.AddTool(s, &mcp.Tool{Name: "peer_memory_search", Description: "scoped"},
				func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok for " + pc.Name}}}, nil, nil
				})
		}
	}
	l, _ := NewListener(ListenerConfig{Store: store, SelfID: "hub-id", SelfVersion: "1.0", Registrar: reg})
	srv := httptest.NewServer(l.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := dialClient(t, ctx, srv.URL, p.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	tools, _ := sess.ListTools(ctx, nil)
	hasMem := false
	for _, tl := range tools.Tools {
		if tl.Name == "peer_memory_search" {
			hasMem = true
		}
	}
	if !hasMem {
		t.Error("memory=true peer should see peer_memory_search in the scoped server")
	}
}

// TestPeerContext_CurrentShareIsLive proves the call-time share re-check sees a
// downgrade the connect-time snapshot would miss — the contract TEN-186's
// knowledge tools rely on so `share … off` lands without a reconnect.
func TestPeerContext_CurrentShareIsLive(t *testing.T) {
	dir := t.TempDir()
	store, _ := LoadStore(dir)
	store.CreateInvite("hub", "hub-id", "https://hub", "", time.Hour, "spoke")
	store.SetShare("spoke", "memory", true)

	pc := PeerContext{Name: "spoke", Share: SharePolicy{Memory: true}, store: store}
	if !pc.CurrentShare().Memory {
		t.Fatal("precondition: memory should be on")
	}
	// Downgrade in the store (as `tenant peer share spoke memory=off` would).
	store.SetShare("spoke", "memory", false)
	if pc.CurrentShare().Memory {
		t.Error("CurrentShare must reflect the live downgrade, not the connect-time snapshot")
	}
	if !pc.Share.Memory {
		t.Error("the connect-time snapshot is intentionally unchanged (used only for tools/list visibility)")
	}
}

func TestCheckBindPolicy(t *testing.T) {
	cases := []struct {
		addr    string
		overlay bool
		ok      bool
	}{
		{"127.0.0.1:9100", false, true},    // loopback always ok
		{"localhost:9100", false, true},    // loopback name ok
		{"[::1]:9100", false, true},        // ipv6 loopback ok
		{"0.0.0.0:9100", false, false},     // explicit all-interfaces w/o overlay → refused
		{":9100", false, false},            // EMPTY host = all interfaces → refused (idiomatic form)
		{"", false, false},                 // empty addr = all interfaces → refused
		{":9100", true, true},              // ...but allowed under declared overlay
		{"192.168.1.5:9100", false, false}, // LAN w/o overlay → refused
		{"192.168.1.5:9100", true, true},   // overlay declared → allowed
	}
	for _, tc := range cases {
		err := CheckBindPolicy(tc.addr, tc.overlay)
		if tc.ok && err != nil {
			t.Errorf("checkBindPolicy(%q, overlay=%v) = %v, want ok", tc.addr, tc.overlay, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("checkBindPolicy(%q, overlay=%v) = nil, want refusal", tc.addr, tc.overlay)
		}
	}
}

// touch bumps a file's mtime into the future so ReloadIfChanged definitely sees
// a change regardless of filesystem mtime granularity.
func touch(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}
