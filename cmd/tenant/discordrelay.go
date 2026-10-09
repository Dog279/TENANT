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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/attach"
	"tenant/internal/logs/event"
	"tenant/internal/model"
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

// channelReader reads a channel's latest messages (the catch-up after a
// READY, TEN-499). *discord.Service satisfies it; faked in tests.
type channelReader interface {
	ReadChannel(ctx context.Context, channelID string, limit int) ([]discord.Message, error)
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
	botID      string // the bot's own Discord user ID; set from READY via OnReady
	rl         *rateLimiter
	approver   *discordApprover    // optional; set by the manager (TEN-117/119) — drives the button prompts
	confirm    offsiteConfirm      // optional; the per-category permission broker (TEN-231) the turn's gated tools route through; nil ⇒ fall back to approver.Confirm
	locked     func(string) bool   // optional; that broker's lock, stamped for the tools that don't ask it (withOffsiteLock)
	degraded   func() bool         // optional; when true the model is on the echo fallback
	ingest     func(text string)   // optional; surface an inbound that drives a turn in the shared activity feed (TUI + dashboard), TEN-232
	files      *attachmentIngestor // optional; downloads operator attachments into the relay inbox — nil ⇒ attachments ignored
	images     *attach.Store       // optional; image attachments also go to the model as images (TEN-493)
	dataDir    string              // optional; enables ext-approve button verdicts under <dataDir>/approvals (extapprove.go), the interrupted-turn record and the DM cursor (TEN-499)
	stopping   func() bool         // optional; true while Tenant is stopping (TEN-499): a cut turn is recorded and the operator told
	reader     channelReader       // optional; the catch-up after a READY reads the DMs missed while down (TEN-499)

	mu     sync.Mutex
	base   context.Context // lifetime of the relay; background turns derive from it
	active bool
	cancel context.CancelFunc
	// cursor is the last message id accepted per DM channel (TEN-499), kept
	// in <dataDir>/discord-cursor.json: a message at or before it is one the
	// relay has seen, and the catch-up after a READY starts after it. Only
	// DMs: a guild message's channel is not read back (ReadChannel's messages
	// carry no guild id, and a guild turn needs a mention anyway).
	cursor map[string]string
}

func newRelay(runner relayRunner, sender messageSender, operatorID string) *relay {
	return &relay{
		runner: runner, sender: sender,
		operatorID: strings.TrimSpace(operatorID),
		rl:         newRateLimiter(6, 30*time.Second), // ~6 messages / 30s per user
		base:       context.Background(),
	}
}

// Start records the relay's base context (so Disable cancels in-flight turns)
// and loads the DM cursor. The gateway's OnMessage should be pointed at
// handleInbound after Start.
func (r *relay) Start(ctx context.Context) {
	r.mu.Lock()
	r.base = ctx
	r.cursor = map[string]string{}
	if r.dataDir != "" {
		if b, err := os.ReadFile(r.cursorPath()); err == nil {
			_ = json.Unmarshal(b, &r.cursor) // unreadable ⇒ no cursor: nothing is caught up, nothing is skipped
		} else if !errors.Is(err, fs.ErrNotExist) {
			sysConfigNotLoaded.Log(ctx, "path", r.cursorPath(), "fallback", "no Discord catch-up this start", "err", err)
		}
	}
	r.mu.Unlock()
}

func (r *relay) cursorPath() string { return filepath.Join(r.dataDir, "discord-cursor.json") }

// setBotID records the bot's own Discord user ID (from the READY event via
// the gateway's OnReady callback). Required for guild mention detection + stripping.
func (r *relay) setBotID(id string) {
	r.mu.Lock()
	r.botID = id
	r.mu.Unlock()
}

// ready is the gateway's OnReady: the bot id, then the catch-up. It runs on
// the gateway's dispatch goroutine on purpose: READY and the MESSAGE_CREATEs
// after it are dispatched in order by the same loop, so a live message
// cannot be routed ahead of the missed ones (which would move the cursor
// past them); the loop is held for the catch-up's bounded fetch only.
func (r *relay) ready(botID string) {
	r.setBotID(botID)
	r.catchUp()
}

// snowflake is a Discord id as a number: ids are time-ordered, so a bigger
// one is a later message. 0 when it is not one (an empty id, a test's).
func snowflake(id string) uint64 {
	n, _ := strconv.ParseUint(id, 10, 64)
	return n
}

// accept reports whether a DM message is new to the relay and, if so, moves
// the channel's cursor past it and saves the cursor. An id that is not a
// snowflake is always new, and moves nothing.
func (r *relay) accept(channelID, messageID string) bool {
	id := snowflake(messageID)
	if id == 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id <= snowflake(r.cursor[channelID]) {
		return false
	}
	r.cursor[channelID] = messageID
	if r.dataDir != "" {
		if b, err := json.Marshal(r.cursor); err == nil {
			if err := atomicWrite(r.cursorPath(), b); err != nil {
				sysNotSaved.Log(context.Background(), "what", "the Discord cursor", "path", r.cursorPath(), "err", err)
			}
		}
	}
	return true
}

// catchUp reads each cursor channel's latest messages and routes the ones
// after the cursor, oldest first, through handleInbound as if they had just
// arrived: a session that identifies anew (after a reboot, or a drop that
// could not resume) has missed every DM sent meanwhile. Bounded, so a slow
// Discord cannot hold the gateway loop long.
func (r *relay) catchUp() {
	r.mu.Lock()
	reader, base := r.reader, r.base
	cursors := make(map[string]string, len(r.cursor))
	for ch, id := range r.cursor {
		cursors[ch] = id
	}
	r.mu.Unlock()
	if reader == nil || len(cursors) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(base, 10*time.Second)
	defer cancel()
	for ch, cur := range cursors {
		msgs, err := reader.ReadChannel(ctx, ch, 20)
		if err != nil {
			discord.CatchUpFailed.Log(ctx, "channel", ch, "err", err)
			continue
		}
		var missed []discord.Message
		for _, m := range msgs {
			if snowflake(m.ID) > snowflake(cur) {
				missed = append(missed, m)
			}
		}
		if len(missed) == 0 {
			continue
		}
		slices.SortFunc(missed, func(a, b discord.Message) int { return cmp.Compare(snowflake(a.ID), snowflake(b.ID)) })
		discord.CaughtUp.Log(ctx, "channel", ch, "n", len(missed))
		for _, m := range missed {
			// ponytail: ReadChannel drops attachments, so a missed message's
			// files are not fetched; the text is. Only DM channels have a
			// cursor, so GuildID stays empty (the DM path).
			r.handleInbound(discord.Inbound{
				MessageID: m.ID, ChannelID: cmp.Or(m.ChannelID, ch),
				AuthorID: m.Author.ID, AuthorBot: m.Author.Bot, Content: m.Content,
			})
		}
	}
}

// handleInbound is the gateway callback. It enforces the H1 inbound boundary
// (drop bots; rate-limit; cap length) then routes the message to a new turn,
// an interjection, or a stop.
//
// DMs: operator-only (single-operator allowlist).
// Guild messages: any server member may @mention the bot; the mention is
// stripped before the text is passed to the agent.
func (r *relay) handleInbound(in discord.Inbound) {
	if in.AuthorBot {
		return // never react to bots (loop guard)
	}

	if in.GuildID != "" {
		// Guild message: only respond when the bot is explicitly mentioned.
		r.mu.Lock()
		bid := r.botID
		r.mu.Unlock()
		if bid == "" {
			return // bot ID not yet known (no READY); can't detect mentions
		}
		if !isMentioned(in.Content, bid) {
			return // not mentioned, ignore
		}
		in.Content = stripMention(in.Content, bid)
	} else {
		// DM: operator-only allowlist, deny-by-default.
		if r.operatorID == "" || in.AuthorID != r.operatorID {
			return
		}
		if !r.accept(in.ChannelID, in.MessageID) {
			return // seen before the restart or the reconnect (TEN-499)
		}
	}

	discord.Received.Log(context.Background(), "message", in.MessageID, "channel", in.ChannelID, "guild", in.GuildID,
		"author", in.AuthorID, "bytes", len(in.Content), "attachments", len(in.Attachments), "text", in.Content)

	// An approval reply (exact nonce / "deny") is consumed by the approver, not
	// routed as a new turn or counted against the rate limit.
	if r.approver != nil && r.approver.tryConsume(in) {
		return
	}
	// Attachments are OPERATOR-ONLY on every surface: guild members may drive
	// text turns via @mention, but only the operator writes files to the host.
	atts := in.Attachments
	if r.files == nil || r.operatorID == "" || in.AuthorID != r.operatorID {
		atts = nil
	}
	text := strings.TrimSpace(in.Content)
	if text == "" && len(atts) == 0 {
		return
	}
	if len(text) > maxInboundLen {
		text = text[:maxInboundLen]
	}
	if !r.rl.allow(in.AuthorID) {
		discord.Throttled.Log(context.Background(), "author", in.AuthorID, "channel", in.ChannelID)
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

	if len(atts) > 0 {
		// Download OFF the gateway goroutine — a 100 MB fetch here would
		// stall the socket's read loop (missed heartbeats → reconnect).
		go r.ingestAndRoute(in.ChannelID, in.MessageID, text, atts)
		return
	}
	r.routeTurn(in.ChannelID, in.MessageID, text)
}

// ingestAndRoute downloads the message's attachments, appends the saved-file
// manifest to the operator's text, and routes the combined turn. Runs on its
// own goroutine (see handleInbound); per-batch deadline so a dead CDN can't
// wedge the pipeline.
func (r *relay) ingestAndRoute(channelID, messageID, text string, atts []discord.Attachment) {
	r.mu.Lock()
	base := r.base
	r.mu.Unlock()
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, attachmentIngestTimeout)
	defer cancel()

	saved, rejected := r.files.Ingest(ctx, atts)
	if len(saved) == 0 && text == "" {
		// Attachment-only message, nothing usable — answer directly instead
		// of driving a turn about files that don't exist.
		msg := "no attachments accepted (allowed: png, jpg, gif, webp, heic, pdf, csv, txt, mp4; max 100 MB)."
		if len(rejected) > 0 {
			msg = "no attachments accepted: " + strings.Join(rejected, "; ")
		}
		r.reply(channelID, msg)
		return
	}
	full := text
	if manifest := attachmentManifest(saved, rejected); manifest != "" {
		if full != "" {
			full += "\n\n"
		}
		full += manifest
	}
	images, notes := r.imagesOf(ctx, saved)
	for _, n := range notes {
		full += "\n" + n
	}
	r.routeTurnWith(channelID, messageID, full, images)
}

// imagesOf turns the saved files that are images into model images
// (TEN-493); the text manifest still lists every file. One that can't be
// shown (too large, HEIC off macOS) comes back as a note instead.
func (r *relay) imagesOf(ctx context.Context, saved []savedAttachment) ([]model.Image, []string) {
	if r.images == nil {
		return nil, nil
	}
	var imgs []model.Image
	var notes []string
	for _, s := range saved {
		data, err := os.ReadFile(s.Path) // the file this relay just downloaded
		if err != nil || attach.Sniff(data) == "" {
			continue
		}
		img, err := r.images.Add(ctx, data, filepath.Base(s.Path))
		if err != nil {
			notes = append(notes, fmt.Sprintf("[image %s not shown: %v]", filepath.Base(s.Path), err))
			continue
		}
		imgs = append(imgs, img)
	}
	return imgs, notes
}

// routeTurn surfaces text in the shared activity feed, then starts a turn or
// folds into the running one — the shared tail of every inbound path.
func (r *relay) routeTurn(channelID, messageID, text string) {
	r.routeTurnWith(channelID, messageID, text, nil)
}

// routeTurnWith is routeTurn with the message's images (TEN-493).
func (r *relay) routeTurnWith(channelID, messageID, text string, images []model.Image) {
	if r.ingest != nil {
		r.ingest(text)
	}
	if r.start(channelID, messageID, text, images) {
		return // new turn launched; its goroutine posts the answer
	}
	// A turn is already running. Single operator ⇒ it's theirs ⇒ fold this in.
	interjectWithImages(r.runner, text, images)
	r.reply(channelID, "added to the running turn.")
}

// handleInteraction routes a button click to the approver — but ONLY the
// operator's clicks count (defense-in-depth; in a DM only they can click).
// The "ext-" namespace belongs to external-automation approvals
// (extapprove.go) and is routed before the nonce approver so an external
// click can't be mis-read as a stale approval.
func (r *relay) handleInteraction(in discord.Interaction) {
	if r.operatorID == "" || in.UserID != r.operatorID {
		// Not the operator: never act, but ACK privately — an un-ACKed click
		// shows "This interaction failed" to whoever tapped it.
		if r.approver != nil {
			r.approver.ackEphemeral(in, "Only the operator can approve this.")
		}
		return
	}
	if strings.HasPrefix(in.CustomID, "ext-") {
		r.handleExtApprove(in)
		return
	}
	if r.approver != nil && r.approver.tryInteraction(in) {
		return
	}
	if r.approver != nil {
		r.approver.ackEphemeral(in, "That button isn't active anymore.")
	}
}

// start launches a background turn unless one is active. Returns false if busy.
func (r *relay) start(channelID, messageID, text string, images []model.Image) bool {
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
		r.approver.setChannel(channelID) // the button prompts post to THIS turn's channel
	}
	// Route gated tools through the per-category broker (TEN-231) when wired —
	// allow/deny resolve without a prompt, "ask" delegates to the button approver.
	// Fall back to the raw button approver for older wiring / tests.
	approvals := "NONE (gated actions will prompt the host broker)"
	switch {
	case r.confirm != nil:
		turnCtx = withOffsiteConfirm(ctx, r.confirm)
		approvals = "broker"
	case r.approver != nil:
		turnCtx = withOffsiteConfirm(ctx, r.approver.Confirm)
		approvals = "approver"
	}
	turnCtx = withExecSurface(withOffsiteLock(turnCtx, r.locked), surfaceDiscord) // picks the discord exec profile

	go func() {
		defer r.finish(cancel)
		// The turn is a RelayTurn activity naming where its approvals go — the
		// diagnostic that was missing when prompts seemed to land on the wrong
		// surface. A turn the operator stopped is not a failure.
		actCtx, act := event.Start(turnCtx, discord.RelayTurn, "channel", channelID, "approvals", approvals)
		res, err := r.runner.Turn(actCtx, agent.TurnRequest{
			UserQuery: text, RouteSurface: agent.RouteSurfaceRelay, Images: images,
		})
		failed := err
		if ctx.Err() != nil {
			failed = nil
		}
		act.End(&failed)
		if ctx.Err() != nil && r.stopping != nil && r.stopping() {
			// Tenant stopping cut the turn (TEN-499): record it for the next
			// start and tell the operator now, on a fresh, short context —
			// the turn's is gone and the teardown is waiting.
			if err := recordInterrupted(r.dataDir, newInterrupted("discord", channelID, messageID, text, res)); err != nil {
				sysNotSaved.Log(context.Background(), "what", "the interrupted-turn record", "path", interruptedPath(r.dataDir), "err", err)
			}
			r.replyWithin(channelID, stopNotice, 2*time.Second)
			return
		}
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
// posts ONE chunked final answer; no live progress.
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
		text = strings.TrimSpace(stripStatusToken(res.Response))
	}
	if text == "" {
		text = "(no answer)"
	}
	for _, c := range chunkMessage(text, discordMsgLimit) {
		r.reply(channelID, c)
	}
}

// reply sends a bot message best-effort: a transport failure is not surfaced,
// and discord.Service.SendMessage's Delivery activity records it.
func (r *relay) reply(channelID, content string) { r.replyWithin(channelID, content, 15*time.Second) }

// replyWithin is reply with its own deadline (the stop notice gets 2 s).
func (r *relay) replyWithin(channelID, content string, timeout time.Duration) {
	if r.sender == nil || channelID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = r.sender.Send(ctx, channelID, content)
}

// stripStatusToken removes trailing status-escalation tokens that the agent
// appends to satisfy the status-escalation skill (e.g. "(done)", "DONE",
// "STATUS: DONE"). These are internal conventions — never meant for Discord.
func stripStatusToken(s string) string {
	// Strip trailing "STATUS: <TOKEN>" lines first (the multi-line escalation format).
	s = regexp.MustCompile(`(?im)^\s*STATUS:\s*(DONE|DONE_WITH_CONCERNS|BLOCKED|NEEDS_CONTEXT)\b.*$`).ReplaceAllString(s, "")
	// Strip a trailing standalone token on its own line: "(done)", "DONE", etc.
	s = regexp.MustCompile(`(?im)\n+\s*\(?(DONE|DONE_WITH_CONCERNS|BLOCKED|NEEDS_CONTEXT)\)?\s*$`).ReplaceAllString(s, "")
	return strings.TrimSpace(s)
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

// isMentioned checks whether content contains a mention of the bot's user ID.
// Discord mentions appear as <@ID> or <@!ID>.
func isMentioned(content, botID string) bool {
	return strings.Contains(content, "<@"+botID+">") ||
		strings.Contains(content, "<@!"+botID+">")
}

// stripMention removes a leading <@!ID> or <@ID> mention prefix from content,
// returning the trimmed remainder. If the mention isn't at position zero, the
// content is returned as-is (the mention is mid-message and we leave it).
func stripMention(content, botID string) string {
	for _, prefix := range []string{"<@!" + botID + ">", "<@" + botID + ">"} {
		if strings.HasPrefix(content, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(content, prefix))
		}
	}
	return strings.TrimSpace(content)
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

func (d discordSender) SendCard(ctx context.Context, channelID, content string, buttons []discord.Button) (string, error) {
	m, err := d.svc.SendCard(ctx, channelID, content, buttons)
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

func (d discordSender) RespondUpdate(ctx context.Context, interactionID, token, content string, buttons []discord.Button, v2 bool) error {
	return d.svc.RespondUpdate(ctx, interactionID, token, content, buttons, v2)
}

func (d discordSender) RespondEphemeral(ctx context.Context, interactionID, token, text string) error {
	return d.svc.RespondEphemeral(ctx, interactionID, token, text)
}

func (d discordSender) EditMessage(ctx context.Context, channelID, messageID, content string, buttons []discord.Button, v2 bool) error {
	return d.svc.EditMessage(ctx, channelID, messageID, content, buttons, v2)
}
