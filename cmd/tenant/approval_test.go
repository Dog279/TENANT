package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"tenant/internal/tui"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCategorize(t *testing.T) {
	cases := map[string]string{
		"os_exec":           catExec,
		"os_exec_dangerous": catDestructive,
		"sql_ddl":           catDestructive,
		"web_transact":      catDestructive,
		"web_interact":      catWeb,
		"web_click":         catWeb,
		"gsuite_send":       catSend,
		"imessage_send":     catSend,
		"x_post":            catSend,
		"something_unknown": catDestructive, // safest default
	}
	for action, want := range cases {
		if got := categorize(action); got != want {
			t.Errorf("categorize(%q)=%q want %q", action, got, want)
		}
	}
}

// allow/deny modes resolve without prompting; ask prompts.
func TestBroker_AllowDenyShortCircuit(t *testing.T) {
	b := newApprovalBroker(testLog())
	b.SetPermission(catExec, "allow")
	b.SetPermission(catSend, "deny")

	if !b.Confirm(context.Background(), "os_exec", "ls") {
		t.Fatal("allow mode must approve without prompting")
	}
	if b.Confirm(context.Background(), "gsuite_send", "email") {
		t.Fatal("deny mode must block without prompting")
	}
}

// drainOnce answers the next approval request with the given decision.
func drainOnce(b *approvalBroker, d tui.ApprovalDecision) {
	go func() {
		req := <-b.Requests()
		req.Reply <- d
	}()
}

func TestBroker_ApproveOnceDoesNotPersist(t *testing.T) {
	b := newApprovalBroker(testLog())
	drainOnce(b, tui.ApproveOnce)
	if !b.Confirm(context.Background(), "os_exec_dangerous", "rm -rf x") {
		t.Fatal("approve once should allow this call")
	}
	// Still ask next time (no session/always grant).
	if b.modes[catDestructive] != modeAsk {
		t.Fatalf("approve once must not change the mode: %v", b.modes[catDestructive])
	}
}

func TestBroker_ApproveSessionSkipsLaterPrompts(t *testing.T) {
	b := newApprovalBroker(testLog())
	drainOnce(b, tui.ApproveSession)
	if !b.Confirm(context.Background(), "os_exec", "python a.py") {
		t.Fatal("approve session should allow")
	}
	// Second call in the same category must NOT prompt (would block here
	// since nothing drains Requests) — a short timeout proves it returned.
	done := make(chan bool, 1)
	go func() { done <- b.Confirm(context.Background(), "os_exec", "python b.py") }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("session grant should approve silently")
		}
	case <-time.After(time.Second):
		t.Fatal("session grant did not skip the prompt (it blocked)")
	}
	// Session grant is not persisted as a mode change.
	if b.modes[catExec] != modeAsk {
		t.Fatal("session grant must not flip the persisted mode")
	}
}

func TestBroker_ApproveAlwaysPersists(t *testing.T) {
	b := newApprovalBroker(testLog())
	var saved map[string]string
	b.persist = func(m map[string]string) { saved = m }

	drainOnce(b, tui.ApproveAlways)
	if !b.Confirm(context.Background(), "web_interact", "click X") {
		t.Fatal("approve always should allow")
	}
	if b.modes[catWeb] != modeAllow {
		t.Fatal("approve always must flip the category to allow")
	}
	if saved[catWeb] != "allow" {
		t.Fatalf("approve always must persist: %v", saved)
	}
	// Subsequent calls are silent (allow mode).
	if !b.Confirm(context.Background(), "web_click", "click Y") {
		t.Fatal("after always, web is allowed silently")
	}
}

func TestBroker_DenyDecisionBlocks(t *testing.T) {
	b := newApprovalBroker(testLog())
	drainOnce(b, tui.DenyOnce)
	if b.Confirm(context.Background(), "os_exec", "whoami") {
		t.Fatal("deny decision must block the action")
	}
}

func TestBroker_ContextCancelDenies(t *testing.T) {
	b := newApprovalBroker(testLog())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: Confirm must not block forever
	if b.Confirm(ctx, "os_exec", "ls") {
		t.Fatal("cancelled context must deny (fail safe)")
	}
}

func TestBroker_SeedAndLoadModes(t *testing.T) {
	b := newApprovalBroker(testLog())
	b.seedFromFlags(&pluginFlags{osAllowExec: true, gsuiteAllowSend: true})
	if b.modes[catExec] != modeAllow || b.modes[catSend] != modeAllow {
		t.Fatal("flags should seed allow")
	}
	if b.modes[catDestructive] != modeAsk {
		t.Fatal("destructive must never be flag-seeded to allow")
	}
	// Persisted modes override flags.
	b.loadModes(map[string]string{"exec": "deny", "web": "allow", "bogus": "allow"})
	if b.modes[catExec] != modeDeny || b.modes[catWeb] != modeAllow {
		t.Fatal("loadModes should override")
	}
	if _, ok := b.modes["bogus"]; ok {
		t.Fatal("unknown categories must be ignored")
	}
}

func TestBroker_PermissionsControl(t *testing.T) {
	b := newApprovalBroker(testLog())
	if ok, err := b.SetPermission("nope", "allow"); ok || err != nil {
		t.Fatalf("unknown category: ok=%v err=%v", ok, err)
	}
	if _, err := b.SetPermission(catExec, "weird"); err == nil {
		t.Fatal("bad mode should error")
	}
	ok, err := b.SetPermission(catExec, "deny")
	if !ok || err != nil {
		t.Fatalf("set exec deny: ok=%v err=%v", ok, err)
	}
	for _, in := range b.Permissions() {
		if in.Category == catExec && in.Mode != "deny" {
			t.Fatalf("Permissions should reflect set: %+v", in)
		}
	}
}
