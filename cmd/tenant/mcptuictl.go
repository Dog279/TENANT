package main

// mcptuictl.go adapts the tool mux's runtime remote-MCP methods to
// tui.MCPControl (TEN-164), so `/mcp add|list|remove` manages connectors live
// in the TUI and persists the server list to launchConfig (survives restart).

import "tenant/internal/tui"

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
		out = append(out, tui.MCPServerInfo{Label: s.Label, URL: s.URL, Enabled: s.Enabled, ToolCount: s.ToolCount})
	}
	return out
}

// Add connects + activates the server (browser OAuth, blocking) and persists it.
func (a mcpTUIControl) Add(url string) (tui.MCPServerInfo, error) {
	label, n, err := a.mux.AddRemoteMCP(url)
	if err != nil {
		return tui.MCPServerInfo{}, err
	}
	a.persist(url, true)
	return tui.MCPServerInfo{Label: label, URL: url, Enabled: true, ToolCount: n}, nil
}

// Remove disables + forgets a server (by URL or label) and drops it from config.
// The live session (if any) is torn down at mux.Close; a disabled connector is
// idle, so leaving it until shutdown is harmless.
func (a mcpTUIControl) Remove(target string) (bool, error) {
	label, url := a.resolve(target)
	if label == "" {
		return false, nil
	}
	a.mux.SetEnabled(label, false)
	a.mux.forgetPlugin(label)
	a.persist(url, false)
	return true, nil
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

func (a mcpTUIControl) persist(url string, add bool) {
	if a.lc == nil {
		return
	}
	if add {
		for _, u := range a.lc.MCPRemotes {
			if u == url {
				return // already persisted
			}
		}
		a.lc.MCPRemotes = append(a.lc.MCPRemotes, url)
	} else {
		kept := a.lc.MCPRemotes[:0]
		for _, u := range a.lc.MCPRemotes {
			if u != url {
				kept = append(kept, u)
			}
		}
		a.lc.MCPRemotes = kept
	}
	_ = a.lc.save(a.cfgDir)
}
