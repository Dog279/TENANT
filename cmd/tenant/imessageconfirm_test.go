package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/plugins/imessage"
	"tenant/internal/tui"
)

// fakeApproveSender records the texts the approver sends (prompt + echo) and can
// be made to fail. Safe for concurrent use (Confirm posts from one goroutine, a
// test resolver from another).
type fakeApproveSender struct {
	mu   sync.Mutex
	sent []string // "chat|text"
	err  error
}

func (f *fakeApproveSender) SendText(_ context.Context, chatGUID, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.sent = append(f.sent, chatGUID+"|"+text)
	return "ok", nil
}

func (f *fakeApproveSender) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	copy(out, f.sent)
	return out
}

// promptNonces extracts every nonce from the approver's prompt texts, in order.
func promptNonces(s *fakeApproveSender) []string {
	var out []string
	for _, line := range s.texts() {
		if i := strings.Index(line, "Reply  Y "); i >= 0 {
			fields := strings.Fields(line[i+len("Reply  Y "):])
			if len(fields) > 0 {
				out = append(out, fields[0])
			}
		}
	}
	return out
}

// firstPromptNonce blocks (briefly) until the approver has texted its FIRST
// prompt, then returns its nonce. The prompt shape is "... Reply  Y <nonce> ...".
func firstPromptNonce(t *testing.T, ap *imessageApprover, s *fakeApproveSender) string {
	t.Helper()
	return waitForNonce(t, s, 1)
}

// waitForNonce blocks until at least `want` prompts have been sent, then returns
// the want-th nonce (1-indexed). Polling the SEND (not just the pending count)
// closes the register-then-post window so the prompt text is guaranteed present.
func waitForNonce(t *testing.T, s *fakeApproveSender, want int) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ns := promptNonces(s); len(ns) >= want {
			return ns[want-1]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("prompt #%d was never sent: %v", want, s.texts())
	return ""
}

const opHandle = "+1 (555) 010-0000" // normalizes to 15550100000

// --- construction / fail-closed gates ---

func TestIMessageApprover_NoOperatorIsNil(t *testing.T) {
	// No operator handle ⇒ nil approver ⇒ caller keeps Phase-1 deny-all.
	if ap := newIMessageApprover(&fakeApproveSender{}, "", nil); ap != nil {
		t.Fatal("empty operator must yield a nil approver (Phase-1 default)")
	}
	if ap := newIMessageApprover(&fakeApproveSender{}, "   ", nil); ap != nil {
		t.Fatal("whitespace operator must yield a nil approver")
	}
	if ap := newIMessageApprover(nil, opHandle, nil); ap != nil {
		t.Fatal("nil sender must yield a nil approver (cannot prompt ⇒ no Phase-2)")
	}
}

func TestIMessageApprover_NilApproverSafe(t *testing.T) {
	// A nil approver's OnReply is a clean no-op (the Phase-1 responder path).
	var ap *imessageApprover
	if ap.OnReply("Y abc123", opHandle) {
		t.Fatal("nil approver OnReply must return false (not handled)")
	}
	if ap.PendingCount() != 0 {
		t.Fatal("nil approver PendingCount must be 0")
	}
}

func TestIMessageApprover_ConfirmDeniesWithoutChat(t *testing.T) {
	// FAIL-CLOSED: no chat pinned ⇒ cannot reach the operator ⇒ deny immediately
	// (and register nothing).
	ap := newIMessageApprover(&fakeApproveSender{}, opHandle, nil)
	if ap.Confirm(context.Background(), "os_exec", "rm -rf /") {
		t.Fatal("Confirm must deny when no chat is pinned (cannot prompt)")
	}
	if ap.PendingCount() != 0 {
		t.Fatal("a denied-before-register Confirm must leave no pending entry")
	}
}

// --- happy path: operator approves / denies ---

func TestIMessageApprover_OperatorApproves(t *testing.T) {
	s := &fakeApproveSender{}
	ap := newIMessageApprover(s, opHandle, nil)
	ap.setChat("chatOP")

	done := make(chan bool, 1)
	go func() { done <- ap.Confirm(context.Background(), "os_exec", "ls -la") }()

	nonce := firstPromptNonce(t, ap, s)
	// Operator replies "Y <nonce>" — handled + approves.
	if !ap.OnReply("Y "+nonce, opHandle) {
		t.Fatal("operator 'Y <nonce>' must be handled")
	}
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("operator Y must APPROVE the action")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Confirm did not return after approval")
	}
	if ap.PendingCount() != 0 {
		t.Fatal("the pending entry must be gone after resolve (single-use)")
	}
}

func TestIMessageApprover_OperatorDenies(t *testing.T) {
	s := &fakeApproveSender{}
	ap := newIMessageApprover(s, opHandle, nil)
	ap.setChat("chatOP")

	done := make(chan bool, 1)
	go func() { done <- ap.Confirm(context.Background(), "os_exec", "ls") }()
	nonce := firstPromptNonce(t, ap, s)

	if !ap.OnReply("deny "+nonce, opHandle) {
		t.Fatal("operator 'deny <nonce>' must be handled")
	}
	select {
	case ok := <-done:
		if ok {
			t.Fatal("operator deny must DENY the action")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Confirm did not return after denial")
	}
}

// --- SECURITY: operator-handle gate ---

func TestIMessageApprover_StrangerCannotApprove(t *testing.T) {
	s := &fakeApproveSender{}
	ap := newIMessageApprover(s, opHandle, nil)
	ap.setChat("chatOP")

	done := make(chan bool, 1)
	go func() { done <- ap.Confirm(context.Background(), "os_exec", "danger") }()
	nonce := firstPromptNonce(t, ap, s)

	// A stranger replies with the EXACT correct nonce — must NOT be handled and
	// must NOT approve. (Even a leaked/guessed nonce is useless without the handle.)
	if ap.OnReply("Y "+nonce, "+15559999999") {
		t.Fatal("a non-operator reply must NOT be handled")
	}
	if ap.OnReply("yes "+nonce, "stranger@evil.com") {
		t.Fatal("a non-operator email reply must NOT be handled")
	}
	// The action is still pending (nothing resolved it).
	if ap.PendingCount() != 1 {
		t.Fatalf("stranger replies must not resolve the pending action, count=%d", ap.PendingCount())
	}

	// And now the real operator approves — proving the nonce was never consumed.
	if !ap.OnReply("Y "+nonce, opHandle) {
		t.Fatal("operator reply (same nonce) must still work after stranger attempts")
	}
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("operator must approve after stranger attempts failed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Confirm did not return")
	}
}

func TestIMessageApprover_OperatorHandleNormalized(t *testing.T) {
	// The operator gate compares CANONICAL handles: a differently-formatted but
	// equivalent handle from the operator still approves.
	s := &fakeApproveSender{}
	ap := newIMessageApprover(s, opHandle, nil) // configured as "+1 (555) 010-0000"
	ap.setChat("chatOP")

	done := make(chan bool, 1)
	go func() { done <- ap.Confirm(context.Background(), "os_exec", "x") }()
	nonce := firstPromptNonce(t, ap, s)

	// chat.db would surface the sender as the bare E.164 "+15550100000".
	if !ap.OnReply("Y "+nonce, "+15550100000") {
		t.Fatal("an equivalently-normalized operator handle must approve")
	}
	if ok := <-done; !ok {
		t.Fatal("normalized operator reply must approve")
	}
}

// --- SECURITY: nonce mismatch / single-use ---

func TestIMessageApprover_NonceMismatchDenies(t *testing.T) {
	s := &fakeApproveSender{}
	ap := newIMessageApprover(s, opHandle, nil)
	ap.setChat("chatOP")

	done := make(chan bool, 1)
	go func() { done <- ap.Confirm(context.Background(), "os_exec", "x") }()
	_ = firstPromptNonce(t, ap, s)

	// Operator replies with a WRONG nonce: OnReply still returns handled=true (the
	// operator owns approval-shaped traffic, so we short-circuit), but the real
	// pending entry is NOT resolved — it stays pending and later times out / waits.
	if !ap.OnReply("Y WRONGNONCE", opHandle) {
		t.Fatal("an approval-shaped operator reply is consumed even on a bad nonce")
	}
	if ap.PendingCount() != 1 {
		t.Fatalf("a mismatched nonce must NOT resolve the real pending entry, count=%d", ap.PendingCount())
	}
	// Confirm is still blocked (not resolved).
	select {
	case <-done:
		t.Fatal("Confirm must NOT return on a nonce mismatch")
	case <-time.After(50 * time.Millisecond):
	}
	// Clean up: cancel via the right nonce path would need the nonce; instead deny
	// the real one by timing it out is slow, so resolve it to release the goroutine.
	// Grab the real nonce and approve to unblock.
	nonce := firstPromptNonce(t, ap, s)
	ap.OnReply("deny "+nonce, opHandle)
	<-done
}

func TestIMessageApprover_NonceSingleUse(t *testing.T) {
	s := &fakeApproveSender{}
	ap := newIMessageApprover(s, opHandle, nil)
	ap.setChat("chatOP")

	done := make(chan bool, 1)
	go func() { done <- ap.Confirm(context.Background(), "os_exec", "x") }()
	nonce := firstPromptNonce(t, ap, s)

	if !ap.OnReply("Y "+nonce, opHandle) {
		t.Fatal("first approval must be handled")
	}
	<-done
	// Replaying the SAME nonce must not resolve anything (it's deleted). It's still
	// "handled" (operator approval-shaped traffic), but no pending entry exists.
	if ap.PendingCount() != 0 {
		t.Fatal("nonce must be deleted after first use")
	}
	// A replay does not panic / double-send on a closed-or-missing entry.
	_ = ap.OnReply("Y "+nonce, opHandle)
}

// --- SECURITY: reply shapes ---

func TestIMessageApprover_NonApprovalIsNormalTurn(t *testing.T) {
	s := &fakeApproveSender{}
	ap := newIMessageApprover(s, opHandle, nil)
	ap.setChat("chatOP")
	// Pending a request so OnReply has something to (not) match.
	go func() { _ = ap.Confirm(context.Background(), "os_exec", "x") }()
	nonce := firstPromptNonce(t, ap, s)

	// These are NOT approval-shaped (wrong field count or no nonce) ⇒ a normal turn.
	cases := []string{
		"what's the weather?", // ordinary message (3+ fields)
		"Y",                   // bare yes, no nonce (1 field)
		"yes",                 // bare yes (1 field)
		"Y " + nonce + " now", // trailing chatter (3 fields)
		"",                    // empty
		"   ",                 // whitespace
	}
	for _, c := range cases {
		if ap.OnReply(c, opHandle) {
			t.Errorf("non-approval message %q must NOT be handled (it's a normal turn)", c)
		}
	}
	// A 2-field approval-shaped message with a BAD nonce (e.g. "approve everything")
	// IS consumed (handled=true) so a typo'd approval never leaks to the agent —
	// but it resolves NOTHING (the real nonce stays pending). This can only ever
	// fail-to-approve, never approve the wrong action.
	if !ap.OnReply("approve everything", opHandle) {
		t.Error("a 2-field approval-shaped operator reply should be consumed (handled)")
	}
	if ap.PendingCount() != 1 {
		t.Fatalf("a bad-nonce approval must not resolve the real pending action, count=%d", ap.PendingCount())
	}
	ap.OnReply("deny "+nonce, opHandle) // clean up
}

// --- SECURITY: fail-closed on timeout + ctx cancel ---

func TestIMessageApprover_TimeoutDenies(t *testing.T) {
	s := &fakeApproveSender{}
	ap := newIMessageApprover(s, opHandle, nil)
	ap.timeout = 30 * time.Millisecond // shrink for the test
	ap.setChat("chatOP")

	start := time.Now()
	if ap.Confirm(context.Background(), "os_exec", "x") {
		t.Fatal("Confirm must DENY when no reply arrives before the timeout")
	}
	if time.Since(start) < 25*time.Millisecond {
		t.Fatal("Confirm returned before the timeout elapsed")
	}
	if ap.PendingCount() != 0 {
		t.Fatal("the pending entry must be cleaned up after a timeout")
	}
}

func TestIMessageApprover_CtxCancelDenies(t *testing.T) {
	s := &fakeApproveSender{}
	ap := newIMessageApprover(s, opHandle, nil)
	ap.timeout = 10 * time.Second // long, so the cancel (not timeout) wins
	ap.setChat("chatOP")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- ap.Confirm(ctx, "os_exec", "x") }()
	_ = firstPromptNonce(t, ap, s)
	cancel() // turn aborted / process teardown

	select {
	case ok := <-done:
		if ok {
			t.Fatal("Confirm must DENY on ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Confirm did not return after ctx cancel")
	}
}

// --- nonce quality ---

func TestIMessageApprover_NonceUnguessableAndUnique(t *testing.T) {
	s := &fakeApproveSender{}
	ap := newIMessageApprover(s, opHandle, nil)
	ap.setChat("chatOP")
	ap.timeout = 5 * time.Second

	seen := map[string]struct{}{}
	for i := 0; i < 50; i++ {
		go func() { _ = ap.Confirm(context.Background(), "os_exec", "x") }()
		n := waitForNonce(t, s, i+1) // the (i+1)-th distinct prompt
		if len(n) < 6 {
			t.Fatalf("nonce too short to be unguessable: %q", n)
		}
		if _, dup := seen[n]; dup {
			t.Fatalf("nonce collision (not from crypto/rand?): %q", n)
		}
		seen[n] = struct{}{}
	}
	// Resolve them all so goroutines exit (deny each by its nonce).
	for n := range seen {
		ap.OnReply("deny "+n, opHandle)
	}
}

// --- SECURITY: origin isolation ---
//
// Two pending approvals exist at once: one in the iMessage approver, one in the
// LOCAL broker's shared registry (the on-host TUI/dashboard path). A phone "Y
// <nonce>" must resolve ONLY the iMessage one and NEVER the local registry entry.
func TestIMessageApprover_OriginIsolation(t *testing.T) {
	s := &fakeApproveSender{}
	ap := newIMessageApprover(s, opHandle, nil)
	ap.setChat("chatOP")

	// A LOCAL broker with a pending "ask" (blocks on the TUI request channel /
	// registry; nobody drains it here, so it stays pending).
	local := newApprovalBroker(nil)
	localDone := make(chan bool, 1)
	go func() {
		// catExec defaults to "ask" ⇒ registers in the shared registry and blocks.
		localDone <- local.Confirm(context.Background(), "os_exec", "local danger")
	}()
	// Wait for the local broker to register its pending entry.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(local.Pending()) == 0 {
		time.Sleep(time.Millisecond)
	}
	if len(local.Pending()) != 1 {
		t.Fatalf("local broker should have 1 pending, got %d", len(local.Pending()))
	}
	localID := local.Pending()[0].ID

	// An iMessage gated action is pending too.
	imsgDone := make(chan bool, 1)
	go func() { imsgDone <- ap.Confirm(context.Background(), "os_exec", "phone danger") }()
	nonce := firstPromptNonce(t, ap, s)

	// The phone Y carries the iMessage approver's nonce. Crucially that nonce is a
	// crypto/rand token, NOT the local registry id ("ap-1"). Feeding it to OnReply
	// resolves the iMessage approval and CANNOT touch the local registry.
	if !ap.OnReply("Y "+nonce, opHandle) {
		t.Fatal("phone Y must resolve the iMessage approval")
	}
	if ok := <-imsgDone; !ok {
		t.Fatal("iMessage approval must be approved by the phone Y")
	}

	// The LOCAL broker's pending entry is UNTOUCHED — the phone never reached it.
	if len(local.Pending()) != 1 || local.Pending()[0].ID != localID {
		t.Fatalf("local pending must be untouched by a phone Y, got %v", local.Pending())
	}
	select {
	case <-localDone:
		t.Fatal("the local Confirm must NOT have resolved from a phone reply")
	case <-time.After(50 * time.Millisecond):
	}

	// Belt-and-suspenders: even if the phone reply literally CARRIED the local
	// registry id, OnReply parses it as a nonce against the iMessage map only —
	// it has no code path into the local registry. Confirm it's not handled here
	// (the id "ap-1" alone is not an approval-shaped 2-field reply), and the local
	// entry remains.
	if ap.OnReply(localID, opHandle) {
		t.Fatal("a bare local-registry id must not be a valid iMessage approval reply")
	}
	if len(local.Pending()) != 1 {
		t.Fatalf("local pending still untouched, got %d", len(local.Pending()))
	}

	// Clean up the local goroutine.
	local.DenyAll()
	<-localDone
}

// --- responder integration: an approval reply is consumed, not run as a turn ---

func TestIMessageResponder_ConsumesApprovalReply(t *testing.T) {
	s := &fakeApproveSender{}
	ap := newIMessageApprover(s, opHandle, nil)
	ap.setChat("chatOP")
	// A pending gated action is waiting for the operator.
	confirmed := make(chan bool, 1)
	go func() { confirmed <- ap.Confirm(context.Background(), "os_exec", "x") }()
	nonce := firstPromptNonce(t, ap, s)

	// The responder receives "Y <nonce>" FROM THE OPERATOR. It must consume it via
	// OnReply and NOT drive an agent turn.
	p := &fakeIMsgPoller{msgs: []imessage.InboundMessage{fromInbound("chatOP", opHandle, "Y "+nonce)}}
	out := &fakeIMsgSender{}
	r := &fakeIMsgRunner{reply: "should not run"}
	resp := &imessageResponder{poller: p, sender: out, runner: r, confirm: denyAllConfirm, approver: ap}

	resp.drain(context.Background())

	if r.turns != 0 {
		t.Fatalf("an approval reply must NOT drive an agent turn, turns=%d", r.turns)
	}
	if ok := <-confirmed; !ok {
		t.Fatal("the pending gated action must be approved by the operator's Y")
	}
	// The responder itself sent nothing (the approver echoed the outcome).
	if len(out.sent) != 0 {
		t.Fatalf("the responder should not send its own reply for a consumed approval, got %v", out.sent)
	}
}

func TestIMessageResponder_NonOperatorReplyRunsAsTurn(t *testing.T) {
	s := &fakeApproveSender{}
	ap := newIMessageApprover(s, opHandle, nil)
	ap.setChat("chatOP")
	go func() { _ = ap.Confirm(context.Background(), "os_exec", "x") }()
	nonce := firstPromptNonce(t, ap, s)

	// A STRANGER (allowlisted to chat, but not the operator) texts "Y <nonce>".
	// OnReply returns false ⇒ the responder treats it as a normal turn. The gated
	// action stays pending (the stranger can't approve it).
	p := &fakeIMsgPoller{msgs: []imessage.InboundMessage{fromInbound("chatX", "+15557654321", "Y "+nonce)}}
	out := &fakeIMsgSender{}
	r := &fakeIMsgRunner{reply: "answered"}
	resp := &imessageResponder{poller: p, sender: out, runner: r, confirm: denyAllConfirm, approver: ap}

	resp.drain(context.Background())

	if r.turns != 1 {
		t.Fatalf("a non-operator 'Y <nonce>' must run as a normal turn, turns=%d", r.turns)
	}
	if ap.PendingCount() != 1 {
		t.Fatalf("the stranger must not have resolved the pending action, count=%d", ap.PendingCount())
	}
}

// fromInbound builds an InboundMessage carrying a sender handle (From) — the
// field the operator-handle gate reads.
func fromInbound(chat, from, text string) imessage.InboundMessage {
	m := inbound(chat, text)
	m.From = from
	return m
}

// --- SECURITY: broker composition (deny-by-default still wins) ---
//
// Even with a Phase-2 operator wired as the broker's "ask" backend, a category
// at the offsite default (deny) is refused WITHOUT texting the operator. Only a
// category explicitly set to "ask" routes to the text handshake. This proves the
// strongest fail-closed posture: configuring an operator does not silently open
// any category.
func TestIMessageApprover_BrokerDenyByDefault(t *testing.T) {
	s := &fakeApproveSender{}
	ap := newIMessageApprover(s, opHandle, nil)
	ap.setChat("chatOP")

	host := newApprovalBroker(nil)
	br := newOffsiteApprovalBroker(nil, host) // all categories default DENY
	br.ask = func(ctx context.Context, req tui.ApprovalRequest) tui.ApprovalDecision {
		if ap.Confirm(ctx, req.Action, req.Detail) {
			return tui.ApproveOnce
		}
		return tui.DenyOnce
	}

	// catExec defaults to deny offsite ⇒ refused without reaching the approver.
	if br.Confirm(context.Background(), "os_exec", "rm -rf /") {
		t.Fatal("a deny-default category must be refused offsite")
	}
	if ap.PendingCount() != 0 || len(s.texts()) != 0 {
		t.Fatalf("deny-default must NOT text the operator: pending=%d sent=%v", ap.PendingCount(), s.texts())
	}

	// Operator opens exec to "ask" ⇒ now it routes to the text handshake.
	if _, err := br.SetPermission(catExec, "ask"); err != nil {
		t.Fatalf("SetPermission: %v", err)
	}
	done := make(chan bool, 1)
	go func() { done <- br.Confirm(context.Background(), "os_exec", "ls") }()
	nonce := waitForNonce(t, s, 1) // the approver texted the operator
	if !ap.OnReply("Y "+nonce, opHandle) {
		t.Fatal("operator Y should resolve the ask-routed approval")
	}
	if ok := <-done; !ok {
		t.Fatal("an ask-routed category must approve on operator Y")
	}
}

// --- parse helper ---

func TestParseIMsgApprovalReply(t *testing.T) {
	approve := map[string]bool{"y": true, "yes": true, "approve": true}
	deny := map[string]bool{"n": true, "no": true, "deny": true}
	for _, in := range []string{"Y abc", "yes ABC", "APPROVE x9", "n abc", "No abc", "DENY q"} {
		verb, nonce, ok := parseIMsgApprovalReply(in)
		if !ok || nonce == "" {
			t.Fatalf("%q should parse as an approval reply", in)
		}
		if !approve[verb] && !deny[verb] {
			t.Fatalf("%q parsed to an unexpected verb %q", in, verb)
		}
	}
	for _, in := range []string{"", "y", "yes", "hello there", "Y a b", "maybe abc", "  "} {
		if _, _, ok := parseIMsgApprovalReply(in); ok {
			t.Errorf("%q must NOT parse as an approval reply", in)
		}
	}
}
