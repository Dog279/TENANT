package main

// mcp_dashctl.go adapts mcpTUIControl to dashboard.MCPControl (TEN-205): the
// web Remote-services page reuses the SAME runtime connector machinery as the
// TUI /mcp. Connect blocks on a host-side OAuth browser, so the dashboard
// handler runs it off the request goroutine (hybrid model: connect at the
// host, list/remove from anywhere). No business logic forks.

import "tenant/internal/dashboard"

type dashMCP struct{ m mcpTUIControl }

func (d dashMCP) Servers() []dashboard.MCPServerView {
	in := d.m.List()
	out := make([]dashboard.MCPServerView, 0, len(in))
	for _, s := range in {
		out = append(out, dashboard.MCPServerView{
			Label:     s.Label,
			URL:       s.URL,
			Enabled:   s.Enabled,
			ToolCount: s.ToolCount,
		})
	}
	return out
}

// Connect blocks on the host-side browser OAuth flow; the dashboard handler
// calls it in a goroutine.
func (d dashMCP) Connect(url string) error {
	_, err := d.m.Add(url)
	return err
}

func (d dashMCP) Remove(target string) (bool, error) { return d.m.Remove(target) }
