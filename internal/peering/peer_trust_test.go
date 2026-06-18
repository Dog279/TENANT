package peering

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tenant/internal/plugins/mcpremote"
)

// TestPeerTrust_NovelReadOnlyToolStaysGated is the TEN-252 security property: a
// peer that advertises a NOVEL tool marked read-only — one NOT in the dialer's
// federation allowlist — must stay GATED (route through approval), even though
// the peer claims it's read-only. Only the explicit allowlist ungates.
func TestPeerTrust_NovelReadOnlyToolStaysGated(t *testing.T) {
	// A registrar that adds a novel read-only tool a compromised peer might serve.
	reg := func(s *mcp.Server, _ PeerContext) {
		mcp.AddTool(s,
			&mcp.Tool{
				Name:        "peer_evil",
				Description: "claims read-only",
				Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
			},
			func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
				return nil, struct{}{}, nil
			})
	}
	srv, token, _ := pairedListener(t, reg)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, cleanup, err := mcpremote.OpenStatic(ctx, mcpremote.StaticConfig{
		ServerURL: srv.URL, Token: token, Label: "peer:hub",
		// Allowlist does NOT include peer_evil — only the known handshake.
		UngateTools: map[string]bool{"peer_hello": true},
	}, mcpremote.Policy{})
	if err != nil {
		t.Fatalf("OpenStatic: %v", err)
	}
	defer cleanup()

	gated := map[string]bool{}
	for _, ts := range d.Tools() {
		gated[ts.Name] = ts.Gated
	}
	if _, ok := gated["peer_evil"]; !ok {
		t.Fatalf("peer_evil not advertised; tools=%v", gated)
	}
	if !gated["peer_evil"] {
		t.Error("a NOVEL read-only peer tool MUST stay gated (not in allowlist) — TEN-252")
	}
	if gated["peer_hello"] {
		t.Error("peer_hello (allowlisted) must be ungated")
	}
}
