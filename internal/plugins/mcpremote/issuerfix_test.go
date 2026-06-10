package mcpremote

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIssuerHostRelated(t *testing.T) {
	related := [][2]string{
		{"https://cf.mcp.atlassian.com", "https://mcp.atlassian.com"}, // CDN subdomain
		{"https://mcp.atlassian.com", "https://cf.mcp.atlassian.com"}, // symmetric
		{"https://mcp.atlassian.com", "https://mcp.atlassian.com"},    // equal
	}
	for _, c := range related {
		if !issuerHostRelated(c[0], c[1]) {
			t.Errorf("issuerHostRelated(%q,%q) = false, want true", c[0], c[1])
		}
	}
	unrelated := [][2]string{
		{"https://evil.com", "https://mcp.atlassian.com"},
		{"https://atlassian.com.evil.com", "https://mcp.atlassian.com"}, // suffix-trick, not a label boundary
	}
	for _, c := range unrelated {
		if issuerHostRelated(c[0], c[1]) {
			t.Errorf("issuerHostRelated(%q,%q) = true, want false", c[0], c[1])
		}
	}
}

func TestRewriteASIssuer(t *testing.T) {
	// The real Atlassian shape: CDN issuer, endpoints split across hosts.
	meta := `{"issuer":"https://cf.mcp.atlassian.com","authorization_endpoint":"https://mcp.atlassian.com/v1/authorize","token_endpoint":"https://cf.mcp.atlassian.com/v1/token","code_challenge_methods_supported":["S256"]}`
	out, changed := rewriteASIssuer([]byte(meta), "https://mcp.atlassian.com")
	if !changed {
		t.Fatal("expected the CDN issuer to be rewritten")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["issuer"] != "https://mcp.atlassian.com" {
		t.Errorf("issuer = %v, want https://mcp.atlassian.com", m["issuer"])
	}
	// Endpoints must be preserved untouched.
	if m["token_endpoint"] != "https://cf.mcp.atlassian.com/v1/token" {
		t.Errorf("token_endpoint was altered: %v", m["token_endpoint"])
	}
	if !strings.Contains(string(out), "code_challenge_methods_supported") {
		t.Error("other fields dropped")
	}

	// An UNRELATED issuer must NOT be rewritten (no masking a redirect to evil).
	evil := `{"issuer":"https://evil.com"}`
	if _, changed := rewriteASIssuer([]byte(evil), "https://mcp.atlassian.com"); changed {
		t.Error("unrelated issuer must not be rewritten")
	}
}
