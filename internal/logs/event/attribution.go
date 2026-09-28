package event

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
)

// Attribution: who did the work, through which surface, and under which
// approval. They are the event's Actor, Surface and Corr.ApprovalID, not
// data. The fields "actor" and "surface", passed like any other (declared or
// not), set the first two; a template may still name them. The approval comes
// from the context:
//
//	catalog.ApprovalYes.Audit(event.WithApproval(ctx, id), "actor", "discord:1234", "surface", "discord", …)

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

type toolCallKey struct{}

// WithToolCall correlates the events written under ctx with one tool call:
// its approval, its sandbox run and their records name the call, so two
// calls in one turn stay apart.
func WithToolCall(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, toolCallKey{}, id)
}

// attribute moves e's actor and surface fields into its attributes, and sets
// its approval and tool call from ctx.
func attribute(ctx context.Context, e *Event) {
	e.Data = slices.DeleteFunc(e.Data, func(f Field) bool {
		switch f.Key {
		case "actor":
			e.Actor = f.Text()
		case "surface":
			e.Surface = f.Text()
		default:
			return false
		}
		return true
	})
	if ctx == nil {
		return
	}
	if s, ok := ctx.Value(approvalKey{}).(*atomic.Pointer[string]); ok {
		e.Corr.ApprovalID = *s.Load()
	}
	if id, ok := ctx.Value(toolCallKey{}).(string); ok && e.Corr.ToolCallID == "" {
		e.Corr.ToolCallID = id
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
