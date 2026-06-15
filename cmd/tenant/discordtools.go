package main

// discordtools.go is the offsite capability surface for the Discord relay agent.
//
// All tools are available — the Discord agent has the same tool surface as the
// local TUI. The operator allowlist verifies identity; the per-action button
// approver gates dangerous calls. No tool wall.
//
// The registry is LIVE (TEN-229): it delegates to the running tool mux rather
// than a snapshot taken at relay-construction time, so tools that come online
// after the relay starts — the Atlassian/Jira MCP connector (which adopts its
// 31 tools asynchronously), `/enable`, `/configure` — appear to the Discord
// agent immediately, exactly as they do in the TUI. A frozen snapshot was the
// reason remote turns couldn't see the Jira tools.

import (
	"context"
	"strings"
	"sync/atomic"

	"tenant/internal/agent"
	"tenant/internal/model"
)

// execGate is a live on/off switch, retained for backward compatibility with
// the relay manager's SetExec / AllowExec persistence. When all tools are
// available regardless, the gate no longer restricts the surface — but the
// manager still flips it and persists the preference.
type execGate struct{ on atomic.Bool }

func (g *execGate) set(v bool)    { g.on.Store(v) }
func (g *execGate) enabled() bool { return g.on.Load() }

// discordRegistry exposes the full, LIVE tool catalog by delegating to the
// running mux — so the Discord agent always sees what the TUI sees, including
// tools registered after the relay started. The gate is retained but no longer
// changes which tools are visible — it's a no-op surface switch kept for API
// compatibility with the relay manager.
type discordRegistry struct {
	live agent.ToolRegistry // the running mux (composite), not a snapshot
}

func (r *discordRegistry) Get(name string) (model.ToolSpec, bool) { return r.live.Get(name) }
func (r *discordRegistry) Search(ctx context.Context, emb []float32, k int) ([]model.ToolSpec, error) {
	return r.live.Search(ctx, emb, k)
}
func (r *discordRegistry) All() []model.ToolSpec { return r.live.All() }

// restrictForDiscord builds the offsite tool surface over the LIVE registry (no
// filtering, no snapshot). Returns a registry + dispatcher that expose every
// tool, plus the execGate for API compatibility with the relay manager.
// Dangerous calls are still routed to the per-action button approver via
// originConfirm; the operator allowlist gates who can drive the agent at all.
func restrictForDiscord(live agent.ToolRegistry, inner agent.ToolDispatcher) (*discordRegistry, *restrictedDispatcher, *execGate) {
	gate := &execGate{}
	registry := &discordRegistry{live: live}
	disp := &restrictedDispatcher{inner: inner}
	return registry, disp, gate
}

// restrictedDispatcher delegates every call to the inner (full) dispatcher.
// No tool is refused — the wall is removed.
type restrictedDispatcher struct {
	inner agent.ToolDispatcher
}

func (d *restrictedDispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	return d.inner.Dispatch(ctx, call)
}

// discordRoutingDispatcher routes discord_* tools to the approver-wired Discord
// dispatcher (so a gated discord_send prompts via the button approver, not the
// shared TUI broker) and everything else to the (now full) restricted surface.
type discordRoutingDispatcher struct {
	discord agent.ToolDispatcher // discord plugin wired to the discordApprover
	rest    agent.ToolDispatcher // full tool surface
}

func (d *discordRoutingDispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if strings.HasPrefix(call.Name, "discord_") {
		return d.discord.Dispatch(ctx, call)
	}
	return d.rest.Dispatch(ctx, call)
}
