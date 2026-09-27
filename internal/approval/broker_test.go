package approval

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/control"
	"tenant/internal/runtrace"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testCategories stands in for the command's action tables.
var testCategories = map[string]string{
	"os_exec":      CatExec,
	"os_write":     CatWrite,
	"web_interact": CatWeb,
	"gsuite_send":  CatSend,
	"x_post":       CatSend,
}

func testCategorize(action string) string {
	return Categorize(action, func(a string) (string, bool) {
		c, ok := testCategories[a]
		return c, ok
	})
}

func newTestBroker(log *slog.Logger) *Broker { return NewBroker(log, testCategorize) }

func newTestDiscordBroker(log *slog.Logger) *Broker { return NewDiscordBroker(log, testCategorize) }

// allow/deny modes resolve without prompting; ask prompts.
func TestBroker_AllowDenyShortCircuit(t *testing.T) {
	b := newTestBroker(testLog())
	b.SetPermission(CatExec, "allow")
	b.SetPermission(CatSend, "deny")

	if !b.Confirm(context.Background(), "os_exec", "ls") {
		t.Fatal("allow mode must approve without prompting")
	}
	if b.Confirm(context.Background(), "gsuite_send", "email") {
		t.Fatal("deny mode must block without prompting")
	}
}

// drainOnce answers the next approval request with the given decision.
func drainOnce(b *Broker, d control.ApprovalDecision) {
	go func() {
		req := <-b.Requests()
		req.Reply <- d
	}()
}

func TestBroker_ApproveOnceDoesNotPersist(t *testing.T) {
	b := newTestBroker(testLog())
	drainOnce(b, control.ApproveOnce)
	if !b.Confirm(context.Background(), "os_exec_dangerous", "rm -rf x") {
		t.Fatal("approve once should allow this call")
	}
	// Still ask next time (no session/always grant).
	if b.modes[CatDestructive] != ModeAsk {
		t.Fatalf("approve once must not change the mode: %v", b.modes[CatDestructive])
	}
}

func TestBroker_ApproveSessionSkipsLaterPrompts(t *testing.T) {
	b := newTestBroker(testLog())
	drainOnce(b, control.ApproveSession)
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
	if b.modes[CatExec] != ModeAsk {
		t.Fatal("session grant must not flip the persisted mode")
	}
}

func TestBroker_ApproveAlwaysPersists(t *testing.T) {
	b := newTestBroker(testLog())
	var saved map[string]string
	b.SetPersist(func(m map[string]string) { saved = m })

	drainOnce(b, control.ApproveAlways)
	if !b.Confirm(context.Background(), "web_interact", "click X") {
		t.Fatal("approve always should allow")
	}
	if b.modes[CatWeb] != ModeAllow {
		t.Fatal("approve always must flip the category to allow")
	}
	if saved[CatWeb] != "allow" {
		t.Fatalf("approve always must persist: %v", saved)
	}
	// Subsequent calls are silent (allow mode).
	if !b.Confirm(context.Background(), "web_interact", "click Y") {
		t.Fatal("after always, web is allowed silently")
	}
}

func TestBroker_DenyDecisionBlocks(t *testing.T) {
	b := newTestBroker(testLog())
	drainOnce(b, control.DenyOnce)
	if b.Confirm(context.Background(), "os_exec", "whoami") {
		t.Fatal("deny decision must block the action")
	}
}

// TestDiscordBroker_AskBackend covers the TEN-231 pluggable "ask" backend: the
// Discord broker defaults every category to ASK and routes an ask-mode action to
// a custom backend (the button approver) instead of the TUI request channel —
// allow/deny still short-circuit; the backend's verdict drives the ask path.
func TestDiscordBroker_AskBackend(t *testing.T) {
	b := newTestDiscordBroker(testLog())
	// Default is ASK for every category (faithful to the pre-TEN-231 behavior).
	for _, c := range Categories {
		if b.modes[c] != ModeAsk {
			t.Fatalf("discord broker default for %s = %v, want ask", c, b.modes[c])
		}
	}

	var asked []string
	verdict := true
	b.SetAsk(func(_ context.Context, req control.ApprovalRequest) control.ApprovalDecision {
		asked = append(asked, req.Action)
		if verdict {
			return control.ApproveOnce
		}
		return control.DenyOnce
	})

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
	b.SetPermission(CatWrite, "allow")
	b.SetPermission(CatSend, "deny")
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
	b := newTestDiscordBroker(testLog()) // ask=nil, requests=nil
	if b.Confirm(context.Background(), "os_exec", "ls") {
		t.Fatal("ask mode with no backend must fail closed (deny)")
	}
}

func TestBroker_ContextCancelDenies(t *testing.T) {
	b := newTestBroker(testLog())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: Confirm must not block forever
	if b.Confirm(ctx, "os_exec", "ls") {
		t.Fatal("cancelled context must deny (fail safe)")
	}
}

func TestBroker_PermissionsControl(t *testing.T) {
	b := newTestBroker(testLog())
	if ok, err := b.SetPermission("nope", "allow"); ok || err != nil {
		t.Fatalf("unknown category: ok=%v err=%v", ok, err)
	}
	if _, err := b.SetPermission(CatExec, "weird"); err == nil {
		t.Fatal("bad mode should error")
	}
	ok, err := b.SetPermission(CatExec, "deny")
	if !ok || err != nil {
		t.Fatalf("set exec deny: ok=%v err=%v", ok, err)
	}
	for _, in := range b.Permissions() {
		if in.Category == CatExec && in.Mode != "deny" {
			t.Fatalf("Permissions should reflect set: %+v", in)
		}
	}
}

// confirmAsync runs Confirm on a goroutine and waits for it to register a
// pending request, returning the assigned id + a channel carrying the decision.
func confirmAsync(t *testing.T, b *Broker, ctx context.Context, action string) (string, <-chan bool) {
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
	b := newTestBroker(testLog())
	id, done := confirmAsync(t, b, context.Background(), "os_exec")
	if !b.Resolve(id, control.ApproveOnce) {
		t.Fatal("first Resolve should win (true)")
	}
	if b.Resolve(id, control.ApproveOnce) {
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

// Approval events raised inside a turn carry its trace ID, so the approvals
// lines join the run record (TEN-362).
func TestBroker_ApprovalEventsCarryTrace(t *testing.T) {
	b := newTestBroker(testLog())
	var mu sync.Mutex
	var got []string
	b.SetEvents(func(e agent.Event) { mu.Lock(); got = append(got, string(e.Kind)+"/"+e.TraceID); mu.Unlock() })
	ctx := runtrace.WithSpan(context.Background(), runtrace.NewSpan("4f2a"))
	id, done := confirmAsync(t, b, ctx, "os_exec")
	b.Resolve(id, control.ApproveOnce)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if s := strings.Join(got, ","); s != "approval_pending/4f2a,approval_resolved/4f2a" {
		t.Fatalf("events = %s", s)
	}
	// A Discord-button approval's mirror lines too.
	d := newTestDiscordBroker(testLog())
	var beside []string
	d.SetEvents(func(e agent.Event) { beside = append(beside, e.TraceID) })
	d.SetAsk(func(context.Context, control.ApprovalRequest) control.ApprovalDecision { return control.ApproveOnce })
	d.askDecision(ctx, CatExec, "os_exec", "x")
	if s := strings.Join(beside, ","); s != "4f2a,4f2a" {
		t.Fatalf("beside traces = %s", s)
	}
}

func TestBroker_FirstResolverWinsRace(t *testing.T) {
	b := newTestBroker(testLog())
	id, done := confirmAsync(t, b, context.Background(), "os_exec")
	const n = 8
	start := make(chan struct{})
	results := make(chan bool, n)
	for i := 0; i < n; i++ {
		go func() { <-start; results <- b.Resolve(id, control.ApproveOnce) }()
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
	b := newTestBroker(testLog())
	if b.Resolve("ap-does-not-exist", control.ApproveOnce) {
		t.Error("resolving an unknown id should return false")
	}
	if err := b.Decide("nope", "approve"); err == nil {
		t.Error("Decide on an unknown id should error")
	}
}

func TestBroker_CtxCancelDeniesAndCleansUp(t *testing.T) {
	b := newTestBroker(testLog())
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
	if b.Resolve(id, control.ApproveOnce) {
		t.Error("late Resolve of a cancelled id should be false")
	}
}

func TestBroker_DecideResolves(t *testing.T) {
	b := newTestBroker(testLog())
	id, done := confirmAsync(t, b, context.Background(), "os_exec")
	if err := b.Decide(id, "deny"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := <-done; got {
		t.Error("Decide(deny) should deny the action")
	}
}

func TestBroker_DenyAllDenies(t *testing.T) {
	b := newTestBroker(testLog())
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
	host := newTestBroker(testLog())
	off := NewOffsiteBroker(testLog(), host)
	if ok, err := off.SetPermission("exec", "ask"); !ok || err != nil { // offsite defaults to deny
		t.Fatalf("set offsite exec ask: ok=%v err=%v", ok, err)
	}
	id, done := confirmAsync(t, off, context.Background(), "os_exec")
	if p := host.Pending(); len(p) != 1 || p[0].ID != id {
		t.Fatalf("host should see the offsite pending request, got %+v", p)
	}
	if !host.Resolve(id, control.ApproveOnce) {
		t.Fatal("host.Resolve should resolve the shared offsite request")
	}
	if got := <-done; !got {
		t.Error("offsite Confirm should unblock approved via host.Resolve")
	}
}

// Per-tool rules are the operator's explicit word for one tool and beat the
// category on every broker: allow/deny decide silently, ask always prompts, and
// "approve always" on a rule-forced prompt flips the RULE, never the category.
func TestBroker_ToolRulesBeatCategories(t *testing.T) {
	ctx := context.Background()
	b := newTestBroker(testLog())
	b.rules = NewRules()
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
	_, _ = b.SetPermission(CatSend, "allow")
	_ = b.rules.Set("gsuite_send", "deny")
	if b.Confirm(ctx, "gsuite_send", "email") {
		t.Fatal("deny rule must block despite category allow")
	}

	// ask: forces a prompt even though the category is allow.
	_ = b.rules.Set("x_post", "ask")
	drainOnce(b, control.DenyOnce)
	if b.Confirm(ctx, "x_post", "tweet") {
		t.Fatal("ask rule must prompt; the operator denied")
	}

	// approve-always on a rule-forced prompt: the rule becomes allow, the
	// category is untouched (here exec is deny, and the rule still wins).
	_ = b.rules.Set("os_exec", "ask")
	_, _ = b.SetPermission(CatExec, "deny")
	drainOnce(b, control.ApproveAlways)
	if !b.Confirm(ctx, "os_exec", "ls") {
		t.Fatal("rule ask + approve always should allow this call")
	}
	if b.modes[CatExec] != ModeDeny {
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
	rb := newTestDiscordBroker(testLog())
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

// The host mirror of an iMessage-origin approval is labeled "imessage", not
// "discord" (the origin literal was hardcoded).
func TestOffsiteBroker_MirrorCarriesOwnOrigin(t *testing.T) {
	host := newTestBroker(nil)
	b := NewOffsiteBroker(nil, host)
	var ids []string
	b.SetEvents(func(e agent.Event) {
		if e.Approval != nil {
			ids = append(ids, e.Approval.ID+"/"+e.Approval.Origin)
		}
	})
	b.SetAsk(func(context.Context, control.ApprovalRequest) control.ApprovalDecision { return control.DenyOnce })
	if d := b.askDecision(context.Background(), CatExec, "run", "d"); d != control.DenyOnce {
		t.Fatalf("askDecision = %v", d)
	}
	if len(ids) != 2 || ids[0] != "beside-imessage/imessage" || ids[1] != "beside-imessage/imessage" {
		t.Fatalf("mirror events must carry the broker's own origin, got %v", ids)
	}
}

// The offsite broker denies every category until the operator opens one; the
// host and Discord brokers ask.
func TestBrokerDefaults(t *testing.T) {
	host := newTestBroker(testLog())
	for _, tc := range []struct {
		name, want string
		b          *Broker
	}{
		{"host", "ask", host},
		{"offsite", "deny", NewOffsiteBroker(testLog(), host)},
		{"discord", "ask", newTestDiscordBroker(testLog())},
	} {
		modes := tc.b.Modes()
		if len(modes) != len(Categories) {
			t.Errorf("%s broker has modes for %d categories", tc.name, len(modes))
		}
		for cat, mode := range modes {
			if mode != tc.want {
				t.Errorf("%s broker: %s = %s, want %s", tc.name, cat, mode, tc.want)
			}
		}
	}
}
