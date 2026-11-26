package main

import (
	"context"
	"strings"
	"testing"

	"tenant/internal/fleet"
	"tenant/internal/logs/catalog"
)

// TEN-553: by default a machine's one-time code confirms it. Nothing is
// typed on the master after `fleet enroll`: the machine is confirmed as it
// enrolls, with the scheme any confirmation gives it, it links, the operator
// is told, and the Security record names Tenant and the code, not a person.
func TestFleetCodeAloneConfirmsAMachine(t *testing.T) {
	ev := captureChannelEvents(t)
	l := newFleetLab(t)
	l.settings(settings{
		Permissions: map[string]string{"exec": "allow", "web": "deny"},
		ToolRules:   map[string]string{"os_exec(rm *)": "deny"},
	})
	mgr := l.master()
	l.confirmMode("") // the default

	// What `fleet enroll` tells the operator follows the setting.
	said := func() string {
		return enrollText(enrolled{Name: "x", Code: "tntfleet1_x", Addresses: []string{"127.0.0.1:9777"}, Digits: fleetConfirmDigits(l.cfg)})
	}
	if text := said(); !strings.Contains(text, "the code confirms it") || strings.Contains(text, "six digits and waits") {
		t.Errorf("what `fleet enroll` says with the default:\n%s", text)
	}
	web1 := l.enroll("web1", fleet.Reported{OS: "linux", User: "svc", Version: "9.9.9", Sandbox: "host", Ceiling: "full"})
	if web1.code != "" {
		t.Errorf("the machine was given digits to show: %q", web1.code)
	}
	if m, err := fleet.Open(l.cfg).Machine(web1.id); err != nil || m.State != fleet.StateConfirmed {
		t.Fatalf("after its code: %+v, %v; want it confirmed", m, err)
	}
	// The scheme a confirmation gives: a copy of the master's.
	if s := l.schemeOf(web1.id); s.Permissions["exec"] != "allow" || s.Permissions["web"] != "deny" || s.ToolRules["os_exec(rm *)"] != "deny" {
		t.Errorf("the scheme it started with: %+v", s)
	}
	if !hasRecord(recordsOf(ev, catalog.FleetMachineChanged), actorTenant+"@"+surfaceEnrollCode, "machine="+web1.id, "change=confirmed", "one-time enrollment code") {
		t.Errorf("no Security record of the confirmation by code:\n%s", strings.Join(recordsOf(ev, catalog.FleetMachineChanged), "\n"))
	}
	told := strings.Join(l.told(), "\n")
	for _, want := range []string{`"web1"`, web1.id, "joined with its enrollment code", `system "linux"`, `account "svc"`, "tenant fleet revoke web1"} {
		if !strings.Contains(told, want) {
			t.Errorf("the operator was not told %q:\n%s", want, told)
		}
	}
	// And it is a machine like any other: it links and is online.
	web1.start(t, &labTools{})
	eventually(t, "the machine links without anything typed", func() bool { return online(mgr, web1.id) })

	// The same code a second time: refused, and the machine it enrolled is
	// paused until the operator has looked, since nobody compared digits.
	dir := t.TempDir()
	other, err := fleet.MintIdentity(fleet.ChildIdentityPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	// The master's refusal, not one this side made before it asked.
	if _, _, err := fleet.Enroll(context.Background(), web1.enrollCode, other, fleet.Reported{OS: "linux", Sandbox: "host", Ceiling: "full"}); err == nil || !strings.Contains(err.Error(), "enrollment refused") {
		t.Fatalf("a spent code presented again: %v; want the master to refuse it", err)
	}
	if m, _ := fleet.Open(l.cfg).Machine(web1.id); m.State != fleet.StatePaused {
		t.Errorf("after its code was presented again the machine is %s, want paused", m.State)
	}
	if told := strings.Join(l.told(), "\n"); !strings.Contains(told, "ALERT") || !strings.Contains(told, "It is PAUSED") || !strings.Contains(told, "tenant fleet resume "+web1.id) {
		t.Errorf("the alert does not say the machine is paused and how to bring it back:\n%s", told)
	}
	if !hasRecord(recordsOf(ev, catalog.FleetMachineChanged), actorTenant+"@"+surfaceEnrollCode, "machine="+web1.id, "change=paused") {
		t.Errorf("no Security record of the pause:\n%s", strings.Join(recordsOf(ev, catalog.FleetMachineChanged), "\n"))
	}
	if list, _ := fleet.Open(l.cfg).Machines(); len(list) != 1 {
		t.Errorf("the second machine got a record: %+v", list)
	}
	// The operator says the machine is theirs, by the id the alert gave or by
	// its name; and can pause one themselves.
	state := func() fleet.State { m, _ := fleet.Open(l.cfg).Machine(web1.id); return m.State }
	if out := l.must("resume", web1.id); state() != fleet.StateConfirmed || !strings.Contains(out, "is resumed") {
		t.Errorf("after resume the machine is %s:\n%s", state(), out)
	}
	if out := l.must("pause", "web1"); state() != fleet.StatePaused || !strings.Contains(out, "is paused") {
		t.Errorf("after pause the machine is %s:\n%s", state(), out)
	}
	if _, err := l.run("pause", "nobody"); err == nil {
		t.Error("pausing a machine that is not in the fleet succeeded")
	}
	l.must("resume", "web1")
	if !hasRecord(recordsOf(ev, catalog.FleetMachineChanged), actorOperator+"@"+surfaceCLI, "machine="+web1.id, "change=resumed") {
		t.Errorf("no Security record of the operator's resume:\n%s", strings.Join(recordsOf(ev, catalog.FleetMachineChanged), "\n"))
	}

	// The six digits are an option: with it on, a machine waits as before.
	// A value nobody understands asks for them too.
	for name, mode := range map[string]string{"db1": "digits", "db2": "sometimes"} {
		l.confirmMode(mode)
		ch := l.enroll(name, fleet.Reported{})
		if len(ch.code) != 6 {
			t.Errorf("fleet.confirm %q: the machine shows %q, want six digits", mode, ch.code)
		}
		if m, _ := fleet.Open(l.cfg).Machine(ch.id); m.State != fleet.StatePending {
			t.Errorf("fleet.confirm %q: the machine is %s, want pending", mode, m.State)
		}
		if _, ok := l.saved().FleetSchemes[ch.id]; ok {
			t.Errorf("fleet.confirm %q: a pending machine was given a scheme", mode)
		}
	}
	l.confirmMode("digits")
	if text := said(); !strings.Contains(text, "six digits and waits") || strings.Contains(text, "the code confirms it") {
		t.Errorf("what `fleet enroll` says with fleet.confirm on:\n%s", text)
	}
}
