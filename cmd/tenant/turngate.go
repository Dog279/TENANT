package main

import (
	"context"
	"sync"
	"time"

	"tenant/internal/agent"
)

// turnRunner is the slice of *agent.Agent that drives a conversation turn —
// the intersection of dashboard.AgentRunner and relayRunner. *agent.Agent
// satisfies it; *turnGate wraps one and itself satisfies both consumer
// interfaces (it has the same method set).
type turnRunner interface {
	Turn(ctx context.Context, req agent.TurnRequest) (*agent.TurnResult, error)
	Interject(msg string)
}

// turnGate serializes Turn execution on a SINGLE shared agent across every
// driver that can start one — the dashboard coordinator, the comms relays, and
// (later) a peer-delegated run. agent.Turn has no internal lock (agent.go:279):
// in the interactive TUI the operator serializes by convention (the !busy
// guard) and the dashboard's wsCoordinator only single-flights ITS OWN turns,
// so a relay-driven turn can still race a dashboard-driven turn on the shared
// working set. In a 24/7 headless hub that race is no longer hypothetical —
// unattended sources fire whenever traffic arrives — so `tenant serve` routes
// every driver through ONE gate.
//
// The gate is a capacity-1 semaphore rather than a plain mutex so a turn that
// is QUEUED behind a long-running one still honors its context: during a
// graceful shutdown the queued caller's cancelled ctx releases it instead of
// blocking on a lock that ignores cancellation. Interject is NOT gated — it is
// injectMu-guarded inside the agent and is a no-op when idle, so it may be
// called concurrently with an in-flight turn (that is the whole point: fold a
// new message into the running turn).
type turnGate struct {
	inner turnRunner
	sem   chan struct{} // capacity 1 — holding the slot == owning the turn

	mu          sync.Mutex
	active      bool
	activeSince time.Time
}

func newTurnGate(inner turnRunner) *turnGate {
	return &turnGate{inner: inner, sem: make(chan struct{}, 1)}
}

// Turn acquires the single turn slot (or returns ctx.Err() if the caller is
// cancelled while waiting), runs the inner turn, then releases the slot.
func (g *turnGate) Turn(ctx context.Context, req agent.TurnRequest) (*agent.TurnResult, error) {
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-g.sem }()

	g.setActive(true)
	defer g.setActive(false)
	return g.inner.Turn(ctx, req)
}

// Interject forwards a mid-turn message straight to the agent (no gating).
func (g *turnGate) Interject(msg string) { g.inner.Interject(msg) }

func (g *turnGate) setActive(on bool) {
	g.mu.Lock()
	g.active = on
	if on {
		g.activeSince = time.Now()
	} else {
		g.activeSince = time.Time{}
	}
	g.mu.Unlock()
}

// ActiveTurn is the exported liveness reporter the dashboard reads via
// duck-typing (dashboard.liveness) to populate /api/status. Mirrors status().
func (g *turnGate) ActiveTurn() (bool, time.Duration) { return g.status() }

// status reports whether a turn is currently running and, if so, how long it
// has been active — the headless liveness signal the dashboard /api/status and
// the doctor serve-mode check read to flag a stuck turn.
func (g *turnGate) status() (active bool, age time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.active {
		return false, 0
	}
	return true, time.Since(g.activeSince)
}
