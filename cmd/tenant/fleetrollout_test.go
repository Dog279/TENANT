package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/fleet"
)

// fakeUpdates stands in for fleetUpdates: each machine's update ends the way
// plan says after polls looks at its state, and the fake checks that no more
// than limit run at once.
type fakeUpdates struct {
	t     *testing.T
	limit int

	mu       sync.Mutex
	plan     map[string]fakeRun
	runs     map[string]*fleetUpdateState
	polls    map[string]int
	held     map[string]bool // stays running until released
	order    []string        // the order machines were started in
	inFlight int
	most     int
}

type fakeRun struct {
	refuse string // Start refuses with this
	end    string // the stage it ends in; "vanish": its state is gone
	polls  int    // State calls before it ends
	hold   bool
}

func newFakeUpdates(t *testing.T, limit int, plan map[string]fakeRun) *fakeUpdates {
	f := &fakeUpdates{t: t, limit: limit, plan: plan, runs: map[string]*fleetUpdateState{}, polls: map[string]int{}, held: map[string]bool{}}
	for id, p := range plan {
		f.held[id] = p.hold
	}
	return f
}

func (f *fakeUpdates) Start(_ *fleet.Manager, id string, _ fleetUpdateOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.plan[id]
	if p.refuse != "" {
		return errors.New(p.refuse)
	}
	if cur := f.runs[id]; cur != nil && !cur.Over() {
		f.t.Errorf("%s started while its update runs", id)
	}
	f.order = append(f.order, id)
	f.runs[id] = &fleetUpdateState{Machine: id, Stage: fleetUpdFetching, Started: time.Now()}
	if f.inFlight++; f.inFlight > f.most {
		f.most = f.inFlight
	}
	if f.inFlight > f.limit {
		f.t.Errorf("%d updates in flight, limit %d", f.inFlight, f.limit)
	}
	return nil
}

func (f *fakeUpdates) State(id string) (fleetUpdateState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.runs[id]
	if s == nil {
		return fleetUpdateState{}, false
	}
	p := f.plan[id]
	if !s.Over() && !f.held[id] {
		if f.polls[id]++; f.polls[id] > p.polls {
			if p.end == "vanish" {
				delete(f.runs, id)
				f.inFlight--
				return fleetUpdateState{}, false
			}
			s.Stage, s.Finished = p.end, time.Now()
			s.Lines = append(s.Lines, id+" ended "+p.end)
			f.inFlight--
		}
	}
	return *s, true
}

func (f *fakeUpdates) release(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[id] = false
}

func (f *fakeUpdates) started() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func rolloutRig(t *testing.T, limit int, plan map[string]fakeRun) (*fleetRollout, *fakeUpdates) {
	t.Helper()
	prev := fleetUpdatePoll
	fleetUpdatePoll = time.Millisecond
	t.Cleanup(func() { fleetUpdatePoll = prev })
	f := newFakeUpdates(t, max(limit, 1), plan)
	return &fleetRollout{updates: f}, f
}

func rolloutMachines(ids ...string) []fleetRolloutMachine {
	out := make([]fleetRolloutMachine, len(ids))
	for i, id := range ids {
		out[i] = fleetRolloutMachine{ID: id, Name: "name-" + id}
	}
	return out
}

// waitRollout waits for the rollout to end and returns how it ended.
func waitRollout(t *testing.T, r *fleetRollout) fleetRolloutState {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if s, ok := r.State(); ok && s.Over() {
			return s
		}
	}
	t.Fatal("the rollout did not end")
	return fleetRolloutState{}
}

// statuses is each machine as "id=status".
func statuses(s fleetRolloutState) string {
	var b []string
	for _, m := range s.Machines {
		b = append(b, m.ID+"="+m.Status)
	}
	return strings.Join(b, " ")
}

func TestRolloutAllDoneInOrderWithinLimit(t *testing.T) {
	plan := map[string]fakeRun{}
	var ids []string
	for i := range 7 {
		id := fmt.Sprintf("m%d", i)
		ids = append(ids, id)
		plan[id] = fakeRun{end: fleetUpdDone, polls: 1 + i%3}
	}
	r, f := rolloutRig(t, 3, plan)
	if err := r.Start(nil, rolloutMachines(ids...), fleetUpdateOpts{version: "v2.0.0"}, 3); err != nil {
		t.Fatal(err)
	}
	s := waitRollout(t, r)
	want := "m0=done m1=done m2=done m3=done m4=done m5=done m6=done"
	if got := statuses(s); got != want || s.Stopped != "" || s.Limit != 3 || s.Version != "v2.0.0" || s.Started.IsZero() {
		t.Errorf("got %s, stopped %q, limit %d, version %q", got, s.Stopped, s.Limit, s.Version)
	}
	if s.Machines[2].Name != "name-m2" || s.Machines[2].Reason != "m2 ended done" {
		t.Errorf("machine 2: %+v", s.Machines[2])
	}
	if got := strings.Join(f.started(), " "); got != strings.Join(ids, " ") {
		t.Errorf("started in the order %s", got)
	}
	if f.most != 3 {
		t.Errorf("at most %d at once, want 3", f.most)
	}
}

func TestRolloutStopsAtFirstRollback(t *testing.T) {
	r, f := rolloutRig(t, 1, map[string]fakeRun{
		"a": {end: fleetUpdDone}, "b": {end: fleetUpdRolledBack, polls: 2}, "c": {end: fleetUpdDone}, "d": {end: fleetUpdDone}})
	if err := r.Start(nil, rolloutMachines("a", "b", "c", "d"), fleetUpdateOpts{}, 0); err != nil {
		t.Fatal(err)
	}
	s := waitRollout(t, r)
	if got := statuses(s); got != "a=done b=rolled_back c=not_started d=not_started" || s.Stopped != "stopped at name-b" || s.Limit != 1 {
		t.Errorf("got %s, stopped %q, limit %d", got, s.Stopped, s.Limit)
	}
	if s.Machines[1].Reason != "b ended rolled_back" || s.Machines[2].Reason != "not started: the rollout was stopped at name-b" {
		t.Errorf("reasons %q, %q", s.Machines[1].Reason, s.Machines[2].Reason)
	}
	if got := strings.Join(f.started(), " "); got != "a b" {
		t.Errorf("started %s", got)
	}
}

// With a limit of 2 the failing machine's sibling is in flight: it finishes
// and is reported, and nothing more starts.
func TestRolloutStopsAtFirstFailureAndSiblingFinishes(t *testing.T) {
	r, f := rolloutRig(t, 2, map[string]fakeRun{
		"a": {end: fleetUpdFailed}, "b": {end: fleetUpdDone, polls: 5}, "c": {end: fleetUpdDone}})
	if err := r.Start(nil, rolloutMachines("a", "b", "c"), fleetUpdateOpts{}, 2); err != nil {
		t.Fatal(err)
	}
	s := waitRollout(t, r)
	if got := statuses(s); got != "a=failed b=done c=not_started" || s.Stopped != "stopped at name-a" {
		t.Errorf("got %s, stopped %q", got, s.Stopped)
	}
	if got := strings.Join(f.started(), " "); got != "a b" {
		t.Errorf("started %s", got)
	}
}

func TestRolloutSkipsRefusedAndGoesOn(t *testing.T) {
	r, _ := rolloutRig(t, 1, map[string]fakeRun{
		"a": {refuse: "a is offline"}, "b": {end: fleetUpdRefused, polls: 1}, "c": {end: fleetUpdDone}})
	if err := r.Start(nil, rolloutMachines("a", "b", "c"), fleetUpdateOpts{}, 1); err != nil {
		t.Fatal(err)
	}
	s := waitRollout(t, r)
	if got := statuses(s); got != "a=skipped b=skipped c=done" || s.Stopped != "" {
		t.Errorf("got %s, stopped %q", got, s.Stopped)
	}
	if s.Machines[0].Reason != "a is offline" || s.Machines[1].Reason != "b ended refused" {
		t.Errorf("reasons %q, %q", s.Machines[0].Reason, s.Machines[1].Reason)
	}
}

// A machine whose update state is gone ends the rollout as a failure rather
// than leaving it waiting forever.
func TestRolloutEndsWhenAnUpdateStateVanishes(t *testing.T) {
	r, _ := rolloutRig(t, 1, map[string]fakeRun{"a": {end: "vanish"}, "b": {end: fleetUpdDone}})
	if err := r.Start(nil, rolloutMachines("a", "b"), fleetUpdateOpts{}, 1); err != nil {
		t.Fatal(err)
	}
	if s := waitRollout(t, r); statuses(s) != "a=failed b=not_started" || s.Stopped != "stopped at name-a" {
		t.Errorf("got %s, stopped %q", statuses(s), s.Stopped)
	}
}

func TestRolloutStopAndSecondStart(t *testing.T) {
	r, f := rolloutRig(t, 1, map[string]fakeRun{"a": {end: fleetUpdDone, hold: true}, "b": {end: fleetUpdDone}, "c": {end: fleetUpdDone}})
	if r.Stop() {
		t.Error("Stop with no rollout said it stopped one")
	}
	if err := r.Start(nil, rolloutMachines("a", "b", "c"), fleetUpdateOpts{}, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(nil, rolloutMachines("c"), fleetUpdateOpts{}, 1); err == nil || !strings.Contains(err.Error(), "running already") {
		t.Errorf("a second rollout: %v", err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		s, _ := r.State()
		if statuses(s) == "a=running b=waiting c=waiting" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a never ran on its own: %s", statuses(s))
		}
	}
	if !r.Stop() || !r.Stop() {
		t.Error("Stop on a running rollout said none ran")
	}
	if s, _ := r.State(); s.Over() || s.Stopped != "stopped by the operator" {
		t.Errorf("stopped with a in flight: over %v, %q", s.Over(), s.Stopped)
	}
	f.release("a")
	s := waitRollout(t, r)
	if got := statuses(s); got != "a=done b=not_started c=not_started" || s.Machines[1].Reason != "not started: the rollout was stopped by the operator" {
		t.Errorf("got %s, %q", got, s.Machines[1].Reason)
	}
	if r.Stop() {
		t.Error("Stop on an ended rollout said it stopped one")
	}
	// The ended one stays readable until the next starts.
	if s2, ok := r.State(); !ok || statuses(s2) != statuses(s) {
		t.Errorf("after the end: %v %s", ok, statuses(s2))
	}
	if err := r.Start(nil, rolloutMachines("c"), fleetUpdateOpts{}, 1); err != nil {
		t.Fatal(err)
	}
	if s := waitRollout(t, r); statuses(s) != "c=done" || s.Stopped != "" {
		t.Errorf("the next rollout: %s %q", statuses(s), s.Stopped)
	}
}

func TestRolloutRefusesBadInput(t *testing.T) {
	r, f := rolloutRig(t, 8, map[string]fakeRun{"a": {end: fleetUpdDone}})
	for _, c := range []struct {
		ms    []fleetRolloutMachine
		limit int
		want  string
	}{
		{nil, 1, "at least one machine"},
		{rolloutMachines("a"), 9, "1 to 8"},
		{rolloutMachines("a"), -1, "1 to 8"},
		{rolloutMachines("a", "a"), 1, "named twice"},
	} {
		if err := r.Start(nil, c.ms, fleetUpdateOpts{}, c.limit); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%d machines, limit %d: %v", len(c.ms), c.limit, err)
		}
	}
	if _, ok := r.State(); ok || len(f.started()) != 0 {
		t.Error("a refused rollout left a state or started a machine")
	}
	if err := r.Start(nil, rolloutMachines("a"), fleetUpdateOpts{}, 8); err != nil {
		t.Fatal(err)
	}
	if s := waitRollout(t, r); s.Limit != 8 || statuses(s) != "a=done" {
		t.Errorf("limit 8: %d %s", s.Limit, statuses(s))
	}
}

func TestRolloutStateIsACopy(t *testing.T) {
	r, _ := rolloutRig(t, 1, map[string]fakeRun{"a": {end: fleetUpdDone}})
	if err := r.Start(nil, rolloutMachines("a"), fleetUpdateOpts{}, 1); err != nil {
		t.Fatal(err)
	}
	s := waitRollout(t, r)
	s.Machines[0].Status, s.Machines[0].Reason, s.Stopped = "x", "y", "z"
	if again, _ := r.State(); again.Machines[0].Status != fleetUpdDone || again.Machines[0].Reason != "a ended done" || again.Stopped != "" {
		t.Errorf("the copy reached the rollout: %+v", again)
	}
}

// The rollout is handed to keep after each change, and the Tenant that starts
// next closes one it finds in the middle where it stood: nothing is started
// from it, and it says which machines were done, which were being updated
// and which never started.
func TestRolloutKeptAndRestored(t *testing.T) {
	r, f := rolloutRig(t, 1, map[string]fakeRun{"a": {end: fleetUpdDone}, "b": {end: fleetUpdDone, hold: true}, "c": {end: fleetUpdDone}})
	var mu sync.Mutex
	var kept []fleetRolloutState
	r.keep = func(s fleetRolloutState) {
		mu.Lock()
		defer mu.Unlock()
		kept = append(kept, s)
	}
	last := func() fleetRolloutState {
		mu.Lock()
		defer mu.Unlock()
		if len(kept) == 0 {
			return fleetRolloutState{}
		}
		return kept[len(kept)-1]
	}
	if err := r.Start(nil, rolloutMachines("a", "b", "c"), fleetUpdateOpts{version: "v2.0.0"}, 1); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); statuses(last()) != "a=done b=running c=waiting"; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("what was kept never said a done and b running: %s", statuses(last()))
		}
	}
	mid := last()
	if s, _ := r.State(); statuses(s) != statuses(mid) || mid.Over() || mid.Version != "v2.0.0" || mid.Limit != 1 {
		t.Fatalf("kept %+v, the rollout is at %s", mid, statuses(s))
	}

	// The Tenant that starts next, a minute after that was saved.
	at := time.Now().Add(-time.Minute)
	started := len(f.started())
	next := &fleetRollout{updates: f}
	var closed []fleetRolloutState
	next.keep = func(s fleetRolloutState) { closed = append(closed, s) }
	if !next.restore(mid, at) {
		t.Error("a rollout with a machine running and one waiting was not reported as cut short")
	}
	s, ok := next.State()
	if !ok || !s.Over() || !s.Finished.Equal(at) || s.Stopped != "cut short when this Tenant stopped" || statuses(s) != "a=done b=interrupted c=not_started" {
		t.Fatalf("restored: %s, stopped %q, over %v, ended %s", statuses(s), s.Stopped, s.Over(), s.Finished)
	}
	if s.Machines[0].Reason != "a ended done" || s.Machines[1].Reason != "its update was under way when this Tenant stopped" ||
		s.Machines[2].Reason != "not started: the rollout was cut short when this Tenant stopped" {
		t.Errorf("reasons: %q", s.Machines)
	}
	if len(closed) != 1 || statuses(closed[0]) != statuses(s) || !closed[0].Over() {
		t.Errorf("the closed rollout was not kept: %+v", closed)
	}
	if next.Stop() || len(f.started()) != started {
		t.Error("a restored rollout is running, or started a machine")
	}
	if got := rolloutCutWords(s); !strings.Contains(got, "Of its 3 machines, 1 had ended, 1 never started, being updated: name-b.") {
		t.Errorf("the operator is told: %s", got)
	}
	// What this process has is not replaced by a second read.
	if next.restore(fleetRolloutState{Started: time.Now(), Machines: rolloutMachines("z")}, at) {
		t.Error("a second restore says it took a rollout that was cut short")
	}
	if again, _ := next.State(); statuses(again) != statuses(s) {
		t.Errorf("a second restore replaced the rollout: %s", statuses(again))
	}

	// One the operator had stopped, with a machine still in flight, keeps its
	// reason; a file with no time of its own ends now.
	stopped := mid
	stopped.Machines = append([]fleetRolloutMachine(nil), mid.Machines...)
	stopped.Stopped = "stopped by the operator"
	byHand := &fleetRollout{updates: f}
	byHand.restore(stopped, time.Time{})
	if s, _ := byHand.State(); s.Stopped != "stopped by the operator" || statuses(s) != "a=done b=interrupted c=not_started" ||
		s.Machines[2].Reason != "not started: the rollout was stopped by the operator" || time.Since(s.Finished) > time.Minute {
		t.Errorf("a stopped rollout, restored: %+v", s)
	}
	if got := rolloutCutWords(fleetRolloutState{Machines: []fleetRolloutMachine{{ID: "a", Status: fleetUpdDone}, {ID: "b", Status: fleetRollNotStarted}}}); !strings.Contains(got, "1 had ended, 1 never started, none was being updated.") {
		t.Errorf("with none in flight: %s", got)
	}

	// One that had ended reads back as it ended, and a new one starts after it.
	f.release("b")
	done := waitRollout(t, r)
	if statuses(last()) != "a=done b=done c=done" || !last().Finished.Equal(done.Finished) {
		t.Errorf("the end was not kept: %s", statuses(last()))
	}
	ended := &fleetRollout{updates: f}
	if cut := ended.restore(done, at); cut {
		t.Error("a rollout that had ended was reported as cut short")
	}
	if s, _ := ended.State(); statuses(s) != "a=done b=done c=done" || s.Stopped != "" || !s.Finished.Equal(done.Finished) {
		t.Errorf("an ended rollout, restored: %+v", s)
	}
	// Every machine had ended and only the rollout's own end was not written
	// yet: it ended, and was not cut short.
	almost := done
	almost.Finished = time.Time{}
	last1 := &fleetRollout{updates: f}
	if cut := last1.restore(almost, at); cut {
		t.Error("a rollout whose machines had all ended was reported as cut short")
	}
	if s, _ := last1.State(); !s.Over() || s.Stopped != "" || !s.Finished.Equal(at) {
		t.Errorf("a rollout whose machines had all ended, restored: %+v", s)
	}
	if err := next.Start(nil, rolloutMachines("c"), fleetUpdateOpts{}, 1); err != nil {
		t.Errorf("a new rollout after a restored one: %v", err)
	}
	waitRollout(t, next)
}
