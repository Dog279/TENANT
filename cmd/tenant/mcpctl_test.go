package main

import "testing"

// The port is part of the label: several tool servers on one host (the agent's
// own local ones, 127.0.0.1:9000 and :9001) must get distinct labels. Hosted
// services have no port, so their labels are unchanged.
func TestMcpLabel(t *testing.T) {
	cases := map[string]string{
		"https://mcp.atlassian.com/v1/mcp": "mcp:mcp.atlassian.com",
		"https://example.com:8443/mcp":     "mcp:example.com:8443",
		"http://localhost:8000/mcp":        "mcp:localhost:8000",
		"http://127.0.0.1:9000/mcp":        "mcp:127.0.0.1:9000",
		"mcp.foo.com":                      "mcp:mcp.foo.com",
	}
	for in, want := range cases {
		if got := mcpLabel(in); got != want {
			t.Errorf("mcpLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
