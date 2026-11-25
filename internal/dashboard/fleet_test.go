package dashboard

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/control"
)

// fakeFleet is a FleetControl: it records each change as
// "op|machine|args…" and applies it to its view as the fleet does.
type fakeFleet struct {
	mu    sync.Mutex
	view  FleetView
	code  string // the six digits its pending machines show
	err   error  // what every change fails with
	calls []string
	// identity is the last recovery identity Recover was handed.
	identity string
}

// Init makes the fake a master, as the real control does.
func (f *fakeFleet) Init(listen string, addresses []string, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join([]string{"init", listen, strings.Join(addresses, " "), by}, "|"))
	if f.err != nil {
		return f.err
	}
	f.view.Master, f.view.NoRecovery, f.view.Holder = true, true, "no process holds the fleet yet"
	return nil
}

func (f *fakeFleet) View() FleetView {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.view
	v.Machines = slices.Clone(v.Machines)
	return v
}

// change records a call and finds its machine.
func (f *fakeFleet) change(op, id string, args ...string) (*FleetMachine, error) {
	f.calls = append(f.calls, strings.Join(append([]string{op, id}, args...), "|"))
	if f.err != nil {
		return nil, f.err
	}
	i := slices.IndexFunc(f.view.Machines, func(m FleetMachine) bool { return m.ID == id })
	if i < 0 {
		return nil, PlainError(fleetGone)
	}
	return &f.view.Machines[i], nil
}

func (f *fakeFleet) Confirm(id, code string, set []string, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	args := append([]string{code}, set...)
	if by != "" { // a browser that is not on this computer
		args = append(args, "by "+by)
	}
	m, err := f.change("confirm", id, args...)
	switch {
	case err != nil:
		return err
	case by != "" && !f.view.RemoteConfirm: // as the real control reads the setting again
		return PlainError("Nothing changed: confirming a machine from a browser that is not on the computer running Tenant is turned off.")
	case m.State != "pending":
		return PlainError("That machine isn't waiting to be confirmed. Nothing changed.")
	case code != f.code:
		return PlainError("Those aren't the six digits that machine shows. Nothing changed.")
	}
	m.State, m.HasScheme, m.Perms, m.Start = "confirmed", true, fleetPerms("ask"), nil
	return nil
}

func (f *fakeFleet) Rename(id, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.change("rename", id, name)
	if err == nil {
		m.Name = name
	}
	return err
}

func (f *fakeFleet) Revoke(id string, del bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.change("revoke", id, fmt.Sprint(del))
	if err != nil {
		return err
	}
	m.State = "revoked"
	if del {
		f.view.Machines = slices.DeleteFunc(f.view.Machines, func(x FleetMachine) bool { return x.ID == id })
	}
	return nil
}

func (f *fakeFleet) SetPermission(id, category, mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.change("permission", id, category, mode)
	if err != nil {
		return err
	}
	for i := range m.Perms {
		if m.Perms[i].Category == category {
			m.Perms[i].Mode = mode
		}
	}
	return nil
}

func (f *fakeFleet) SetRule(id, pattern, mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.change("rule", id, pattern, mode)
	if err == nil {
		m.Rules = append(m.Rules, ToolRuleRow{Pattern: pattern, Mode: mode})
	}
	return err
}

func (f *fakeFleet) RemoveRule(id, pattern string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.change("unrule", id, pattern)
	if err != nil {
		return false, err
	}
	n := len(m.Rules)
	m.Rules = slices.DeleteFunc(m.Rules, func(r ToolRuleRow) bool { return r.Pattern == pattern })
	return len(m.Rules) < n, nil
}

func (f *fakeFleet) Recopy(id, from string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.change("recopy", id, from)
	if err == nil && from == "master" {
		m.Drift, m.HasScheme, m.Perms = nil, true, fleetPerms("ask")
	}
	return err
}

func (f *fakeFleet) Recover(id, was string, identity []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identity = string(identity)
	clear(identity)
	if _, err := f.change("recover", id, was); err != nil {
		return "", err
	}
	if strings.TrimSpace(f.identity) != "AGE-SECRET-KEY-1TEST" {
		return "", PlainError("that is not a recovery identity")
	}
	return "generation 3 was read", nil
}

func (f *fakeFleet) Update(id, version, arch string, allowOlder bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.change("update", id, version, arch, fmt.Sprint(allowOlder))
	switch {
	case err != nil:
		return err
	case !m.Online:
		return PlainError("Nothing was sent: " + m.Name + " is offline. Nothing is queued.")
	}
	m.Update = &FleetUpdate{From: "1.2.3", To: version, Stage: "fetching", Running: true, Started: "2026-11-19 09:00",
		Lines: []string{"fetching " + version + " for linux/amd64"}}
	return nil
}

// fakeEnrollCode is the code the fake makes: the shape of a real one.
const fakeEnrollCode = "tntfleet1_eyJuIjoid2ViMSJ9-_Zz09"

func (f *fakeFleet) Enroll(name string, boot bool, by string) (FleetEnrollment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join([]string{"enroll", name, fmt.Sprint(boot), by}, "|"))
	switch {
	case f.err != nil:
		return FleetEnrollment{}, f.err
	case name == "web 1" || name == "taken":
		return FleetEnrollment{}, fmt.Errorf("fleet: %q is not a machine name", name)
	}
	q := "'" + fakeEnrollCode + "'"
	return FleetEnrollment{Name: name, Code: fakeEnrollCode, Expires: "2026-11-19 10:15", Boot: boot,
		Addresses: []string{"192.168.1.5:9777", "10.0.0.2:9777"},
		Warning:   "warning: every IP address this code carries is private, link-local or loopback: a machine at another site cannot dial these.",
		Lines: []FleetPasteLine{
			{"Linux, macOS, nothing installed", "curl -fsSL https://github.com/o/r/releases/latest/download/install.sh | TENANT_CHILD=user TENANT_ENROLL_CODE=" + q + " sh"},
			{"Windows, Tenant installed (PowerShell)", q + " | tenant child install"},
		}}, nil
}

func (f *fakeFleet) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeFleet) machine(id string) FleetMachine {
	for _, m := range f.View().Machines {
		if m.ID == id {
			return m
		}
	}
	return FleetMachine{}
}

// fleetPerms is a machine's eight safety settings, all in mode, worded as the
// control words them for a machine.
func fleetPerms(mode string) []PermissionRow {
	rows := harnessPerms(false)
	for i := range rows {
		rows[i].Mode = mode
		rows[i].Can = strings.ReplaceAll(rows[i].Can, "on this computer", "on this machine")
	}
	return rows
}

const (
	fleetPending   = "m0000000000000000" // waits for its six digits
	fleetConfirmed = "m0000000000000001" // online, with settings, rules and drift
	fleetCode      = "042917"
)

// said is s as the control hands over a machine's own words: in quotes.
func said(s string) string { return `"` + s + `"` }

// fleetFake is a fleet of n machines for the page: the first pending, the
// second online with rules and drift, then offline, unreadable and revoked
// ones, with the longest names and reported values the control can send.
func fleetFake(n int) *fakeFleet {
	f := &fakeFleet{code: fleetCode, view: FleetView{Master: true, Here: true, Holder: "this process holds the fleet, on 127.0.0.1:9777"}}
	long := said(strings.Repeat(`\u202e`, 64)) + "..." // 64 characters, each its escape: the longest a value gets
	for i := range n {
		m := FleetMachine{
			ID: fmt.Sprintf("m%016x", i), Name: longName(i), State: "confirmed", Link: "offline, last seen 2026-11-18 09:12",
			Certificate: strings.Repeat("ab", 32), Enrolled: "2026-11-17 08:00", Confirmed: "2026-11-17 08:05",
			OS: long, User: long, Version: long, Sandbox: long, Ceiling: long,
			HasScheme: true, Perms: fleetPerms([]string{"ask", "allow", "deny"}[i%3]),
		}
		switch {
		case i == 0:
			m.State, m.Confirmed, m.Link, m.HasScheme, m.Perms = "pending", "", "waits for confirmation", false, nil
			m.Start = &FleetStart{}
			for _, p := range fleetPerms("ask") {
				m.Start.Perms = append(m.Start.Perms, FleetStartPerm{PermissionRow: p, Note: "(this master: allow)"})
			}
			m.Start.Rules = []FleetStartRule{{ToolRuleRow{Pattern: "os_run(systemctl status *)", Mode: "allow"}, "fleet rule"}}
		case i == 1:
			m.Online, m.Link, m.Remote, m.LinkVersion, m.Calls = true, "online since 2026-11-18 10:02", said("192.168.1.20:50112"), 1, 2
			m.OS, m.User, m.Version, m.Sandbox, m.Ceiling = said("linux"), said("root"), said("1.2.3"), said("host"), said("full")
			m.LastEnded = long
			for j := range 12 {
				m.Rules = append(m.Rules, ToolRuleRow{Pattern: "os_run(" + longURL(j) + " *)", Mode: []string{"ask", "allow", "deny"}[j%3]})
			}
			m.Drift = []FleetDrift{
				{Key: "web", Title: "Act on websites", Have: "allow", Want: "ask"},
				{Rule: true, Key: "os_run(" + longURL(0) + " *)", Have: "allow"},
				{Rule: true, Key: "os_run(systemctl status *)", Want: "allow", FleetRule: true},
				{Rule: true, Key: "write_note", Have: "deny", Want: "ask"},
			}
		case i%5 == 3:
			m.HasScheme, m.Perms = false, nil
		case i%5 == 4:
			m.State, m.Revoked, m.Link = "revoked", "2026-11-18 11:00", "-"
		}
		f.view.Machines = append(f.view.Machines, m)
	}
	f.view.Rules = []ToolRuleRow{{Pattern: "os_run(systemctl status *)", Mode: "allow"}, {Pattern: longURL(3), Mode: "deny"}}
	return f
}

func init() {
	harnessFakes = append(harnessFakes, func(s *Server, mode fakeMode) {
		f := fleetFake(13)
		if mode == failing { // fleet.json can't be read
			f.view.Machines, f.view.Err = nil, errors.New(rawStoreClosed)
		}
		s.SetFleet(f)
	})
	harnessPages = append(harnessPages, "/fleet", "/fleet/"+fleetPending, "/fleet/"+fleetConfirmed)
}

// fleetServer is a dashboard at 127.0.0.1:9770 with f as its fleet and a
// signed-in browser's session.
func fleetServer(f FleetControl) *Server {
	s := withSession(New(Config{Addr: "127.0.0.1:9770", Auth: "s3cret"}, nil, nil, nil, nil, nil))
	if f != nil {
		s.SetFleet(f)
	}
	return s
}

// here is the signed-in browser on the computer running Tenant, sending the
// page's own form.
func here(r *http.Request) {
	r.RemoteAddr, r.Host = "127.0.0.1:50000", "127.0.0.1:9770"
	r.Header.Set("Origin", "http://127.0.0.1:9770")
	signedIn(r)
}

func and(sets ...func(*http.Request)) func(*http.Request) {
	return func(r *http.Request) {
		for _, set := range sets {
			set(r)
		}
	}
}

func header(k, v string) func(*http.Request) { return func(r *http.Request) { r.Header.Set(k, v) } }

// fleetFlash is the message a plain post's redirect carries.
func fleetFlash(rec *httptest.ResponseRecorder) (page, msg, errMsg string) {
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		return "", "", ""
	}
	return u.Path, u.Query().Get("msg"), u.Query().Get("err")
}

// --- refusals ---

// A machine is confirmed from a browser signed in on the computer running
// Tenant, and from nowhere else: not signed out, not with the API key, not
// from another address, not through `tailscale serve` or a proxy, not from
// another page. Every refusal leaves the machine pending, never reaches the
// control, and is in the Security log.
func TestFleetConfirmOnlyAtThisComputer(t *testing.T) {
	c := installCapture(t)
	f := fleetFake(3)
	s := fleetServer(f)
	s.SetTrustedHosts([]string{"box.tailnet.ts.net"})
	h := s.served()
	form := url.Values{"machine": {fleetPending}, "code": {fleetCode}}
	tailnet := func(r *http.Request) {
		r.RemoteAddr, r.Host = "127.0.0.1:50000", "box.tailnet.ts.net"
		r.Header.Set("Origin", "https://box.tailnet.ts.net")
		signedIn(r)
	}
	for _, tc := range []struct {
		name   string
		set    func(*http.Request)
		code   int
		reason string // secure()'s reason, or "" for the Fleet page's own refusal
	}{
		{"signed out", func(r *http.Request) { here(r); r.Header.Del("Cookie") }, http.StatusUnauthorized, "token"},
		{"a stale sign-in", func(r *http.Request) {
			here(r)
			r.Header.Set("Cookie", dashCookieName+"=old")
		}, http.StatusUnauthorized, "token"},
		{"no Origin", func(r *http.Request) { here(r); r.Header.Del("Origin") }, http.StatusForbidden, "csrf"},
		{"another port on this computer", and(here, header("Origin", "http://127.0.0.1:9999")), http.StatusForbidden, "origin"},
		{"another site", and(here, header("Origin", "http://evil.example")), http.StatusForbidden, "origin"},
		{"the API key", func(r *http.Request) { here(r); r.Header.Del("Cookie"); bearer("s3cret")(r) }, http.StatusForbidden, ""},
		{"the API key with no Origin", func(r *http.Request) {
			r.RemoteAddr, r.Host = "127.0.0.1:50000", "127.0.0.1:9770"
			bearer("s3cret")(r)
		}, http.StatusForbidden, ""},
		{"the API key beside a stale sign-in", func(r *http.Request) {
			here(r)
			r.Header.Set("Cookie", dashCookieName+"=old")
			bearer("s3cret")(r)
		}, http.StatusForbidden, ""},
		{"another computer", func(r *http.Request) { here(r); r.RemoteAddr = "192.168.1.20:50000" }, http.StatusForbidden, ""},
		{"another computer over IPv6", func(r *http.Request) { here(r); r.RemoteAddr = "[2001:db8::1]:50000" }, http.StatusForbidden, ""},
		{"an address that is not one", func(r *http.Request) { here(r); r.RemoteAddr = "localhost:50000" }, http.StatusForbidden, ""},
		{"the tailnet name", tailnet, http.StatusForbidden, ""},
		{"tailscale serve", and(tailnet, header("X-Forwarded-For", "100.64.0.7"), header("Tailscale-User-Login", "op@example.com")), http.StatusForbidden, ""},
		{"a proxy that keeps the loopback name: Forwarded", and(here, header("Forwarded", "for=203.0.113.9")), http.StatusForbidden, ""},
		{"a proxy: Via", and(here, header("Via", "1.1 proxy")), http.StatusForbidden, ""},
		{"a proxy: X-Forwarded-For", and(here, header("X-Forwarded-For", "203.0.113.9")), http.StatusForbidden, ""},
		{"a proxy: X-Forwarded-Host", and(here, header("X-Forwarded-Host", "dash.example.com")), http.StatusForbidden, ""},
		{"a proxy: X-Forwarded-Proto", and(here, header("X-Forwarded-Proto", "https")), http.StatusForbidden, ""},
		{"a proxy: X-Real-IP", and(here, header("X-Real-IP", "203.0.113.9")), http.StatusForbidden, ""},
		{"a tunnel: CF-Connecting-IP", and(here, header("CF-Connecting-IP", "203.0.113.9")), http.StatusForbidden, ""},
		{"a tailnet header alone", and(here, header("Tailscale-User-Name", "Op")), http.StatusForbidden, ""},
	} {
		before := c.notConfirmed()
		rec := sendForm(h, http.MethodPost, "/fleet/confirm", form, tc.set)
		if rec.Code != tc.code || envelope(rec) != tc.reason {
			t.Errorf("%s: %d (refused for %q), want %d for %q:\n%s", tc.name, rec.Code, envelope(rec), tc.code, tc.reason, rec.Body.String())
		}
		if tc.reason == "" {
			if !strings.Contains(rec.Body.String(), fleetNotHere) {
				t.Errorf("%s: the refusal doesn't say where a machine is confirmed:\n%s", tc.name, rec.Body.String())
			}
			if c.notConfirmed() != before+1 || !c.hasSecurity("was not confirmed: not asked from a browser signed in on the computer running Tenant", "machine="+fleetPending, "@dashboard") {
				t.Errorf("%s: no Security record of the refusal:\n%s", tc.name, strings.Join(c.security(), "\n"))
			}
		}
		if got := f.called(); len(got) != 0 {
			t.Fatalf("%s reached the control: %q", tc.name, got)
		}
		if st := f.machine(fleetPending).State; st != "pending" {
			t.Fatalf("%s: the machine is %s", tc.name, st)
		}
	}

	// The same form from the browser on this computer, by each loopback name.
	for i, set := range []func(*http.Request){
		here,
		func(r *http.Request) {
			here(r)
			r.Host = "localhost:9770"
			r.Header.Set("Origin", "http://localhost:9770")
		},
		func(r *http.Request) {
			here(r)
			r.RemoteAddr, r.Host = "[::1]:50000", "[::1]:9770"
			r.Header.Set("Origin", "http://[::1]:9770")
		},
	} {
		f = fleetFake(3)
		s.SetFleet(f)
		rec := sendForm(h, http.MethodPost, "/fleet/confirm", form, set)
		page, msg, errMsg := fleetFlash(rec)
		if rec.Code != http.StatusSeeOther || page != "/fleet/"+fleetPending || errMsg != "" || !strings.Contains(msg, "is confirmed") {
			t.Errorf("the browser on this computer (%d): %d to %q, msg %q, err %q", i, rec.Code, page, msg, errMsg)
		}
		if got := f.called(); !slices.Equal(got, []string{"confirm|" + fleetPending + "|" + fleetCode}) || f.machine(fleetPending).State != "confirmed" {
			t.Errorf("the browser on this computer (%d): calls %q, machine %s", i, got, f.machine(fleetPending).State)
		}
	}
}

// With remote confirmation turned on at the master's terminal (TEN-545), a
// signed-in browser that is not on this computer confirms a machine by giving
// the sign-in key again. A request that carries only the API key still confirms
// nothing, a session without the key confirms nothing, the key is never taken from the address, wrong keys
// are slowed per address as sign-in's are, and every refusal and the
// confirmation itself say they were asked from elsewhere. Turned off, the same
// requests are refused as before, key or no key.
func TestFleetConfirmFromElsewhere(t *testing.T) {
	c := installCapture(t)
	now := time.Now()
	loginNow = func() time.Time { return now }
	defer func() { loginNow = time.Now }()
	f := fleetFake(3)
	f.view.RemoteConfirm = true
	s := fleetServer(f)
	s.SetTrustedHosts([]string{"box.tailnet.ts.net"})
	h := s.served()
	// `tailscale serve`: a loopback peer, the tailnet's name, the proxy's headers.
	tailnet := func(r *http.Request) {
		r.RemoteAddr, r.Host = "127.0.0.1:50000", "box.tailnet.ts.net"
		r.Header.Set("Origin", "https://box.tailnet.ts.net")
		r.Header.Set("X-Forwarded-For", "100.64.0.7")
		signedIn(r)
	}
	// Another computer that reaches the dashboard by a name it answers to.
	lan := func(r *http.Request) {
		tailnet(r)
		r.Header.Del("X-Forwarded-For")
		r.RemoteAddr = "192.168.1.20:50000"
	}
	form := func(code, key string) url.Values {
		v := url.Values{"machine": {fleetPending}, "code": {code}}
		if key != "" {
			v.Set("key", key)
		}
		return v
	}
	by := func(addr string) string { return "by dashboard session " + sessionTag(testSession) + " from " + addr }
	pending := func(why string, calls int) {
		t.Helper()
		if got := f.called(); len(got) != calls || f.machine(fleetPending).State != "pending" {
			t.Fatalf("%s: calls %q, machine %s", why, got, f.machine(fleetPending).State)
		}
	}

	// Never the API key, with or without the page's Origin, key field or not.
	for name, set := range map[string]func(*http.Request){
		"the API key": func(r *http.Request) { tailnet(r); r.Header.Del("Cookie"); bearer("s3cret")(r) },
		"the API key with no Origin": func(r *http.Request) {
			r.RemoteAddr, r.Host = "192.168.1.20:50000", "box.tailnet.ts.net"
			bearer("s3cret")(r)
		},
		"the API key beside a stale sign-in": func(r *http.Request) {
			tailnet(r)
			r.Header.Set("Cookie", dashCookieName+"=old")
			bearer("s3cret")(r)
		},
	} {
		rec := sendForm(h, http.MethodPost, "/fleet/confirm", form(fleetCode, "s3cret"), set)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), fleetNotHere) {
			t.Errorf("%s: %d\n%s", name, rec.Code, rec.Body.String())
		}
		pending(name, 0)
	}

	// A session alone is not enough: no key, a wrong key, the key in the address.
	for _, tc := range []struct {
		name, target string
		form         url.Values
		said, record string
	}{
		{"no key", "/fleet/confirm", form(fleetCode, ""), fleetKeyWrong, "the sign-in key given again was wrong"},
		{"a wrong key", "/fleet/confirm", form(fleetCode, "s3cre"), fleetKeyWrong, "the sign-in key given again was wrong"},
		{"the session's own id as the key", "/fleet/confirm", form(fleetCode, testSession), fleetKeyWrong, "the sign-in key given again was wrong"},
		{"the key in the address", "/fleet/confirm?key=s3cret", form(fleetCode, ""), fleetKeyInURL, "the sign-in key was sent in the address"},
		{"the key in the address and in the form", "/fleet/confirm?key=s3cret", form(fleetCode, "s3cret"), fleetKeyInURL, "the sign-in key was sent in the address"},
	} {
		s.loginOK("127.0.0.1:1") // each case starts with no wrong key counted
		before := c.notConfirmed()
		rec := sendForm(h, http.MethodPost, tc.target, tc.form, tailnet)
		page, _, errMsg := fleetFlash(rec)
		if rec.Code != http.StatusSeeOther || page != "/fleet/"+fleetPending || errMsg != tc.said {
			t.Errorf("%s: %d to %q saying %q", tc.name, rec.Code, page, errMsg)
		}
		if strings.Contains(rec.Header().Get("Location"), "s3cret") {
			t.Errorf("%s: the key is in the answer's address: %s", tc.name, rec.Header().Get("Location"))
		}
		if c.notConfirmed() != before+1 || !c.hasSecurity("was not confirmed: "+tc.record, "machine="+fleetPending, "@"+surfaceRemote) {
			t.Errorf("%s: no Security record of the refusal, from elsewhere:\n%s", tc.name, strings.Join(c.security(), "\n"))
		}
		pending(tc.name, 0)
	}

	// Wrong keys are slowed per address, as sign-in's are, and the wait refuses
	// the right key too, before the digits are looked at.
	s.loginOK("127.0.0.1:1")
	for range loginFreeTries + 1 {
		sendForm(h, http.MethodPost, "/fleet/confirm", form(fleetCode, "guess"), tailnet)
	}
	rec := sendForm(h, http.MethodPost, "/fleet/confirm", form(fleetCode, "s3cret"), tailnet)
	if _, _, errMsg := fleetFlash(rec); !strings.Contains(errMsg, "too many wrong keys from this address") {
		t.Errorf("the right key during the wait: %q", errMsg)
	}
	if !c.hasSecurity("was not confirmed: too many wrong sign-in keys from this address", "@"+surfaceRemote) {
		t.Errorf("no Security record of the wait:\n%s", strings.Join(c.security(), "\n"))
	}
	pending("during the wait", 0)
	// The wait is the address's: another computer is not held up by it. With
	// the right key its digits are looked at: wrong ones reach the control and
	// count against the machine, as at this computer.
	rec = sendForm(h, http.MethodPost, "/fleet/confirm", form("000000", "s3cret"), lan)
	if _, _, errMsg := fleetFlash(rec); !strings.Contains(errMsg, "aren't the six digits") {
		t.Errorf("wrong digits with the right key: %q", errMsg)
	}
	want := []string{"confirm|" + fleetPending + "|000000|" + by("192.168.1.20:50000")}
	if got := f.called(); !slices.Equal(got, want) || f.machine(fleetPending).State != "pending" {
		t.Fatalf("wrong digits with the right key: calls %q, want %q", got, want)
	}
	if s.fails["fleet confirm "+fleetPending].n != 1 {
		t.Errorf("wrong digits from elsewhere were not counted against the machine: %v", s.fails)
	}

	// The right key and the right digits, through `tailscale serve` once its
	// address's wait is over, with a setting chosen for the machine.
	now = now.Add(2 * time.Second)
	v := form(fleetCode, "s3cret")
	v["set"] = []string{"", "web=deny"}
	rec = sendForm(h, http.MethodPost, "/fleet/confirm", v, tailnet)
	page, msg, errMsg := fleetFlash(rec)
	if rec.Code != http.StatusSeeOther || page != "/fleet/"+fleetPending || errMsg != "" || !strings.Contains(msg, "is confirmed") {
		t.Errorf("from elsewhere with the key: %d to %q, msg %q, err %q", rec.Code, page, msg, errMsg)
	}
	// Every request `tailscale serve` passes on comes from 127.0.0.1: the record
	// also holds what the proxy said of who it was, in quotes, as not checked.
	want = append(want, "confirm|"+fleetPending+"|"+fleetCode+"|web=deny|"+by("127.0.0.1:50000")+`; its proxy says "100.64.0.7" (not checked)`)
	if got := f.called(); !slices.Equal(got, want) || f.machine(fleetPending).State != "confirmed" {
		t.Errorf("from elsewhere with the key: calls %q, want %q; machine %s", got, want, f.machine(fleetPending).State)
	}
	if strings.Contains(rec.Header().Get("Location"), "s3cret") {
		t.Errorf("the key is in the answer's address: %s", rec.Header().Get("Location"))
	}

	// The browser on this computer needs no key, on or off, and is not "by" anyone.
	f = fleetFake(3)
	f.view.RemoteConfirm = true
	s.SetFleet(f)
	if rec := sendForm(h, http.MethodPost, "/fleet/confirm", form(fleetCode, ""), here); !slices.Equal(f.called(), []string{"confirm|" + fleetPending + "|" + fleetCode}) {
		t.Errorf("the browser on this computer, remote confirmation on: %d, calls %q", rec.Code, f.called())
	}

	// Turned off (the default), the key opens nothing.
	f = fleetFake(3)
	s.SetFleet(f)
	for name, set := range map[string]func(*http.Request){"tailscale serve": tailnet, "another computer": lan} {
		rec := sendForm(h, http.MethodPost, "/fleet/confirm", form(fleetCode, "s3cret"), set)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), fleetNotHere) {
			t.Errorf("turned off, %s with the key: %d\n%s", name, rec.Code, rec.Body.String())
		}
		pending("turned off, "+name, 0)
	}
}

// What a proxy on this computer says of who it passed on goes into the record
// of a confirmation from elsewhere as another party's words: the tailnet login
// before the forwarded address, cut short, in quotes it cannot close, and only
// from a peer on this computer (a request from another computer has no proxy
// here to speak for it, and could say anything).
func TestProxySaid(t *testing.T) {
	for _, tc := range []struct {
		name, remote string
		headers      map[string]string
		want         string
	}{
		{"tailscale serve", "127.0.0.1:50000", map[string]string{"Tailscale-User-Login": "op@example.com", "X-Forwarded-For": "100.64.0.7"}, `; its proxy says "op@example.com" (not checked)`},
		{"a proxy that only forwards", "[::1]:50000", map[string]string{"X-Forwarded-For": "203.0.113.9, 10.0.0.1"}, `; its proxy says "203.0.113.9, 10.0.0.1" (not checked)`},
		{"no proxy header", "127.0.0.1:50000", nil, ""},
		{"another computer saying it is someone", "192.168.1.20:50000", map[string]string{"Tailscale-User-Login": "op@example.com"}, ""},
		{"words that try to close the quote and start a line", "127.0.0.1:50000", map[string]string{"Tailscale-User-Login": "x\" (checked)\noperator@dashboard " + string(rune(0x202e))},
			`; its proxy says "x\" (checked)\noperator@dashboard ` + "\\" + `u202e" (not checked)`},
		{"more than is kept", "127.0.0.1:50000", map[string]string{"X-Forwarded-For": strings.Repeat("9", 500)}, `; its proxy says "` + strings.Repeat("9", 128) + `" (not checked)`},
	} {
		r := httptest.NewRequest(http.MethodPost, "/fleet/confirm", nil)
		r.RemoteAddr = tc.remote
		for k, v := range tc.headers {
			r.Header.Set(k, v)
		}
		if got := proxySaid(r); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Refusing a request that carries only the API key keeps a script from
// confirming by accident. It is not a boundary against whoever holds the key
// (DESIGN.md §5): the API key is the sign-in key, so a script that signs in
// with it and sends the form with the dashboard's Origin is a signed-in
// browser as far as the dashboard can tell. This test states that limit, so
// that nobody reads more into "never the API key" than is there.
func TestFleetConfirmFromElsewhereTheKeysHolder(t *testing.T) {
	f := fleetFake(3)
	f.view.RemoteConfirm = true
	s := New(Config{Addr: "127.0.0.1:9770", Auth: "s3cret", AllowedHosts: []string{"box.tailnet.ts.net"}}, nil, nil, nil, nil, nil)
	s.SetFleet(f)
	h := s.served()
	script := func(r *http.Request) {
		r.RemoteAddr, r.Host = "192.168.1.20:50000", "box.tailnet.ts.net"
		r.Header.Set("Origin", "https://box.tailnet.ts.net")
	}
	rec := sendForm(h, http.MethodPost, "/auth/login", url.Values{"token": {"s3cret"}}, script)
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == dashCookieName {
			session = c
		}
	}
	if session == nil {
		t.Fatalf("the script's sign-in set no session: %d", rec.Code)
	}
	form := url.Values{"machine": {fleetPending}, "code": {fleetCode}, "key": {"s3cret"}}
	signed := func(r *http.Request) { script(r); r.AddCookie(session) }
	rec = sendForm(h, http.MethodPost, "/fleet/confirm", form, signed)
	if _, msg, _ := fleetFlash(rec); !strings.Contains(msg, "is confirmed") || f.machine(fleetPending).State != "confirmed" {
		t.Errorf("the key's holder, signed in from a script: %d %q, machine %s", rec.Code, msg, f.machine(fleetPending).State)
	}
	// Turned off, the same script is refused: off is what holds against the key.
	f = fleetFake(3)
	s.SetFleet(f)
	rec = sendForm(h, http.MethodPost, "/fleet/confirm", form, signed)
	if rec.Code != http.StatusForbidden || f.machine(fleetPending).State != "pending" || len(f.called()) != 0 {
		t.Errorf("turned off, the key's holder: %d, machine %s, calls %q", rec.Code, f.machine(fleetPending).State, f.called())
	}
}

// The machine's page offers the form with a key field to a signed-in browser
// elsewhere only while remote confirmation is on; otherwise it says where a
// machine is confirmed and how the operator turns this on. The browser on
// this computer never gets a key field.
func TestFleetConfirmFormFromElsewhere(t *testing.T) {
	f := fleetFake(3)
	s := fleetServer(f)
	s.SetTrustedHosts([]string{"box.tailnet.ts.net"})
	elsewhere := func(r *http.Request) {
		r.RemoteAddr, r.Host = "127.0.0.1:50000", "box.tailnet.ts.net"
		r.Header.Set("X-Forwarded-For", "100.64.0.7")
		signedIn(r)
	}
	local := func(r *http.Request) { here(r); r.Header.Del("Origin") }
	page := func(set func(*http.Request)) string {
		rec := sendForm(s.served(), http.MethodGet, "/fleet/"+fleetPending, nil, set)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET the machine's page: %d", rec.Code)
		}
		return rec.Body.String()
	}
	const keyField, formAction, howTo = `name="key"`, `action="/fleet/confirm"`, "tenant fleet remote-confirm on"
	if body := page(elsewhere); strings.Contains(body, keyField) || strings.Contains(body, formAction) || !strings.Contains(body, howTo) || !strings.Contains(body, "This browser can't.") {
		t.Errorf("turned off, a browser elsewhere: want no form, and how it is turned on:\n%s", body)
	}
	if body := page(local); strings.Contains(body, keyField) || !strings.Contains(body, formAction) {
		t.Errorf("turned off, the browser on this computer: want the form with no key field:\n%s", body)
	}
	f.mu.Lock()
	f.view.RemoteConfirm = true
	f.mu.Unlock()
	if body := page(elsewhere); !strings.Contains(body, keyField) || !strings.Contains(body, `type="password"`) || !strings.Contains(body, formAction) ||
		!strings.Contains(body, `name="set"`) || !strings.Contains(body, "This browser is not on the computer running Tenant") {
		t.Errorf("turned on, a browser elsewhere: want the form, the choices and the key field:\n%s", body)
	}
	if body := page(local); strings.Contains(body, keyField) || !strings.Contains(body, formAction) {
		t.Errorf("turned on, the browser on this computer: want the form with no key field:\n%s", body)
	}
}

// notConfirmed counts the Security records of a refused confirmation.
func (c *capture) notConfirmed() int {
	return strings.Count(strings.Join(c.security(), "\n"), " was not confirmed: ")
}

// hasSecurity reports a Security record holding every part.
func (c *capture) hasSecurity(parts ...string) bool {
	for _, l := range c.security() {
		if !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(l, p) }) {
			return true
		}
	}
	return false
}

// atThisComputer on its own: what counts as the browser on this computer.
func TestAtThisComputer(t *testing.T) {
	s := fleetServer(nil)
	for _, tc := range []struct {
		name string
		set  func(*http.Request)
		want bool
	}{
		{"127.0.0.1", here, true},
		{"a GET has no Origin", func(r *http.Request) { here(r); r.Header.Del("Origin") }, true},
		{"localhost, any case, with its dot", func(r *http.Request) { here(r); r.Host = "LocalHost.:9770" }, true},
		{"IPv4 mapped in IPv6", func(r *http.Request) { here(r); r.RemoteAddr = "[::ffff:127.0.0.1]:50000" }, true},
		{"anywhere in 127/8", func(r *http.Request) { here(r); r.RemoteAddr, r.Host = "127.9.9.9:1", "127.0.0.2:9770" }, true},
		{"a name that only starts with localhost", func(r *http.Request) { here(r); r.Host = "localhost.evil.example:9770" }, false},
		{"a name under localhost", func(r *http.Request) { here(r); r.Host = "dash.localhost:9770" }, false},
		{"this computer's LAN address as the name", func(r *http.Request) { here(r); r.Host = "192.168.1.5:9770" }, false},
		{"no Host", func(r *http.Request) { here(r); r.Host = "" }, false},
		{"a Host with a path", func(r *http.Request) { here(r); r.Host = "127.0.0.1:9770/x" }, false},
		{"a link-local peer with a zone", func(r *http.Request) { here(r); r.RemoteAddr = "[fe80::1%eth0]:50000" }, false},
		{"no peer address", func(r *http.Request) { here(r); r.RemoteAddr = "" }, false},
		{"a unix socket peer", func(r *http.Request) { here(r); r.RemoteAddr = "@" }, false},
		{"no sign-in", func(r *http.Request) { here(r); r.Header.Del("Cookie") }, false},
		{"the key as the cookie", func(r *http.Request) { here(r); r.Header.Set("Cookie", dashCookieName+"=s3cret") }, false},
		{"a forwarded header in another case", and(here, header("x-forwarded-for", "10.0.0.1")), false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/fleet", nil)
		tc.set(r)
		if got := s.atThisComputer(r); got != tc.want {
			t.Errorf("%s: atThisComputer = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Wrong digits, a machine that is not pending and a machine that is not
// there change nothing and say so plainly; too many wrong codes for one
// machine make the next attempts wait, without the control being asked.
func TestFleetConfirmRefusalsAndTheLimit(t *testing.T) {
	c := installCapture(t)
	now := time.Now()
	loginNow = func() time.Time { return now }
	defer func() { loginNow = time.Now }()
	f := fleetFake(3)
	s := fleetServer(f)
	confirm := func(id, code string, set func(*http.Request)) (*httptest.ResponseRecorder, string) {
		rec := sendForm(s.Handler(), http.MethodPost, "/fleet/confirm", url.Values{"machine": {id}, "code": {code}}, and(here, set))
		_, msg, errMsg := fleetFlash(rec)
		return rec, msg + errMsg
	}
	none := func(*http.Request) {}

	if _, said := confirm("m9999999999999999", fleetCode, none); said != fleetGone {
		t.Errorf("a machine that is not there: %q", said)
	}
	if _, said := confirm("", fleetCode, none); said != fleetGone {
		t.Errorf("no machine named: %q", said)
	}
	if _, said := confirm(longName(0), fleetCode, none); said != fleetGone { // its name is not its id
		t.Errorf("a machine named by its name: %q", said)
	}
	if got := f.called(); len(got) != 0 {
		t.Fatalf("a machine the fleet doesn't have reached the control: %q", got)
	}
	if _, said := confirm(fleetConfirmed, fleetCode, none); !strings.Contains(said, "isn't waiting to be confirmed") {
		t.Errorf("a machine that is not pending: %q", said)
	}

	// Wrong digits, in a plain post and in place.
	rec, said := confirm(fleetPending, "000000", none)
	if page, _, _ := fleetFlash(rec); rec.Code != http.StatusSeeOther || page != "/fleet/"+fleetPending || !strings.Contains(said, "aren't the six digits") {
		t.Errorf("wrong digits: %d to %q saying %q", rec.Code, page, said)
	}
	rec, _ = confirm(fleetPending, "", datastar)
	if body := rec.Body.String(); !strings.Contains(body, `<div id="toast" class="toast err" role="alert"><span class="toast-text">Those aren&#39;t the six digits`) || strings.Contains(body, `id="fleet-body"`) {
		t.Errorf("wrong digits in place: want the alert and the form left as it is:\n%s", body)
	}

	// Five wrong codes are free. From the sixth, each makes the next wait:
	// one second, then two, up to a minute. A wait refuses the right code too.
	for range loginFreeTries - 2 {
		confirm(fleetPending, "111111", none)
	}
	if _, said := confirm(fleetPending, "111111", none); !strings.Contains(said, "aren't the six digits") { // the sixth
		t.Fatalf("the sixth wrong code: %q", said)
	}
	asked := len(f.called())
	for _, code := range []string{"222222", fleetCode} {
		rec, said := confirm(fleetPending, code, none)
		if rec.Code != http.StatusSeeOther || said != "Too many wrong codes for this machine. Try again in a second." {
			t.Errorf("during the wait, %s: %d %q", code, rec.Code, said)
		}
	}
	if len(f.called()) != asked || f.machine(fleetPending).State != "pending" {
		t.Fatalf("an attempt during the wait reached the control: %q", f.called()[asked:])
	}
	if !c.hasSecurity("was not confirmed: too many failed attempts", "machine="+fleetPending, "operator@dashboard") {
		t.Errorf("no Security record of the limit:\n%s", strings.Join(c.security(), "\n"))
	}
	// The wait is the machine's: another machine is not held up by it.
	other := fleetFake(1).view.Machines[0]
	other.ID = "m00000000000000aa"
	f.mu.Lock()
	f.view.Machines = append(f.view.Machines, other)
	f.mu.Unlock()
	if _, said := confirm(other.ID, fleetCode, none); !strings.Contains(said, "is confirmed") {
		t.Errorf("another machine during the first one's wait: %q", said)
	}
	// It grows: the seventh wrong code costs two seconds.
	now = now.Add(1100 * time.Millisecond)
	confirm(fleetPending, "333333", none)
	now = now.Add(1100 * time.Millisecond)
	if _, said := confirm(fleetPending, fleetCode, none); !strings.Contains(said, "Too many wrong codes") {
		t.Errorf("one second into a two-second wait: %q", said)
	}
	// Once it is over the right code confirms, and the count starts again.
	now = now.Add(time.Second)
	if _, said := confirm(fleetPending, fleetCode, none); !strings.Contains(said, "is confirmed") || f.machine(fleetPending).State != "confirmed" {
		t.Fatalf("the right code after the wait: %q", said)
	}
	if _, left := s.fails["fleet confirm "+fleetPending]; left || s.loginWait("fleet confirm "+fleetPending) != 0 {
		t.Errorf("a confirmation left its count behind: %v", s.fails)
	}
}

// Many confirmations at once are taken one at a time: the wait after wrong
// codes can't be outrun by sending them together.
func TestFleetConfirmAttemptsDoNotRace(t *testing.T) {
	now := time.Now()
	loginNow = func() time.Time { return now } // the first wait never ends
	defer func() { loginNow = time.Now }()
	f := fleetFake(1)
	s := fleetServer(f)
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendForm(s.Handler(), http.MethodPost, "/fleet/confirm", url.Values{"machine": {fleetPending}, "code": {"999999"}}, here)
		}()
	}
	wg.Wait()
	if n := len(f.called()); n != loginFreeTries+1 {
		t.Errorf("%d of 40 wrong codes sent at once reached the control, want the %d before the first wait", n, loginFreeTries+1)
	}
}

// Every other change refuses a machine the fleet doesn't have, a value that
// is not one, and a loosening without its confirm step, before the control.
func TestFleetFormRefusals(t *testing.T) {
	f := fleetFake(3)
	s := fleetServer(f)
	for _, tc := range []struct {
		name, path string
		form       url.Values
		code       int
		want       string // in the flash, or the 428
	}{
		{"rename nobody", "/fleet/rename", url.Values{"machine": {"m9999999999999999"}, "name": {"web9"}}, 303, fleetGone},
		{"rename to nothing", "/fleet/rename", url.Values{"machine": {fleetConfirmed}, "name": {"  "}}, 303, "Type the machine's new name."},
		{"revoke without the second click", "/fleet/revoke", url.Values{"machine": {fleetConfirmed}, "settings": {"keep"}}, 428, notConfirmed},
		{"revoke nobody", "/fleet/revoke", url.Values{"machine": {"nobody"}, "settings": {"keep"}, "confirm": {"yes"}}, 303, fleetGone},
		{"revoke without saying what becomes of its settings", "/fleet/revoke", url.Values{"machine": {fleetConfirmed}, "confirm": {"yes"}}, 303, "Choose whether its approval settings are kept or deleted."},
		{"revoke with another word", "/fleet/revoke", url.Values{"machine": {fleetConfirmed}, "settings": {"DELETE"}, "confirm": {"yes"}}, 303, "Choose whether its approval settings are kept or deleted."},
		{"allow without the second click", "/fleet/permission", url.Values{"machine": {fleetConfirmed}, "category": {"web"}, "mode": {"allow"}}, 428, notConfirmed},
		{"allow, shouted, without the second click", "/fleet/permission", url.Values{"machine": {fleetConfirmed}, "category": {"web"}, "mode": {" ALLOW"}}, 428, notConfirmed},
		{"a mode that is not one", "/fleet/permission", url.Values{"machine": {fleetConfirmed}, "category": {"web"}, "mode": {"always"}, "confirm": {"yes"}}, 303, "Pick Ask first, Allow or Block."},
		{"a category that is not one", "/fleet/permission", url.Values{"machine": {fleetConfirmed}, "category": {"root"}, "mode": {"deny"}}, 303, "That safety check doesn't exist for this machine."},
		{"a setting of a machine with none", "/fleet/permission", url.Values{"machine": {fleetPending}, "category": {"web"}, "mode": {"deny"}}, 303, "That safety check doesn't exist for this machine."},
		{"a setting of nobody", "/fleet/permission", url.Values{"machine": {""}, "category": {"web"}, "mode": {"deny"}}, 303, fleetGone},
		{"a rule that lets a tool run, without the second click", "/fleet/rules/add", url.Values{"machine": {fleetConfirmed}, "pattern": {"write_note"}, "mode": {"allow"}}, 428, notConfirmed},
		{"a rule with no pattern", "/fleet/rules/add", url.Values{"machine": {fleetConfirmed}, "pattern": {" "}, "mode": {"deny"}}, 303, "A rule needs a tool name or pattern"},
		{"a rule for nobody", "/fleet/rules/add", url.Values{"machine": {"x"}, "pattern": {"write_note"}, "mode": {"deny"}}, 303, fleetGone},
		{"removing a rule that asks, without the second click", "/fleet/rules/remove", url.Values{"machine": {fleetConfirmed}, "pattern": {"os_run(" + longURL(0) + " *)"}}, 428, notConfirmed},
		{"removing a rule nobody has, without the second click", "/fleet/rules/remove", url.Values{"machine": {fleetConfirmed}, "pattern": {"nothing"}}, 428, notConfirmed},
		{"removing a rule nobody has", "/fleet/rules/remove", url.Values{"machine": {fleetConfirmed}, "pattern": {"nothing"}, "confirm": {"yes"}}, 303, "This machine has no such rule."},
		{"removing a rule of nobody", "/fleet/rules/remove", url.Values{"machine": {"x"}, "pattern": {"nothing"}, "confirm": {"yes"}}, 303, fleetGone},
		{"copy again without the second click", "/fleet/scheme", url.Values{"machine": {fleetConfirmed}, "from": {"master"}}, 428, notConfirmed},
		{"a copy from somewhere else", "/fleet/scheme", url.Values{"machine": {fleetConfirmed}, "from": {"evil"}, "confirm": {"yes"}}, 303, "Choose Copy again or Apply fleet rules."},
		{"a copy for nobody", "/fleet/scheme", url.Values{"machine": {"x"}, "from": {"master"}, "confirm": {"yes"}}, 303, fleetGone},
	} {
		calls := f.called()
		if tc.name == "removing a rule nobody has" {
			calls = append(calls, "unrule|"+fleetConfirmed+"|nothing") // asked, and there was none
		}
		rec := sendForm(s.Handler(), http.MethodPost, tc.path, tc.form, nil)
		_, msg, errMsg := fleetFlash(rec)
		if rec.Code != tc.code || msg != "" || !strings.Contains(errMsg+rec.Body.String(), tc.want) {
			t.Errorf("%s: %d, msg %q, err %q, want %d with %q", tc.name, rec.Code, msg, errMsg, tc.code, tc.want)
		}
		if got := f.called(); !slices.Equal(got, calls) {
			t.Errorf("%s reached the control: %q", tc.name, got[len(calls):])
		}
	}
	// A rule that lets a tool run is removed in one tap: that tightens.
	allowed := "os_run(" + longURL(1) + " *)"
	if rec := sendForm(s.Handler(), http.MethodPost, "/fleet/rules/remove", url.Values{"machine": {fleetConfirmed}, "pattern": {allowed}}, nil); rec.Code != http.StatusSeeOther {
		t.Errorf("removing a rule that allows = %d, want it through without a second click", rec.Code)
	}

	// A change the fleet refuses says so in fixed words; the reason is logged.
	c := installCapture(t)
	f.mu.Lock()
	f.err = errors.New(`fleet: "a b" is not a machine name: use up to 63 letters`)
	f.mu.Unlock()
	for path, tc := range map[string]struct {
		form url.Values
		want string
	}{
		"/fleet/confirm":      {url.Values{"machine": {fleetPending}, "code": {fleetCode}}, "Couldn't confirm that machine. Nothing changed."},
		"/fleet/rename":       {url.Values{"machine": {fleetConfirmed}, "name": {"a b"}}, "Couldn't rename that machine. A name is up to 63 letters"},
		"/fleet/revoke":       {url.Values{"machine": {fleetConfirmed}, "settings": {"keep"}}, "Couldn't finish revoking that machine."},
		"/fleet/permission":   {url.Values{"machine": {fleetConfirmed}, "category": {"web"}, "mode": {"deny"}}, "Couldn't change that safety setting."},
		"/fleet/rules/add":    {url.Values{"machine": {fleetConfirmed}, "pattern": {"x"}, "mode": {"deny"}}, "Couldn't add that rule."},
		"/fleet/rules/remove": {url.Values{"machine": {fleetConfirmed}, "pattern": {"x"}}, "Couldn't remove that rule."},
		"/fleet/scheme":       {url.Values{"machine": {fleetConfirmed}, "from": {"master"}}, "Couldn't replace that machine's approval settings."},
	} {
		rec := sendForm(s.Handler(), http.MethodPost, path, withField(tc.form, "confirm", "yes"), here)
		if _, msg, errMsg := fleetFlash(rec); msg != "" || !strings.HasPrefix(errMsg, tc.want) || strings.Contains(errMsg, "a b") {
			t.Errorf("%s failing: msg %q, err %q, want %q and not the control's own words", path, msg, errMsg, tc.want)
		}
	}
	if !c.has("2201 ", "action=fleet rename", "is not a machine name") {
		t.Errorf("the failure isn't logged as ActionFailed: %q", c.lines())
	}
}

// --- the page ---

// htmlText is s as html/template writes it into element content.
var htmlText = strings.NewReplacer("&", "&amp;", "'", "&#39;", "+", "&#43;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "\x00", "\uFFFD").Replace

// The page's states: no control, no fleet, an unreadable fleet, the list and
// each kind of machine.
func TestFleetStates(t *testing.T) {
	c := installCapture(t)
	page := func(s *Server, path string) string {
		t.Helper()
		rec := get(t, s, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	if body := page(New(Config{}, nil, nil, nil, nil, nil), "/fleet?msg=Saved.&err=Nope."); !strings.Contains(body, `<div class="card state-off">The fleet isn&#39;t available on this Tenant.</div>`) ||
		!strings.Contains(body, `<div class="banner ok" role="status">Saved.</div>`) || !strings.Contains(body, `<div class="banner err" role="alert">Nope.</div>`) {
		t.Errorf("no control: want the off card and both flashes:\n%s", body)
	}
	if body := page(fleetServer(&fakeFleet{}), "/fleet"); !strings.Contains(body, "This Tenant has no fleet yet.") || strings.Contains(body, "<table") {
		t.Errorf("no fleet: want how to start one:\n%s", body)
	}
	if body := page(fleetServer(&fakeFleet{view: FleetView{Master: true, Holder: "no process holds the fleet yet, so no machine can connect"}}), "/fleet"); !strings.Contains(body, "No machines yet.") ||
		!strings.Contains(body, "no process holds the fleet yet") || !strings.Contains(body, "No fleet rules.") {
		t.Errorf("a fleet with no machine:\n%s", body)
	}
	broken := fleetServer(&fakeFleet{view: FleetView{Master: true, Holder: "x", Err: errors.New(rawStoreClosed)}})
	for _, path := range []string{"/fleet", "/fleet/" + fleetPending} {
		if body := page(broken, path); !strings.Contains(body, `<div class="banner err" role="alert">`+htmlText(fleetUnreadable)+`</div>`) || strings.Contains(body, rawStoreClosed) {
			t.Errorf("%s with an unreadable fleet: want the plain line and not the error:\n%s", path, body)
		}
	}
	if !c.has("2201 ", "action=fleet list", rawStoreClosed) {
		t.Errorf("the reason isn't in the log: %q", c.lines())
	}

	f := fleetFake(13)
	f.view.Here, f.view.Holder = false, "tenant serve (process 4242) holds the fleet, on 0.0.0.0:9777 since 2026-11-18 09:00:00"
	f.view.Machines[2].Link = "" // nobody here knows
	s := fleetServer(f)
	list := page(s, "/fleet")
	for _, want := range []string{
		"tenant serve (process 4242) holds the fleet, on 0.0.0.0:9777 since 2026-11-18 09:00:00",
		"Only the process that holds the fleet knows which machines are online.",
		`<tr><th>Machine</th><th>State</th><th>Last call</th><th>System*</th><th>Runs as*</th><th>Version*</th><th>Memory</th><th>Approval settings</th></tr>`,
		`<a class="linkbtn" href="/fleet/` + fleetConfirmed + `">` + longName(1) + `</a><div class="dim nm">` + fleetConfirmed + `</div>`,
		`<span class="tag-new">waiting for you</span>`, `<span class="on-tag">online</span>`, `<span class="risk">offline</span>`, `<span class="act-errtag">revoked</span>`,
		`<td>online since 2026-11-18 10:02</td>`, `<td><span class="dim">known to the process that holds the fleet</span></td>`,
		`<td class="fl-said">&#34;linux&#34;</td>`, `<td class="fl-said">&#34;root&#34;</td>`, `<td class="fl-said">&#34;1.2.3&#34;</td>`,
		`<td>unknown</td>`,
		`<td>none until you confirm it</td>`, `<td>0 ask first, 8 allowed, 0 blocked, 12 rules <span class="dim">· differs from a fresh copy</span></td>`,
		`<td>none that can be read: every request to it is refused</td>`,
		"* Reported by the machine: its own words, in quotes. This Tenant has not checked them.",
		`<span class="nm">os_run(systemctl status *)</span> <span class="tag">runs without asking</span>`,
	} {
		if !strings.Contains(list, want) {
			t.Errorf("the list is missing %q", want)
		}
	}
	if strings.Count(list, `<td>unknown</td>`) != 13 {
		t.Errorf("the memory column: %d of 13 machines say it is unknown", strings.Count(list, `<td>unknown</td>`))
	}
	if strings.Count(list, `action="/fleet/`) != strings.Count(list, `action="/fleet/add"`) {
		t.Errorf("the list carries a form other than Add machine: every change to a machine is on its own page")
	}

	// A pending machine, seen from another computer: everything it would start
	// with, and where to confirm it; no form takes the digits.
	away := page(s, "/fleet/"+fleetPending)
	for _, want := range []string{
		"Reported by the machine", "its own words, in quotes. This Tenant has not checked them.",
		"Run commands, Change files, Irreversible actions, Send messages and Leave the sandbox start no looser than Ask first",
		`<strong>Run commands</strong>: Ask first <span class="dim">(this master: allow)</span>`,
		`Rule <span class="nm">os_run(systemctl status *)</span> <span class="tag">runs without asking</span> <span class="dim">fleet rule</span>`,
		"A machine is confirmed on the computer running Tenant",
	} {
		if !strings.Contains(away, want) {
			t.Errorf("a pending machine, seen from elsewhere, is missing %q", want)
		}
	}
	if strings.Contains(away, `action="/fleet/confirm"`) || strings.Contains(away, `name="code"`) || strings.Contains(away, "<select") {
		t.Errorf("a browser that can't confirm is offered the form:\n%s", away)
	}
	// On the computer running Tenant: the form, a choice per category.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/fleet/"+fleetPending, nil)
	here(r)
	r.Header.Del("Origin")
	s.Handler().ServeHTTP(rec, r)
	local := rec.Body.String()
	for _, want := range []string{
		`<form method="POST" action="/fleet/confirm" data-on:submit__prevent__throttle.1s="@post('/fleet/confirm', {contentType:'form', retryMaxCount:0})">`,
		`<input type="hidden" name="machine" value="` + fleetPending + `">`,
		`<select name="set" aria-label="Run commands">`, `<option value="exec=allow">Allow</option>`, `<option value="sandbox=deny">Block</option>`,
		`<input type="text" name="code" class="cinput short" inputmode="numeric" autocomplete="off"`,
		"Until you confirm it, this Tenant sends the machine nothing.",
	} {
		if !strings.Contains(local, want) {
			t.Errorf("a pending machine, on this computer, is missing %q", want)
		}
	}
	if strings.Count(local, `<select name="set"`) != 8 || strings.Contains(local, "A machine is confirmed on the computer running Tenant") {
		t.Errorf("the confirm form: %d choices, want one per category and no note about another computer", strings.Count(local, `<select name="set"`))
	}

	// A confirmed machine: its settings, its rules, its drift and both actions.
	one := page(s, "/fleet/"+fleetConfirmed)
	for _, want := range []string{
		`<strong>` + longName(1) + `</strong> <span class="on-tag">online</span>`,
		`online since 2026-11-18 10:02, from <span class="fl-said">&#34;192.168.1.20:50112&#34;</span>, link version 1, requests in progress: 2`,
		`<tr><th>Its own limit</th><td class="fl-said">&#34;full&#34;</td></tr>`,
		"Can create and change files on this machine without asking you.",
		`<input type="hidden" name="machine" value="` + fleetConfirmed + `"><input type="hidden" name="category" value="write"><input type="hidden" name="mode" value="deny">`,
		`<strong>Act on websites</strong>: this machine is set to Allow; a fresh copy would be Ask first.`,
		`: only this machine has this rule (Don&#39;t ask); a fresh copy would not.`,
		`<strong>Rule <span class="nm">os_run(systemctl status *)</span></strong>: this machine doesn&#39;t have this rule; a fresh copy would (Don&#39;t ask). It is one of the fleet&#39;s rules.`,
		`<strong>Rule <span class="nm">write_note</span></strong>: this machine&#39;s rule is Block; a fresh copy&#39;s would be Ask first.`,
		`<input type="hidden" name="from" value="master"><input type="hidden" name="confirm" value="yes"><button type="submit" class="btn sm danger">Yes, copy again</button>`,
		`<input type="hidden" name="from" value="defaults"><input type="hidden" name="confirm" value="yes"><button type="submit" class="btn sm">Yes, apply fleet rules</button>`,
		`<input type="hidden" name="settings" value="keep">`, `<input type="hidden" name="settings" value="delete">`,
	} {
		if !strings.Contains(one, want) {
			t.Errorf("a confirmed machine's page is missing %q", want)
		}
	}
	if strings.Contains(one, "on this computer") {
		t.Errorf("a machine's settings are worded for this computer:\n%s", one)
	}
	// One whose settings can't be read: refused, and how to mend it.
	if body := page(s, "/fleet/m0000000000000003"); !strings.Contains(body, "no approval settings that can be read, so every request to it is refused") ||
		!strings.Contains(body, "Yes, copy again") || strings.Contains(body, "Yes, apply fleet rules") || strings.Contains(body, `action="/fleet/permission"`) {
		t.Errorf("a machine with no readable settings:\n%s", body)
	}
	// With the fleet's rules unreadable there is no fresh copy to compare with
	// or to give: the page says so, and offers neither action.
	f.mu.Lock()
	f.view.NoFresh = true
	f.mu.Unlock()
	if body := page(s, "/fleet/"+fleetConfirmed); !strings.Contains(body, "A fresh copy can't be worked out right now") || strings.Contains(body, "Yes, copy again") || strings.Contains(body, "No differences") {
		t.Errorf("a machine when no fresh copy can be worked out:\n%s", body)
	}
	f.mu.Lock()
	f.view.NoFresh = false
	f.mu.Unlock()
	// A revoked one: only its record is left to delete.
	if body := page(s, "/fleet/m0000000000000004"); !strings.Contains(body, "This machine is revoked: it can no longer connect.") || !strings.Contains(body, "Yes, delete its record") ||
		strings.Contains(body, `action="/fleet/rename"`) || strings.Contains(body, `action="/fleet/permission"`) || strings.Contains(body, `value="keep"`) {
		t.Errorf("a revoked machine:\n%s", body)
	}
	// One the fleet doesn't have goes back to the list, saying so.
	rec = get(t, s, "/fleet/m9999999999999999")
	if p, _, errMsg := fleetFlash(rec); rec.Code != http.StatusSeeOther || p != "/fleet" || errMsg != fleetGone {
		t.Errorf("a machine that is not there: %d to %q saying %q", rec.Code, p, errMsg)
	}
}

// hostileSaid is what a machine might call itself, its system or its
// version. Shown raw, as if the control had not quoted it, none of it may
// reach the page as anything but text.
var hostileSaid = []string{
	`<script>alert(1)</script>`,
	`"><img src=x onerror=alert(1)>`,
	`</td></tr></table><h1>Confirmed</h1>`,
	`web1</a><form method="POST" action="/fleet/confirm">`,
	"a\u202eb\u200bc\ufeff",
	"\x1b[2J\x1b[H\x00\x08\x7f",
	"line\r\n  state       confirmed",
	`{{.ID}}{{template "ui-confirm"}}`,
	`x'"),(window.p=1),('`,
	"Reported by the machine: its own words, in quotes. This Tenant has not checked them.",
	strings.Repeat("A", 4000),
	"\xff\xfe not UTF-8",
}

// Whatever a machine says about itself is element text, exactly as
// html/template escapes it, in every place the page shows it; the page has no
// more markup for it than for a machine that says "linux".
func TestFleetReportedTextIsOnlyText(t *testing.T) {
	render := func(evil string) (list, one string) {
		f := fleetFake(2)
		m := &f.view.Machines[1]
		m.Name, m.OS, m.User, m.Version, m.Sandbox, m.Ceiling, m.Remote, m.LastEnded, m.MemoryDetail = evil, evil, evil, evil, evil, evil, evil, evil, evil
		m.Rules = []ToolRuleRow{{Pattern: evil, Mode: "allow"}}
		m.Drift = []FleetDrift{{Rule: true, Key: evil, Have: "allow"}}
		f.view.Holder, f.view.Rules = evil, []ToolRuleRow{{Pattern: evil, Mode: "deny"}}
		s := fleetServer(f)
		return get(t, s, "/fleet").Body.String(), get(t, s, "/fleet/"+fleetConfirmed).Body.String()
	}
	cleanList, cleanOne := render("linux")
	for _, evil := range hostileSaid {
		list, one := render(evil)
		esc := htmlText(evil)
		shown := htmlText(control.VisibleText(evil)) // a rule's pattern and the memory line go through visibleText
		for _, want := range []string{
			`<div>` + esc + `</div>`, // who holds the fleet
			`">` + esc + `</a><div class="dim nm">` + fleetConfirmed + `</div>`,
			`<span class="nm">` + shown + `</span> <span class="act-errtag">blocked</span>`,
		} {
			if !strings.Contains(list, want) {
				t.Errorf("the list for %q is missing exactly %q", evil, want)
			}
		}
		if cell := `<td class="fl-said">` + esc + `</td>`; strings.Count(list, cell) != 3 { // its system, its user, its version
			t.Errorf("the list for %q has exactly %q %d times, want 3", evil, cell, strings.Count(list, cell))
		}
		for _, want := range []string{
			`<strong>` + esc + `</strong> <span class="on-tag">online</span>`,
			`, from <span class="fl-said">` + esc + `</span>, link version 1, requests in progress: 2`,
			`<tr><th>Last ended</th><td class="fl-said">` + esc + `</td></tr>`,
			`<tr><th>System</th><td class="fl-said">` + esc + `</td></tr>`,
			`<tr><th>Runs as</th><td class="fl-said">` + esc + `</td></tr>`,
			`<tr><th>Version</th><td class="fl-said">` + esc + `</td></tr>`,
			`<tr><th>Sandbox</th><td class="fl-said">` + esc + `</td></tr>`,
			`<tr><th>Its own limit</th><td class="fl-said">` + esc + `</td></tr>`,
			`<span class="dim">` + shown + `</span></td></tr>`, // the memory line
			`<span class="nm">` + shown + `</span>`,
			`<strong>Rule <span class="nm">` + shown + `</span></strong>: only this machine has this rule`,
			`<input type="hidden" name="pattern" value="`,
		} {
			if !strings.Contains(one, want) {
				t.Errorf("the machine's page for %q is missing exactly %q", evil, want)
			}
		}
		for name, pair := range map[string][2]string{"list": {list, cleanList}, "machine": {one, cleanOne}} {
			got, clean := pair[0], pair[1]
			for _, tag := range []string{"<", "<form", "<a ", "<script", "<img", "<h1", "<table", "<input", " data-", "onerror=\""} {
				if g, w := strings.Count(got, tag), strings.Count(clean, tag); g != w {
					t.Errorf("the %s page for %q has %d of %q, a plain machine's has %d", name, evil, g, tag, w)
				}
			}
			if bad := injectedExprs(got); len(bad) > 0 {
				t.Errorf("the %s page for %q puts it in a Datastar expression: %q", name, evil, bad)
			}
		}
	}
}

// --- changes ---

// Every Fleet form answers a Datastar post in place: the toast, the status
// strip and #fleet-body, never a 303 the fetch would follow; a plain post
// gets the 303 and its flash. With no control the region says so.
func TestFleet_InlineAnswersEveryForm(t *testing.T) {
	forms := []struct {
		path string
		form url.Values
		call string
		msg  string
	}{
		{"/fleet/confirm", url.Values{"machine": {fleetPending}, "code": {fleetCode}, "set": {"", "exec=allow", "", "web=deny"}}, "confirm|" + fleetPending + "|" + fleetCode + "|exec=allow|web=deny", longName(0) + " is confirmed. Your agent can work on it now, as its approval settings say."},
		{"/fleet/rename", url.Values{"machine": {fleetConfirmed}, "name": {" web1 "}}, "rename|" + fleetConfirmed + "|web1", "Renamed. Its approval settings and everything else stay with it."},
		{"/fleet/permission", url.Values{"machine": {fleetConfirmed}, "category": {"web"}, "mode": {"ask"}}, "permission|" + fleetConfirmed + "|web|ask", "On web1: Act on websites asks you first."},
		{"/fleet/permission", url.Values{"machine": {fleetConfirmed}, "category": {"exec"}, "mode": {"Allow "}}, "permission|" + fleetConfirmed + "|exec|allow", "On web1: Run commands is allowed without asking."},
		{"/fleet/rules/add", url.Values{"machine": {fleetConfirmed}, "pattern": {" write_note "}, "mode": {"allow"}}, "rule|" + fleetConfirmed + "|write_note|allow", "Rule added for web1: what it matches runs without asking."},
		{"/fleet/rules/remove", url.Values{"machine": {fleetConfirmed}, "pattern": {"write_note"}}, "unrule|" + fleetConfirmed + "|write_note", "Rule removed for web1."},
		{"/fleet/scheme", url.Values{"machine": {fleetConfirmed}, "from": {"defaults"}}, "recopy|" + fleetConfirmed + "|defaults", "The fleet's rules are laid over the approval settings of web1 again."},
		{"/fleet/scheme", url.Values{"machine": {fleetConfirmed}, "from": {"master"}}, "recopy|" + fleetConfirmed + "|master", "web1 has a fresh copy of this Tenant's approval settings."},
		{"/fleet/revoke", url.Values{"machine": {fleetConfirmed}, "settings": {"keep"}}, "revoke|" + fleetConfirmed + "|false", "web1 is revoked: it can no longer connect. Its approval settings are kept under its id."},
		{"/fleet/revoke", url.Values{"machine": {fleetConfirmed}, "settings": {"delete"}}, "revoke|" + fleetConfirmed + "|true", "web1 is revoked: it can no longer connect, and its record and approval settings are deleted."},
	}
	f := fleetFake(3)
	s := fleetServer(f)
	for i, tc := range forms {
		rec := sendForm(s.Handler(), http.MethodPost, tc.path, withField(tc.form, "confirm", "yes"), and(here, datastar))
		body := rec.Body.String()
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" || rec.Header().Get("Location") != "" {
			t.Errorf("%s: %d %q to %q, want 200 text/event-stream", tc.path, rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Location"))
		}
		for _, want := range []string{
			"data: mode replace\ndata: elements <div id=\"toast\" class=\"toast ok\"><span class=\"toast-text\">" + htmlText(tc.msg) + "</span>",
			`data: elements <div id="ui-status"`, `data: elements <div id="fleet-body">`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s is missing %q:\n%.600s", tc.path, want, body)
			}
		}
		if got := f.called(); len(got) != i+1 || got[i] != tc.call {
			t.Errorf("%s: the control got %q, want %q last", tc.path, got, tc.call)
		}
	}
	// The last one deleted the machine its page was about: the region is the list.
	if last := sendForm(s.Handler(), http.MethodPost, "/fleet/rename", url.Values{"machine": {fleetConfirmed}, "name": {"x"}}, and(here, datastar)).Body.String(); !strings.Contains(last, htmlText(fleetGone)) {
		t.Errorf("a change to a machine that was just deleted:\n%.400s", last)
	}

	// A plain post: the 303, to the machine's page while the fleet has it.
	f = fleetFake(3)
	s = fleetServer(f)
	for _, tc := range forms {
		rec := sendForm(s.Handler(), http.MethodPost, tc.path, withField(tc.form, "confirm", "yes"), here)
		want := "/fleet/" + tc.form.Get("machine")
		if tc.form.Get("settings") == "delete" {
			want = "/fleet"
		}
		if page, msg, errMsg := fleetFlash(rec); rec.Code != http.StatusSeeOther || page != want || msg != tc.msg || errMsg != "" {
			t.Errorf("%s, a plain post: %d to %q, msg %q, err %q; want 303 to %s saying %q", tc.path, rec.Code, page, msg, errMsg, want, tc.msg)
		}
	}
	// No control: the region says so.
	for _, tc := range forms {
		body := sendForm(fleetServer(nil).Handler(), http.MethodPost, tc.path, withField(tc.form, "confirm", "yes"), and(here, datastar)).Body.String()
		if !strings.Contains(body, `data: elements <div id="fleet-body">`) || !strings.Contains(body, "The fleet isn&#39;t available on this Tenant.") {
			t.Errorf("%s with no control: the region doesn't say so:\n%.400s", tc.path, body)
		}
	}
}

// --- the approval card of a machine's action ---

// A request for an action on a fleet machine names the machine, says it is
// not this computer, and offers no grant for the rest of the session: that
// grant is this computer's.
func TestApprovalCardNamesTheMachine(t *testing.T) {
	q := &fakeQueue{}
	q.add(
		PendingApproval{ID: "ap-here", Category: "exec", Action: "os_exec", Detail: "uname -a", Origin: "local", SessionOK: true},
		PendingApproval{ID: "ap-web1", Category: "exec", Action: "os_exec", Detail: `[host m0123456789abcdef "web1"] uname -a`, Origin: "local", SessionOK: true,
			Host: "m0123456789abcdef", HostName: "web1"},
		PendingApproval{ID: "ap-evil", Category: "write", Action: "os_write", Detail: `[host m00000000000000ee "\u202e<b>this computer</b>"] /etc/passwd`, Origin: "local", SessionOK: true,
			Host: "m00000000000000ee", HostName: `\u202e<b>this computer</b>`},
		PendingApproval{ID: "ap-odd", Category: "other", Action: "thing", Detail: `[host m00000000000000ff "db"] x`, Origin: "local", Host: "m00000000000000ff", HostName: "db"},
	)
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetApprovals(q)
	s.SetSafety(&fakeSafety{perms: safetyPerms()}, func() SafetyFacts { return SafetyFacts{Sandboxed: true} })
	items := map[string]ApprovalItem{}
	for _, it := range s.queue().Items {
		items[it.ID] = it
	}
	for id, want := range map[string]ApprovalItem{
		"ap-here": {Title: "Your agent wants to run commands and programs on this computer", Standing: true},
		"ap-web1": {Title: `On the machine "web1": your agent wants to run commands and programs`, Machine: "m0123456789abcdef"},
		"ap-evil": {Title: `On the machine "\u202e<b>this computer</b>": your agent wants to create and change files`, Machine: "m00000000000000ee"},
		"ap-odd":  {Title: `On the machine "db": your agent is asking for your OK`, Machine: "m00000000000000ff"},
	} {
		if got := items[id]; got.Title != want.Title || got.Machine != want.Machine || got.Standing != want.Standing {
			t.Errorf("%s: title %q, machine %q, session grant %v; want %q, %q, %v", id, got.Title, got.Machine, got.Standing, want.Title, want.Machine, want.Standing)
		}
	}
	body := get(t, s, "/safety").Body.String()
	for _, want := range []string{
		`<b>On the machine &#34;web1&#34;: your agent wants to run commands and programs</b>`,
		`<div class="dim">os_exec · on the machine m0123456789abcdef, not this computer · waiting less than a minute</div>`,
		`<b>On the machine &#34;\u202e&lt;b&gt;this computer&lt;/b&gt;&#34;: your agent wants to create and change files</b>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the queue is missing exactly %q", want)
		}
	}
	if n := strings.Count(body, "Allow for the rest of this session"); n != 2 { // the summary and the step's label, for ap-here alone
		t.Errorf("the session grant is offered %d times, want it for this computer's request only", n/2)
	}
	// And the session grant for a machine's request is refused if asked for.
	rec := sendForm(s.Handler(), http.MethodPost, "/safety/approvals/ap-web1?decision=approve_session&confirm=yes", nil, nil)
	if _, _, errMsg := fleetFlash(rec); errMsg != notAvailable || len(q.calls()) != 0 {
		t.Errorf("a session grant for a machine's request: %q, decided %q", errMsg, q.calls())
	}
}

// --- GET /api/fleet ---

func TestFleetAPI(t *testing.T) {
	var none restFleet
	rec := get(t, New(Config{}, nil, nil, nil, nil, nil), "/api/fleet")
	if err := json.Unmarshal(rec.Body.Bytes(), &none); err != nil || rec.Code != http.StatusOK || none.Master || none.Machines == nil || len(none.Machines) != 0 {
		t.Fatalf("no control: %d %s (%v), want an empty fleet", rec.Code, rec.Body.String(), err)
	}
	if !strings.Contains(rec.Body.String(), `"machines":[]`) {
		t.Errorf("no control: machines is not an empty list: %s", rec.Body.String())
	}

	s := fleetServer(fleetFake(5))
	var got restFleet
	rec = get(t, s, "/api/fleet")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET /api/fleet: %v, %q", err, rec.Header().Get("Content-Type"))
	}
	if !got.Master || !got.HeldHere || got.Holder == "" || len(got.Machines) != 5 || got.Rules["os_run(systemctl status *)"] != "allow" {
		t.Errorf("the fleet: %+v", got)
	}
	p, m := got.Machines[0], got.Machines[1]
	if p.ID != fleetPending || p.State != "pending" || p.Online || p.Memory != "not_available" || p.Permissions != nil || p.Confirmed != "" {
		t.Errorf("the pending machine: %+v", p)
	}
	if m.ID != fleetConfirmed || !m.Online || m.Calls != 2 || m.Reported != (restFleetReported{`"linux"`, `"root"`, `"1.2.3"`, `"host"`, `"full"`}) ||
		len(m.Permissions) != 8 || m.Permissions["exec"] != "allow" || len(m.ToolRules) != 12 || len(m.Drift) != 4 ||
		m.Drift[0] != (restFleetDrift{Category: "web", Have: "allow", Want: "ask"}) || m.Drift[2] != (restFleetDrift{Rule: "os_run(systemctl status *)", Want: "allow"}) {
		t.Errorf("the confirmed machine: %+v", m)
	}
	if !strings.Contains(rec.Body.String(), `"reported_by_the_machine":{"os":"\"linux\""`) {
		t.Errorf("what the machine reports is not under a key that says whose words they are:\n%s", rec.Body.String())
	}
	// An unreadable fleet says so plainly.
	var bad restFleet
	rec = get(t, fleetServer(&fakeFleet{view: FleetView{Master: true, Err: errors.New(rawStoreClosed)}}), "/api/fleet")
	if _ = json.Unmarshal(rec.Body.Bytes(), &bad); bad.Error != fleetUnreadable || strings.Contains(rec.Body.String(), rawStoreClosed) {
		t.Errorf("an unreadable fleet: %s", rec.Body.String())
	}
	// It is read-only: there is no row that changes the fleet, whatever is sent.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, path := range []string{"/api/fleet", "/api/fleet/" + fleetPending, "/api/fleet/confirm", "/api/fleet/" + fleetPending + "/confirm"} {
			f := fleetFake(2)
			rec := sendForm(fleetServer(f).Handler(), method, path, url.Values{"machine": {fleetPending}, "code": {fleetCode}}, here)
			if (rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed) || len(f.called()) != 0 {
				t.Errorf("%s %s = %d, calls %q; want no such row", method, path, rec.Code, f.called())
			}
		}
	}
}

// The words of a drift line, each kind.
func TestFleetDriftWords(t *testing.T) {
	for d, want := range map[FleetDrift]string{
		{Key: "exec", Title: "Run commands", Have: "allow", Want: "ask"}:    "this machine is set to Allow; a fresh copy would be Ask first.",
		{Key: "web", Title: "Act on websites", Have: "deny", Want: "allow"}: "this machine is set to Block; a fresh copy would be Allow.",
		{Rule: true, Key: "x", Have: "deny"}:                                "only this machine has this rule (Block); a fresh copy would not.",
		{Rule: true, Key: "x", Want: "ask"}:                                 "this machine doesn't have this rule; a fresh copy would (Ask first).",
		{Rule: true, Key: "x", Have: "ask", Want: "allow", FleetRule: true}: "this machine's rule is Ask first; a fresh copy's would be Don't ask. It is one of the fleet's rules.",
	} {
		if got := d.Words(); got != want {
			t.Errorf("%+v: %q, want %q", d, got, want)
		}
	}
}

// What the list says of a machine's memory and of its approval settings.
func TestFleetMachineWords(t *testing.T) {
	for state, want := range map[string]string{FleetMemoryUnknown: "unknown", FleetMemoryInStep: "in step", FleetMemoryBehind: "behind",
		FleetMemoryNeedsKey: "needs recovery key", "something a later version says": "unknown"} {
		if got := (FleetMachine{Memory: state}).MemoryWords(); got != want {
			t.Errorf("memory %q: %q, want %q", state, got, want)
		}
	}
	for want, m := range map[string]FleetMachine{
		"none until you confirm it": {State: "pending", HasScheme: true, Perms: fleetPerms("allow")}, // one it adopted is not its own yet
		"none":                      {State: "revoked"},
		"none that can be read: every request to it is refused": {State: "confirmed"},
		"8 ask first, 0 allowed, 0 blocked":                     {State: "confirmed", HasScheme: true, Perms: fleetPerms("ask")},
		"0 ask first, 0 allowed, 8 blocked, 1 rule":             {State: "paused", HasScheme: true, Perms: fleetPerms("deny"), Rules: []ToolRuleRow{{Pattern: "x", Mode: "ask"}}},
	} {
		if got := m.Approvals(); got != want {
			t.Errorf("%+v: %q, want %q", m, got, want)
		}
	}
}

// A long wait is said in whole seconds, rounded up.
func TestFleetConfirmWaitWords(t *testing.T) {
	now := time.Now()
	loginNow = func() time.Time { return now }
	defer func() { loginNow = time.Now }()
	f := fleetFake(1)
	s := fleetServer(f)
	s.fails = map[string]loginFails{"fleet confirm " + fleetPending: {n: 9, until: now.Add(7500 * time.Millisecond)}}
	rec := sendForm(s.Handler(), http.MethodPost, "/fleet/confirm", url.Values{"machine": {fleetPending}, "code": {fleetCode}}, here)
	if _, _, errMsg := fleetFlash(rec); errMsg != "Too many wrong codes for this machine. Try again in 8 seconds." || len(f.called()) != 0 {
		t.Errorf("seven and a half seconds into a wait: %q, calls %q", errMsg, f.called())
	}
}

// --- updating a machine ---

// The Update action refuses before the control: a request that is not signed
// in, one without its second click, and a machine the fleet does not have.
// A machine the control will not update (offline) is refused in its words.
func TestFleetUpdateRefusals(t *testing.T) {
	f := fleetFake(3)
	s := fleetServer(f)
	h := s.served()
	form := url.Values{"machine": {fleetConfirmed}, "version": {"v2.0.0"}, "confirm": {"yes"}}
	for _, tc := range []struct {
		name string
		form url.Values
		set  func(*http.Request)
		code int
		want string
	}{
		{"signed out", form, func(r *http.Request) { here(r); r.Header.Del("Cookie") }, http.StatusUnauthorized, ""},
		{"a wrong API key", form, bearer("nope"), http.StatusForbidden, ""},
		{"another site", form, and(here, header("Origin", "http://evil.example")), http.StatusForbidden, ""},
		{"without the second click", withField(url.Values{"machine": {fleetConfirmed}}), here, http.StatusPreconditionRequired, notConfirmed},
		{"an unknown machine", withField(form, "machine", "m9999999999999999"), here, http.StatusSeeOther, fleetGone},
		{"no machine", withField(form, "machine", ""), here, http.StatusSeeOther, fleetGone},
	} {
		before := f.called()
		rec := sendForm(h, http.MethodPost, "/fleet/update", tc.form, tc.set)
		_, msg, errMsg := fleetFlash(rec)
		if rec.Code != tc.code || msg != "" || !strings.Contains(errMsg+rec.Body.String(), tc.want) {
			t.Errorf("%s: %d, msg %q, err %q, want %d with %q", tc.name, rec.Code, msg, errMsg, tc.code, tc.want)
		}
		if got := f.called(); !slices.Equal(got, before) {
			t.Errorf("%s reached the control: %q", tc.name, got[len(before):])
		}
		if f.machine(fleetConfirmed).Update != nil {
			t.Fatalf("%s started an update", tc.name)
		}
	}
	// An offline machine: the control's own refusal, in its plain words.
	offline := "m0000000000000002"
	rec := sendForm(h, http.MethodPost, "/fleet/update", withField(form, "machine", offline), here)
	if _, msg, errMsg := fleetFlash(rec); rec.Code != http.StatusSeeOther || msg != "" || !strings.Contains(errMsg, "is offline. Nothing is queued.") {
		t.Errorf("an offline machine: %d, msg %q, err %q", rec.Code, msg, errMsg)
	}
}

// The Update action starts the control's update, with what the form asked
// for, from a signed-in browser or with the API key (the command line), and
// the machine's page and the API then show how far it has got.
func TestFleetUpdateStartsAndShows(t *testing.T) {
	f := fleetFake(3)
	s := fleetServer(f)
	h := s.served()
	rec := sendForm(h, http.MethodPost, "/fleet/update", url.Values{"machine": {fleetConfirmed}, "version": {" v2.0.0 "}, "allow_older": {"yes"},
		"arch": {"arm64"}, "confirm": {"yes"}}, here)
	if page, msg, errMsg := fleetFlash(rec); rec.Code != http.StatusSeeOther || page != "/fleet/"+fleetConfirmed || errMsg != "" || !strings.Contains(msg, "Updating") {
		t.Fatalf("update: %d %s msg %q err %q", rec.Code, page, msg, errMsg)
	}
	if got := f.called(); !slices.Equal(got, []string{"update|" + fleetConfirmed + "|v2.0.0|arm64|true"}) {
		t.Errorf("the control was asked %q", got)
	}
	// The command line's way in: the API key, no browser.
	f.mu.Lock()
	f.view.Machines[1].Update = nil
	f.mu.Unlock()
	if rec := sendForm(h, http.MethodPost, "/fleet/update", url.Values{"machine": {fleetConfirmed}, "confirm": {"yes"}}, func(r *http.Request) {
		r.RemoteAddr, r.Host = "127.0.0.1:50000", "127.0.0.1:9770"
		bearer("s3cret")(r)
	}); rec.Code != http.StatusSeeOther {
		t.Errorf("with the API key: %d %s", rec.Code, rec.Body.String())
	}
	f.mu.Lock()
	f.view.Machines[1].Update = &FleetUpdate{From: "1.2.3", To: "2.0.0", Stage: "rolled_back", Started: "2026-11-19 09:00", Finished: "2026-11-19 09:02",
		Lines: []string{"fetched tenant_2.0.0_linux_x86_64.tar.gz (40.0 MB)", `rolled back to "1.2.3": "<b>it exited</b>"`}}
	f.mu.Unlock()
	page := get(t, s, "/fleet/"+fleetConfirmed).Body.String()
	for _, want := range []string{"rolled back", "fetched tenant_2.0.0_linux_x86_64.tar.gz", htmlText(`"<b>it exited</b>"`), `action="/fleet/update"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the machine's page lacks %q", want)
		}
	}
	if strings.Contains(page, "<b>it exited</b>") {
		t.Error("the machine's words reached the page unescaped")
	}
	var api restFleet
	if err := json.Unmarshal(get(t, s, "/api/fleet").Body.Bytes(), &api); err != nil {
		t.Fatal(err)
	}
	if u := api.Machines[1].Update; u == nil || u.Stage != "rolled_back" || u.Running || len(u.Lines) != 2 || u.To != "2.0.0" {
		t.Errorf("the API's update: %+v", u)
	}
	// An offline machine's page offers no Update; a pending one has no card.
	if page := get(t, s, "/fleet/m0000000000000002").Body.String(); strings.Contains(page, `action="/fleet/update"`) || !strings.Contains(page, "nothing is queued") {
		t.Error("the offline machine's page offers an update, or does not say why it does not")
	}
	if page := get(t, s, "/fleet/"+fleetPending).Body.String(); strings.Contains(page, `action="/fleet/update"`) {
		t.Error("a pending machine's page offers an update")
	}
}

// postIdentity is the command line's POST /fleet/recover: the machine in
// the URL, the recovery identity the whole body.
func postIdentity(h http.Handler, query, body string, set func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/fleet/recover?"+query, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/octet-stream")
	if set != nil {
		set(r)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// The recovery identity reaches the control as the request's body, from a
// signed-in browser or the API key, after the second click; never from a
// form field, another site, or for a machine that is not the fleet's. What
// the control says comes back as the flash.
func TestFleetRecover(t *testing.T) {
	f := fleetFake(3)
	s := fleetServer(f)
	h := s.served()
	ok := "machine=" + fleetConfirmed + "&confirm=yes"
	for _, tc := range []struct {
		name, query, body string
		set               func(*http.Request)
		code              int
		want              string
	}{
		{"signed out", ok, "AGE-SECRET-KEY-1TEST", func(r *http.Request) { here(r); r.Header.Del("Cookie") }, http.StatusUnauthorized, ""},
		{"a wrong API key", ok, "AGE-SECRET-KEY-1TEST", bearer("nope"), http.StatusForbidden, ""},
		{"another site", ok, "AGE-SECRET-KEY-1TEST", and(here, header("Origin", "http://evil.example")), http.StatusForbidden, ""},
		{"without the second click", "machine=" + fleetConfirmed, "AGE-SECRET-KEY-1TEST", here, http.StatusPreconditionRequired, notConfirmed},
		{"an unknown machine", "machine=m9999999999999999&confirm=yes", "AGE-SECRET-KEY-1TEST", here, http.StatusSeeOther, fleetGone},
		{"no identity", ok, "", here, http.StatusSeeOther, "the recovery identity is the body"},
		{"an identity too long to be one", ok, strings.Repeat("A", maxRecoveryIdentity+1), here, http.StatusSeeOther, "the recovery identity is the body"},
	} {
		before := f.called()
		rec := postIdentity(h, tc.query, tc.body, tc.set)
		_, msg, errMsg := fleetFlash(rec)
		if rec.Code != tc.code || msg != "" || !strings.Contains(errMsg+rec.Body.String(), tc.want) {
			t.Errorf("%s: %d, msg %q, err %q, want %d with %q", tc.name, rec.Code, msg, errMsg, tc.code, tc.want)
		}
		if got := f.called(); !slices.Equal(got, before) {
			t.Errorf("%s reached the control: %q", tc.name, got[len(before):])
		}
	}
	// As a form field it is not read: the body is the form, not an identity.
	rec := sendForm(h, http.MethodPost, "/fleet/recover", url.Values{"machine": {fleetConfirmed}, "confirm": {"yes"}, "identity": {"AGE-SECRET-KEY-1TEST"}}, here)
	if _, _, errMsg := fleetFlash(rec); !strings.Contains(errMsg, "the recovery identity is the body") || f.identity != "" {
		t.Errorf("a form field: %d, err %q, the control got %q", rec.Code, errMsg, f.identity)
	}
	rec = postIdentity(h, ok+"&was=m00000000000000aa", "AGE-SECRET-KEY-1TEST\n", here)
	if page, msg, errMsg := fleetFlash(rec); rec.Code != http.StatusSeeOther || page != "/fleet/"+fleetConfirmed || errMsg != "" || !strings.Contains(msg, "generation 3 was read") {
		t.Fatalf("recover: %d %s msg %q err %q", rec.Code, page, msg, errMsg)
	}
	if got := f.called(); got[len(got)-1] != "recover|"+fleetConfirmed+"|m00000000000000aa" {
		t.Errorf("the control was asked %q", got)
	}
	// The command line's way in, with the API key; a wrong identity is the
	// control's refusal.
	rec = postIdentity(h, ok, "AGE-SECRET-KEY-1WRONG", func(r *http.Request) {
		r.RemoteAddr, r.Host = "127.0.0.1:50000", "127.0.0.1:9770"
		bearer("s3cret")(r)
	})
	if _, _, errMsg := fleetFlash(rec); rec.Code != http.StatusSeeOther || !strings.Contains(errMsg, "not a recovery identity") {
		t.Errorf("a wrong identity with the API key: %d, err %q", rec.Code, errMsg)
	}
}

// The recovery identity is taken only over a connection that cannot be read
// on the way: from this computer (a loopback peer) or over TLS. From anywhere
// else it is refused before the handler touches the body (which the server's
// body cap, httplimits.Protect, has buffered like every POST's: on plain HTTP
// the bytes have crossed the network by then, which is why the command line
// refuses to send them), the control never sees it, and the refusal says
// nothing of what it carried; signed in or with the API key alike.
func TestFleetRecoverOnlyOverLoopbackOrTLS(t *testing.T) {
	f := fleetFake(3)
	s := fleetServer(f)
	h := s.served()
	const secret = "AGE-SECRET-KEY-1TEST"
	send := func(remote string, tlsOn bool, set func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/fleet/recover?machine="+fleetConfirmed+"&confirm=yes", strings.NewReader(secret))
		r.Header.Set("Content-Type", "application/octet-stream")
		set(r)
		r.RemoteAddr = remote
		if tlsOn {
			r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	apiKey := func(r *http.Request) { r.Host = "127.0.0.1:9770"; bearer("s3cret")(r) }
	for name, set := range map[string]func(*http.Request){"the API key": apiKey, "a signed-in browser": here} {
		before := f.called()
		rec := send("192.0.2.7:50000", false, set)
		_, msg, errMsg := fleetFlash(rec)
		if msg != "" || !strings.Contains(errMsg+rec.Body.String(), "not this computer") || strings.Contains(errMsg+rec.Body.String(), "AGE-") {
			t.Errorf("%s from another computer over plain HTTP: %d, msg %q, err %q", name, rec.Code, msg, errMsg)
		}
		if got := f.called(); !slices.Equal(got, before) {
			t.Errorf("%s from another computer reached the control: %q", name, got[len(before):])
		}
	}
	if rec := send("127.0.0.1:50000", false, apiKey); !strings.Contains(fleetMsg(rec), "generation 3 was read") {
		t.Errorf("from this computer: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := send("192.0.2.7:50000", true, apiKey); !strings.Contains(fleetMsg(rec), "generation 3 was read") {
		t.Errorf("from another computer over TLS: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func fleetMsg(rec *httptest.ResponseRecorder) string {
	_, msg, _ := fleetFlash(rec)
	return msg
}
