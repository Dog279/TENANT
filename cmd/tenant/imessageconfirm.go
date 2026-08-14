package main

// imessageconfirm.go is TEN-267 (Phase 2): the iMessage analogue of the Discord
// approver (discordapprove.go) — a nonce-bound, approve-ONCE, origin-scoped
// text-confirm handshake for iMessage-driven turns.
//
// Phase 1 fails CLOSED: every gated tool over iMessage is denied because the
// responder has no way to reach the operator out-of-band. Phase 2 gives it one:
// a gated action over iMessage texts the OPERATOR a prompt carrying a short
// unguessable nonce; a "Y <nonce>" / "yes <nonce>" reply FROM THE OPERATOR
// HANDLE approves JUST that action; "deny <nonce>" / no reply / timeout / a
// reply from anyone else → DENY.
//
// Why a SEPARATE type (mirrors discordApprover, NOT a new approvalBroker mode):
// the dedicated iMessage agent gets its OWN approver wired to its tool policies,
// so this approver is STRUCTURALLY unable to read or mutate the shared
// approvalBroker's modes/session/registry — an offsite phone decision can never
// flip a category to "allow", persist a grant, or resolve a LOCAL or DISCORD
// pending request. It owns its own pending map and resolves ONLY its own nonces.
//
// Security keystones (every one is unit-tested in imessageconfirm_test.go):
//   - Fail-closed: Confirm returns false on no operator, no send path, ctx
//     cancel, timeout, or nonce mismatch.
//   - Operator-handle gate: OnReply approves ONLY for a reply whose sender
//     handle normalizes-equal to the configured operator. A stranger texting
//     "Y <nonce>" can never approve.
//   - Nonce: crypto/rand (genNonce), single-use (deleted under lock on first
//     resolve), and only matches its own pending entry.
//   - Origin isolation: a phone Y resolves ONLY this approver's pending map; it
//     never touches the local broker or the Discord approver.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"tenant/internal/plugins/imessage"
)

// imsgApproveSender sends a one-off iMessage to the operator (the approval
// prompt and the resolution echo). *imessage.nativeService satisfies it via
// SendText; in tests a fake stands in. Kept minimal (just the send) so the
// approver depends on nothing else.
type imsgApproveSender interface {
	SendText(ctx context.Context, chatGUID, text string) (string, error)
}

// imsgPending is one outstanding approval: the channel the blocked Confirm waits
// on (buffered cap 1 so a resolver never blocks) plus the action for the echo.
type imsgPending struct {
	reply  chan bool
	action string
}

// imessageApprover satisfies a plugin Policy.Confirm: func(ctx, action, detail) bool.
// It is the "ask"-mode backend for the iMessage offsite broker (set as
// approvalBroker.ask), exactly like discordApprover backs the Discord broker.
//
// operator is the canonical (NormalizeHandle'd) handle whose reply approves.
// chatGUID is where the prompt is texted — the active inbound turn's chat, set
// per turn via setChat (the responder is single-goroutine so this is race-free
// across turns; the mutex still guards the field for the dashboard read path and
// the -race detector).
type imessageApprover struct {
	sender   imsgApproveSender
	operator string // canonical operator handle (NormalizeHandle); "" ⇒ Phase-2 off
	log      *slog.Logger
	timeout  time.Duration

	mu       sync.Mutex
	chatGUID string                  // where to text the active turn's prompt
	pending  map[string]*imsgPending // nonce → pending entry
}

// newIMessageApprover builds a Phase-2 approver for a configured operator handle.
// operator is normalized here so OnReply can compare canonical-to-canonical. A
// blank operator yields a nil approver (the caller keeps Phase-1 deny-all) — see
// the wiring; we never return a "live" approver that can't identify its operator.
func newIMessageApprover(sender imsgApproveSender, operator string, log *slog.Logger) *imessageApprover {
	op := imessage.NormalizeHandle(operator)
	if op == "" || sender == nil {
		return nil
	}
	return &imessageApprover{
		sender:   sender,
		operator: op,
		log:      log,
		timeout:  approvalTimeout,
		pending:  map[string]*imsgPending{},
	}
}

// setChat records the chat the active turn's approval prompts should text into.
// The responder calls this before each turn (mirrors discordApprover.setChannel).
func (a *imessageApprover) setChat(chatGUID string) {
	a.mu.Lock()
	a.chatGUID = chatGUID
	a.mu.Unlock()
}

func (a *imessageApprover) curChat() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.chatGUID
}

// Confirm posts a nonce-bound prompt to the operator and BLOCKS for an exact
// "Y <nonce>" reply from the operator handle. Approve-ONCE (the bool is "proceed
// with THIS action only"); no operator / no send path / ctx-cancel / timeout /
// nonce-deny all DENY (fail closed). Holds no mode/session state.
func (a *imessageApprover) Confirm(ctx context.Context, action, detail string) bool {
	chat := a.curChat()
	// No operator handle, no send path, or no chat to prompt into ⇒ we cannot
	// reach the operator ⇒ deny BEFORE registering anything (fail closed).
	if a.operator == "" || a.sender == nil || chat == "" {
		return false
	}

	nonce := genNonce()
	reply := make(chan bool, 1)
	a.mu.Lock()
	a.pending[nonce] = &imsgPending{reply: reply, action: action}
	a.mu.Unlock()
	// Always drop our entry on the way out — single-use even if we exit via
	// timeout/ctx (delete-under-lock; a late OnReply then finds nothing).
	defer func() {
		a.mu.Lock()
		delete(a.pending, nonce)
		a.mu.Unlock()
	}()

	prompt := fmt.Sprintf(
		"⚠️ Approve %s? Reply  Y %s  to allow (or  deny %s ) — auto-denies in %s.\n%s",
		action, nonce, nonce, a.timeout, clipDetail(detail))
	a.post(chat, prompt)

	tctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	select {
	case ok := <-reply:
		return ok // OnReply already texted the operator the outcome
	case <-tctx.Done():
		// Timed out OR the turn's ctx was cancelled ⇒ deny. We text the operator
		// only on a real timeout, not on a parent-ctx cancel (process teardown).
		if ctx.Err() == nil {
			a.post(chat, fmt.Sprintf("⌛ approval %s expired — denied.", nonce))
		}
		return false
	}
}

// OnReply routes an inbound iMessage to a pending approval. It returns
// handled=true ONLY when the message was an approval reply that THIS approver
// owns — so the responder short-circuits (does NOT drive an agent turn for a
// bare "Y abc123"). A non-approval message, or a reply from a non-operator
// handle, returns false (the responder treats it as a normal turn / ignores it).
//
// SECURITY: the operator-handle gate is enforced HERE. A reply is considered
// ONLY if fromHandle normalizes-equal to the configured operator. A stranger's
// "Y <nonce>" returns (false) and never resolves anything — even if they somehow
// guessed the nonce.
func (a *imessageApprover) OnReply(text, fromHandle string) (handled bool) {
	if a == nil {
		return false
	}
	// Operator-handle gate: only the operator can drive an approval reply. We
	// compare canonical-to-canonical and require a non-empty configured operator.
	if a.operator == "" || imessage.NormalizeHandle(fromHandle) != a.operator {
		return false
	}

	verb, nonce, ok := parseIMsgApprovalReply(text)
	if !ok {
		return false // not an approval-shaped message ⇒ a normal turn
	}

	// Resolve + delete UNDER LOCK (single-use, race-proof). The buffered send is
	// done while holding the lock — the channel is cap-1 and freshly removed from
	// the map so no other goroutine can also send; the send never blocks.
	a.mu.Lock()
	p := a.pending[nonce]
	if p == nil {
		a.mu.Unlock()
		// Operator replied with a well-formed but unknown/expired nonce. We OWN
		// approval-shaped traffic from the operator, so short-circuit (don't drive
		// a turn on "Y stale123") — but nothing was approved.
		a.post(a.curChat(), fmt.Sprintf("⌛ approval %s not found (already resolved or expired).", nonce))
		return true
	}
	delete(a.pending, nonce)
	approve := verb == "y" || verb == "yes" || verb == "approve"
	trySignal(p.reply, approve)
	action := p.action
	a.mu.Unlock()

	outcome := "✅ approved."
	if !approve {
		outcome = "⛔ denied."
	}
	a.post(a.curChat(), fmt.Sprintf("%s (%s)", outcome, action))
	return true
}

// PendingCount reports how many approvals are outstanding (test/diagnostic).
func (a *imessageApprover) PendingCount() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

// post sends one iMessage best-effort (the approval prompt / echo). Failures are
// logged, never fatal: a dropped prompt simply means the operator never approves
// and the action auto-denies on timeout (fail closed).
func (a *imessageApprover) post(chatGUID, text string) {
	if a.sender == nil || strings.TrimSpace(chatGUID) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := a.sender.SendText(ctx, chatGUID, text); err != nil && a.log != nil {
		a.log.Warn("imessage approver: send failed", "err", err)
	}
}

// parseIMsgApprovalReply recognizes an approval reply and splits it into a verb
// and the nonce. Accepted shapes (case-insensitive, surrounding space trimmed):
//
//	Y <nonce>   yes <nonce>   approve <nonce>   ⇒ approve
//	N <nonce>   no  <nonce>   deny    <nonce>   ⇒ deny
//
// The nonce is REQUIRED — a bare "Y" never matches, so two stacked approvals
// can't be mis-bound and a casual "yes" in conversation isn't an approval. ok is
// false for anything else (a normal turn). The match is exactly two whitespace-
// separated fields so trailing chatter ("Y abc do it") is NOT an approval.
func parseIMsgApprovalReply(text string) (verb, nonce string, ok bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) != 2 {
		return "", "", false
	}
	v := strings.ToLower(fields[0])
	switch v {
	case "y", "yes", "approve", "n", "no", "deny":
	default:
		return "", "", false
	}
	// Normalize the deny verbs to a single canonical for the caller's check, but
	// keep them distinct from approve. We return the lowercased verb as-is and let
	// OnReply decide approve vs deny; here we only validate shape.
	return v, fields[1], true
}
