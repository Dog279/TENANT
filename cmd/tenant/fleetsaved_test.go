package main

// fleetsaved_test.go: what a restart of the master keeps of the fleet's
// updates and its rollout (TEN-552): fleet-updates.json written as they
// change, read back by the Tenant that starts next, and a rollout cut in the
// middle by the master going away.

import (
	"encoding/json"
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
	put("applying", fleetUpdApplying)
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
		unheard, cut    bool
	}{
		{"done", fleetUpdDone, "back on the new version 2.0.0", false, false},
		{"back", fleetUpdRolledBack, `rolled back to "1.0.0": "it broke"`, false, false},
		{"quiet", fleetUpdFailed, "has not said within", true, false},
		{"fetching", fleetUpdFailed, "before anything was sent to the machine: nothing changed there", false, true},
		{"sending", fleetUpdFailed, "while the archive was on its way", false, true},
		{"applying", fleetUpdFailed, "while the machine was applying the update", true, true},
	} {
		was, _ := u.State(c.id)
		st, ok := next.State(c.id)
		if !ok || st.Stage != c.stage || st.Unheard != c.unheard || !st.Over() || !strings.Contains(lastLine(st.Lines), c.last) ||
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
	if doc, _, ok := savedDoc(dir); !ok || doc.Machines["applying"].Stage != fleetUpdFailed || !doc.Machines["applying"].Unheard {
		t.Errorf("the file after restore: %+v", doc.Machines["applying"])
	}
	before, _ := next.State("applying")
	if _, _, err := next.restore(); err != nil {
		t.Fatal(err)
	}
	if again, _ := next.State("applying"); len(again.Lines) != len(before.Lines) {
		t.Errorf("a second restore spoke again: %q", again.Lines)
	}
	// An update this process ran since is the newer word: restore keeps it.
	later := newFleetUpdates(dir)
	mine := &fleetUpdateState{Machine: "done", Stage: fleetUpdSending, Started: time.Now()}
	later.runs["done"] = mine
	if _, _, err := later.restore(); err != nil {
		t.Fatal(err)
	}
	if st, _ := later.State("done"); st.Stage != fleetUpdSending || st.Over() {
		t.Errorf("restore replaced an update this process is running: %+v", st)
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
