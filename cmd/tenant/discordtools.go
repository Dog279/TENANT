package main

// discordtools.go is the offsite capability surface. TEN-118 (v1) cut it to
// READ/RESEARCH/COMMS-ONLY: every gated (dangerous) tool removed except the two
// low-blast-radius Discord comms tools, and team/orchestra fan-out cut. TEN-123
// (v2) adds an OPT-IN exec mode: when the operator flips `/relay exec on`, the
// gated dangerous tools (os_exec/write, web_transact, sql, send, …) are exposed
// again — but EVERY call still hits the origin-scoped button approver
// (no-allow, fail-closed; see discordconfirm.go + discordapprove.go). team/
// orchestra stay cut even in exec mode: their fan-out spawns sub-agents that
// are NOT individually button-gated, so they'd multiply blast radius per
// approval. The exec toggle is live (an atomic gate) — no agent rebuild.

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"tenant/internal/agent"
	"tenant/internal/model"
)

// discordKeptGated are the only gated tools kept offsite even WITHOUT exec mode:
// the bot posting/reacting in the SAME DM the operator drives — reversible,
// parse=[] (no mass-ping), non-financial, non-RCE. Both still hit the approver.
var discordKeptGated = map[string]bool{
	"discord_send_message": true,
	"discord_react":        true,
}

// discordToolAllowed reports whether a tool may run in an offsite Discord turn,
// given the live exec-mode flag.
func discordToolAllowed(sp model.ToolSpec, allowExec bool) bool {
	// team/orchestra fan-out multiplies blast radius + cost per approval and
	// spawns sub-agents that aren't individually button-gated — cut ALWAYS,
	// even in exec mode.
	if strings.HasPrefix(sp.Name, "team") || strings.HasPrefix(sp.Name, "orchestr") {
		return false
	}
	// Gated == dangerous (os_exec, web_transact, sql_ddl, gsuite_send, …).
	// Without exec opt-in, cut (except the kept Discord comms tools). WITH exec
	// opt-in, available — but still button-gated by the origin-scoped approver.
	if sp.Gated && !discordKeptGated[sp.Name] {
		return allowExec
	}
	return true
}

// execGate is the live on/off switch for offsite exec mode, shared by the
// dynamic registry (what the model SEES) and the restricted dispatcher (what it
// may RUN). Flipped by the relay manager on `/relay exec on|off`.
type execGate struct{ on atomic.Bool }

func (g *execGate) set(v bool)    { g.on.Store(v) }
func (g *execGate) enabled() bool { return g.on.Load() }

// discordRegistry exposes the read/comms tools always, plus the gated dangerous
// tools only while exec mode is on — switching live between two static surfaces
// so a runtime `/relay exec on` immediately changes the tools the model is
// offered (and `off` immediately hides them again).
type discordRegistry struct {
	base *agent.StaticRegistry // exec-off surface (read/research/comms)
	exec *agent.StaticRegistry // exec-on surface (+ dangerous tools)
	gate *execGate
}

func (r *discordRegistry) reg() *agent.StaticRegistry {
	if r.gate.enabled() {
		return r.exec
	}
	return r.base
}

func (r *discordRegistry) Get(name string) (model.ToolSpec, bool) { return r.reg().Get(name) }
func (r *discordRegistry) Search(ctx context.Context, emb []float32, k int) ([]model.ToolSpec, error) {
	return r.reg().Search(ctx, emb, k)
}
func (r *discordRegistry) All() []model.ToolSpec { return r.reg().All() }

// restrictForDiscord builds the offsite tool surface: a live registry exposing
// only the allowed specs (so the model never sees a tool it can't use) and a
// dispatcher that refuses any cut tool by name with an explicit offsite-boundary
// error (so the model learns the boundary instead of looping). Both honor the
// returned execGate, so exec mode toggles at runtime.
func restrictForDiscord(full []model.ToolSpec, inner agent.ToolDispatcher) (*discordRegistry, *restrictedDispatcher, *execGate) {
	gate := &execGate{}
	base := agent.NewStaticRegistry()
	exec := agent.NewStaticRegistry()
	allowedBase := make(map[string]bool, len(full))
	allowedExec := make(map[string]bool, len(full))
	for _, sp := range full {
		if discordToolAllowed(sp, false) {
			base.Register(sp)
			allowedBase[sp.Name] = true
		}
		if discordToolAllowed(sp, true) {
			exec.Register(sp)
			allowedExec[sp.Name] = true
		}
	}
	reg := &discordRegistry{base: base, exec: exec, gate: gate}
	disp := &restrictedDispatcher{inner: inner, allowedBase: allowedBase, allowedExec: allowedExec, gate: gate}
	return reg, disp, gate
}

// restrictedDispatcher refuses any tool not allowed in the CURRENT exec mode.
// The allowed sets are computed once; the gate selects which applies per call,
// so a tool unlocked by `/relay exec on` is dispatchable immediately and re-cut
// by `off`.
type restrictedDispatcher struct {
	inner       agent.ToolDispatcher
	allowedBase map[string]bool // exec-off
	allowedExec map[string]bool // exec-on
	gate        *execGate
}

func (d *restrictedDispatcher) allowed(name string) bool {
	if d.gate != nil && d.gate.enabled() {
		return d.allowedExec[name]
	}
	return d.allowedBase[name]
}

func (d *restrictedDispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if !d.allowed(call.Name) {
		return fmt.Sprintf("%q is not available over Discord — this offsite session is read/research/comms only "+
			"(exec / write / destructive / outbound-send require `/relay exec on` at the local console; "+
			"team / orchestra actions are never available offsite).", call.Name), true, nil
	}
	return d.inner.Dispatch(ctx, call)
}

// discordRoutingDispatcher routes discord_* tools to the approver-wired Discord
// dispatcher (so a gated discord_send prompts via the button approver, not the
// shared TUI broker) and everything else to the restricted dispatcher.
type discordRoutingDispatcher struct {
	discord agent.ToolDispatcher // discord plugin wired to the discordApprover
	rest    agent.ToolDispatcher // restricted read/comms (+exec) surface
}

func (d *discordRoutingDispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if strings.HasPrefix(call.Name, "discord_") {
		return d.discord.Dispatch(ctx, call)
	}
	return d.rest.Dispatch(ctx, call)
}
