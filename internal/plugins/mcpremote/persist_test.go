package mcpremote

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
	"tenant/internal/model"
)

// TestPersistentHandler_ServesCachedToken proves the "survives restart" core:
// a persisted token is loaded and served (as the bearer) WITHOUT a browser.
func TestPersistentHandler_ServesCachedToken(t *testing.T) {
	dir := t.TempDir()
	const server = "https://srv.example/v1/mcp"
	path := tokenCachePath(dir, server)
	if err := saveTokenCache(path, &tokenCache{
		ServerURL: server,
		ClientID:  "client-x",
		AuthURL:   "https://as.example/authorize",
		TokenURL:  "https://as.example/token",
		Token:     &oauth2.Token{AccessToken: "cached-abc", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	// 0600 perms on the cache file.
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("token cache perms = %v, want 0600", fi.Mode().Perm())
	}

	h := newPersistentHandler(server, "http://127.0.0.1:8765/callback", "Tenant", path, false, nil, http.DefaultClient)
	ts, err := h.TokenSource(context.Background())
	if err != nil || ts == nil {
		t.Fatalf("expected a token source from cache: ts=%v err=%v", ts, err)
	}
	tok, err := ts.Token() // unexpired ⇒ returned as-is, no network
	if err != nil || tok.AccessToken != "cached-abc" {
		t.Fatalf("cached token not served: tok=%v err=%v", tok, err)
	}
}

// TestPersistentHandler_NonInteractiveFailsClosed proves a launch reconnect with
// no usable cache fails cleanly and NEVER opens a browser.
func TestPersistentHandler_NonInteractiveFailsClosed(t *testing.T) {
	dir := t.TempDir()
	const server = "https://srv.example/v1/mcp"
	h := newPersistentHandler(server, "http://127.0.0.1:8765/callback", "Tenant", tokenCachePath(dir, server), false, nil, http.DefaultClient)

	ts, err := h.TokenSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ts != nil {
		t.Error("no cache ⇒ TokenSource must be nil (so the transport will call Authorize)")
	}
	if err := h.Authorize(context.Background(), nil, nil); !errors.Is(err, errNoCachedSession) {
		t.Errorf("non-interactive Authorize must fail closed with errNoCachedSession, got %v", err)
	}
}

// TestSilentReconnect_FromCachedToken is the end-to-end proof that a persisted
// token reconnects an MCP session — connect → tools/list → tools/call — with NO
// browser. A fake MCP server (go-sdk) requires the cached bearer.
func TestSilentReconnect_FromCachedToken(t *testing.T) {
	const bearer = "cached-access-xyz"
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ping",
		Description: "returns pong",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, struct{}{}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	})
	ms := httptest.NewServer(authed)
	defer ms.Close()
	serverURL := ms.URL

	// Pre-seed the cache as if a prior browser auth had persisted the token.
	dir := t.TempDir()
	if err := saveTokenCache(tokenCachePath(dir, serverURL), &tokenCache{
		ServerURL: serverURL,
		ClientID:  "client-x",
		AuthURL:   "https://unused/authorize",
		TokenURL:  "https://unused/token",
		Token:     &oauth2.Token{AccessToken: bearer, TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}

	// Silent (non-interactive) reconnect — MUST NOT need a browser.
	disp, cleanup, err := Open(context.Background(), Config{
		ServerURL: serverURL, Label: "mcp:test", CacheDir: dir, Interactive: false,
	}, true /* trustAnnotations ⇒ the read-only tool is ungated */, Policy{})
	if err != nil {
		t.Fatalf("silent reconnect from cached token failed: %v", err)
	}
	defer cleanup()

	found := false
	for _, ts := range disp.Tools() {
		if ts.Name == "ping" {
			found = true
		}
	}
	if !found {
		t.Fatal("ping tool not surfaced after silent reconnect")
	}
	out, isErr, err := disp.Dispatch(context.Background(), model.ToolCall{Name: "ping"})
	if err != nil || isErr || !strings.Contains(out, "pong") {
		t.Fatalf("dispatch over reconnected session: out=%q isErr=%v err=%v", out, isErr, err)
	}
}
