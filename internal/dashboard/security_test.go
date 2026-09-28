package dashboard

import (
	"context"
	"errors"
	"html"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"tenant/internal/logs/event"
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
	req("POST", "/tools", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession}) })

	s := authedServer()
	for _, tok := range []string{"nope", "s3cret"} {
		r := postForm("/auth/login", url.Values{"token": {tok}})
		r.RemoteAddr = "10.0.0.9:5555"
		s.handleLogin(httptest.NewRecorder(), r)
	}
	for _, cookie := range []string{"", "stale", "s3cret", testSession} { // only the signed-in one is the operator's
		r := httptest.NewRequest("POST", "/auth/logout", nil)
		r.RemoteAddr = "10.0.0.9:5555"
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: dashCookieName, Value: cookie})
		}
		s.handleLogout(httptest.NewRecorder(), r)
	}

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

// ---- TEN-213: the route inventory and the sweeps over it ----

// routeRe reads a mounted route: every one is a literal "METHOD /pattern" in
// a HandleFunc call (mountedRoutes fails on any other kind of registration).
var routeRe = regexp.MustCompile(`HandleFunc\("([A-Z]+) ([^"]+)"`)

// mountedRoutes is every "METHOD /pattern" the dashboard serves, read from
// this package's sources and the log book API's, which it mounts.
func mountedRoutes(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, glob := range []string{"*.go", "../logs/logapi/*.go"} {
		files, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			src := string(b)
			found := routeRe.FindAllStringSubmatch(src, -1)
			if n := strings.Count(src, "HandleFunc(") + strings.Count(src, ".Handle("); n != len(found) {
				t.Errorf("%s registers %d routes the inventory can't read: mount each with a literal \"METHOD /path\" in HandleFunc", f, n-len(found))
			}
			for _, m := range found {
				out = append(out, m[1]+" "+m[2])
			}
		}
	}
	if len(out) < 100 {
		t.Fatalf("the scan found %d routes: it is broken", len(out))
	}
	return out
}

var wildcard = regexp.MustCompile(`\{[^}]*\}`)

// routePath is pattern's path with each wildcard filled with v ({$} with
// nothing).
func routePath(pattern, v string) string {
	return wildcard.ReplaceAllStringFunc(pattern, func(w string) string {
		if w == "{$}" {
			return ""
		}
		return v
	})
}

// confirmGate is when a route asks for its confirm step (DESIGN.md §5, D2):
// the forms that meet it (428 without confirm=yes) and the ones that go
// straight through. arm, when set, puts a server in the state where the
// form it returns meets the step too.
type confirmGate struct {
	confirms, passes []url.Values
	arm              func(*Server) url.Values
}

// everyField sets every form field a dashboard handler reads to a value that
// asks for more: a route without a step must not grow one, whatever it's sent.
var everyField = func() url.Values {
	v := url.Values{}
	for _, k := range requestFields {
		v.Set(k, "true")
	}
	return v
}()

// requestFields is every form field and query parameter a handler reads by
// name, read from the same sources as mountedRoutes, so a new one is swept
// without anyone listing it.
var requestFields = func() []string {
	re := regexp.MustCompile(`(?:FormValue\(|\.Get\(|\.Has\(|bodyOnly\(r, |flag\(\w+, |number\([^,()]+(?:\(\))?, )"([a-z][a-z_]*)"`)
	var files []string
	for _, glob := range []string{"*.go", "../logs/logapi/*.go"} {
		m, err := filepath.Glob(glob)
		if err != nil {
			panic(err)
		}
		files = append(files, m...)
	}
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			panic(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}()

var (
	always = confirmGate{confirms: []url.Values{{}}}
	// noStep: it tightens, can be undone on the page, or typing it is the
	// friction (D3). /api/* is a JSON client acting deliberately (D2).
	noStep = confirmGate{passes: []url.Values{{}, everyField}}
)

// unless is confirmUnless(key, safe...): every spelling of "more" meets the
// step, a missing field too unless "" is safe, and each safe value passes.
func unless(key string, safe ...string) confirmGate {
	var g confirmGate
	for _, v := range append(slices.Clone(moreAliases), "approve_session", "approve_always", "") {
		f := url.Values{key: {v}}
		if v == "" {
			f = url.Values{}
		}
		if slices.Contains(safe, strings.ToLower(strings.TrimSpace(v))) {
			g.passes = append(g.passes, f)
		} else {
			g.confirms = append(g.confirms, f)
		}
	}
	for _, v := range safe {
		g.passes = append(g.passes, url.Values{key: {v}})
	}
	return g
}

func withArm(g confirmGate, arm func(*Server) url.Values) confirmGate { g.arm = arm; return g }

// reviewedMutations is the TEN-213 ledger: every route that changes
// something, and when it asks first. A route missing here fails
// TestRouteInventory until someone decides, and writes down, which it is.
var reviewedMutations = map[string]confirmGate{
	// Access (TEN-208): letting someone in, cutting everyone off and loosening
	// what a channel may do ask first; turning off, asking and blocking don't.
	"POST /access/imessage/allow":      always,
	"POST /access/imessage/deny":       {passes: []url.Values{{}, {"handle": {"a@b.c"}}}, arm: lastIMessageContact},
	"POST /access/imessage/clear":      always,
	"POST /access/imessage/responder":  unless("on", "", "false", "off"),
	"POST /access/imessage/permission": unless("mode", "ask", "deny"),
	"POST /access/discord/enable":      unless("on", "", "false", "off"),
	"POST /access/discord/operator":    always,
	"POST /access/discord/exec":        unless("on", "", "false", "off"),
	"POST /access/discord/permission":  unless("mode", "ask", "deny"),
	// Helpers (TEN-209): clearing instructions, removing and resetting lose them.
	"POST /agents/add":    noStep,
	"POST /agents/model":  noStep,
	"POST /agents/rename": noStep,
	"POST /agents/soul":   {confirms: []url.Values{{}, {"soul": {"  "}}}, passes: []url.Values{{"soul": {"Be terse."}}}},
	"POST /agents/remove": always,
	"POST /agents/reset":  always,
	// Scheduled jobs.
	"POST /cron/add":         noStep,
	"POST /cron/{id}/enable": noStep,
	"POST /cron/{id}/delete": always,
	"POST /cron/{id}/run":    noStep,
	"POST /cron/exec":        unless("enabled", "", "false"),
	// Provider keys: storing one is typed; removing it loses it.
	"POST /settings/keys/set":    noStep,
	"POST /settings/keys/remove": always,
	// Quality.
	"POST /eval/schedule":    noStep,
	"POST /eval/run":         noStep,
	"POST /eval/judge":       noStep,
	"POST /eval/judge/clear": noStep,
	// Remote services: a new one starts at Ask; removing loses its sign-in.
	"POST /mcp/connect": noStep,
	"POST /mcp/remove":  always,
	"POST /mcp/trust":   unless("mode", "ask", "deny"),
	// Autopilot (TEN-212): Stop is the safe direction.
	"POST /goal/stop": noStep,
	// Integrations: a repository starts watch-only; write asks first.
	"POST /integrations/save":                noStep,
	"POST /integrations/test":                noStep,
	"POST /integrations/disconnect":          always,
	"POST /integrations/github/repos/add":    unless("mode", "", "read"),
	"POST /integrations/github/repos/mode":   unless("mode", "read"),
	"POST /integrations/github/repos/remove": noStep,
	// Research (TEN-210, D20): Stop and Delete lose the run.
	"POST /research/start":       noStep,
	"POST /research/clarify":     noStep,
	"POST /research/stop":        always,
	"POST /research/{id}/replay": noStep,
	"POST /research/{id}/delete": always,
	// Model: a switch changes every answer; removing loses the setup.
	"POST /models/use":     always,
	"POST /models/add":     noStep,
	"POST /models/remove":  always,
	"POST /models/ceiling": noStep,
	"POST /models/reload":  noStep,
	// Approvals & safety (TEN-207, D4): Allow is the step; Ask, Deny, Allow
	// once and "Keep them at Ask" are one tap; pairing's Allow once asks.
	"POST /safety/mode":           unless("mode", "ask", "deny"),
	"POST /safety/new":            noStep,
	"POST /safety/approvals/{id}": withArm(unless("decision", "approve", "deny"), pairingRequest),
	// Sign-in.
	"POST /auth/login":  noStep,
	"POST /auth/logout": noStep,
	// Skills: accepting without you, trusting and forgetting ask first.
	"POST /skills/auto":   unless("mode", "off"),
	"POST /skills/accept": noStep,
	"POST /skills/enable": noStep,
	"POST /skills/forget": always,
	"POST /skills/trust":  unless("trusted", "", "false"),
	// Tools. A switched-on tool still asks through its category's check.
	"POST /tools/{name}/toggle":    noStep,
	"POST /plugins/{label}/toggle": noStep,
	"POST /posture":                unless("allow_send", "", "false"),
	"POST /tools/rules/add":        unless("mode", "ask", "deny"),
	"POST /tools/rules/remove":     always, // unless the rule itself is allow (ruleMode); the harness has none
	// Chat.
	"POST /chat/send":      noStep,
	"POST /chat/interject": noStep,
	"POST /chat/stop":      noStep,
	// Memory: a removed fact comes back from Recently removed.
	"POST /memory/soul/edit":                  noStep,
	"POST /memory/facts/resolve":              noStep,
	"POST /memory/facts/{id}/delete":          noStep,
	"POST /memory/facts/{id}/restore":         noStep,
	"POST /memory/facts/{id}/edit":            noStep,
	"POST /memory/facts/{id}/invalidate":      noStep,
	"POST /memory/facts/{id}/revalidate":      noStep,
	"POST /memory/facts/{id}/restore-version": noStep,
	// Logs: a reveal is recorded; the Security log refuses to be cleared or
	// weakened (logs/store/policy.go).
	"POST /logs/detail/{seq}/reveal": noStep,
	"POST /logs/views":               noStep,
	"POST /logs/views/delete":        noStep,
	"POST /logs/properties":          noStep,
	"POST /logs/clear":               always,
	// The JSON API.
	"POST /api/memory/soul":                       noStep,
	"POST /api/memory/facts/resolve":              noStep,
	"POST /api/memory/facts/{id}/restore":         noStep,
	"DELETE /api/memory/facts/{id}":               noStep,
	"POST /api/memory/userprofile/resync":         noStep,
	"POST /api/memory/facts/{id}/edit":            noStep,
	"POST /api/memory/facts/{id}/invalidate":      noStep,
	"POST /api/memory/facts/{id}/revalidate":      noStep,
	"POST /api/memory/facts/{id}/restore-version": noStep,
	"POST /api/tools/{name}":                      noStep,
	"POST /api/plugins/{label}":                   noStep,
	"POST /api/posture":                           noStep,
	"POST /api/approvals/{id}":                    noStep,
	"POST /api/addon/event":                       noStep,
	"POST /api/logs/{source}":                     noStep,
	"POST /api/events":                            noStep,
	"PATCH /api/logbook/logs/{log}":               noStep,
	"POST /api/logbook/logs/{log}/clear":          noStep,
	"POST /api/logbook/views":                     noStep,
	"DELETE /api/logbook/views/{name}":            noStep,
	"POST /api/logbook/recover":                   noStep,
	"POST /api/logbook/alerts":                    noStep,
	"PATCH /api/logbook/alerts/{name}":            noStep,
	"DELETE /api/logbook/alerts/{name}":           noStep,
}

// lastIMessageContact: removing the only allowed person cuts everyone off.
func lastIMessageContact(s *Server) url.Values {
	s.SetAccess(&fakeAccess{view: AccessView{IMessageAvailable: true, Handles: []string{"a@b.c"}, SingleHandle: true}})
	return url.Values{"handle": {"a@b.c"}}
}

// pairingRequest: pairing's Allow once asks (D4), for the route's {id}.
func pairingRequest(s *Server) url.Values {
	s.SetApprovals(&fakeApprovals{pending: []PendingApproval{{ID: "x", Category: "pairing", Action: "pair", Detail: "PIN 1234"}}})
	return url.Values{"decision": {"approve"}}
}

// inventoryServer has every route mounted and no controls: the confirm step
// belongs to the route, whatever its control.
func inventoryServer(cfg Config) *Server {
	return withSession(New(cfg, nil, nil, newFakeMem(), nil, nil))
}

// sendForm serves one form request on h, the way a page posts it. A stream
// ends with its 200ms context.
func sendForm(h http.Handler, method, target string, form url.Values, set func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if set != nil {
		set(r)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r.WithContext(ctx))
	return rec
}

func withField(f url.Values, kv ...string) url.Values {
	v := url.Values{}
	for k, vs := range f {
		v[k] = slices.Clone(vs)
	}
	for i := 0; i < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

// TestRouteInventory is the ledger's check (TEN-213): every route that
// changes something is in reviewedMutations and does what it says there, on
// the real mux. A route that asks first answers 428 without confirm=yes, to
// every spelling of "more", and goes through with it; one that doesn't never
// grows a step, whatever it's sent. No GET ever asks.
func TestRouteInventory(t *testing.T) {
	s := inventoryServer(Config{})
	seen := map[string]bool{}
	for _, route := range mountedRoutes(t) {
		method, pattern, _ := strings.Cut(route, " ")
		path := routePath(pattern, "x")
		if _, p := s.mux.Handler(httptest.NewRequest(method, path, nil)); p != route {
			t.Errorf("%s: %s %s reaches %q", route, method, path, p)
			continue
		}
		if method == http.MethodGet {
			if rec := sendForm(s.Handler(), method, path+"?"+everyField.Encode(), nil, nil); rec.Code == http.StatusPreconditionRequired {
				t.Errorf("%s asks for a confirm step: a GET never changes anything", route)
			}
			continue
		}
		g, ok := reviewedMutations[route]
		if !ok {
			t.Errorf("%s changes something and isn't in reviewedMutations: decide whether it asks first (DESIGN.md §5) and add it to the TEN-213 ledger", route)
			continue
		}
		seen[route] = true
		for _, f := range g.confirms {
			if rec := sendForm(s.Handler(), method, path, f, nil); rec.Code != http.StatusPreconditionRequired || !strings.Contains(rec.Body.String(), notConfirmed) {
				t.Errorf("%s %v without confirm = %d, want 428", route, f, rec.Code)
			}
			if rec := sendForm(s.Handler(), method, path, withField(f, "confirm", "yes"), nil); rec.Code == http.StatusPreconditionRequired {
				t.Errorf("%s %v with confirm=yes still asks", route, f)
			}
		}
		for _, f := range g.passes {
			if rec := sendForm(s.Handler(), method, path, f, nil); rec.Code == http.StatusPreconditionRequired {
				t.Errorf("%s %v asks for a confirm step it shouldn't", route, f)
			}
		}
		if g.arm != nil {
			armed := inventoryServer(Config{})
			f := g.arm(armed)
			if rec := sendForm(armed.Handler(), method, path, f, nil); rec.Code != http.StatusPreconditionRequired {
				t.Errorf("%s %v, armed, without confirm = %d, want 428", route, f, rec.Code)
			}
			if rec := sendForm(armed.Handler(), method, path, withField(f, "confirm", "yes"), nil); rec.Code == http.StatusPreconditionRequired {
				t.Errorf("%s %v, armed, with confirm=yes still asks", route, f)
			}
		}
	}
	for route := range reviewedMutations {
		if !seen[route] {
			t.Errorf("%s is in reviewedMutations but not mounted: take it out", route)
		}
	}
}

// envelope is what secure() refused a request for, as its Security record
// says it ("" when the request got through to a handler).
func envelope(rec *httptest.ResponseRecorder) string {
	for reason, msg := range map[string]string{"host": "unrecognized dashboard host", "origin": "cross-origin request rejected",
		"token": "missing or invalid bearer token", "csrf": "CSRF validation failed"} {
		if strings.Contains(rec.Body.String(), `{"error":"`+msg+`"}`) {
			return reason
		}
	}
	return ""
}

func signedIn(r *http.Request) { r.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession}) }

// from is a signed-in browser's request sent by a page at origin.
func from(origin string) func(*http.Request) {
	return func(r *http.Request) { signedIn(r); r.Header.Set("Origin", origin) }
}

func bearer(key string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) }
}

// TestEveryMutationRefusesCSRF sends every route that changes something
// through the production chain (served()) the ways another page could: signed
// out, with the sign-in cookie but no Origin, from another port on this host,
// from another site, from an opaque origin (Origin: null), and through a
// rebinding host. secure() refuses each before any handler runs and files its
// Security record. Only this page's own Origin, or the bearer key, gets
// through.
func TestEveryMutationRefusesCSRF(t *testing.T) {
	c := installCapture(t)
	s := inventoryServer(Config{Addr: "example.com", Auth: "s3cret"})
	h := s.served()
	for _, route := range mountedRoutes(t) {
		method, pattern, _ := strings.Cut(route, " ")
		if method == http.MethodGet {
			continue
		}
		withSession(s) // this page's own /auth/logout ends it
		path := routePath(pattern, "x")
		for _, tc := range []struct {
			name   string
			set    func(*http.Request)
			code   int
			reason string
		}{
			{"signed out", nil, http.StatusUnauthorized, "token"},
			{"no Origin", signedIn, http.StatusForbidden, "csrf"},
			{"another port", from("http://example.com:9999"), http.StatusForbidden, "origin"},
			{"another site", from("http://evil.example"), http.StatusForbidden, "origin"},
			{"Origin null", from("null"), http.StatusForbidden, "origin"},
			{"rebinding", func(r *http.Request) { from("http://evil.example")(r); r.Host = "evil.example" }, http.StatusForbidden, "host"},
		} {
			if tc.reason == "token" && (pattern == "/auth/login" || pattern == "/auth/logout") {
				tc.code, tc.reason = http.StatusForbidden, "csrf" // reachable signed out, still never cross-site
			}
			rec := sendForm(h, method, path, everyField, tc.set)
			if rec.Code != tc.code || envelope(rec) != tc.reason {
				t.Errorf("%s, %s: %d (refused for %q), want %d refused for %s", route, tc.name, rec.Code, envelope(rec), tc.code, tc.reason)
			}
		}
		for _, csrf := range []string{"csrf", "origin", "host"} {
			if n := c.countSecurity("reason="+csrf, "method="+method, "route="+pattern); n == 0 {
				t.Errorf("%s: no Security record of the %s refusal", route, csrf)
			}
		}
		for name, set := range map[string]func(*http.Request){"this page": from("http://example.com"), "the bearer key": bearer("s3cret")} {
			if rec := sendForm(h, method, path, url.Values{}, set); envelope(rec) != "" {
				t.Errorf("%s from %s: refused (%d, %s), want it through", route, name, rec.Code, envelope(rec))
			}
		}
	}
}

// countSecurity counts the Security records holding every part.
func (c *capture) countSecurity(parts ...string) int {
	n := 0
	for _, l := range c.security() {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(l+" ", p+" ")
		}
		if all {
			n++
		}
	}
	return n
}

// navigate is a person opening the page in a browser.
func navigate(r *http.Request) {
	r.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
}

// TestUnauthenticatedGETSweep opens every GET route signed out, with a stale
// sign-in and with a wrong key: a person is sent to the sign-in form, and a
// script, Datastar and a stream get 401. No route answers 200 but the two
// open ones: the liveness probe and the sign-in form.
func TestUnauthenticatedGETSweep(t *testing.T) {
	s := inventoryServer(Config{Addr: "example.com", Auth: "s3cret"})
	h := s.served()
	for _, route := range mountedRoutes(t) {
		method, pattern, _ := strings.Cut(route, " ")
		if method != http.MethodGet {
			continue
		}
		path := routePath(pattern, "x")
		if pattern == "/healthz" || pattern == "/settings" {
			if rec := sendForm(h, method, path, nil, nil); rec.Code != http.StatusOK {
				t.Errorf("%s signed out = %d, want the open page (200)", route, rec.Code)
			}
			continue
		}
		for _, tc := range []struct {
			name   string
			set    func(*http.Request)
			person bool
		}{
			{"a person", navigate, true},
			{"a stale sign-in", func(r *http.Request) {
				navigate(r)
				r.AddCookie(&http.Cookie{Name: dashCookieName, Value: "old-key"})
			}, true},
			{"a script", nil, false},
			{"a wrong key", bearer("nope"), false},
			{"Datastar", func(r *http.Request) {
				r.Header.Set("Accept", "text/event-stream, text/html, application/json")
				r.Header.Set("Datastar-Request", "true")
				r.Header.Set("Sec-Fetch-Mode", "cors")
			}, false},
			{"a stream", func(r *http.Request) { r.Header.Set("Accept", "text/event-stream") }, false},
		} {
			rec := sendForm(h, method, path, nil, tc.set)
			loc := rec.Header().Get("Location")
			switch {
			case rec.Code == http.StatusOK:
				t.Errorf("%s, %s: 200 signed out:\n%.300s", route, tc.name, rec.Body.String())
			case tc.person && (rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/settings?next=")):
				t.Errorf("%s, %s: %d → %q, want 303 to the sign-in form", route, tc.name, rec.Code, loc)
			case !tc.person && (rec.Code != http.StatusUnauthorized || envelope(rec) != "token"):
				t.Errorf("%s, %s: %d, want 401", route, tc.name, rec.Code)
			}
		}
	}
}

// TestAuthHeadersAndSignInRedirect: every answer, from every route, signed
// in or out, refuses framing, runs no script but the dashboard's own (D31),
// keeps its Referer same-origin and is never sniffed; a person opening a page
// signed out is sent to the sign-in form, while the API and streams get 401,
// and only GET /healthz is open (HEAD needs the key, as a monitor must know).
func TestAuthHeadersAndSignInRedirect(t *testing.T) {
	s := inventoryServer(Config{Addr: "example.com", Auth: "s3cret"})
	h := s.served()
	want := map[string]string{
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "frame-ancestors 'none'; script-src 'self' 'unsafe-eval'; object-src 'none'; base-uri 'none'",
		"Referrer-Policy":         "same-origin",
		"X-Content-Type-Options":  "nosniff",
	}
	for _, route := range append(mountedRoutes(t), "GET /no/such/page") {
		method, pattern, _ := strings.Cut(route, " ")
		for name, set := range map[string]func(*http.Request){"signed out": nil, "signed in": bearer("s3cret"), "cross-site": from("http://evil.example")} {
			rec := sendForm(h, method, routePath(pattern, "x"), nil, set)
			for k, v := range want {
				if got := rec.Header().Get(k); got != v {
					t.Errorf("%s %s (%d): %s = %q, want %q", route, name, rec.Code, k, got, v)
				}
			}
		}
	}
	for _, tc := range []struct {
		name, method, path string
		set                func(*http.Request)
		code               int
		loc                string
	}{
		{"a person opening /", http.MethodGet, "/", navigate, http.StatusSeeOther, "/settings?next=%2F"},
		{"the API", http.MethodGet, "/api/status", func(r *http.Request) { r.Header.Set("Accept", "application/json") }, http.StatusUnauthorized, ""},
		{"a stream", http.MethodGet, "/events", func(r *http.Request) { r.Header.Set("Accept", "text/event-stream") }, http.StatusUnauthorized, ""},
		{"HEAD /healthz", http.MethodHead, "/healthz", nil, http.StatusUnauthorized, ""},
		{"GET /healthz", http.MethodGet, "/healthz", nil, http.StatusOK, ""},
	} {
		rec := sendForm(h, tc.method, tc.path, nil, tc.set)
		if rec.Code != tc.code || rec.Header().Get("Location") != tc.loc {
			t.Errorf("%s: %d → %q, want %d → %q", tc.name, rec.Code, rec.Header().Get("Location"), tc.code, tc.loc)
		}
		if tc.code == http.StatusUnauthorized && tc.method == http.MethodGet && !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s: a 401 is JSON, got %q", tc.name, rec.Header().Get("Content-Type"))
		}
	}
}

// A reveal shows an event's sensitive values to the signed-in operator on
// this page, or to the bearer key, and files one FieldRevealed record per
// reveal. Signed out, from another page or with a wrong key it shows
// nothing and files nothing; the page's GETs never reveal (TEN-213).
func TestLogsRevealNeedsSignInAndThisPage(t *testing.T) {
	s, _, st := logsBook(t)
	evs := commit(t, st, event.Event{Log: "dashboard", Provider: "dashboard", ID: 2001, Level: event.Warning, Msg: "HTTP server: tls",
		Data: []event.Field{{Key: "err", Value: "tls: bad record"}, {Key: "stack", Value: "the secret payload", Sensitive: true}}})
	seq := itoa(int(evs[0].Seq))
	h := s.served()
	ours := func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession})
		r.Header.Set("Origin", "http://example.com")
	}
	theirs := func(origin string) func(*http.Request) {
		return func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession})
			r.Header.Set("Origin", origin)
		}
	}
	// A reveal files FieldRevealed (4904); an export files LogExported (4903),
	// which says it carried the sensitive values.
	reveals := func() int {
		n := len(security(t, st, 4904))
		for _, e := range security(t, st, 4903) {
			if strings.Contains(e.Msg, "with sensitive values") {
				n++
			}
		}
		return n
	}
	reveal := "/logs/detail/" + seq + "/reveal"
	reads := []string{"/api/events?sensitive=1&q=log%3Adashboard", "/api/events/" + seq + "?sensitive=1", "/api/events/export?sensitive=1&q=log%3Adashboard"}
	for name, set := range map[string]func(*http.Request){
		"signed out":   nil,
		"no Origin":    func(r *http.Request) { r.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession}) },
		"another port": theirs("http://example.com:1"),
		"another site": theirs("http://evil.example"),
		"a wrong key":  bearer("nope"),
	} {
		if rec := sendForm(h, http.MethodPost, reveal, nil, set); envelope(rec) == "" || strings.Contains(rec.Body.String(), "the secret payload") {
			t.Errorf("reveal, %s: %d, want refused:\n%s", name, rec.Code, rec.Body.String())
		}
		for _, p := range reads {
			if name == "no Origin" {
				continue // a signed-in read with no Origin is this page's own
			}
			if rec := sendForm(h, http.MethodGet, p, nil, set); envelope(rec) == "" || strings.Contains(rec.Body.String(), "the secret payload") {
				t.Errorf("GET %s, %s: %d, want refused", p, name, rec.Code)
			}
		}
	}
	if n := reveals(); n != 0 {
		t.Fatalf("%d reveal records from refused requests", n)
	}
	if rec := sendForm(h, http.MethodGet, "/logs/detail/"+seq+"?tab=details&sensitive=1", nil, ours); rec.Code != http.StatusOK ||
		strings.Contains(rec.Body.String(), "the secret payload") || reveals() != 0 {
		t.Errorf("the page's GET revealed (%d, %d records)", rec.Code, reveals())
	}
	if rec := sendForm(h, http.MethodPost, reveal, nil, ours); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "the secret payload") || reveals() != 1 {
		t.Errorf("the page's reveal: %d, %d records, want the value and 1", rec.Code, reveals())
	}
	for i, p := range reads {
		rec := sendForm(h, http.MethodGet, p, nil, bearer("tok"))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "the secret payload") || reveals() != 2+i {
			t.Errorf("GET %s with the key: %d, %d records, want the value and %d", p, rec.Code, reveals(), 2+i)
		}
	}
	if rec := sendForm(h, http.MethodGet, "/api/events/"+seq, nil, bearer("tok")); strings.Contains(rec.Body.String(), "the secret payload") || reveals() != 1+len(reads) {
		t.Errorf("a read without sensitive=1 revealed (%d records)", reveals())
	}
}

// TestSecretsNeverFromQuery: every place a secret is typed takes it from the
// form's body only (F2), through the production chain. One in the URL, where
// history and proxy logs keep it, is refused before any control or
// comparison sees it, logged as KeyInURL and never echoed; one in the body is
// taken, and no page shows it back. A control's failure, which can quote the
// secret, reaches the log masked and the page as a fixed message (F3). No
// record carries a secret in the clear.
func TestSecretsNeverFromQuery(t *testing.T) {
	const secret = "sk-SENTINEL-7f3"
	c := installCapture(t)
	keys := &fakeSecrets{}
	models := &fakeModelCtl{}
	brave := map[string][]IntegrationField{"brave": {{Key: "api_key", Secret: true}}}
	integ := &fakeIntegrationsCtl{list: []IntegrationView{{ID: "brave", Label: "Brave"}}, fields: brave}
	s := withSession(New(Config{Addr: "example.com", Auth: "s3cret"}, nil, nil, nil, nil, nil))
	s.SetSecrets(keys)
	s.SetModels(models)
	s.SetIntegrations(integ)
	h, page := s.served(), from("http://example.com")
	answer := func(rec *httptest.ResponseRecorder) string {
		loc, _ := url.QueryUnescape(rec.Header().Get("Location"))
		return loc + "\n" + rec.Body.String()
	}
	intakes := []struct {
		route, page, field string
		form               url.Values
		calls              func() []string
	}{
		{"/settings/keys/set", "/settings/keys", "value", url.Values{"service": {"openai"}}, func() []string { return keys.setCalls }},
		{"/models/add", "/models", "key", url.Values{"kind": {"anthropic"}}, func() []string { return models.addCalls }},
		{"/integrations/save", "/integrations", "api_key", url.Values{"integration": {"brave"}}, func() []string { return integ.configCalls }},
	}
	for _, in := range intakes {
		rec := sendForm(h, http.MethodPost, in.route+"?"+in.field+"="+secret, withField(in.form, in.field, secret), page)
		if a := answer(rec); rec.Code != http.StatusSeeOther || !strings.Contains(a, secretInURL) || strings.Contains(a, secret) || len(in.calls()) != 0 {
			t.Errorf("%s with the secret in the URL: %d %q, calls %q; want refused, unechoed, no call", in.route, rec.Code, a, in.calls())
		}
		rec = sendForm(h, http.MethodPost, in.route, withField(in.form, in.field, secret), page)
		if calls := in.calls(); len(calls) != 1 || !strings.Contains(calls[0], secret) || strings.Contains(answer(rec), secret) {
			t.Errorf("%s with the secret in the body: calls %q, answer %q; want it taken, unechoed", in.route, calls, answer(rec))
		}
		if strings.Contains(sendForm(h, http.MethodGet, in.page, nil, page).Body.String(), secret) {
			t.Errorf("%s shows the saved secret back", in.page)
		}
	}
	// The sign-in key: in the URL it signs nobody in, in the body it does.
	origin := func(r *http.Request) { r.Header.Set("Origin", "http://example.com") }
	if rec := sendForm(h, http.MethodPost, "/auth/login?token=s3cret", url.Values{"token": {"s3cret"}}, origin); rec.Code != http.StatusBadRequest ||
		len(rec.Result().Cookies()) != 0 || strings.Contains(answer(rec), "s3cret") {
		t.Errorf("sign-in with the key in the URL: %d, cookies %v", rec.Code, rec.Result().Cookies())
	}
	if rec := sendForm(h, http.MethodPost, "/auth/login", url.Values{"token": {"s3cret"}}, origin); rec.Code != http.StatusSeeOther || len(rec.Result().Cookies()) != 1 {
		t.Errorf("sign-in with the key in the body: %d, cookies %v", rec.Code, rec.Result().Cookies())
	}
	inURL := 0
	for _, l := range c.lines() {
		if strings.HasPrefix(l, "2203 ") {
			inURL++
		}
	}
	if inURL != len(intakes)+1 {
		t.Errorf("KeyInURL records = %d, want %d:\n%s", inURL, len(intakes)+1, strings.Join(c.lines(), "\n"))
	}

	// A control that fails quoting the secret.
	keys.setErr = errors.New("bad key " + secret)
	s.SetModels(&failingModelCtl{})
	s.SetIntegrations(&failingIntegrations{fakeIntegrationsCtl{fields: brave}})
	for _, in := range intakes {
		rec := sendForm(h, http.MethodPost, in.route, withField(in.form, in.field, secret), page)
		if a := answer(rec); !strings.Contains(a, "err=Couldn't") || strings.Contains(a, secret) {
			t.Errorf("%s failing: %q, want a fixed message only", in.route, a)
		}
	}
	notSaved := 0
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.Provider == Events.Name && e.ID == 2202 {
			notSaved++
		}
		for _, f := range e.Data {
			if f.Key == "err" && e.Provider == Events.Name && e.ID == 2202 && !f.Sensitive {
				t.Errorf("KeyNotSaved's err isn't masked")
			}
			if !f.Sensitive && (strings.Contains(f.Text(), secret) || strings.Contains(f.Text(), "s3cret")) {
				t.Errorf("%s %d field %s carries a secret in the clear: %q", e.Provider, e.ID, f.Key, f.Text())
			}
		}
		if strings.Contains(e.Msg, secret) || strings.Contains(e.Msg, "s3cret") {
			t.Errorf("%s %d says a secret: %q", e.Provider, e.ID, e.Msg)
		}
	}
	if notSaved != len(intakes) {
		t.Errorf("KeyNotSaved records = %d, want %d", notSaved, len(intakes))
	}
}

// injection closes a quoted string in a Datastar expression and runs script
// (F1's payload).
const injection = `x'"),(window.p=1),('`

// dataAttr is one data-* attribute as a page sends it.
var dataAttr = regexp.MustCompile(`\s(data-[^\s=>/"']+)=(?:"([^"]*)"|'([^']*)')`)

// injectedExprs lists page's data-* attributes other than data-on* (which
// html/template escapes as script) whose value, decoded as the browser
// decodes it before Datastar evaluates it, carries the injection.
func injectedExprs(page string) []string {
	var bad []string
	for _, m := range dataAttr.FindAllStringSubmatch(page, -1) {
		if v := html.UnescapeString(m[2] + m[3]); !strings.HasPrefix(m[1], "data-on") && strings.Contains(v, "x'") {
			bad = append(bad, m[1]+`="`+v+`"`)
		}
	}
	return bad
}

// poison prefixes p to every string reachable from v through this package's
// and the control package's types (the fakes and the views they return),
// unexported fields included: a fake's state is what its control reports.
// Other packages' values (a mutex, a time's location) are left alone.
func poison(v reflect.Value, p string, seen map[uintptr]bool) {
	ours := func(t reflect.Type) bool {
		return t.PkgPath() == "tenant/internal/dashboard" || t.PkgPath() == "tenant/internal/control"
	}
	switch v.Kind() {
	case reflect.Interface:
		if !v.IsNil() {
			poison(v.Elem(), p, seen)
		}
	case reflect.Pointer:
		if !v.IsNil() && ours(v.Type().Elem()) && !seen[v.Pointer()] {
			seen[v.Pointer()] = true
			poison(v.Elem(), p, seen)
		}
	case reflect.Struct:
		if ours(v.Type()) && v.CanAddr() {
			for i := range v.NumField() {
				f := v.Field(i)
				poison(reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem(), p, seen)
			}
		}
	case reflect.Slice:
		for i := range v.Len() {
			poison(v.Index(i), p, seen)
		}
	case reflect.Map:
		if v.CanSet() {
			for _, k := range v.MapKeys() {
				e := reflect.New(v.Type().Elem()).Elem()
				e.Set(v.MapIndex(k))
				poison(e, p, seen)
				v.SetMapIndex(k, e)
			}
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(p + v.String())
		}
	}
}

// TestNoRawInputInDatastarExpressions is F1's sweep (DESIGN.md §8): no text
// from a request or an agent reaches a Datastar expression other than
// data-on*. Every GET route is asked with the injection in every path segment
// and every parameter a handler reads, and each harness page with it in one
// parameter at a time, from a server with no controls and from one whose
// fakes hold the injection in every string they report (names, details,
// reports, goals, log text).
func TestNoRawInputInDatastarExpressions(t *testing.T) {
	bare := inventoryServer(Config{})
	poisoned := harnessServer(t, Config{}, populated, nil)
	sv := reflect.ValueOf(poisoned).Elem()
	seen := map[uintptr]bool{}
	for i := range sv.NumField() {
		if f := sv.Field(i); f.Kind() == reflect.Interface {
			poison(f, injection, seen)
		}
	}
	_, st := poisoned.logbookStore()
	logged := commit(t, st, event.Event{Log: "cron", Task: injection, Actor: injection, Msg: injection, Data: []event.Field{{Key: injection, Value: injection}}})
	seq := itoa(int(logged[0].Seq))
	for _, p := range []string{"/tools", "/safety", "/research", "/logs/detail/" + seq + "?tab=details"} {
		if !strings.Contains(html.UnescapeString(get(t, poisoned, p).Body.String()), injection) {
			t.Fatalf("%s doesn't show the fakes' injection: poison missed them", p)
		}
	}

	all := url.Values{}
	for _, k := range requestFields {
		all.Set(k, injection)
	}
	check := func(s *Server, target string) {
		t.Helper()
		for _, b := range injectedExprs(sendForm(s.Handler(), http.MethodGet, target, nil, nil).Body.String()) {
			t.Errorf("GET %s puts the injection in a Datastar expression: %s", target, b)
		}
	}
	for _, route := range mountedRoutes(t) {
		if method, pattern, _ := strings.Cut(route, " "); method == http.MethodGet {
			for _, s := range []*Server{bare, poisoned} {
				check(s, routePath(pattern, url.PathEscape(injection))+"?"+all.Encode())
			}
		}
	}
	pages := append(slices.Clone(harnessPages), "/ui/status", "/logs/components", "/logs/rows", "/logs/count",
		"/logs/detail/"+seq+"?tab=details", "/logs/event/"+seq, "/settings",
		"/agents?edit="+url.QueryEscape(injection+longName(2)), "/research/"+url.PathEscape(injection+"r-1"))
	for _, p := range pages {
		sep := map[bool]string{true: "&", false: "?"}[strings.Contains(p, "?")]
		for _, k := range requestFields {
			check(poisoned, p+sep+url.Values{k: {injection}}.Encode())
		}
		check(bare, p)
	}
}

// F7: a remote service that fails to connect is logged without its address,
// which can carry a key (https://…/sse?api_key=…): the url field is masked,
// and the error, which quotes the address, doesn't repeat it.
func TestOAuthConnectorURLIsSensitive(t *testing.T) {
	c := installCapture(t)
	const addr = "https://mcp.example.com/sse?api_key=SENTINEL-mcp"
	fc := &failingConnect{fakeMCPCtl: &fakeMCPCtl{}, done: make(chan struct{})}
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetMCP(fc)
	submitForm(t, s, "/mcp/connect", url.Values{"url": {addr}}.Encode())
	<-fc.done
	var logged *event.Event
	for deadline := time.Now().Add(2 * time.Second); logged == nil && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		c.mu.Lock()
		for _, e := range c.events {
			if e.Provider == Events.Name && e.ID == 2205 {
				logged = e
			}
		}
		c.mu.Unlock()
	}
	if logged == nil {
		t.Fatal("no ConnectorNotAdded record")
	}
	if strings.Contains(logged.Msg, "SENTINEL") {
		t.Errorf("the record says the address: %q", logged.Msg)
	}
	for _, f := range logged.Data {
		if strings.Contains(f.Text(), "SENTINEL") && !f.Sensitive {
			t.Errorf("field %s carries the address unmasked: %q", f.Key, f.Text())
		}
	}
}

// failingConnect fails to connect with an error that quotes the address, as
// mcpremote's does ("mcpremote: connect <url>: …").
type failingConnect struct {
	*fakeMCPCtl
	done chan struct{}
}

func (f *failingConnect) Connect(u string) error {
	defer close(f.done)
	return errors.New("mcpremote: connect " + u + ": 401 Unauthorized")
}
