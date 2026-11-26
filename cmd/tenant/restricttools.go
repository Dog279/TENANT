package main

// restricttools.go builds a permanently read/comms-safe tool surface for an
// UNATTENDED runner (the cron scheduler). It is intentionally separate from the
// Discord relay's restrictForDiscord (discordtools.go): the relay is driven by a
// present human over a Discord channel and keeps two gated comms tools live
// behind a button approver, whereas a cron job runs with NOBODY watching and has
// no channel to scope to. So this surface is stricter — EVERY gated/destructive
// tool is cut on the safe surface (no kept-gated exceptions), team/orchestra
// fan-out is cut there too, plus any name matching extraDeny (cron uses it to
// cut the cron_* tools so a scheduled job cannot schedule more jobs). The EXEC
// surface, which the operator turns on knowingly, keeps the fan-out tools: a
// helper it spawns inherits the job's approver and exec surface
// (inheritExecSurface), so it answers to the same unattended rules.
//
// The surface is a live filtered VIEW over the full registry, not a snapshot
// copied into a StaticRegistry. That distinction sank the daily brief for five
// straight days: StaticRegistry.Search ignores the query embedding and returns
// the first K tools in REGISTRATION order, and the composite registers the
// per-agent local tools (spawn/memory/web) before the shared mux — so the
// model's 12 surfaced tools were always the local set, and money_*/gmail_*/
// discord_send_message sat in the dispatcher's allowlist without the model ever
// SEEING them. Every run reported "the required tools are not available", which
// was true from where the model stood. Delegating Search to the live registry
// keeps relevance ranking and the core-tool pins, and means /enable, /disable
// and late plugin activation reach cron without a restart.

import (
	"context"
	"fmt"
	"strings"

	"tenant/internal/agent"
	"tenant/internal/model"
)

// fanOut reports whether a tool is team/orchestra fan-out (cut on the safe
// unattended surface — it spawns sub-agents that aren't individually gated).
func fanOut(name string) bool {
	return strings.HasPrefix(name, "team") || strings.HasPrefix(name, "orchestr") || name == "spawn_agent"
}

// readCommsSafe reports whether a tool may run on an unattended read/comms-safe
// surface: never gated/destructive, never team/orchestra fan-out.
func readCommsSafe(sp model.ToolSpec) bool {
	return !fanOut(sp.Name) && !sp.Gated
}

// execAllowed reports whether a tool may run on an unattended EXEC surface:
// gated/dangerous tools ARE allowed (the caller stamps the turn ctx with an
// auto-approver), and so is team/orchestra fan-out: a helper spawned from the
// run inherits its approver and exec surface (inheritExecSurface).
func execAllowed(model.ToolSpec) bool {
	return true
}

// restrictedView is the live filtered registry. Everything answers from the
// inner registry AT CALL TIME — nothing is copied, so the view can never go
// stale against the running mux.
type restrictedView struct {
	inner   agent.ToolRegistry
	allow   func(model.ToolSpec) bool
	deny    func(name string) bool // may be nil
	surface string                 // e.g. "scheduled cron jobs"
}

func (v *restrictedView) permitted(sp model.ToolSpec) bool {
	if v.deny != nil && v.deny(sp.Name) {
		return false
	}
	return v.allow(sp)
}

func (v *restrictedView) Get(name string) (model.ToolSpec, bool) {
	sp, ok := v.inner.Get(name)
	if !ok || !v.permitted(sp) {
		return model.ToolSpec{}, false
	}
	return sp, true
}

func (v *restrictedView) All() []model.ToolSpec {
	full := v.inner.All()
	out := make([]model.ToolSpec, 0, len(full))
	for _, sp := range full {
		if v.permitted(sp) {
			out = append(out, sp)
		}
	}
	return out
}

// Search delegates ranking to the inner registry, then filters. The extra
// headroom matters: every disallowed tool that outranks an allowed one would
// otherwise consume one of the K slots, and on the safe surface the gated cut
// is large enough to starve the result entirely. The filtered result is NOT
// re-truncated to k — the composite returns its local tools first, so a naive
// [:k] would reintroduce exactly the registration-order starvation this view
// exists to fix.
func (v *restrictedView) Search(ctx context.Context, embedding []float32, k int) ([]model.ToolSpec, error) {
	extra := 0
	for _, sp := range v.inner.All() {
		if !v.permitted(sp) {
			extra++
		}
	}
	ranked, err := v.inner.Search(ctx, embedding, k+extra)
	if err != nil {
		return nil, err
	}
	out := make([]model.ToolSpec, 0, len(ranked))
	for _, sp := range ranked {
		if v.permitted(sp) {
			out = append(out, sp)
		}
	}
	return out, nil
}

var _ agent.ToolRegistry = (*restrictedView)(nil)

// restrictSurface builds (registry, dispatcher) as live filtered views over the
// full registry/dispatcher. The registry is what the model SEES; the dispatcher
// refuses any tool outside the view with a surface-specific message so the
// model learns the boundary instead of looping. extraDeny may be nil.
func restrictSurface(full agent.ToolRegistry, inner agent.ToolDispatcher, surface string, allow func(model.ToolSpec) bool, extraDeny func(name string) bool) (*restrictedView, *readCommsDispatcher) {
	view := &restrictedView{inner: full, allow: allow, deny: extraDeny, surface: surface}
	return view, &readCommsDispatcher{inner: inner, view: view, surface: surface}
}

// restrictReadComms is the read/comms-safe surface (gated tools cut).
func restrictReadComms(full agent.ToolRegistry, inner agent.ToolDispatcher, surface string, extraDeny func(name string) bool) (*restrictedView, *readCommsDispatcher) {
	return restrictSurface(full, inner, surface, readCommsSafe, extraDeny)
}

// restrictExec is the exec surface (gated tools and team/orchestra kept). Only
// for explicitly exec-opted-in unattended jobs whose turn ctx carries an
// auto-approver.
func restrictExec(full agent.ToolRegistry, inner agent.ToolDispatcher, surface string, extraDeny func(name string) bool) (*restrictedView, *readCommsDispatcher) {
	return restrictSurface(full, inner, surface, execAllowed, extraDeny)
}

// readCommsDispatcher refuses any tool outside the view's live allowlist,
// delegating allowed calls to the inner (full) dispatcher. The check is at
// CALL time against the live registry, so a tool the operator /disables is
// refused on the next run without a restart.
type readCommsDispatcher struct {
	inner   agent.ToolDispatcher
	view    *restrictedView
	surface string // e.g. "scheduled cron jobs"
}

func (d *readCommsDispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if _, ok := d.view.Get(call.Name); !ok {
		return fmt.Sprintf("%q is not available to %s — these runs are UNATTENDED and read/research-only "+
			"(exec / write / destructive / outbound-send, job-scheduling, and team/orchestra are disabled). "+
			"Report what you found instead of attempting that action.", call.Name, d.surface), true, nil
	}
	return d.inner.Dispatch(ctx, call)
}
