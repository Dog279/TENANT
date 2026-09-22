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

// Trust is the live per-connector decision: allow skips the gate entirely,
// deny refuses every call, ask (the default) keeps the deny-by-default gate.
func TestDispatcher_TrustModes(t *testing.T) {
	calls := 0
	d := &Dispatcher{label: "mcp:127.0.0.1:9000", gated: map[string]bool{"write_note": true, "echo": false},
		policy: Policy{Confirm: func(context.Context, string, string) bool { calls++; return true }}}
	ctx := context.Background()

	if d.Trust() != TrustAsk {
		t.Fatalf("default trust = %q, want ask", d.Trust())
	}
	if err := d.gateCall(ctx, "write_note"); err != nil || calls != 1 {
		t.Fatalf("ask: gated tool should prompt once: err=%v calls=%d", err, calls)
	}
	if err := d.gateCall(ctx, "echo"); err != nil || calls != 1 {
		t.Fatalf("ask: ungated tool must not prompt: err=%v calls=%d", err, calls)
	}

	d.SetTrust("allow")
	if err := d.gateCall(ctx, "write_note"); err != nil || calls != 1 {
		t.Fatalf("allow: no prompt expected: err=%v calls=%d", err, calls)
	}

	d.SetTrust("deny")
	err := d.gateCall(ctx, "echo")
	if err == nil || !strings.Contains(err.Error(), "set to deny") || calls != 1 {
		t.Fatalf("deny: every call refused without prompting: err=%v calls=%d", err, calls)
	}

	d.SetTrust("bogus") // ignored
	if d.Trust() != TrustDeny {
		t.Errorf("an invalid mode must not change trust, got %q", d.Trust())
	}
	for in, want := range map[string]string{"": "ask", "ALLOW": "allow", "trusted": "allow", "block": "deny", "prompt": "ask"} {
		if got, ok := ParseTrust(in); !ok || got != want {
			t.Errorf("ParseTrust(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	if _, ok := ParseTrust("maybe"); ok {
		t.Error("ParseTrust must reject unknown modes")
	}
}

// A per-tool rule is consulted before the connector's trust: deny beats a
// trusted connector, allow skips the gate under ask, ask prompts under allow.
func TestDispatcher_RulesBeforeTrust(t *testing.T) {
	calls := 0
	rules := map[string]string{"mcp:t:delete_all": "deny", "mcp:t:search": "allow", "mcp:t:write_note": "ask"}
	d := &Dispatcher{label: "mcp:t", gated: map[string]bool{"delete_all": true, "search": true, "write_note": true, "other": true},
		policy: Policy{
			Confirm: func(context.Context, string, string) bool { calls++; return true },
			Rule:    func(action string) string { return rules[action] },
		}}
	ctx := context.Background()

	d.SetTrust("allow")
	if err := d.gateCall(ctx, "delete_all"); err == nil || !strings.Contains(err.Error(), "denied by a tool rule") {
		t.Errorf("deny rule must beat trust allow: %v", err)
	}
	if err := d.gateCall(ctx, "write_note"); err != nil || calls != 1 {
		t.Errorf("ask rule must prompt even under trust allow: err=%v calls=%d", err, calls)
	}
	if err := d.gateCall(ctx, "other"); err != nil || calls != 1 {
		t.Errorf("no rule + trust allow: no prompt: err=%v calls=%d", err, calls)
	}

	d.SetTrust("ask")
	if err := d.gateCall(ctx, "search"); err != nil || calls != 1 {
		t.Errorf("allow rule must skip the gate under trust ask: err=%v calls=%d", err, calls)
	}
	if err := d.gateCall(ctx, "other"); err != nil || calls != 2 {
		t.Errorf("no rule + trust ask: gated tool prompts: err=%v calls=%d", err, calls)
	}

	d.SetTrust("deny")
	if err := d.gateCall(ctx, "search"); err == nil || !strings.Contains(err.Error(), "set to deny") {
		t.Errorf("connector deny still wins over an allow rule (the operator hid it): %v", err)
	}
}
