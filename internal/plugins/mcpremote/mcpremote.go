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
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tenant/internal/logs/event"
)

// Config configures a remote MCP connection.
type Config struct {
	ServerURL    string             // e.g. https://mcp.atlassian.com/v1/mcp
	Label        string             // tool-group label in the mux (e.g. "atlassian-mcp")
	ClientName   string             // DCR client_name (default "Tenant")
	CallbackAddr string             // localhost callback host:port (default 127.0.0.1:8765)
	OpenBrowser  func(string) error // opens the consent URL (default: print only)
	// CacheDir, if set, persists the OAuth token (+ DCR client + endpoints) under
	// it with owner-only protection so the server reconnects across restarts without a browser.
	CacheDir string
	// Interactive permits the browser sign-in flow while connecting. When
	// false (launch-time silent reconnect), and on every call after the
	// connect, a missing/expired cached token fails cleanly instead of popping
	// a browser.
	Interactive bool
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
// the callback server. ctx is the connection's: the handler files its sign-in
// events under it.
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
		fetcher = newLazyCodeFetcher(cfg.callbackAddr(), cfg.OpenBrowser, cfg.Label)
	}

	handler := newPersistentHandler(
		cfg.ServerURL, redirect, cfg.clientName(),
		tokenCachePath(cfg.CacheDir, cfg.ServerURL),
		cfg.Interactive, fetcher, httpClient, context.WithoutCancel(ctx), // outlives a bounded dial
	)
	handler.label = cfg.Label
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
	session, err := newClient(ctx, cfg.Label).Connect(ctx, transport, nil)
	// The browser is for this connect, which someone is waiting on. A sign-in
	// that dies later is announced and repaired with `tenant mcp connect`, not
	// signed in again by a browser opened in the middle of a task.
	handler.mu.Lock()
	handler.interactive = false
	handler.mu.Unlock()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mcpremote: connect %s: %w", cfg.ServerURL, err)
	}
	cleanup := func() { _ = session.Close() }
	return session, handler, cleanup, nil
}

// Open connects and builds a gated Dispatcher over the remote server's tools.
// trustAnnotations relaxes the deny-by-default gate for tools the server marks
// read-only; when false, EVERY remote tool is treated as a gated write. The
// connection is a Connection activity in the connector's log, from here to
// the cleanup.
func Open(ctx context.Context, cfg Config, trustAnnotations bool, policy Policy) (*Dispatcher, func(), error) {
	ctx, act := event.Start(WithServerLog(ctx, cfg.Label), Connection, "server", cfg.Label, "url", cfg.ServerURL, "interactive", cfg.Interactive)
	session, handler, cleanup, err := connect(ctx, cfg)
	if err != nil {
		if strings.Contains(err.Error(), errNoCachedSession.Error()) { // the SDK may not keep it wrapped
			NoSavedSignIn.Log(ctx, "server", cfg.Label, "url", cfg.ServerURL)
			act.End(nil) // nothing failed: a launch reconnect just can't sign in
		} else {
			act.End(&err)
		}
		return nil, nil, err
	}
	// nil ungate ⇒ pure annotation rule (TEN-252): the remote-MCP/Atlassian path
	// keeps honoring --mcp-trust-annotations exactly as before.
	d, err := newDispatcher(ctx, cfg.Label, session, trustAnnotations, nil, policy)
	if err != nil {
		cleanup()
		err = fmt.Errorf("mcpremote: list tools: %w", err)
		act.End(&err)
		return nil, nil, err
	}
	d.auth = handler // lets Dispatch/AuthHealth read the connector's auth state
	return d, d.live(ctx, act, cleanup), nil
}

// newClient is Tenant's MCP client for a connection to label, which files a
// server's announcement that its tools changed under ctx.
func newClient(ctx context.Context, label string) *mcp.Client {
	ctx = context.WithoutCancel(ctx)
	return mcp.NewClient(&mcp.Implementation{Name: "tenant", Version: "0.1"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			ToolsChanged.Log(ctx, "server", label)
		},
	})
}

// live finishes a connection that is up: it files Connected with the tools d
// lists, marks d's calls as its own to log, and returns the cleanup that
// closes the session and ends act, once.
func (d *Dispatcher) live(ctx context.Context, act *event.Activity, closeSession func()) func() {
	names := make([]string, 0, len(d.specs))
	for _, s := range d.specs {
		names = append(names, s.Name)
	}
	Connected.Log(ctx, "server", d.label, "tools", len(names), "names", strings.Join(names, ","))
	d.ownLog = true
	return sync.OnceFunc(func() {
		closeSession()
		act.End(nil)
	})
}
