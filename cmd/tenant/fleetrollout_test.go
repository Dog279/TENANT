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
