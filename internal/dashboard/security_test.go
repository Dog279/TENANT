package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// security renders the Security records captured, one "id actor@surface
// k=v…" line each.
func (c *capture) security() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		if e.Log != "security" {
			continue
		}
		l := strings.TrimSpace(e.Msg) + " | " + e.Actor + "@" + e.Surface
		for _, f := range e.Data {
			l += " " + f.Key + "=" + f.Text()
		}
		out = append(out, l)
	}
	return out
}

// Sign-in, sign-out, a failed token and the CSRF, host and origin
// rejections are Security records; a request with no token at all is not.
func TestAccessSecurityRecords(t *testing.T) {
	c := installCapture(t)
	h := authServer("s3cret")
	req := func(method, target string, set func(*http.Request)) {
		r := httptest.NewRequest(method, target, nil)
		r.Host, r.RemoteAddr = "example.com", "10.0.0.9:5555"
		if set != nil {
			set(r)
		}
		authDo(h, r)
	}
	req("GET", "/tools", nil) // no token: not recorded
	req("GET", "/tools", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") })
	req("GET", "/tools", func(r *http.Request) { r.Host = "evil.example" })
	req("GET", "/tools", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	req("POST", "/tools", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: dashCookieName, Value: "s3cret"}) })

	s := authedServer()
	for _, tok := range []string{"nope", "s3cret"} {
		r := postForm("/auth/login", url.Values{"token": {tok}})
		r.RemoteAddr = "10.0.0.9:5555"
		s.handleLogin(httptest.NewRecorder(), r)
	}
	r := httptest.NewRequest("POST", "/auth/logout", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	s.handleLogout(httptest.NewRecorder(), r)

	want := []string{
		"Request from 10.0.0.9:5555 refused: token | 10.0.0.9:5555@dashboard reason=token remote=10.0.0.9:5555 method=GET route=/tools",
		"Request from 10.0.0.9:5555 refused: host | 10.0.0.9:5555@dashboard reason=host remote=10.0.0.9:5555 method=GET route=/tools",
		"Request from 10.0.0.9:5555 refused: origin | 10.0.0.9:5555@dashboard reason=origin remote=10.0.0.9:5555 method=GET route=/tools",
		"Request from 10.0.0.9:5555 refused: csrf | 10.0.0.9:5555@dashboard reason=csrf remote=10.0.0.9:5555 method=POST route=(no route)",
		"Request from 10.0.0.9:5555 refused: sign_in | 10.0.0.9:5555@dashboard reason=sign_in remote=10.0.0.9:5555 method=POST route=/auth/login",
		"Dashboard sign-in from 10.0.0.9:5555 | operator@dashboard remote=10.0.0.9:5555",
		"Dashboard sign-out from 10.0.0.9:5555 | operator@dashboard remote=10.0.0.9:5555",
	}
	if got := c.security(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("security records:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
