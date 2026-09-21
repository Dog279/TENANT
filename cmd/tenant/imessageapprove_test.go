package main

// TEN-230 Phase 2 tests: iMessage-origin approvals resolve over iMessage, in the
// originating chat, without deadlocking the responder's poll loop.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/plugins/imessage"
	"tenant/internal/tui"
)

// syncSender is a goroutine-safe imsgSender (the approver sends from the turn
// goroutine while the test reads).
type syncSender struct {
	mu   sync.Mutex
	sent []string // "chat|text"
}

func (s *syncSender) SendText(_ context.Context, chatGUID, text string) (string, error) {
	s.mu.Lock()
	s.sent = append(s.sent, chatGUID+"|"+text)
	s.mu.Unlock()
	return "ok", nil
}

func (s *syncSender) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

func (s *syncSender) waitFor(n int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if len(s.all()) >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// nullPoller never delivers anything; tests drive handle directly.
type nullPoller struct {
	mu   sync.Mutex
	recs []string
}

func (p *nullPoller) Poll(context.Context, int) ([]imessage.InboundMessage, error) { return nil, nil }
func (p *nullPoller) RecordSent(chat, text string) {
	p.mu.Lock()
	p.recs = append(p.recs, chat+"|"+text)
	p.mu.Unlock()
}

// gatedRunner's Turn asks for approval the way a gated tool would, and reports
// the verdict it got.
type gatedRunner struct{ verdict chan bool }

func (g *gatedRunner) Turn(ctx context.Context, _ agent.TurnRequest) (*agent.TurnResult, error) {
	ok := false
	if c := offsiteConfirmFrom(ctx); c != nil {
		ok = c(ctx, "run a command", "rm -rf /tmp/x")
	}
	g.verdict <- ok
	if ok {
		return &agent.TurnResult{Response: "done"}, nil
	}
	return &agent.TurnResult{Response: "refused"}, nil
}

func newTestIMsgApprover(ss *syncSender, timeout time.Duration) *imessageApprover {
	a := newIMessageApprover(nil)
	a.timeout = timeout
	a.setSender(func(ctx context.Context, chat, text string) { _, _ = ss.SendText(ctx, chat, text) })
	return a
}

// The prompt carries a code; only that code, from the SAME chat, approves — and
// only once (a second gated action re-prompts, there is no session grant).
func TestIMessageApprover_NonceApproveOnceOriginBound(t *testing.T) {
	ss := &syncSender{}
	a := newTestIMsgApprover(ss, 2*time.Second)
	a.setChat("chat-A")

	res := make(chan bool, 1)
	go func() { res <- a.Confirm(context.Background(), "run a command", "rm -rf /tmp/x") }()
	if !ss.waitFor(1, time.Second) {
		t.Fatal("no prompt texted")
	}
	prompt := ss.all()[0]
	nonce := nonceFrom(prompt)
	if nonce == "" || !strings.HasPrefix(prompt, "chat-A|") {
		t.Fatalf("prompt must carry APPROVE-<code> and go to the originating chat: %q", prompt)
	}
	if a.tryConsume("chat-B", "APPROVE-"+nonce) {
		t.Fatal("a code from ANOTHER chat must not approve (origin-bound)")
	}
	if a.tryConsume("chat-B", "no") {
		t.Fatal("a stray 'no' from a chat with nothing pending is a normal message")
	}
	if !a.tryConsume("chat-A", "approve-"+nonce) {
		t.Fatal("the originating chat's code must be consumed")
	}
	if v, ok := waitBool(res, time.Second); !ok || !v {
		t.Fatalf("Confirm should approve on the matching code (v=%v ok=%v)", v, ok)
	}

	// Approve-once: the next gated action re-prompts.
	go func() { res <- a.Confirm(context.Background(), "act2", "d2") }()
	if _, ok := waitBool(res, 200*time.Millisecond); ok {
		t.Fatal("a second action auto-approved — there must be NO session grant over iMessage")
	}
	if !ss.waitFor(3, time.Second) { // prompt1, approved., prompt2
		t.Fatal("second action did not re-prompt")
	}
	if !a.tryConsume("chat-A", "deny") {
		t.Fatal("'deny' in the originating chat must be consumed")
	}
	if v, ok := waitBool(res, time.Second); !ok || v {
		t.Fatal("'deny' must refuse the action")
	}
}

func TestIMessageApprover_TimeoutAndNoChatDeny(t *testing.T) {
	ss := &syncSender{}
	a := newTestIMsgApprover(ss, 80*time.Millisecond)
	if a.Confirm(context.Background(), "act", "d") {
		t.Fatal("with no active chat the approver must DENY (fail closed)")
	}
	a.setChat("chat-A")
	if a.Confirm(context.Background(), "act", "d") {
		t.Fatal("an un-answered approval must DENY")
	}
	found := false
	for _, s := range ss.all() {
		if strings.Contains(s, "expired") {
			found = true
		}
	}
	if !found {
		t.Errorf("expiry must be texted back, sends=%v", ss.all())
	}
	// The broker adapter maps the bool to approve-once / deny.
	if d := a.askBackend()(context.Background(), tui.ApprovalRequest{Action: "act"}); d != tui.DenyOnce {
		t.Errorf("askBackend on timeout = %v, want DenyOnce", d)
	}
}

// THE Phase-2 regression: a turn blocked on an approval is released by the
// operator's reply arriving on a LATER poll. On the old synchronous handle the
// poll loop sat inside the very turn that was waiting for the reply, so the
// reply could never be delivered and every approval expired.
func TestIMessageResponder_ApprovalReplyReleasesBlockedTurn(t *testing.T) {
	ss := &syncSender{}
	appr := newTestIMsgApprover(ss, 3*time.Second)
	gr := &gatedRunner{verdict: make(chan bool, 1)}
	r := &imessageResponder{poller: &nullPoller{}, sender: ss, runner: gr, approver: appr}
	r.confirm = appr.Confirm
	appr.setSender(r.send)
	ctx := context.Background()

	r.handle(ctx, inbound("chat-A", "do the thing"))
	if !ss.waitFor(1, 2*time.Second) {
		t.Fatal("the approval prompt never posted — the turn blocked the loop")
	}
	nonce := nonceFrom(ss.all()[0])
	if nonce == "" {
		t.Fatalf("no code in the prompt: %q", ss.all()[0])
	}

	// A second user text mid-turn gets a busy reply and does NOT start a turn.
	r.handle(ctx, inbound("chat-A", "hello?"))
	if !ss.waitFor(2, time.Second) || !strings.Contains(ss.all()[1], "still working") {
		t.Fatalf("a text mid-turn must get a busy reply, sends=%v", ss.all())
	}

	// The operator's reply, delivered by a later poll, releases the turn.
	r.handle(ctx, inbound("chat-A", "APPROVE-"+nonce))
	select {
	case ok := <-gr.verdict:
		if !ok {
			t.Fatal("the turn must see the approval")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the turn was never released by the reply")
	}
	r.waitIdle()
	last := ss.all()[len(ss.all())-1]
	if !strings.HasSuffix(last, "|done") {
		t.Errorf("the turn's reply must follow the approval, sends=%v", ss.all())
	}
	// And the responder is free again.
	r.mu.Lock()
	active := r.active
	r.mu.Unlock()
	if active {
		t.Error("responder must be idle after the turn")
	}
}

// The host mirror of an iMessage-origin approval is labeled "imessage", not
// "discord" (the origin literal was hardcoded).
func TestOffsiteBroker_MirrorCarriesOwnOrigin(t *testing.T) {
	host := newApprovalBroker(nil)
	b := newOffsiteApprovalBroker(nil, host)
	var ids []string
	b.reg.emit = func(e agent.Event) {
		if e.Approval != nil {
			ids = append(ids, e.Approval.ID+"/"+e.Approval.Origin)
		}
	}
	b.ask = func(context.Context, tui.ApprovalRequest) tui.ApprovalDecision { return tui.DenyOnce }
	if d := b.askDecision(context.Background(), catExec, "run", "d"); d != tui.DenyOnce {
		t.Fatalf("askDecision = %v", d)
	}
	if len(ids) != 2 || ids[0] != "beside-imessage/imessage" || ids[1] != "beside-imessage/imessage" {
		t.Fatalf("mirror events must carry the broker's own origin, got %v", ids)
	}
}
