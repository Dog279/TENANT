package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"tenant/internal/agent"
	"tenant/internal/dashboard"
	"tenant/internal/tui"
)

// headlessApprovals is the TEN-194 drain for dangerous-action approvals when
// tenant runs as a daemon. In the interactive TUI the approval broker's
// requests channel is drained by listenApprovals (tui.go), which prompts the
// operator. Headless there is no TUI, so a gated "ask"-mode action would post
// to the broker's requests chan (buffer 4) and then block forever on its reply
// — wedging the single-flight brain. This drain consumes that channel, holds
// each request as pending, surfaces it in the activity feed, and exposes it via
// the dashboard's /api/approvals so the operator resolves it from the web UI
// (or a future attach client). It satisfies dashboard.ApprovalControl.
//
// Fail-safe on shutdown: when the drain's context is cancelled it denies every
// still-pending request (DenyOnce), so the blocked turn goroutines unblock
// rather than hang the graceful shutdown.
type headlessApprovals struct {
	requests <-chan tui.ApprovalRequest
	emit     func(agent.Event) // feed sink: surfaces "approval pending" on the dashboard
	seq      atomic.Uint64

	mu      sync.Mutex
	pending map[string]*pendingApproval
}

type pendingApproval struct {
	id    string
	req   tui.ApprovalRequest
	at    time.Time
	reply chan tui.ApprovalDecision
}

// newHeadlessApprovals wires the drain over a broker's request channel. emit may
// be nil (no feed surfacing).
func newHeadlessApprovals(requests <-chan tui.ApprovalRequest, emit func(agent.Event)) *headlessApprovals {
	return &headlessApprovals{
		requests: requests,
		emit:     emit,
		pending:  map[string]*pendingApproval{},
	}
}

// run drains the request channel until ctx is cancelled, then denies everything
// still pending so no turn goroutine stays blocked on its reply. Call in a
// goroutine.
func (h *headlessApprovals) run(ctx context.Context) {
	for {
		select {
		case req, ok := <-h.requests:
			if !ok {
				h.denyAll()
				return
			}
			h.add(req)
		case <-ctx.Done():
			h.denyAll()
			return
		}
	}
}

func (h *headlessApprovals) add(req tui.ApprovalRequest) {
	// A request with no reply channel is informational only — nothing can be
	// decided, so don't queue it (defensive; the broker always sets Reply).
	if req.Reply == nil {
		return
	}
	id := fmt.Sprintf("ap-%d", h.seq.Add(1))
	p := &pendingApproval{id: id, req: req, at: time.Now(), reply: req.Reply}
	h.mu.Lock()
	h.pending[id] = p
	h.mu.Unlock()
	if h.emit != nil {
		h.emit(agent.Event{
			Kind: agent.EventApproval,
			Text: fmt.Sprintf("%s: %s — approve in the dashboard (/api/approvals)", req.Action, req.Detail),
		})
	}
}

// Pending implements dashboard.ApprovalControl.
func (h *headlessApprovals) Pending() []dashboard.PendingApproval {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]dashboard.PendingApproval, 0, len(h.pending))
	now := time.Now()
	for _, p := range h.pending {
		out = append(out, dashboard.PendingApproval{
			ID:       p.id,
			Category: p.req.Category,
			Action:   p.req.Action,
			Detail:   p.req.Detail,
			AgeSecs:  int(now.Sub(p.at).Seconds()),
		})
	}
	return out
}

// Decide implements dashboard.ApprovalControl: resolve one pending request.
func (h *headlessApprovals) Decide(id, decision string) error {
	d, err := parseApprovalDecision(decision)
	if err != nil {
		return err
	}
	h.mu.Lock()
	p := h.pending[id]
	if p != nil {
		delete(h.pending, id)
	}
	h.mu.Unlock()
	if p == nil {
		return fmt.Errorf("approval %q not found (already resolved or expired)", id)
	}
	// reply is buffered (cap 1) so this never blocks even if the waiting turn
	// already gave up (ctx-cancelled) and no longer reads it.
	p.reply <- d
	return nil
}

func (h *headlessApprovals) denyAll() {
	h.mu.Lock()
	pend := h.pending
	h.pending = map[string]*pendingApproval{}
	h.mu.Unlock()
	for _, p := range pend {
		select {
		case p.reply <- tui.DenyOnce:
		default:
		}
	}
}

// parseApprovalDecision maps the REST decision string to a broker decision.
// Deny-by-default: an empty/unknown string is rejected rather than silently
// approving.
func parseApprovalDecision(s string) (tui.ApprovalDecision, error) {
	switch s {
	case "approve", "approve_once", "once":
		return tui.ApproveOnce, nil
	case "approve_session", "session":
		return tui.ApproveSession, nil
	case "approve_always", "always":
		return tui.ApproveAlways, nil
	case "deny", "deny_once", "reject":
		return tui.DenyOnce, nil
	default:
		return tui.DenyOnce, fmt.Errorf("unknown decision %q (want approve|approve_session|approve_always|deny)", s)
	}
}
