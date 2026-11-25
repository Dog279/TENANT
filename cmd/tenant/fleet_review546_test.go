package main

// fleet_review546_test.go holds what TEN-546's independent review asked to be
// held: a confirmation lays what its screen showed and its record names the
// layers; Apply's record says which of a machine's own rules it replaced; and
// a rollout starts nothing once its links are another manager's.

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/dashboard"
	"tenant/internal/fleet"
	"tenant/internal/logs/catalog"
)

// A group's rule can be added without any step, since it moves no machine. A
// pending machine is the exception in waiting: at its confirmation the group's
// rules are laid. So the confirmation is held to the screen that was read: a
// rule added between the screen and the digits confirms nothing. And the
// record of a confirmation names the defaults and the groups it laid.
func TestFleetConfirmNamesItsLayersAndHoldsToItsScreen(t *testing.T) {
	g := newGroupLab(t)
	g.must("permissions", "--defaults", "set", "mcp", "deny")
	g.must("group", "create", "ops")
	g.must("group", "rules", "ops", "add", "x_post", "deny")
	g.must("group", "add", "ops", "delta")

	// The operator reads the confirm screen; before they type the digits, a
	// rule that lets a tool run is added to the group (as the dashboard's
	// group page can, with no second step).
	lab := fleetAsk
	fleetAsk = func(string) (string, error) {
		f, err := fileFleetSchemes(g.cfg, g.c.agent)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.SetGroupRule("ops", "web_fetch", "allow"); err != nil {
			t.Fatal(err)
		}
		return g.delta.code, nil
	}
	out, err := g.run("confirm", "delta")
	fleetAsk = lab
	if err == nil || !strings.Contains(err.Error(), "changed since the screen you read") {
		t.Fatalf("a confirmation after its group changed: %v\n%s", err, out)
	}
	if strings.Contains(out, "web_fetch") {
		t.Fatalf("the lab is wrong: the screen already showed the rule added after it:\n%s", out)
	}
	if m, _ := fleet.Open(g.cfg).Machine(g.delta.id); m.State != fleet.StatePending {
		t.Fatalf("the machine is %s after a refused confirmation", m.State)
	}
	if _, has := g.saved().FleetSchemes[g.delta.id]; has {
		t.Fatal("a refused confirmation left the machine a scheme")
	}
	if !hasRecord(recordsOf(g.ev, catalog.FleetConfirmRefused), "name=delta", "changed since the screen you read") {
		t.Error("no Security record of the refused confirmation")
	}

	// Read again (the screen now shows the rule), then the digits.
	g.answer = ""
	if screen, _ := g.run("confirm", "delta"); !strings.Contains(screen, "web_fetch") || !strings.Contains(screen, "group ops") {
		t.Errorf("the confirm screen read again:\n%s", screen)
	}
	g.confirm("delta", g.delta)
	if s := g.schemeOf(g.delta.id); s.ToolRules["web_fetch"] != "allow" || s.ToolRules["x_post"] != "deny" || s.Permissions["mcp"] != "deny" {
		t.Errorf("delta at confirm: %+v", s)
	}
	if got := recordsOf(g.ev, catalog.FleetMachineChanged); !hasRecord(got, "name=delta", "change=confirmed", `"fleet_defaults":{"mcp":"deny"}`, `"groups_laid":["ops"]`) {
		t.Errorf("the record of the confirmation does not name the layers it laid:\n%s", strings.Join(got, "\n"))
	}
}

// Apply replaces every rule of a machine's own that a laid rule answers for,
// a block included. Its record, one per machine, says which.
func TestFleetApplyRecordsTheRulesItReplaced(t *testing.T) {
	g := newGroupLab(t)
	g.must("permissions", "alpha", "add", "os_exec(git status --short)", "deny")
	g.must("permissions", "alpha", "add", "x_post", "deny")
	g.must("group", "create", "ops")
	g.must("group", "add", "ops", "alpha", "beta")
	g.must("group", "rules", "ops", "add", "os_exec(git *)", "allow")
	out := g.must("group", "apply", "ops")
	if s := g.schemeOf(g.alpha.id); s.ToolRules["os_exec(git status --short)"] != "" || s.ToolRules["os_exec(git *)"] != "allow" || s.ToolRules["x_post"] != "deny" {
		t.Fatalf("alpha after apply: %v", s.ToolRules)
	}
	const replaced = "of its own rules, replaced: os_exec(git status --short) (was deny)"
	if !strings.Contains(out, replaced) {
		t.Errorf("the answer does not say which rule was replaced:\n%s", out)
	}
	got := recordsOf(g.ev, catalog.FleetMachineChanged)
	if !hasRecord(got, "name=alpha", "change=scheme changed", "group(s) ops", replaced) {
		t.Errorf("alpha's record does not say which rule was replaced:\n%s", strings.Join(got, "\n"))
	}
	for _, r := range got {
		if strings.Contains(r, "change=scheme changed") && (strings.Contains(r, "x_post (was") || strings.Contains(r, "name=beta") && strings.Contains(r, "replaced:")) {
			t.Errorf("a record names a rule that was not replaced: %s", r)
		}
	}
	// What the page reads of it: the layers' name moves with a group's rule,
	// and a pending machine's start carries the name of what it would lay.
	ctl := newDashFleet(g.c, g.stg, &g.stgMu)
	before := ctl.View()
	g.must("group", "add", "ops", "delta")
	g.must("group", "rules", "ops", "add", "web_fetch", "deny")
	g.sync()
	after := ctl.View()
	if before.LayersKey == "" || after.LayersKey == "" || before.LayersKey == after.LayersKey {
		t.Errorf("the layers' name did not move with a group's rule: %q then %q", before.LayersKey, after.LayersKey)
	}
	start := func(v dashboard.FleetView) string {
		for _, m := range v.Machines {
			if m.ID == g.delta.id && m.Start != nil {
				return m.Start.Key
			}
		}
		return ""
	}
	if a, b := start(before), start(after); a == "" || b == "" || a == b {
		t.Errorf("the pending machine's start did not move with its group: %q then %q", a, b)
	}
}

// A rollout whose manager no longer holds the fleet's links (the listener
// came down, another manager took over) starts no more machines: the ones in
// flight finish, the rest are reported as not started, and why.
func TestRolloutStopsWhenTheLinksAreNoLongerHeld(t *testing.T) {
	r, f := rolloutRig(t, 1, map[string]fakeRun{"a": {end: fleetUpdDone, hold: true}, "b": {end: fleetUpdDone}, "c": {end: fleetUpdDone}})
	var gone atomic.Bool
	r.holds = func(*fleet.Manager) bool { return !gone.Load() }
	if err := r.Start(nil, rolloutMachines("a", "b", "c"), fleetUpdateOpts{}, 1); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if s, _ := r.State(); statuses(s) == "a=running b=waiting c=waiting" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first machine never started")
		}
	}
	gone.Store(true)
	f.release("a")
	s := waitRollout(t, r)
	if statuses(s) != "a=done b=not_started c=not_started" || !strings.Contains(s.Stopped, "no longer held the fleet's links") {
		t.Errorf("after the links went: %s, stopped %q", statuses(s), s.Stopped)
	}
	if got := f.started(); len(got) != 1 || got[0] != "a" {
		t.Errorf("machines started: %v, want a alone", got)
	}
	if why := s.Machines[1].Reason; !strings.Contains(why, "not started: the rollout was stopped when this Tenant no longer held the fleet's links") {
		t.Errorf("why b was not started: %q", why)
	}
}
