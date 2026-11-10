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
// missing or wrong token yields 401 JSON, except a person opening a page, who
// is sent to the sign-in form (signInPage). GET /healthz is exempt so liveness
// probes work unauthenticated. checkBindPolicy refuses to start any listener,
// including loopback, when s.cfg.Auth is empty.
//
// CORS: same-origin only. A cross-origin request (Origin host != request Host)
// is rejected 403; same-origin and Origin-less (curl, native WebSocket)
// requests are allowed. No permissive Access-Control-Allow-Origin is emitted.
//
// Every answer refuses framing (F5) and runs no script but the dashboard's
// own (contentSecurityPolicy). Referrer-Policy is same-origin, not
// no-referrer: with no-referrer a browser sends "Origin: null" on a plain form
// POST, which checkOrigin rejects (the sign-in form included).
func (s *Server) secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Content-Security-Policy", contentSecurityPolicy)
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("X-Content-Type-Options", "nosniff")
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
			if signInPage(r) {
				http.Redirect(w, r, "/settings?"+url.Values{"next": {r.URL.RequestURI()}}.Encode(), http.StatusSeeOther)
				return
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
		dropForeignFlash(r)
		h.ServeHTTP(w, r)
	})
}

// dropForeignFlash takes the flash (?msg=, ?err=) off a page a browser opened
// from anywhere but this dashboard. A flash is the dashboard's own words after
// a form post (a same-origin redirect); from a link on another page, even one
// on this host's other ports (which the cookie reaches), it would put someone
// else's words in the dashboard's banner (REVSEC-4). A request without
// Sec-Fetch-Site (curl, an older browser) keeps it.
func dropForeignFlash(r *http.Request) {
	site := r.Header.Get("Sec-Fetch-Site")
	if site == "" || site == "same-origin" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return
	}
	q := r.URL.Query()
	if !q.Has("msg") && !q.Has("err") {
		return
	}
	q.Del("msg")
	q.Del("err")
	r.URL.RawQuery = q.Encode()
	r.Form = nil
}

// contentSecurityPolicy is D31: no page may be framed, and the only script
// that runs is /datastar.js from this dashboard, which evaluates its data-*
// expressions (unsafe-eval). The pages carry no inline script (the
// TestTemplates_NoInlineJS sweep), so script injected into a page, a
// javascript: link or an on*= handler never runs; nosniff keeps a log file
// or a JSON answer from being loaded as script.
//
// img-src and media-src: a page loads pictures, sound and video from this
// dashboard only. The pages show text a model wrote after reading mail, web
// pages and tool results; an image address in that text must not make the
// browser send a request somewhere, with no click, that no approval saw. The
// Markdown renderer already turns an image into a link (internal/md); this
// holds for every other way an <img> could reach a page.
const contentSecurityPolicy = "frame-ancestors 'none'; script-src 'self' 'unsafe-eval'; object-src 'none'; base-uri 'none'; img-src 'self' data:; media-src 'self'"

// signInPage reports whether an unauthorized request is a person opening a
// page, who is better sent to the sign-in form than shown JSON (F4, D30): a
// GET navigation that accepts HTML. Datastar's own fetches say text/html too,
// so they are told apart by their Datastar-Request header (and Sec-Fetch-Mode
// where the browser sends it); they, the API and the streams keep the 401.
func signInPage(r *http.Request) bool {
	mode := r.Header.Get("Sec-Fetch-Mode")
	return r.Method == http.MethodGet && r.Header.Get("Datastar-Request") == "" &&
		(mode == "" || mode == "navigate") && strings.Contains(r.Header.Get("Accept"), "text/html")
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
	if _, err := r.Cookie(dashCookieName); err == nil {
		if crossSiteFetch(r) {
			return "cross-site cookie"
		}
		return "invalid"
	}
	if r.Header.Get("Authorization") != "" || r.URL.Query().Has("token") {
		return "invalid"
	}
	return "missing"
}

// crossSiteFetch reports a request the browser says another site (or another
// port on this computer: same-site) started and that is not a page
// navigation — the kind the sign-in cookie must not sign (TEN-415). A request
// without the header (curl, an older browser) is not one.
func crossSiteFetch(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return (site == "cross-site" || site == "same-site") && r.Header.Get("Sec-Fetch-Mode") != "navigate"
}

// checkAuth reports whether r is authorized. The exempt liveness probe and
// authentication entry points remain reachable without credentials; all other
// requests require the configured token. Run refuses to start with no token.
//
// The token comes from `Authorization: Bearer <token>`, never from the URL
// (F2): the /ws `?token=` fallback is gone, since a same-origin browser
// handshake carries the cookie. dashCookieName carries a session id (not the
// key) after a successful /auth/login, so browser page navigations (which
// can't set an Authorization header) authenticate automatically. HttpOnly +
// SameSite=Strict; see auth_pages.go (TEN-106).
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
	// 1. Session cookie — SSR page loads, form submissions and the browser's
	// WebSocket handshake (browser sends it on same-origin). Not a fetch some
	// other site made the browser send it with (an <img> on another port of
	// this computer is enough to file a sensitive-read record, TEN-415).
	if s.validCookie(r) && !crossSiteFetch(r) {
		return true
	}
	// 2. Bearer header — API clients, curl (unchanged).
	return s.hasValidBearer(r)
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
	// A loopback listener answers to every loopback name on its port
	// (localhost, 127.0.0.1, [::1]): http://localhost:9770 is what people type
	// (TEN-414). Rebinding-safe: an attacker's name never resolves to one of
	// these in the Host header.
	if lh, lp, ok := splitAuthority(s.cfg.Addr); ok && isLoopbackHost(lh) {
		if rh, rp, rok := splitAuthority(requestHost); rok && isLoopbackHost(rh) && (lp == "" || lp == "0" || rp == lp) {
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
