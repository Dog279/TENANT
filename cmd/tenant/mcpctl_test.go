package main

import "testing"

func TestMcpLabel(t *testing.T) {
	cases := map[string]string{
		"https://mcp.atlassian.com/v1/mcp": "mcp:mcp.atlassian.com",
		"https://example.com:8443/mcp":     "mcp:example.com",
		"http://localhost:8000/mcp":        "mcp:localhost",
		"mcp.foo.com":                      "mcp:mcp.foo.com",
	}
	for in, want := range cases {
		if got := mcpLabel(in); got != want {
			t.Errorf("mcpLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
