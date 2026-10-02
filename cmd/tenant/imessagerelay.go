package main

// imessagerelay.go is the iMessage autonomous responder (TEN-230, Phase 1):
// text in → agent turn → text out, over the native macOS transport.
//
// The inbound anti-loop layer already exists — imessage.Watcher (watch.go,
// ported from openclaw) yields only actionable messages (is_from_me filter,
// ROWID cursor, echo/dedup cache, allowlist). This file is the "autonomous
// responder" its header flagged as the follow-up: poll the watcher, drive a
// dedicated agent turn per inbound message, reply via the native send path, and
// RecordSent so our own reply never loops back.
//
// Phase 1 fails CLOSED on dangerous tools: every turn runs with a deny-all
// offsite-confirm, so gated tools (os_exec, imessage_send to a third party,
// writes) are refused. Phase 2 (TEN-230) swaps that for a text-confirm handshake
// routed to the operator handle.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/lifecycle"
	"tenant/internal/logs/event"
	"tenant/internal/memory/archive"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/soul"
	"tenant/internal/memory/userprofile"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/plugins/imessage"
	"tenant/internal/runtrace"
)

// imsgPoller yields actionable inbound messages and records our own sends so
// they don't loop. *imessage.Watcher satisfies it.
type imsgPoller interface {
	Poll(ctx context.Context, limit int) ([]imessage.InboundMessage, error)
	RecordSent(chatGUID, text string)
}

// imsgSender sends an iMessage reply. *imessage.nativeService (via the Native
// interface, SendText) satisfies it.
type imsgSender interface {
	SendText(ctx context.Context, chatGUID, text string) (string, error)
}

// imsgRunner is the agent the responder drives. *agent.Agent satisfies it.
type imsgRunner interface {
	Turn(ctx context.Context, req agent.TurnRequest) (*agent.TurnResult, error)
}

// imessageResponder polls the watcher and drives one agent turn per actionable
// inbound message, replying over iMessage. Run owns the poll loop; each turn
// runs in its own goroutine, one at a time.
type imessageResponder struct {
	poller   imsgPoller
	sender   imsgSender
	runner   imsgRunner
	confirm  offsiteConfirm // stamped on each turn ctx
	interval time.Duration  // poll cadence (0 ⇒ 3s)
	pollN    int            // max messages per poll (0 ⇒ 20)
	log      *slog.Logger
	degraded func() bool       // optional: true ⇒ model on echo fallback, refuse turns
	ingest   func(text string) // optional: surface an inbound that drives a turn in the shared activity feed (TUI + dashboard), TEN-232
	// approver is the Phase-2 text-confirm handshake (TEN-267). nil ⇒ Phase-1
	// (gated tools stay deny-all via confirm). When set, an inbound approval reply
	// ("Y <nonce>" from the operator) is consumed here and NEVER drives a turn, and
	// each turn's prompt-chat is pinned so a gated tool texts the right chat.
	approver *imessageApprover
	// allowed reports whether a sender may drive a turn: the live allowlist or
	// the operator (TEN-485). nil ⇒ nobody drives (fail closed).
	allowed func(handle string) bool
	// passive, when it reports true, makes every text observe-only (TEN-485).
	passive func() bool

	pollErr string // the last poll error, so a failing poll is recorded once, not every few seconds; Run's goroutine only

	mu     sync.Mutex
	active bool           // one turn at a time — a second text mid-turn gets a "busy" reply
	wg     sync.WaitGroup // in-flight turns (waitIdle, for tests)
	sendMu sync.Mutex     // serializes SendText: replies, "busy" notices and approver prompts
}

// Run polls on a ticker until ctx is cancelled: one Responder activity.
func (r *imessageResponder) Run(ctx context.Context) {
	iv := r.interval
	if iv <= 0 {
		iv = 3 * time.Second
	}
	ctx, act := event.Start(ctx, imessage.Responder, "interval", iv)
	defer act.End(nil)
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.drain(ctx)
		}
	}
}

// drain processes one poll's worth of actionable messages, sequentially.
func (r *imessageResponder) drain(ctx context.Context) {
	limit := r.pollN
	if limit <= 0 {
		limit = 20
	}
	pctx, act := event.Start(ctx, imessage.Poll)
	msgs, err := r.poller.Poll(pctx, limit)
	act.End(&err)
	if err != nil {
		if err.Error() != r.pollErr {
			imessage.PollFailed.Log(ctx, "err", err)
			trackIMessage(ctx, lifecycle.Degraded, err)
		}
		r.pollErr = err.Error()
		return
	}
	if r.pollErr != "" {
		trackIMessage(ctx, lifecycle.Running, nil) // polls work again
	}
	r.pollErr = ""
	for _, m := range msgs {
		if ctx.Err() != nil {
			return
		}
		r.handle(ctx, m)
	}
}

// handle routes one inbound message: an approval reply resolves the pending
// prompt; otherwise it drives a turn — OFF the poll loop. A turn blocked on an
// approval needs the loop to keep draining so the operator's reply can reach
// the approver; run inline, the loop would be stuck behind the very turn that
// is waiting for it (the Phase-2 deadlock). One turn at a time.
func (r *imessageResponder) handle(ctx context.Context, m imessage.InboundMessage) {
	text := strings.TrimSpace(m.Text)
	if text == "" || strings.TrimSpace(m.ChatGUID) == "" {
		return // nothing actionable (attachment-only, empty, or unknown chat)
	}
	// TEN-485: anything that may not drive is observed — logged (log alerts can
	// act on it) and shown in the feed — and never answered. Approval replies go
	// through the same gate, so a spoofable SMS or group text can't approve.
	if why := r.undrivable(m); why != "" {
		imessage.Observed.Log(ctx, "row", m.RowID, "reason", why, "from", m.From, "chat", m.ChatGUID, "text", text)
		if r.ingest != nil {
			r.ingest(m.From + " (" + why + "): " + text)
		}
		return
	}
	imessage.Received.Log(ctx, "row", m.RowID, "bytes", len(text), "from", m.From, "chat", m.ChatGUID, "text", text)
	// Phase-2 (TEN-267): an approval reply ("Y <nonce>" from the OPERATOR handle)
	// resolves a pending gated action and is CONSUMED here — it must never drive a
	// new agent turn. OnReply enforces the operator-handle gate itself; it echoes
	// the outcome to the operator, so we just stop. A non-operator reply, or a
	// non-approval message, returns false and falls through to the normal path.
	// Turns run in their own goroutine (below), so this loop keeps polling while
	// a turn waits on an approval — that is what lets the reply arrive at all.
	if r.approver != nil && r.approver.OnReply(text, m.From) {
		return
	}
	// Refuse rather than answer with a stub while the model is degraded — the
	// remote texter never sees the console banner (mirrors the Discord relay).
	if r.degraded != nil && r.degraded() {
		r.send(ctx, m.ChatGUID, "⚠ the model is unavailable on the host right now (running on a local fallback). Try again once it's back.")
		return
	}
	r.mu.Lock()
	if r.active {
		r.mu.Unlock()
		r.send(ctx, m.ChatGUID, "⏳ still working on your last message — I'll answer when it's done.")
		return
	}
	r.active = true
	r.mu.Unlock()
	// Surface the inbound in the shared activity feed (TUI + dashboard) before
	// driving the turn, so the operator sees offsite traffic live (TEN-232).
	if r.ingest != nil {
		r.ingest(text)
	}
	// Phase-2 (TEN-267): pin THIS turn's chat as the approval-prompt target, so a
	// gated tool fired during the turn texts the prompt to the chat that drove it.
	// (Phase-1: approver is nil and gated tools stay deny-all via confirm.)
	if r.approver != nil {
		r.approver.setChat(m.ChatGUID)
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			r.active = false
			r.mu.Unlock()
		}()
		r.turn(ctx, m.ChatGUID, text)
	}()
}

// undrivable reports why m only gets observed, or "" when it drives a turn:
// passive mode, a group or non-iMessage chat, or a sender off the allowlist.
func (r *imessageResponder) undrivable(m imessage.InboundMessage) string {
	if r.passive != nil && r.passive() {
		return "passive"
	}
	if why := imessage.Undrivable(m); why != "" {
		return why
	}
	if r.allowed == nil || !r.allowed(m.From) {
		return "not_allowed"
	}
	return ""
}

// turn drives one agent turn, a Turn activity, and replies. The turn ctx
// carries the offsite-confirm: Phase-1 deny-all, or (Phase-2) the per-category
// broker whose "ask" texts the operator the nonce handshake.
func (r *imessageResponder) turn(ctx context.Context, chatGUID, text string) {
	turnCtx, act := event.Start(withExecSurface(withOffsiteConfirm(ctx, r.confirm), surfaceIMessage), imessage.Turn, "chat", chatGUID)
	res, err := r.runner.Turn(turnCtx, agent.TurnRequest{
		UserQuery: text, RouteSurface: agent.RouteSurfaceRelay,
	})
	failed := err
	if ctx.Err() != nil {
		failed = nil // the responder stopped: not a failure
	}
	act.End(&failed)
	if err != nil {
		r.send(ctx, chatGUID, "sorry — I hit an error handling that. Try again.")
		return
	}
	reply := strings.TrimSpace(res.Response)
	if reply == "" {
		reply = "(no response)"
	}
	r.send(ctx, chatGUID, reply)
}

// SendText is the one iMessage send path — the responder's replies and the
// approver's prompts (it is the approver's sender) — serialized, since the
// approver prompts from the turn goroutine while the poll loop may be sending
// a "busy" reply, and recorded so the watcher's echo cache drops the text if
// Apple surfaces it back (anti-loop). Each send is a Delivery activity.
func (r *imessageResponder) SendText(ctx context.Context, chatGUID, text string) (_ string, err error) {
	ctx, act := event.Start(ctx, imessage.Delivery, "bytes", len(text), "chat", chatGUID)
	defer act.End(&err)
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	id, err := r.sender.SendText(ctx, chatGUID, text)
	if err == nil {
		r.poller.RecordSent(chatGUID, text)
	}
	return id, err
}

// send replies best-effort; SendText's Delivery activity records a failure.
func (r *imessageResponder) send(ctx context.Context, chatGUID, text string) {
	_, _ = r.SendText(ctx, chatGUID, text)
}

// imessageAgentDeps are the shared ingredients for the dedicated iMessage agent
// — everything except its own working set. Mirrors discordAgentDeps; fullTools
// is the LIVE registry (TEN-229) so the texted agent sees the same surface as
// the TUI.
type imessageAgentDeps struct {
	router      *model.Router
	soulLive    *soul.Live
	archive     *archive.Writer
	episodic    *episodic.Store
	semantic    *semantic.Store
	runs        *runtrace.Store
	skills      agent.SkillRetriever
	compactor   agent.Compactor
	userProfile *userprofile.Profile
	fullTools   agent.ToolRegistry
	fullDisp    agent.ToolDispatcher
	sysPrompt   string
	log         *slog.Logger
	routeHook   agent.RouteHook
	routeActive agent.RouteActive
	routeSticky agent.RouteSticky
	// events, when set, receives the agent's tool calls/results/finals as agent
	// "tenant-imessage", as the Discord relay's do: the log book and the
	// dashboard feed see what a texted turn did.
	events func(agent.Event)
}

// imessageSysSuffix tells the dedicated agent it is answering over iMessage:
// keep it tight, and dangerous actions need approval (Phase 1 refuses them).
const imessageSysSuffix = "\n\nYou are answering over iMessage (SMS-style). Keep replies concise and " +
	"plain-text — no markdown tables or long code blocks. You can read/search the user's messages and " +
	"use your read/research tools freely. Dangerous or gated actions (running commands, writing files, " +
	"texting a different person) require the operator's approval; if one isn't available, say so plainly " +
	"rather than pretending it's done."

// responderRunnable is the long-poll loop the manager starts/stops.
// *imessageResponder satisfies it.
type responderRunnable interface {
	Run(ctx context.Context)
}

// imessageResponderManager starts/stops the responder live (TEN-230 Phase 1c)
// so `/imessage on|off` works without a relaunch. buildFn opens the native
// transport + Watcher + agent LAZILY on Start — chat.db isn't touched until the
// operator turns it on — and returns a cleanup run on Stop. allowFrom only
// feeds the status line: the responder reads the allowlist per message
// (TEN-485). persist records the intent (imessage.enabled) so it survives
// restart.
type imessageResponderManager struct {
	base      context.Context
	buildFn   func() (responderRunnable, func(), error)
	allowFrom func() []string
	persist   func(enabled bool) error
	// onStart runs after each successful Start: it turns on the agent's
	// imessage_* tools, so reading what the responder sees needs no /enable,
	// flag or relaunch.
	onStart func()

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	cleanup func()
}

// trackIMessage files the responder's state in the Components view
// (lifecycle): running, degraded while chat.db can't be polled, stopped.
func trackIMessage(ctx context.Context, st lifecycle.State, err error) {
	lifecycle.Track(ctx, "imessage responder", lifecycle.Connector, "imessage", st, err)
}

// On reports whether the responder loop is currently running.
func (m *imessageResponderManager) On() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Start builds + launches the responder (idempotent). Persists enabled=true.
func (m *imessageResponderManager) Start() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return "imessage responder is already on", nil
	}
	var allow []string
	if m.allowFrom != nil {
		allow = m.allowFrom()
	}
	r, cleanup, err := m.buildFn()
	if err != nil {
		trackIMessage(m.base, lifecycle.Failed, err)
		return "", err
	}
	base := m.base
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	m.cancel, m.cleanup, m.running = cancel, cleanup, true
	trackIMessage(base, lifecycle.Running, nil)
	go r.Run(ctx)
	if m.onStart != nil {
		m.onStart()
	}
	if m.persist != nil {
		if perr := m.persist(true); perr != nil {
			return "", errNotSaved("imessage responder ON", perr)
		}
	}
	return fmt.Sprintf("imessage responder ON — %d allowed handle(s); gated tools follow /imessage permissions (\"ask\" texts the operator a Y <nonce> prompt when imessage.operator is set)", len(allow)), nil
}

// Stop cancels the loop + releases the transport (idempotent). Persists enabled=false.
func (m *imessageResponderManager) Stop() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running {
		return "imessage responder is already off", nil
	}
	if m.cancel != nil {
		m.cancel()
	}
	if m.cleanup != nil {
		m.cleanup()
	}
	m.cancel, m.cleanup, m.running = nil, nil, false
	trackIMessage(m.base, lifecycle.Stopped, nil)
	if m.persist != nil {
		if perr := m.persist(false); perr != nil {
			return "", errNotSaved("imessage responder OFF", perr)
		}
	}
	return "imessage responder OFF", nil
}

// buildIMessageAgent constructs the dedicated iMessage agent over the live tool
// surface (gating enforced per-turn via the responder's offsite-confirm).
func buildIMessageAgent(d imessageAgentDeps) (*agent.Agent, error) {
	return agent.New(agent.Config{
		AgentID:    "tenant-imessage",
		Router:     d.router,
		SoulLive:   d.soulLive,
		Working:    working.New(),
		Archive:    d.archive,
		Episodic:   d.episodic,
		Semantic:   d.semantic,
		Runs:       d.runs,
		Tools:      d.fullTools,
		Dispatcher: d.fullDisp,
		// A sensitive outbound flow asks the approver stamped on the turn
		// ctx (deny-all in Phase 1, the operator's "Y <nonce>" in Phase 2),
		// like any gated tool. nil here denied every such flow outright
		// (TEN-336).
		ConfirmSensitiveEgress: originConfirm(nil),
		Logger:                 d.log,
		Skills:                 d.skills,
		Compactor:              d.compactor,
		UserProfile:            d.userProfile,
		SystemPrompt:           d.sysPrompt + imessageSysSuffix,
		Observer:               mirrorObserver(d.events, "tenant-imessage", "imessage"),
		RouteHook:              d.routeHook,
		RouteActive:            d.routeActive,
		RouteSticky:            d.routeSticky,
	})
}
