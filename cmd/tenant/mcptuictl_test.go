package main

import "testing"

func TestRegisterAndForgetMCPRemote(t *testing.T) {
	m := newToolMux()
	label := m.registerMCPRemote("https://mcp.atlassian.com/v1/mcp")
	if label != "mcp:mcp.atlassian.com" {
		t.Fatalf("label = %q", label)
	}
	list := m.RemoteMCPList()
	if len(list) != 1 || list[0].URL != "https://mcp.atlassian.com/v1/mcp" {
		t.Fatalf("RemoteMCPList = %+v", list)
	}
	if list[0].Enabled || list[0].ToolCount != 0 {
		t.Errorf("a registered-but-unconnected server should be disabled with 0 tools: %+v", list[0])
	}
	// Idempotent: re-registering the same URL doesn't duplicate.
	m.registerMCPRemote("https://mcp.atlassian.com/v1/mcp")
	if len(m.RemoteMCPList()) != 1 {
		t.Errorf("re-register duplicated the server")
	}
	// Forget removes it entirely.
	m.forgetPlugin(label)
	if len(m.RemoteMCPList()) != 0 {
		t.Errorf("forgetPlugin left the server registered")
	}
	if m.hasPlugin(label) {
		t.Errorf("forgetPlugin left the plugin registered")
	}
}

func TestMCPControl_Persist(t *testing.T) {
	lc := &launchConfig{}
	a := newMCPControl(newToolMux(), t.TempDir(), lc)
	a.persist("https://x/mcp", true)
	if len(lc.MCPRemotes) != 1 {
		t.Fatalf("persist add: %v", lc.MCPRemotes)
	}
	a.persist("https://x/mcp", true) // dedupe
	if len(lc.MCPRemotes) != 1 {
		t.Errorf("persist should dedupe: %v", lc.MCPRemotes)
	}
	a.persist("https://x/mcp", false)
	if len(lc.MCPRemotes) != 0 {
		t.Errorf("persist remove: %v", lc.MCPRemotes)
	}
}

func TestMCPControl_Resolve(t *testing.T) {
	m := newToolMux()
	m.registerMCPRemote("https://mcp.atlassian.com/v1/mcp")
	a := newMCPControl(m, t.TempDir(), nil)

	if l, _ := a.resolve("mcp:mcp.atlassian.com"); l == "" {
		t.Error("resolve by label failed")
	}
	if l, _ := a.resolve("https://mcp.atlassian.com/v1/mcp"); l == "" {
		t.Error("resolve by URL failed")
	}
	if l, _ := a.resolve("nope"); l != "" {
		t.Error("resolve matched a nonexistent server")
	}
}
