package main

// discordrelay.go is TEN-116 (T2): the relay core that bridges the inbound
// Discord gateway (TEN-115) to a DEDICATED agent and streams the final answer
// back to the operator's DM. Per the finalized plan (docs/plan-discord-relay.md
// verdict 1B), the Discord agent is its OWN *agent.Agent with its OWN broker —
// so every event on r.broker belongs to a Discord-driven turn (no shared-agent
// event mis-routing, no cross-front-end interject crossing). This file owns the
// inbound gate, the single-active-turn coordinator, and the outbound pump; the
// dedicated agent + approver + lifecycle are wired by the manager (TEN-119).

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/plugins/discord"
)

// maxInboundLen caps an inbound DM fed to a turn (defense against a giant paste
// blowing the prompt budget; the operator can split a long ask).
const maxInboundLen = 4000

// discordMsgLimit is Discord's hard per-message character cap.
const discordMsgLimit = 2000

// relayRunner is the slice of *agent.Agent the relay drives (Turn + Interject).
// Kept as an interface so the relay is unit-testable with a fake.
type relayRunner interface {
	Turn(ctx context.Context, req agent.TurnRequest) (*agent.TurnResult, error)
	Interject(msg string)
}

// messageSender sends a chat message to a Discord channel (the bot's reply
// path). Implemented by discordSender over *discord.Service; faked in tests.
type messageSender interface {
	Send(ctx context.Context, channelID, content string) error
}

// relay routes operator DMs to the dedicated agent and posts the agent's final
// answer back. Single-active-turn: at most one Discord turn runs at a time. The
// final answer is delivered from the turn goroutine via TurnResult.Response
// (race-free + authoritative) rather than the async broker — v1 streams no live
// progress, so the relay needs no broker subscription.
type relay struct {
	runner     relayRunner
	sender     messageSender
	operatorID string
	log        *slog.Logger
	rl         *rateLimiter
	approver   *discordApprover // optional; set by the manager (TEN-117/119)
	degraded   func() bool      // optional; when true the model is on the echo fallback

	mu     sync.Mutex
	base   context.Context // lifetime of the relay; background turns derive from it
	active bool
	cancel context.CancelFunc
}

func newRelay(runner relayRunner, sender messageSender, operatorID string, log *slog.Logger) *relay {
	return &relay{
		runner: runner, sender: sender,
		operatorID: strings.TrimSpace(operatorID), log: log,
		rl:   newRateLimiter(6, 30*time.Second), // ~6 messages / 30s per user
		base: context.Background(),
	}
}

// Start records the relay's base context (so Disable cancels in-flight turns).
// The gateway's OnMessage should be pointed at handleInbound after Start.
func (r *relay) Start(ctx context.Context) {
	r.mu.Lock()
	r.base = ctx
	r.mu.Unlock()
}

// handleInbound is the gateway callback. It enforces the H1 inbound boundary
// (drop bots, non-operator, non-DM; rate-limit; cap length) then routes the
// message to a new turn, an interjection, or a stop.
func (r *relay) handleInbound(in discord.Inbound) {
	if in.AuthorBot {
		return // never react to bots (loop guard)
	}
	if r.operatorID == "" || in.AuthorID != r.operatorID {
		return // single-operator allowlist, deny-by-default
	}
	if in.GuildID != "" {
		return // DM-only in v1 (no guild surface)
	}
	// An approval reply (exact nonce / "deny") is consumed by the approver, not
	// routed as a new turn or counted against the rate limit.
	if r.approver != nil && r.approver.tryConsume(in) {
		return
	}
	text := strings.TrimSpace(in.Content)
	if text == "" {
		return
	}
	if len(text) > maxInboundLen {
		text = text[:maxInboundLen]
	}
	if !r.rl.allow(in.AuthorID) {
		r.reply(in.ChannelID, "slow down — rate limited; try again shortly.")
		return
	}

	switch strings.ToLower(text) {
	case "stop", "cancel":
		if r.stopActive() {
			r.reply(in.ChannelID, "stopped the running turn.")
		} else {
			r.reply(in.ChannelID, "nothing is running.")
		}
		return
	}

	// While the local model is degraded to the echo fallback, refuse rather than
	// drive a turn — the remote operator never sees the terminal banner and must
	// not be handed an echo stub presented as a real answer.
	if r.degraded != nil && r.degraded() {
		r.reply(in.ChannelID, "⚠ the model is unavailable on the host right now (running on a local fallback). I'm not answering with a stub — try again once it's back, or fix it at the console with /model.")
		return
	}

	if r.start(in.ChannelID, text) {
		return // new turn launched; its goroutine posts the answer
	}
	// A turn is already running. Single operator ⇒ it's theirs ⇒ fold this in.
	r.runner.Interject(text)
	r.reply(in.ChannelID, "added to the running turn.")
}

// handleInteraction routes a button click to the approver — but ONLY the
// operator's clicks count (defense-in-depth; in a DM only they can click).
func (r *relay) handleInteraction(in discord.Interaction) {
	if r.operatorID == "" || in.UserID != r.operatorID {
		return
	}
	if r.approver != nil {
		r.approver.tryInteraction(in)
	}
}

// start launches a background turn unless one is active. Returns false if busy.
func (r *relay) start(channelID, text string) bool {
	r.mu.Lock()
	if r.active {
		r.mu.Unlock()
		return false
	}
	base := r.base
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	r.active = true
	r.cancel = cancel
	r.mu.Unlock()

	// Approvals raised by this turn's gated tools post to the operator's DM, and
	// the ctx is STAMPED with the approver so any dangerous action (exec mode)
	// routes its approval to the origin-scoped button approver — never the local
	// TUI broker (so a local "allow" can't leak offsite). Plain read/comms turns
	// carry the stamp harmlessly: nothing dangerous is reachable to trigger it.
	turnCtx := ctx
	if r.approver != nil {
		r.approver.setChannel(channelID)
		turnCtx = withOffsiteConfirm(ctx, r.approver.Confirm)
	}

	go func() {
		defer r.finish(cancel)
		res, err := r.runner.Turn(turnCtx, agent.TurnRequest{UserQuery: text})
		r.deliver(ctx, channelID, res, err)
	}()
	return true
}

func (r *relay) finish(cancel context.CancelFunc) {
	r.mu.Lock()
	r.active = false
	r.cancel = nil
	r.mu.Unlock()
	cancel()
}

// stopActive cancels the active turn (if any). Returns whether one was running.
func (r *relay) stopActive() bool {
	r.mu.Lock()
	cancel := r.cancel
	was := r.active
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return was
}

// deliver posts a finished turn's answer to its DM channel. A cancelled turn
// (operator "stop") posts nothing here — the stop path already confirmed. v1
// posts ONE chunked final answer + a terminal "(done)"; no live progress.
func (r *relay) deliver(ctx context.Context, channelID string, res *agent.TurnResult, err error) {
	if ctx.Err() != nil {
		return // stopped/cancelled
	}
	if err != nil {
		r.reply(channelID, "the turn failed: "+err.Error())
		return
	}
	text := ""
	if res != nil {
		text = strings.TrimSpace(res.Response)
	}
	if text == "" {
		text = "(no answer)"
	}
	for _, c := range chunkMessage(text, discordMsgLimit) {
		r.reply(channelID, c)
	}
	r.reply(channelID, "(done)")
}

// reply sends a bot message, logging (not surfacing) a transport failure.
func (r *relay) reply(channelID, content string) {
	if r.sender == nil || channelID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.sender.Send(ctx, channelID, content); err != nil && r.log != nil {
		r.log.Warn("discord relay: send failed", "channel", channelID, "err", err)
	}
}

// chunkMessage splits s into pieces no longer than limit runes, preferring to
// break on a newline near the boundary so a chunk doesn't split mid-line.
func chunkMessage(s string, limit int) []string {
	r := []rune(s)
	if len(r) <= limit {
		return []string{s}
	}
	var out []string
	for len(r) > 0 {
		n := limit
		if n > len(r) {
			n = len(r)
		}
		// try to break on the last newline within the window (not too early)
		if n == limit {
			if nl := lastIndexRune(r[:n], '\n'); nl > limit/2 {
				n = nl + 1
			}
		}
		out = append(out, string(r[:n]))
		r = r[n:]
	}
	return out
}

func lastIndexRune(r []rune, target rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == target {
			return i
		}
	}
	return -1
}

// --- rate limiter (per-user token bucket) ---

type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	capacity int
	refill   time.Duration // one token per refill interval
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(capacity int, window time.Duration) *rateLimiter {
	per := window / time.Duration(maxIntPos(capacity, 1))
	return &rateLimiter{buckets: map[string]*bucket{}, capacity: capacity, refill: per}
}

// allow consumes a token for user, refilling by elapsed time. Returns false
// when the bucket is empty (rate exceeded).
func (rl *rateLimiter) allow(user string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b := rl.buckets[user]
	if b == nil {
		b = &bucket{tokens: float64(rl.capacity), last: now}
		rl.buckets[user] = b
	}
	if rl.refill > 0 {
		b.tokens += now.Sub(b.last).Seconds() / rl.refill.Seconds()
		if b.tokens > float64(rl.capacity) {
			b.tokens = float64(rl.capacity)
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func maxIntPos(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// discordSender adapts *discord.Service to messageSender (the relay's replies)
// AND discordIO (the approver's button prompts + interaction ACKs).
type discordSender struct{ svc *discord.Service }

func (d discordSender) Send(ctx context.Context, channelID, content string) error {
	_, err := d.svc.SendMessage(ctx, channelID, content)
	return err
}

func (d discordSender) SendComponents(ctx context.Context, channelID, content string, buttons []discord.Button) (string, error) {
	m, err := d.svc.SendComponents(ctx, channelID, content, buttons)
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

func (d discordSender) RespondInteraction(ctx context.Context, interactionID, token, content string) error {
	return d.svc.RespondInteraction(ctx, interactionID, token, content)
}
