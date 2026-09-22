package main

// discordapprove.go is TEN-117 (T3): the safety keystone — a nonce-bound,
// approve-ONCE, origin-scoped approval path for Discord-driven turns.
//
// Why a SEPARATE type (not a new param on approvalBroker.Confirm): the dedicated
// Discord agent (verdict 1B) gets its OWN approver wired to its tool policies, so
// this approver is STRUCTURALLY unable to read or mutate the shared
// approvalBroker's modes/session — an offsite decision can never flip a category
// to "allow" or persist/leak a grant into the local TUI. It also has no
// session/always concept at all: every grant is one action only.
//
// Spoof/race fixes (the debate's must-fix #2): each gated prompt carries a short
// random nonce; the operator must reply that exact token, which satisfies the
// SPECIFIC pending Confirm (so two stacked approvals can't be mis-bound by a bare
// "y"). No reply (or a gateway drop, or ctx cancel) → DENY (fail closed).
//
// The card (the v2 overhaul): a prompt is a Components-v2 card (legacy layout
// as the fallback, plain text as the last resort) with a green Approve / red
// Deny button pair. Every resolution — click, text reply, or expiry — rewrites
// THAT card in place: verdict line appended, action/detail kept, buttons
// disabled. So no live buttons ever linger, the message stays an audit record,
// and a click is ACKed first (within Discord's 3-second window) before anything
// else happens. Every post and outcome is logged, because a prompt that quietly
// failed to post is exactly the failure that left the operator staring at a TUI
// mirror while the approval expired.

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"tenant/internal/plugins/discord"
)

// approvalTimeout bounds how long a gated action waits for the operator before
// auto-denying — long enough for a human on a phone, short enough that a turn
// can't wedge forever on one that never answers. Operator-tunable via
// relay.approval_timeout / `/relay timeout`.
const approvalTimeout = 10 * time.Minute

// ackTimeout bounds an interaction ACK. Discord invalidates the token 3 s after
// the click, so an ACK that waits longer is worse than none — it holds the
// handler and still shows "interaction failed".
const ackTimeout = 2500 * time.Millisecond

// Card layouts. v2 is the Components-v2 container card; legacy is plain content
// plus a button row. v2 falls back to legacy automatically when Discord rejects
// the post (an older API surface, a permission gap), and legacy falls back to a
// plain-text nonce prompt.
const (
	cardStyleV2     = "v2"
	cardStyleLegacy = "legacy"
)

// discordIO is the approver's Discord surface: a plain message, the two card
// layouts (returning the message id so the card can be rewritten later), the
// interaction ACKs, and an out-of-band edit for resolutions that happen without
// an interaction token (expiry, text replies).
type discordIO interface {
	Send(ctx context.Context, channelID, content string) error
	SendComponents(ctx context.Context, channelID, content string, buttons []discord.Button) (messageID string, err error)
	SendCard(ctx context.Context, channelID, content string, buttons []discord.Button) (messageID string, err error)
	RespondInteraction(ctx context.Context, interactionID, token, content string) error
	RespondUpdate(ctx context.Context, interactionID, token, content string, buttons []discord.Button, v2 bool) error
	RespondEphemeral(ctx context.Context, interactionID, token, text string) error
	EditMessage(ctx context.Context, channelID, messageID, content string, buttons []discord.Button, v2 bool) error
}

// pendingApproval is one outstanding prompt: the waiting Confirm's reply
// channel plus everything needed to rewrite its card later.
type pendingApproval struct {
	reply    chan bool
	channel  string
	msgID    string // "" when no card posted (plain-text fallback) — nothing to edit; guarded by the approver's mu
	v2       bool   // layout the card was posted with; edits must match it; guarded by the approver's mu
	action   string
	detail   string
	postedAt time.Time
}

// discordApprover satisfies a plugin Policy.Confirm: func(ctx, action, detail) bool.
type discordApprover struct {
	io        discordIO
	log       *slog.Logger
	timeout   time.Duration
	cardStyle string           // cardStyleV2 (default) | cardStyleLegacy
	notify    func(string)     // optional: an operator feed line when a prompt degrades
	now       func() time.Time // injectable clock (tests)

	mu      sync.Mutex
	channel string                      // operator channel for the active turn (set by the relay)
	pending map[string]*pendingApproval // nonce → outstanding prompt
}

func newDiscordApprover(io discordIO, log *slog.Logger) *discordApprover {
	return &discordApprover{
		io: io, log: log, timeout: approvalTimeout, cardStyle: cardStyleV2, now: time.Now,
		pending: map[string]*pendingApproval{},
	}
}

// setChannel records where approval prompts for the active turn should post.
func (a *discordApprover) setChannel(ch string) {
	a.mu.Lock()
	a.channel = ch
	a.mu.Unlock()
}

func (a *discordApprover) curChannel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.channel
}

// approvalButtons is the Approve/Deny pair. disabled freezes them on a resolved
// card — greyed out, still visible, so the verdict reads in place.
func approvalButtons(nonce string, disabled bool) []discord.Button {
	return []discord.Button{
		{Label: "Approve", Emoji: "✅", CustomID: "approve:" + nonce, Style: 3, Disabled: disabled},
		{Label: "Deny", Emoji: "⛔", CustomID: "deny:" + nonce, Style: 4, Disabled: disabled},
	}
}

// cardText is the prompt body: what is being asked, nothing else. The nonce
// reply instructions only appear on the plain-text fallback.
func cardText(action, detail string) string {
	s := "**Approval needed:** " + action
	if d := clipDetail(detail); d != "" {
		s += "\n" + d
	}
	return s
}

// verdictText rewrites a card to its outcome while KEEPING the action/detail,
// so the message remains a record of what was decided, by whom, and when.
func verdictText(p *pendingApproval, verdict, by string, at time.Time) string {
	var line string
	switch verdict {
	case "approved":
		line = "✅ Approved"
	case "denied":
		line = "⛔ Denied"
	default:
		line = "⌛ Expired — auto-denied"
	}
	if by != "" {
		line += " by <@" + by + ">"
	}
	line += " · " + at.UTC().Format("15:04 UTC")
	return cardText(p.action, p.detail) + "\n\n" + line
}

// Confirm posts a card and blocks for the operator's verdict. It is
// approve-ONCE (the bool is "proceed with THIS action"); ctx/timeout/no-channel
// all DENY (fail closed). Holds no mode/session state.
func (a *discordApprover) Confirm(ctx context.Context, action, detail string) bool {
	channel := a.curChannel()
	if channel == "" || a.io == nil {
		a.logWarn("discord approval: no operator channel — denied", "action", action)
		return false
	}
	nonce := genNonce()
	p := &pendingApproval{reply: make(chan bool, 1), channel: channel, action: action, detail: detail, postedAt: a.now()}
	a.mu.Lock()
	a.pending[nonce] = p
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, nonce)
		a.mu.Unlock()
	}()

	a.postCard(ctx, nonce, p)

	tctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	select {
	case ok := <-p.reply:
		return ok // the resolver already rewrote the card
	case <-tctx.Done():
		a.expire(nonce, p)
		return false
	}
}

// postCard posts the prompt: v2 card → legacy card → plain text carrying the
// nonce reply instructions. Each degradation is logged, and the last one also
// surfaces on the operator feed, so a prompt can never fail to reach Discord
// silently again.
func (a *discordApprover) postCard(ctx context.Context, nonce string, p *pendingApproval) {
	content := cardText(p.action, p.detail)
	buttons := approvalButtons(nonce, false)
	if a.cardStyle != cardStyleLegacy {
		id, err := a.io.SendCard(ctx, p.channel, content, buttons)
		if err == nil {
			a.setCard(p, id, true)
			a.logInfo("discord approval posted", "layout", "v2", "channel", p.channel, "message", id, "action", p.action)
			return
		}
		a.logWarn("discord approval: v2 card rejected — falling back to legacy buttons", "channel", p.channel, "err", err)
	}
	id, err := a.io.SendComponents(ctx, p.channel, content, buttons)
	if err == nil {
		a.setCard(p, id, false)
		a.logInfo("discord approval posted", "layout", "legacy", "channel", p.channel, "message", id, "action", p.action)
		return
	}
	a.logWarn("discord approval: buttons failed to post — falling back to a text prompt", "channel", p.channel, "err", err)
	if a.notify != nil {
		a.notify("discord approval: buttons failed to post in " + p.channel + " — sent a text prompt instead (reply APPROVE-" + nonce + " / deny)")
	}
	a.post(p.channel, fmt.Sprintf("%s\nReply `APPROVE-%s` to approve or `deny` (auto-denies in %s).", content, nonce, a.timeout))
}

// setCard and card guard the posted card's id and layout: postCard sets them
// after the entry is already pending (so an early reply still finds it), while
// the click and text resolvers read them from other goroutines.
func (a *discordApprover) setCard(p *pendingApproval, msgID string, v2 bool) {
	a.mu.Lock()
	p.msgID, p.v2 = msgID, v2
	a.mu.Unlock()
}

func (a *discordApprover) card(p *pendingApproval) (msgID string, v2 bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return p.msgID, p.v2
}

// expire freezes the ORIGINAL card as expired (buttons disabled) so no live
// buttons linger after the auto-deny, then logs the outcome. When no card ever
// posted, it falls back to a line.
func (a *discordApprover) expire(nonce string, p *pendingApproval) {
	a.logInfo("discord approval expired — denied", "channel", p.channel, "action", p.action,
		"waited", a.now().Sub(p.postedAt).Round(time.Second))
	if msgID, v2 := a.card(p); msgID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := a.io.EditMessage(ctx, p.channel, msgID, verdictText(p, "expired", "", a.now()), approvalButtons(nonce, true), v2); err == nil {
			return
		} else {
			a.logWarn("discord approval: expiry edit failed", "message", msgID, "err", err)
		}
	}
	a.post(p.channel, fmt.Sprintf("⌛ approval `%s` expired — denied.", nonce))
}

// freeze rewrites a card to its verdict after a TEXT resolution (there is no
// interaction token to ACK with), via an out-of-band edit. Best-effort.
func (a *discordApprover) freeze(nonce string, p *pendingApproval, verdict, by string) {
	msgID, v2 := a.card(p)
	if msgID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.io.EditMessage(ctx, p.channel, msgID, verdictText(p, verdict, by, a.now()), approvalButtons(nonce, true), v2); err != nil {
		a.logWarn("discord approval: card edit failed", "message", msgID, "err", err)
	}
}

// tryConsume routes an operator reply to a pending approval. Returns true when
// the message WAS an approval reply (exact nonce → approve that one; deny/no →
// deny all pending), so the relay must not treat it as a new turn or rate-limit
// it. A non-matching message returns false (it's a normal turn).
func (a *discordApprover) tryConsume(in discord.Inbound) bool {
	text := strings.ToLower(strings.TrimSpace(in.Content))
	a.mu.Lock()
	if len(a.pending) == 0 {
		a.mu.Unlock()
		return false
	}
	channel := a.channel
	if text == "deny" || text == "no" {
		type hit struct {
			nonce string
			p     *pendingApproval
		}
		var denied []hit
		for nonce, p := range a.pending {
			trySignal(p.reply, false)
			denied = append(denied, hit{nonce, p})
		}
		a.mu.Unlock()
		for _, h := range denied {
			a.freeze(h.nonce, h.p, "denied", in.AuthorID)
			a.logInfo("discord approval resolved", "outcome", "denied", "via", "text", "by", in.AuthorID,
				"channel", h.p.channel, "action", h.p.action, "latency", a.now().Sub(h.p.postedAt).Round(time.Millisecond))
		}
		a.post(channel, "⛔ denied.")
		return true
	}
	for nonce, p := range a.pending {
		n := strings.ToLower(nonce)
		if text == "approve-"+n || text == n {
			trySignal(p.reply, true)
			a.mu.Unlock()
			a.freeze(nonce, p, "approved", in.AuthorID)
			a.logInfo("discord approval resolved", "outcome", "approved", "via", "text", "by", in.AuthorID,
				"channel", p.channel, "action", p.action, "latency", a.now().Sub(p.postedAt).Round(time.Millisecond))
			a.post(channel, "✅ approved.")
			return true
		}
	}
	a.mu.Unlock()
	return false
}

// tryInteraction resolves a pending approval from a button click. It ACKs
// FIRST — rewriting the card in place to its verdict with the buttons disabled,
// under a bounded ctx — and only then releases the waiting Confirm. Discord
// invalidates the interaction 3 s after the click, so nothing may run ahead of
// the ACK. Returns true when the click was an approval button.
func (a *discordApprover) tryInteraction(in discord.Interaction) bool {
	approve, nonce, ok := parseButtonID(in.CustomID)
	if !ok {
		return false
	}
	a.mu.Lock()
	p := a.pending[nonce]
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()
	if p == nil {
		// Stale click (already resolved / expired): tell only the clicker and
		// leave the card exactly as its resolution left it.
		_ = a.io.RespondEphemeral(ctx, in.ID, in.Token, "⌛ This approval already resolved or expired.")
		return true
	}
	verdict := "approved"
	if !approve {
		verdict = "denied"
	}
	_, v2 := a.card(p)
	if err := a.io.RespondUpdate(ctx, in.ID, in.Token, verdictText(p, verdict, in.UserID, a.now()), approvalButtons(nonce, true), v2); err != nil {
		a.logWarn("discord approval: click ACK failed (the tap may show as failed; the verdict still applies)", "err", err)
	}
	trySignal(p.reply, approve)
	a.logInfo("discord approval resolved", "outcome", verdict, "via", "button", "by", in.UserID,
		"channel", p.channel, "action", p.action, "latency", a.now().Sub(p.postedAt).Round(time.Millisecond))
	return true
}

// ackEphemeral answers a click the approver will not act on (not the operator,
// or a button that isn't wired) so it never shows "interaction failed".
func (a *discordApprover) ackEphemeral(in discord.Interaction, text string) {
	if a.io == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()
	_ = a.io.RespondEphemeral(ctx, in.ID, in.Token, text)
}

// parseButtonID splits "approve:<nonce>" / "deny:<nonce>".
func parseButtonID(custom string) (approve bool, nonce string, ok bool) {
	if n, found := strings.CutPrefix(custom, "approve:"); found {
		return true, n, true
	}
	if n, found := strings.CutPrefix(custom, "deny:"); found {
		return false, n, true
	}
	return false, "", false
}

func (a *discordApprover) post(channel, content string) {
	if a.io == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.io.Send(ctx, channel, content); err != nil {
		a.logWarn("discord approver: send failed", "channel", channel, "err", err)
	}
}

func (a *discordApprover) logInfo(msg string, kv ...any) {
	if a.log != nil {
		a.log.Info(msg, kv...)
	}
}

func (a *discordApprover) logWarn(msg string, kv ...any) {
	if a.log != nil {
		a.log.Warn(msg, kv...)
	}
}

func trySignal(ch chan bool, v bool) {
	select {
	case ch <- v:
	default:
	}
}

// genNonce returns a short, unguessable token (4 bytes of crypto/rand →
// 6 base32 chars, e.g. "7Q2XK4").
func genNonce() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "XXXXXX"
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	if len(s) > 6 {
		s = s[:6]
	}
	return s
}

func clipDetail(s string) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 300 {
		return string([]rune(s)[:299]) + "…"
	}
	return s
}
