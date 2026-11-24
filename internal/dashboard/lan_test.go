package dashboard

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

// onLAN is a request that arrived on the LAN listener: over TLS, at the
// listener's address local (ip:port), from remote, naming host.
func onLAN(lan *LAN, local, remote, host string) func(*http.Request) {
	return func(r *http.Request) {
		at := netip.MustParseAddrPort(local)
		ctx := context.WithValue(r.Context(), lanKey{}, lan)
		ctx = context.WithValue(ctx, http.LocalAddrContextKey, &net.TCPAddr{IP: at.Addr().AsSlice(), Port: int(at.Port())})
		*r = *r.WithContext(ctx)
		r.TLS, r.RemoteAddr, r.Host = &tls.ConnectionState{}, remote, host
	}
}

// The LAN listener answers a request that names the address it arrived at, or
// one of this computer's names, on its own port: a name another site points
// at this computer (DNS rebinding) is neither. It takes a request from this
// computer, a private or link-local address, or a network the operator
// listed, and from nowhere else; that refusal is in the Security log. The
// dashboard's own listener answers to none of it.
func TestLANHostAndSource(t *testing.T) {
	c := installCapture(t)
	s := fleetServer(nil)
	s.cfg.AllowedHosts = []string{"dash.example.com"}
	h := s.served()
	lan := &LAN{Names: []string{"studio", "studio.local"}, Allow: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")}}
	const at, peer = "192.168.1.5:9771", "192.168.1.77:50000"
	for _, tc := range []struct {
		name                string
		local, remote, host string
		code                int
		reason              string
	}{
		{"the address it arrived at", at, peer, "192.168.1.5:9771", http.StatusOK, ""},
		{"this computer's name", at, peer, "studio:9771", http.StatusOK, ""},
		{"its name in other letters, with a dot", at, peer, "Studio.Local.:9771", http.StatusOK, ""},
		{"a name the operator trusts for the dashboard", at, peer, "dash.example.com:9771", http.StatusOK, ""},
		{"an IPv6 address it arrived at", "[fd00::5]:9771", "[fd00::77]:50000", "[fd00::5]:9771", http.StatusOK, ""},
		{"localhost, on the loopback interface", "127.0.0.1:9771", "127.0.0.1:50000", "localhost:9771", http.StatusOK, ""},
		{"a link-local address", at, "[fe80::1%en0]:50000", "192.168.1.5:9771", http.StatusOK, ""},
		{"a network the operator listed", at, "100.64.0.7:50000", "192.168.1.5:9771", http.StatusOK, ""},

		{"another site's name, pointed here", at, peer, "evil.example:9771", http.StatusForbidden, "host"},
		{"another of this computer's addresses", at, peer, "10.0.0.9:9771", http.StatusForbidden, "host"},
		{"the dashboard's own port", at, peer, "192.168.1.5:9770", http.StatusForbidden, "host"},
		{"no port, which is 443", at, peer, "192.168.1.5", http.StatusForbidden, "host"},
		{"localhost, on another interface", at, peer, "localhost:9771", http.StatusForbidden, "host"},
		{"no host", at, peer, "", http.StatusForbidden, "host"},
		{"a public address", at, "8.8.8.8:50000", "192.168.1.5:9771", http.StatusForbidden, "source"},
		{"an overlay address nobody listed", at, "100.127.255.255:50000", "192.168.1.5:9771", http.StatusOK, ""},
		{"an address past the listed network", at, "100.128.0.1:50000", "192.168.1.5:9771", http.StatusForbidden, "source"},
		{"an address that is not one", at, "nonsense", "192.168.1.5:9771", http.StatusForbidden, "source"},
	} {
		before := c.countSecurity("refused: "+tc.reason, "remote="+tc.remote)
		rec := sendForm(h, http.MethodGet, "/healthz", nil, onLAN(lan, tc.local, tc.remote, tc.host))
		if rec.Code != tc.code || envelope(rec) != tc.reason {
			t.Errorf("%s: %d (refused for %q), want %d for %q", tc.name, rec.Code, envelope(rec), tc.code, tc.reason)
		}
		if tc.reason != "" && c.countSecurity("refused: "+tc.reason, "remote="+tc.remote) != before+1 {
			t.Errorf("%s: the refusal is not in the Security log: %q", tc.name, c.security())
		}
	}
	// The dashboard's own listener is as it was: it does not answer to a LAN
	// address, and takes a loopback name as before.
	if rec := sendForm(h, http.MethodGet, "/healthz", nil, func(r *http.Request) { r.RemoteAddr, r.Host = peer, "192.168.1.5:9771" }); rec.Code != http.StatusForbidden {
		t.Errorf("the dashboard's own listener answered to a LAN address: %d", rec.Code)
	}
	if rec := sendForm(h, http.MethodGet, "/healthz", nil, func(r *http.Request) { r.RemoteAddr, r.Host = "127.0.0.1:50000", "localhost:9770" }); rec.Code != http.StatusOK {
		t.Errorf("the dashboard's own listener: %d", rec.Code)
	}
}

// A browser on the network signs in on the LAN listener with the dashboard's
// key and gets a session cookie marked Secure, then uses the dashboard as one
// on this computer does. What stays at this computer stays there: it is not
// "at this computer", so it confirms no fleet machine. A sign-in on the
// plain loopback listener keeps its cookie as it was.
func TestLANSignInAndWhatStaysLocal(t *testing.T) {
	f := fleetFake(1)
	s := fleetServer(f)
	h := s.served()
	lan := &LAN{}
	const origin = "https://192.168.1.5:9771"
	remote := onLAN(lan, "192.168.1.5:9771", "192.168.1.77:50000", "192.168.1.5:9771")

	if rec := sendForm(h, http.MethodGet, "/fleet", nil, and(remote, header("Accept", "text/html"))); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/settings?") {
		t.Fatalf("a browser that has not signed in: %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := sendForm(h, http.MethodPost, "/auth/login", url.Values{"token": {"wrong"}}, and(remote, header("Origin", origin))); rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("a wrong key: %d, cookies %v", rec.Code, rec.Result().Cookies())
	}
	rec := sendForm(h, http.MethodPost, "/auth/login", url.Values{"token": {"s3cret"}}, and(remote, header("Origin", origin)))
	var session *http.Cookie
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == dashCookieName {
			session = ck
		}
	}
	if rec.Code != http.StatusSeeOther || session == nil || !session.Secure || !session.HttpOnly || session.SameSite != http.SameSiteStrictMode {
		t.Fatalf("sign-in over the LAN: %d, cookie %+v", rec.Code, session)
	}
	browser := and(remote, func(r *http.Request) { r.AddCookie(session); r.Header.Set("Origin", origin) })
	if rec := sendForm(h, http.MethodGet, "/fleet", nil, browser); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Add machine") {
		t.Fatalf("the Fleet page over the LAN: %d", rec.Code)
	}
	// A change any signed-in browser may make goes through; the one step that
	// is this computer's does not, and reaches nothing.
	if rec := sendForm(h, http.MethodPost, "/fleet/rename", url.Values{"machine": {f.view.Machines[0].ID}, "name": {"web9"}}, browser); rec.Code != http.StatusSeeOther {
		t.Errorf("a rename over the LAN: %d %s", rec.Code, rec.Body.String())
	}
	f.calls = nil
	if rec := sendForm(h, http.MethodPost, "/fleet/confirm", url.Values{"machine": {f.view.Machines[0].ID}, "code": {"123456"}}, browser); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), fleetNotHere) || len(f.called()) != 0 {
		t.Errorf("a confirmation over the LAN: %d, calls %q", rec.Code, f.called())
	}
	// A page on another site cannot use the session, and neither can the same
	// browser pointed at the listener by another name.
	if rec := sendForm(h, http.MethodPost, "/fleet/rename", url.Values{"machine": {f.view.Machines[0].ID}, "name": {"x"}}, and(browser, header("Origin", "https://evil.example"))); rec.Code != http.StatusForbidden {
		t.Errorf("another site's form over the LAN: %d", rec.Code)
	}

	// The plain loopback listener: the cookie is not marked Secure, or the
	// browser would never send it back over http.
	rec = sendForm(h, http.MethodPost, "/auth/login", url.Values{"token": {"s3cret"}}, func(r *http.Request) {
		r.RemoteAddr, r.Host = "127.0.0.1:50000", "127.0.0.1:9770"
		r.Header.Set("Origin", "http://127.0.0.1:9770")
	})
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == dashCookieName && ck.Secure {
			t.Errorf("the loopback sign-in's cookie is marked Secure: %+v", ck)
		}
	}
}
