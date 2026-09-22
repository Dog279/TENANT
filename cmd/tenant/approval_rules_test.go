package main

import (
	"context"
	"testing"

	"tenant/internal/tui"
)

// Per-tool rules are the operator's explicit word for one tool and beat the
// category on every broker: allow/deny decide silently, ask always prompts, and
// "approve always" on a rule-forced prompt flips the RULE, never the category.
func TestBroker_ToolRulesBeatCategories(t *testing.T) {
	ctx := context.Background()
	b := newApprovalBroker(testLog())
	b.rules = newToolRules()
	var persisted map[string]string
	b.rules.persist = func(m map[string]string) { persisted = m }

	// allow: silent even though exec is "ask", and even for a connector tool
	// whose category is destructive (ask-only) — no prompt, no channel read.
	_ = b.rules.Set("os_exec", "allow")
	_ = b.rules.Set("mcp:127.0.0.1:9000:write_note", "allow")
	if !b.Confirm(ctx, "os_exec", "ls") {
		t.Fatal("allow rule must approve silently")
	}
	if !b.Confirm(ctx, "mcp:127.0.0.1:9000:write_note", "remote write") {
		t.Fatal("allow rule must approve a destructive-class connector tool silently")
	}

	// deny: blocks even when the category is allow.
	_, _ = b.SetPermission(catSend, "allow")
	_ = b.rules.Set("gsuite_send", "deny")
	if b.Confirm(ctx, "gsuite_send", "email") {
		t.Fatal("deny rule must block despite category allow")
	}

	// ask: forces a prompt even though the category is allow.
	_ = b.rules.Set("x_post", "ask")
	drainOnce(b, tui.DenyOnce)
	if b.Confirm(ctx, "x_post", "tweet") {
		t.Fatal("ask rule must prompt; the operator denied")
	}

	// approve-always on a rule-forced prompt: the rule becomes allow, the
	// category is untouched (here exec is deny, and the rule still wins).
	_ = b.rules.Set("os_exec", "ask")
	_, _ = b.SetPermission(catExec, "deny")
	drainOnce(b, tui.ApproveAlways)
	if !b.Confirm(ctx, "os_exec", "ls") {
		t.Fatal("rule ask + approve always should allow this call")
	}
	if b.modes[catExec] != modeDeny {
		t.Fatal("approve always on a rule prompt must not touch the category")
	}
	if persisted["os_exec"] != "allow" {
		t.Fatalf("the rule should now be allow and persisted: %v", persisted)
	}
	if !b.Confirm(ctx, "os_exec", "ls again") {
		t.Fatal("after always, the tool is allowed silently by its rule")
	}

	// A bare name matches the connector-qualified action id, and the set is
	// shared: the relay broker (its own categories) sees the same rules.
	_ = b.rules.Set("delete_thing", "deny")
	if b.Confirm(ctx, "mcp:h:1:delete_thing", "x") {
		t.Fatal("bare-name deny must match a connector's tool")
	}
	rb := newDiscordApprovalBroker(testLog())
	rb.rules = b.rules
	if rb.Confirm(ctx, "mcp:h:1:delete_thing", "x") {
		t.Fatal("the relay broker shares the rule set")
	}
	// The TUI's rule surface is the broker.
	if rules := b.Rules(); len(rules) == 0 {
		t.Fatal("Rules() should list the set")
	}
	if removed, _ := b.RemoveRule("delete_thing"); !removed {
		t.Fatal("RemoveRule")
	}
	if err := b.SetRule("bad rule", "allow"); err == nil {
		t.Fatal("SetRule must validate")
	}
}
