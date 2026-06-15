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
	"tenant/internal/memory/archive"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/soul"
	"tenant/internal/memory/userprofile"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/plugins/imessage"
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
// inbound message, replying over iMessage. Single-goroutine (Run owns the loop),
// so Poll + RecordSent are never called concurrently here.
type imessageResponder struct {
	poller   imsgPoller
	sender   imsgSender
	runner   imsgRunner
	confirm  offsiteConfirm // stamped on each turn ctx; Phase 1 = denyAllConfirm
	interval time.Duration  // poll cadence (0 ⇒ 3s)
	pollN    int            // max messages per poll (0 ⇒ 20)
	log      *slog.Logger
	degraded func() bool // optional: true ⇒ model on echo fallback, refuse turns
}

// denyAllConfirm is the Phase-1 gate: every gated tool is refused offsite until
// the Phase-2 text-confirm handshake lands.
func denyAllConfirm(context.Context, string, string) bool { return false }

// Run polls on a ticker until ctx is cancelled.
func (r *imessageResponder) Run(ctx context.Context) {
	iv := r.interval
	if iv <= 0 {
		iv = 3 * time.Second
	}
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
	msgs, err := r.poller.Poll(ctx, limit)
	if err != nil {
		r.logger().Warn("imessage responder: poll failed", "err", err)
		return
	}
	for _, m := range msgs {
		if ctx.Err() != nil {
			return
		}
		r.handle(ctx, m)
	}
}

// handle drives one turn for one inbound message and replies.
func (r *imessageResponder) handle(ctx context.Context, m imessage.InboundMessage) {
	text := strings.TrimSpace(m.Text)
	if text == "" || strings.TrimSpace(m.ChatGUID) == "" {
		return // nothing actionable (attachment-only, empty, or unknown chat)
	}
	// Refuse rather than answer with a stub while the model is degraded — the
	// remote texter never sees the console banner (mirrors the Discord relay).
	if r.degraded != nil && r.degraded() {
		r.send(ctx, m.ChatGUID, "⚠ the model is unavailable on the host right now (running on a local fallback). Try again once it's back.")
		return
	}
	// Phase 1: deny-all offsite-confirm ⇒ gated/dangerous tools are refused.
	turnCtx := withOffsiteConfirm(ctx, r.confirm)
	res, err := r.runner.Turn(turnCtx, agent.TurnRequest{UserQuery: text})
	if err != nil {
		r.logger().Warn("imessage responder: turn failed", "chat", m.ChatGUID, "err", err)
		r.send(ctx, m.ChatGUID, "sorry — I hit an error handling that. Try again.")
		return
	}
	reply := strings.TrimSpace(res.Response)
	if reply == "" {
		reply = "(no response)"
	}
	r.send(ctx, m.ChatGUID, reply)
}

// send replies and records the send so the watcher's echo cache drops it if
// Apple surfaces it back (anti-loop).
func (r *imessageResponder) send(ctx context.Context, chatGUID, text string) {
	if _, err := r.sender.SendText(ctx, chatGUID, text); err != nil {
		r.logger().Warn("imessage responder: send failed", "chat", chatGUID, "err", err)
		return
	}
	r.poller.RecordSent(chatGUID, text)
}

func (r *imessageResponder) logger() *slog.Logger {
	if r.log != nil {
		return r.log
	}
	return slog.Default()
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
	skills      agent.SkillRetriever
	compactor   agent.Compactor
	userProfile *userprofile.Profile
	fullTools   agent.ToolRegistry
	fullDisp    agent.ToolDispatcher
	sysPrompt   string
	log         *slog.Logger
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
// operator turns it on — and returns a cleanup run on Stop. allowFrom is read at
// each Start so live `/imessage allow` edits take effect on the next on. persist
// records the intent (imessage.enabled) so it survives restart.
type imessageResponderManager struct {
	base      context.Context
	buildFn   func(allowFrom []string) (responderRunnable, func(), error)
	allowFrom func() []string
	persist   func(enabled bool) error
	log       *slog.Logger

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	cleanup func()
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
	r, cleanup, err := m.buildFn(allow)
	if err != nil {
		return "", err
	}
	base := m.base
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	m.cancel, m.cleanup, m.running = cancel, cleanup, true
	go r.Run(ctx)
	if m.persist != nil {
		if perr := m.persist(true); perr != nil && m.log != nil {
			m.log.Warn("imessage responder: persist enabled failed", "err", perr)
		}
	}
	return fmt.Sprintf("imessage responder ON — native; %d allowed handle(s); gated tools require approval (Phase 2)", len(allow)), nil
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
	if m.persist != nil {
		if perr := m.persist(false); perr != nil && m.log != nil {
			m.log.Warn("imessage responder: persist disabled failed", "err", perr)
		}
	}
	return "imessage responder OFF", nil
}

// buildIMessageAgent constructs the dedicated iMessage agent over the live tool
// surface (gating enforced per-turn via the responder's offsite-confirm).
func buildIMessageAgent(d imessageAgentDeps) (*agent.Agent, error) {
	return agent.New(agent.Config{
		AgentID:      "tenant-imessage",
		Router:       d.router,
		SoulLive:     d.soulLive,
		Working:      working.New(),
		Archive:      d.archive,
		Episodic:     d.episodic,
		Semantic:     d.semantic,
		Tools:        d.fullTools,
		Dispatcher:   d.fullDisp,
		Logger:       d.log,
		Skills:       d.skills,
		Compactor:    d.compactor,
		UserProfile:  d.userProfile,
		SystemPrompt: d.sysPrompt + imessageSysSuffix,
	})
}
