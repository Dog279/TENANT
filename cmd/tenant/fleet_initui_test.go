package main

// A fleet set up from a running Tenant: /fleet init in the terminal app and
// Set up the fleet on the dashboard's Fleet page. Both run the command line's
// own init (initFleet); the recovery key is made at a terminal and nowhere
// else, which for the terminal app is the terminal it hands over.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"tenant/internal/fleet"
	"tenant/internal/logs/catalog"
)

// noFleetYet fails the test if the lab was made a master.
func noFleetYet(t *testing.T, l *fleetLab, after string) {
	t.Helper()
	if _, err := os.Lstat(fleet.MasterIdentityPath(l.cfg)); err == nil {
		t.Fatalf("%s left a fleet behind", after)
	}
}

func TestFleetInitFromTheTerminalApp(t *testing.T) {
	l := newFleetLab(t)
	fleetInitWait = 5 * time.Second // what init says waits for this process's hub
	op := operator(nil)
	withOperator(t, op.open)
	l.stop = startFleetHub(context.Background(), l.c, "tui", newApprovalBroker(slog.Default()), l.tell, slog.Default())
	ctl := newFleetTUIControl(l.c, l.saved(), &l.stgMu)
	if s := ctl.Status(); !strings.Contains(s, "no fleet yet: /fleet init") {
		t.Fatalf("/fleet with no fleet said: %s", s)
	}

	// What init refuses makes nothing.
	if _, err := ctl.Init("nine", nil); err == nil {
		t.Fatal("a listen address that is not one was taken")
	}
	if _, err := ctl.Init("127.0.0.1:0", []string{"not an address"}); err == nil {
		t.Fatal("an address that is not one was taken")
	}
	noFleetYet(t, l, "a refused init")

	in, err := ctl.Init("127.0.0.1:0", []string{"10.0.0.1:9777"})
	if err != nil || in.Recovery == nil {
		t.Fatalf("init: %v (a recovery step: %v)", err, in.Recovery != nil)
	}
	// The fleet is made before the terminal is asked for; nothing is shown yet.
	if _, err := fleetIdentity(l.cfg); err != nil || op.opened != 0 {
		t.Fatalf("before the recovery step: identity %v, terminal opened %d times", err, op.opened)
	}
	if err := in.Recovery(); err != nil {
		t.Fatalf("the recovery step: %v", err)
	}
	said := in.Done(nil)
	for _, want := range []string{"this Tenant is now a fleet master", "machines dial  10.0.0.1:9777", "recovery key   proven", "this process holds the fleet, on 127.0.0.1:"} {
		if !strings.Contains(said, want) {
			t.Errorf("/fleet init does not say %q:\n%s", want, said)
		}
	}
	// The identity went to the terminal once and to nothing the app shows or keeps.
	shown := identityOnScreen.FindAllString(op.shown(), -1)
	if len(shown) != 1 || strings.Contains(said, "AGE-SECRET-KEY") {
		t.Fatalf("the identity was shown %d times on the terminal; the app said:\n%s", len(shown), said)
	}
	assertNowhere(t, shown[0], l.cfg, l.data)
	if rec, proven, err := fleet.Open(l.cfg).Recovery(); err != nil || rec == "" || proven.IsZero() {
		t.Fatalf("the recovery key on record: %q %v %v", rec, proven, err)
	}
	if fleetManager() == nil {
		t.Fatal("the running app did not take the fleet it made")
	}

	// Again: nothing is replaced and the terminal is not asked for.
	id1, _ := fleetIdentity(l.cfg)
	again, err := ctl.Init("", nil)
	if err != nil || again.Recovery != nil || !strings.Contains(again.Done(nil), "already; nothing was replaced") || op.opened != 1 {
		t.Fatalf("a second init: %v, a recovery step: %v, terminal opened %d times", err, again.Recovery != nil, op.opened)
	}
	if id2, _ := fleetIdentity(l.cfg); id2.Fingerprint != id1.Fingerprint {
		t.Fatal("a second init replaced the master's identity")
	}
}

// The operator cancels at the terminal (Enter alone): the fleet is made, no
// recovery key is recorded, the app says so in its own words, and the step is
// offered again the next time.
func TestFleetInitFromTheTerminalAppCancelled(t *testing.T) {
	l := newFleetLab(t)
	notes(t)
	op := operator([]string{""})
	withOperator(t, op.open)
	ctl := newFleetTUIControl(l.c, l.saved(), &l.stgMu)
	in, err := ctl.Init("127.0.0.1:0", nil)
	if err != nil || in.Recovery == nil {
		t.Fatalf("init: %v", err)
	}
	recErr := in.Recovery()
	if !errors.Is(recErr, errRecoveryNotSetUp) {
		t.Fatalf("a cancelled recovery step: %v", recErr)
	}
	said := in.Done(recErr)
	if !strings.Contains(said, "recovery key   not made") || !strings.Contains(said, "/fleet init") || strings.Contains(said, "needs a person at a terminal") {
		t.Fatalf("after a cancel /fleet init said:\n%s", said)
	}
	if rec, _, _ := fleet.Open(l.cfg).Recovery(); rec != "" {
		t.Fatalf("a cancelled step recorded %q", rec)
	}
	if again, err := ctl.Init("", nil); err != nil || again.Recovery == nil {
		t.Fatalf("the step is not offered again: %v", err)
	}
}

// Set up the fleet on the real dashboard of a Tenant with no fleet: the API
// key makes none and a refused address makes none; a signed-in browser's form
// makes the Tenant a master, its running hub takes the fleet, the Security
// record says which session asked, and a code can be made at once. The page
// never opens the recovery terminal: it says the key is missing until init at
// a terminal makes it.
func TestFleetInitFromTheDashboard(t *testing.T) {
	op := operator(nil)
	withOperator(t, op.open)
	d := &fleetDash{t: t, ev: captureChannelEvents(t), l: newFleetLab(t)}
	l := d.l
	l.settings(settings{})
	l.stg, l.broker = l.saved(), newApprovalBroker(slog.Default())
	loadFleetSettings(l.stg, &l.stgMu, l.cfg, "main", l.tell)
	l.stop = startFleetHub(context.Background(), l.c, "serve", l.broker, l.tell, slog.Default())
	d.up()

	if body := d.page("/fleet"); !strings.Contains(body, `action="/fleet/init"`) || !strings.Contains(body, `value="`+defaultFleetListen+`"`) {
		t.Fatalf("the page of a Tenant with no fleet:\n%s", body)
	}
	good := url.Values{"listen": {"127.0.0.1:0"}, "addresses": {"10.0.0.1:9777, master.example:9777"}}
	if a := d.send(http.MethodPost, "/fleet/init", good, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+fleetDashKey)
		r.Header.Set("Origin", d.base)
	}); a.code != http.StatusForbidden || !strings.Contains(a.body, "Nothing changed") {
		t.Fatalf("the API key: %d %s", a.code, a.body)
	}
	for name, form := range map[string]url.Values{
		"a listen address that is not one": {"listen": {"nine"}},
		"an address that is not one":       {"listen": {"127.0.0.1:0"}, "addresses": {"10.0.0.1"}},
	} {
		if a := d.send(http.MethodPost, "/fleet/init", form, d.browser); a.code != http.StatusSeeOther || a.msg != "" || !strings.Contains(a.errMsg, "Nothing changed") {
			t.Fatalf("%s: %d, msg %q, err %q", name, a.code, a.msg, a.errMsg)
		}
	}
	noFleetYet(t, l, "a refused request")

	if a := d.send(http.MethodPost, "/fleet/init", good, d.browser); a.code != http.StatusSeeOther || a.errMsg != "" || !strings.Contains(a.msg, "This Tenant is a fleet master") {
		t.Fatalf("set up: %d, msg %q, err %q", a.code, a.msg, a.errMsg)
	}
	eventually(t, "the running Tenant takes the fleet it made", func() bool { return fleetManager() != nil })
	if addrs, err := fleet.Open(l.cfg).Addresses(); err != nil || strings.Join(addrs, " ") != "10.0.0.1:9777 master.example:9777" {
		t.Fatalf("the addresses on record: %q %v", addrs, err)
	}
	if recs := d.records(catalog.FleetMachineChanged); !hasRecord(recs, "initialised", "asked by dashboard session ") || hasRecord(recs, d.cookie.Value) {
		t.Fatalf("the Security record: %q", recs)
	}
	if op.opened != 0 {
		t.Fatalf("the dashboard opened the recovery terminal %d times", op.opened)
	}
	body := d.page("/fleet")
	if !strings.Contains(body, "This fleet has no recovery key yet") || !strings.Contains(body, `action="/fleet/add"`) || strings.Contains(body, `action="/fleet/init"`) {
		t.Fatalf("the page of a new master:\n%s", body)
	}
	if a := d.post("/fleet/add", "name", "web1", "runs", "user"); a.code != http.StatusOK || regexp.MustCompile(`tntfleet2_[A-Za-z0-9_-]+`).FindString(a.body) == "" {
		t.Fatalf("a code on the new fleet: %d\n%s", a.code, a.body)
	}

	// init at a terminal makes the recovery key, and the page stops asking.
	l.must("init")
	if body := d.page("/fleet"); strings.Contains(body, "no recovery key") || op.opened != 1 {
		t.Fatalf("after init at a terminal (opened %d times):\n%s", op.opened, body)
	}
}
