package main

// The fleet log: a master's catalogued record of its fleet (internal/fleet's
// events.go). The audit records of the same changes are the Security log's.

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"tenant/internal/fleet"
	"tenant/internal/logs/event"
)

// fleetLog is every fleet event written so far, each as its id, opcode and
// fields: where a secret must never be.
func fleetLog(c *channelEvents) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, e := range c.events {
		if e.Provider != "fleet" {
			continue
		}
		b.WriteString(e.Msg + " " + e.Opcode)
		for _, f := range e.Data {
			b.WriteString(" " + f.Key + "=" + f.Text())
		}
		b.WriteString("\n")
	}
	return b.String()
}

// The fleet log covers a fleet from the day it is made: this Tenant becoming
// a master, its port opening, the machines' memory, a code made, a machine
// enrolling, a confirmation refused and then given, the machine's link, its
// revocation, the link ending, and the port closing. A Tenant with no fleet
// writes nothing there. No line holds an enrollment code.
func TestFleetLogCoversAMachinesLife(t *testing.T) {
	ev := captureChannelEvents(t)
	l := newFleetLab(t)
	l.stop = startFleetHub(context.Background(), l.c, "serve", newApprovalBroker(slog.Default()), l.tell, slog.Default())
	if said := fleetLog(ev); said != "" {
		t.Fatalf("a Tenant with no fleet wrote to the fleet log:\n%s", said)
	}
	field := func(e *event.Event, key string) string { return eventField(e, key).Text() }
	// one is the event of d (and op) whose field key holds want, awaited.
	one := func(d *event.Def, op, key, want string) *event.Event {
		t.Helper()
		var found *event.Event
		eventually(t, "the fleet log has "+d.Spec().Template+" with "+key+" "+want, func() bool {
			for _, e := range ev.matching(d, op) {
				if key == "" || strings.Contains(field(e, key), want) {
					found = e
					return true
				}
			}
			return false
		})
		return found
	}

	l.must("init", "--listen", "127.0.0.1:0")
	if e := one(fleet.Initialised, "", "", ""); e.Surface != surfaceCLI || field(e, "certificate") == "" || field(e, "listen") != "127.0.0.1:0" {
		t.Fatalf("the fleet being made: %+v", e.Data)
	}
	if e := one(fleet.Listening, event.OpStart, "role", "serve"); !strings.HasPrefix(field(e, "addr"), "127.0.0.1:") {
		t.Fatalf("the port opening: %+v", e.Data)
	}
	one(fleet.MemoryReady, "", "", "")
	mgr := fleetManager()
	l.must("init", "--address", mgr.Addr().String())
	one(fleet.AddressesChanged, "", "addresses", mgr.Addr().String())
	if n := len(ev.matching(fleet.Initialised, "")); n != 1 {
		t.Fatalf("a second init wrote that the fleet was made %d times", n)
	}

	ch := l.enroll("web1", fleet.Reported{})
	one(fleet.MachineChanged, "", "change", "code issued")
	if e := one(fleet.Enrolled, "", "name", "web1"); field(e, "machine") != ch.id || !strings.HasPrefix(field(e, "remote"), "127.0.0.1:") {
		t.Fatalf("the machine enrolling: %+v", e.Data)
	}
	// The code again, from elsewhere: refused, and an error in the fleet log.
	other, err := fleet.MintIdentity(fleet.ChildIdentityPath(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fleet.Enroll(context.Background(), ch.enrollCode, other, fleet.Reported{OS: "linux", User: "root", Version: "1.2.3", Sandbox: "host", Ceiling: "full"}); err == nil {
		t.Fatal("a spent code enrolled a second machine")
	}
	one(fleet.CodeReplayed, "", "name", "web1")

	l.answer = "000000"
	if _, err := l.run("confirm", "web1"); err == nil {
		t.Fatal("six wrong digits confirmed the machine")
	}
	one(fleet.ConfirmRefused, "", "name", "web1")
	l.confirm("web1", ch)
	one(fleet.MachineChanged, "", "change", "confirmed")

	ch.start(t, &labTools{})
	if e := one(fleet.Connected, "", "name", "web1"); field(e, "machine") != ch.id {
		t.Fatalf("the machine's link: %+v", e.Data)
	}
	l.must("revoke", "web1", "--keep-scheme")
	one(fleet.MachineChanged, "", "change", "revoked")
	if e := one(fleet.Disconnected, "", "machine", ch.id); field(e, "why") == "" {
		t.Fatalf("the link ending: %+v", e.Data)
	}

	l.stop()
	l.stop = nil
	if e := one(fleet.Listening, event.OpStop, "role", "serve"); field(e, "duration") == "" {
		t.Fatalf("the port closing: %+v", e.Data)
	}
	if said := fleetLog(ev); strings.Contains(said, ch.enrollCode) || strings.Contains(said, strings.TrimPrefix(ch.enrollCode, "tntfleet2_")) {
		t.Fatalf("the fleet log holds an enrollment code:\n%s", said)
	}
}

// A fleet that cannot open its port says why in the fleet log, once per
// cause, and one held by another process says who holds it.
func TestFleetLogWhenThePortDoesNotOpen(t *testing.T) {
	ev := captureChannelEvents(t)
	l := newFleetLab(t)
	l.must("init", "--listen", "127.0.0.1:0")
	prev := fleetListenGuard
	fleetListenGuard = func(string) error { return context.DeadlineExceeded }
	t.Cleanup(func() { fleetListenGuard = prev })
	l.stop = startFleetHub(context.Background(), l.c, "tui", newApprovalBroker(slog.Default()), l.tell, slog.Default())
	eventually(t, "the hub has looked more than once", func() bool { return len(l.told()) > 0 })
	eventually(t, "the failure is in the fleet log", func() bool { return len(ev.matching(fleet.ListenerNotStarted, "")) > 0 })
	if n := len(ev.matching(fleet.ListenerNotStarted, "")); n != 1 {
		t.Fatalf("one cause was logged %d times", n)
	}
	if e := ev.matching(fleet.ListenerNotStarted, "")[0]; eventField(e, "addr").Text() != "127.0.0.1:0" || eventField(e, "err").Text() == "" {
		t.Fatalf("the failure: %+v", e.Data)
	}
	if len(ev.matching(fleet.Listening, event.OpStart)) != 0 {
		t.Fatal("a port that did not open was logged as open")
	}
}
