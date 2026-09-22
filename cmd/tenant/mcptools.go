package main

// mcptools.go: the agent-facing connector tools — mcp_list / mcp_add /
// mcp_remove / mcp_trust — the "automated adoption" half of the tool-server
// story. The agent can now build a Model Context Protocol tool server (any
// language, Streamable HTTP, on this box or elsewhere), start it, and adopt it
// into its own tool surface in the same turn:
//
//   1. mcp_add <url>  → the operator approves the ADOPTION (permission category
//      "mcp": ask by default; "allow" makes adoption hands-off, "deny" forbids it),
//   2. the server's tools come live with trust "ask" — every gated call prompts —
//   3. and, when the agent asked for trust=allow, the operator gets ONE more
//      prompt to let that connector's tools run without per-call approval.
//
// Trust is live and per connector (mcpremote.Trust*), changeable any time from
// /mcp trust, the dashboard, or mcp_trust; the connection and its trust are
// persisted so they come back at the next launch. Everything is gated so a
// prompt-injected turn can never quietly wire in a server, and the tools are
// cut from unattended cron runners (notCron).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"tenant/internal/logs"
	"tenant/internal/model"
	"tenant/internal/plugins/mcpremote"
	"tenant/internal/tui"
)

// mcpAdopter is the slice of mcpTUIControl the tools drive (a fake in tests).
type mcpAdopter interface {
	List() []tui.MCPServerInfo
	Add(url string) (tui.MCPServerInfo, error)
	Remove(target string) (bool, error)
	SetTrust(target, mode string) (tui.MCPServerInfo, error)
	ToolNames(label string) []string
}

type mcpToolPlugin struct {
	ctl     mcpAdopter
	confirm func(ctx context.Context, action, detail string) bool // the broker's gate (origin-aware); nil ⇒ fail closed
	log     *slog.Logger
}

func newMCPToolPlugin(ctl mcpAdopter, confirm func(context.Context, string, string) bool) *mcpToolPlugin {
	return &mcpToolPlugin{ctl: ctl, confirm: confirm, log: logs.For("mcp")}
}

func (p *mcpToolPlugin) Tools() []model.ToolSpec {
	obj := func(props string, req ...string) json.RawMessage {
		r := ""
		for i, x := range req {
			if i > 0 {
				r += ","
			}
			r += `"` + x + `"`
		}
		return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":[` + r + `]}`)
	}
	return []model.ToolSpec{
		{Name: "mcp_list",
			Description: "List the MCP tool servers connected to this agent: label, URL, tool count, trust mode (ask = each gated call prompts the operator, allow = its tools run without prompts, deny = refused) and whether a sign-in expired.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`)},
		{Name: "mcp_add",
			Description: "Connect a Model Context Protocol tool server by its Streamable-HTTP URL so its tools become available to you from the next turn on — for example a server you just wrote and started on this machine (http://127.0.0.1:<port>/mcp). The operator must approve the adoption. The server's tools arrive with trust 'ask' (each gated call prompts the operator); pass trust='allow' to ask the operator to let them run without per-call prompts. The connection is remembered across restarts.",
			Parameters: obj(`"url":{"type":"string","description":"the server's Streamable-HTTP endpoint, http:// or https://"},`+
				`"trust":{"type":"string","enum":["ask","allow"],"description":"requested trust for its tools (default ask)"},`+
				`"reason":{"type":"string","description":"one line shown to the operator in the approval prompt: what the server is and why you want it"}`, "url"),
			Gated: true},
		{Name: "mcp_remove",
			Description: "Disconnect and forget a connected tool server (by label or URL). The operator must approve.",
			Parameters:  obj(`"target":{"type":"string","description":"the server's label (from mcp_list) or URL"}`, "target"),
			Gated:       true},
		{Name: "mcp_trust",
			Description: "Change a connected tool server's trust: ask (each gated call prompts the operator), allow (its tools run without per-call prompts), deny (refuse and hide its tools). The operator must approve.",
			Parameters:  obj(`"target":{"type":"string","description":"the server's label or URL"},"mode":{"type":"string","enum":["ask","allow","deny"]}`, "target", "mode"),
			Gated:       true},
	}
}

// ok is the operator gate; a missing confirm hook fails closed.
func (p *mcpToolPlugin) ok(ctx context.Context, action, detail string) bool {
	return p.confirm != nil && p.confirm(ctx, action, detail)
}

func (p *mcpToolPlugin) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	var args struct {
		URL    string `json:"url"`
		Trust  string `json:"trust"`
		Reason string `json:"reason"`
		Target string `json:"target"`
		Mode   string `json:"mode"`
	}
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return "invalid arguments: " + err.Error(), true, nil
		}
	}
	switch call.Name {
	case "mcp_list":
		return p.list(), false, nil
	case "mcp_add":
		return p.add(ctx, strings.TrimSpace(args.URL), args.Trust, strings.TrimSpace(args.Reason))
	case "mcp_remove":
		return p.remove(ctx, strings.TrimSpace(args.Target))
	case "mcp_trust":
		return p.trust(ctx, strings.TrimSpace(args.Target), args.Mode)
	}
	return "unknown tool " + call.Name, true, nil
}

func (p *mcpToolPlugin) list() string {
	servers := p.ctl.List()
	if len(servers) == 0 {
		return "no MCP tool servers connected. mcp_add <url> connects one (the operator approves)."
	}
	var b strings.Builder
	for _, s := range servers {
		state := "registered, not connected"
		switch {
		case s.NeedsReauth:
			state = "sign-in expired — its tools fail until the operator reconnects it"
		case s.Enabled:
			state = fmt.Sprintf("on, %d tools", s.ToolCount)
		case s.ToolCount > 0:
			state = fmt.Sprintf("connected, %d tools (disabled)", s.ToolCount)
		}
		trust := s.Trust
		if trust == "" {
			trust = mcpremote.TrustAsk
		}
		fmt.Fprintf(&b, "- %s  %s  [%s]  trust: %s\n", s.Label, s.URL, state, trust)
	}
	b.WriteString("trust: ask = each gated call prompts the operator · allow = no per-call prompts · deny = refused")
	return b.String()
}

func (p *mcpToolPlugin) add(ctx context.Context, url, trust, reason string) (string, bool, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "url must start with http:// or https:// — the server's Streamable-HTTP endpoint, e.g. http://127.0.0.1:9000/mcp", true, nil
	}
	want, ok := mcpremote.ParseTrust(trust)
	if !ok || want == mcpremote.TrustDeny {
		return "trust must be ask or allow", true, nil
	}
	detail := url
	if reason != "" {
		detail += " — " + reason
	}
	if !p.ok(ctx, "mcp_add", detail) {
		p.log.Info("mcp: adoption declined by the operator", "url", url)
		return "not approved: the operator declined connecting " + url + ". Do not retry unless they ask you to.", true, nil
	}
	info, err := p.ctl.Add(url)
	if err != nil {
		p.log.Warn("mcp: adoption failed", "url", url, "err", err.Error())
		return "connect failed: " + err.Error() + " — is the server running and serving Streamable HTTP at that URL?", true, nil
	}
	names := p.ctl.ToolNames(info.Label)
	mode := mcpremote.TrustAsk
	if want == mcpremote.TrustAllow {
		prompt := fmt.Sprintf("%s — allow its %d tool(s) (%s) to run WITHOUT per-call approval? Deny keeps a prompt per gated call.", info.Label, len(names), joinMax(names, 8))
		if p.ok(ctx, "mcp_trust", prompt) {
			mode = mcpremote.TrustAllow
		}
	}
	if mode != mcpremote.TrustAsk {
		if _, err := p.ctl.SetTrust(info.Label, mode); err != nil {
			p.log.Warn("mcp: trust change failed after adoption", "label", info.Label, "err", err.Error())
			mode = mcpremote.TrustAsk
		}
	}
	p.log.Info("mcp: adoption by the agent approved", "label", info.Label, "url", url, "tools", len(names), "trust", mode, "reason", reason)

	var b strings.Builder
	fmt.Fprintf(&b, "connected %s (%s) — %d tool(s) now available", info.Label, url, len(names))
	if len(names) > 0 {
		b.WriteString(": " + joinMax(names, 20))
	}
	b.WriteString(".\n")
	switch mode {
	case mcpremote.TrustAllow:
		b.WriteString("trust: allow — its tools run without per-call approval.\n")
	default:
		b.WriteString("trust: ask — each gated tool call asks the operator first. (mcp_trust can request allow if the operator agrees.)\n")
	}
	b.WriteString("The connection is remembered: it reconnects when Tenant restarts, if the server is running.")
	return b.String(), false, nil
}

func (p *mcpToolPlugin) remove(ctx context.Context, target string) (string, bool, error) {
	if target == "" {
		return "target (label or URL) required", true, nil
	}
	if !p.ok(ctx, "mcp_remove", target) {
		return "not approved: the operator declined removing " + target, true, nil
	}
	removed, err := p.ctl.Remove(target)
	if err != nil {
		return "remove failed: " + err.Error(), true, nil
	}
	if !removed {
		return "no connected server matched " + target + " (see mcp_list)", true, nil
	}
	p.log.Info("mcp: removal by the agent approved", "target", target)
	return "removed " + target + " — its tools are gone.", false, nil
}

func (p *mcpToolPlugin) trust(ctx context.Context, target, mode string) (string, bool, error) {
	m, ok := mcpremote.ParseTrust(mode)
	if target == "" || !ok {
		return "target (label or URL) and mode ask|allow|deny required", true, nil
	}
	if !p.ok(ctx, "mcp_trust", target+" → "+m) {
		return "not approved: the operator declined setting " + target + " to " + m, true, nil
	}
	info, err := p.ctl.SetTrust(target, m)
	if err != nil {
		return "trust change failed: " + err.Error(), true, nil
	}
	p.log.Info("mcp: trust change by the agent approved", "label", info.Label, "trust", m)
	return fmt.Sprintf("%s trust is now %s (%d tools).", info.Label, m, info.ToolCount), false, nil
}

func joinMax(names []string, max int) string {
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:max], ", ") + fmt.Sprintf(", … (+%d more)", len(names)-max)
}
