package toolmux

import "testing"

func TestMCPLabel_KeepsPortAndDisambiguates(t *testing.T) {
	if got := MCPLabel("https://mcp.atlassian.com/v1/mcp"); got != "mcp:mcp.atlassian.com" {
		t.Errorf("hosted label changed: %q", got)
	}
	if got := MCPLabel("http://127.0.0.1:9000/mcp"); got != "mcp:127.0.0.1:9000" {
		t.Errorf("local label should keep the port: %q", got)
	}
	m := New()
	a := m.RegisterMCPRemote("http://127.0.0.1:9000/a")
	b := m.RegisterMCPRemote("http://127.0.0.1:9000/b")
	if a != "mcp:127.0.0.1:9000" || b != "mcp:127.0.0.1:9000/b" {
		t.Errorf("collision labels = %q, %q", a, b)
	}
	if again := m.labelFor("http://127.0.0.1:9000/b"); again != b {
		t.Errorf("a registered URL must keep its label: %q", again)
	}
}
