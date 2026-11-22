package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"testing"
)

// loopbackPart reports whether s, a Host or a peer address, names this
// computer once any port and brackets are taken off: a loopback address, or
// (name) localhost. It is read here with net/netip, where the dashboard reads
// it with net: the two must agree on every request that is let through.
func loopbackPart(s string, name bool) bool {
	h := s
	if i := strings.LastIndexByte(s, ':'); i >= 0 && (strings.HasSuffix(s[:i], "]") || strings.Count(s, ":") == 1) {
		h = s[:i]
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if name {
		// A Host may end in the root's dot: "localhost." and "127.0.0.1." are
		// the same names to a browser as without it.
		if h = strings.TrimSuffix(h, "."); strings.ToLower(h) == "localhost" {
			return true
		}
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.Zone() == "" && a.Unmap().IsLoopback()
}

var proxyHeader = regexp.MustCompile(`(?i)^(forwarded|via|x-forwarded-.*|x-real-.*|tailscale-.*|cf-.*)$`)

// Where a fleet machine may be confirmed is decided from what a request says
// of itself: its Host, the address it came from, its headers and its cookie.
// However those are spelled, a request counts as the browser on the computer
// running Tenant only if it came from a loopback address, names the dashboard
// by a loopback name with nothing of a URL in it, carries no proxy's header,
// and holds the live session. (What follows the name's colon is not read
// here: net/http refuses a Host that is not one before any handler, and a
// form's Origin has to match it to the letter.)
func FuzzAtThisComputer(f *testing.F) {
	for _, seed := range [][5]string{
		{"127.0.0.1:9770", "127.0.0.1:50000", "", "", testSession},
		{"localhost:9770", "[::1]:50000", "Accept", "text/html", testSession},
		{"[::1]:9770", "[::ffff:127.0.0.1]:1", "", "", testSession},
		{"LOCALHOST.:9770", "127.0.0.1:50000", "", "", testSession},
		{"127.0.0.0.", "127.0.0.0", "0", "0", testSession}, // found by this fuzz: a dotted address, all of 127/8
		{"loCAlhost: 0", "127.0.0.1", "", "", testSession}, // found by this fuzz: the port is not read, the name is
		{"box.tailnet.ts.net", "127.0.0.1:50000", "X-Forwarded-For", "100.64.0.7", testSession},
		{"127.0.0.1:9770", "127.0.0.1:50000", "x-forwarded-for", "203.0.113.9", testSession},
		{"127.0.0.1:9770", "127.0.0.1:50000", "X-Forwarded-For ", "203.0.113.9", testSession},
		{"127.0.0.1:9770", "127.0.0.1:50000", "Tailscale-User-Login", "a@b.c", testSession},
		{"127.0.0.1:9770", "127.0.0.1:50000", "Cookie", dashCookieName + "=" + testSession, "stale"},
		{"127.0.0.1:9770", "192.168.1.20:50000", "", "", testSession},
		{"127.0.0.1:9770", "[fe80::1%lo]:50000", "", "", testSession},
		{"127.0.0.1:9770", "localhost:50000", "", "", testSession},
		{"127.0.0.1.evil.example:9770", "127.0.0.1:50000", "", "", testSession},
		{"localhost.evil.example", "127.0.0.1:50000", "", "", testSession},
		{"evil.example@127.0.0.1:9770", "127.0.0.1:50000", "", "", testSession},
		{"127.0.0.1:9770/x", "127.0.0.1:50000", "", "", testSession},
		{"127.0.0.1:9770#localhost", "127.0.0.1:50000", "", "", testSession},
		{" localhost", "127.0.0.1:50000", "", "", testSession},
		{"0x7f.0.0.1", "127.0.0.1:50000", "", "", testSession},
		{"[::1%25eth0]:9770", "::1", "", "", testSession},
		{"127.0.0.1:9770", "127.0.0.1:50000", "", "", "s3cret"},
		{"127.0.0.1:9770", "127.0.0.1:50000", "Authorization", "Bearer s3cret", ""},
		{"127.0.0.1:9770", "127.0.0.1:50000", "", "", `"` + testSession + `"; ` + dashCookieName + "=x"},
		{"", "", "", "", ""},
	} {
		f.Add(seed[0], seed[1], seed[2], seed[3], seed[4])
	}
	s := fleetServer(nil)
	f.Fuzz(func(t *testing.T, host, remote, name, value, cookie string) {
		r := httptest.NewRequest(http.MethodPost, "/fleet/confirm", nil)
		r.Host, r.RemoteAddr, r.Header = host, remote, http.Header{}
		if name != "" {
			r.Header[name] = []string{value} // as it is spelled, canonical or not
		}
		r.Header.Add("Cookie", dashCookieName+"="+cookie)
		if !s.atThisComputer(r) {
			return
		}
		switch {
		case !loopbackPart(remote, false):
			t.Errorf("a request from %q counts as this computer's", remote)
		case !loopbackPart(host, true) || strings.ContainsAny(host, "/@?#"):
			t.Errorf("a request for host %q counts as this computer's", host)
		case proxyHeader.MatchString(name):
			t.Errorf("a request with the header %q counts as this computer's", name)
		case !strings.Contains(cookie, testSession) && !(http.CanonicalHeaderKey(name) == "Cookie" && strings.Contains(value, testSession)):
			t.Errorf("a request with the cookie %q (and %q: %q) counts as signed in", cookie, name, value)
		}
	})
}
