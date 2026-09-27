package dashboard

// auth.go is TEN-79: the dashboard's security envelope — bearer-token auth,
// same-origin CORS, a modern TLS config, and a fail-closed bind policy. These
// compile against the Wave-1 Server (see the CONTRACT block in server.go) and
// are wired during integration: secure(s.mux) wraps the routed mux in Run(),
// checkBindPolicy() guards the top of Run() before Listen, and tlsConfig()
// selects ListenAndServeTLS vs ListenAndServe.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"tenant/internal/logs/catalog"
)

// secure wraps the routed handler with host validation, bearer-token auth,
// same-origin CORS, and CSRF protection for browser mutations.
//
// Auth: every non-health request must carry
// `Authorization: Bearer <token>` matching it (constant-time compare); a
// missing or wrong token yields 401 JSON. GET /healthz is exempt so liveness
// probes work unauthenticated. checkBindPolicy refuses to start any listener,
// including loopback, when s.cfg.Auth is empty.
//
// CORS: same-origin only. A cross-origin request (Origin host != request Host)
// is rejected 403; same-origin and Origin-less (curl, native WebSocket)
// requests are allowed. No permissive Access-Control-Allow-Origin is emitted.
func (s *Server) secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, route := r.Context(), s.route(r)
		if !s.checkHost(r.Host) {
			HostRejected.Log(ctx, "host", r.Host, "method", r.Method, "route", route, "remote", r.RemoteAddr)
			refused(r, "host", route)
			writeError(w, http.StatusForbidden, "unrecognized dashboard host")
			return
		}
		if !s.checkOrigin(r) {
			OriginRejected.Log(ctx, "origin", r.Header.Get("Origin"), "method", r.Method, "route", route, "remote", r.RemoteAddr)
			refused(r, "origin", route)
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		if !s.checkAuth(r) {
			cred := credential(r)
			AuthRejected.Log(ctx, "method", r.Method, "route", route, "remote", r.RemoteAddr, "credential", cred)
			if cred == "invalid" { // a failed token; none at all is only a visitor who hasn't signed in
				refused(r, "token", route)
			}
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		if !s.checkCSRF(r) {
			CSRFRejected.Log(ctx, "method", r.Method, "route", route, "remote", r.RemoteAddr)
			refused(r, "csrf", route)
			writeError(w, http.StatusForbidden, "CSRF validation failed")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// refused writes a refused request's Security record (4303), attributed to
// its remote address, before the response goes out: whole even when the
// client has gone (auditCtx).
func refused(r *http.Request, reason, route string) {
	_ = catalog.AccessRefused.Audit(auditCtx(r), "reason", reason, "remote", r.RemoteAddr, "method", r.Method, "route", route,
		"actor", r.RemoteAddr, "surface", "dashboard")
}

// auditCtx is r's context for a Security record: not cancelled when the
// client disconnects, since the record must be written whatever it does.
func auditCtx(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }

// credential says whether a rejected request carried a credential at all
// ("invalid") or none ("missing"), never what it was.
func credential(r *http.Request) string {
	if _, err := r.Cookie(dashCookieName); err == nil || r.Header.Get("Authorization") != "" || r.URL.Query().Has("token") {
		return "invalid"
	}
	return "missing"
}

// checkAuth reports whether r is authorized. The exempt liveness probe and
// authentication entry points remain reachable without credentials; all other
// requests require the configured token. Run refuses to start with no token.
//
// The token normally comes from `Authorization: Bearer <token>`. For the WS
// endpoint (GET /ws) ONLY, it may instead come from a `?token=<token>` query
// param: browsers cannot set the Authorization header on a WebSocket
// handshake, so the query param is the sole way a browser can authenticate the
// upgrade (TEN-84). The query fallback is deliberately scoped to /ws — REST
// paths (/api/*) honor the header only, to limit token-in-URL exposure.
// dashCookieName carries the PSK after a successful /auth/login, so browser
// page navigations (which can't set an Authorization header) authenticate
// automatically. HttpOnly + SameSite=Strict; see auth_pages.go (TEN-106).
const dashCookieName = "tenant-dash"

func (s *Server) checkAuth(r *http.Request) bool {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		return true
	}
	// Auth entry points are always reachable — that's where the PSK is presented.
	switch r.URL.Path {
	case "/settings", "/auth/login", "/auth/logout":
		return true
	}
	if s.cfg.Auth == "" {
		return false
	}
	// 1. Cookie — SSR page loads + form submissions (browser sends it on same-origin).
	if c, err := r.Cookie(dashCookieName); err == nil {
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.cfg.Auth)) == 1 {
			return true
		}
	}
	// 2. Bearer header — API clients, curl (unchanged).
	if got := bearerToken(r.Header.Get("Authorization")); got != "" {
		return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Auth)) == 1
	}
	// 3. Query param — WS upgrade only; browsers can't set headers on a WS handshake.
	if r.URL.Path == "/ws" {
		if t := r.URL.Query().Get("token"); t != "" {
			return subtle.ConstantTimeCompare([]byte(t), []byte(s.cfg.Auth)) == 1
		}
	}
	return false
}

// hasValidBearer reports whether the request explicitly authenticates with the
// dashboard token. Bearer-authenticated API mutations are not vulnerable to
// CSRF because browsers do not attach Authorization headers ambiently.
func (s *Server) hasValidBearer(r *http.Request) bool {
	got := bearerToken(r.Header.Get("Authorization"))
	return got != "" && s.cfg.Auth != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Auth)) == 1
}

// bearerToken returns the token from an `Authorization: Bearer <token>` header
// value, or "" if the value lacks the Bearer prefix.
func bearerToken(h string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}

// checkOrigin enforces same-origin against an already allowlisted request Host.
// Origin-less reads (curl, same-origin navigation) are allowed; unsafe browser
// requests are separately required to carry a matching Origin by checkCSRF.
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return sameAuthority(u.Host, r.Host)
}

// checkHost rejects Host values that were not derived from the configured
// listener or explicitly listed in AllowedHosts. This check is independent of
// Origin so an attacker cannot make both reflected values agree during DNS
// rebinding. An allowlist entry without a port — or a port of 0, i.e. an
// ephemeral ":0" listener that binds an OS-assigned port — permits that host on
// any port; entries with a concrete port require an exact authority match. The
// host part is always matched exactly, so port-wildcarding does not weaken
// rebinding protection (rebinding attacks the host, never the port).
func (s *Server) checkHost(requestHost string) bool {
	for _, allowed := range append([]string{s.cfg.Addr}, s.cfg.AllowedHosts...) {
		if authorityAllowed(requestHost, allowed) {
			return true
		}
	}
	s.trustedMu.RLock()
	defer s.trustedMu.RUnlock()
	for _, allowed := range s.trusted {
		if authorityAllowed(requestHost, allowed) {
			return true
		}
	}
	return false
}

// SetTrustedHosts replaces the Host authorities trusted at runtime in addition
// to the listener address and cfg.AllowedHosts, with the same matching rules.
// `tailscale serve` needs it: Tailscale's proxy forwards the browser's Host
// (node.tailnet.ts.net), which the listener-derived allowlist rejects with 403
// (TEN-332). Safe to call while serving; nil clears the set.
func (s *Server) SetTrustedHosts(hosts []string) {
	s.trustedMu.Lock()
	s.trusted = append([]string(nil), hosts...)
	s.trustedMu.Unlock()
}

func authorityAllowed(requestHost, allowed string) bool {
	rh, rp, rok := splitAuthority(requestHost)
	ah, ap, aok := splitAuthority(allowed)
	if !rok || !aok || !strings.EqualFold(rh, ah) {
		return false
	}
	return ap == "" || ap == "0" || strings.EqualFold(rp, ap)
}

func sameAuthority(a, b string) bool {
	ah, ap, aok := splitAuthority(a)
	bh, bp, bok := splitAuthority(b)
	return aok && bok && strings.EqualFold(ah, bh) && strings.EqualFold(ap, bp)
}

// splitAuthority parses an HTTP Host-style authority without accepting URL
// syntax, whitespace, userinfo, paths, or malformed IPv6 literals.
func splitAuthority(authority string) (host, port string, ok bool) {
	if authority == "" || strings.TrimSpace(authority) != authority || strings.ContainsAny(authority, "/@?#") {
		return "", "", false
	}
	if h, p, err := net.SplitHostPort(authority); err == nil {
		if h == "" || p == "" {
			return "", "", false
		}
		return strings.TrimSuffix(strings.ToLower(h), "."), p, true
	}
	if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		h := strings.TrimSuffix(strings.TrimPrefix(authority, "["), "]")
		if ip := net.ParseIP(h); ip != nil {
			return strings.ToLower(h), "", true
		}
		return "", "", false
	}
	if strings.Contains(authority, ":") {
		return "", "", false
	}
	host = strings.TrimSuffix(strings.ToLower(authority), ".")
	if host == "" {
		return "", "", false
	}
	return host, "", true
}

// checkCSRF requires browser-authenticated mutations to prove they originated
// at this dashboard. Modern browsers attach Origin to form and fetch POSTs;
// rejecting a missing Origin closes the origin-less loophole, while the
// allowlisted Host + exact Origin match prevents DNS rebinding. Explicit Bearer
// clients remain usable because their credential is never ambient.
func (s *Server) checkCSRF(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if s.hasValidBearer(r) {
		return true
	}
	return r.Header.Get("Origin") != "" && s.checkOrigin(r)
}

// checkBindPolicy is the fail-closed guard called at the top of Run() before
// Listen. Authentication is required on every listener, including loopback.
// Non-loopback listeners additionally require TLS.
func (s *Server) checkBindPolicy() error {
	if s.cfg.Auth == "" {
		return fmt.Errorf("dashboard requires an auth token, including on loopback")
	}
	host, _, err := net.SplitHostPort(s.cfg.Addr)
	if err != nil {
		// No port? Treat the whole value as the host (best-effort) so a
		// misconfig still gets the security check rather than silently passing.
		host = s.cfg.Addr
	}
	if isLoopbackHost(host) {
		return nil
	}
	if s.cfg.TLSCert != "" && s.cfg.TLSKey != "" {
		return nil
	}
	return fmt.Errorf("dashboard refuses to bind %q without TLS — set dashboard.tls_cert/tls_key, or bind 127.0.0.1", s.cfg.Addr)
}

// isLoopbackHost reports whether host names the local machine: the literal
// "localhost", an empty host (unspecified — but only loopback semantics here),
// or any IP in a loopback range (127.0.0.0/8, ::1).
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// tlsConfig builds the server's TLS configuration from the configured cert/key
// PEM paths, pinning a modern floor of TLS 1.2. It returns (nil, nil) when no
// cert/key is set, signaling the caller to serve plaintext (ListenAndServe).
func (s *Server) tlsConfig() (*tls.Config, error) {
	if s.cfg.TLSCert == "" || s.cfg.TLSKey == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(s.cfg.TLSCert, s.cfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("dashboard: load TLS keypair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// GenerateToken mints a cryptographically random, URL-safe bearer token (32
// bytes of crypto/rand, base64url, no padding) for first-run auth.
//
// This only produces the value; the orchestrator in cmd/tenant persists it
// (e.g. to the dashboard config / token file) and feeds it back as cfg.Auth.
func GenerateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read never returns an error on supported platforms;
		// panic rather than hand back a weak/empty token.
		panic("dashboard: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
