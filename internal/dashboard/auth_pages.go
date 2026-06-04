package dashboard

// auth_pages.go is TEN-106: the PSK→cookie entry flow that lets the (soon-to-be)
// server-rendered pages authenticate browser navigation without a Bearer header.
// The settings page is deliberately self-contained (its own inline html/template)
// so the auth gate ships ahead of the SSR template infrastructure (TEN-107).

import (
	"crypto/subtle"
	"html/template"
	"net/http"
)

// settingsTmpl is the standalone PSK-entry page. Dark palette matching the panel.
var settingsTmpl = template.Must(template.New("settings").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tenant · authenticate</title>
<style>
  :root{--bg:#0b1120;--card:#111a2e;--line:#27365a;--fg:#e6edf6;--dim:#8595b0;--cyan:#22d3ee;--violet:#8b5cf6;--red:#fb7185}
  *{box-sizing:border-box} html,body{height:100%;margin:0}
  body{background:var(--bg);color:var(--fg);font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;display:grid;place-items:center}
  .card{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:28px 30px;width:360px;max-width:92vw}
  h1{font-size:16px;margin:0 0 6px} p{color:var(--dim);margin:0 0 18px;font-size:13px}
  input{width:100%;border:1px solid var(--line);border-radius:10px;padding:11px 13px;color:var(--fg);background:#0a1322;font:inherit;outline:none}
  input:focus{border-color:var(--cyan)}
  button{margin-top:14px;width:100%;border:none;border-radius:10px;padding:11px;color:#04111c;background:linear-gradient(135deg,var(--cyan),var(--violet));cursor:pointer;font:inherit;font-weight:700}
  .err{color:var(--red);font-size:12.5px;margin-top:12px}
</style></head>
<body>
  <form class="card" method="POST" action="/auth/login">
    <h1>Authenticate</h1>
    <p>Paste the dashboard auth token printed in your terminal.</p>
    <input type="password" name="token" placeholder="auth token" autocomplete="off" autofocus>
    <button type="submit">Unlock</button>
    {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
  </form>
</body></html>`))

type settingsData struct{ Error string }

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
	_ = settingsTmpl.Execute(w, settingsData{})
}

// handleLogin validates the posted PSK in constant time, drops a session cookie,
// and redirects to the dashboard. A wrong PSK re-renders the form with a 403.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	token := r.FormValue("token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Auth)) != 1 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_ = settingsTmpl.Execute(w, settingsData{Error: "Invalid token — check the value printed in your terminal."})
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
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout clears the session cookie and returns to the settings page.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     dashCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
