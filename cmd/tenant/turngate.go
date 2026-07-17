package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"tenant/internal/agent"
)

// turnRunner is the slice of *agent.Agent that drives a conversation turn.
// *agent.Agent and *turnGate both satisfy the dashboard and relay consumers.
type turnRunner interface {
	Turn(ctx context.Context, req agent.TurnRequest) (*agent.TurnResult, error)
	Interject(msg string)
}

var (
	// ErrTurnQueueFull reports that the bounded shared-turn ingress queue has
	// no room for another unique request. Duplicate requests can still join an
	// existing entry because they consume no additional queue slot.
	ErrTurnQueueFull = errors.New("turn scheduler queue full")
	// ErrTurnSourceQuota reports that one source has consumed its configured
	// share of the queue even though another source may still have capacity.
	ErrTurnSourceQuota = errors.New("turn scheduler source quota exceeded")
)

const (
	defaultTurnQueueDepth = 32
	initialTurnEstimate   = 30 * time.Second
)

var defaultTurnSourceQuotas = map[agent.RouteSurface]int{
	agent.RouteSurfaceTUI:       16,
	agent.RouteSurfaceDashboard: 16,
	agent.RouteSurfaceRelay:     8,
	agent.RouteSurfacePeer:      8,
	agent.RouteSurfaceCron:      4,
	agent.RouteSurfaceResearch:  4,
	"background":                4,
}

type turnPriority uint8

const (
	turnPriorityInteractive turnPriority = iota
	turnPriorityRelay
	turnPriorityBackground
)

type resolvedTurnSchedulerConfig struct {
	depth  int
	quotas map[agent.RouteSurface]int
}

func resolveTurnSchedulerConfig(cfg turnSchedulerConfig) resolvedTurnSchedulerConfig {
	depth := cfg.QueueDepth
	if depth <= 0 {
		depth = defaultTurnQueueDepth
	}
	quotas := make(map[agent.RouteSurface]int, len(defaultTurnSourceQuotas)+len(cfg.SourceQuotas))
	for source, quota := range defaultTurnSourceQuotas {
		quotas[source] = min(quota, depth)
	}
	for rawSource, quota := range cfg.SourceQuotas {
		source := canonicalTurnSource(agent.RouteSurface(rawSource))
		if quota > 0 {
			quotas[source] = min(quota, depth)
		}
	}
	return resolvedTurnSchedulerConfig{depth: depth, quotas: quotas}
}

type turnOutcome struct {
	result *agent.TurnResult
	err    error
}

type turnWaiter struct {
	done chan turnOutcome
}

type scheduledTurn struct {
	req       agent.TurnRequest
	source    agent.RouteSurface
	priority  turnPriority
	key       string
	enqueued  time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	waiters   map[*turnWaiter]struct{}
	started   bool
	completed bool
}

// turnGate is a bounded ingress scheduler in front of one shared agent. It
// chooses WHO gets the agent next but never changes the capacity of the inner
// turn runner: exactly one call to inner.Turn may run at a time.
//
// Priority is strict across three source classes (interactive TUI/dashboard,
// relay/peer, then cron/research/background) and FIFO within a class. Per-
// source queued-entry quotas stop one surface consuming the entire bounded
// queue. Identical requests from the same source coalesce while queued or
// active and fan one result out to every live waiter.
type turnGate struct {
	inner turnRunner
	cfg   resolvedTurnSchedulerConfig

	mu          sync.Mutex
	queue       []*scheduledTurn
	queuedBySrc map[agent.RouteSurface]int
	byKey       map[string]*scheduledTurn
	active      *scheduledTurn
	activeSince time.Time
	avgTurn     time.Duration
}

func newTurnGate(inner turnRunner) *turnGate {
	return newTurnGateConfigured(inner, turnSchedulerConfig{})
}

func newTurnGateConfigured(inner turnRunner, cfg turnSchedulerConfig) *turnGate {
	return &turnGate{
		inner:       inner,
		cfg:         resolveTurnSchedulerConfig(cfg),
		queuedBySrc: make(map[agent.RouteSurface]int),
		byKey:       make(map[string]*scheduledTurn),
		avgTurn:     initialTurnEstimate,
	}
}

// Turn joins a duplicate request or admits a unique request to the bounded
// queue, then waits for its result. A canceled waiter is removed immediately;
// the shared queued/running request is canceled only after its last waiter
// leaves.
func (g *turnGate) Turn(ctx context.Context, req agent.TurnRequest) (*agent.TurnResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source := canonicalTurnSource(req.RouteSurface)
	key := turnDedupeKey(source, req)
	w := &turnWaiter{done: make(chan turnOutcome, 1)}

	g.mu.Lock()
	entry := g.byKey[key]
	if entry != nil {
		entry.waiters[w] = struct{}{}
		g.mu.Unlock()
	} else {
		if len(g.queue) >= g.cfg.depth {
			g.mu.Unlock()
			return nil, fmt.Errorf("%w (depth %d)", ErrTurnQueueFull, g.cfg.depth)
		}
		quota := g.sourceQuota(source)
		if g.queuedBySrc[source] >= quota {
			g.mu.Unlock()
			return nil, fmt.Errorf("%w for %q (quota %d)", ErrTurnSourceQuota, source, quota)
		}
		sharedCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		entry = &scheduledTurn{
			req: req, source: source, priority: priorityForTurnSource(source),
			key: key, enqueued: time.Now(), ctx: sharedCtx, cancel: cancel,
			waiters: map[*turnWaiter]struct{}{w: {}},
		}
		g.queue = append(g.queue, entry)
		g.queuedBySrc[source]++
		g.byKey[key] = entry
		g.dispatchLocked()
		g.mu.Unlock()
	}

	select {
	case out := <-w.done:
		return out.result, out.err
	case <-ctx.Done():
		g.cancelWaiter(entry, w)
		return nil, ctx.Err()
	}
}

func (g *turnGate) sourceQuota(source agent.RouteSurface) int {
	if quota := g.cfg.quotas[source]; quota > 0 {
		return quota
	}
	return g.cfg.depth
}

func (g *turnGate) cancelWaiter(entry *scheduledTurn, w *turnWaiter) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if entry.completed {
		return
	}
	if _, ok := entry.waiters[w]; !ok {
		return
	}
	delete(entry.waiters, w)
	if len(entry.waiters) != 0 {
		return
	}
	entry.cancel()
	if entry.started {
		return
	}
	g.removeQueuedLocked(entry)
	delete(g.byKey, entry.key)
}

func (g *turnGate) dispatchLocked() {
	if g.active != nil || len(g.queue) == 0 {
		return
	}
	best := 0
	for i := 1; i < len(g.queue); i++ {
		if g.queue[i].priority < g.queue[best].priority {
			best = i
		}
	}
	entry := g.queue[best]
	g.queue = append(g.queue[:best], g.queue[best+1:]...)
	g.queuedBySrc[entry.source]--
	entry.started = true
	g.active = entry
	g.activeSince = time.Now()
	go g.run(entry)
}

func (g *turnGate) run(entry *scheduledTurn) {
	started := time.Now()
	result, err := g.inner.Turn(entry.ctx, entry.req)
	duration := time.Since(started)

	g.mu.Lock()
	entry.completed = true
	entry.cancel()
	if current := g.byKey[entry.key]; current == entry {
		delete(g.byKey, entry.key)
	}
	if g.active == entry {
		g.active = nil
		g.activeSince = time.Time{}
	}
	if duration > 0 {
		g.avgTurn = (g.avgTurn*3 + duration) / 4
	}
	waiters := make([]*turnWaiter, 0, len(entry.waiters))
	for waiter := range entry.waiters {
		waiters = append(waiters, waiter)
	}
	entry.waiters = nil
	g.dispatchLocked()
	g.mu.Unlock()

	out := turnOutcome{result: result, err: err}
	for _, waiter := range waiters {
		waiter.done <- out
	}
}

func (g *turnGate) removeQueuedLocked(target *scheduledTurn) {
	for i, entry := range g.queue {
		if entry != target {
			continue
		}
		g.queue = append(g.queue[:i], g.queue[i+1:]...)
		g.queuedBySrc[target.source]--
		return
	}
}

// Interject forwards a mid-turn message straight to the agent. The agent's
// inject mutex makes this safe and interjection must not wait behind ingress.
func (g *turnGate) Interject(msg string) { g.inner.Interject(msg) }

// ActiveTurn is the serve-liveness reporter consumed by dashboard status.
func (g *turnGate) ActiveTurn() (bool, time.Duration) { return g.status() }

func (g *turnGate) status() (active bool, age time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active == nil {
		return false, 0
	}
	return true, time.Since(g.activeSince)
}

// TurnQueueStatus returns queue depth, age, dispatch position, and estimated
// wait without exposing request content. It is consumed by /api/status.
func (g *turnGate) TurnQueueStatus() agent.TurnQueueStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	ordered := make([]*scheduledTurn, 0, len(g.queue))
	// Build the same stable order dispatchLocked produces: strict class
	// priority and original append order within each class.
	for priority := turnPriorityInteractive; priority <= turnPriorityBackground; priority++ {
		for _, entry := range g.queue {
			if entry.priority == priority {
				ordered = append(ordered, entry)
			}
		}
	}
	status := agent.TurnQueueStatus{Depth: len(ordered)}
	if len(ordered) == 0 {
		return status
	}
	oldest := ordered[0].enqueued
	remaining := time.Duration(0)
	if g.active != nil {
		remaining = max(g.avgTurn-now.Sub(g.activeSince), 0)
	}
	status.Entries = make([]agent.TurnQueueEntry, 0, len(ordered))
	for i, entry := range ordered {
		if entry.enqueued.Before(oldest) {
			oldest = entry.enqueued
		}
		status.Entries = append(status.Entries, agent.TurnQueueEntry{
			Position: i + 1,
			Source:   entry.source,
			Age:      now.Sub(entry.enqueued),
			ETA:      remaining + time.Duration(i)*g.avgTurn,
		})
	}
	status.OldestAge = now.Sub(oldest)
	return status
}

func canonicalTurnSource(source agent.RouteSurface) agent.RouteSurface {
	source = agent.RouteSurface(strings.ToLower(strings.TrimSpace(string(source))))
	if source == "" {
		return "background"
	}
	return source
}

func priorityForTurnSource(source agent.RouteSurface) turnPriority {
	switch source {
	case agent.RouteSurfaceTUI, agent.RouteSurfaceDashboard:
		return turnPriorityInteractive
	case agent.RouteSurfaceRelay, agent.RouteSurfacePeer:
		return turnPriorityRelay
	default:
		return turnPriorityBackground
	}
}

func turnDedupeKey(source agent.RouteSurface, req agent.TurnRequest) string {
	// Length prefixes avoid collisions between adjacent variable-width fields.
	parts := []string{
		string(source), req.UserQuery, string(req.RouteSurface),
		string(req.RouteOverride), strconv.Itoa(req.LoopCeiling),
	}
	var b strings.Builder
	for _, part := range parts {
		fmt.Fprintf(&b, "%d:%s|", len(part), part)
	}
	return b.String()
}
