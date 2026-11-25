package main

// fleet_remoteconfirm_test.go is TEN-545 on the real files: a pending machine
// confirmed from a signed-in dashboard browser that is not on the computer
// running Tenant. It drives fleet_dashctl_test.go's lab (a real fleet, a real
// child, the dashboard on a loopback port) by the name a tailnet or a proxy
// gives the dashboard, which is how such a browser reaches it.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"tenant/internal/dashboard"
	"tenant/internal/fleet"
	"tenant/internal/logs/catalog"
)

// Remote confirmation is off until the operator turns it on at the master's
// terminal. Off, the sign-in key opens nothing. On, a signed-in browser
// elsewhere confirms a machine with the key again and the machine's digits;
// the API key still confirms nothing; and the records say it was asked from
// elsewhere, by which session. Turned off again, the door is shut again, for
// the page and for the control behind it.
func TestFleetDashConfirmFromElsewhere(t *testing.T) {
	d := newFleetDash(t, settings{Permissions: map[string]string{"exec": "allow"}})
	ch := d.l.enroll("web1", fleet.Reported{})
	tools := &labTools{}
	client, _ := ch.start(t, tools)
	eventually(t, "the pending machine is told to wait", func() bool { return client.Status().State == fleet.LinkWaiting })

	elsewhere := func(r *http.Request) { // what `tailscale serve` sends on for a signed-in browser
		r.AddCookie(d.cookie)
		r.Host = "dash.tailnet.example"
		r.Header.Set("Origin", "http://dash.tailnet.example")
		r.Header.Set("X-Forwarded-For", "100.64.0.7")
		r.Header.Set("Tailscale-User-Login", "op@example.com")
	}
	apiKey := func(r *http.Request) {
		r.Host = "dash.tailnet.example"
		r.Header.Set("Origin", "http://dash.tailnet.example")
		r.Header.Set("Authorization", "Bearer "+fleetDashKey)
	}
	form := func(code, key string) url.Values {
		v := url.Values{"machine": {ch.id}, "code": {code}}
		if key != "" {
			v.Set("key", key)
		}
		return v
	}
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
	}
	const notHere = "a machine is confirmed in a browser on the computer running Tenant"

	// Off, as it is until someone says otherwise: the key and the right digits
	// open nothing, the page offers no form, and the command says off.
	if said := d.l.must("remote-confirm"); !strings.Contains(said, "remote confirmation is off") {
		t.Errorf("the switch before anyone set it: %q", said)
	}
	if a := d.send(http.MethodPost, "/fleet/confirm", form(ch.code, fleetDashKey), elsewhere); a.code != http.StatusForbidden || !strings.Contains(a.body, notHere) {
		t.Errorf("turned off, with the key and the digits: %d %q", a.code, a.body)
	}
	pending("turned off")
	if page := d.send(http.MethodGet, "/fleet/"+ch.id, nil, elsewhere).body; strings.Contains(page, `action="/fleet/confirm"`) || !strings.Contains(page, "tenant fleet remote-confirm on") {
		t.Errorf("turned off, the page from elsewhere offers the form, or does not say how it is turned on:\n%s", page)
	}
	// The control itself refuses a confirmation said to come from elsewhere.
	ctl := newDashFleet(d.l.c, d.l.stg, &d.l.stgMu)
	var plain dashboard.PlainError
	if err := ctl.Confirm(ch.id, ch.code, nil, "dashboard session 00000000 from 203.0.113.9:1"); !errors.As(err, &plain) || !strings.Contains(err.Error(), "turned off") {
		t.Errorf("turned off, the control asked directly: %v", err)
	}
	pending("turned off, the control asked directly")

	// Turning it on takes a person at a terminal. A command with no terminal
	// behind it (a script, the agent) is refused and writes nothing; so is an
	// answer that is not "on".
	asked := fleetAsk // the lab's: it answers with l.answer
	fleetAsk = func(string) (string, error) {
		return "", errors.New("this needs a person at a terminal: standard input is not one")
	}
	if out, err := d.l.run("remote-confirm", "on"); err == nil || !strings.Contains(err.Error(), "needs a person at a terminal") {
		t.Errorf("turning it on with no terminal: %q, %v", out, err)
	}
	fleetAsk = asked
	for _, answer := range []string{"", "yes", "off"} {
		d.l.answer = answer
		if out, err := d.l.run("remote-confirm", "on"); err == nil || err.Error() != "not turned on" {
			t.Errorf("turning it on, answered %q: %q, %v", answer, out, err)
		}
	}
	if fleetRemoteConfirmOn(d.l.cfg) || hasRecord(d.records(catalog.FleetMachineChanged), "remote confirmation turned on") {
		t.Fatal("a refused switch turned remote confirmation on, or recorded that it did")
	}
	// The operator turns it on at the master's terminal: config.json holds it,
	// the Security log and the fleet log say so, and the running dashboard
	// reads it without a restart.
	d.l.answer, d.l.asked = "on", nil
	if said := d.l.must("remote-confirm", "on"); !strings.Contains(said, "remote confirmation is on") || len(d.l.asked) != 1 || !strings.Contains(d.l.asked[0], "Type on to turn it on") {
		t.Errorf("turning it on: %q, asked %q", said, d.l.asked)
	}
	if lc, err := loadLaunchConfig(d.l.cfg); err != nil || !lc.Fleet.RemoteConfirm {
		t.Fatalf("config.json after turning it on: %+v (%v)", lc.Fleet, err)
	}
	if got := d.records(catalog.FleetMachineChanged); !hasRecord(got, "operator@cli", "change=remote confirmation turned on") {
		t.Errorf("no Security record of the switch:\n%s", strings.Join(got, "\n"))
	}
	if page := d.send(http.MethodGet, "/fleet/"+ch.id, nil, elsewhere).body; !strings.Contains(page, `action="/fleet/confirm"`) || !strings.Contains(page, `name="key"`) {
		t.Fatalf("turned on, the page from elsewhere has no form with a key field:\n%s", page)
	}
	if page := d.page("/fleet/" + ch.id); strings.Contains(page, `name="key"`) {
		t.Errorf("the page on this computer asks for the key")
	}

	// On: still not a request that carries only the API key, and never a
	// session without the key. (What that does not hold against, the key's
	// holder signing in from a script, is internal/dashboard's
	// TestFleetConfirmFromElsewhereTheKeysHolder.)
	if a := d.send(http.MethodPost, "/fleet/confirm", form(ch.code, fleetDashKey), apiKey); a.code != http.StatusForbidden || !strings.Contains(a.body, notHere) {
		t.Errorf("turned on, the API key: %d %q", a.code, a.body)
	}
	pending("turned on, the API key")
	for why, key := range map[string]string{"no key": "", "a wrong key": fleetDashKey + "x"} {
		a := d.send(http.MethodPost, "/fleet/confirm", form(ch.code, key), elsewhere)
		if a.code != http.StatusSeeOther || a.page != "/fleet/"+ch.id || !strings.Contains(a.errMsg, "that isn't the dashboard's sign-in key") {
			t.Errorf("turned on, %s: %d to %s, err %q", why, a.code, a.page, a.errMsg)
		}
		pending("turned on, " + why)
	}
	if got := d.records(catalog.FleetConfirmRefused); !hasRecord(got, "dashboard session ", "@"+surfaceDashboardRemote, "machine="+ch.id, "reason=the sign-in key given again was wrong") {
		t.Errorf("no Security record of the wrong key, from elsewhere:\n%s", strings.Join(got, "\n"))
	}
	// The key, and digits that are not the machine's: the control's refusal,
	// recorded as asked from elsewhere and by whom.
	wrong := "000000"
	if wrong == ch.code {
		wrong = "111111"
	}
	if a := d.send(http.MethodPost, "/fleet/confirm", form(wrong, fleetDashKey), elsewhere); !strings.Contains(a.errMsg, "aren't the six digits that machine shows") {
		t.Errorf("turned on, the key and wrong digits: %d, err %q", a.code, a.errMsg)
	}
	pending("turned on, the key and wrong digits")
	if got := d.records(catalog.FleetConfirmRefused); !hasRecord(got, "operator@"+surfaceDashboardRemote, "machine="+ch.id, "that is not the code the machine shows", "asked by dashboard session ") {
		t.Errorf("no Security record of the wrong digits, from elsewhere:\n%s", strings.Join(got, "\n"))
	}

	// The key and the machine's own digits: confirmed, clamped as at this
	// computer, and the link comes up.
	a := d.send(http.MethodPost, "/fleet/confirm", form(ch.code, fleetDashKey), elsewhere)
	if a.code != http.StatusSeeOther || a.page != "/fleet/"+ch.id || a.errMsg != "" || !strings.HasPrefix(a.msg, "web1 is confirmed.") {
		t.Fatalf("turned on, the key and the machine's digits: %d to %s, msg %q, err %q", a.code, a.page, a.msg, a.errMsg)
	}
	if strings.Contains(a.page+a.msg+a.errMsg, fleetDashKey) {
		t.Errorf("the key is in the answer")
	}
	if m, _ := fleet.Open(d.l.cfg).Machine(ch.id); m.State != fleet.StateConfirmed {
		t.Fatalf("the machine is %s", m.State)
	}
	if scheme := d.l.schemeOf(ch.id); scheme.Permissions["exec"] != "ask" {
		t.Errorf("the scheme it started with: %v; want exec no looser than ask whatever the master allows", scheme.Permissions)
	}
	eventually(t, "the confirmed machine links", func() bool { return client.Status().State == fleet.LinkConnected && online(d.mgr, ch.id) })
	// Every request the proxy passes on comes from this computer, so the record
	// also quotes what the proxy said of who it was, as not checked.
	if got := d.records(catalog.FleetMachineChanged); !hasRecord(got, "operator@"+surfaceDashboardRemote, "name=web1", "machine="+ch.id, "change=confirmed", `"asked_by":"dashboard session `,
		`from 127.0.0.1:`, `its proxy says \"op@example.com\" (not checked)`) || hasRecord(got, "operator@dashboard ", "change=confirmed") {
		t.Errorf("the Security record of the confirmation does not say it was asked from elsewhere, and by whom:\n%s", strings.Join(got, "\n"))
	}

	// Turned off again: shut for the next machine, whatever its page showed.
	// Turning it off asks nobody.
	other := d.l.enroll("web2", fleet.Reported{})
	d.l.asked = nil
	if said := d.l.must("remote-confirm", "off"); !strings.Contains(said, "remote confirmation is off") || len(d.l.asked) != 0 {
		t.Errorf("turning it off: %q, asked %q", said, d.l.asked)
	}
	if a := d.send(http.MethodPost, "/fleet/confirm", url.Values{"machine": {other.id}, "code": {other.code}, "key": {fleetDashKey}}, elsewhere); a.code != http.StatusForbidden || !strings.Contains(a.body, notHere) {
		t.Errorf("turned off again, with the key and the digits: %d %q", a.code, a.body)
	}
	if m, _ := fleet.Open(d.l.cfg).Machine(other.id); m.State != fleet.StatePending {
		t.Fatalf("turned off again, the next machine is %s", m.State)
	}
	// A value that is not one changes nothing.
	if _, err := d.l.run("remote-confirm", "maybe"); err == nil {
		t.Errorf("remote-confirm maybe was taken")
	}
	if _, err := d.l.run("remote-confirm", "on", "now"); err == nil {
		t.Errorf("remote-confirm on now was taken")
	}
	if lc, err := loadLaunchConfig(d.l.cfg); err != nil || lc.Fleet.RemoteConfirm {
		t.Errorf("config.json after the refused values: %+v (%v)", lc.Fleet, err)
	}
}

// A config.json that cannot be read leaves remote confirmation off.
func TestFleetRemoteConfirmUnreadableConfigIsOff(t *testing.T) {
	l := newFleetLab(t)
	l.answer = "on"
	l.must("remote-confirm", "on")
	if !fleetRemoteConfirmOn(l.cfg) {
		t.Fatal("turned on, and read as off")
	}
	if err := os.WriteFile(launchConfigPath(l.cfg), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fleetRemoteConfirmOn(l.cfg) {
		t.Error("a config.json that cannot be read was taken as on")
	}
}
