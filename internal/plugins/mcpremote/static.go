package mcpremote

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// StaticConfig dials a peer MCP server (TEN-186) with a STATIC bearer token —
// the pre-exchanged pairing secret (TEN-183), NOT OAuth/DCR. The connection is
// plain Streamable-HTTP with the bearer injected per request, optionally over a
// fingerprint-pinned TLS link (TOFU-by-invite, TEN-185). This is the
// "StaticTokenHandler" the federation design calls for — deliberately NOT the
// issuer-rewriting OAuth client (that's an Atlassian-CDN workaround).
type StaticConfig struct {
	ServerURL string
	Token     string
	Label     string      // tool namespace, e.g. "peer:laptop"
	TLS       *tls.Config // nil ⇒ plain HTTP (overlay); else the pinned-cert config
}

// OpenStatic connects with a static bearer and returns a gated Dispatcher over
// the peer's tools. trustAnnotations is TRUE: a peer's read-only knowledge tools
// (peer_wiki_search / peer_memory_search) are read-safe on the CALLING side —
// the serving side's share policy is the gate (Premise 1). Returns the
// Dispatcher + a cleanup that closes the session.
func OpenStatic(ctx context.Context, cfg StaticConfig, policy Policy) (*Dispatcher, func(), error) {
	if cfg.ServerURL == "" || cfg.Token == "" {
		return nil, nil, fmt.Errorf("mcpremote: OpenStatic requires ServerURL + Token")
	}
	httpClient := newStaticClient(cfg.Token, cfg.TLS)
	transport := &mcp.StreamableClientTransport{
		Endpoint:             cfg.ServerURL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true, // TEN-180: request/response only, survives sleep
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "tenant", Version: "0.1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		// Release the transport's idle TCP/TLS connections — Connect created none
		// that a session would own, so without this an unreachable endpoint leaks
		// a connection per call (TEN-250: the heartbeat probes a dead peer every
		// 30s, which made this recurring rather than one-off).
		httpClient.CloseIdleConnections()
		return nil, nil, fmt.Errorf("mcpremote: connect peer %s: %w", cfg.ServerURL, err)
	}
	label := cfg.Label
	if label == "" {
		label = "peer"
	}
	d, err := newDispatcher(ctx, label, session, true, policy)
	if err != nil {
		_ = session.Close()
		httpClient.CloseIdleConnections()
		return nil, nil, fmt.Errorf("mcpremote: list peer tools: %w", err)
	}
	return d, func() { _ = session.Close() }, nil
}

// newStaticClient builds the pairing-token HTTP client: a bearer-injecting
// transport (optionally over pinned TLS) that REFUSES redirects. Refusing is
// load-bearing security, not just hygiene — staticBearer re-injects the bearer
// on every hop, which would defeat Go's built-in cross-host Authorization
// stripping, so a malicious peer could 302 the dial to an attacker host and
// capture the long-lived pairing token (cleartext under overlay/plain HTTP).
// MCP Streamable-HTTP is a fixed-endpoint protocol, so refusing breaks nothing.
func newStaticClient(token string, tlsCfg *tls.Config) *http.Client {
	base := &http.Transport{}
	if tlsCfg != nil {
		base.TLSClientConfig = tlsCfg
	}
	return &http.Client{
		Transport:     staticBearer{token: token, base: base},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// staticBearer injects a fixed Authorization header on every request — the
// minimal pairing-token client (no OAuth handler).
type staticBearer struct {
	token string
	base  http.RoundTripper
}

// CloseIdleConnections forwards to the base transport so http.Client's
// CloseIdleConnections() reaches the real connection pool (a custom
// RoundTripper otherwise hides it). Used to release conns when a dial fails.
func (s staticBearer) CloseIdleConnections() {
	if c, ok := s.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

func (s staticBearer) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+s.token)
	return s.base.RoundTrip(r)
}
