package dashboard

// Server-wide turn coordination (TEN-80). The agent is a SINGLE shared
// instance, so a turn must be serialized across ALL connections — not just
// per-socket as the TEN-78 handler did. wsCoordinator is that one coordinator:
// at most one Turn runs on the agent at any moment, server-wide.
//
// Concurrency model: a single mutex guards the small active-turn state
// (whether a turn is running, its cancel func, the owning connection, and a
// monotonic turn id). The turn body itself runs in its own goroutine and
// streams its events through the broker like before; the coordinator only
// gates entry and tracks the owner so stop/disconnect can cancel the right
// turn.

import (
	"context"
	"sync"

	"tenant/internal/agent"
)

// wsCoordinator serializes Turn execution across the whole Server. Only one
// turn is active at a time; the connection that started it is its owner, so
// that connection's "stop" (or its disconnect) cancels it while other clients
// keep streaming the shared event feed.
type wsCoordinator struct {
	runner AgentRunner

	mu     sync.Mutex
	active bool               // a Turn is currently running on the agent
	owner  *wsConn            // the connection that started the active turn
	cancel context.CancelFunc // cancels the active turn's context
	id     uint64             // monotonic id of the active turn (and counter)
}

// newWSCoordinator builds the coordinator over the shared agent runner. The
// runner may be nil (bare health-only server); startTurn handles that by
// declining with a notice rather than calling a nil agent.
func newWSCoordinator(runner AgentRunner) *wsCoordinator {
	return &wsCoordinator{runner: runner}
}

// startTurn launches a turn on behalf of conn unless one is already running
// anywhere on the server. When a turn is already active, conn gets a "busy"
// notice and no second Turn call is made (the single-active-turn invariant).
//
// The turn runs under a child of conn's connection context (parent) so that
// conn's teardown cancels its own turn; the coordinator additionally holds the
// cancel func so "stop" and an owner disconnect can cancel it explicitly.
func (co *wsCoordinator) startTurn(conn *wsConn, parent context.Context, query string) {
	if co.runner == nil {
		conn.notice("agent unavailable")
		return
	}
	co.mu.Lock()
	if co.active {
		co.mu.Unlock()
		conn.notice("a turn is already running")
		return
	}
	turnCtx, cancel := context.WithCancel(parent)
	co.id++
	turnID := co.id
	co.active = true
	co.owner = conn
	co.cancel = cancel
	co.mu.Unlock()

	go func() {
		defer co.finish(turnID, cancel)
		// Turn streams its own events through the broker (the server's
		// Observer is broker.Publish), so every client's writer pump relays
		// them. We only drive the call and drop its buffered result.
		_, _ = co.runner.Turn(turnCtx, agent.TurnRequest{UserQuery: query})
	}()
}

// finish clears the active-turn state after a turn goroutine returns. It only
// clears state still owned by turnID so a late finish can't stomp a turn that
// another client started in the meantime.
func (co *wsCoordinator) finish(turnID uint64, cancel context.CancelFunc) {
	co.mu.Lock()
	if co.active && co.id == turnID {
		co.active = false
		co.owner = nil
		co.cancel = nil
	}
	co.mu.Unlock()
	cancel() // release the context's resources regardless
}

// interject forwards a mid-turn message to the shared agent. Always allowed:
// it queues regardless of who (if anyone) owns the active turn, matching the
// agent's own no-op-when-idle semantics.
func (co *wsCoordinator) interject(msg string) {
	if co.runner != nil {
		co.runner.Interject(msg)
	}
}

// stop cancels the active turn if conn owns it. A stop from a non-owner (a
// client that didn't start the running turn) is ignored so one browser can't
// cancel another's turn.
func (co *wsCoordinator) stop(conn *wsConn) {
	co.mu.Lock()
	var cancel context.CancelFunc
	if co.active && co.owner == conn {
		cancel = co.cancel
	}
	co.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// disconnected is called when conn's handler tears down. If conn owned the
// active turn, that turn is canceled; a non-owner disconnect leaves the
// running turn alone. (conn's own connection context is also canceled by the
// caller, which would cancel its turn too — this makes that intent explicit
// and central, and is a no-op for non-owners.)
func (co *wsCoordinator) disconnected(conn *wsConn) {
	co.stop(conn)
}

// startBackground launches a turn with NO connection owner — the SSR/SSE path
// (TEN-109), where a stateless POST starts the turn and its events fan out to
// every /events SSE stream via the broker. Returns false if the agent is
// unavailable or a turn is already running. The turn runs under a background
// context (not a request), so it survives the POST returning; stopActive (or a
// natural finish) ends it. Single-active-turn is preserved.
func (co *wsCoordinator) startBackground(query string) bool {
	co.mu.Lock()
	if co.runner == nil || co.active {
		co.mu.Unlock()
		return false
	}
	turnCtx, cancel := context.WithCancel(context.Background())
	co.id++
	turnID := co.id
	co.active = true
	co.owner = nil // connection-less
	co.cancel = cancel
	co.mu.Unlock()

	go func() {
		defer co.finish(turnID, cancel)
		_, _ = co.runner.Turn(turnCtx, agent.TurnRequest{UserQuery: query})
	}()
	return true
}

// stopActive cancels the active turn regardless of owner — the SSR Stop button
// (a local single-operator panel has no cross-client ownership to protect).
func (co *wsCoordinator) stopActive() {
	co.mu.Lock()
	cancel := co.cancel
	co.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
