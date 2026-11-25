package main

// fleetrollout.go (TEN-546, section 2.5): a rollout, the staggered update of
// many machines. It runs in the process that holds the fleet, beside
// fleetUpdates, and updates each machine through the same fleetUpdates.Start
// a single update uses: up to limit at once, the next one started each time
// one ends.
//
// A machine that ends done is updated. One that is refused, at the start or
// once its update looked at it, is skipped with its reason and the rollout
// goes on. The first that ends rolled back or failed stops the rollout: no
// further machine is started, those in flight finish, and those never started
// are reported as not started.
//
// A rollout lives in memory, like fleetUpdates' state: a restart of the
// master forgets it, and the machines' own updates with it.

import (
	"errors"
	"slices"
	"sync"
	"time"

	"tenant/internal/fleet"
)

// What a machine in a rollout is at. done, rolled_back and failed are the
// update's own stages.
const (
	fleetRollWaiting    = "waiting"
	fleetRollRunning    = "running"
	fleetRollSkipped    = "skipped"
	fleetRollNotStarted = "not_started"
)

// fleetRolloutLimit is the most machines a rollout updates at once.
const fleetRolloutLimit = 8

// fleetRolloutMachine is one machine of a rollout. ID and Name are what the
// caller gave; Status is one of waiting, running, done, skipped, rolled_back,
// failed, not_started; Reason is the line that says why, where there is one.
// A running machine's stage is its update's (fleetUpdates.State).
type fleetRolloutMachine struct {
	ID, Name string
	Status   string
	Reason   string
}

// fleetRolloutState is a rollout, as far as it has got. Stopped is "" while
// it runs on its own and once it ended on its own, "stopped by the operator",
// or "stopped at <name>" after a machine rolled back or failed.
type fleetRolloutState struct {
	Started, Finished time.Time
	Limit             int
	Version           string // as asked for: "" is the latest release
	Stopped           string
	Machines          []fleetRolloutMachine // in the order given
}

// Over reports whether the rollout has ended.
func (s fleetRolloutState) Over() bool { return !s.Finished.IsZero() }

// fleetRollout is this process's rollout: one at a time, the last one kept
// until the next starts.
type fleetRollout struct {
	updates interface { // *fleetUpdates; tests fake it
		Start(mgr *fleet.Manager, machine string, o fleetUpdateOpts) error
		State(machine string) (fleetUpdateState, bool)
	}

	mu   sync.Mutex
	cur  *fleetRolloutState
	stop chan struct{} // closed by Stop
}

func newFleetRollout(u *fleetUpdates) *fleetRollout { return &fleetRollout{updates: u} }

// State is a copy of the current or last rollout; ok is false when there was
// none.
func (r *fleetRollout) State() (fleetRolloutState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return fleetRolloutState{}, false
	}
	out := *r.cur
	out.Machines = slices.Clone(r.cur.Machines)
	return out, true
}

// Start begins a rollout of machines, in their order, limit at a time (0 is
// 1), or says at once why it will not.
func (r *fleetRollout) Start(mgr *fleet.Manager, machines []fleetRolloutMachine, o fleetUpdateOpts, limit int) error {
	switch {
	case len(machines) == 0:
		return errors.New("a rollout needs at least one machine")
	case limit < 0 || limit > fleetRolloutLimit:
		return errors.New("a rollout updates 1 to 8 machines at once")
	case limit == 0:
		limit = 1
	}
	ms := make([]fleetRolloutMachine, len(machines))
	seen := map[string]bool{}
	for i, m := range machines {
		if seen[m.ID] {
			return errors.New(fleetName(m.Name) + " is named twice in the rollout")
		}
		seen[m.ID] = true
		ms[i] = fleetRolloutMachine{ID: m.ID, Name: m.Name, Status: fleetRollWaiting}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil && !r.cur.Over() {
		return errors.New("a rollout is running already: stop it, or wait for it to end")
	}
	r.cur = &fleetRolloutState{Started: time.Now(), Limit: limit, Version: o.version, Machines: ms}
	r.stop = make(chan struct{})
	go r.run(mgr, r.cur, o, r.stop, fleetUpdatePoll)
	return nil
}

// Stop asks the running rollout to start no more machines; those in flight
// finish. It reports whether a rollout was running.
func (r *fleetRollout) Stop() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil || r.cur.Over() {
		return false
	}
	if r.cur.Stopped == "" {
		r.cur.Stopped = "stopped by the operator"
		close(r.stop)
	}
	return true
}

// set records machine i's status, and a rollback or a failure stops the
// rollout there.
func (r *fleetRollout) set(s *fleetRolloutState, i int, status, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s.Machines[i].Status, s.Machines[i].Reason = status, reason
	if (status == fleetUpdRolledBack || status == fleetUpdFailed) && s.Stopped == "" {
		s.Stopped = "stopped at " + fleetName(s.Machines[i].Name)
	}
}

func (r *fleetRollout) stopped(s *fleetRolloutState) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return s.Stopped
}

// run is the rollout, after Start. Only it writes the machines' statuses, so
// it reads them without the lock. It ends once no machine is in flight and no
// more will be started.
func (r *fleetRollout) run(mgr *fleet.Manager, s *fleetRolloutState, o fleetUpdateOpts, stop <-chan struct{}, poll time.Duration) {
	next, inFlight := 0, 0
	for {
		for i := range s.Machines {
			if s.Machines[i].Status != fleetRollRunning {
				continue
			}
			u, ok := r.updates.State(s.Machines[i].ID)
			switch {
			case !ok:
				r.set(s, i, fleetUpdFailed, "its update's progress is no longer held here: how it ended is not known")
			case !u.Over():
				continue
			case u.Stage == fleetUpdRefused:
				r.set(s, i, fleetRollSkipped, lastLine(u.Lines))
			case u.Stage == fleetUpdDone || u.Stage == fleetUpdRolledBack:
				r.set(s, i, u.Stage, lastLine(u.Lines))
			default: // failed, or a stage this does not know
				r.set(s, i, fleetUpdFailed, lastLine(u.Lines))
			}
			inFlight--
		}
		for inFlight < s.Limit && next < len(s.Machines) && r.stopped(s) == "" {
			if err := r.updates.Start(mgr, s.Machines[next].ID, o); err != nil {
				r.set(s, next, fleetRollSkipped, err.Error())
			} else {
				r.set(s, next, fleetRollRunning, "")
				inFlight++
			}
			next++
		}
		if inFlight == 0 && (next == len(s.Machines) || r.stopped(s) != "") {
			r.mu.Lock()
			for i := next; i < len(s.Machines); i++ {
				s.Machines[i].Status = fleetRollNotStarted
				s.Machines[i].Reason = "not started: the rollout was " + s.Stopped
			}
			s.Finished = time.Now()
			r.mu.Unlock()
			return
		}
		select {
		case <-time.After(poll):
		case <-stop:
			stop = nil // closed: wake once, then poll
		}
	}
}

// lastLine is what an update last said: why it ended.
func lastLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}
