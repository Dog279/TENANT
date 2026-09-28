package dashboard

// auth_pages.go is TEN-106: the PSK→cookie entry flow that lets the
// server-rendered pages authenticate browser navigation without a Bearer
// header. The sign-in page is self-contained (templates/signin.html, its own
// styles): nothing else is served before sign-in.

import (
	"crypto/subtle"
	"html/template"
	"net/http"
	"net/url"
	"strings"
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

// validCookie reports whether r carries a session cookie matching the PSK.
func (s *Server) validCookie(r *http.Request) bool {
	c, err := r.Cookie(dashCookieName)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.cfg.Auth)) == 1
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

// handleLogin validates the posted PSK in constant time, drops a session cookie,
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
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Auth)) != 1 {
		SignInFailed.Log(r.Context(), "remote", r.RemoteAddr)
		refused(r, "sign_in", "/auth/login")
		signInFailed(w, r, http.StatusForbidden, "That key didn't match. Check the file dashboard-auth-token on the computer running Tenant.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     dashCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Secure only when TLS is configured; loopback HTTP is the common case.
		Secure: s.cfg.TLSCert != "" && s.cfg.TLSKey != "",
	})
	_ = catalog.SignedIn.Audit(auditCtx(r), "remote", r.RemoteAddr, "actor", "operator", "surface", "dashboard")
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

// handleLogout clears the session cookie and returns to the settings page.
// Only a signed-in browser's sign-out is the operator's: the route is open to
// anyone, and a record naming the operator must not be one anyone can file.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	signedIn := s.validCookie(r)
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
