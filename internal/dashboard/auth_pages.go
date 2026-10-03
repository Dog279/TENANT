package dashboard

// auth_pages.go is TEN-106: the PSK→cookie entry flow that lets the
// server-rendered pages authenticate browser navigation without a Bearer
// header. The sign-in page is self-contained (templates/signin.html, its own
// styles): nothing else is served before sign-in.

import (
	"crypto/subtle"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"tenant/internal/logs/catalog"
)

// settingsTmpl is the sign-in page.
var settingsTmpl = template.Must(template.ParseFS(ssrFS, "templates/signin.html"))

type settingsData struct{ Error, Next string }

// safeNext is where sign-in returns to: next when it is a path on this
// dashboard, else "/". "//host" and "/\host" are other sites to a browser,
// and a control character can turn into one, so those go home too. The flash
// fields (msg, err) and token are dropped: a link someone else wrote must not
// put its words in the dashboard's own banner once the operator signs in.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsFunc(next, func(r rune) bool {
		return r == '\\' || unicode.IsControl(r)
	}) {
		return "/"
	}
	path, query, ok := strings.Cut(next, "?")
	if !ok {
		return next
	}
	q, _ := url.ParseQuery(query)
	q.Del("msg")
	q.Del("err")
	q.Del("token")
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// sessionTTL is how long a sign-in lasts. The cookie holds a random session
// id, never the key: cookies aren't kept apart by port, so every other
// service on this host (or on the tailnet name) receives it, and what it
// receives must not be the key that also works as a Bearer credential, must
// end, and must end on Log out. A restart ends every session.
const sessionTTL = 7 * 24 * time.Hour

// newSession starts a signed-in browser's session and returns its id.
func (s *Server) newSession() string {
	id, now := GenerateToken(), time.Now()
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]time.Time{}
	}
	for k, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = now.Add(sessionTTL)
	return id
}

// validSession reports whether id is a live session.
func (s *Server) validSession(id string) bool {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	exp, ok := s.sessions[id]
	return ok && time.Now().Before(exp)
}

// validCookie reports whether r carries a live session cookie.
func (s *Server) validCookie(r *http.Request) bool {
	c, err := r.Cookie(dashCookieName)
	return err == nil && s.validSession(c.Value)
}

// handleSettingsPage renders the PSK entry form. If auth is disabled or the
// caller already holds a valid cookie, it redirects straight to the dashboard.
func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth == "" || s.validCookie(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = settingsTmpl.Execute(w, settingsData{Next: r.URL.Query().Get("next")})
}

// signInFailed re-renders the sign-in form with a plain error.
func signInFailed(w http.ResponseWriter, r *http.Request, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = settingsTmpl.Execute(w, settingsData{Error: msg, Next: r.FormValue("next")})
}

// handleLogin validates the posted PSK in constant time, starts a session,
// and redirects to the dashboard. A wrong PSK re-renders the form with a 403.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// The key travels in the body only (F2): one in the URL is refused before
	// it's compared, so a link can never sign anyone in.
	token, ok := bodyOnly(r, "token")
	if !ok {
		KeyInURL.Log(r.Context(), "cred", "dashboard sign-in key")
		signInFailed(w, r, http.StatusBadRequest, "Paste the key into the form, not the address bar. The key in that address may now be in your browser's history: to change it, delete the file dashboard-auth-token and restart Tenant.")
		return
	}
	if wait := s.loginWait(r.RemoteAddr); wait > 0 {
		SignInFailed.Log(r.Context(), "remote", r.RemoteAddr, "throttled", wait.String())
		refused(r, "sign_in", "/auth/login")
		signInFailed(w, r, http.StatusTooManyRequests, fmt.Sprintf("Too many wrong keys from this address. Try again in %d seconds.", int(wait/time.Second)+1))
		return
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Auth)) != 1 {
		s.loginFailed(r.RemoteAddr)
		SignInFailed.Log(r.Context(), "remote", r.RemoteAddr)
		refused(r, "sign_in", "/auth/login")
		signInFailed(w, r, http.StatusForbidden, "That key didn't match. Check the file dashboard-auth-token on the computer running Tenant.")
		return
	}
	s.loginOK(r.RemoteAddr)
	http.SetCookie(w, &http.Cookie{
		Name:     dashCookieName,
		Value:    s.newSession(),
		Path:     "/",
		MaxAge:   int(sessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Secure only when TLS is configured; loopback HTTP is the common case.
		Secure: s.cfg.TLSCert != "" && s.cfg.TLSKey != "",
	})
	_ = catalog.SignedIn.Audit(auditCtx(r), "remote", r.RemoteAddr, "actor", "operator", "surface", "dashboard")
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

// handleLogout ends the session, clears its cookie and returns to the
// settings page.
// Only a signed-in browser's sign-out is the operator's: the route is open to
// anyone, and a record naming the operator must not be one anyone can file.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	signedIn := s.validCookie(r)
	if signedIn {
		c, _ := r.Cookie(dashCookieName)
		s.sessMu.Lock()
		delete(s.sessions, c.Value)
		s.sessMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     dashCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	if signedIn {
		_ = catalog.SignedOut.Audit(auditCtx(r), "remote", r.RemoteAddr, "actor", "operator", "surface", "dashboard")
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// Sign-in throttling (TEN-403): after loginFreeTries wrong keys from one
// address, each further wrong key doubles a wait, up to loginMaxWait, during
// which that address's attempts are refused before the key is compared. A
// right key clears the count. ponytail: keyed by the remote IP, so a shared
// NAT is one address; per-key hashing if that ever matters.
const (
	loginFreeTries = 5
	loginMaxWait   = time.Minute
)

// loginNow is swapped by tests.
var loginNow = time.Now

func remoteIP(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// loginWait is how long addr must still wait, or 0.
func (s *Server) loginWait(addr string) time.Duration {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	if f, ok := s.fails[remoteIP(addr)]; ok {
		if d := f.until.Sub(loginNow()); d > 0 {
			return d
		}
	}
	return 0
}

// loginFailed records a wrong key from addr and sets its wait.
func (s *Server) loginFailed(addr string) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	if s.fails == nil || len(s.fails) > 10000 {
		s.fails = map[string]loginFails{}
	}
	f := s.fails[remoteIP(addr)]
	f.n++
	if f.n > loginFreeTries {
		wait := time.Second << uint(min(f.n-loginFreeTries-1, 10))
		if wait > loginMaxWait {
			wait = loginMaxWait
		}
		f.until = loginNow().Add(wait)
	}
	s.fails[remoteIP(addr)] = f
}

// loginOK forgets addr's wrong keys.
func (s *Server) loginOK(addr string) {
	s.failMu.Lock()
	delete(s.fails, remoteIP(addr))
	s.failMu.Unlock()
}
