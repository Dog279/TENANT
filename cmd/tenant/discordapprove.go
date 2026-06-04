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
// auto-denying — so a turn can't wedge forever on a phone that never answers.
const approvalTimeout = 5 * time.Minute

// discordIO is the approver's Discord surface: a plain message, a message with
// approve/deny buttons (returns the message id), and a button-interaction ACK.
type discordIO interface {
	Send(ctx context.Context, channelID, content string) error
	SendComponents(ctx context.Context, channelID, content string, buttons []discord.Button) (messageID string, err error)
	RespondInteraction(ctx context.Context, interactionID, token, content string) error
}

// discordApprover satisfies a plugin Policy.Confirm: func(ctx, action, detail) bool.
type discordApprover struct {
	io      discordIO
	log     *slog.Logger
	timeout time.Duration

	mu      sync.Mutex
	channel string               // operator DM channel for the active turn (set by the relay)
	pending map[string]chan bool // nonce → reply channel (true=approve once, false=deny)
}

func newDiscordApprover(io discordIO, log *slog.Logger) *discordApprover {
	return &discordApprover{io: io, log: log, timeout: approvalTimeout, pending: map[string]chan bool{}}
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

// Confirm posts a nonce-bound prompt and blocks for an exact-match reply. It is
// approve-ONCE (the bool is "proceed with THIS action"); ctx/timeout/no-channel
// all DENY (fail closed). Holds no mode/session state.
func (a *discordApprover) Confirm(ctx context.Context, action, detail string) bool {
	channel := a.curChannel()
	if channel == "" || a.io == nil {
		return false // can't reach the operator → deny
	}
	nonce := genNonce()
	reply := make(chan bool, 1)
	a.mu.Lock()
	a.pending[nonce] = reply
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, nonce)
		a.mu.Unlock()
	}()

	prompt := fmt.Sprintf(
		"⚠️ Approve **%s**?\n%s\nTap a button below — or reply `APPROVE-%s` / `deny` (auto-denies in %s).",
		action, clipDetail(detail), nonce, a.timeout)
	buttons := []discord.Button{
		{Label: "✅ Approve", CustomID: "approve:" + nonce, Style: 3},
		{Label: "⛔ Deny", CustomID: "deny:" + nonce, Style: 4},
	}
	if _, err := a.io.SendComponents(ctx, channel, prompt, buttons); err != nil {
		a.post(channel, prompt) // components failed → plain-text prompt (the reply path still works)
	}

	tctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	select {
	case ok := <-reply:
		return ok // the resolver (button ACK / text reply) gave the UX feedback
	case <-tctx.Done():
		a.post(channel, fmt.Sprintf("⌛ approval `%s` expired — denied.", nonce))
		return false
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
		for _, ch := range a.pending {
			trySignal(ch, false)
		}
		a.mu.Unlock()
		a.post(channel, "⛔ denied.")
		return true
	}
	for nonce, ch := range a.pending {
		n := strings.ToLower(nonce)
		if text == "approve-"+n || text == n {
			trySignal(ch, true)
			a.mu.Unlock()
			a.post(channel, "✅ approved.")
			return true
		}
	}
	a.mu.Unlock()
	return false
}

// tryInteraction resolves a pending approval from a button click and ACKs the
// interaction (editing the prompt to the outcome within Discord's ~3s window).
// Returns true when the click was an approval button (so the relay stops there).
func (a *discordApprover) tryInteraction(in discord.Interaction) bool {
	approve, nonce, ok := parseButtonID(in.CustomID)
	if !ok {
		return false
	}
	a.mu.Lock()
	ch := a.pending[nonce]
	a.mu.Unlock()
	if ch == nil {
		// stale button (already resolved / expired) — ACK so the click doesn't error.
		_ = a.io.RespondInteraction(context.Background(), in.ID, in.Token, "⌛ This approval already resolved or expired.")
		return true
	}
	trySignal(ch, approve)
	outcome := "✅ Approved."
	if !approve {
		outcome = "⛔ Denied."
	}
	_ = a.io.RespondInteraction(context.Background(), in.ID, in.Token, outcome)
	return true
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
	if err := a.io.Send(ctx, channel, content); err != nil && a.log != nil {
		a.log.Warn("discord approver: send failed", "err", err)
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
