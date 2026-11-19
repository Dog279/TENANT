package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"tenant/internal/dashboard"
	"tenant/internal/fleet"
	"tenant/internal/logs/catalog"
)

// Add machine on a Tenant with no fleet, on a fleet with no address, and for a
// name the fleet does not take: no code, and the first two say, in the page's
// own words, the command that fixes it.
func TestFleetDashEnrollRefusals(t *testing.T) {
	l := newFleetLab(t)
	d := newDashFleet(l.c, l.saved(), &l.stgMu)
	var pe dashboard.PlainError
	if _, err := d.Enroll("web1", false, "s"); !errors.As(err, &pe) || !strings.Contains(err.Error(), "tenant fleet init") {
		t.Errorf("no fleet: %v", err)
	}
	l.must("init", "--listen", "127.0.0.1:0", "--address", "10.0.0.1:9777")
	if _, err := d.Enroll("web 1", true, "s"); err == nil || errors.As(err, &pe) {
		t.Errorf("a name that is not one: %v", err)
	}
	l.must("address", "remove", "10.0.0.1:9777")
	if _, err := d.Enroll("web1", false, "s"); !errors.As(err, &pe) || !strings.Contains(err.Error(), "tenant fleet address add") {
		t.Errorf("no address: %v", err)
	}
}

// Add machine on the real dashboard of a real master (TEN-541): the API key
// makes no code; a signed-in browser gets one in the answer, and only there;
// the Security record says a code was made, for which name, by which session,
// and holds no code; the code enrolls a machine, which then waits on the page
// for its six digits, typed on this computer.
func TestFleetDashAddMachine(t *testing.T) {
	t.Setenv("TENANT_UPDATE_REPO", "acme/tenant")
	d := newFleetDash(t, settings{})
	form := url.Values{"name": {"web7"}, "runs": {"user"}}

	// Refusals first: the API key, alone or with the page's Origin.
	for _, set := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+fleetDashKey) },
		func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+fleetDashKey)
			r.Header.Set("Origin", d.base)
		},
	} {
		if a := d.send(http.MethodPost, "/fleet/add", form, set); a.code != http.StatusForbidden || !strings.Contains(a.body, "No code was made") {
			t.Fatalf("the API key: %d %s", a.code, a.body)
		}
	}
	if recs := d.records(catalog.FleetMachineChanged); hasRecord(recs, "code issued") {
		t.Fatalf("a refused request made a code: %q", recs)
	}

	a := d.send(http.MethodPost, "/fleet/add", form, d.browser)
	if a.code != http.StatusOK || a.page != "" {
		t.Fatalf("Add machine: %d to %q (%s)", a.code, a.page, a.errMsg)
	}
	code := regexp.MustCompile(`tntfleet1_[A-Za-z0-9_-]+`).FindString(a.body)
	if code == "" {
		t.Fatalf("no code in the answer:\n%s", a.body)
	}
	lines, _ := enrollPaste(code, false)
	for _, l := range lines {
		if !strings.Contains(a.body, pageText(l.Line)) {
			t.Errorf("the answer lacks %q", l.Line)
		}
	}
	if !strings.Contains(a.body, "a machine at another site cannot dial these") { // the lab's master listens on loopback
		t.Error("the answer has no warning about addresses only this site can dial")
	}
	recs := d.records(catalog.FleetMachineChanged)
	if !hasRecord(recs, "operator@dashboard", "name=web7", "change=code issued", "asked by dashboard session ", "for a child that runs as the user") {
		t.Errorf("no Security record of the code: %q", recs)
	}
	d.ev.mu.Lock()
	for _, e := range d.ev.events {
		text := e.Msg
		for _, f := range e.Data {
			text += " " + f.Text()
		}
		if strings.Contains(text, code) || strings.Contains(text, d.cookie.Value) {
			t.Errorf("a log line holds the code or the session's key: %s", text)
		}
	}
	d.ev.mu.Unlock()
	if strings.Contains(d.page("/fleet"), code) {
		t.Error("the page shows the code again")
	}

	// The code enrolls a machine, which waits for its digits on the page.
	dir := t.TempDir()
	ident, err := fleet.MintIdentity(fleet.ChildIdentityPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	child, six, err := fleet.Enroll(context.Background(), code, ident, fleet.Reported{OS: "linux", Sandbox: "host", Ceiling: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if page := d.page("/fleet"); !strings.Contains(page, "web7") || !strings.Contains(page, "waiting for you") {
		t.Fatalf("the new machine is not waiting on the page:\n%s", page)
	}
	if a := d.post("/fleet/confirm", "machine", child.ID, "code", six); !strings.Contains(a.msg, "is confirmed") {
		t.Fatalf("confirm: %d %q %q", a.code, a.msg, a.errMsg)
	}
}
