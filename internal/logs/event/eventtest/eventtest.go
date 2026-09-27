// Package eventtest captures the events a test writes.
package eventtest

import (
	"context"
	"slices"
	"sync"
	"testing"

	"tenant/internal/logs/event"
)

// Recorder is an event emitter that keeps what it is given, from any
// goroutine.
type Recorder struct {
	mu     sync.Mutex
	events []*event.Event
	fill   []func(context.Context, *event.Event)
}

// Capture makes a Recorder the event emitter until the test ends. Each fill
// runs on an event, with the context it was written under, before it is
// kept: to set what the log book takes from the context, such as the turn's
// trace (runtrace.IDFromContext). Not for parallel tests: the emitter is
// process-wide.
func Capture(t testing.TB, fill ...func(context.Context, *event.Event)) *Recorder {
	r := &Recorder{fill: fill}
	event.Install(r)
	t.Cleanup(func() { event.Install(nil) })
	return r
}

func (r *Recorder) Emit(ctx context.Context, e *event.Event) {
	for _, f := range r.fill {
		f(ctx, e)
	}
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *Recorder) Audit(ctx context.Context, e *event.Event) error { r.Emit(ctx, e); return nil }

// Events returns the events kept so far, in the order they were written.
func (r *Recorder) Events() []*event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}
