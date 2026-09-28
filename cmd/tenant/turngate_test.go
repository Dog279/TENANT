package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/logs"
)

// gateFakeRunner records admission order and max concurrency. Individual
// queries can be held open to build a deterministic scheduler backlog.
type gateFakeRunner struct {
	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	turns       int32
	interjects  int32
	order       []string
	started     chan string
	blocks      map[string]chan struct{}
}

func newGateFakeRunner() *gateFakeRunner {
	return &gateFakeRunner{started: make(chan string, 128), blocks: make(map[string]chan struct{})}
}

func (r *gateFakeRunner) Turn(ctx context.Context, req agent.TurnRequest) (*agent.TurnResult, error) {
	r.mu.Lock()
	r.inFlight++
	if r.inFlight > r.maxInFlight {
		r.maxInFlight = r.inFlight
	}
	r.order = append(r.order, req.UserQuery)
	block := r.blocks[req.UserQuery]
	r.mu.Unlock()
	atomic.AddInt32(&r.turns, 1)
	r.started <- req.UserQuery

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}

	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
	return &agent.TurnResult{Response: req.UserQuery}, ctx.Err()
}

func (r *gateFakeRunner) Interject(string) { atomic.AddInt32(&r.interjects, 1) }

func (r *gateFakeRunner) block(query string) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan struct{})
	r.blocks[query] = ch
	return ch
}

type asyncTurn struct {
	result *agent.TurnResult
	err    error
}

func startGateTurn(g *turnGate, ctx context.Context, query string, source agent.RouteSurface) <-chan asyncTurn {
	done := make(chan asyncTurn, 1)
	go func() {
		result, err := g.Turn(ctx, agent.TurnRequest{UserQuery: query, RouteSurface: source})
		done <- asyncTurn{result: result, err: err}
	}()
	return done
}

func awaitStarted(t *testing.T, r *gateFakeRunner, want string) {
	t.Helper()
	select {
	case got := <-r.started:
		if got != want {
			t.Fatalf("started %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %q to start", want)
	}
}

func awaitTurn(t *testing.T, done <-chan asyncTurn) asyncTurn {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for turn")
		return asyncTurn{}
	}
}

func TestTurnGate_PriorityOrdering(t *testing.T) {
	r := newGateFakeRunner()
	release := r.block("active")
	g := newTurnGateConfigured(r, turnSchedulerConfig{QueueDepth: 8})

	active := startGateTurn(g, context.Background(), "active", agent.RouteSurfaceCron)
	awaitStarted(t, r, "active")
	if src := g.TurnQueueStatus().ActiveSource; src != "cron" {
		t.Errorf("ActiveSource = %q, want the running turn's source (cron)", src) // the dashboard strip names it
	}
	cron := startGateTurn(g, context.Background(), "cron", agent.RouteSurfaceCron)
	waitFor(t, func() bool { return g.TurnQueueStatus().Depth == 1 })
	relay := startGateTurn(g, context.Background(), "relay", agent.RouteSurfaceRelay)
	waitFor(t, func() bool { return g.TurnQueueStatus().Depth == 2 })
	dash1 := startGateTurn(g, context.Background(), "dash-1", agent.RouteSurfaceDashboard)
	waitFor(t, func() bool { return g.TurnQueueStatus().Depth == 3 })
	dash2 := startGateTurn(g, context.Background(), "dash-2", agent.RouteSurfaceDashboard)
	waitFor(t, func() bool { return g.TurnQueueStatus().Depth == 4 })

	close(release)
	awaitStarted(t, r, "dash-1")
	awaitStarted(t, r, "dash-2")
	awaitStarted(t, r, "relay")
	awaitStarted(t, r, "cron")
	for _, done := range []<-chan asyncTurn{active, cron, relay, dash1, dash2} {
		if out := awaitTurn(t, done); out.err != nil {
			t.Fatalf("turn failed: %v", out.err)
		}
	}
}

func TestTurnGate_FIFOFairnessWithinClass(t *testing.T) {
	r := newGateFakeRunner()
	release := r.block("active")
	g := newTurnGate(r)
	active := startGateTurn(g, context.Background(), "active", agent.RouteSurfaceDashboard)
	awaitStarted(t, r, "active")

	done := make([]<-chan asyncTurn, 0, 3)
	for _, query := range []string{"peer-1", "relay-2", "peer-3"} {
		source := agent.RouteSurfacePeer
		if query == "relay-2" {
			source = agent.RouteSurfaceRelay
		}
		done = append(done, startGateTurn(g, context.Background(), query, source))
		wantDepth := len(done)
		waitFor(t, func() bool { return g.TurnQueueStatus().Depth == wantDepth })
	}
	close(release)
	for _, want := range []string{"peer-1", "relay-2", "peer-3"} {
		awaitStarted(t, r, want)
	}
	_ = awaitTurn(t, active)
	for _, ch := range done {
		_ = awaitTurn(t, ch)
	}
}

func TestTurnGate_BoundedQueueRejectsClearly(t *testing.T) {
	hub := logs.NewMemory(10)
	prev := logs.Default()
	logs.Install(hub)
	t.Cleanup(func() { logs.Install(prev) })
	r := newGateFakeRunner()
	release := r.block("active")
	g := newTurnGateConfigured(r, turnSchedulerConfig{QueueDepth: 1})
	active := startGateTurn(g, context.Background(), "active", agent.RouteSurfaceDashboard)
	awaitStarted(t, r, "active")
	queued := startGateTurn(g, context.Background(), "queued", agent.RouteSurfaceRelay)
	waitFor(t, func() bool { return g.TurnQueueStatus().Depth == 1 })

	_, err := g.Turn(context.Background(), agent.TurnRequest{UserQuery: "rejected", RouteSurface: agent.RouteSurfaceCron})
	if !errors.Is(err, ErrTurnQueueFull) {
		t.Fatalf("error = %v, want ErrTurnQueueFull", err)
	}
	if got := err.Error(); got != "turn scheduler queue full (depth 1)" {
		t.Fatalf("error = %q, want clear bounded-depth message", got)
	}
	// The refusal is logged too: the gate used to log nothing.
	if es, _ := hub.Snapshot(); len(es) != 1 || es[0].Source != "turns" || es[0].Field("event") != "turns/2201" || es[0].Field("source") != "cron" {
		t.Errorf("refusal not logged: %+v", es)
	}
	close(release)
	_ = awaitTurn(t, active)
	_ = awaitTurn(t, queued)
}

func TestTurnGate_CancellationRemovesQueuedWaiter(t *testing.T) {
	r := newGateFakeRunner()
	release := r.block("active")
	g := newTurnGate(r)
	active := startGateTurn(g, context.Background(), "active", agent.RouteSurfaceDashboard)
	awaitStarted(t, r, "active")

	ctx, cancel := context.WithCancel(context.Background())
	queued := startGateTurn(g, ctx, "cancel-me", agent.RouteSurfaceRelay)
	waitFor(t, func() bool { return g.TurnQueueStatus().Depth == 1 })
	cancel()
	out := awaitTurn(t, queued)
	if !errors.Is(out.err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", out.err)
	}
	waitFor(t, func() bool { return g.TurnQueueStatus().Depth == 0 })
	if got := atomic.LoadInt32(&r.turns); got != 1 {
		t.Fatalf("inner runner saw %d turns, want only active turn", got)
	}
	close(release)
	_ = awaitTurn(t, active)
}

func TestTurnGate_DuplicateRequestsCoalesce(t *testing.T) {
	r := newGateFakeRunner()
	release := r.block("same")
	g := newTurnGate(r)
	first := startGateTurn(g, context.Background(), "same", agent.RouteSurfaceDashboard)
	awaitStarted(t, r, "same")
	second := startGateTurn(g, context.Background(), "same", agent.RouteSurfaceDashboard)
	waitFor(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.active != nil && len(g.active.waiters) == 2
	})
	if depth := g.TurnQueueStatus().Depth; depth != 0 {
		t.Fatalf("coalesced duplicate added queue depth: %d", depth)
	}
	close(release)
	for _, done := range []<-chan asyncTurn{first, second} {
		out := awaitTurn(t, done)
		if out.err != nil || out.result == nil || out.result.Response != "same" {
			t.Fatalf("coalesced outcome = %+v", out)
		}
	}
	if got := atomic.LoadInt32(&r.turns); got != 1 {
		t.Fatalf("inner runner ran %d turns, want 1", got)
	}
}

func TestTurnGate_CancelOneCoalescedWaiterKeepsSharedRequest(t *testing.T) {
	r := newGateFakeRunner()
	release := r.block("active")
	g := newTurnGate(r)
	active := startGateTurn(g, context.Background(), "active", agent.RouteSurfaceDashboard)
	awaitStarted(t, r, "active")

	ctx, cancel := context.WithCancel(context.Background())
	first := startGateTurn(g, ctx, "same", agent.RouteSurfaceRelay)
	waitFor(t, func() bool { return g.TurnQueueStatus().Depth == 1 })
	second := startGateTurn(g, context.Background(), "same", agent.RouteSurfaceRelay)
	waitFor(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return len(g.queue) == 1 && len(g.queue[0].waiters) == 2
	})

	cancel()
	if out := awaitTurn(t, first); !errors.Is(out.err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v, want context.Canceled", out.err)
	}
	if depth := g.TurnQueueStatus().Depth; depth != 1 {
		t.Fatalf("live coalesced waiter lost shared queue entry; depth = %d", depth)
	}
	close(release)
	_ = awaitTurn(t, active)
	awaitStarted(t, r, "same")
	out := awaitTurn(t, second)
	if out.err != nil || out.result == nil || out.result.Response != "same" {
		t.Fatalf("remaining waiter outcome = %+v", out)
	}
	if got := atomic.LoadInt32(&r.turns); got != 2 {
		t.Fatalf("inner runner ran %d turns, want active + one shared request", got)
	}
}

func TestTurnGate_PerSourceQuota(t *testing.T) {
	r := newGateFakeRunner()
	release := r.block("active")
	g := newTurnGateConfigured(r, turnSchedulerConfig{
		QueueDepth: 4, SourceQuotas: map[string]int{"relay": 1},
	})
	active := startGateTurn(g, context.Background(), "active", agent.RouteSurfaceDashboard)
	awaitStarted(t, r, "active")
	queued := startGateTurn(g, context.Background(), "relay-1", agent.RouteSurfaceRelay)
	waitFor(t, func() bool { return g.TurnQueueStatus().Depth == 1 })

	_, err := g.Turn(context.Background(), agent.TurnRequest{UserQuery: "relay-2", RouteSurface: agent.RouteSurfaceRelay})
	if !errors.Is(err, ErrTurnSourceQuota) {
		t.Fatalf("error = %v, want ErrTurnSourceQuota", err)
	}
	close(release)
	_ = awaitTurn(t, active)
	_ = awaitTurn(t, queued)
}

func TestTurnGate_QueuePositionAndETA(t *testing.T) {
	r := newGateFakeRunner()
	release := r.block("active")
	g := newTurnGate(r)
	active := startGateTurn(g, context.Background(), "active", agent.RouteSurfaceDashboard)
	awaitStarted(t, r, "active")
	cron := startGateTurn(g, context.Background(), "cron", agent.RouteSurfaceCron)
	waitFor(t, func() bool { return g.TurnQueueStatus().Depth == 1 })
	dash := startGateTurn(g, context.Background(), "dash", agent.RouteSurfaceDashboard)
	waitFor(t, func() bool { return g.TurnQueueStatus().Depth == 2 })

	status := g.TurnQueueStatus()
	if status.Depth != 2 || len(status.Entries) != 2 {
		t.Fatalf("queue status = %+v", status)
	}
	if got := status.Entries[0]; got.Position != 1 || got.Source != agent.RouteSurfaceDashboard {
		t.Errorf("entry 0 = %+v, want dashboard at position 1", got)
	}
	if got := status.Entries[1]; got.Position != 2 || got.Source != agent.RouteSurfaceCron {
		t.Errorf("entry 1 = %+v, want cron at position 2", got)
	}
	if status.Entries[1].ETA <= status.Entries[0].ETA {
		t.Errorf("ETAs not increasing by position: %+v", status.Entries)
	}
	close(release)
	for _, done := range []<-chan asyncTurn{active, cron, dash} {
		_ = awaitTurn(t, done)
	}
}

func TestTurnGate_NoConcurrencyInvariant(t *testing.T) {
	r := newGateFakeRunner()
	g := newTurnGateConfigured(r, turnSchedulerConfig{
		QueueDepth: 64, SourceQuotas: map[string]int{"dashboard": 64},
	})
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := g.Turn(context.Background(), agent.TurnRequest{
				UserQuery: fmt.Sprintf("turn-%02d", i), RouteSurface: agent.RouteSurfaceDashboard,
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Turn failed: %v", err)
		}
	}
	if r.maxInFlight != 1 {
		t.Fatalf("max concurrent turns = %d, want 1", r.maxInFlight)
	}
	if got := atomic.LoadInt32(&r.turns); got != 20 {
		t.Fatalf("ran %d turns, want 20", got)
	}
}

func TestTurnGate_InterjectNotGated(t *testing.T) {
	r := newGateFakeRunner()
	release := r.block("active")
	g := newTurnGate(r)
	active := startGateTurn(g, context.Background(), "active", agent.RouteSurfaceDashboard)
	awaitStarted(t, r, "active")

	g.Interject("hello")
	if got := atomic.LoadInt32(&r.interjects); got != 1 {
		t.Fatalf("interjects = %d, want 1", got)
	}
	close(release)
	_ = awaitTurn(t, active)
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
