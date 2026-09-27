// Package lifecycle orders a long-running command's shutdown (TEN-314). A
// component registers how to stop it, and at which stage, where it is built;
// Shutdown then stops everything stage by stage:
//
//  1. Unblock: release work waiting on the operator (deny pending approvals),
//     so nothing in flight blocks the stages after it.
//  2. Intake: stop accepting new work (listeners, ingest, relays).
//  3. Drain: let in-flight work finish (schedulers and their running jobs).
//  4. Release: close what the work used (runtimes, stores), last registered
//     first, like deferred calls.
//
// So no goroutine starts a turn mid-teardown, and none writes to a store that
// is already closed.
package lifecycle

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Stage is when a stop runs during Shutdown.
type Stage int

const (
	Unblock Stage = iota
	Intake
	Drain
	Release
	numStages
)

func (s Stage) String() string {
	switch s {
	case Unblock:
		return "unblock"
	case Intake:
		return "intake"
	case Drain:
		return "drain"
	case Release:
		return "release"
	}
	return fmt.Sprintf("stage(%d)", int(s))
}

type step struct {
	name string
	stop func() error
}

// Group collects stops by stage. The zero value is not usable; call New.
type Group struct {
	mu      sync.Mutex
	stages  [numStages][]step
	stopped bool
	log     *slog.Logger
}

// New returns an empty group that logs each stop to log (nil: no logging).
func New(log *slog.Logger) *Group { return &Group{log: log} }

// Add registers stop to run at stage. Within a stage, stops run in reverse
// order of Add: a component added later may depend on one added earlier. A
// stop added after Shutdown began runs at once, so nothing started late is
// left running.
func (g *Group) Add(stage Stage, name string, stop func() error) {
	if stop == nil {
		return
	}
	if stage < 0 || stage >= numStages {
		panic(fmt.Sprintf("lifecycle: unknown stage %d for %q", int(stage), name))
	}
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		g.run(stage, step{name, stop})
		return
	}
	g.stages[stage] = append(g.stages[stage], step{name, stop})
	g.mu.Unlock()
}

// AddFunc is Add for a stop that cannot fail.
func (g *Group) AddFunc(stage Stage, name string, stop func()) {
	if stop == nil {
		return
	}
	g.Add(stage, name, func() error { stop(); return nil })
}

// Shutdown runs every registered stop, stage by stage, and returns their
// errors joined. A stop that panics is reported as an error and the rest
// still run. Only the first call does anything, so a command can both defer
// it (for an early return) and call it at the end.
func (g *Group) Shutdown() error {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return nil
	}
	g.stopped = true
	stages := g.stages
	g.stages = [numStages][]step{}
	g.mu.Unlock()

	var errs []error
	for stage := Stage(0); stage < numStages; stage++ {
		steps := stages[stage]
		for i := len(steps) - 1; i >= 0; i-- {
			if err := g.run(stage, steps[i]); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Steps lists the registered stops in the order Shutdown would run them, as
// "stage/name". For tests and diagnostics.
func (g *Group) Steps() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for stage := Stage(0); stage < numStages; stage++ {
		steps := g.stages[stage]
		for i := len(steps) - 1; i >= 0; i-- {
			out = append(out, stage.String()+"/"+steps[i].name)
		}
	}
	return out
}

func (g *Group) run(stage Stage, s step) (err error) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s/%s: panic: %v", stage, s.name, r)
		}
		if g.log != nil {
			if err != nil {
				g.log.Warn("shutdown step failed", "stage", stage.String(), "step", s.name, "err", err, "took", time.Since(start).Round(time.Millisecond))
			} else {
				g.log.Debug("shutdown step done", "stage", stage.String(), "step", s.name, "took", time.Since(start).Round(time.Millisecond))
			}
		}
	}()
	if e := s.stop(); e != nil {
		return fmt.Errorf("%s/%s: %w", stage, s.name, e)
	}
	return nil
}
