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

// TestDiscordBroker_AskBackend covers the TEN-231 pluggable "ask" backend: the
// Discord broker defaults every category to ASK and routes an ask-mode action to
// a custom backend (the button approver) instead of the TUI request channel —
// allow/deny still short-circuit; the backend's verdict drives the ask path.
func TestDiscordBroker_AskBackend(t *testing.T) {
	b := newDiscordApprovalBroker(testLog())
	// Default is ASK for every category (faithful to the pre-TEN-231 behavior).
	for _, c := range catOrder {
		if b.modes[c] != modeAsk {
			t.Fatalf("discord broker default for %s = %v, want ask", c, b.modes[c])
		}
	}

	var asked []string
	verdict := true
	b.ask = func(_ context.Context, req tui.ApprovalRequest) tui.ApprovalDecision {
		asked = append(asked, req.Action)
		if verdict {
			return tui.ApproveOnce
		}
		return tui.DenyOnce
	}

	// ask-mode + backend approves → action runs, backend was consulted.
	if !b.Confirm(context.Background(), "os_exec", "ls") {
		t.Fatal("ask backend approved but Confirm denied")
	}
	// ask-mode + backend denies → blocked.
	verdict = false
	if b.Confirm(context.Background(), "os_exec", "rm") {
		t.Fatal("ask backend denied but Confirm approved")
	}
	if len(asked) != 2 {
		t.Fatalf("backend consulted %d times, want 2: %v", len(asked), asked)
	}

	// allow/deny modes must NOT consult the backend (short-circuit).
	before := len(asked)
	b.SetPermission(catWrite, "allow")
	b.SetPermission(catSend, "deny")
	if !b.Confirm(context.Background(), "os_write", "f") {
		t.Fatal("allow must approve without the backend")
	}
	if b.Confirm(context.Background(), "gsuite_send", "mail") {
		t.Fatal("deny must block without the backend")
	}
	if len(asked) != before {
		t.Fatal("allow/deny must not consult the ask backend")
	}
}

// TestDiscordBroker_FailsClosed: an ask-mode action with NO backend and NO
// request channel must deny (never silently allow when the operator is unreachable).
func TestDiscordBroker_FailsClosed(t *testing.T) {
	b := newDiscordApprovalBroker(testLog()) // ask=nil, requests=nil
	if b.Confirm(context.Background(), "os_exec", "ls") {
		t.Fatal("ask mode with no backend must fail closed (deny)")
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

// --- TEN-203: id-keyed fan-out (Pending/Resolve, multi-observer) ---

// confirmAsync runs Confirm on a goroutine and waits for it to register a
// pending request, returning the assigned id + a channel carrying the decision.
func confirmAsync(t *testing.T, b *approvalBroker, ctx context.Context, action string) (string, <-chan bool) {
	t.Helper()
	done := make(chan bool, 1)
	go func() { done <- b.Confirm(ctx, action, "do "+action) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p := b.Pending(); len(p) == 1 {
			return p[0].ID, done
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("Confirm never registered a pending request for %q", action)
	return "", nil
}

func TestBroker_ResolveUnblocksOnce(t *testing.T) {
	b := newApprovalBroker(testLog())
	id, done := confirmAsync(t, b, context.Background(), "os_exec")
	if !b.Resolve(id, tui.ApproveOnce) {
		t.Fatal("first Resolve should win (true)")
	}
	if b.Resolve(id, tui.ApproveOnce) {
		t.Error("second Resolve of the same id should be a no-op (false)")
	}
	select {
	case got := <-done:
		if !got {
			t.Error("Confirm should return true after ApproveOnce")
		}
	case <-time.After(time.Second):
		t.Fatal("Confirm did not unblock after Resolve")
	}
	if p := b.Pending(); len(p) != 0 {
		t.Errorf("pending should be empty after resolve, got %d", len(p))
	}
}

func TestBroker_FirstResolverWinsRace(t *testing.T) {
	b := newApprovalBroker(testLog())
	id, done := confirmAsync(t, b, context.Background(), "os_exec")
	const n = 8
	start := make(chan struct{})
	results := make(chan bool, n)
	for i := 0; i < n; i++ {
		go func() { <-start; results <- b.Resolve(id, tui.ApproveOnce) }()
	}
	close(start)
	wins := 0
	for i := 0; i < n; i++ {
		if <-results {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("exactly one resolver should win, got %d", wins)
	}
	if got := <-done; !got {
		t.Error("agent should unblock approved exactly once")
	}
}

func TestBroker_ResolveUnknownIsNoop(t *testing.T) {
	b := newApprovalBroker(testLog())
	if b.Resolve("ap-does-not-exist", tui.ApproveOnce) {
		t.Error("resolving an unknown id should return false")
	}
	if err := b.Decide("nope", "approve"); err == nil {
		t.Error("Decide on an unknown id should error")
	}
}

func TestBroker_CtxCancelDeniesAndCleansUp(t *testing.T) {
	b := newApprovalBroker(testLog())
	ctx, cancel := context.WithCancel(context.Background())
	id, done := confirmAsync(t, b, ctx, "os_exec")
	cancel()
	select {
	case got := <-done:
		if got {
			t.Error("ctx cancel must DENY (return false)")
		}
	case <-time.After(time.Second):
		t.Fatal("Confirm did not unblock on ctx cancel")
	}
	if p := b.Pending(); len(p) != 0 {
		t.Errorf("cancelled request should be dropped from pending, got %d", len(p))
	}
	if b.Resolve(id, tui.ApproveOnce) {
		t.Error("late Resolve of a cancelled id should be false")
	}
}

func TestBroker_DecideResolves(t *testing.T) {
	b := newApprovalBroker(testLog())
	id, done := confirmAsync(t, b, context.Background(), "os_exec")
	if err := b.Decide(id, "deny"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := <-done; got {
		t.Error("Decide(deny) should deny the action")
	}
}

func TestBroker_DenyAllDenies(t *testing.T) {
	b := newApprovalBroker(testLog())
	_, done := confirmAsync(t, b, context.Background(), "os_exec")
	b.DenyAll()
	if got := <-done; got {
		t.Error("DenyAll should deny the pending action")
	}
	if p := b.Pending(); len(p) != 0 {
		t.Errorf("DenyAll should clear pending, got %d", len(p))
	}
}

// The iMessage offsite broker SHARES the host registry: an offsite-raised
// approval is visible in + resolvable through the host (TEN-203 critical fix).
func TestBroker_OffsiteSharesRegistry(t *testing.T) {
	host := newApprovalBroker(testLog())
	off := newOffsiteApprovalBroker(testLog(), host)
	if ok, err := off.SetPermission("exec", "ask"); !ok || err != nil { // offsite defaults to deny
		t.Fatalf("set offsite exec ask: ok=%v err=%v", ok, err)
	}
	id, done := confirmAsync(t, off, context.Background(), "os_exec")
	if p := host.Pending(); len(p) != 1 || p[0].ID != id {
		t.Fatalf("host should see the offsite pending request, got %+v", p)
	}
	if !host.Resolve(id, tui.ApproveOnce) {
		t.Fatal("host.Resolve should resolve the shared offsite request")
	}
	if got := <-done; !got {
		t.Error("offsite Confirm should unblock approved via host.Resolve")
	}
}
