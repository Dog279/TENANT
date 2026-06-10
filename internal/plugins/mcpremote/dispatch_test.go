package mcpremote

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGatedTool_DenyByDefault(t *testing.T) {
	cases := []struct {
		readOnly, trust, wantGated bool
	}{
		{readOnly: false, trust: false, wantGated: true}, // unannotated, untrusted → gated
		{readOnly: true, trust: false, wantGated: true},  // read-only but untrusted → still gated
		{readOnly: false, trust: true, wantGated: true},  // trusted but a write → gated
		{readOnly: true, trust: true, wantGated: false},  // trusted + read-only → the ONLY ungated case
	}
	for _, c := range cases {
		if got := gatedTool(c.readOnly, c.trust); got != c.wantGated {
			t.Errorf("gatedTool(readOnly=%v, trust=%v) = %v, want %v", c.readOnly, c.trust, got, c.wantGated)
		}
	}
}

func TestGate_BlocksByDefault(t *testing.T) {
	d := &Dispatcher{label: "mcp:test", policy: Policy{}} // no AllowWrite, no Confirm
	if err := d.gate(context.Background(), "create_thing"); err == nil {
		t.Fatal("expected a write to be blocked with no AllowWrite and no Confirm")
	}
	// AllowWrite opens it.
	d.policy.AllowWrite = true
	if err := d.gate(context.Background(), "create_thing"); err != nil {
		t.Errorf("AllowWrite should permit: %v", err)
	}
}

func TestGate_ConfirmDecides(t *testing.T) {
	approved := &Dispatcher{label: "mcp:test", policy: Policy{Confirm: func(context.Context, string, string) bool { return true }}}
	if err := approved.gate(context.Background(), "create_thing"); err != nil {
		t.Errorf("approving Confirm should permit: %v", err)
	}
	denied := &Dispatcher{label: "mcp:test", policy: Policy{Confirm: func(context.Context, string, string) bool { return false }}}
	if err := denied.gate(context.Background(), "create_thing"); err == nil {
		t.Error("denying Confirm should block")
	}
}

func TestRenderContent(t *testing.T) {
	got := renderContent([]mcp.Content{
		&mcp.TextContent{Text: "line one"},
		&mcp.TextContent{Text: "line two"},
	})
	if !strings.Contains(got, "line one") || !strings.Contains(got, "line two") {
		t.Errorf("renderContent dropped text: %q", got)
	}
	if renderContent(nil) != "" {
		t.Error("nil content should render empty")
	}
}
