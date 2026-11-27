package main

// fleet_dashrollout_test.go: the dashboard's rollout (TEN-546, 2.6) through
// dashFleet, over this process's real fleetRollout with a fake of its
// per-machine seam (fakeUpdates, fleetrollout_test.go): no machine is really
// updated and no release is fetched. The machines are a fleetLab's, enrolled
// and confirmed; the page is the real dashboard over real HTTP.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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

// A master that starts again takes up what the one before it saved
// (TEN-552). This is the real hub (startFleetHub) started a second time on
// the same dirs and port, and the real dashboard over it:
//
//   - a saved file that does not read does not stop it: it holds the fleet,
//     and shows no rollout;
//   - a rollout it finds in the middle is closed where it stood, the operator
//     is told, and the Fleet page says which machine was being updated, which
//     never started, and that the one that took its update has not redialled;
//   - when that machine links again (a real child, saying what its own
//     child-update.json holds), its word is on the page.
func TestFleetDashRolloutAfterAMasterRestart(t *testing.T) {
	d := newFleetDash(t, settings{})
	a := d.l.enroll("web1", fleet.Reported{})
	d.l.confirm("web1", a)
	b := d.l.enroll("web2", fleet.Reported{})
	d.l.confirm("web2", b)
	d.l.sync()
	// The master starts again on the port it had, so that a machine finds it.
	addr := d.mgr.Addr().String()
	if err := persistLaunchConfig(d.l.cfg, nil, func(lc *launchConfig) { lc.Fleet.Listen = addr }); err != nil {
		t.Fatal(err)
	}
	restart := func(saved string) {
		t.Helper()
		d.l.stop()
		if err := os.MkdirAll(d.l.data, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d.l.data, fleetSavedName), []byte(saved), 0o600); err != nil {
			t.Fatal(err)
		}
		d.l.stop = startFleetHub(context.Background(), d.l.c, "serve", d.l.broker, d.l.tell, slog.Default())
		eventually(t, "the master holds the fleet again", func() bool { return fleetManager() != nil })
	}

	restart(`{"saved":"2026-11-26T10:00:00Z","machines":{"` + a.id + `":{"machine":"` + a.id + `","sta`)
	if _, ok := fleetRolloutOf(t).State(); ok {
		t.Error("a rollout came out of a file that does not read")
	}
	if page := d.page("/fleet"); strings.Contains(page, "Rollout <span") || !strings.Contains(page, `<td class="dim" colspan="3">none sent from this Tenant</td>`) {
		t.Error("the page after a saved file that does not read: a rollout, or no Updates card")
	}

	began, saved := time.Now().Add(-10*time.Minute), time.Now().Add(-5*time.Minute)
	doc, err := json.Marshal(fleetSaved{Saved: saved,
		Machines: map[string]*fleetUpdateState{a.id: {Machine: a.id, From: "1.2.3", To: "9.9.9", Stage: fleetUpdApplying, Started: began, Lines: []string{"sent 0.1 MB"}}},
		Rollout: &fleetRolloutState{Started: began, Limit: 1, Version: "v9.9.9", Machines: []fleetRolloutMachine{
			{ID: a.id, Name: "web1", Status: fleetRollRunning}, {ID: b.id, Name: "web2", Status: fleetRollWaiting}}}})
	if err != nil {
		t.Fatal(err)
	}
	restart(string(doc))
	s, ok := fleetRolloutOf(t).State()
	if !ok || !s.Over() || s.Stopped != fleetRolloutCut || statuses(s) != a.id+"=interrupted "+b.id+"=not_started" {
		t.Fatalf("the rollout after the restart: %s, stopped %q", statuses(s), s.Stopped)
	}
	if told := strings.Join(d.l.told(), "\n"); !strings.Contains(told, "fleet: the rollout started "+fleetTime(began)+" was cut short when this Tenant stopped, and is not taken up again. Of its 2 machines, 0 had ended, 1 never started, being updated: web1.") {
		t.Errorf("the operator was told:\n%s", told)
	}
	ctl := newDashFleet(d.l.c, d.l.stg, &d.l.stgMu)
	away := "has not redialled since " + fleetTime(saved)
	page := d.page("/fleet")
	for _, want := range []string{"The rollout was cut short when this Tenant stopped.",
		`<a class="linkbtn" href="/fleet/` + a.id + `">web1</a> <span class="risk">interrupted</span> <span class="dim">this Tenant stopped while the machine was applying the update`,
		`<span class="tag">not started</span> <span class="dim">not started: the rollout was cut short when this Tenant stopped</span>`,
		`<td>to 9.9.9</td>`, `<span class="act-errtag">failed</span> <strong>` + away + `</strong>`,
		`<td class="dim" colspan="3">none sent from this Tenant</td>`} {
		if !strings.Contains(page, want) {
			t.Errorf("the Fleet page after the restart is missing %q", want)
		}
	}
	if strings.Contains(page, "Stop the rollout") {
		t.Error("a rollout a restart cut short can be stopped")
	}
	if one := d.page("/fleet/" + a.id); !strings.Contains(one, "This machine took the update and "+away+".") {
		t.Errorf("the machine's own page does not say it %s", away)
	}
	var api struct {
		Machines []struct {
			ID     string `json:"id"`
			Update *struct {
				Stage string `json:"stage"`
				Away  string `json:"not_redialled_since"`
			} `json:"update"`
		} `json:"machines"`
	}
	if ans := d.send("GET", "/api/fleet", nil, d.browser); json.Unmarshal([]byte(ans.body), &api) != nil || len(api.Machines) != 2 {
		t.Fatalf("/api/fleet: %d %s", ans.code, ans.body)
	}
	for _, m := range api.Machines {
		if m.ID == a.id && (m.Update == nil || m.Update.Stage != fleetUpdFailed || m.Update.Away != fleetTime(saved)) {
			t.Errorf("/api/fleet for web1: %+v", m.Update)
		}
		if m.ID == b.id && m.Update != nil {
			t.Errorf("/api/fleet gives web2 an update: %+v", m.Update)
		}
	}

	// web1 links again: its child says what its own record holds.
	if err := fleet.WriteChildUpdate(a.dir, fleet.ChildUpdate{UpdateReport: fleet.UpdateReport{From: "1.2.3", To: "9.9.9", State: fleet.UpdateRolledBack,
		Reason: "the new version did not reach the master within 1m0s", At: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	startChild(t, a.dir)
	eventually(t, "web1's word on its next link is recorded", func() bool {
		return ctl.View().Rollout.Machines[0].Stage == fleetUpdRolledBack
	})
	page = d.page("/fleet")
	for _, want := range []string{`<span class="act-errtag">interrupted, then rolled back</span> <span class="dim">the machine linked again: rolled back to`,
		`&#34;the new version did not reach the master within 1m0s&#34;`, `<span class="act-errtag">rolled back</span> <span class="dim">the machine linked again`} {
		if !strings.Contains(page, want) {
			t.Errorf("the Fleet page once web1 is back is missing %q", want)
		}
	}
	if strings.Contains(page, "has not redialled") {
		t.Error("the page still says web1 has not redialled")
	}
	// The rollout a restart closed does not stand in the way of the next.
	fleetRolloutOf(t).updates = newFakeUpdates(t, fleetRolloutLimit, map[string]fakeRun{a.id: {end: fleetUpdDone}, b.id: {end: fleetUpdDone}})
	if err := ctl.StartRollout([]string{a.id, b.id}, "v9.9.9", "", false, 1); err != nil {
		t.Fatalf("a new rollout after the restart: %v", err)
	}
	waitRollout(t, fleetRolloutOf(t))
}
