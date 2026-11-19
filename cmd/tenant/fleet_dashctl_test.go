package main

// fleet_dashctl_test.go drives the dashboard's Fleet page (TEN-533) as the
// operator's browser does: a real fleet (fleetLab: fleet.json and the settings
// file in temp dirs, the hub's manager on a loopback port, real children
// linked to it) behind a real dashboard, every request real HTTP through the
// production chain (dashboardManager.Enable → Serve → served()). The page's
// own states and its forms' refusals with a fake fleet are in
// internal/dashboard/fleet_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"tenant/internal/dashboard"
	"tenant/internal/fleet"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
)

const fleetDashKey = "fleet-dash-test-key"

// fleetDash is a fleetLab's master with its dashboard up on a loopback port
// and a browser signed in to it on this computer.
type fleetDash struct {
	t      *testing.T
	l      *fleetLab
	mgr    *fleet.Manager
	ev     *channelEvents
	base   string
	cookie *http.Cookie
}

// newFleetDash starts a master whose settings file is stg, and its dashboard.
func newFleetDash(t *testing.T, stg settings) *fleetDash {
	t.Helper()
	d := &fleetDash{t: t, ev: captureChannelEvents(t), l: newFleetLab(t)}
	d.l.settings(stg)
	d.mgr = d.l.master()
	dm := &dashboardManager{
		base: context.Background(),
		// The dashboard also answers to a name a proxy publishes it under, as it
		// does to a tailnet name: a request by that name is not a local one.
		cfg:    dashboard.Config{Addr: "127.0.0.1:0", Auth: fleetDashKey, AllowedHosts: []string{"dash.tailnet.example"}},
		fleet:  newDashFleet(d.l.c, d.l.stg, &d.l.stgMu),
		notify: func(string) {},
	}
	addr, err := dm.Enable()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dm.Shutdown() })
	if !healthzUp(addr, 5*time.Second) {
		t.Fatal("the dashboard did not come up")
	}
	d.base = "http://" + addr
	a := d.send(http.MethodPost, "/auth/login", url.Values{"token": {fleetDashKey}}, func(r *http.Request) { r.Header.Set("Origin", d.base) })
	for _, c := range a.cookies {
		if c.Name == "tenant-dash" && c.Value != "" {
			d.cookie = c
		}
	}
	if d.cookie == nil {
		t.Fatalf("sign-in set no session cookie (%d)", a.code)
	}
	return d
}

// fleetAnswer is one response: its status, where it redirects and the flash
// that carries, and its body.
type fleetAnswer struct {
	code        int
	page        string
	msg, errMsg string
	body        string
	cookies     []*http.Cookie
}

// send is one request to the dashboard, shaped by set.
func (d *fleetDash) send(method, path string, form url.Values, set func(*http.Request)) fleetAnswer {
	d.t.Helper()
	req, err := http.NewRequest(method, d.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		d.t.Fatal(err)
	}
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if set != nil {
		set(req)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	a := fleetAnswer{code: resp.StatusCode, body: string(b), cookies: resp.Cookies()}
	if u, err := url.Parse(resp.Header.Get("Location")); err == nil {
		a.page, a.msg, a.errMsg = u.Path, u.Query().Get("msg"), u.Query().Get("err")
	}
	return a
}

// browser is the signed-in browser on this computer, sending the page's own
// form: the session cookie and the dashboard's own Origin.
func (d *fleetDash) browser(r *http.Request) {
	r.AddCookie(d.cookie)
	r.Header.Set("Origin", d.base)
}

// post sends one of the page's forms from that browser, with its confirm step
// taken.
func (d *fleetDash) post(path string, kv ...string) fleetAnswer {
	d.t.Helper()
	form := url.Values{"confirm": {"yes"}}
	for i := 0; i < len(kv); i += 2 {
		form.Add(kv[i], kv[i+1])
	}
	return d.send(http.MethodPost, path, form, d.browser)
}

func (d *fleetDash) page(path string) string {
	d.t.Helper()
	a := d.send(http.MethodGet, path, nil, func(r *http.Request) { r.AddCookie(d.cookie) })
	if a.code != http.StatusOK {
		d.t.Fatalf("GET %s = %d → %s (%s)", path, a.code, a.page, a.errMsg)
	}
	return a.body
}

// records lists the Security records of def as "actor@surface k=v …".
func (d *fleetDash) records(def *event.Def) []string {
	var out []string
	for _, e := range d.ev.matching(def, "") {
		l := e.Actor + "@" + e.Surface
		for _, f := range e.Data {
			l += " " + f.Key + "=" + f.Text()
		}
		out = append(out, l)
	}
	return out
}

func hasRecord(records []string, parts ...string) bool {
	for _, r := range records {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(r, p)
		}
		if all {
			return true
		}
	}
	return false
}

// pageText is s as html/template writes it into element content.
var pageText = strings.NewReplacer("&", "&amp;", "'", "&#39;", "+", "&#43;", "<", "&lt;", ">", "&gt;", `"`, "&#34;").Replace

// --- refusals first ---

// A pending machine is confirmed on the dashboard by the six digits it shows,
// typed in a browser signed in on this computer, and by nothing else: not
// signed out, not with the API key, not by a name a proxy or a tailnet gives
// the dashboard, not through a proxy on this computer, not from another page,
// not with other digits. Until then the master sends it nothing.
func TestFleetDashConfirmRefusals(t *testing.T) {
	d := newFleetDash(t, settings{Permissions: map[string]string{"exec": "allow", "web": "allow"}})
	ch := d.l.enroll("web1", fleet.Reported{})
	tools := &labTools{}
	client, _ := ch.start(t, tools)
	eventually(t, "the pending machine is told to wait", func() bool { return client.Status().State == fleet.LinkWaiting })
	pending := func(why string) {
		t.Helper()
		if m, err := fleet.Open(d.l.cfg).Machine(ch.id); err != nil || m.State != fleet.StatePending {
			t.Fatalf("%s: the machine is %v (%v), want pending", why, m.State, err)
		}
		if _, ok := d.l.saved().FleetSchemes[ch.id]; ok {
			t.Fatalf("%s: a scheme was saved for a machine that is not confirmed", why)
		}
		if _, err := d.mgr.Call(context.Background(), ch.id, fleet.Call{Tool: "os_exec"}); !errors.Is(err, fleet.ErrPending) {
			t.Fatalf("%s: a call to the pending machine: %v, want ErrPending", why, err)
		}
		if tools.calls.Load() != 0 || client.Status().State != fleet.LinkWaiting {
			t.Fatalf("%s: the pending machine was sent something: %d calls, link %s", why, tools.calls.Load(), client.Status().State)
		}
	}
	pending("before any confirmation")

	form := url.Values{"machine": {ch.id}, "code": {ch.code}}
	key := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+fleetDashKey) }
	byName := func(r *http.Request) { // what `tailscale serve` or a proxy on this computer sends on
		r.AddCookie(d.cookie)
		r.Host = "dash.tailnet.example"
		r.Header.Set("Origin", "http://dash.tailnet.example")
	}
	notHere := "a machine is confirmed in a browser on the computer running Tenant"
	for _, tc := range []struct {
		why  string
		set  func(*http.Request)
		code int
		body string
	}{
		{"signed out", func(r *http.Request) { r.Header.Set("Origin", d.base) }, http.StatusUnauthorized, "missing or invalid bearer token"},
		{"a sign-in from before a restart", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "tenant-dash", Value: "stale"})
			r.Header.Set("Origin", d.base)
		}, http.StatusUnauthorized, "missing or invalid bearer token"},
		{"the signed-in browser's cookie with no Origin", func(r *http.Request) { r.AddCookie(d.cookie) }, http.StatusForbidden, "CSRF validation failed"},
		{"a page on another port of this computer", func(r *http.Request) { r.AddCookie(d.cookie); r.Header.Set("Origin", "http://127.0.0.1:1") }, http.StatusForbidden, "cross-origin request rejected"},
		{"the API key", key, http.StatusForbidden, notHere},
		{"the API key, dressed as the page", func(r *http.Request) { key(r); r.Header.Set("Origin", d.base) }, http.StatusForbidden, notHere},
		{"the name a tailnet or a proxy gives the dashboard", byName, http.StatusForbidden, notHere},
		{"tailscale serve", func(r *http.Request) {
			byName(r)
			r.Header.Set("X-Forwarded-For", "100.64.0.7")
			r.Header.Set("Tailscale-User-Login", "operator@example.com")
		}, http.StatusForbidden, notHere},
		{"a proxy on this computer that keeps the loopback name", func(r *http.Request) { d.browser(r); r.Header.Set("X-Forwarded-For", "203.0.113.9") }, http.StatusForbidden, notHere},
		{"a proxy that says only Via", func(r *http.Request) { d.browser(r); r.Header.Set("Via", "1.1 edge") }, http.StatusForbidden, notHere},
	} {
		if a := d.send(http.MethodPost, "/fleet/confirm", form, tc.set); a.code != tc.code || !strings.Contains(a.body, tc.body) {
			t.Errorf("%s: %d %q, want %d with %q", tc.why, a.code, a.body, tc.code, tc.body)
		}
		pending(tc.why)
	}
	// Each of the Fleet page's own refusals is a Security record that names
	// the machine and where the request came from, not the operator.
	if got := d.records(catalog.FleetConfirmRefused); len(got) != 6 || !hasRecord(got, "127.0.0.1:", "@dashboard", "name=web1", "machine="+ch.id, "reason=not asked from a browser signed in on the computer running Tenant") || hasRecord(got, "operator@") {
		t.Errorf("the Security records of the refused confirmations:\n%s", strings.Join(got, "\n"))
	}
	// A browser that is not on this computer is not offered the form; the one
	// that is, is.
	if away := d.send(http.MethodGet, "/fleet/"+ch.id, nil, byName).body; strings.Contains(away, `action="/fleet/confirm"`) || !strings.Contains(away, "A machine is confirmed on the computer running Tenant") {
		t.Errorf("the pending machine's page, by the tailnet name, offers the form:\n%s", away)
	}
	if page := d.page("/fleet/" + ch.id); !strings.Contains(page, `action="/fleet/confirm"`) || !strings.Contains(page, `name="code"`) {
		t.Fatalf("the pending machine's page on this computer has no form:\n%s", page)
	}

	// The browser on this computer, with digits that are not the machine's.
	wrong := "000000"
	if wrong == ch.code {
		wrong = "111111"
	}
	for _, typed := range []string{wrong, ch.code[:5], ch.code + "0", "abcdef", "١٢٣٤٥٦", ""} {
		a := d.post("/fleet/confirm", "machine", ch.id, "code", typed)
		if a.code != http.StatusSeeOther || a.page != "/fleet/"+ch.id || a.msg != "" || !strings.Contains(a.errMsg, "aren't the six digits that machine shows") {
			t.Errorf("confirm with %q: %d to %s, msg %q, err %q", typed, a.code, a.page, a.msg, a.errMsg)
		}
		pending("after the wrong digits " + typed)
	}
	if got := d.records(catalog.FleetConfirmRefused); !hasRecord(got, "operator@dashboard", "machine="+ch.id, "that is not the code the machine shows") {
		t.Errorf("no Security record of the wrong digits:\n%s", strings.Join(got, "\n"))
	}
	// Six wrong codes: the seventh attempt waits, the right digits included.
	if a := d.post("/fleet/confirm", "machine", ch.id, "code", ch.code); !strings.Contains(a.errMsg, "Too many wrong codes for this machine. Try again in a second.") {
		t.Errorf("the seventh attempt: %d, msg %q, err %q, want the wait", a.code, a.msg, a.errMsg)
	}
	pending("during the wait after six wrong codes")
	if got := d.records(catalog.FleetConfirmRefused); !hasRecord(got, "operator@dashboard", "machine="+ch.id, "reason=too many failed attempts") {
		t.Errorf("no Security record of the wait:\n%s", strings.Join(got, "\n"))
	}
	// A choice that is not one, a machine that is not there.
	time.Sleep(1100 * time.Millisecond) // the wait is one second
	if a := d.post("/fleet/confirm", "machine", ch.id, "code", ch.code, "set", "root=allow"); a.msg != "" || a.errMsg != "Couldn't confirm that machine. Nothing changed." {
		t.Errorf("confirm with a category that is not one: msg %q, err %q", a.msg, a.errMsg)
	}
	pending("after a category that is not one")
	time.Sleep(2100 * time.Millisecond)                                                                                             // and that failure made it two
	if a := d.post("/fleet/confirm", "machine", "web1", "code", ch.code); !strings.Contains(a.errMsg, "isn't part of this fleet") { // its name is not its id
		t.Errorf("confirm by name: msg %q, err %q", a.msg, a.errMsg)
	}
	if a := d.post("/fleet/confirm", "machine", "m0000000000000000", "code", ch.code); !strings.Contains(a.errMsg, "isn't part of this fleet") {
		t.Errorf("confirm of a machine that is not there: msg %q, err %q", a.msg, a.errMsg)
	}
	pending("after confirmations of other machines")

	// Then the machine's own digits, with one category set for it: confirmed,
	// its scheme saved as the confirm screen showed it, and the link comes up.
	a := d.post("/fleet/confirm", "machine", ch.id, "code", " "+ch.code[:3]+" "+ch.code[3:], "set", "", "set", "web=deny")
	if a.code != http.StatusSeeOther || a.page != "/fleet/"+ch.id || a.errMsg != "" || !strings.HasPrefix(a.msg, "web1 is confirmed.") {
		t.Fatalf("confirm with the machine's digits: %d to %s, msg %q, err %q", a.code, a.page, a.msg, a.errMsg)
	}
	if m, _ := fleet.Open(d.l.cfg).Machine(ch.id); m.State != fleet.StateConfirmed {
		t.Fatalf("the machine is %s", m.State)
	}
	scheme := d.l.schemeOf(ch.id)
	if scheme.Permissions["exec"] != "ask" || scheme.Permissions["web"] != "deny" {
		t.Errorf("the scheme it started with: %v; want exec no looser than ask whatever the master allows, and web as set", scheme.Permissions)
	}
	eventually(t, "the confirmed machine links", func() bool { return client.Status().State == fleet.LinkConnected && online(d.mgr, ch.id) })
	if got := d.records(catalog.FleetMachineChanged); !hasRecord(got, "operator@dashboard", "name=web1", "machine="+ch.id, "change=confirmed", `"set_by_the_operator":{"web":"deny"}`) {
		t.Errorf("no Security record of the confirmation:\n%s", strings.Join(got, "\n"))
	}
	if page := d.page("/fleet"); !strings.Contains(page, `<span class="on-tag">online</span>`) || !strings.Contains(page, `<td class="fl-said">&#34;root&#34;</td>`) {
		t.Errorf("the list after the confirmation:\n%s", page)
	}
	// A confirmed machine is not confirmed twice.
	if a := d.post("/fleet/confirm", "machine", ch.id, "code", ch.code); !strings.Contains(a.errMsg, "isn't waiting to be confirmed") {
		t.Errorf("a second confirmation: msg %q, err %q", a.msg, a.errMsg)
	}
	// No row of the API confirms, with the key or from the browser.
	other := d.l.enroll("web2", fleet.Reported{})
	for _, path := range []string{"/api/fleet", "/api/fleet/confirm", "/api/fleet/" + other.id, "/api/fleet/" + other.id + "/confirm"} {
		for _, set := range []func(*http.Request){key, d.browser} {
			if a := d.send(http.MethodPost, path, url.Values{"machine": {other.id}, "code": {other.code}}, set); a.code != http.StatusNotFound && a.code != http.StatusMethodNotAllowed {
				t.Errorf("POST %s = %d, want no such row", path, a.code)
			}
		}
	}
	if m, _ := fleet.Open(d.l.cfg).Machine(other.id); m.State != fleet.StatePending {
		t.Fatalf("the API confirmed a machine: it is %s", m.State)
	}
}

// --- the page, the settings and the drift, on the real files ---

func TestFleetDashSettingsAndDrift(t *testing.T) {
	d := newFleetDash(t, settings{
		Permissions: map[string]string{"exec": "allow", "web": "allow", "mcp": "deny"},
		ToolRules:   map[string]string{"os_exec(git status *)": "allow", "write_note": "deny"},
	})
	d.l.must("permissions", "--defaults", "add", "os_exec(systemctl status *)", "allow")
	d.l.sync()
	ch := d.l.enroll("web1", fleet.Reported{OS: "linux", User: "svc tenant", Version: "1.2.3 (reported by you)", Sandbox: "host", Ceiling: "no-exec"})
	client, done := ch.start(t, &labTools{})

	// Pending: what it reports, as its own words, and what it would start with.
	page := d.page("/fleet/" + ch.id)
	for _, want := range []string{
		`<strong>web1</strong> <span class="tag-new">waiting for you</span>`,
		`<span class="nm">` + ch.id + `</span>`,
		"Reported by the machine", "its own words, in quotes. This Tenant has not checked them.",
		`<tr><th>System</th><td class="fl-said">&#34;linux&#34;</td></tr>`,
		`<tr><th>Runs as</th><td class="fl-said">&#34;svc tenant&#34;</td></tr>`,
		`<tr><th>Version</th><td class="fl-said">&#34;1.2.3 (reported by you)&#34;</td></tr>`,
		`<tr><th>Sandbox</th><td class="fl-said">&#34;host&#34;</td></tr>`,
		`<tr><th>Its own limit</th><td class="fl-said">&#34;no-exec&#34;</td></tr>`,
		`<tr><th>Memory</th><td>unknown <span class="dim">`, // its reason follows
		`<strong>Run commands</strong>: Ask first <span class="dim">(this master: allow)</span>`,
		`<strong>Act on websites</strong>: Allow`,
		`<strong>Connect remote services</strong>: Block`,
		`Rule <span class="nm">os_exec(git status *)</span> <span class="risk">asks you first</span>`,
		`Rule <span class="nm">os_exec(systemctl status *)</span> <span class="tag">runs without asking</span> <span class="dim">fleet rule</span>`,
		`Rule <span class="nm">write_note</span> <span class="act-errtag">blocked</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the pending machine's page is missing %q", want)
		}
	}
	if strings.Contains(page, `action="/fleet/permission"`) || strings.Contains(page, "Compared with a fresh copy") {
		t.Errorf("a pending machine has settings to change:\n%s", page)
	}
	// A machine with no scheme yet has none to edit.
	if a := d.post("/fleet/permission", "machine", ch.id, "category", "web", "mode", "deny"); !strings.Contains(a.errMsg, "doesn't exist for this machine") {
		t.Errorf("a setting of a pending machine: msg %q, err %q", a.msg, a.errMsg)
	}
	if a := d.post("/fleet/scheme", "machine", ch.id, "from", "master"); a.msg != "" || a.errMsg == "" {
		t.Errorf("a copy for a pending machine: msg %q, err %q", a.msg, a.errMsg)
	}
	if _, ok := d.l.saved().FleetSchemes[ch.id]; ok {
		t.Fatal("a pending machine was given a scheme")
	}

	if a := d.post("/fleet/confirm", "machine", ch.id, "code", ch.code); a.errMsg != "" {
		t.Fatalf("confirm: %q", a.errMsg)
	}
	eventually(t, "the machine links", func() bool { return online(d.mgr, ch.id) })
	list := d.page("/fleet")
	for _, want := range []string{
		"this process holds the fleet, on " + d.mgr.Addr().String(),
		`<a class="linkbtn" href="/fleet/` + ch.id + `">web1</a><div class="dim nm">` + ch.id + `</div>`,
		`<span class="on-tag">online</span>`, `<td>online since `,
		`<td class="fl-said">&#34;root&#34;</td>`, // what it says now that it is linked
		`<td>unknown</td>`,
		`<td>6 ask first, 1 allowed, 1 blocked, 3 rules</td>`,
		`<span class="nm">os_exec(systemctl status *)</span> <span class="tag">runs without asking</span>`,
	} {
		if !strings.Contains(list, want) {
			t.Errorf("the list is missing %q", want)
		}
	}
	page = d.page("/fleet/" + ch.id)
	if !strings.Contains(page, "No differences: this machine&#39;s approval settings are what a fresh copy would be.") || strings.Contains(page, "Yes, copy again") {
		t.Errorf("a machine just confirmed differs from a fresh copy:\n%s", page)
	}
	if !strings.Contains(page, "Asks you before it can run commands and programs on this machine.") || strings.Contains(page, "on this computer") {
		t.Errorf("the machine's settings are not worded for the machine:\n%s", page)
	}

	// Loosening asks first; then the settings file holds the change, for this
	// machine alone, and the drift view says what differs and from what.
	if a := d.send(http.MethodPost, "/fleet/permission", url.Values{"machine": {ch.id}, "category": {"exec"}, "mode": {"allow"}}, d.browser); a.code != http.StatusPreconditionRequired {
		t.Errorf("Allow without its second click = %d, want 428", a.code)
	}
	if d.l.schemeOf(ch.id).Permissions["exec"] != "ask" {
		t.Fatal("the refused change was made")
	}
	if a := d.post("/fleet/permission", "machine", ch.id, "category", "exec", "mode", "allow"); a.msg != "On web1: Run commands is allowed without asking." {
		t.Errorf("Allow: msg %q, err %q", a.msg, a.errMsg)
	}
	if a := d.post("/fleet/rules/add", "machine", ch.id, "pattern", "os_exec(uptime *)", "mode", "allow"); a.errMsg != "" {
		t.Errorf("a rule: %q", a.errMsg)
	}
	if a := d.post("/fleet/rules/add", "machine", ch.id, "pattern", "os_exec(*)", "mode", "deny"); a.msg != "" || !strings.HasPrefix(a.errMsg, "Couldn't add that rule.") {
		t.Errorf("a rule that is not one: msg %q, err %q", a.msg, a.errMsg)
	}
	if a := d.send(http.MethodPost, "/fleet/rules/remove", url.Values{"machine": {ch.id}, "pattern": {"write_note"}}, d.browser); a.code != http.StatusPreconditionRequired {
		t.Errorf("removing a rule that blocks, without its second click = %d, want 428", a.code)
	}
	if a := d.send(http.MethodPost, "/fleet/rules/remove", url.Values{"machine": {ch.id}, "pattern": {"os_exec(systemctl status *)"}}, d.browser); a.msg != "Rule removed for web1." {
		t.Errorf("removing a rule that allows, in one click: %d, msg %q, err %q", a.code, a.msg, a.errMsg)
	}
	d.l.stgMu.Lock()
	d.l.stg.Permissions["web"] = "deny" // the master's own setting moves; the machine's does not
	d.l.stgMu.Unlock()
	scheme := d.l.schemeOf(ch.id)
	if scheme.Permissions["exec"] != "allow" || scheme.Permissions["web"] != "allow" || scheme.ToolRules["os_exec(uptime *)"] != "allow" || scheme.ToolRules["os_exec(systemctl status *)"] != "" {
		t.Fatalf("the machine's scheme in the settings file: %+v", scheme)
	}
	if got := d.l.saved(); got.Permissions["exec"] != "allow" || got.ToolRules["os_exec(uptime *)"] != "" {
		t.Fatalf("a machine's change reached the master's own scheme: %v %v", got.Permissions, got.ToolRules)
	}
	page = d.page("/fleet/" + ch.id)
	for _, want := range []string{
		`<strong>Run commands</strong>: this machine is set to Allow; a fresh copy would be Ask first.`,
		`<strong>Act on websites</strong>: this machine is set to Allow; a fresh copy would be Block.`,
		`<strong>Rule <span class="nm">os_exec(systemctl status *)</span></strong>: this machine doesn&#39;t have this rule; a fresh copy would (Don&#39;t ask). It is one of the fleet&#39;s rules.`,
		`<strong>Rule <span class="nm">os_exec(uptime *)</span></strong>: only this machine has this rule (Don&#39;t ask); a fresh copy would not.`,
		"Yes, copy again", "Yes, apply fleet rules",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the drift view is missing %q", want)
		}
	}
	if n := strings.Count(page, `<div class="perm"><div class="perm-text"><strong>`); n != 4 {
		t.Errorf("the drift view lists %d differences, want 4:\n%s", n, page)
	}

	// The API row says the same, to the key; it has nothing to post to.
	var api struct {
		Master   bool `json:"master"`
		HeldHere bool `json:"held_here"`
		Machines []struct {
			ID, Name, State, Memory string
			Online                  bool
			Reported                map[string]string `json:"reported_by_the_machine"`
			Permissions             map[string]string
			ToolRules               map[string]string `json:"tool_rules"`
			Drift                   []map[string]string
		}
		Rules map[string]string `json:"fleet_rules"`
	}
	a := d.send(http.MethodGet, "/api/fleet", nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+fleetDashKey) })
	if err := json.Unmarshal([]byte(a.body), &api); err != nil || a.code != http.StatusOK || len(api.Machines) != 1 {
		t.Fatalf("GET /api/fleet: %d %v\n%s", a.code, err, a.body)
	}
	if m := api.Machines[0]; !api.Master || !api.HeldHere || m.ID != ch.id || m.Name != "web1" || m.State != "confirmed" || !m.Online || m.Memory != "not_available" ||
		m.Reported["user"] != `"root"` || m.Permissions["exec"] != "allow" || m.ToolRules["os_exec(uptime *)"] != "allow" || len(m.Drift) != 4 ||
		m.Drift[0]["category"] != "exec" || m.Drift[0]["have"] != "allow" || m.Drift[0]["want"] != "ask" || api.Rules["os_exec(systemctl status *)"] != "allow" {
		t.Errorf("GET /api/fleet:\n%s", a.body)
	}
	if a := d.send(http.MethodGet, "/api/fleet", nil, nil); a.code != http.StatusUnauthorized {
		t.Errorf("GET /api/fleet signed out = %d", a.code)
	}

	// Apply fleet rules: the fleet's rule is back, nothing else moves.
	if a := d.send(http.MethodPost, "/fleet/scheme", url.Values{"machine": {ch.id}, "from": {"defaults"}}, d.browser); a.code != http.StatusPreconditionRequired {
		t.Errorf("Apply fleet rules without its second click = %d, want 428", a.code)
	}
	if a := d.post("/fleet/scheme", "machine", ch.id, "from", "defaults"); a.msg != "The fleet's rules are laid over the approval settings of web1 again." {
		t.Errorf("Apply fleet rules: msg %q, err %q", a.msg, a.errMsg)
	}
	if scheme = d.l.schemeOf(ch.id); scheme.ToolRules["os_exec(systemctl status *)"] != "allow" || scheme.Permissions["exec"] != "allow" || scheme.ToolRules["os_exec(uptime *)"] != "allow" {
		t.Fatalf("after Apply fleet rules: %+v", scheme)
	}
	// Copy again: a fresh copy of the master's scheme as it is now.
	if a := d.post("/fleet/scheme", "machine", ch.id, "from", "master"); a.msg != "web1 has a fresh copy of this Tenant's approval settings." {
		t.Errorf("Copy again: msg %q, err %q", a.msg, a.errMsg)
	}
	scheme = d.l.schemeOf(ch.id)
	if scheme.Permissions["exec"] != "ask" || scheme.Permissions["web"] != "deny" || scheme.Permissions["mcp"] != "deny" || len(scheme.ToolRules) != 3 ||
		scheme.ToolRules["os_exec(git status *)"] != "ask" || scheme.ToolRules["os_exec(systemctl status *)"] != "allow" || scheme.ToolRules["write_note"] != "deny" {
		t.Fatalf("after Copy again: %+v", scheme)
	}
	if page = d.page("/fleet/" + ch.id); !strings.Contains(page, "No differences") {
		t.Errorf("after Copy again the machine still differs:\n%s", page)
	}
	if got := d.records(catalog.FleetMachineChanged); !hasRecord(got, "operator@dashboard", "machine="+ch.id, "change=scheme changed", "the fleet's rules laid over") ||
		!hasRecord(got, "operator@dashboard", "machine="+ch.id, "change=scheme changed", "a fresh copy of the master's local scheme") {
		t.Errorf("no Security records of the two copies:\n%s", strings.Join(got, "\n"))
	}
	if got := d.records(catalog.ModeChanged); !hasRecord(got, "operator@dashboard", "broker=fleet:"+ch.id, "category=exec", "mode=allow", "was=ask") {
		t.Errorf("no Security record of the loosened setting:\n%s", strings.Join(got, "\n"))
	}

	// Rename: the label moves, the id and what is kept under it stay.
	for _, bad := range []string{"a b", ch.id, "<b>", strings.Repeat("x", 64)} {
		if a := d.post("/fleet/rename", "machine", ch.id, "name", bad); a.msg != "" || !strings.HasPrefix(a.errMsg, "Couldn't rename that machine. A name is up to 63 letters") || strings.Contains(a.errMsg, "<b>") {
			t.Errorf("rename to %q: msg %q, err %q", bad, a.msg, a.errMsg)
		}
	}
	if a := d.post("/fleet/rename", "machine", ch.id, "name", "edge1"); a.errMsg != "" || a.page != "/fleet/"+ch.id {
		t.Errorf("rename: %s, err %q", a.page, a.errMsg)
	}
	if m, _ := fleet.Open(d.l.cfg).Machine(ch.id); m.Name != "edge1" || len(d.l.schemeOf(ch.id).ToolRules) != 3 {
		t.Fatalf("after the rename: %q, scheme %+v", m.Name, d.l.schemeOf(ch.id))
	}

	// Revoke asks first. Keeping the settings: the link closes for good and the
	// scheme stays under the id. Then its record is deleted with them.
	if a := d.send(http.MethodPost, "/fleet/revoke", url.Values{"machine": {ch.id}, "settings": {"keep"}}, d.browser); a.code != http.StatusPreconditionRequired || !online(d.mgr, ch.id) {
		t.Fatalf("revoke without its second click = %d, online %v", a.code, online(d.mgr, ch.id))
	}
	if a := d.post("/fleet/revoke", "machine", ch.id, "settings", "keep"); a.page != "/fleet/"+ch.id || !strings.Contains(a.msg, "edge1 is revoked: it can no longer connect. Its approval settings are kept under its id.") {
		t.Errorf("revoke: to %s, msg %q, err %q", a.page, a.msg, a.errMsg)
	}
	select {
	case err := <-done:
		if !errors.Is(err, fleet.ErrRevoked) {
			t.Errorf("the revoked machine's link ended with %v, want ErrRevoked", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the revoked machine's link is still up: %s", client.Status().State)
	}
	if m, _ := fleet.Open(d.l.cfg).Machine(ch.id); m.State != fleet.StateRevoked || len(d.l.schemeOf(ch.id).ToolRules) != 3 {
		t.Fatalf("after the revoke: %s", m.State)
	}
	if page = d.page("/fleet/" + ch.id); !strings.Contains(page, "This machine is revoked") || strings.Contains(page, `action="/fleet/permission"`) {
		t.Errorf("the revoked machine's page:\n%s", page)
	}
	if a := d.post("/fleet/scheme", "machine", ch.id, "from", "master"); a.msg != "" || a.errMsg == "" {
		t.Errorf("a copy for a revoked machine: msg %q, err %q", a.msg, a.errMsg)
	}
	if a := d.post("/fleet/revoke", "machine", ch.id, "settings", "delete"); a.page != "/fleet" || !strings.Contains(a.msg, "its record and approval settings are deleted") {
		t.Errorf("delete: to %s, msg %q, err %q", a.page, a.msg, a.errMsg)
	}
	if _, err := fleet.Open(d.l.cfg).Machine(ch.id); !errors.Is(err, fleet.ErrUnknown) {
		t.Fatalf("the deleted machine's record: %v", err)
	}
	if _, ok := d.l.saved().FleetSchemes[ch.id]; ok {
		t.Fatal("the deleted machine's scheme is still in the settings file")
	}
	if a := d.send(http.MethodGet, "/fleet/"+ch.id, nil, func(r *http.Request) { r.AddCookie(d.cookie) }); a.code != http.StatusSeeOther || a.page != "/fleet" || !strings.Contains(a.errMsg, "isn't part of this fleet") {
		t.Errorf("the deleted machine's page: %d to %s saying %q", a.code, a.page, a.errMsg)
	}
	if got := d.records(catalog.FleetMachineChanged); !hasRecord(got, "operator@dashboard", "change=renamed", "now edge1") || !hasRecord(got, "operator@dashboard", "change=revoked") || !hasRecord(got, "operator@dashboard", "change=removed") {
		t.Errorf("the Security records of the rename and the revokes:\n%s", strings.Join(got, "\n"))
	}
}

// --- what a machine says about itself ---

// fleetViewOnly is a dashboard.FleetControl that only shows a view.
type fleetViewOnly struct {
	dashboard.FleetControl
	v dashboard.FleetView
}

func (f *fleetViewOnly) View() dashboard.FleetView { return f.v }

// fleetPager is a dashboard that shows one machine at a time.
type fleetPager struct {
	srv  *dashboard.Server
	ctl  *fleetViewOnly
	data string // the data dir whose memory record the page reads ("" for none)
}

func newFleetPager() *fleetPager {
	p := &fleetPager{srv: dashboard.New(dashboard.Config{}, nil, nil, nil, nil, nil), ctl: &fleetViewOnly{}}
	p.srv.SetFleet(p.ctl)
	return p
}

// pages renders the list and the machine's own page for one machine, as the
// adapter hands it to the dashboard (dashMachine).
func (p *fleetPager) pages(t testing.TB, m fleet.Machine, link *fleet.MachineStatus) (list, one string) {
	t.Helper()
	mv := dashMachine(m, link)
	mv.Memory, mv.MemoryDetail = memoryStateIn(p.data, m.ID)
	p.ctl.v = dashboard.FleetView{Master: true, Here: link != nil, Holder: "this process holds the fleet, on 127.0.0.1:9777", Machines: []dashboard.FleetMachine{mv}}
	get := func(path string) string {
		rec := httptest.NewRecorder()
		p.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	return get("/fleet"), get("/fleet/" + m.ID)
}

func fleetPages(t *testing.T, m fleet.Machine, link *fleet.MachineStatus) (list, one string) {
	t.Helper()
	return newFleetPager().pages(t, m, link)
}

// saidEverywhere is a confirmed, linked machine that says said wherever a
// machine has a say: its name, what it reports, where it connected from and
// why its link ended.
func saidEverywhere(said string) (fleet.Machine, *fleet.MachineStatus) {
	m := fleet.Machine{ID: "m0123456789abcdef", Name: said, Fingerprint: strings.Repeat("ab", 32), State: fleet.StateConfirmed,
		Reported: fleet.Reported{OS: said, User: said, Version: said, Sandbox: said, Ceiling: said}, EnrolledAt: time.Unix(1_700_000_000, 0), ConfirmedAt: time.Unix(1_700_000_300, 0)}
	return m, &fleet.MachineStatus{Machine: m, Online: true, Since: time.Unix(1_700_000_400, 0), Link: 1, Remote: said, InFlight: 1, LastError: said}
}

// oddRunes is every character of page that is not printable ASCII or a line end.
func oddRunes(page string) map[rune]bool {
	out := map[rune]bool{}
	for _, r := range page {
		if r != '\n' && r != '\r' && (r < 0x20 || r > 0x7e) {
			out[r] = true
		}
	}
	return out
}

// Whatever a machine says about itself reaches the page as the terminal's
// screens show it: escaped to printable ASCII, cut short, in quotes it cannot
// close, under words that say whose it is. Byte for byte, in every place: it
// adds no markup, no line and no character the page does not have for a
// machine that says "linux", and it cannot pass for the page's own words.
func TestFleetDashShowsWhatAMachineSaysAsItsWords(t *testing.T) {
	const id = "m0123456789abcdef"
	machine, odd := saidEverywhere, oddRunes
	cleanList, cleanOne := fleetPages(t, fleet.Machine{ID: id, Name: "web1", Fingerprint: strings.Repeat("ab", 32), State: fleet.StateConfirmed,
		Reported: fleet.Reported{OS: "linux", User: "linux", Version: "linux", Sandbox: "linux", Ceiling: "linux"}, EnrolledAt: time.Unix(1_700_000_000, 0), ConfirmedAt: time.Unix(1_700_000_300, 0)},
		&fleet.MachineStatus{Online: true, Since: time.Unix(1_700_000_400, 0), Link: 1, Remote: "linux", InFlight: 1, LastError: "linux"})
	for _, evil := range fleetHostile {
		m, link := machine(evil)
		list, one := fleetPages(t, m, link)
		q := pageText(fleetQuote(evil)) // its words, in quotes, as the page's bytes
		name := pageText(fleetName(evil))
		if cell := `<td class="fl-said">` + q + `</td>`; strings.Count(list, cell) != 3 { // its system, its user, its version
			t.Errorf("the list for %q has exactly %q %d times, want 3:\n%s", evil, cell, strings.Count(list, cell), list)
		}
		if want := `<a class="linkbtn" href="/fleet/` + id + `">` + name + `</a><div class="dim nm">` + id + `</div>`; !strings.Contains(list, want) {
			t.Errorf("the list for %q is missing exactly %q", evil, want)
		}
		for _, want := range []string{
			`<strong>` + name + `</strong> <span class="on-tag">online</span>`,
			`, from <span class="fl-said">` + q + `</span>, link version 1, requests in progress: 1`,
			`<tr><th>Last ended</th><td class="fl-said">` + q + `</td></tr>`,
			`<tr><th>System</th><td class="fl-said">` + q + `</td></tr>`,
			`<tr><th>Runs as</th><td class="fl-said">` + q + `</td></tr>`,
			`<tr><th>Version</th><td class="fl-said">` + q + `</td></tr>`,
			`<tr><th>Sandbox</th><td class="fl-said">` + q + `</td></tr>`,
			`<tr><th>Its own limit</th><td class="fl-said">` + q + `</td></tr>`,
		} {
			if !strings.Contains(one, want) {
				t.Errorf("the machine's page for %q is missing exactly %q", evil, want)
			}
		}
		for where, pair := range map[string][2]string{"list": {list, cleanList}, "machine's page": {one, cleanOne}} {
			got, clean := pair[0], pair[1]
			for r := range odd(got) {
				if !odd(clean)[r] {
					t.Errorf("the %s for %q carries %U, which a plain machine's does not", where, evil, r)
				}
			}
			for _, part := range []string{"\n", "<", ">", "<form", "<a ", "<td", "<tr", "<script", "<img", "<input", " data-"} {
				if g, w := strings.Count(got, part), strings.Count(clean, part); g != w {
					t.Errorf("the %s for %q has %q %d times, a plain machine's %d", where, evil, part, g, w)
				}
			}
			// Seven values and a name, each at most 64 characters as escapes.
			if len(got) > len(clean)+9*(12*64+16) {
				t.Errorf("the %s for %q is %d bytes, a plain machine's %d: its words are not cut", where, evil, len(got), len(clean))
			}
			// Where its words read like the page's own, they stand inside its quotes.
			if head := evil[:min(len(evil), 24)]; strings.HasPrefix(evil, "Reported by the machine") {
				if g, w := strings.Count(got, head), strings.Count(clean, head); g-w != strings.Count(got, "&#34;"+head) || g == w {
					t.Errorf("the %s shows %q %d times outside the machine's quotes (a plain machine's page has it %d times)", where, head, g-strings.Count(got, "&#34;"+head), w)
				}
			}
		}
	}

	// The page marks them: the footnote under the list, the heading on the
	// machine's page, and every value in quotes.
	if !strings.Contains(cleanList, "* Reported by the machine: its own words, in quotes. This Tenant has not checked them.") || !strings.Contains(cleanList, "<th>System*</th><th>Runs as*</th><th>Version*</th>") {
		t.Errorf("the list does not mark what the machines reported:\n%s", cleanList)
	}
	if !strings.Contains(cleanOne, `Reported by the machine <span class="dim">· its own words, in quotes. This Tenant has not checked them.</span>`) {
		t.Errorf("the machine's page does not mark what it reported:\n%s", cleanOne)
	}
}

// --- memory, and a fleet another process holds ---

// The memory column shows what the fleet's memory says of each machine: in a
// process that does not hold the fleet, the holder's record of it
// (<data>/fleet/states.json), and "unknown" with the reason when there is
// none. The states are the dashboard's own strings.
func TestFleetDashMemoryColumn(t *testing.T) {
	prev := theFleet.Swap(nil)
	t.Cleanup(func() { theFleet.Store(prev) })
	m := fleet.Machine{ID: "m0123456789abcdef", Name: "web1", State: fleet.StateConfirmed}
	p := newFleetPager()
	p.data = t.TempDir()
	if list, one := p.pages(t, m, nil); !strings.Contains(list, "<td>unknown</td>") || !strings.Contains(one, `<tr><th>Memory</th><td>unknown <span class="dim">known to the process that holds the fleet</span></td></tr>`) {
		t.Errorf("the memory column with no record:\n%s", one)
	}
	if err := os.MkdirAll(fleetShadowDir(p.data), 0o700); err != nil {
		t.Fatal(err)
	}
	for state, words := range map[string]string{dashboard.FleetMemoryInStep: "in step", dashboard.FleetMemoryBehind: "behind", dashboard.FleetMemoryNeedsKey: "needs recovery key"} {
		raw, _ := json.Marshal(map[string]memoryState{m.ID: {State: state, Detail: "its newest copy is from 12:03", At: time.Now()}})
		if err := os.WriteFile(fleetMemoryStatesPath(p.data), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		list, one := p.pages(t, m, nil)
		if !strings.Contains(list, "<td>"+words+"</td>") || !strings.Contains(one, `<tr><th>Memory</th><td>`+words+` <span class="dim">its newest copy is from 12:03 (as the process that holds the fleet recorded it at `) {
			t.Errorf("memory %s: the page does not say %q:\n%s", state, words, one)
		}
	}
}

// When another process holds the fleet, the page says which, shows no
// machine as online or offline, and its changes still work: they go through
// the files, which the holder watches.
func TestFleetDashWhenAnotherProcessHoldsTheFleet(t *testing.T) {
	l := newFleetLab(t)
	ctl := newDashFleet(l.c, &settings{}, &l.stgMu)
	if v := ctl.View(); v.Master || len(v.Machines) != 0 {
		t.Fatalf("a Tenant that was never made a master: %+v", v)
	}
	l.master()
	ch := l.enroll("web1", fleet.Reported{})
	l.confirm("web1", ch)
	l.sync()
	l.stop() // this process gives the fleet up
	l.stop = nil
	since := time.Now()
	theFleet.Store(&fleetHub{cfgDir: l.cfg, store: fleet.Open(l.cfg), held: true, holder: fleet.Holder{PID: 4242, Role: "serve", Address: "0.0.0.0:9777", Since: since}})
	t.Cleanup(func() { theFleet.Store(nil) })

	ctl = newDashFleet(l.c, l.stg, &l.stgMu)
	v := ctl.View()
	if want := "tenant serve (process 4242) holds the fleet, on 0.0.0.0:9777 since " + since.Local().Format(time.DateTime); !v.Master || v.Here || v.Holder != want {
		t.Errorf("who holds the fleet: here %v, %q; want %q", v.Here, v.Holder, want)
	}
	if len(v.Machines) != 1 || v.Machines[0].Link != "" || v.Machines[0].Online || !v.Machines[0].HasScheme || len(v.Machines[0].Perms) != 8 {
		t.Fatalf("the machine, seen from a process that does not hold the fleet: %+v", v.Machines)
	}
	srv := dashboard.New(dashboard.Config{}, nil, nil, nil, nil, nil)
	srv.SetFleet(ctl)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fleet", nil))
	for _, want := range []string{v.Holder, "Only the process that holds the fleet knows which machines are online.", `<span class="dim">known to the process that holds the fleet</span>`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the list is missing %q:\n%s", want, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), ">online<") || strings.Contains(rec.Body.String(), ">offline<") {
		t.Errorf("a process that does not hold the fleet says which machines are online:\n%s", rec.Body.String())
	}
	if err := ctl.Rename(ch.id, "edge1"); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetPermission(ch.id, "web", "deny"); err != nil {
		t.Fatal(err)
	}
	if m, _ := fleet.Open(l.cfg).Machine(ch.id); m.Name != "edge1" || l.schemeOf(ch.id).Permissions["web"] != "deny" {
		t.Errorf("a change made by a process that does not hold the fleet did not reach the files: %q %v", m.Name, l.schemeOf(ch.id).Permissions)
	}
	// The control names a machine by its id alone, and says so plainly.
	var plain dashboard.PlainError
	for name, err := range map[string]error{
		"rename by name":   ctl.Rename("edge1", "x"),
		"revoke nobody":    ctl.Revoke("m0000000000000000", true),
		"a setting":        ctl.SetPermission("", "web", "deny"),
		"a rule":           ctl.SetRule("nobody", "x", "deny"),
		"a copy":           ctl.Recopy("nobody", "master"),
		"a confirmation":   ctl.Confirm("edge1", ch.code, nil),
		"an unknown field": ctl.SetPermission(ch.id, "root", "allow"),
	} {
		if err == nil || (name != "an unknown field" && !errors.As(err, &plain)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ctl.RemoveRule("nobody", "x"); !errors.As(err, &plain) {
		t.Errorf("removing a rule of nobody: %v", err)
	}
}

// When the fleet's rules or fleet.json cannot be read, the page says so and
// nothing is done on a guess: no pending machine is confirmed, no scheme is
// copied, and the raw error stays off the page.
func TestFleetDashWhenTheFilesCannotBeRead(t *testing.T) {
	l := newFleetLab(t)
	l.master()
	done := l.enroll("web1", fleet.Reported{})
	l.confirm("web1", done)
	waiting := l.enroll("web2", fleet.Reported{})
	l.sync()
	ctl := newDashFleet(l.c, l.stg, &l.stgMu)
	if v := ctl.View(); v.NoFresh || v.Err != nil || len(v.Machines) != 2 || v.Machines[1].Start == nil {
		t.Fatalf("the fleet while its files are whole: %+v", v)
	}
	for name, err := range map[string]error{
		"a copy from nowhere":                       ctl.Recopy(done.id, "evil"),
		"the fleet's rules for a machine with none": ctl.Recopy(waiting.id, "defaults"),
		"a fresh copy for a pending machine":        ctl.Recopy(waiting.id, "master"),
	} {
		if err == nil {
			t.Errorf("%s was made", name)
		}
	}

	// The fleet's rules are not a rule set: there is no fresh copy to show,
	// to compare with, or to start a machine on.
	if _, problems := defaultFleet.Load(l.saved().FleetSchemes, json.RawMessage(`{"os_exec(*)": "sometimes"}`)); len(problems) != 1 {
		t.Fatalf("the broken rules loaded with %v", problems)
	}
	v := ctl.View()
	if !v.NoFresh || v.Err != nil || len(v.Machines) != 2 || v.Machines[0].Drift != nil || !v.Machines[0].HasScheme || v.Machines[1].Start != nil {
		t.Fatalf("the fleet with rules that can't be read: %+v", v)
	}
	if err := ctl.Recopy(done.id, "master"); err == nil {
		t.Error("a fresh copy was made from rules that can't be read")
	}
	var plain dashboard.PlainError
	if err := ctl.Confirm(waiting.id, waiting.code, nil); err == nil || errors.As(err, &plain) {
		t.Errorf("a machine was confirmed on rules that can't be read: %v", err)
	}
	if m, _ := fleet.Open(l.cfg).Machine(waiting.id); m.State != fleet.StatePending {
		t.Fatalf("the machine is %s", m.State)
	}
	if _, ok := l.saved().FleetSchemes[waiting.id]; ok {
		t.Fatal("a scheme was saved for the machine that was not confirmed")
	}

	// fleet.json is not a fleet file: no machine is listed, and the page says
	// so in its own words.
	l.stop() // nothing holds the file while it is replaced
	l.stop = nil
	if err := os.WriteFile(fleet.StorePath(l.cfg), []byte(`{"version": 99, "<b>machines</b>": `), 0o600); err != nil {
		t.Fatal(err)
	}
	ctl = newDashFleet(l.c, l.stg, &l.stgMu)
	if v = ctl.View(); !v.Master || v.Err == nil || len(v.Machines) != 0 {
		t.Fatalf("the fleet with a fleet.json that can't be read: %+v", v)
	}
	for name, err := range map[string]error{"rename": ctl.Rename(done.id, "edge1"), "revoke": ctl.Revoke(done.id, true), "a setting": ctl.SetPermission(done.id, "web", "deny"),
		"a confirmation": ctl.Confirm(waiting.id, waiting.code, nil)} {
		if err == nil || errors.As(err, &plain) {
			t.Errorf("%s with fleet.json unreadable: %v, want the store's error", name, err)
		}
	}
	srv := dashboard.New(dashboard.Config{}, nil, nil, nil, nil, nil)
	srv.SetFleet(ctl)
	for _, path := range []string{"/fleet", "/fleet/" + done.id} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "The list of machines can&#39;t be read right now. Until it can, no machine is called.") ||
			strings.Contains(body, "version") && strings.Contains(body, "99") && strings.Contains(body, "machines&lt;") {
			t.Errorf("GET %s with fleet.json unreadable = %d:\n%s", path, rec.Code, body)
		}
	}
}
