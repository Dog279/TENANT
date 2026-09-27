package approval

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"tenant/internal/agent"
	"tenant/internal/control"
	"tenant/internal/runtrace"
)

// registry is the id-keyed source of truth for pending dangerous-action
// approvals (TEN-203). It is SHARED across the brokers that front the same
// operator (the host broker + the iMessage offsite broker share one), so any
// observer — the interactive TUI and the web dashboard — sees every pending
// request and can resolve it by id. First-resolver-wins via delete-under-lock;
// the per-request reply chan (buffered cap 1) is ALWAYS sent OUTSIDE the lock so
// the lock is never held across a channel op.
type registry struct {
	mu      sync.Mutex
	pending map[string]*pendingReq
	seq     atomic.Uint64
	// emit publishes pending/resolved events to the live event stream so both
	// observers refresh. Set once at wiring (nil ⇒ no event surfacing — fail-safe).
	emit func(agent.Event)
}

type pendingReq struct {
	id     string
	req    control.ApprovalRequest
	origin string
	at     time.Time
	reply  chan control.ApprovalDecision
	span   *runtrace.Span
}

func newRegistry() *registry {
	return &registry{pending: map[string]*pendingReq{}}
}

// register inserts a new pending request and emits EventApprovalPending. Returns
// the assigned id + the buffered reply chan the caller blocks on.
func (r *registry) register(ctx context.Context, req control.ApprovalRequest, origin string) (string, chan control.ApprovalDecision) {
	id := fmt.Sprintf("ap-%d", r.seq.Add(1)) // atomic — no lock needed for the id
	reply := make(chan control.ApprovalDecision, 1)
	req.ID = id
	req.Reply = reply
	p := &pendingReq{id: id, req: req, origin: origin, at: time.Now(), reply: reply, span: runtrace.SpanFromContext(ctx)}
	r.mu.Lock()
	r.pending[id] = p
	r.mu.Unlock()
	r.emitEvent(agent.EventApprovalPending, p, "")
	p.recordApproval("pending")
	return id, reply
}

// Resolve fires the decision for id — first-resolver-wins. Returns false (clean
// no-op) for an unknown / already-resolved id, so the loser of a TUI/dashboard
// race is harmless. The reply send is buffered + outside the lock.
func (r *registry) Resolve(id string, d control.ApprovalDecision) bool {
	r.mu.Lock()
	p := r.pending[id]
	if p != nil {
		delete(r.pending, id)
	}
	r.mu.Unlock()
	if p == nil {
		return false
	}
	p.reply <- d // cap-1 buffer ⇒ never blocks, fires exactly once (delete-under-lock gated)
	r.emitEvent(agent.EventApprovalResolved, p, outcomeOf(d))
	p.recordApproval(outcomeOf(d))
	return true
}

// removeIfPending drops id (if still pending) without an operator decision — used
// when the requesting turn's ctx is cancelled, so Pending() stays honest and a
// late dashboard Resolve is a clean no-op. Emits Resolved(expired).
func (r *registry) removeIfPending(id string) {
	r.mu.Lock()
	p := r.pending[id]
	if p != nil {
		delete(r.pending, id)
	}
	r.mu.Unlock()
	if p != nil {
		r.emitEvent(agent.EventApprovalResolved, p, "expired")
		p.recordApproval("expired")
	}
}

// DenyAll denies every still-pending request (process/serve teardown), so no
// agent goroutine stays blocked. Buffered, non-blocking sends.
func (r *registry) DenyAll() {
	r.mu.Lock()
	pend := r.pending
	r.pending = map[string]*pendingReq{}
	r.mu.Unlock()
	for _, p := range pend {
		select {
		case p.reply <- control.DenyOnce:
		default:
		}
		r.emitEvent(agent.EventApprovalResolved, p, "denied")
		p.recordApproval("denied")
	}
}

func (p *pendingReq) recordApproval(outcome string) {
	if p == nil || p.span == nil {
		return
	}
	p.span.AddApproval(runtrace.Approval{
		ID: p.id, Category: p.req.Category, Action: p.req.Action,
		Detail: p.req.Detail, Origin: p.origin, Outcome: outcome,
	})
}

// Pending is the snapshot the dashboard / TUI read. Never nil (stable REST shape).
func (r *registry) Pending() []control.PendingApproval {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]control.PendingApproval, 0, len(r.pending))
	now := time.Now()
	for _, p := range r.pending {
		out = append(out, control.PendingApproval{
			ID:       p.id,
			Category: p.req.Category,
			Action:   p.req.Action,
			Detail:   p.req.Detail,
			AgeSecs:  int(now.Sub(p.at).Seconds()),
		})
	}
	return out
}

func (r *registry) emitEvent(kind agent.EventKind, p *pendingReq, outcome string) {
	if r.emit == nil {
		return
	}
	r.emit(agent.Event{
		Kind: kind,
		Text: fmt.Sprintf("%s: %s", p.req.Action, p.req.Detail),
		Approval: &agent.ApprovalEvent{
			ID: p.id, Category: p.req.Category, Action: p.req.Action,
			Detail: p.req.Detail, Origin: p.origin, Outcome: outcome,
		},
	})
}

// emitBeside surfaces a pending/resolved event for a request handled by a
// REPLACEMENT backend (the Discord button approver) that is NOT in the
// resolvable registry — observers SEE it (Origin=discord) but can't resolve it
// from the dashboard, because that path owns its own resolution (see askDecision).
func (r *registry) emitBeside(kind agent.EventKind, req control.ApprovalRequest, origin, outcome string) {
	if r == nil || r.emit == nil {
		return
	}
	r.emit(agent.Event{
		Kind: kind,
		Text: fmt.Sprintf("%s: %s", req.Action, req.Detail),
		Approval: &agent.ApprovalEvent{
			ID: "beside-" + origin, Category: req.Category, Action: req.Action,
			Detail: req.Detail, Origin: origin, Outcome: outcome,
		},
	})
}

func outcomeOf(d control.ApprovalDecision) string {
	switch d {
	case control.ApproveOnce:
		return "approved"
	case control.ApproveSession:
		return "approved_session"
	case control.ApproveAlways:
		return "approved_always"
	default:
		return "denied"
	}
}
