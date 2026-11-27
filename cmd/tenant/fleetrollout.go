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
// A rollout is saved in the data dir as it advances, with the machines' own
// updates (fleetUpdates.save), so a restart of the master does not forget it
// (TEN-552). A rollout the master was in the middle of is not run on by the
// Tenant that starts next: it is closed where it stood (restore), which says
// which machines were done, which were being updated and which never started.
// Going on by itself would mean updating machines on the word of a process
// that is gone, to a "latest" that may have moved; a new rollout over the
// same machines sends nothing to one that already runs the version.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"tenant/internal/fleet"
)

// What a machine in a rollout is at. done, rolled_back and failed are the
// update's own stages. interrupted is a machine whose update was under way
// when the Tenant that ran the rollout stopped: how it ended is its update's
// record to say (fleetUpdates.heard).
const (
	fleetRollWaiting     = "waiting"
	fleetRollRunning     = "running"
	fleetRollSkipped     = "skipped"
	fleetRollNotStarted  = "not_started"
	fleetRollInterrupted = "interrupted"
)

// fleetRolloutLimit is the most machines a rollout updates at once.
const fleetRolloutLimit = 8

// fleetRolloutCut is why a rollout stopped that a restart cut short.
const fleetRolloutCut = "cut short when this Tenant stopped"

// fleetRolloutMachine is one machine of a rollout. ID and Name are what the
// caller gave; Status is one of waiting, running, done, skipped, rolled_back,
// failed, not_started, interrupted; Reason is the line that says why, where
// there is one. A running machine's stage is its update's
// (fleetUpdates.State). The names are the saved file's.
type fleetRolloutMachine struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// fleetRolloutState is a rollout, as far as it has got. Stopped is "" while
// it runs on its own and once it ended on its own, "stopped by the operator",
// "stopped at <name>" after a machine rolled back or failed, "stopped when
// this Tenant no longer held the fleet's links", or "cut short when this
// Tenant stopped".
type fleetRolloutState struct {
	Started  time.Time             `json:"started"`
	Finished time.Time             `json:"finished,omitzero"`
	Limit    int                   `json:"limit"`
	Version  string                `json:"version,omitempty"` // as asked for: "" is the latest release
	Stopped  string                `json:"stopped,omitempty"`
	Machines []fleetRolloutMachine `json:"machines"` // in the order given
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

	// holds reports whether mgr is still the manager that holds the fleet's
	// links; nil (tests) is always. A rollout whose manager has gone starts
	// no more machines: their links are another manager's now, or nobody's.
	holds func(mgr *fleet.Manager) bool

	// keep, when set, is given the rollout after each change, to save it
	// (fleetUpdates.keepRollout). Tests leave it nil.
	keep func(fleetRolloutState)

	mu   sync.Mutex
	cur  *fleetRolloutState
	stop chan struct{} // closed by Stop
}

func newFleetRollout(u *fleetUpdates) *fleetRollout {
	return &fleetRollout{updates: u, keep: u.keepRollout, holds: func(mgr *fleet.Manager) bool { return fleetManager() == mgr }}
}

// saved hands the rollout as it stands to keep; r.mu is held.
func (r *fleetRollout) saved() {
	if r.keep != nil && r.cur != nil {
		out := *r.cur
		out.Machines = slices.Clone(r.cur.Machines)
		r.keep(out)
	}
}

// restore takes the rollout an earlier Tenant on this data dir saved, as the
// last one. One that Tenant was in the middle of is closed as of at, when it
// was last saved: a machine that was being updated is interrupted, one that
// was waiting never started, and nothing more is sent. cut reports that there
// was such a machine: the rollout did not get to its end. It does nothing
// while this process has a rollout running, or one that started no earlier
// than s: a second read (this process took the fleet again) brings in only a
// later rollout, one another Tenant ran in between.
func (r *fleetRollout) restore(s fleetRolloutState, at time.Time) (cut bool) {
	s.Machines = slices.Clone(s.Machines) // the caller's are not this rollout's to change
	if !s.Over() {
		cut = slices.ContainsFunc(s.Machines, func(m fleetRolloutMachine) bool {
			return m.Status == fleetRollRunning || m.Status == fleetRollWaiting
		})
		if cut && s.Stopped == "" {
			s.Stopped = fleetRolloutCut
		}
		for i := range s.Machines {
			switch m := &s.Machines[i]; m.Status {
			case fleetRollRunning:
				m.Status, m.Reason = fleetRollInterrupted, "its update was under way when this Tenant stopped"
			case fleetRollWaiting:
				m.Status, m.Reason = fleetRollNotStarted, "not started: the rollout was "+s.Stopped
			}
		}
		if s.Finished = at; at.IsZero() {
			s.Finished = time.Now()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil && (!r.cur.Over() || !s.Started.After(r.cur.Started)) {
		return false
	}
	r.cur = &s
	r.saved()
	return cut
}

// rolloutCutWords tells the operator where a rollout stood that a restart
// cut short.
func rolloutCutWords(s fleetRolloutState) string {
	var ended, never int
	var mid []string
	for _, m := range s.Machines {
		switch m.Status {
		case fleetRollInterrupted:
			mid = append(mid, fleetName(m.Name))
		case fleetRollNotStarted:
			never++
		default:
			ended++
		}
	}
	under := "none was being updated"
	if len(mid) > 0 {
		under = "being updated: " + strings.Join(mid, ", ")
	}
	return fmt.Sprintf("the rollout started %s was cut short when this Tenant stopped, and is not taken up again. Of its %d machines, %d had ended, %d never started, %s. The dashboard's Fleet page shows each one; a new rollout sends nothing to a machine that already runs the version",
		fleetTime(s.Started), len(s.Machines), ended, never, under)
}

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
	r.saved()
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
		r.saved()
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
	r.saved()
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
		if r.holds != nil && next < len(s.Machines) && r.stopped(s) == "" && !r.holds(mgr) {
			r.mu.Lock()
			s.Stopped = "stopped when this Tenant no longer held the fleet's links"
			r.saved()
			r.mu.Unlock()
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
			r.saved()
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
