package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// initCalls is what of f's calls set up a fleet.
func initCalls(f *fakeFleet) []string {
	var out []string
	for _, c := range f.called() {
		if strings.HasPrefix(c, "init|") {
			out = append(out, c)
		}
	}
	return out
}

// Set up the fleet is on the page of a Tenant with no fleet, filled with what
// init itself would choose. A signed-in browser sending the page's own form
// makes the Tenant a master, and nothing else does: not signed out, not the
// API key, not another page. The recovery key is never made here: the page
// says where it is, before and after.
func TestFleetInit(t *testing.T) {
	c := installCapture(t)
	f := &fakeFleet{}
	f.view.Listen, f.view.Dial = "0.0.0.0:9777", "studio:9777, 10.0.0.2:9777"
	s := fleetServer(f)
	h := s.served()

	body := get(t, s, "/fleet").Body.String()
	for _, want := range []string{`action="/fleet/init"`, `name="listen" class="cinput" maxlength="255" value="0.0.0.0:9777"`,
		`value="studio:9777, 10.0.0.2:9777"`, "Make this Tenant a fleet master", "The recovery key is not made here", "<code>tenant fleet init</code>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page of a Tenant with no fleet lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `action="/fleet/add"`) {
		t.Errorf("a Tenant with no fleet offers a code:\n%s", body)
	}

	// Refusals first: none reaches the fleet, and the page's own is on record.
	good := url.Values{"listen": {"127.0.0.1:9777"}, "addresses": {"a.example:9777"}}
	for _, tc := range []struct {
		name   string
		set    func(*http.Request)
		code   int
		reason string // secure()'s reason; "" for the Fleet page's own refusal
	}{
		{"signed out", func(r *http.Request) { here(r); r.Header.Del("Cookie") }, http.StatusUnauthorized, "token"},
		{"no Origin", func(r *http.Request) { here(r); r.Header.Del("Origin") }, http.StatusForbidden, "csrf"},
		{"another site", and(here, header("Origin", "http://evil.example")), http.StatusForbidden, "origin"},
		{"the API key", func(r *http.Request) { here(r); r.Header.Del("Cookie"); bearer("s3cret")(r) }, http.StatusForbidden, ""},
		{"the API key beside a stale sign-in", func(r *http.Request) {
			here(r)
			r.Header.Set("Cookie", dashCookieName+"=old")
			bearer("s3cret")(r)
		}, http.StatusForbidden, ""},
	} {
		before := c.countSecurity("fleet set up without a signed-in browser")
		rec := sendForm(h, http.MethodPost, "/fleet/init", good, tc.set)
		if rec.Code != tc.code || envelope(rec) != tc.reason {
			t.Errorf("%s: %d (refused for %q), want %d for %q:\n%s", tc.name, rec.Code, envelope(rec), tc.code, tc.reason, rec.Body.String())
		}
		if tc.reason == "" && (!strings.Contains(rec.Body.String(), fleetInitSignIn) || c.countSecurity("fleet set up without a signed-in browser") != before+1) {
			t.Errorf("%s: the refusal doesn't say where a fleet is set up, or is not on record:\n%s", tc.name, rec.Body.String())
		}
		if got := initCalls(f); len(got) != 0 {
			t.Fatalf("%s reached the fleet: %q", tc.name, got)
		}
	}
	for name, form := range map[string]url.Values{
		"a listen address too long to be one": {"listen": {strings.Repeat("a", 256)}},
		"more addresses than the field takes": {"addresses": {strings.Repeat("a.example:9777, ", 200)}},
	} {
		rec := sendForm(h, http.MethodPost, "/fleet/init", form, here)
		if _, msg, errMsg := fleetFlash(rec); rec.Code != http.StatusSeeOther || msg != "" || !strings.Contains(errMsg, "too long") || len(initCalls(f)) != 0 {
			t.Errorf("%s: %d, msg %q, err %q, calls %q", name, rec.Code, msg, errMsg, initCalls(f))
		}
	}

	// What the fleet refuses in plain words is shown; anything else is the
	// page's own words, with the reason left to the log.
	for _, tc := range []struct {
		err  error
		want string
	}{
		{PlainError("Nothing changed: the fleet port listens on a host:port address, like 0.0.0.0:9777."), "the fleet port listens on a host:port address"},
		{errors.New("fleet.json: locked"), fleetInitFailed},
	} {
		f.err, f.calls = tc.err, nil
		rec := sendForm(h, http.MethodPost, "/fleet/init", good, here)
		if _, msg, errMsg := fleetFlash(rec); rec.Code != http.StatusSeeOther || msg != "" || !strings.Contains(errMsg, tc.want) || strings.Contains(errMsg, "locked") {
			t.Errorf("a fleet that says %q: %d, msg %q, err %q", tc.err, rec.Code, msg, errMsg)
		}
	}
	f.err, f.calls = nil, nil

	// The page's form: the addresses are split on commas and spaces, and the
	// fleet is told which session asked, by a tag that is not its key.
	rec := sendForm(h, http.MethodPost, "/fleet/init", url.Values{"listen": {" 127.0.0.1:9777 "}, "addresses": {"a.example:9777, 10.0.0.2:9777\n b.example:9777"}}, here)
	page, msg, errMsg := fleetFlash(rec)
	if rec.Code != http.StatusSeeOther || page != "/fleet" || errMsg != "" || !strings.Contains(msg, "This Tenant is a fleet master") {
		t.Fatalf("set up: %d to %q, msg %q, err %q", rec.Code, page, msg, errMsg)
	}
	want := regexp.MustCompile(`^init\|127\.0\.0\.1:9777\|a\.example:9777 10\.0\.0\.2:9777 b\.example:9777\|dashboard session [0-9a-f]{8} from 127\.0\.0\.1:50000$`)
	if got := initCalls(f); len(got) != 1 || !want.MatchString(got[0]) {
		t.Fatalf("the fleet was asked for %q", got)
	}

	// A master with no recovery key: the page says so and where it is made,
	// and offers the first machine.
	body = get(t, s, "/fleet").Body.String()
	for _, want := range []string{"This fleet has no recovery key yet, so no machine's memory is kept.", "<code>tenant fleet init</code>", `action="/fleet/add"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page of a new master lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `action="/fleet/init"`) {
		t.Errorf("a master is offered Set up the fleet again:\n%s", body)
	}
	f.view.NoRecovery = false
	if body := get(t, s, "/fleet").Body.String(); strings.Contains(body, "no recovery key") {
		t.Errorf("a fleet with its recovery key is told it has none:\n%s", body)
	}
}
