package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// testSession is the sign-in cookie a signed-in browser sends in these
// tests; withSession starts it on s.
const testSession = "a-signed-in-browser"

func withSession(s *Server) *Server {
	s.sessMu.Lock()
	s.sessions = map[string]time.Time{testSession: time.Now().Add(time.Hour)}
	s.sessMu.Unlock()
	return s
}

func authedServer() *Server {
	return withSession(New(Config{Addr: "example.com", Auth: "s3cret"}, nil, nil, nil, nil, nil))
}

func TestCheckAuth_CookieAndExemptions(t *testing.T) {
	s := authedServer()
	check := func(setup func(*http.Request)) bool {
		r := httptest.NewRequest("GET", "/tools", nil)
		setup(r)
		return s.checkAuth(r)
	}
	if !check(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession}) }) {
		t.Error("valid cookie should authenticate")
	}
	if check(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: dashCookieName, Value: "s3cret"}) }) {
		t.Error("the key itself as the cookie must NOT authenticate: the cookie is a session id")
	}
	if check(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: dashCookieName, Value: "wrong"}) }) {
		t.Error("invalid cookie must NOT authenticate")
	}
	if !check(func(r *http.Request) { r.Header.Set("Authorization", "Bearer s3cret") }) {
		t.Error("valid bearer header should still authenticate (API clients unaffected)")
	}
	if check(func(r *http.Request) {}) {
		t.Error("no credential must be rejected")
	}
	for _, p := range []string{"/settings", "/auth/login", "/auth/logout"} {
		if r := httptest.NewRequest("GET", p, nil); !s.checkAuth(r) {
			t.Errorf("%s should be auth-exempt", p)
		}
	}
	if r := httptest.NewRequest("GET", "/ws?token=s3cret", nil); s.checkAuth(r) {
		t.Error("a key in the URL must not authenticate, /ws included (F2)")
	}
}

func TestCheckAuth_ClosedWhenNoToken(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil) // Auth=""
	if r := httptest.NewRequest("GET", "/tools", nil); s.checkAuth(r) {
		t.Error("no configured token must fail closed")
	}
}

func postForm(path string, vals url.Values) *http.Request {
	r := httptest.NewRequest("POST", path, strings.NewReader(vals.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestLogin_SetsCookieAndRedirects(t *testing.T) {
	s := authedServer()
	rec := httptest.NewRecorder()
	s.handleLogin(rec, postForm("/auth/login", url.Values{"token": {"s3cret"}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("want redirect to /, got %q", loc)
	}
	cs := rec.Result().Cookies()
	if len(cs) == 0 || cs[0].Name != dashCookieName || cs[0].Value == "" || cs[0].Value == "s3cret" || cs[0].MaxAge <= 0 {
		t.Fatalf("expected a %s session cookie (not the key) with an expiry, got %+v", dashCookieName, cs)
	}
	if !cs[0].HttpOnly || cs[0].SameSite != http.SameSiteStrictMode {
		t.Error("cookie must be HttpOnly + SameSite=Strict")
	}
}

func TestLogin_WrongTokenForbidden(t *testing.T) {
	s := authedServer()
	rec := httptest.NewRecorder()
	s.handleLogin(rec, postForm("/auth/login", url.Values{"token": {"nope"}}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 on invalid PSK, got %d", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("no cookie should be set on a bad login")
	}
	if !strings.Contains(rec.Body.String(), "That key didn&#39;t match.") {
		t.Error("expected the error re-rendered in the form")
	}
}

func TestSettings_RedirectsWhenAuthed(t *testing.T) {
	s := authedServer()
	r := httptest.NewRequest("GET", "/settings", nil)
	r.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession})
	rec := httptest.NewRecorder()
	s.handleSettingsPage(rec, r)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("an already-authed /settings should 303 to /, got %d", rec.Code)
	}
}

func TestSettings_RendersFormWhenUnauthed(t *testing.T) {
	s := authedServer()
	rec := httptest.NewRecorder()
	s.handleSettingsPage(rec, httptest.NewRequest("GET", "/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Paste your sign-in key. It's in the file dashboard-auth-token on the computer running Tenant.") {
		t.Error("expected the PSK entry form")
	}
}

func TestLogout_ClearsCookie(t *testing.T) {
	s := authedServer()
	rec := httptest.NewRecorder()
	s.handleLogout(rec, httptest.NewRequest("POST", "/auth/logout", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings" {
		t.Fatalf("logout should 303 to /settings, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	cs := rec.Result().Cookies()
	if len(cs) == 0 || cs[0].MaxAge >= 0 {
		t.Error("logout must expire the cookie (MaxAge < 0)")
	}
}

// REVSEC-3: the cookie other services on this host receive is a session id,
// not the key: it isn't a Bearer credential, it expires, and Log out ends it.
func TestSession_CookieIsNotTheKey(t *testing.T) {
	s := New(Config{Addr: "example.com", Auth: "s3cret"}, nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	s.handleLogin(rec, postForm("/auth/login", url.Values{"token": {"s3cret"}}))
	cs := rec.Result().Cookies()
	if len(cs) != 1 || cs[0].Value == "s3cret" || cs[0].MaxAge != int(sessionTTL/time.Second) {
		t.Fatalf("sign-in cookie = %+v, want a session id with an expiry", cs)
	}
	with := func(set func(*http.Request)) bool {
		r := httptest.NewRequest("GET", "/tools", nil)
		set(r)
		return s.checkAuth(r)
	}
	cookie := func(r *http.Request) { r.AddCookie(cs[0]) }
	if !with(cookie) {
		t.Fatal("the session cookie should sign the browser in")
	}
	if with(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+cs[0].Value) }) {
		t.Error("the session id must not work as a Bearer key")
	}
	r := httptest.NewRequest("POST", "/auth/logout", nil)
	r.AddCookie(cs[0])
	s.handleLogout(httptest.NewRecorder(), r)
	if with(cookie) {
		t.Error("Log out must end the session")
	}
	id := s.newSession()
	s.sessMu.Lock()
	s.sessions[id] = time.Now().Add(-time.Second)
	s.sessMu.Unlock()
	if with(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: dashCookieName, Value: id}) }) {
		t.Error("an expired session must not sign in")
	}
}

// TEN-403: after five wrong keys an address waits, doubling each time; a
// right key clears it.
func TestLogin_ThrottlesFailures(t *testing.T) {
	s := authedServer()
	now := time.Now()
	loginNow = func() time.Time { return now }
	defer func() { loginNow = time.Now }()
	try := func(token string) int {
		rec := httptest.NewRecorder()
		s.handleLogin(rec, postForm("/auth/login", url.Values{"token": {token}}))
		return rec.Code
	}
	for i := 0; i < loginFreeTries; i++ {
		if code := try("nope"); code != http.StatusForbidden {
			t.Fatalf("try %d: want 403, got %d", i+1, code)
		}
	}
	if code := try("nope"); code != http.StatusForbidden {
		t.Fatalf("the sixth wrong key is still compared (403), got %d", code)
	}
	if code := try("s3cret"); code != http.StatusTooManyRequests {
		t.Fatalf("during the wait even the right key is refused with 429, got %d", code)
	}
	now = now.Add(2 * time.Second)
	if code := try("nope"); code != http.StatusForbidden {
		t.Fatalf("after the wait a wrong key is compared again, got %d", code)
	}
	now = now.Add(time.Second) // the second wait is 2s: still waiting
	if code := try("nope"); code != http.StatusTooManyRequests {
		t.Fatalf("the wait doubled, want 429, got %d", code)
	}
	now = now.Add(2 * time.Second)
	if code := try("s3cret"); code != http.StatusSeeOther {
		t.Fatalf("the right key signs in after the wait, got %d", code)
	}
	if s.loginWait(postForm("/auth/login", nil).RemoteAddr) != 0 {
		t.Error("a right key clears the count")
	}
}

// TEN-415: the cookie signs page navigations and the page's own requests,
// not a fetch another site (or another port here) made the browser send.
func TestCheckAuth_CrossSiteCookieRefused(t *testing.T) {
	s := authedServer()
	check := func(site, mode string, bearer bool) bool {
		r := httptest.NewRequest("GET", "/api/status", nil)
		r.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession})
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		if mode != "" {
			r.Header.Set("Sec-Fetch-Mode", mode)
		}
		if bearer {
			r.Header.Set("Authorization", "Bearer s3cret")
		}
		return s.checkAuth(r)
	}
	for _, c := range []struct {
		site, mode string
		want       bool
	}{
		{"", "", true}, {"same-origin", "cors", true}, {"none", "navigate", true},
		{"same-site", "navigate", true}, {"cross-site", "navigate", true},
		{"same-site", "no-cors", false}, {"cross-site", "cors", false}, {"cross-site", "", false},
	} {
		if got := check(c.site, c.mode, false); got != c.want {
			t.Errorf("site %q mode %q: cookie accepted = %v, want %v", c.site, c.mode, got, c.want)
		}
	}
	if !check("cross-site", "cors", true) {
		t.Error("a bearer token is not a cookie: unaffected")
	}
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession})
	r.Header.Set("Sec-Fetch-Site", "same-site")
	if got := credential(r); got != "cross-site cookie" {
		t.Errorf("credential = %q, want the reason named", got)
	}
}
