package dashboard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// enrollCalls is what of f's calls made a code.
func enrollCalls(f *fakeFleet) []string {
	var out []string
	for _, c := range f.called() {
		if strings.HasPrefix(c, "enroll|") {
			out = append(out, c)
		}
	}
	return out
}

// everything is every event written while c was installed, each as its
// message and its fields: where a code must never be.
func (c *capture) everything() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, e := range c.events {
		b.WriteString(e.Msg + " " + e.Actor + " " + e.Surface)
		for _, f := range e.Data {
			b.WriteString(" " + f.Key + "=" + f.Text())
		}
		b.WriteString("\n")
	}
	return b.String()
}

// --- refusals first ---

// A code for a new machine is made by a signed-in browser sending the page's
// own form, and by nothing else: not signed out, not with the API key, not
// from another page. A refusal never reaches the fleet; one the page turns
// away itself is in the Security log. A form that names no usable machine or
// no way to run makes nothing.
func TestFleetAddRefusals(t *testing.T) {
	c := installCapture(t)
	f := fleetFake(3)
	h := fleetServer(f).served()
	good := url.Values{"name": {"web9"}, "runs": {"user"}}
	for _, tc := range []struct {
		name   string
		form   url.Values
		set    func(*http.Request)
		code   int
		reason string // secure()'s reason; "" for the Fleet page's own refusal
	}{
		{"signed out", good, func(r *http.Request) { here(r); r.Header.Del("Cookie") }, http.StatusUnauthorized, "token"},
		{"a stale sign-in", good, func(r *http.Request) { here(r); r.Header.Set("Cookie", dashCookieName+"=old") }, http.StatusUnauthorized, "token"},
		{"no Origin", good, func(r *http.Request) { here(r); r.Header.Del("Origin") }, http.StatusForbidden, "csrf"},
		{"another port on this computer", good, and(here, header("Origin", "http://127.0.0.1:9999")), http.StatusForbidden, "origin"},
		{"another site", good, and(here, header("Origin", "http://evil.example")), http.StatusForbidden, "origin"},
		{"an opaque origin", good, and(here, header("Origin", "null")), http.StatusForbidden, "origin"},
		{"the API key", good, func(r *http.Request) { here(r); r.Header.Del("Cookie"); bearer("s3cret")(r) }, http.StatusForbidden, ""},
		{"the API key with no Origin", good, func(r *http.Request) {
			r.RemoteAddr, r.Host = "127.0.0.1:50000", "127.0.0.1:9770"
			bearer("s3cret")(r)
		}, http.StatusForbidden, ""},
		{"the API key beside a stale sign-in", good, func(r *http.Request) {
			here(r)
			r.Header.Set("Cookie", dashCookieName+"=old")
			bearer("s3cret")(r)
		}, http.StatusForbidden, ""},
		{"the API key and the session, with no Origin", good, func(r *http.Request) {
			here(r)
			r.Header.Del("Origin")
			bearer("s3cret")(r)
		}, http.StatusForbidden, ""},
	} {
		before := c.countSecurity("fleet code without a signed-in browser")
		rec := sendForm(h, http.MethodPost, "/fleet/add", tc.form, tc.set)
		if rec.Code != tc.code || envelope(rec) != tc.reason {
			t.Errorf("%s: %d (refused for %q), want %d for %q:\n%s", tc.name, rec.Code, envelope(rec), tc.code, tc.reason, rec.Body.String())
		}
		if tc.reason == "" {
			if !strings.Contains(rec.Body.String(), fleetAddSignIn) {
				t.Errorf("%s: the refusal doesn't say where a code is made:\n%s", tc.name, rec.Body.String())
			}
			if c.countSecurity("fleet code without a signed-in browser") != before+1 {
				t.Errorf("%s: no Security record of the refusal:\n%s", tc.name, strings.Join(c.security(), "\n"))
			}
		}
		if got := enrollCalls(f); len(got) != 0 {
			t.Fatalf("%s reached the fleet: %q", tc.name, got)
		}
	}

	// A signed-in browser's form that names nothing the fleet can take.
	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{"no name", url.Values{"runs": {"user"}}, fleetAddName},
		{"a blank name", url.Values{"name": {"   "}, "runs": {"user"}}, fleetAddName},
		{"a name longer than any the fleet takes", url.Values{"name": {strings.Repeat("a", 64)}, "runs": {"user"}}, fleetAddName},
		{"no way to run", url.Values{"name": {"web9"}}, "Choose whether it runs as you or from boot."},
		{"another way to run", url.Values{"name": {"web9"}, "runs": {"root"}}, "Choose whether it runs as you or from boot."},
		{"a way to run in other letters", url.Values{"name": {"web9"}, "runs": {"BOOT"}}, "Choose whether it runs as you or from boot."},
	} {
		rec := sendForm(h, http.MethodPost, "/fleet/add", tc.form, here)
		page, msg, errMsg := fleetFlash(rec)
		if rec.Code != http.StatusSeeOther || page != "/fleet" || msg != "" || !strings.Contains(errMsg, tc.want) {
			t.Errorf("%s: %d to %q, msg %q, err %q", tc.name, rec.Code, page, msg, errMsg)
		}
		if got := enrollCalls(f); len(got) != 0 {
			t.Fatalf("%s reached the fleet: %q", tc.name, got)
		}
	}

	// A body past the dashboard's form limit is not read at all.
	if rec := sendForm(h, http.MethodPost, "/fleet/add", url.Values{"name": {strings.Repeat("a", 1<<20)}, "runs": {"boot"}}, here); rec.Code < 400 || len(enrollCalls(f)) != 0 {
		t.Errorf("a megabyte of name: %d, calls %q", rec.Code, enrollCalls(f))
	}

	// A name the fleet refuses (the same check `tenant fleet enroll` makes)
	// is the page's plain words, and the fleet's raw words stay in the log.
	for _, name := range []string{"web 1", "taken"} {
		rec := sendForm(h, http.MethodPost, "/fleet/add", url.Values{"name": {name}, "runs": {"user"}}, here)
		if _, _, errMsg := fleetFlash(rec); rec.Code != http.StatusSeeOther || errMsg != fleetAddName {
			t.Errorf("%q: %d, err %q", name, rec.Code, errMsg)
		}
		if strings.Contains(rec.Body.String()+rec.Header().Get("Location"), fakeEnrollCode) {
			t.Errorf("%q: a refusal carries a code", name)
		}
	}
	f.err = errors.New("fleet.json: locked")
	if rec := sendForm(h, http.MethodPost, "/fleet/add", good, here); rec.Code != http.StatusSeeOther {
		t.Errorf("a fleet that fails: %d", rec.Code)
	}
}

// The code is shown once, in the answer to the request that made it, and is
// kept nowhere: not in a redirect or its flash, not in a cookie, not in the
// page a reload shows, not in any log line. The fleet is told which session
// asked, by a tag that is not the session's key. Any signed-in browser may
// make one (A55: only the confirmation is local-only), by any name the
// dashboard answers to.
func TestFleetAddShowsTheCodeOnce(t *testing.T) {
	c := installCapture(t)
	f := fleetFake(3)
	s := fleetServer(f)
	s.SetTrustedHosts([]string{"box.tailnet.ts.net"})
	h := s.served()
	tailnet := func(r *http.Request) {
		r.RemoteAddr, r.Host = "100.64.0.7:50000", "box.tailnet.ts.net"
		r.Header.Set("Origin", "https://box.tailnet.ts.net")
		signedIn(r)
	}
	datastar := and(here, header("Datastar-Request", "true"))
	tag := regexp.MustCompile(`^enroll\|web9\|(true|false)\|dashboard session [0-9a-f]{8} from (127\.0\.0\.1|100\.64\.0\.7):50000$`)
	for _, tc := range []struct {
		name string
		runs string
		set  func(*http.Request)
		sse  bool
	}{
		{"a plain post, as the user", "user", here, false},
		{"a plain post, from boot", "boot", here, false},
		{"the page's own Datastar post", "user", datastar, true},
		{"a browser on the tailnet", "boot", tailnet, false},
	} {
		f.calls = nil
		rec := sendForm(h, http.MethodPost, "/fleet/add", url.Values{"name": {" web9 "}, "runs": {tc.runs}}, tc.set)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
			t.Fatalf("%s: %d to %q:\n%s", tc.name, rec.Code, rec.Header().Get("Location"), body)
		}
		// A plain answer is never stored; a Datastar answer is the event stream's,
		// which is a POST's answer and has no freshness to be kept for.
		if got := rec.Header().Get("Cache-Control"); got != "no-store" && !(tc.sse && got == "no-cache") {
			t.Errorf("%s: Cache-Control %q", tc.name, got)
		}
		if tc.sse != strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
			t.Errorf("%s: answered as %q", tc.name, rec.Header().Get("Content-Type"))
		}
		for _, want := range []string{fakeEnrollCode, "TENANT_ENROLL_CODE=&#39;" + fakeEnrollCode + "&#39; sh", "works once, until 2026-11-19 10:15",
			"192.168.1.5:9777", "a machine at another site cannot dial these", "Copy", "el.previousElementSibling.textContent"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the answer lacks %q:\n%s", tc.name, want, body)
			}
		}
		if strings.Contains(body, `"`+fakeEnrollCode) || strings.Contains(body, `writeText('`) {
			t.Errorf("%s: the code is in an attribute or an expression:\n%s", tc.name, body)
		}
		for _, ck := range rec.Result().Cookies() {
			if strings.Contains(ck.Value, fakeEnrollCode) {
				t.Errorf("%s: a cookie carries the code", tc.name)
			}
		}
		calls := enrollCalls(f)
		if len(calls) != 1 || !tag.MatchString(calls[0]) || strings.Contains(calls[0], testSession) || strings.HasPrefix(calls[0], "enroll|web9|"+map[string]string{"user": "true", "boot": "false"}[tc.runs]) {
			t.Errorf("%s: the fleet was asked %q", tc.name, calls)
		}
		// A reload shows the page, not the code.
		if again := sendForm(h, http.MethodGet, "/fleet", nil, tc.set); again.Code != http.StatusOK || strings.Contains(again.Body.String(), fakeEnrollCode) {
			t.Errorf("%s: the page shows the code again (%d)", tc.name, again.Code)
		}
	}
	if all := c.everything(); strings.Contains(all, fakeEnrollCode) || strings.Contains(all, "eyJuIjoid2ViMSJ9") {
		t.Errorf("a log line holds the code:\n%s", all)
	}
}

// The Fleet page offers Add machine on a fleet that can be listed, and the
// form posts the two fields the route takes.
func TestFleetPageOffersAddMachine(t *testing.T) {
	h := fleetServer(fleetFake(3)).served()
	rec := sendForm(h, http.MethodGet, "/fleet", nil, here)
	body := rec.Body.String()
	for _, want := range []string{`action="/fleet/add"`, `name="name"`, `maxlength="63"`, `name="runs" value="user" checked`, `name="runs" value="boot"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the Fleet page lacks %q", want)
		}
	}
	if strings.Contains(body, fakeEnrollCode) {
		t.Error("the page shows a code nobody asked for")
	}
}

// Add machine takes a request's body, Origin, cookie and Authorization as a
// browser or anything else sends them. However they are spelled, the fleet
// is asked for a code only by the live session with the dashboard's own
// Origin, for a name of 1 to 63 bytes once trimmed and a way to run that is
// user or boot to the letter; the code is in the answer only then, never in
// a redirect; and nothing answers 5xx.
func FuzzFleetAdd(f *testing.F) {
	for _, seed := range [][4]string{
		{"name=web1&runs=user", "http://127.0.0.1:9770", testSession, ""},
		{"name=+web1+&runs=boot", "http://127.0.0.1:9770", testSession, ""},
		{"name=web1&runs=user", "http://127.0.0.1:9770", "", "Bearer s3cret"},
		{"name=web1&runs=user", "", testSession, "Bearer s3cret"},
		{"name=web1&runs=user&runs=boot", "http://127.0.0.1:9770", testSession, ""},
		{"runs=boot&name=" + strings.Repeat("a", 63), "http://127.0.0.1:9770", testSession, ""},
		{"name=%00web&runs=user", "http://127.0.0.1:9770", testSession, ""},
		{"name=web1;runs=user", "http://127.0.0.1:9770", testSession, ""},
		{"name=web%0A1&runs=USER", "http://127.0.0.1:9770", testSession, ""},
		{"name=web1&runs=user", "null", testSession, ""},
		{"name=web1&runs=user", "http://127.0.0.1:9770", "old", ""},
		{"%", "http://127.0.0.1:9770", testSession, ""},
	} {
		f.Add(seed[0], seed[1], seed[2], seed[3])
	}
	fake := fleetFake(1)
	h := fleetServer(fake).served()
	f.Fuzz(func(t *testing.T, body, origin, cookie, auth string) {
		fake.mu.Lock()
		fake.calls = nil
		fake.mu.Unlock()
		r := httptest.NewRequest(http.MethodPost, "/fleet/add", strings.NewReader(body))
		r.RemoteAddr, r.Host = "127.0.0.1:50000", "127.0.0.1:9770"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", origin)
		r.Header.Set("Cookie", dashCookieName+"="+cookie)
		r.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code >= 500 {
			t.Fatalf("%d for %q", rec.Code, body)
		}
		if strings.Contains(rec.Header().Get("Location"), fakeEnrollCode) {
			t.Fatalf("a redirect carries the code")
		}
		calls := enrollCalls(fake)
		if len(calls) == 0 {
			if strings.Contains(rec.Body.String(), fakeEnrollCode) {
				t.Fatalf("a code nobody made is in the answer")
			}
			return
		}
		form, _ := url.ParseQuery(body)
		name, runs := strings.TrimSpace(form.Get("name")), form.Get("runs")
		switch {
		case cookie != testSession || !sameOrigin(origin):
			t.Errorf("a code for cookie %q, origin %q, auth %q", cookie, origin, auth)
		case name == "" || len(name) > maxFleetName || runs != "user" && runs != "boot":
			t.Errorf("a code for name %q, runs %q", name, runs)
		}
	})
}

// sameOrigin is the dashboard's own Origin as checkOrigin reads one: an http
// scheme in any case and the same authority as sameAuthority compares them
// (a host's case and its root's dot do not count).
func sameOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Scheme, "http") && sameAuthority(u.Host, "127.0.0.1:9770")
}
