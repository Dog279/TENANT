package main

// restricttools.go builds a permanently read/comms-safe tool surface for an
// UNATTENDED runner (the cron scheduler). It is intentionally separate from the
// Discord relay's restrictForDiscord (discordtools.go): the relay is driven by a
// present human over a Discord channel and keeps two gated comms tools live
// behind a button approver, whereas a cron job runs with NOBODY watching and has
// no channel to scope to. So this surface is stricter — EVERY gated/destructive
// tool is cut (no kept-gated exceptions), team/orchestra fan-out is cut, plus any
// name matching extraDeny (cron uses it to cut the cron_* tools so a scheduled
// job cannot schedule more jobs). Keeping it a separate, parallel function means
// the proven relay path is left byte-identical (additive, Windows-stable).

import (
	"context"
	"fmt"
	"strings"

	"tenant/internal/agent"
	"tenant/internal/model"
)

// fanOut reports whether a tool is team/orchestra fan-out (cut on every
// unattended surface — it spawns sub-agents that aren't individually gated).
func fanOut(name string) bool {
	return strings.HasPrefix(name, "team") || strings.HasPrefix(name, "orchestr")
}

// readCommsSafe reports whether a tool may run on an unattended read/comms-safe
// surface: never gated/destructive, never team/orchestra fan-out.
func readCommsSafe(sp model.ToolSpec) bool {
	return !fanOut(sp.Name) && !sp.Gated
}

// execAllowed reports whether a tool may run on an unattended EXEC surface:
// gated/dangerous tools ARE allowed (the caller stamps the turn ctx with an
// auto-approver), but team/orchestra fan-out is still cut.
func execAllowed(sp model.ToolSpec) bool {
	return !fanOut(sp.Name)
}

// restrictSurface builds (registry, dispatcher) exposing only the tools from
// full that pass `allow` (minus extraDeny). The registry is what the model SEES;
// the dispatcher refuses any other tool by name with a surface-specific message
// so the model learns the boundary instead of looping. extraDeny may be nil.
func restrictSurface(full []model.ToolSpec, inner agent.ToolDispatcher, surface string, allow func(model.ToolSpec) bool, extraDeny func(name string) bool) (*agent.StaticRegistry, *readCommsDispatcher) {
	reg := agent.NewStaticRegistry()
	allowed := make(map[string]bool, len(full))
	for _, sp := range full {
		if extraDeny != nil && extraDeny(sp.Name) {
			continue
		}
		if !allow(sp) {
			continue
		}
		reg.Register(sp)
		allowed[sp.Name] = true
	}
	return reg, &readCommsDispatcher{inner: inner, allowed: allowed, surface: surface}
}

// restrictReadComms is the read/comms-safe surface (gated tools cut). Behavior
// preserved exactly for the default cron runner + any other caller.
func restrictReadComms(full []model.ToolSpec, inner agent.ToolDispatcher, surface string, extraDeny func(name string) bool) (*agent.StaticRegistry, *readCommsDispatcher) {
	return restrictSurface(full, inner, surface, readCommsSafe, extraDeny)
}

// restrictExec is the exec surface (gated tools kept; team/orchestra cut). Only
// for explicitly exec-opted-in unattended jobs whose turn ctx carries an
// auto-approver.
func restrictExec(full []model.ToolSpec, inner agent.ToolDispatcher, surface string, extraDeny func(name string) bool) (*agent.StaticRegistry, *readCommsDispatcher) {
	return restrictSurface(full, inner, surface, execAllowed, extraDeny)
}

// readCommsDispatcher refuses any tool not on the read/comms-safe allowlist,
// delegating allowed calls to the inner (full) dispatcher.
type readCommsDispatcher struct {
	inner   agent.ToolDispatcher
	allowed map[string]bool
	surface string // e.g. "scheduled cron jobs"
}

func (d *readCommsDispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if !d.allowed[call.Name] {
		return fmt.Sprintf("%q is not available to %s — these runs are UNATTENDED and read/research-only "+
			"(exec / write / destructive / outbound-send, job-scheduling, and team/orchestra are disabled). "+
			"Report what you found instead of attempting that action.", call.Name, d.surface), true, nil
	}
	return d.inner.Dispatch(ctx, call)
}
