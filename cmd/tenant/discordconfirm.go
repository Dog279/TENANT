package main

// discordconfirm.go is the V2 (TEN-123) approval-routing seam. Every gated
// plugin calls one injected `confirm(ctx, action, detail)`; that's the single
// place where a dangerous action is approved. originConfirm wraps it so that a
// turn STAMPED as offsite (a Discord-driven turn — see withOffsiteConfirm)
// routes its approval to the origin-scoped approver carried on the ctx (the
// Discord button approver: no allow/session memory, fail-closed), while every
// other (local TUI) turn uses the local broker UNCHANGED.
//
// Why this layer: the action + detail are resolved INSIDE the plugin (e.g.
// "os_exec" + the command), so the plugin's Policy.Confirm is the only place the
// prompt can categorize correctly. The ctx threads relay → agent.Turn →
// dispatchBatch → plugin.Policy.Confirm untouched (agent.go), so the stamp
// reaches here. The security property this buys: a local "exec = allow" mode can
// NEVER leak to an offsite turn, because for an offsite turn the local broker is
// not consulted at all.

import "context"

// offsiteConfirm is the approval surface (the same shape as approvalBroker.Confirm
// and discordApprover.Confirm). A defined type so it's collision-safe as a
// context value.
type offsiteConfirm func(ctx context.Context, action, detail string) bool

type offsiteConfirmKey struct{}

// withOffsiteConfirm stamps ctx so dangerous actions in this turn route to c
// (the origin-scoped approver) instead of the local broker. A nil c is a no-op
// (the turn stays local-broker-gated).
func withOffsiteConfirm(ctx context.Context, c offsiteConfirm) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, offsiteConfirmKey{}, c)
}

// offsiteConfirmFrom returns the stamped approver, or nil for a local turn.
func offsiteConfirmFrom(ctx context.Context) offsiteConfirm {
	c, _ := ctx.Value(offsiteConfirmKey{}).(offsiteConfirm)
	return c
}

// originConfirm wraps the local broker's Confirm: a turn stamped offsite routes
// to the ctx approver; an unstamped (local) turn uses the broker as before. This
// is the function handed to buildToolMux, so EVERY gated plugin honors it.
func originConfirm(broker offsiteConfirm) offsiteConfirm {
	return func(ctx context.Context, action, detail string) bool {
		if c := offsiteConfirmFrom(ctx); c != nil {
			return c(ctx, action, detail)
		}
		if broker == nil {
			return false // no broker + not offsite-approved → fail closed
		}
		return broker(ctx, action, detail)
	}
}
