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
	// OnAuthChange fires once when the connector's OAuth token dies (the refresh
	// grant failed with nothing valid left) and once when it is authorized
	// again — the hook for a feed line / operator notification. Nil = none.
	OnAuthChange func(needsReauth bool, err string)
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
func connect(ctx context.Context, cfg Config) (*mcp.ClientSession, *persistentHandler, func(), error) {
	if cfg.ServerURL == "" {
		return nil, nil, nil, fmt.Errorf("mcpremote: ServerURL required")
	}
	redirect := "http://" + cfg.callbackAddr() + "/callback"
	// A shared client whose transport reconciles CDN-fronted AS-metadata issuers
	// (see issuerfix.go) — used for both the OAuth metadata/token requests and
	// the MCP traffic itself.
	httpClient := newRewritingClient()

	// The callback listener is only needed for an interactive browser flow, and
	// even then only WHILE a sign-in runs: the lazy fetcher binds the port when
	// the server answers 401 and releases it once the code is in hand, so an
	// idle connector holds no port and a long-running hub can add connectors
	// indefinitely. A non-interactive (launch-time) reconnect uses the cached
	// token and never opens a browser.
	var fetcher auth.AuthorizationCodeFetcher
	if cfg.Interactive {
		fetcher = newLazyCodeFetcher(cfg.callbackAddr(), cfg.OpenBrowser)
	}

	handler := newPersistentHandler(
		cfg.ServerURL, redirect, cfg.clientName(),
		tokenCachePath(cfg.CacheDir, cfg.ServerURL),
		cfg.Interactive, fetcher, httpClient, cfg.Logger,
	)
	handler.onAuthChange = cfg.OnAuthChange
	transport := &mcp.StreamableClientTransport{
		Endpoint:   cfg.ServerURL,
		HTTPClient: httpClient,
		// Disable the standalone SSE stream (the persistent server→client hanging
		// GET). Tenant is a request/response tool client — it calls tools/list +
		// tools/call and never consumes server-initiated push (notifications,
		// sampling). That stream is pure liability: it dies on laptop sleep / long
		// idle, the SDK burns MaxRetries (5) reconnecting it, then `c.fail()`s the
		// ENTIRE session ("standalone SSE stream: exceeded 5 retries without
		// progress") — so every tool call fails until a full re-connect. Disabling
		// it makes each tool call an independent POST that re-auths via the OAuth
		// handler per request, so the connection survives sleep. (TEN-180)
		DisableStandaloneSSE: true,
		OAuthHandler:         handler,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "tenant", Version: "0.1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mcpremote: connect %s: %w", cfg.ServerURL, err)
	}
	cleanup := func() { _ = session.Close() }
	return session, handler, cleanup, nil
}

// Open connects and builds a gated Dispatcher over the remote server's tools.
// trustAnnotations relaxes the deny-by-default gate for tools the server marks
// read-only; when false, EVERY remote tool is treated as a gated write.
func Open(ctx context.Context, cfg Config, trustAnnotations bool, policy Policy) (*Dispatcher, func(), error) {
	session, handler, cleanup, err := connect(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	// nil ungate ⇒ pure annotation rule (TEN-252): the remote-MCP/Atlassian path
	// keeps honoring --mcp-trust-annotations exactly as before.
	d, err := newDispatcher(ctx, cfg.Label, session, trustAnnotations, nil, policy)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("mcpremote: list tools: %w", err)
	}
	d.auth = handler // lets Dispatch/AuthHealth read the connector's auth state
	return d, cleanup, nil
}
