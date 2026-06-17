package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/agent"
)

// gateFakeRunner records max concurrency seen inside Turn and lets a test hold
// a turn open until released, to prove the gate serializes.
type gateFakeRunner struct {
	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	turns       int32
	interjects  int32
	hold        chan struct{} // if non-nil, Turn blocks on it (simulate a long turn)
}

func (r *gateFakeRunner) Turn(ctx context.Context, _ agent.TurnRequest) (*agent.TurnResult, error) {
	r.mu.Lock()
	r.inFlight++
	if r.inFlight > r.maxInFlight {
		r.maxInFlight = r.inFlight
	}
	r.mu.Unlock()
	atomic.AddInt32(&r.turns, 1)

	if r.hold != nil {
		select {
		case <-r.hold:
		case <-ctx.Done():
		}
	}

	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
	return &agent.TurnResult{}, ctx.Err()
}

func (r *gateFakeRunner) Interject(string) { atomic.AddInt32(&r.interjects, 1) }

// TestTurnGate_Serializes: many concurrent Turn calls never overlap inside the
// inner runner (max in-flight == 1).
func TestTurnGate_Serializes(t *testing.T) {
	r := &gateFakeRunner{}
	g := newTurnGate(r)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = g.Turn(context.Background(), agent.TurnRequest{UserQuery: "x"})
		}()
	}
	wg.Wait()

	if r.maxInFlight != 1 {
		t.Fatalf("max concurrent turns = %d, want 1 (gate must serialize)", r.maxInFlight)
	}
	if got := atomic.LoadInt32(&r.turns); got != 20 {
		t.Fatalf("ran %d turns, want 20", got)
	}
}

// TestTurnGate_QueuedTurnRespectsCancel: a turn queued behind a held one is
// released by its own cancelled ctx (graceful-shutdown invariant) — it does
// NOT block forever and never reaches the inner runner.
func TestTurnGate_QueuedTurnRespectsCancel(t *testing.T) {
	r := &gateFakeRunner{hold: make(chan struct{})}
	g := newTurnGate(r)

	// Occupy the gate with a long-running turn.
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = g.Turn(context.Background(), agent.TurnRequest{UserQuery: "long"})
	}()
	<-started
	// Wait until the held turn is actually inside the inner runner.
	waitFor(t, func() bool { active, _ := g.status(); return active })

	// A second caller whose ctx cancels while queued must return promptly.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := g.Turn(ctx, agent.TurnRequest{UserQuery: "queued"})
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("queued+cancelled Turn should return ctx.Err(), got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued Turn did not unblock on ctx cancel (it blocked on the gate)")
	}

	// Only the held turn ever entered the inner runner.
	if got := atomic.LoadInt32(&r.turns); got != 1 {
		t.Fatalf("inner runner saw %d turns, want 1 (the queued one must not run)", got)
	}
	close(r.hold) // let the held turn finish
}

// TestTurnGate_InterjectNotGated: Interject passes through even while a turn
// holds the gate (fold-into-running-turn semantics).
func TestTurnGate_InterjectNotGated(t *testing.T) {
	r := &gateFakeRunner{hold: make(chan struct{})}
	g := newTurnGate(r)

	go func() { _, _ = g.Turn(context.Background(), agent.TurnRequest{UserQuery: "long"}) }()
	waitFor(t, func() bool { active, _ := g.status(); return active })

	g.Interject("hello") // must not block on the held gate
	if got := atomic.LoadInt32(&r.interjects); got != 1 {
		t.Fatalf("interjects = %d, want 1", got)
	}
	close(r.hold)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
