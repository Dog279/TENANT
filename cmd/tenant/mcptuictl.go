package main

// mcptuictl.go adapts the tool mux's runtime remote-MCP methods to
// tui.MCPControl (TEN-164), so `/mcp add|list|remove` manages connectors live
// in the TUI and persists the server list to launchConfig (survives restart).

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"tenant/internal/logs"
	"tenant/internal/plugins/mcpremote"
	"tenant/internal/tui"
)

// mcpAudit is the log-book line for every connector change, whichever surface
// (TUI, dashboard, the agent's mcp_* tools, /configure atlassian) drove it —
// so "when did that server get trusted?" is answerable from <data>/logs/mcp/.
var mcpAudit = logs.For("mcp")

// mcpAuthNotifier builds the mux's auth-change callback: a feed line every
// time, plus a direct operator DM (best-effort, a failed one is logged) — the
// operator is usually remote when a connector dies, and the feed is not where
// they are looking.
func mcpAuthNotifier(feed func(string), dm func(string) error) func(label, url string, needsReauth bool, err string) {
	send := func(label, msg string) {
		if dm == nil {
			return
		}
		if derr := dm(msg); derr != nil {
			mcpAudit.Warn("mcp: operator DM about a connector's sign-in failed", "label", label, "err", derr)
		}
	}
	return func(label, url string, needsReauth bool, err string) {
		if needsReauth {
			if feed != nil {
				feed("⚠ " + label + " needs re-authorization — its tools fail until you reconnect at the host:  tenant mcp connect " + url)
			}
			send(label, "⚠ **"+label+"** needs re-authorization (its sign-in expired and could not be refreshed: "+err+"). Its tools — Jira/Confluence if this is Atlassian — fail until you reconnect at the host:\n`tenant mcp connect "+url+"`\nor the dashboard's Remote services page.")
			return
		}
		if feed != nil {
			feed("✅ " + label + " re-authorized — its tools are back")
		}
		send(label, "✅ **"+label+"** re-authorized — its tools are back.")
	}
}

// mcpAuthProblemsFn adapts a registry's dead-connector list to the cron
// runner's banner input ("label (fix: tenant mcp connect url)"). Interface-typed
// so the composite registry and a bare mux both fit.
func mcpAuthProblemsFn(m interface{ RemoteMCPAuthProblems() []mcpServerStatus }) func() []string {
	return func() []string {
		var out []string
		for _, s := range m.RemoteMCPAuthProblems() {
			out = append(out, s.Label+" (fix: tenant mcp connect "+s.URL+")")
		}
		return out
	}
}

type mcpTUIControl struct {
	mux    *toolMux
	cfgDir string
	lc     *launchConfig // may be nil (no persistence)
}

func newMCPControl(mux *toolMux, cfgDir string, lc *launchConfig) mcpTUIControl {
	return mcpTUIControl{mux: mux, cfgDir: cfgDir, lc: lc}
}

func (a mcpTUIControl) List() []tui.MCPServerInfo {
	servers := a.mux.RemoteMCPList()
	out := make([]tui.MCPServerInfo, 0, len(servers))
	for _, s := range servers {
		out = append(out, tui.MCPServerInfo{
			Label: s.Label, URL: s.URL, Enabled: s.Enabled, ToolCount: s.ToolCount, Trust: s.Trust,
			NeedsReauth: s.NeedsReauth, AuthErr: s.AuthErr, AuthSince: s.AuthSince,
		})
	}
	return out
}

// Add connects + activates the server (browser OAuth if the server asks for
// one — a local no-auth server connects instantly) and persists it. A new
// connector starts in trust mode "ask". A failed save is returned: the
// connector is live, but would not come back after a restart.
func (a mcpTUIControl) Add(url string) (tui.MCPServerInfo, error) {
	label, n, err := a.mux.AddRemoteMCP(url)
	if err != nil {
		mcpAudit.Warn("mcp: connector add failed", "url", url, "err", err.Error())
		return tui.MCPServerInfo{}, err
	}
	if err := a.persist(url, true); err != nil {
		return tui.MCPServerInfo{}, errNotSaved("mcp: adding "+label, err)
	}
	trust := a.mux.MCPTrust(label)
	mcpAudit.Info("mcp: connector added", "label", label, "url", url, "tools", n, "trust", trust)
	return tui.MCPServerInfo{Label: label, URL: url, Enabled: true, ToolCount: n, Trust: trust}, nil
}

// SetTrust changes a connector's trust mode LIVE (ask | allow | deny) and
// persists it, so it survives a restart and applies again when the connector
// reconnects. No reconnect, no restart.
func (a mcpTUIControl) SetTrust(target, mode string) (tui.MCPServerInfo, error) {
	m, ok := mcpremote.ParseTrust(mode)
	if !ok {
		return tui.MCPServerInfo{}, fmt.Errorf("trust must be ask | allow | deny")
	}
	label, url := a.resolve(target)
	if label == "" {
		return tui.MCPServerInfo{}, fmt.Errorf("no remote MCP server matched %q", target)
	}
	if err := a.mux.SetMCPTrust(label, m); err != nil {
		return tui.MCPServerInfo{}, err
	}
	if err := a.persistTrust(url, m); err != nil {
		return tui.MCPServerInfo{}, errNotSaved("mcp: trust "+m+" for "+label, err)
	}
	lvl := slog.LevelInfo
	if m == mcpremote.TrustAllow {
		lvl = slog.LevelWarn // a trust grant is the line an auditor looks for
	}
	mcpAudit.Log(nil, lvl, "mcp: connector trust set", "label", label, "url", url, "trust", m)
	for _, s := range a.List() {
		if s.Label == label {
			return s, nil
		}
	}
	return tui.MCPServerInfo{Label: label, URL: url, Trust: m}, nil
}

// ToolNames lists the tool names a connector currently advertises.
func (a mcpTUIControl) ToolNames(label string) []string {
	var out []string
	for _, t := range a.mux.ToolList() {
		if t.Plugin == label {
			out = append(out, t.Name)
		}
	}
	return out
}

// Remove disables + forgets a server (by URL or label) and drops it from config.
// The live session (if any) is torn down at mux.Close; a disabled connector is
// idle, so leaving it until shutdown is harmless.
func (a mcpTUIControl) Remove(target string) (bool, error) {
	label, url := a.resolve(target)
	if label == "" {
		return false, nil
	}
	if strings.HasPrefix(url, "stdio:") {
		// A local server is the operator's config.json entry (TEN-318):
		// forgetting it here would last only until the next launch.
		return false, fmt.Errorf("%s is a local MCP server configured in config.json (mcp_servers): remove it there; /mcp trust %s deny stops its tools now", label, label)
	}
	a.mux.SetEnabled(label, false)
	a.mux.ForgetPlugin(label)
	if err := errors.Join(a.persist(url, false), a.persistTrust(url, "")); err != nil {
		return true, errNotSaved("mcp: removing "+label, err)
	}
	mcpAudit.Info("mcp: connector removed", "label", label, "url", url)
	return true, nil
}

// persistTrust records (mode != "") or clears (mode == "") a connector's trust
// mode in config.json, keyed by URL like the server list.
func (a mcpTUIControl) persistTrust(url, mode string) error {
	if a.lc == nil {
		return nil
	}
	return persistLaunchConfig(a.cfgDir, a.lc, func(lc *launchConfig) {
		if mode == "" || mode == mcpremote.TrustAsk {
			delete(lc.MCPTrust, url)
			return
		}
		if lc.MCPTrust == nil {
			lc.MCPTrust = map[string]string{}
		}
		lc.MCPTrust[url] = mode
	})
}

// resolve maps a URL or label (exact, or the label derived from a URL) to a
// registered server.
func (a mcpTUIControl) resolve(target string) (label, url string) {
	servers := a.mux.RemoteMCPList()
	for _, s := range servers {
		if s.Label == target || s.URL == target {
			return s.Label, s.URL
		}
	}
	want := mcpLabel(target)
	for _, s := range servers {
		if s.Label == want {
			return s.Label, s.URL
		}
	}
	return "", ""
}

func (a mcpTUIControl) persist(url string, add bool) error {
	if a.lc == nil {
		return nil
	}
	// Fresh-load + reapply (persistLaunchConfig) — a.lc is the boot-time
	// object, and saving it directly clobbers every field other writers
	// persisted since boot (the skill-config lost-update bug).
	return persistLaunchConfig(a.cfgDir, a.lc, func(lc *launchConfig) {
		if add {
			for _, u := range lc.MCPRemotes {
				if u == url {
					return // already persisted
				}
			}
			lc.MCPRemotes = append(lc.MCPRemotes, url)
			return
		}
		kept := lc.MCPRemotes[:0]
		for _, u := range lc.MCPRemotes {
			if u != url {
				kept = append(kept, u)
			}
		}
		lc.MCPRemotes = kept
	})
}
