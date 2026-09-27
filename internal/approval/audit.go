package approval

import (
	"context"

	"tenant/internal/control"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
)

// audit.go writes the Security log's approval records (docs/design/logging-v2.md
// §10.1): each prompt raised, and how every approval ended, by whom and
// where, before the gated action goes ahead or not. A grant's record is
// written with Gate, so under logs.security.on_failure: deny a grant whose
// record can't be written is a denial.

// actorTenant is Tenant itself deciding: a standing setting, a timeout, a
// shutdown, no operator to ask.
const actorTenant = "tenant"

// decider is who decided an approval a channel's ask backend raised.
type decider struct {
	actor string
	kv    []any
}

type deciderKey struct{}

// DecidedBy tells the broker, from a channel's ask backend (SetAsk), who
// decided the approval it was asked for under ctx, and what else its record
// keeps (a Discord card's nonce). An empty actor, or no call, leaves Tenant
// as the one that decided, as for a prompt that expired.
func DecidedBy(ctx context.Context, actor string, kv ...any) {
	if d, ok := ctx.Value(deciderKey{}).(*decider); ok {
		if actor != "" {
			d.actor = actor
		}
		d.kv = kv
	}
}

// Verdict records a decision made outside any broker, with nobody asked
// (the cron-exec approver's), and returns whether the action may go ahead:
// allow, unless the grant's record could not be written under the deny
// policy.
func Verdict(ctx context.Context, category, action, detail, origin, via string, allow bool) bool {
	d, outcome := control.DenyOnce, "denied_policy"
	if allow {
		d, outcome = control.ApproveOnce, "approved"
	}
	return verdict(ctx, category, action, detail, origin, via, outcome, d)
}

// verdict records a decision nobody was asked for, under a new approval ID:
// in the turn's run record as outcome (none when ""), and in the Security
// log. It returns whether the action may go ahead.
func verdict(ctx context.Context, cat, action, detail, origin, via, outcome string, d control.ApprovalDecision) bool {
	id := policyID(cat, action)
	if outcome != "" {
		recordPolicyApproval(ctx, id, cat, action, detail, origin, outcome)
	}
	return granted(approvalRec{id, cat, action, detail, origin}.decided(ctx, d, actorTenant, origin, via))
}

func granted(d control.ApprovalDecision) bool {
	return d == control.ApproveOnce || d == control.ApproveSession || d == control.ApproveAlways
}

// approvalRec is one approval as its records name it.
type approvalRec struct{ id, cat, action, detail, origin string }

// under is ctx for r's records: correlated with the approval, and not
// cancelled with the turn (the record of a prompt that expired because its
// turn ended must still be written). It also tells the gated action, through
// its slot (event.WithApproval), which approval decided it.
func (r approvalRec) under(ctx context.Context) context.Context {
	event.NoteApproval(ctx, r.id)
	return event.WithApproval(context.WithoutCancel(ctx), r.id)
}

// fields are r's record's fields, attributed to actor on surface.
func (r approvalRec) fields(actor, surface string, kv ...any) []any {
	return append([]any{"category", r.cat, "action", r.action, "detail", r.detail, "origin", r.origin,
		"actor", actor, "surface", surface}, kv...)
}

// asked records the prompt, raised by actor. An error means the action must
// not go ahead.
func (r approvalRec) asked(ctx context.Context, actor string) error {
	return catalog.ApprovalAsk.Gate(r.under(ctx), r.fields(actor, r.origin)...)
}

// decided records d, decided by actor on surface (via: what decided, see
// catalog.ApprovalYes), and returns it, or DenyOnce when a grant's record
// could not be written under the deny policy.
func (r approvalRec) decided(ctx context.Context, d control.ApprovalDecision, actor, surface, via string, kv ...any) control.ApprovalDecision {
	ctx = r.under(ctx)
	switch {
	case granted(d):
		grant := map[control.ApprovalDecision]string{control.ApproveOnce: "once", control.ApproveSession: "session", control.ApproveAlways: "always"}[d]
		if catalog.ApprovalYes.Gate(ctx, r.fields(actor, surface, append([]any{"grant", grant, "via", via}, kv...)...)...) != nil {
			return control.DenyOnce
		}
	case d == control.Expired:
		_ = catalog.ApprovalExpired.Audit(ctx, r.fields(actor, surface, kv...)...)
	case d == control.DenyShutdown:
		_ = catalog.ApprovalShutdown.Audit(ctx, r.fields(actor, surface, kv...)...)
	default:
		_ = catalog.ApprovalNo.Audit(ctx, r.fields(actor, surface, append([]any{"via", via}, kv...)...)...)
	}
	return d
}
