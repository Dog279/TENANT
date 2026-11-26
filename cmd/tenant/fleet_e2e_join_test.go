package main

import (
	"regexp"
	"strings"
	"testing"
)

// TEN-553 on real processes: a master, and a machine that joins it with one
// paste. Nothing is typed on the master after `fleet enroll`. Then the six
// digits as the option they are: on, a machine waits; off again takes a
// person at a terminal.
func TestFleetJoinByCodeEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a master and two machines as processes")
	}
	h := thisHost(t)
	m := h.master(false)
	m.serve()
	m.init("")

	if out := m.must(nil, "fleet", "digits"); !strings.Contains(out, "the six digits are off") {
		t.Fatalf("a new fleet's setting:\n%s", out)
	}
	from := m.proc.out.count()
	web1 := m.joinByCode(h, "web1")
	// The operator is told where they look, by the process that holds the fleet.
	m.proc.await(from, "joined with its enrollment code", "serve says a machine joined by its code")
	if out := m.must(nil, "fleet", "list"); !regexp.MustCompile(`web1\s+confirmed`).MatchString(out) {
		t.Errorf("fleet list after the one paste:\n%s", out)
	}
	web1.run()
	if st := web1.state("connected to the master"); strings.Contains(st, "waiting for confirmation") {
		t.Errorf("the machine's status:\n%s", st)
	}

	// The option: on needs nobody, and the next machine waits for its digits.
	if out := m.must(nil, "fleet", "digits", "on"); !strings.Contains(out, "the six digits are on") {
		t.Fatalf("fleet digits on:\n%s", out)
	}
	m.digits = true
	web2 := m.enroll(h, "web2")
	if out := m.must(nil, "fleet", "list"); !regexp.MustCompile(`web2\s+pending`).MatchString(out) {
		t.Errorf("fleet list with the digits on:\n%s", out)
	}
	web2.confirm()
	if out := m.must(nil, "fleet", "list"); !regexp.MustCompile(`web2\s+confirmed`).MatchString(out) {
		t.Errorf("fleet list after the digits:\n%s", out)
	}

	// Off again widens who may join, so it takes a person at a terminal.
	if _, errOut, exit := m.cli(nil, "fleet", "digits", "off"); exit == 0 || !strings.Contains(errOut, "terminal") {
		t.Errorf("digits off with nobody at a terminal: exit %d\n%s", exit, errOut)
	}
	if out := m.must(nil, "fleet", "digits"); !strings.Contains(out, "the six digits are on") {
		t.Errorf("a refused switch changed the setting:\n%s", out)
	}
	off := "off"
	if out := m.must(&off, "fleet", "digits", "off"); !strings.Contains(out, "the six digits are off") {
		t.Errorf("digits off, typed at a terminal:\n%s", out)
	}
}
