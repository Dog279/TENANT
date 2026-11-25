package main

// fleet_dashrollout_test.go: the dashboard's rollout (TEN-546, 2.6) through
// dashFleet, over this process's real fleetRollout with a fake of its
// per-machine seam (fakeUpdates, fleetrollout_test.go): no machine is really
// updated and no release is fetched. The machines are a fleetLab's, enrolled
// and confirmed; the page is the real dashboard over real HTTP.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"tenant/internal/dashboard"
	"tenant/internal/fleet"
)

// rolloutLab is a master with two confirmed machines whose rollout runs
// through plan instead of real updates.
func rolloutLab(t *testing.T, plan func(a, b string) map[string]fakeRun) (*fleetDash, labChild, labChild, *fakeUpdates) {
	t.Helper()
	d := newFleetDash(t, settings{})
	a := d.l.enroll("web1", fleet.Reported{})
	d.l.confirm("web1", a)
	b := d.l.enroll("web2", fleet.Reported{})
	d.l.confirm("web2", b)
	d.l.sync()
	prev := fleetUpdatePoll
	fleetUpdatePoll = time.Millisecond
	t.Cleanup(func() { fleetUpdatePoll = prev })
	ro := fleetRollouter()
	if ro == nil {
		t.Fatal("the master holds no rollout")
	}
	fu := newFakeUpdates(t, fleetRolloutLimit, plan(a.id, b.id))
	ro.updates = fu // before any rollout starts: nothing reads it yet
	return d, a, b, fu
}

// Before anything starts, StartRollout refuses what the rollout itself does
// not check: a version or a processor that is not one, a limit outside 1 to
// 8; and the page says so in plain words.
func TestFleetDashRolloutRefusals(t *testing.T) {
	d, a, _, fu := rolloutLab(t, func(a, b string) map[string]fakeRun { return map[string]fakeRun{} })
	ctl := newDashFleet(d.l.c, d.l.stg, &d.l.stgMu)
	var plain dashboard.PlainError
	for name, err := range map[string]error{
		"a version that is not one": ctl.StartRollout([]string{a.id}, "2.x; rm -rf", "", false, 1),
		"a processor not one":       ctl.StartRollout([]string{a.id}, "v2.0.0", "x86", false, 1),
		"no machine at once":        ctl.StartRollout([]string{a.id}, "", "", false, 0),
		"nine at once":              ctl.StartRollout([]string{a.id}, "", "", false, 9),
		"no machine":                ctl.StartRollout(nil, "", "", false, 1),
	} {
		if !errors.As(err, &plain) || !strings.HasPrefix(err.Error(), "Nothing was sent: ") {
			t.Errorf("%s: %v, want a plain refusal", name, err)
		}
	}
	if got := fu.started(); len(got) != 0 {
		t.Fatalf("a refused rollout started %q", got)
	}
	if _, ok := fleetRollouter().State(); ok {
		t.Fatal("a refused rollout left a state")
	}
	// Through the page: the version's refusal is the flash, nothing started.
	if ans := d.post("/fleet/bulk", "action", "update", "version", "v2", "machine", a.id); !strings.Contains(ans.errMsg, "is not a version") || len(fu.started()) != 0 {
		t.Errorf("a bad version from the page: msg %q err %q, started %q", ans.msg, ans.errMsg, fu.started())
	}
	if ctl.StopRollout() {
		t.Error("Stop with no rollout says one was running")
	}
}

// From the page's Update after its step, a rollout runs over the machines in
// the list's order; a machine the fleet no longer has goes in by its id and is
// skipped; the first rollback stops it there; and the page shows each
// machine's end and why it stopped.
func TestFleetDashRolloutRunsAndShows(t *testing.T) {
	gone := "m9999999999999999"
	d, a, b, fu := rolloutLab(t, func(a, b string) map[string]fakeRun {
		return map[string]fakeRun{a: {end: fleetUpdDone, polls: 1}, gone: {refuse: "fleet: unknown machine"}, b: {end: fleetUpdRolledBack, polls: 1}}
	})
	ctl := newDashFleet(d.l.c, d.l.stg, &d.l.stgMu)
	if err := ctl.StartRollout([]string{a.id, gone, b.id}, "v2.0.0", "arm64", true, 1); err != nil {
		t.Fatal(err)
	}
	s := waitRollout(t, fleetRolloutOf(t))
	if got := statuses(s); got != a.id+"=done "+gone+"=skipped "+b.id+"=rolled_back" || s.Stopped != "stopped at web2" || s.Version != "v2.0.0" || s.Limit != 1 {
		t.Errorf("the rollout: %s, stopped %q, version %q, limit %d", got, s.Stopped, s.Version, s.Limit)
	}
	if got := strings.Join(fu.started(), " "); got != a.id+" "+b.id {
		t.Errorf("updates started for %s", got)
	}
	v := ctl.View().Rollout
	if v == nil || v.Running || v.Stopped != "stopped at web2" || len(v.Machines) != 3 || v.Machines[0].Name != "web1" || v.Machines[1].Name != gone ||
		v.Machines[1].Reason != "fleet: unknown machine" || v.Machines[2].Status != "rolled_back" || v.Finished == "" {
		t.Fatalf("the view: %+v", v)
	}
	page := d.page("/fleet")
	for _, want := range []string{`Rollout <span class="dim">· to v2.0.0, 1 at a time</span>`, "The rollout was stopped at web2.",
		`<a class="linkbtn" href="/fleet/` + a.id + `">web1</a> <span class="on-tag">done</span>`, `<span class="act-errtag">rolled back</span>`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page is missing %q", want)
		}
	}

	// The page's own Update: a signed-in browser, its step taken.
	fu2 := newFakeUpdates(t, fleetRolloutLimit, map[string]fakeRun{a.id: {end: fleetUpdDone, hold: true}, b.id: {end: fleetUpdDone}})
	fleetRolloutOf(t).updates = fu2
	if ans := d.post("/fleet/bulk", "action", "update", "version", "v2.1.0", "at_once", "2", "machine", b.id, "machine", a.id); ans.code != 200 || !strings.Contains(ans.body, "The rollout started: 2 machines, 2 at a time.") {
		t.Fatalf("Update from the page: %d %q", ans.code, ans.errMsg)
	}
	waitFor(t, func() bool { return len(fu2.started()) == 2 })
	if got := fu2.started(); got[0] != a.id || got[1] != b.id {
		t.Errorf("started %q, want the list's order", got)
	}
	v = ctl.View().Rollout
	if v == nil || !v.Running || v.Limit != 2 || v.Version != "v2.1.0" || v.Machines[0].ID != a.id {
		t.Fatalf("the running rollout: %+v", v)
	}
	if m := v.Machines[0]; m.Status != fleetRollRunning || m.Stage != fleetUpdFetching {
		t.Errorf("a running machine and its stage: %+v", m)
	}
	if err := ctl.StartRollout([]string{a.id}, "", "", false, 1); err == nil || !strings.Contains(err.Error(), "a rollout is running already") {
		t.Errorf("a second rollout while one runs: %v", err)
	}
	// Stop from the page: no step.
	if ans := d.send("POST", "/fleet/rollout/stop", nil, d.browser); !strings.Contains(ans.msg, "starts no more machines") {
		t.Errorf("Stop from the page: %d %q %q", ans.code, ans.msg, ans.errMsg)
	}
	fu2.release(a.id)
	if s := waitRollout(t, fleetRolloutOf(t)); s.Stopped != "stopped by the operator" {
		t.Errorf("stopped: %q", s.Stopped)
	}
}

// A process that does not hold the fleet starts no rollout, in Update's words.
func TestFleetDashRolloutHeldElsewhere(t *testing.T) {
	l := newFleetLab(t)
	l.master()
	ch := l.enroll("web1", fleet.Reported{})
	l.confirm("web1", ch)
	l.sync()
	l.stop()
	l.stop = nil
	theFleet.Store(&fleetHub{cfgDir: l.cfg, store: fleet.Open(l.cfg), held: true, holder: fleet.Holder{PID: 4242, Role: "serve", Address: "0.0.0.0:9777", Since: time.Now()}})
	t.Cleanup(func() { theFleet.Store(nil) })
	ctl := newDashFleet(l.c, l.stg, &l.stgMu)
	err := ctl.StartRollout([]string{ch.id}, "", "", false, 1)
	if err == nil || !strings.Contains(err.Error(), "tenant serve (process 4242) holds the fleet") || !strings.HasSuffix(err.Error(), "and a machine is updated from the process that holds the fleet.") {
		t.Errorf("held elsewhere: %v", err)
	}
	if ctl.StopRollout() || ctl.View().Rollout != nil {
		t.Error("a process that does not hold the fleet has a rollout")
	}
}

func fleetRolloutOf(t *testing.T) *fleetRollout {
	t.Helper()
	ro := fleetRollouter()
	if ro == nil {
		t.Fatal("no rollout")
	}
	return ro
}
