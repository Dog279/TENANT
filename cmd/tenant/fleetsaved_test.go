package main

// fleetsaved_test.go: what a restart of the master keeps of the fleet's
// updates and its rollout (TEN-552): fleet-updates.json written as they
// change, read back by the Tenant that starts next, and a rollout cut in the
// middle by the master going away.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/fleet"
)

// savedDoc is fleet-updates.json in dir, as it is on disk; ok is false when
// it is not there or does not read.
func savedDoc(dir string) (doc fleetSaved, raw []byte, ok bool) {
	raw, err := os.ReadFile(filepath.Join(dir, fleetSavedName))
	return doc, raw, err == nil && json.Unmarshal(raw, &doc) == nil
}

// Each machine's update and the rollout are written as they change. The next
// Tenant reads them back: an update that had ended, as it ended; one that was
// under way, ended by how far it had got.
func TestFleetUpdatesSavedAndRestored(t *testing.T) {
	dir := t.TempDir()
	u := newFleetUpdates(dir)
	began := time.Now().Add(-time.Hour)
	put := func(id, stage string) *fleetUpdateState {
		s := &fleetUpdateState{Machine: id, From: "1.0.0", To: "2.0.0", Stage: stage, Started: began}
		u.mu.Lock()
		u.runs[id] = s
		u.mu.Unlock()
		u.say(s, "", "a word about "+id)
		return s
	}
	u.say(put("done", fleetUpdApplying), fleetUpdDone, "back on the new version 2.0.0")
	u.say(put("back", fleetUpdApplying), fleetUpdRolledBack, `rolled back to "1.0.0": "it broke"`)
	u.silent(put("quiet", fleetUpdApplying), "the machine has not said within 7m0s whether it runs 2.0.0 or rolled back")
	put("fetching", fleetUpdFetching)
	put("sending", fleetUpdSending)
	earlier := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	u.aim(put("applying", fleetUpdApplying), fleet.MachineStatus{Update: &fleet.UpdateReport{To: "2.0.0", State: fleet.UpdateRolledBack, At: earlier}}, "2.0.0")
	u.keepRollout(fleetRolloutState{Started: began, Limit: 2, Version: "v2.0.0", Machines: []fleetRolloutMachine{
		{ID: "done", Name: "web1", Status: fleetUpdDone, Reason: "back on the new version 2.0.0"},
		{ID: "applying", Name: "web2", Status: fleetRollRunning},
		{ID: "never", Name: "web3", Status: fleetRollWaiting}}})

	doc, raw, ok := savedDoc(dir)
	if !ok || len(doc.Machines) != 6 || doc.Rollout == nil || time.Since(doc.Saved) > time.Minute {
		t.Fatalf("the saved file: ok %v\n%s", ok, raw)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(left) != 0 {
		t.Errorf("a save left its temporary file: %v", left)
	}

	next := newFleetUpdates(dir)
	ro, at, err := next.restore()
	if err != nil || ro == nil || !at.Equal(doc.Saved) {
		t.Fatalf("restore: %v, rollout %v, saved %s want %s", err, ro, at, doc.Saved)
	}
	if ro.Version != "v2.0.0" || ro.Limit != 2 || ro.Over() || statuses(*ro) != "done=done applying=running never=waiting" || ro.Machines[1].Name != "web2" {
		t.Errorf("the rollout read back: %+v", ro)
	}
	for _, c := range []struct {
		id, stage, last string
		unheard, cut    bool // cut: this Tenant's stop ended it
		mayYetSay       bool // Cut: the machine's later word corrects it, and it is not "away"
	}{
		{"done", fleetUpdDone, "back on the new version 2.0.0", false, false, false},
		{"back", fleetUpdRolledBack, `rolled back to "1.0.0": "it broke"`, false, false, false},
		{"quiet", fleetUpdFailed, "has not said within", true, false, false},
		{"fetching", fleetUpdFailed, "before anything was sent to the machine: nothing changed there", false, true, false},
		{"sending", fleetUpdFailed, "Had the machine all of it, how it went is recorded when it links again", false, true, true},
		{"applying", fleetUpdFailed, "while the machine was applying the update", true, true, false},
	} {
		was, _ := u.State(c.id)
		st, ok := next.State(c.id)
		if !ok || st.Stage != c.stage || st.Unheard != c.unheard || st.Cut != c.mayYetSay || !st.Over() || !strings.Contains(lastLine(st.Lines), c.last) ||
			st.From != "1.0.0" || st.To != "2.0.0" || !st.Started.Equal(began) || st.Machine != c.id {
			t.Errorf("%s read back: %+v", c.id, st)
		}
		switch {
		case c.cut && (!st.Finished.Equal(doc.Saved) || len(st.Lines) != len(was.Lines)+1):
			t.Errorf("%s, cut short: ended %s (saved %s), lines %q", c.id, st.Finished, doc.Saved, st.Lines)
		case !c.cut && (!st.Finished.Equal(was.Finished) || len(st.Lines) != len(was.Lines)):
			t.Errorf("%s had ended and reads back otherwise: %+v, was %+v", c.id, st, was)
		}
	}
	// What restore made of them is on disk, and reading it again changes nothing.
	// The rollout is still in it: the file is written again before the rollout
	// is handed on, and a Tenant that died there would have lost it.
	if doc, _, ok := savedDoc(dir); !ok || doc.Machines["applying"].Stage != fleetUpdFailed || !doc.Machines["applying"].Unheard || doc.Rollout == nil || len(doc.Machines) != 6 {
		t.Errorf("the file after restore: %+v, rollout %v, %d machines", doc.Machines["applying"], doc.Rollout, len(doc.Machines))
	}
	before, _ := next.State("applying")
	if _, _, err := next.restore(); err != nil {
		t.Fatal(err)
	}
	if again, _ := next.State("applying"); len(again.Lines) != len(before.Lines) {
		t.Errorf("a second restore spoke again: %q", again.Lines)
	}
	// The time on the word a machine already had about this version is kept
	// too: without it a restarted master could not tell that word from a new one.
	if st, _ := next.State("applying"); !st.Earlier.Equal(earlier) {
		t.Errorf("the earlier word's time read back as %s, want %s", st.Earlier, earlier)
	}
	// A process that takes the fleet again reads the file again (another
	// Tenant may have held the fleet in between). What it has itself stays
	// while it runs, and when it is the same record or a later one; a saved
	// record that started later replaces one of its own that has ended.
	later := newFleetUpdates(dir)
	later.runs["done"] = &fleetUpdateState{Machine: "done", Stage: fleetUpdSending, Started: began.Add(-time.Hour)}
	later.runs["back"] = &fleetUpdateState{Machine: "back", Stage: fleetUpdDone, To: "3.0.0", Started: began.Add(time.Hour), Finished: began.Add(2 * time.Hour)}
	later.runs["quiet"] = &fleetUpdateState{Machine: "quiet", Stage: fleetUpdDone, To: "0.9.0", Started: began.Add(-time.Hour), Finished: began.Add(-time.Minute)}
	if _, _, err := later.restore(); err != nil {
		t.Fatal(err)
	}
	if st, _ := later.State("done"); st.Stage != fleetUpdSending || st.Over() {
		t.Errorf("restore replaced an update this process is running: %+v", st)
	}
	if st, _ := later.State("back"); st.To != "3.0.0" || st.Stage != fleetUpdDone {
		t.Errorf("restore replaced a later update of this process's with an earlier one: %+v", st)
	}
	if st, _ := later.State("quiet"); st.To != "2.0.0" || !st.Unheard {
		t.Errorf("restore kept an ended update of this process's over a later one that was saved: %+v", st)
	}
}

// A saved file that does not read, or holds nothing, does not stop the
// master: restore says so and holds no record, and the next change writes a
// file that reads.
func TestFleetUpdatesSavedFileThatDoesNotRead(t *testing.T) {
	for name, c := range map[string]struct {
		content string
		reads   bool
	}{
		"zero bytes after a power cut": {"\x00\x00\x00\x00", false},
		"half-written":                 {`{"saved":"2026-11-26T10:00:00Z","machines":{"m1":{"machine":"m1","sta`, false},
		"empty":                        {"", false},
		"another shape":                {`[1, 2, 3]`, false},
		"a time that is not one":       {`{"saved":"yesterday"}`, false},
		"nothing in it":                {`{}`, true},
		"a machine that is null":       {`{"machines":{"m1":null},"rollout":null}`, true},
		"larger than is read": {`{"machines":{"m1":{"stage":"done","to":"2.0.0","started":"2026-11-26T10:00:00Z","finished":"2026-11-26T10:01:00Z","lines":["` +
			strings.Repeat("x", 8<<20) + `"]}}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, fleetSavedName), []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
			u := newFleetUpdates(dir)
			ro, _, err := u.restore()
			if (err == nil) != c.reads || ro != nil {
				t.Fatalf("restore: rollout %v, err %v", ro, err)
			}
			if err != nil && !strings.Contains(err.Error(), fleetSavedName+" does not read") {
				t.Errorf("the error does not name the file: %v", err)
			}
			if _, ok := u.State("m1"); ok {
				t.Error("a record came out of a file that holds none")
			}
			u.keepRollout(fleetRolloutState{Started: time.Now(), Limit: 1, Machines: []fleetRolloutMachine{{ID: "m1", Name: "web1", Status: fleetRollWaiting}}})
			if doc, raw, ok := savedDoc(dir); !ok || doc.Rollout == nil {
				t.Errorf("the next change did not replace the file:\n%s", raw)
			}
		})
	}
	// No file at all, and no data dir yet: no record, no error, and the first
	// change makes both.
	dir := filepath.Join(t.TempDir(), "not", "made", "yet")
	u := newFleetUpdates(dir)
	if ro, _, err := u.restore(); ro != nil || err != nil {
		t.Fatalf("with no file: %v %v", ro, err)
	}
	u.keepRollout(fleetRolloutState{Started: time.Now(), Limit: 1})
	if _, _, ok := savedDoc(dir); !ok {
		t.Error("the first change did not make the file")
	}
}

// A machine whose update ended with nobody having its word is settled by
// what it says on a later link, and by nothing else.
func TestFleetUpdateHeardOnALaterLink(t *testing.T) {
	r := newUpdateRig(t)
	k, restart := r.kid("web1", func(p fleet.Update, _ func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
		return honest(p), nil
	})
	quiet := func() {
		s := &fleetUpdateState{Machine: k.id, From: "1.0.0", To: "2.0.0", Stage: fleetUpdApplying, Started: time.Now()}
		r.updates.mu.Lock()
		r.updates.runs[k.id] = s
		r.updates.mu.Unlock()
		r.updates.silent(s, "the machine has not said")
	}
	state := func() fleetUpdateState {
		st, _ := r.updates.State(k.id)
		return st
	}
	said := func(state, to, reason string) func() *fleet.UpdateReport {
		return func() *fleet.UpdateReport {
			return &fleet.UpdateReport{From: "1.0.0", To: to, State: state, Reason: reason}
		}
	}

	// Linked on what it had, with no word, a word about another update, or in
	// its trial run: the record waits on.
	quiet()
	for name, hello := range map[string]func() *fleet.UpdateReport{
		"no word": nil, "another update": said(fleet.UpdateRolledBack, "1.5.0", "an older story"), "its trial run": said(fleet.UpdateTrial, "2.0.0", "")} {
		restart(hello, "1.0.0")
		r.updates.heard(r.mgr, k.id)
		if st := state(); !st.Unheard || st.Stage != fleetUpdFailed || len(st.Lines) != 1 {
			t.Errorf("%s settled the update: %+v", name, st)
		}
	}
	// Offline: away since its link ended, and nothing settled.
	k.stop()
	r.online(k, false)
	r.updates.heard(r.mgr, k.id)
	link, _ := machineStatus(r.mgr, k.id)
	if v := dashUpdate(state(), &link); v.Away != dashTime(link.Since) || v.Away == "" || v.Running {
		t.Errorf("offline and unheard: away %q, link ended %s", v.Away, link.Since)
	}
	if v := dashUpdate(state(), &fleet.MachineStatus{}); v.Away != dashTime(state().Finished) {
		t.Errorf("not seen since this master started: away %q, want when the update was last known of", v.Away)
	}
	if v := dashUpdate(state(), nil); v.Away != "" {
		t.Errorf("a process without the links says a machine is away: %q", v.Away)
	}

	// It says it rolled back, in words of its own.
	restart(said(fleet.UpdateRolledBack, "2.0.0", "it \x1b]0;pwned\x07 broke"), "1.0.0")
	r.updates.heard(r.mgr, k.id)
	st := state()
	if st.Unheard || st.Stage != fleetUpdRolledBack || lastLine(st.Lines) != `the machine linked again: rolled back to "1.0.0": "it  ]0;pwned  broke"` || st.Finished.Before(st.Started) {
		t.Errorf("rolled back, heard later: %+v", st)
	}
	link, _ = machineStatus(r.mgr, k.id)
	if v := dashUpdate(st, &link); v.Away != "" {
		t.Errorf("a machine that is back is still away: %q", v.Away)
	}
	r.updates.heard(r.mgr, k.id) // settled: a later link adds nothing
	if again := state(); len(again.Lines) != len(st.Lines) {
		t.Errorf("a settled update was spoken of again: %q", again.Lines)
	}

	// It is back on the version it was sent, whatever it says of the update.
	quiet()
	restart(nil, "2.0.0")
	r.updates.heard(r.mgr, k.id)
	if st := state(); st.Unheard || st.Stage != fleetUpdDone || lastLine(st.Lines) != "the machine linked again: back on the new version 2.0.0" {
		t.Errorf("done, heard later: %+v", st)
	}
	// It says it is done on a version it does not run: not taken as done.
	quiet()
	restart(said(fleet.UpdateDone, "2.0.0", ""), "1.0.0")
	r.updates.heard(r.mgr, k.id)
	if st := state(); !st.Unheard || st.Stage != fleetUpdFailed {
		t.Errorf("done on a version it does not run was believed: %+v", st)
	}
	// A record that names no version (a saved file that was cut, or edited) is
	// settled by nothing: a machine that reports no version would match it.
	nameless := &fleetUpdateState{Machine: k.id, From: "1.0.0", Stage: fleetUpdApplying, Started: time.Now()}
	r.updates.mu.Lock()
	r.updates.runs[k.id] = nameless
	r.updates.mu.Unlock()
	r.updates.silent(nameless, "the machine has not said")
	restart(nil, "")
	if link, _ := machineStatus(r.mgr, k.id); !link.Online || link.Reported.Version != "" {
		t.Fatalf("the machine was to report no version: %+v", link.Reported)
	}
	r.updates.heard(r.mgr, k.id)
	if st := state(); !st.Unheard || st.Stage != fleetUpdFailed || len(st.Lines) != 1 {
		t.Errorf("a record that names no version was settled by a machine that reports none: %+v", st)
	}
	// An update that is running is its own loop's to end.
	r.updates.mu.Lock()
	r.updates.runs[k.id] = &fleetUpdateState{Machine: k.id, To: "1.0.0", Stage: fleetUpdApplying, Started: time.Now()}
	r.updates.mu.Unlock()
	r.updates.heardAll(r.mgr)
	if st := state(); st.Over() || len(st.Lines) != 0 {
		t.Errorf("heard ended an update that is running: %+v", st)
	}
}

// TestRolloutAcrossAMasterRestart is a rollout of three machines, one at a
// time, whose master goes away while the second is applying its update. The
// master is real (a manager, its updates, its rollout) and so are the links;
// "goes away" is what a kill leaves: the manager closed and the saved file as
// it was at that moment, read by a second set of updates and rollout as the
// Tenant that starts next reads it (fleetHub.restore).
//
// The Tenant that starts next reports exactly where the rollout stopped (the
// first machine done, the second being updated, the third never started),
// sends nothing more, and records what the second machine says on its next
// link. A new rollout over the same machines then sends only to the third.
func TestRolloutAcrossAMasterRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("updates machines over real links, and waits out an update whose master is gone")
	}
	r := newUpdateRig(t)
	var mu sync.Mutex
	pieces := map[string]int{}              // the pieces each machine was sent
	comesBack := map[string]bool{"a": true} // these restart into the new version once they hold it
	done := func() *fleet.UpdateReport {
		return &fleet.UpdateReport{From: "1.0.0", To: "2.0.0", State: fleet.UpdateDone}
	}
	machine := func(name string) (*fleetKid, func(func() *fleet.UpdateReport, string)) {
		return r.kid(name, func(p fleet.Update, again func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
			mu.Lock()
			pieces[name]++
			back := comesBack[name]
			mu.Unlock()
			ack := honest(p)
			if ack.Applying && back {
				again(done, "2.0.0")
			}
			return ack, nil
		})
	}
	sent := func(name string) int {
		mu.Lock()
		defer mu.Unlock()
		return pieces[name]
	}
	a, restartA := machine("a")
	b, restartB := machine("b")
	c, restartC := machine("c")
	machines := []fleetRolloutMachine{{ID: a.id, Name: "a"}, {ID: b.id, Name: "b"}, {ID: c.id, Name: "c"}}

	first := r.updates
	ro := newFleetRollout(first)
	ro.holds = nil // this test's manager is not the process's hub
	if err := ro.Start(r.mgr, machines, fleetUpdateOpts{version: "v2.0.0"}, 1); err != nil {
		t.Fatal(err)
	}
	// The master is killed while b applies: this is what its disk holds.
	var disk []byte
	eventually(t, "a is done and b is applying, on disk", func() bool {
		doc, raw, ok := savedDoc(first.dataDir)
		if !ok || doc.Rollout == nil || doc.Machines[b.id] == nil {
			return false
		}
		disk = raw
		return statuses(*doc.Rollout) == a.id+"=done "+b.id+"=running "+c.id+"=waiting" && doc.Machines[b.id].Stage == fleetUpdApplying
	})
	_ = r.mgr.Close()
	// The first master's goroutines are still in this process, where a kill
	// would have ended them: they give up on b after the rig's five seconds,
	// in their own data dir. The test ends after they do.
	defer func() {
		if s := waitRollout(t, ro); statuses(s) != a.id+"=done "+b.id+"=failed "+c.id+"=not_started" {
			t.Errorf("the first master's own end: %s", statuses(s))
		}
	}()
	sentB := sent("b")

	// The Tenant that starts next, on what the disk held.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fleetSavedName), disk, 0o600); err != nil {
		t.Fatal(err)
	}
	updates := newFleetUpdates(dir)
	rollout := newFleetRollout(updates)
	rollout.holds = nil
	var held atomic.Pointer[fleet.Manager]
	var mgr *fleet.Manager
	eventually(t, "the fleet port is free again", func() bool {
		var err error
		mgr, err = fleet.StartManager(fleet.ManagerConfig{Store: r.store, Identity: r.master, Addr: r.addr, Role: "test",
			Connected: func(id string) {
				if m := held.Load(); m != nil {
					updates.heard(m, id)
				}
			}})
		return err == nil
	})
	r.mu.Lock()
	r.mgr = mgr // the rig closes it
	r.mu.Unlock()
	saved, at, err := updates.restore()
	if err != nil || saved == nil {
		t.Fatalf("restore: %v %v", saved, err)
	}
	rollout.restore(*saved, at)
	held.Store(mgr)
	updates.heardAll(mgr)

	// Exactly where it stopped.
	st, ok := rollout.State()
	if !ok || !st.Over() || st.Stopped != fleetRolloutCut || !st.Finished.Equal(at) || st.Version != "v2.0.0" ||
		statuses(st) != a.id+"=done "+b.id+"=interrupted "+c.id+"=not_started" {
		t.Fatalf("the rollout after the restart: %s, stopped %q, over %v", statuses(st), st.Stopped, st.Over())
	}
	if got := st.Machines[2].Reason; got != "not started: the rollout was cut short when this Tenant stopped" {
		t.Errorf("why c never started: %q", got)
	}
	if got := rolloutCutWords(st); !strings.Contains(got, "Of its 3 machines, 1 had ended, 1 never started, being updated: b.") {
		t.Errorf("what the operator is told: %s", got)
	}
	ub, _ := updates.State(b.id)
	if ub.Stage != fleetUpdFailed || !ub.Unheard || ub.To != "2.0.0" || !strings.Contains(lastLine(ub.Lines), "this Tenant stopped while the machine was applying the update") {
		t.Fatalf("b's update after the restart: %+v", ub)
	}
	if ua, _ := updates.State(a.id); ua.Stage != fleetUpdDone || ua.Unheard {
		t.Errorf("a's update after the restart: %+v", ua)
	}
	if _, ok := updates.State(c.id); ok {
		t.Error("c has an update, and was never sent one")
	}
	if row := dashRollout(st, updates).Machines[1]; row.Status != fleetRollInterrupted || row.Stage != fleetUpdFailed || row.Reason != lastLine(ub.Lines) || row.Words() != "interrupted" {
		t.Errorf("b's row on the page: %+v", row)
	}
	if v := dashUpdate(ub, &fleet.MachineStatus{}); v.Away == "" {
		t.Error("b took an update, has not been seen since, and the page does not say so")
	}

	// b comes back on the new version: its word is recorded, and nothing was
	// sent to it again.
	restartB(done, "2.0.0")
	eventually(t, "b's word on its next link is recorded", func() bool {
		ub, _ = updates.State(b.id)
		return ub.Stage == fleetUpdDone
	})
	if ub.Unheard || lastLine(ub.Lines) != "the machine linked again: back on the new version 2.0.0" {
		t.Errorf("b, heard: %+v", ub)
	}
	st, _ = rollout.State()
	if row := dashRollout(st, updates).Machines[1]; row.Words() != "interrupted, then done" || row.Pill() != "on-tag" {
		t.Errorf("b's row once it is back: %+v (%s)", row, row.Words())
	}
	if doc, _, ok := savedDoc(dir); !ok || doc.Machines[b.id].Stage != fleetUpdDone || doc.Rollout == nil || !doc.Rollout.Over() {
		t.Errorf("the disk after b was heard: %+v", doc.Machines[b.id])
	}
	if sent("b") != sentB || sent("c") != 0 {
		t.Errorf("the restarted master sent something by itself: b had %d pieces and has %d, c has %d", sentB, sent("b"), sent("c"))
	}

	// A new rollout over the same machines: only c is sent anything.
	mu.Lock()
	comesBack["c"] = true
	mu.Unlock()
	restartA(done, "2.0.0")
	restartC(nil, "1.0.0")
	if err := rollout.Start(mgr, machines, fleetUpdateOpts{version: "v2.0.0"}, 1); err != nil {
		t.Fatal(err)
	}
	st = waitRollout(t, rollout)
	if statuses(st) != a.id+"=done "+b.id+"=done "+c.id+"=done" || st.Stopped != "" {
		t.Fatalf("the new rollout: %s, stopped %q; %q", statuses(st), st.Stopped, st.Machines)
	}
	for i, want := range []string{"already runs 2.0.0", "already runs 2.0.0", "back on the new version 2.0.0"} {
		if !strings.Contains(st.Machines[i].Reason, want) {
			t.Errorf("machine %d of the new rollout: %q, want %q", i, st.Machines[i].Reason, want)
		}
	}
	if sent("b") != sentB || sent("c") == 0 {
		t.Errorf("the new rollout: b has %d pieces (had %d), c has %d", sent("b"), sentB, sent("c"))
	}
}

// A machine keeps its word about the last update until the next one starts
// its trial run, which is after it has taken the archive and let its running
// calls finish. So on a second try at a version that rolled back before, a
// link the machine makes in between says what became of the FIRST try. That
// word is not this update's: not for the master that is waiting (the machine
// redials while its calls finish), and not for one that restarted meanwhile.
// Found in review; the update rig's machine here says exactly what a real
// child's file would hold at each moment.
func TestFleetUpdateAnEarlierTrysWordIsNotThisOnes(t *testing.T) {
	if testing.Short() {
		t.Skip("updates a machine over a real link")
	}
	r := newUpdateRig(t)
	yesterdayAt := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	yesterday := func() *fleet.UpdateReport {
		return &fleet.UpdateReport{From: "1.0.0", To: "2.0.0", State: fleet.UpdateRolledBack, Reason: "yesterday's reason", At: yesterdayAt}
	}
	today := func(state, reason string) func() *fleet.UpdateReport {
		return func() *fleet.UpdateReport {
			return &fleet.UpdateReport{From: "1.0.0", To: "2.0.0", State: state, Reason: reason, At: time.Now().UTC().Truncate(time.Second)}
		}
	}
	var mu sync.Mutex
	var took time.Time // when the machine had the whole archive
	k, restart := r.kid("web1", func(p fleet.Update, again func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
		ack := honest(p)
		if ack.Applying {
			mu.Lock()
			took = time.Now()
			mu.Unlock()
			again(yesterday, "1.0.0") // it links again while its calls finish, with the only word it has
		}
		return ack, nil
	})
	state := func() fleetUpdateState {
		st, _ := r.updates.State(k.id)
		return st
	}
	relinked := func() bool { // on a link made after it took the archive
		mu.Lock()
		defer mu.Unlock()
		link, ok := machineStatus(r.mgr, k.id)
		return ok && link.Online && !took.IsZero() && link.Since.After(took)
	}

	// The master is waiting. The machine starts with yesterday's word.
	restart(yesterday, "1.0.0")
	if err := r.updates.Start(r.mgr, k.id, fleetUpdateOpts{version: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the machine links again after it took the archive", relinked)
	time.Sleep(10 * fleetUpdatePoll) // the wait loop has looked at that link several times
	if st := state(); st.Over() || st.Stage != fleetUpdApplying || !st.Earlier.Equal(yesterdayAt) {
		t.Fatalf("yesterday's word ended today's update, or its time was not kept: %+v", st)
	}
	restart(today(fleet.UpdateTrial, ""), "1.0.0") // its trial run, then the new version
	restart(today(fleet.UpdateDone, ""), "2.0.0")
	eventually(t, "today's update ends", func() bool { return state().Over() })
	if st := state(); st.Stage != fleetUpdDone || lastLine(st.Lines) != "back on the new version 2.0.0" {
		t.Errorf("today's update, with the master waiting: %+v", st)
	}

	// The master restarted while the machine applied (restore ends the record
	// so). The record is as Start and aim left it: yesterday's word's time in it.
	quiet := func(cut bool) {
		s := &fleetUpdateState{Machine: k.id, From: "1.0.0", To: "2.0.0", Stage: fleetUpdApplying, Started: time.Now()}
		r.updates.mu.Lock()
		r.updates.runs[k.id] = s
		r.updates.mu.Unlock()
		r.updates.aim(s, fleet.MachineStatus{Update: yesterday()}, "2.0.0")
		if cut {
			r.updates.mu.Lock()
			s.Cut = true
			r.updates.mu.Unlock()
			r.updates.say(s, fleetUpdFailed, "this Tenant stopped while the archive was on its way to the machine")
			return
		}
		r.updates.silent(s, "this Tenant stopped while the machine was applying the update")
	}
	quiet(false)
	restart(yesterday, "1.0.0")
	r.updates.heard(r.mgr, k.id)
	if st := state(); !st.Unheard || st.Stage != fleetUpdFailed || len(st.Lines) != 1 {
		t.Fatalf("yesterday's word settled today's update on the restarted master: %+v", st)
	}
	restart(today(fleet.UpdateDone, ""), "2.0.0")
	r.updates.heard(r.mgr, k.id)
	if st := state(); st.Unheard || st.Stage != fleetUpdDone || lastLine(st.Lines) != "the machine linked again: back on the new version 2.0.0" {
		t.Errorf("the machine runs 2.0.0 and its record says: %+v", st)
	}
	// Today's own rollback is today's word.
	quiet(false)
	restart(yesterday, "1.0.0")
	r.updates.heard(r.mgr, k.id)
	restart(today(fleet.UpdateRolledBack, "today's reason"), "1.0.0")
	r.updates.heard(r.mgr, k.id)
	if st := state(); st.Unheard || st.Stage != fleetUpdRolledBack || lastLine(st.Lines) != `the machine linked again: rolled back to "1.0.0": "today's reason"` {
		t.Errorf("today's rollback, heard later: %+v", st)
	}
	// A word about another version is no earlier word of this one.
	other := &fleetUpdateState{Machine: "x"}
	r.updates.aim(other, fleet.MachineStatus{Update: &fleet.UpdateReport{To: "1.5.0", State: fleet.UpdateDone, At: yesterdayAt}}, "2.0.0")
	if other.To != "2.0.0" || !other.Earlier.IsZero() || !wordOn("2.0.0", time.Time{}, yesterday()) || wordOn("2.0.0", yesterdayAt, yesterday()) || wordOn("2.0.0", time.Time{}, nil) {
		t.Errorf("aim took a word about another version for an earlier try, or wordOn is off: %+v", other)
	}

	// Cut while the archive was on its way: the record says nothing changed,
	// most likely. It is not "away", yesterday's word does not touch it, and
	// if the machine had all of it after all, its word corrects the record.
	quiet(true)
	restart(yesterday, "1.0.0")
	r.updates.heard(r.mgr, k.id)
	if st := state(); !st.Cut || st.Unheard || st.Stage != fleetUpdFailed || len(st.Lines) != 1 {
		t.Fatalf("a record cut while sending, after yesterday's word: %+v", st)
	}
	if v := dashUpdate(state(), &fleet.MachineStatus{}); v.Away != "" {
		t.Errorf("a machine that most likely took nothing is said not to have redialled: %q", v.Away)
	}
	restart(today(fleet.UpdateDone, ""), "2.0.0")
	r.updates.heard(r.mgr, k.id)
	if st := state(); st.Cut || st.Stage != fleetUpdDone || lastLine(st.Lines) != "the machine linked again: back on the new version 2.0.0" {
		t.Errorf("a record cut while sending, for a machine that had it all: %+v", st)
	}
}

// updateRecords stands in for the updates a rollout's rows are read from.
type updateRecords map[string]fleetUpdateState

func (u updateRecords) State(id string) (fleetUpdateState, bool) { s, ok := u[id]; return s, ok }

// The rollout card and the Updates card do not disagree: a machine that
// failed in a rollout by saying nothing in time, or that a restart
// interrupted, shows what its update came to once the machine said. An update
// sent to the machine after the rollout is not that rollout's.
func TestDashRolloutShowsWhatBecameOfAnUpdate(t *testing.T) {
	began := time.Now().Add(-time.Hour)
	ended := began.Add(10 * time.Minute)
	ro := fleetRolloutState{Started: began, Finished: ended, Limit: 1, Stopped: "stopped at web1", Machines: []fleetRolloutMachine{
		{ID: "quiet", Name: "web1", Status: fleetUpdFailed, Reason: "the machine has not said within 7m0s"},
		{ID: "broke", Name: "web2", Status: fleetUpdFailed, Reason: "the machine went offline during the transfer"},
		{ID: "cut", Name: "web3", Status: fleetRollInterrupted, Reason: "its update was under way when this Tenant stopped"},
		{ID: "since", Name: "web4", Status: fleetUpdFailed, Reason: "the machine has not said within 7m0s"},
		{ID: "back", Name: "web5", Status: fleetUpdRolledBack, Reason: "rolled back"},
		{ID: "none", Name: "web6", Status: fleetUpdFailed, Reason: "kept"},
	}}
	in := began.Add(time.Minute)
	rows := dashRollout(ro, updateRecords{
		"quiet": {Stage: fleetUpdDone, Started: in, Lines: []string{"sent", "the machine linked again: back on the new version 2.0.0"}},
		"broke": {Stage: fleetUpdFailed, Started: in, Lines: []string{"another line"}},
		"cut":   {Stage: fleetUpdRolledBack, Started: in, Lines: []string{`the machine linked again: rolled back to "1.0.0": "why"`}},
		"since": {Stage: fleetUpdDone, Started: ended.Add(time.Minute), Lines: []string{"back on the new version 2.0.0"}}, // sent again, after the rollout
		"back":  {Stage: fleetUpdDone, Started: ended.Add(time.Minute), Lines: []string{"back on the new version 2.0.0"}},
	}).Machines
	for i, want := range []struct{ words, pill, reason string }{
		{"failed, then done", "on-tag", "the machine linked again: back on the new version 2.0.0"},
		{"failed", "act-errtag", "the machine went offline during the transfer"},
		{"interrupted, then rolled back", "act-errtag", `the machine linked again: rolled back to "1.0.0": "why"`},
		{"failed", "act-errtag", "the machine has not said within 7m0s"},
		{"rolled back", "act-errtag", "rolled back"},
		{"failed", "act-errtag", "kept"},
	} {
		if got := rows[i]; got.Words() != want.words || got.Pill() != want.pill || got.Reason != want.reason {
			t.Errorf("%s: %q (%s) %q, want %q (%s) %q", got.Name, got.Words(), got.Pill(), got.Reason, want.words, want.pill, want.reason)
		}
	}
	// While a rollout runs it has no end yet: a failed machine's later word shows.
	ro.Finished = time.Time{}
	if got := dashRollout(ro, updateRecords{"quiet": {Stage: fleetUpdDone, Started: in, Lines: []string{"done since"}}}).Machines[0]; got.Words() != "failed, then done" || got.Reason != "done since" {
		t.Errorf("in a running rollout: %q %q", got.Words(), got.Reason)
	}
}

// A machine that is linked on the new version when the master gives up
// waiting for its word is not left "failed, and waiting for its word". Its
// link was told of while the update still ran, a link lasts, and nothing
// would look again: so the wait looks once more as it ends. Found in review,
// where the link came up between the wait's last look and its end; here the
// machine comes back on the new version with no word of the update, which the
// wait reads the same way.
func TestFleetUpdateTheWaitLooksOnceMoreAsItGivesUp(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the time a machine is given to say how its update went")
	}
	r := newUpdateRig(t)
	k, _ := r.kid("web1", func(p fleet.Update, again func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
		ack := honest(p)
		if ack.Applying {
			again(nil, "2.0.0")
		}
		return ack, nil
	})
	if err := r.updates.Start(r.mgr, k.id, fleetUpdateOpts{version: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	state := func() fleetUpdateState {
		st, _ := r.updates.State(k.id)
		return st
	}
	eventually(t, "the update ends as done", func() bool { return state().Stage == fleetUpdDone })
	st := state()
	if st.Unheard || !st.Over() || lastLine(st.Lines) != "the machine linked again: back on the new version 2.0.0" ||
		!strings.Contains(st.Lines[len(st.Lines)-2], "has not said within") {
		t.Errorf("the update of a machine that was linked on 2.0.0 when the wait gave up: %+v", st)
	}
}

// What a machine says on a later link settles its record in one step. Were
// the record let go between "nobody has its word" and how it went, it would
// read as running there: Start would refuse the machine ("is being updated
// already"), and a save for another machine would write it down unfinished,
// for the next Tenant to end as "stopped before anything was sent". Found in
// review. A reader looks at the record the whole time it is settled, over and
// over.
func TestFleetUpdateHeardSettlesInOneStep(t *testing.T) {
	r := newUpdateRig(t)
	k, restart := r.kid("web1", func(p fleet.Update, _ func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
		return honest(p), nil
	})
	restart(nil, "2.0.0")
	u := newFleetUpdates("") // nothing is saved: only the record is looked at
	var running atomic.Int64
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if st, ok := u.State(k.id); ok && !st.Over() {
				running.Add(1)
			}
		}
	}()
	for i := 0; i < 1000; i++ {
		s := &fleetUpdateState{Machine: k.id, To: "2.0.0", Stage: fleetUpdFailed, Unheard: true, Started: time.Now(), Finished: time.Now()}
		u.mu.Lock()
		u.runs[k.id] = s
		u.mu.Unlock()
		u.heard(r.mgr, k.id)
		if st, _ := u.State(k.id); st.Stage != fleetUpdDone || st.Unheard || !st.Over() || len(st.Lines) != 1 {
			t.Fatalf("round %d: the machine's word did not settle the record: %+v", i, st)
		}
	}
	close(stop)
	<-stopped
	if n := running.Load(); n != 0 {
		t.Errorf("a record being settled was seen as running %d times", n)
	}
}

// The saved file is this Tenant's own, and what is read back from it is still
// held to the bounds of what is written there: an update keeps no more lines
// than one is given while it runs, however many the file holds.
func TestFleetUpdatesSavedFileIsHeldToItsBounds(t *testing.T) {
	dir := t.TempDir()
	lines := make([]string, 500)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	began := time.Now().Add(-time.Hour)
	b, err := json.Marshal(fleetSaved{Saved: time.Now(), Machines: map[string]*fleetUpdateState{
		"ended": {Stage: fleetUpdDone, To: "2.0.0", Lines: lines, Started: began, Finished: began.Add(time.Minute)},
		"cut":   {Stage: fleetUpdApplying, To: "2.0.0", Lines: lines, Started: began},
	}})
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, fleetSavedName), b, 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
	u := newFleetUpdates(dir)
	if _, _, err := u.restore(); err != nil {
		t.Fatal(err)
	}
	for id, stage := range map[string]string{"ended": fleetUpdDone, "cut": fleetUpdFailed} {
		if st, _ := u.State(id); len(st.Lines) != 32 || st.Lines[31] != "line 31" || st.Stage != stage || !st.Over() || st.Unheard != (id == "cut") {
			t.Errorf("%s read back with %d lines: stage %s, over %v, unheard %v", id, len(st.Lines), st.Stage, st.Over(), st.Unheard)
		}
	}
	if doc, raw, ok := savedDoc(dir); !ok || len(doc.Machines["cut"].Lines) != 32 || len(raw) > 8<<10 {
		t.Errorf("the file after restore is %d bytes, ok %v", len(raw), ok)
	}
}
