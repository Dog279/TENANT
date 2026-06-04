package dashboard

// auth.go is TEN-79: the dashboard's security envelope — bearer-token auth,
// same-origin CORS, a modern TLS config, and a fail-closed bind policy. These
// compile against the Wave-1 Server (see the CONTRACT block in server.go) and
// are wired during integration: secure(s.mux) wraps the routed mux in Run(),
// checkBindPolicy() guards the top of Run() before Listen, and tlsConfig()
// selects ListenAndServeTLS vs ListenAndServe.

import (
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// secure wraps the routed handler with bearer-token auth and same-origin CORS.
//
// Auth: when s.cfg.Auth is set, every request must carry
// `Authorization: Bearer <token>` matching it (constant-time compare); a
// missing or wrong token yields 401 JSON. GET /healthz is exempt so liveness
// probes work unauthenticated. When s.cfg.Auth is empty, requests pass through
// — safe only because checkBindPolicy forbids exposing an unauthenticated
// server off-loopback.
//
// CORS: same-origin only. A cross-origin request (Origin host != request Host)
// is rejected 403; same-origin and Origin-less (curl, native WebSocket)
// requests are allowed. No permissive Access-Control-Allow-Origin is emitted.
func (s *Server) secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.checkOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		if !s.checkAuth(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// checkAuth reports whether r is authorized. No configured token, or the
// exempt liveness probe (GET /healthz), is always allowed; otherwise the
// presented token must match s.cfg.Auth, compared in constant time to avoid
// leaking the token via timing.
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
	if s.cfg.Auth == "" {
		return true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		return true
	}
	// Auth entry points are always reachable — that's where the PSK is presented.
	switch r.URL.Path {
	case "/settings", "/auth/login", "/auth/logout":
		return true
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

// bearerToken returns the token from an `Authorization: Bearer <token>` header
// value, or "" if the value lacks the Bearer prefix.
func bearerToken(h string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}

// checkOrigin enforces same-origin: an Origin-less request (curl, native WS,
// same-origin navigations that omit it) is allowed; otherwise the Origin URL's
// host must equal the request Host. A malformed Origin is rejected.
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// checkBindPolicy is the fail-closed guard called at the top of Run() before
// Listen. A loopback address (127.0.0.1, ::1, localhost) is always allowed so
// local dev may run plaintext and tokenless. Any non-loopback bind (0.0.0.0, a
// LAN IP, any other host) is refused unless BOTH TLS and an auth token are
// configured, so the dashboard can never be exposed off-host unprotected.
func (s *Server) checkBindPolicy() error {
	host, _, err := net.SplitHostPort(s.cfg.Addr)
	if err != nil {
		// No port? Treat the whole value as the host (best-effort) so a
		// misconfig still gets the security check rather than silently passing.
		host = s.cfg.Addr
	}
	if isLoopbackHost(host) {
		return nil
	}
	if s.cfg.TLSCert != "" && s.cfg.TLSKey != "" && s.cfg.Auth != "" {
		return nil
	}
	return fmt.Errorf("dashboard refuses to bind %q without TLS+auth — set dashboard.tls_cert/tls_key and an auth token, or bind 127.0.0.1", s.cfg.Addr)
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
