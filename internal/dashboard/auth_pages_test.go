package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func authedServer() *Server { return New(Config{Auth: "s3cret"}, nil, nil, nil, nil, nil) }

func TestCheckAuth_CookieAndExemptions(t *testing.T) {
	s := authedServer()
	check := func(setup func(*http.Request)) bool {
		r := httptest.NewRequest("GET", "/tools", nil)
		setup(r)
		return s.checkAuth(r)
	}
	if !check(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: dashCookieName, Value: "s3cret"}) }) {
		t.Error("valid cookie should authenticate")
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
	if r := httptest.NewRequest("GET", "/ws?token=s3cret", nil); !s.checkAuth(r) {
		t.Error("ws ?token= fallback should still authenticate")
	}
}

func TestCheckAuth_OpenWhenNoToken(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil) // Auth=""
	if r := httptest.NewRequest("GET", "/tools", nil); !s.checkAuth(r) {
		t.Error("no configured token = open (loopback-only enforced elsewhere)")
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
	if len(cs) == 0 || cs[0].Name != dashCookieName || cs[0].Value != "s3cret" {
		t.Fatalf("expected %s cookie set, got %+v", dashCookieName, cs)
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
	if !strings.Contains(rec.Body.String(), "Invalid token") {
		t.Error("expected the error re-rendered in the form")
	}
}

func TestSettings_RedirectsWhenAuthed(t *testing.T) {
	s := authedServer()
	r := httptest.NewRequest("GET", "/settings", nil)
	r.AddCookie(&http.Cookie{Name: dashCookieName, Value: "s3cret"})
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
	if !strings.Contains(rec.Body.String(), "auth token") {
		t.Error("expected the PSK entry form")
	}
}

func TestLogout_ClearsCookie(t *testing.T) {
	s := authedServer()
	rec := httptest.NewRecorder()
	s.handleLogout(rec, httptest.NewRequest("GET", "/auth/logout", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings" {
		t.Fatalf("logout should 303 to /settings, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	cs := rec.Result().Cookies()
	if len(cs) == 0 || cs[0].MaxAge >= 0 {
		t.Error("logout must expire the cookie (MaxAge < 0)")
	}
}
