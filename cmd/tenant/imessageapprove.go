package main

// imessageapprove.go is TEN-230 Phase 2: the origin-scoped approval surface for
// iMessage-driven turns. Approvals go back out the channel they came in on — a
// texted operator decides over iMessage, not at the host TUI they are away from
// exactly when a text turn needs a decision (Phase 1 refused every gated tool).
//
// Same doctrine as the Discord approver (TEN-117): nonce-bound, approve-ONCE,
// fail-closed, no session/always grants offsite. iMessage has no buttons, so the
// handshake is text: the prompt carries a short code and the operator replies
// `APPROVE-<code>` (or `deny`). The reply must come from the SAME chat the turn
// started in — an approval is bound to its origin, never to "whoever texts".

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"tenant/internal/tui"
)

type imsgPending struct {
	reply    chan bool
	chat     string // the chat the turn (and so the approval) belongs to
	action   string
	postedAt time.Time
}

// imessageApprover satisfies the iMessage broker's "ask" backend.
type imessageApprover struct {
	log     *slog.Logger
	timeout time.Duration
	now     func() time.Time

	mu      sync.Mutex
	send    func(ctx context.Context, chatGUID, text string) // the responder's anti-loop send; attached per Start
	chat    string                                           // the active turn's chat
	pending map[string]*imsgPending                          // nonce → outstanding prompt
}

func newIMessageApprover(log *slog.Logger) *imessageApprover {
	return &imessageApprover{log: log, timeout: approvalTimeout, now: time.Now, pending: map[string]*imsgPending{}}
}

// setSender attaches the responder's send (the native transport is opened
// lazily on each Start, so the approver outlives it and is re-pointed).
func (a *imessageApprover) setSender(fn func(ctx context.Context, chatGUID, text string)) {
	a.mu.Lock()
	a.send = fn
	a.mu.Unlock()
}

// setChat records the chat the active turn came from — where prompts post and
// the only chat whose replies can resolve them.
func (a *imessageApprover) setChat(chat string) {
	a.mu.Lock()
	a.chat = chat
	a.mu.Unlock()
}

// askBackend adapts Confirm to the broker's "ask" backend: approve-once or deny.
func (a *imessageApprover) askBackend() func(ctx context.Context, req tui.ApprovalRequest) tui.ApprovalDecision {
	return func(ctx context.Context, req tui.ApprovalRequest) tui.ApprovalDecision {
		if a.Confirm(ctx, req.Action, req.Detail) {
			return tui.ApproveOnce
		}
		return tui.DenyOnce
	}
}

// Confirm texts a nonce-bound prompt to the active chat and blocks for the
// operator's reply. ctx/timeout/no-chat all DENY (fail closed).
func (a *imessageApprover) Confirm(ctx context.Context, action, detail string) bool {
	a.mu.Lock()
	chat, send := a.chat, a.send
	a.mu.Unlock()
	if chat == "" || send == nil {
		a.logWarn("imessage approval: no operator chat — denied", "action", action)
		return false
	}
	nonce := genNonce()
	p := &imsgPending{reply: make(chan bool, 1), chat: chat, action: action, postedAt: a.now()}
	a.mu.Lock()
	a.pending[nonce] = p
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, nonce)
		a.mu.Unlock()
	}()

	a.post(chat, fmt.Sprintf("⚠️ Approve %s?\n%s\nReply APPROVE-%s to approve, or DENY. Auto-denies in %s.",
		action, clipDetail(detail), nonce, a.timeout))
	a.logInfo("imessage approval posted", "chat", chat, "action", action)

	tctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	select {
	case ok := <-p.reply:
		return ok
	case <-tctx.Done():
		a.post(chat, fmt.Sprintf("⌛ approval %s expired — denied.", nonce))
		a.logInfo("imessage approval expired — denied", "chat", chat, "action", action,
			"waited", a.now().Sub(p.postedAt).Round(time.Second))
		return false
	}
}

// tryConsume routes an inbound text to a pending approval in the SAME chat.
// Returns true when the text WAS an approval reply (so the responder must not
// treat it as a new turn). A code from another chat, or a stray "no" where
// nothing is pending, is a normal message.
func (a *imessageApprover) tryConsume(chatGUID, text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	a.mu.Lock()
	if len(a.pending) == 0 {
		a.mu.Unlock()
		return false
	}
	if t == "deny" || t == "no" {
		denied := 0
		for _, p := range a.pending {
			if p.chat == chatGUID {
				trySignal(p.reply, false)
				denied++
			}
		}
		a.mu.Unlock()
		if denied == 0 {
			return false
		}
		a.post(chatGUID, "⛔ denied.")
		a.logInfo("imessage approval resolved", "outcome", "denied", "chat", chatGUID, "count", denied)
		return true
	}
	for nonce, p := range a.pending {
		nl := strings.ToLower(nonce)
		if (t == "approve-"+nl || t == nl) && p.chat == chatGUID {
			trySignal(p.reply, true)
			a.mu.Unlock()
			a.post(chatGUID, "✅ approved.")
			a.logInfo("imessage approval resolved", "outcome", "approved", "chat", chatGUID,
				"action", p.action, "latency", a.now().Sub(p.postedAt).Round(time.Millisecond))
			return true
		}
	}
	a.mu.Unlock()
	return false
}

func (a *imessageApprover) post(chat, text string) {
	a.mu.Lock()
	send := a.send
	a.mu.Unlock()
	if send == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	send(ctx, chat, text)
}

func (a *imessageApprover) logInfo(msg string, kv ...any) {
	if a.log != nil {
		a.log.Info(msg, kv...)
	}
}

func (a *imessageApprover) logWarn(msg string, kv ...any) {
	if a.log != nil {
		a.log.Warn(msg, kv...)
	}
}
