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
// is already closed. The shutdown and each stage are activities in the system
// log (events.go), and a stop that fails or panics is an event under its stage.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"tenant/internal/logs/event"
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
	done    chan struct{} // closed when the first Shutdown has run every stop
	log     *slog.Logger
}

// New returns an empty group. log gets a debug line for each stop that
// succeeds (nil: none); the rest is filed as events.
func New(log *slog.Logger) *Group { return &Group{log: log, done: make(chan struct{})} }

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
		g.run(context.Background(), stage, step{name, stop})
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
// it (for an early return) and call it at the end; a later call waits until
// the first has finished, so nobody proceeds while stores are still closing.
func (g *Group) Shutdown() (err error) {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		<-g.done
		return nil
	}
	g.stopped = true
	stages := g.stages
	g.stages = [numStages][]step{}
	g.mu.Unlock()
	defer close(g.done)

	n := 0
	for _, steps := range stages {
		n += len(steps)
	}
	ctx, act := event.Start(context.Background(), shutdownRun, "steps", n)
	defer act.End(&err)
	var errs []error
	for stage := Stage(0); stage < numStages; stage++ {
		if steps := stages[stage]; len(steps) > 0 {
			errs = append(errs, g.runStage(ctx, stage, steps))
		}
	}
	return errors.Join(errs...)
}

// runStage runs one stage's stops, last added first, as an activity.
func (g *Group) runStage(ctx context.Context, stage Stage, steps []step) (err error) {
	ctx, act := event.Start(ctx, stageRun, "stage", stage.String(), "steps", len(steps))
	defer act.End(&err)
	var errs []error
	for i := len(steps) - 1; i >= 0; i-- {
		errs = append(errs, g.run(ctx, stage, steps[i]))
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

func (g *Group) run(ctx context.Context, stage Stage, s step) (err error) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s/%s: panic: %v", stage, s.name, r)
			stepPanicked.Log(ctx, "stage", stage.String(), "step", s.name, "panic", r, "stack", string(debug.Stack()),
				"took", time.Since(start).Round(time.Millisecond))
		}
	}()
	e := s.stop()
	took := time.Since(start).Round(time.Millisecond)
	if e != nil {
		stepFailed.Log(ctx, "stage", stage.String(), "step", s.name, "err", e, "took", took)
		return fmt.Errorf("%s/%s: %w", stage, s.name, e)
	}
	if g.log != nil {
		g.log.Debug("shutdown step done", "stage", stage.String(), "step", s.name, "took", took)
	}
	return nil
}
