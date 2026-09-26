package main

// discordtools.go is the offsite capability surface for the Discord relay agent.
//
// The Discord agent sees the same tools as the local TUI. The operator
// allowlist verifies identity; the Discord broker gates every dangerous call.
// Tool visibility is not the boundary: the broker is. It refuses the categories
// the relay's exec mode unlocks while that mode is off (relayExecLocks), then
// applies tool rules and relay.permissions (ask = a button in Discord).
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

	"tenant/internal/agent"
	"tenant/internal/model"
	"tenant/internal/plugins/discord"
)

// relayExecCategories are what the relay's exec mode (relay.allow_exec,
// `/relay exec`) unlocks: while it is off the Discord broker refuses them
// whatever relay.permissions or a tool rule says. TEN-123 introduced the mode;
// TEN-229 dropped its enforcement along with the tool wall, leaving a switch
// that changed nothing while `/relay status` still reported "read/comms only".
// TEN-338 enforces it again, at the broker.
var relayExecCategories = []string{catExec, catWrite, catDestructive, catSandbox, catSend}

// relayExecLocks reports whether exec mode must be on for action. Discord's
// own post and react stay available with exec mode off, as TEN-123 kept them:
// talking in Discord is the "comms" in read/research/comms. They still follow
// the send category's mode.
func relayExecLocks(action, cat string) bool {
	switch action {
	case discord.ActionSendMessage, discord.ActionReact:
		return false
	}
	return containsString(relayExecCategories, cat)
}

// discordRegistry exposes the full, LIVE tool catalog by delegating to the
// running mux — so the Discord agent always sees what the TUI sees, including
// tools registered after the relay started.
type discordRegistry struct {
	live agent.ToolRegistry // the running mux (composite), not a snapshot
}

func (r *discordRegistry) Get(name string) (model.ToolSpec, bool) { return r.live.Get(name) }
func (r *discordRegistry) Search(ctx context.Context, emb []float32, k int) ([]model.ToolSpec, error) {
	return r.live.Search(ctx, emb, k)
}
func (r *discordRegistry) All() []model.ToolSpec { return r.live.All() }

// restrictForDiscord builds the offsite tool surface over the LIVE registry (no
// filtering, no snapshot). Dangerous calls route to the Discord broker via
// originConfirm; the operator allowlist gates who can drive the agent at all.
func restrictForDiscord(live agent.ToolRegistry, inner agent.ToolDispatcher) (*discordRegistry, *restrictedDispatcher) {
	return &discordRegistry{live: live}, &restrictedDispatcher{inner: inner}
}

// restrictedDispatcher delegates every call to the inner (full) dispatcher.
// No tool is refused here; the broker decides each dangerous call.
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
