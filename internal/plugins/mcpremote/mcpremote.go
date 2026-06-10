// Package mcpremote makes Tenant a CLIENT of a remote MCP server (TEN-162) —
// the "Claude Code connector" model: zero pre-created app, OAuth 2.1 + Dynamic
// Client Registration + browser consent, Streamable-HTTP transport, and the
// server's tools surfaced into Tenant's agent (deny-by-default gated). Built on
// the official go-sdk (which does DCR/PKCE/discovery/exchange/refresh); the only
// hand-roll is the browser + localhost-callback fetcher.
package mcpremote

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Config configures a remote MCP connection.
type Config struct {
	ServerURL    string             // e.g. https://mcp.atlassian.com/v1/mcp
	Label        string             // tool-group label in the mux (e.g. "atlassian-mcp")
	ClientName   string             // DCR client_name (default "Tenant")
	CallbackAddr string             // localhost callback host:port (default 127.0.0.1:8765)
	OpenBrowser  func(string) error // opens the consent URL (default: print only)
	// CacheDir, if set, persists the OAuth token (+ DCR client + endpoints) under
	// it (0600) so the server reconnects across restarts without a browser.
	CacheDir string
	// Interactive permits the browser sign-in flow. When false (launch-time
	// silent reconnect), a missing/expired cached token fails cleanly instead
	// of popping a browser.
	Interactive bool
	// Logger receives connect/refresh failures (TEN-166). Nil = discard.
	Logger *slog.Logger
}

func (c Config) callbackAddr() string {
	if c.CallbackAddr != "" {
		return c.CallbackAddr
	}
	return "127.0.0.1:8765"
}

func (c Config) clientName() string {
	if c.ClientName != "" {
		return c.ClientName
	}
	return "Tenant"
}

// connect authenticates (browser OAuth 2.1 + DCR) and opens an MCP session over
// Streamable HTTP. Returns the session + a cleanup that closes the session and
// the callback server.
func connect(ctx context.Context, cfg Config) (*mcp.ClientSession, func(), error) {
	if cfg.ServerURL == "" {
		return nil, nil, fmt.Errorf("mcpremote: ServerURL required")
	}
	redirect := "http://" + cfg.callbackAddr() + "/callback"
	// A shared client whose transport reconciles CDN-fronted AS-metadata issuers
	// (see issuerfix.go) — used for both the OAuth metadata/token requests and
	// the MCP traffic itself.
	httpClient := newRewritingClient()

	// The callback server (which binds a port) is only needed for the interactive
	// browser flow. A non-interactive (launch-time) reconnect uses the cached
	// token and never opens a browser.
	var fetcher auth.AuthorizationCodeFetcher
	stop := func() {}
	if cfg.Interactive {
		f, s, err := newCodeFetcher(cfg.callbackAddr(), cfg.OpenBrowser)
		if err != nil {
			return nil, nil, err
		}
		fetcher, stop = f, s
	}

	handler := newPersistentHandler(
		cfg.ServerURL, redirect, cfg.clientName(),
		tokenCachePath(cfg.CacheDir, cfg.ServerURL),
		cfg.Interactive, fetcher, httpClient, cfg.Logger,
	)
	transport := &mcp.StreamableClientTransport{
		Endpoint:     cfg.ServerURL,
		HTTPClient:   httpClient,
		OAuthHandler: handler,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "tenant", Version: "0.1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("mcpremote: connect %s: %w", cfg.ServerURL, err)
	}
	cleanup := func() {
		_ = session.Close()
		stop()
	}
	return session, cleanup, nil
}

// Open connects and builds a gated Dispatcher over the remote server's tools.
// trustAnnotations relaxes the deny-by-default gate for tools the server marks
// read-only; when false, EVERY remote tool is treated as a gated write.
func Open(ctx context.Context, cfg Config, trustAnnotations bool, policy Policy) (*Dispatcher, func(), error) {
	session, cleanup, err := connect(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	d, err := newDispatcher(ctx, cfg.Label, session, trustAnnotations, policy)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("mcpremote: list tools: %w", err)
	}
	return d, cleanup, nil
}
