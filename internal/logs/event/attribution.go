package event

import (
	"context"
	"errors"
	"sync/atomic"
)

// Attribution: who did the work under a context, through which surface, and
// under which approval. They are the event's Actor, Surface and
// Corr.ApprovalID, set for every event written under ctx, whatever its
// provider, so a definition never declares them as fields:
//
//	ctx = event.WithActor(ctx, "discord:1234", "discord")
//	catalog.ApprovalYes.Audit(event.WithApproval(ctx, id), "category", cat, …)

type actorKey struct{}

type actor struct{ name, surface string }

// WithActor attributes the events written under ctx to actor (the operator,
// a Discord user, a peer, a remote address, Tenant itself), acting through
// surface (tui, dashboard, discord, imessage, cron, peering…).
func WithActor(ctx context.Context, name, surface string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor{name, surface})
}

type approvalKey struct{}

// WithApproval correlates the events written under ctx with approval id. An
// empty id opens a slot for NoteApproval to fill: an action that asks for
// approval and then runs opens one before it asks, so the records of the run
// carry the approval that let it run.
func WithApproval(ctx context.Context, id string) context.Context {
	s := new(atomic.Pointer[string])
	s.Store(&id)
	return context.WithValue(ctx, approvalKey{}, s)
}

// NoteApproval fills the slot the nearest WithApproval under ctx opened, if
// any. The gate that decides an approval calls it.
func NoteApproval(ctx context.Context, id string) {
	if s, ok := ctx.Value(approvalKey{}).(*atomic.Pointer[string]); ok {
		s.Store(&id)
	}
}

// attribute sets e's actor, surface and approval from ctx.
func attribute(ctx context.Context, e *Event) {
	if ctx == nil {
		return
	}
	if a, ok := ctx.Value(actorKey{}).(actor); ok {
		e.Actor, e.Surface = a.name, a.surface
	}
	if s, ok := ctx.Value(approvalKey{}).(*atomic.Pointer[string]); ok {
		e.Corr.ApprovalID = *s.Load()
	}
}

// ErrRefused marks an Audit error under logs.security.on_failure: deny: the
// record was not written, so the action it records must not go ahead.
var ErrRefused = errors.New("event: the Security log can't be written, so gated actions are refused")

// Gate is Audit for a record a gated action waits on. It returns an error
// only when the action must not go ahead: the record was not written and the
// emitter refuses gated actions until records are written again (ErrRefused).
// With the default policy, or no emitter installed, the action goes ahead;
// the emitter reports the failure.
func (d *Def) Gate(ctx context.Context, kv ...any) error {
	if err := d.Audit(ctx, kv...); errors.Is(err, ErrRefused) {
		return err
	}
	return nil
}
