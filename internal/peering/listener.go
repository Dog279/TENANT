package peering

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// PeerProtocolVersion is the federation capability/envelope version exchanged
// at connect (the peer_hello handshake). A dialing peer (TEN-186) compares it
// and refuses or downgrades LOUDLY on a mismatch — never silently.
const PeerProtocolVersion = 1

// MaxSnippetBytes caps any single snippet a peer knowledge tool returns
// (TEN-186), so a peer can't pull a 40KB chunk into a small model's context.
const MaxSnippetBytes = 4096

// PeerContext is the authenticated peer's identity + capabilities, carried into
// the per-request scoped server and its tool handlers. Built from the verified
// bearer; all-deny by default.
type PeerContext struct {
	Name       string
	InstanceID string
	// Share is the policy SNAPSHOT taken at session creation. Use it only to
	// decide which tools to expose in tools/list. Do NOT gate ENFORCEMENT on it
	// — see CurrentShare.
	Share SharePolicy
	store *Store // for the live re-check at call time
}

// CurrentShare reads the LIVE share policy from the store at CALL time. The
// go-sdk streamable handler caches the scoped server (getServer runs ONCE per
// session, not per request), so a share DOWNGRADE (`tenant peer share … off`)
// only lands mid-session if tool handlers consult this at call time rather than
// gating on the connect-time Share snapshot. Falls back to the snapshot if the
// peer record vanished. TEN-186's knowledge tools MUST gate on CurrentShare().
func (pc PeerContext) CurrentShare() SharePolicy {
	if pc.store != nil {
		if p, ok := pc.store.Get(pc.Name); ok {
			return p.Share
		}
	}
	return pc.Share
}

// ToolRegistrar adds this side's peer tools onto a per-peer scoped mcp.Server.
// The knowledge tools (peer_wiki_search / peer_memory_search) are injected here
// by cmd/tenant in TEN-186; TEN-184 ships only the built-in peer_hello
// handshake. nil ⇒ hello-only.
//
// CONTRACT: a tool handler MUST re-check pc.CurrentShare() at CALL time (not the
// connect-time pc.Share) so a capability downgrade lands without waiting for the
// peer to reconnect — matching the revoke-lands-next-call guarantee.
type ToolRegistrar func(s *mcp.Server, pc PeerContext)

// Listener is the in-process go-sdk streamable-HTTP peer server. It lives in
// the main interactive run path (the only process holding the live bus,
// approval broker, and single-writer stores). Auth is a bearer verifier over
// peers.json; each request gets a per-peer SCOPED mcp.Server (no live toolMux
// passthrough — only the enumerated peer tools).
type Listener struct {
	store        *Store
	selfID       string           // our instance_id (returned in peer_hello / pairing)
	selfName     string           // our self-name (returned in pairing; TEN-239)
	selfVersion  string           // our binary/library version (returned in peer_hello)
	selfFinger   string           // our cert fingerprint (returned in pairing so the dialer pins it)
	overlay      bool             // peer.transport == "overlay": plain HTTP allowed on the overlay iface
	tlsCert      *tls.Certificate // when set, the listener serves HTTPS (TEN-185 pinned-cert)
	registrar    ToolRegistrar
	pairApprover func(ctx context.Context, prompt string) bool // TEN-239: nil ⇒ /pair disabled
	onAuth       func(peerName string)                         // TEN-250: inbound liveness; nil ⇒ no-op
	onGrant      func(peerName string, grant []string)         // TEN-253: caller-announced grant; nil ⇒ no-op
	pairLimiter  *pairLimiter
	log          func(format string, args ...any)

	srv *http.Server
}

// ListenerConfig configures a Listener.
type ListenerConfig struct {
	Store       *Store
	SelfID      string
	SelfName    string // self-name returned to a pairing inviter (TEN-239)
	SelfVersion string
	SelfFinger  string           // our cert fingerprint (TEN-239 pairing response); empty under overlay
	Overlay     bool             // declared overlay network (Tailscale/WireGuard): plain HTTP permitted
	TLSCert     *tls.Certificate // self-signed cert (TEN-185); when set, serve HTTPS + a non-loopback bind is allowed
	Registrar   ToolRegistrar    // nil ⇒ peer_hello only
	// PairApprover gates inbound push-invite pairing (TEN-239): it raises an
	// Approve/Deny prompt (carrying the PIN in `prompt`) and returns true to
	// approve. nil ⇒ the /pair endpoint is disabled (503).
	PairApprover func(ctx context.Context, prompt string) bool
	// OnAuth fires after a peer successfully authenticates a request (TEN-250
	// liveness): the peer's name, for the inbound last-seen signal in /peer. Runs
	// on the request goroutine, so it must be cheap + non-blocking. nil ⇒ no-op.
	OnAuth func(peerName string)
	// OnGrant fires when a caller announces its grant to us in peer_hello
	// (TEN-253), so an acceptor can display "this peer grants us X". Cheap +
	// non-blocking; nil ⇒ no-op.
	OnGrant func(peerName string, grant []string)
	Logger  func(string, ...any)
}

// NewListener builds a Listener. It does not bind until Serve.
func NewListener(cfg ListenerConfig) (*Listener, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("peering: listener requires a peer store")
	}
	log := cfg.Logger
	if log == nil {
		log = func(string, ...any) {}
	}
	ver := cfg.SelfVersion
	if ver == "" {
		ver = "dev"
	}
	return &Listener{
		store:        cfg.Store,
		selfID:       cfg.SelfID,
		selfName:     cfg.SelfName,
		selfVersion:  ver,
		selfFinger:   cfg.SelfFinger,
		overlay:      cfg.Overlay,
		tlsCert:      cfg.TLSCert,
		registrar:    cfg.Registrar,
		pairApprover: cfg.PairApprover,
		onAuth:       cfg.OnAuth,
		onGrant:      cfg.OnGrant,
		pairLimiter:  &pairLimiter{max: 3},
		log:          log,
	}, nil
}

// maxPeerRequestBytes bounds a peer's MCP request body before the go-sdk decodes
// it — a DoS guard so a (compromised) authenticated peer can't OOM the listener
// with a multi-MB JSON blob (e.g. a pathological peer_hello grant array) before
// any application-level length check runs (TEN-253). 1 MiB is generous: peer_hello
// is tiny and knowledge-tool calls are small query strings.
const maxPeerRequestBytes = 1 << 20

// Handler builds the authenticated streamable-HTTP handler. Exposed separately
// from Serve so tests can mount it on an httptest server.
func (l *Listener) Handler() http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(l.getServer, nil)
	authed := auth.RequireBearerToken(l.verify, nil)(mcpHandler)
	limited := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxPeerRequestBytes)
		authed.ServeHTTP(w, r)
	})
	mux := http.NewServeMux()
	// /pair is UNAUTHENTICATED by necessity (no shared secret yet) but only ever
	// creates a pending operator approval (TEN-239); everything else is the
	// bearer-gated MCP surface (body-size-limited above).
	mux.HandleFunc(pairPath, l.handlePair)
	mux.Handle("/", limited)
	return mux
}

// verify is the go-sdk TokenVerifier over peers.json. It re-reads the store if
// the file changed (so an out-of-process revoke/rotate lands on THIS call),
// constant-time-matches the bearer, advances pairing lifecycle (mark-used /
// confirm-rotation), and returns a TokenInfo carrying the peer identity +
// share policy for the per-peer scoped server.
func (l *Listener) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	if err := l.store.ReloadIfChanged(); err != nil {
		l.log("peer listener: peers.json reload failed: %v", err)
		// fall through with the in-memory view rather than failing closed-open;
		// a stale-but-present token still authenticates, revoke still lands once
		// the reload succeeds. (A hard reload error is rare — disk issue.)
	}
	p, matchedPending, ok := l.store.VerifyToken(token)
	if !ok {
		return nil, auth.ErrInvalidToken
	}
	// First successful auth retires the unused-invite bound; a pending-token
	// presentation confirms the staged rotation. Both are no-ops after the
	// transition, so this is cheap per-request.
	_ = l.store.MarkAuthenticated(p.Name)
	if matchedPending {
		_ = l.store.ConfirmRotation(p.Name)
	}
	if l.onAuth != nil {
		l.onAuth(p.Name) // TEN-250: inbound last-seen (cheap, non-blocking)
	}
	return &auth.TokenInfo{
		UserID:     p.Name,
		Expiration: time.Now().Add(24 * time.Hour), // go-sdk requires a non-zero expiry
		Extra: map[string]any{
			"instance_id": p.InstanceID,
			"share":       p.Share,
		},
	}, nil
}

// getServer yields the per-peer SCOPED mcp.Server for one request. It reads the
// authenticated identity the auth middleware stashed in the context, registers
// the built-in handshake tool, then lets the injected registrar add the
// share-gated knowledge tools (TEN-186). A request with no identity (should be
// impossible behind RequireBearerToken) gets an empty, tool-less server.
func (l *Listener) getServer(req *http.Request) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "tenant-peer",
		Version: fmt.Sprintf("%s (peer-proto %d)", l.selfVersion, PeerProtocolVersion),
	}, nil)

	pc, ok := peerContextFrom(req.Context())
	if !ok {
		return s // no identity → no tools
	}
	pc.store = l.store // enable CurrentShare() live re-check inside handlers
	l.registerHello(s, pc)
	if l.registrar != nil {
		l.registrar(s, pc)
	}
	return s
}

// peerContextFrom reconstructs the PeerContext from the auth TokenInfo the
// RequireBearerToken middleware put in the request context.
func peerContextFrom(ctx context.Context) (PeerContext, bool) {
	ti := auth.TokenInfoFromContext(ctx)
	if ti == nil {
		return PeerContext{}, false
	}
	pc := PeerContext{Name: ti.UserID}
	if ti.Extra != nil {
		if id, _ := ti.Extra["instance_id"].(string); id != "" {
			pc.InstanceID = id
		}
		if sp, ok := ti.Extra["share"].(SharePolicy); ok {
			pc.Share = sp
		}
	}
	return pc, true
}

// helloArgs is the OPTIONAL input a caller sends with peer_hello (TEN-253): its
// OWN grant to this server, so an ACCEPTOR (which never dials the caller back)
// can still display "what this peer grants us". Display-only — it never changes
// gating, which each side enforces from its own CurrentShare at call time.
type helloArgs struct {
	Grant []string `json:"grant,omitempty"`
}

// helloResult is the capability stamp a peer fetches at connect.
type helloResult struct {
	InstanceID      string   `json:"instance_id"`
	Version         string   `json:"version"`
	ProtocolVersion int      `json:"protocol_version"`
	Capabilities    []string `json:"capabilities"`
	You             string   `json:"you"` // the calling peer's name, echoed (confirms scoping)
}

// registerHello adds the built-in peer_hello handshake tool. It returns our
// identity + protocol version + the capabilities this peer is granted (its
// share policy) — the dialing side compares ProtocolVersion and refuses/downgrades
// loudly on mismatch (TEN-186).
func (l *Listener) registerHello(s *mcp.Server, pc PeerContext) {
	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "peer_hello",
			Description: "federation handshake: version + capability stamp",
			// Read-only: returns a stamp, no side effects. Lets a trusting dialer
			// treat it as ungated (TEN-251) — it's a liveness/handshake probe.
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		},
		func(_ context.Context, _ *mcp.CallToolRequest, args helloArgs) (*mcp.CallToolResult, helloResult, error) {
			// The caller may announce ITS grant to us (TEN-253) so we can show
			// "this peer grants us X" even though we never dial it back. A non-nil
			// (even empty) grant is a real announce — "shares nothing" → "(none)",
			// distinct from nil (an old peer that announced nothing → "unknown").
			if l.onGrant != nil && args.Grant != nil {
				l.onGrant(pc.Name, args.Grant)
			}
			out := helloResult{
				InstanceID:      l.selfID,
				Version:         l.selfVersion,
				ProtocolVersion: PeerProtocolVersion,
				Capabilities:    grantedCapabilities(pc.CurrentShare()), // live, not the connect-time snapshot
				You:             pc.Name,
			}
			return nil, out, nil
		},
	)
}

// Capabilities is the exported form of grantedCapabilities — the list of share
// flags this policy currently enables (for the TEN-253 grant announce).
func (sp SharePolicy) Capabilities() []string { return grantedCapabilities(sp) }

// grantedCapabilities lists the share flags currently enabled for a peer.
func grantedCapabilities(sp SharePolicy) []string {
	caps := []string{}
	for _, f := range []struct {
		k string
		v bool
	}{{"wiki", sp.Wiki}, {"memory", sp.Memory}, {"skills", sp.Skills}, {"exec", sp.Exec}, {"llm", sp.LLM}} {
		if f.v {
			caps = append(caps, f.k)
		}
	}
	return caps
}

// CheckBindPolicy enforces the secure-by-default bind rule: a non-loopback
// address requires TLS or declared overlay mode. Loopback is always allowed
// (testing/local).
//
// An EMPTY host (":9100" or "") means ALL interfaces in Go's net semantics —
// the same exposure as 0.0.0.0 — so it is refused without TLS/overlay, NOT
// treated as loopback. (This is the idiomatic "listen on port N" form, so the
// refusal names the safe alternatives explicitly.)
func CheckBindPolicy(addr string, overlay, hasTLS bool) error {
	if overlay || hasTLS {
		return nil
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if isLoopbackHost(host) {
		return nil
	}
	if host == "" {
		return fmt.Errorf("refusing to bind ALL interfaces %q without TLS or overlay — "+
			"use 127.0.0.1:PORT for local-only, enable a peer cert (TLS), or set peer.transport: overlay (Tailscale/WireGuard)", addr)
	}
	return fmt.Errorf("refusing to bind non-loopback address %q without TLS or overlay — "+
		"enable a peer cert (TLS) or set peer.transport: overlay (Tailscale/WireGuard)", addr)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ListenAndServe binds addr (enforcing the bind policy) and serves until ctx is
// cancelled. Callers that want a bound address synchronously should Bind + Serve.
func (l *Listener) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := l.Bind(addr)
	if err != nil {
		return err
	}
	return l.Serve(ctx, ln)
}

// Bind enforces the bind policy and returns a bound net.Listener (or an error)
// — letting the caller report the real address before serving in a goroutine.
// When a TLS cert is configured the listener is wrapped for HTTPS, so peers
// dial https:// and pin the cert fingerprint (TEN-185).
func (l *Listener) Bind(addr string) (net.Listener, error) {
	if err := CheckBindPolicy(addr, l.overlay, l.tlsCert != nil); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("peering: bind %q: %w", addr, err)
	}
	if l.tlsCert != nil {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*l.tlsCert}})
	}
	return ln, nil
}

// Secure reports whether the listener serves over TLS (true) or plain HTTP
// (overlay / loopback). Used to choose the http vs https scheme in invites.
func (l *Listener) Secure() bool { return l.tlsCert != nil }

// Serve serves on an existing listener until ctx is cancelled, then shuts down
// gracefully.
func (l *Listener) Serve(ctx context.Context, ln net.Listener) error {
	l.srv = &http.Server{Handler: l.Handler()}
	l.log("peer listener: serving on %s", ln.Addr())
	srvDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = l.srv.Shutdown(shCtx)
		case <-srvDone:
			// Serve returned on its own (e.g. listener closed) — exit promptly
			// rather than parking on ctx for the rest of the process lifetime.
		}
	}()
	err := l.srv.Serve(ln)
	close(srvDone)
	if err != nil && !isClosed(err) {
		return err
	}
	return nil
}

func isClosed(err error) bool {
	return err == http.ErrServerClosed || strings.Contains(err.Error(), "use of closed network connection")
}
