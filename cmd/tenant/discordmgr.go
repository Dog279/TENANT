package main

// discordmgr.go is TEN-119 (T5): the lifecycle manager for the Discord relay,
// mirroring dashboardManager. It builds the DEDICATED Discord agent once (shared
// long-term memory, own working set, restricted+approver-wired tools), then
// Enable/Disable start/stop the gateway+relay under a child context. Implements
// tui.RelayControl so `/relay on|off|status|allow <id>` drives it at runtime.
//
// Safety: Enable FAILS LOUDLY if no operator id is set (never run a bot that
// answers no one), and if Discord isn't configured.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/lifecycle"
	"tenant/internal/memory/archive"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/soul"
	"tenant/internal/memory/userprofile"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/plugins/discord"
	"tenant/internal/runtrace"
	"tenant/internal/tui"
)

// discordAgentDeps are the shared ingredients for the dedicated Discord agent —
// everything except its own working set + restricted tools. Sourced from the
// serve path's main-agent construction.
type discordAgentDeps struct {
	router      *model.Router
	soulLive    *soul.Live
	archive     *archive.Writer
	episodic    *episodic.Store
	semantic    *semantic.Store
	runs        *runtrace.Store
	skills      agent.SkillRetriever
	compactor   agent.Compactor
	userProfile *userprofile.Profile
	// fullTools is the LIVE tool registry (the running mux), not a snapshot —
	// so MCP/Jira tools that come online after the relay starts are visible to
	// the Discord agent (TEN-229).
	fullTools   agent.ToolRegistry
	fullDisp    agent.ToolDispatcher
	sysPrompt   string
	log         *slog.Logger
	routeHook   agent.RouteHook
	routeActive agent.RouteActive
	routeSticky agent.RouteSticky
	// events, when set, mirrors relay-turn tool calls/results/finals to the
	// dashboard activity feed as agent "tenant-discord" - the third silent
	// surface after cron and eval: a Discord-driven turn showed only its
	// ingest line, never what it actually did.
	events func(agent.Event)
	// gateConfirm is the per-category permission broker's Confirm (TEN-231): the
	// discord_* tools route through it (allow → run, deny → block, ask → button)
	// exactly like every other gated tool, instead of always prompting a button.
	// nil ⇒ fall back to the raw button approver (tests / unwired).
	gateConfirm offsiteConfirm
}

const discordSysSuffix = " You are reachable over Discord while the operator is away from the machine. " +
	"You have the same tools as the local TUI. While the operator has Discord exec mode off, running " +
	"commands, writing files, irreversible actions and sends other than Discord posts are refused; they " +
	"turn it on with /relay exec on at the machine. Otherwise a dangerous action may need the operator's " +
	"approval (a button tap that can also time out and deny). Keep answers concise for a phone screen."

// buildDiscordAgent constructs the dedicated Discord agent + Discord service +
// nonce approver + reply sender. Shares the long-term memory stores (continuity)
// but gets its OWN working set and the restricted, approver-wired tool surface.
func buildDiscordAgent(token string, d discordAgentDeps) (relayRunner, *discord.Service, *discordApprover, messageSender, error) {
	svc, err := discord.Open(discord.Config{Token: token})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	sender := discordSender{svc: svc}
	approver := newDiscordApprover(sender, d.log)

	// discord_* tools route through the per-category broker (TEN-231): allow runs
	// silently, deny blocks, ask pops the Discord button. Every other tool
	// delegates to d.fullDisp (the live mux); a gated call there hits originConfirm
	// (wired at the mux), which routes its approval to the SAME broker via the
	// offsite-stamped ctx. Unwired (tests) ⇒ fall back to the raw button approver.
	gateConfirm := d.gateConfirm
	if gateConfirm == nil {
		gateConfirm = approver.Confirm
	}
	ddisc := discord.NewDispatcher(svc, discord.Policy{Confirm: gateConfirm})
	reg, restDisp := restrictForDiscord(d.fullTools, d.fullDisp)
	disp := &discordRoutingDispatcher{discord: ddisc, rest: restDisp}

	var obs func(agent.Event)
	if d.events != nil {
		obs = func(e agent.Event) {
			switch e.Kind {
			case agent.EventToolCall, agent.EventToolResult, agent.EventFinal, agent.EventError:
				e.Agent, e.Source = "tenant-discord", "discord"
				d.events(e)
			}
		}
	}
	ag, err := agent.New(agent.Config{
		AgentID:    "tenant-discord",
		Router:     d.router,
		SoulLive:   d.soulLive,
		Working:    working.New(),
		Archive:    d.archive,
		Episodic:   d.episodic,
		Semantic:   d.semantic,
		Runs:       d.runs,
		Tools:      reg,
		Dispatcher: disp,
		// A sensitive outbound flow asks the approver stamped on the turn
		// ctx (the Discord broker), like any gated tool; unstamped fails
		// closed. nil here denied every such flow outright (TEN-336).
		ConfirmSensitiveEgress: originConfirm(nil),
		Logger:                 d.log,
		Skills:                 d.skills,
		Compactor:              d.compactor,
		UserProfile:            d.userProfile,
		SystemPrompt:           d.sysPrompt + discordSysSuffix,
		Observer:               obs,
		RouteHook:              d.routeHook,
		RouteActive:            d.routeActive,
		RouteSticky:            d.routeSticky,
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return ag, svc, approver, sender, nil
}

// discordRelayManager owns the relay lifecycle. tui.RelayControl.
type discordRelayManager struct {
	base     context.Context
	runner   relayRunner
	svc      *discord.Service
	approver *discordApprover
	sender   messageSender
	// broker is the per-category permission broker (TEN-231) that fronts every
	// gated tool the Discord agent reaches. Its "ask" backend is askOperator (the
	// button approver). Stable across Reconfigure (only the approver swaps), so
	// /relay permissions modes survive a token hot-swap. nil ⇒ pre-TEN-231 path.
	broker  *approvalBroker
	token   string
	dataDir string // enables attachment ingest into <dataDir>/relay-inbox when set
	// inboxKeep is the relay-inbox retention (TEN-309); nil = 30 days.
	inboxKeep func() time.Duration
	log       *slog.Logger
	notify    func(string)
	ingest    func(text string) // surface inbound DMs in the shared activity feed (TUI + dashboard), TEN-232
	degraded  func() bool       // when true, the model is on the echo fallback; relay refuses turns
	persist   func(enabled bool, operatorID string, allowExec bool) error

	// buildFn rebuilds the Discord agent + gateway internals with a fresh token.
	// Set at the serve wiring point so Reconfigure can hot-swap after a live
	// /skill configure discord without restarting the process.
	buildFn func(token string) (relayRunner, *discord.Service, *discordApprover, messageSender, error)

	// start is the test seam (real = realStart: wire+run gateway/relay).
	start func(ctx context.Context, operatorID string) error

	mu         sync.Mutex
	running    bool
	cancel     context.CancelFunc
	operatorID string
	allowExec  bool // exec mode (relay.allow_exec): the broker's lock reads it via execLocked

	// approvalTimeout / cardStyle are the operator's approval-card knobs
	// (relay.approval_timeout, relay.card_style), applied to the live approver
	// on every (re)start. Zero / "" ⇒ the approver's defaults (10m, v2).
	approvalTimeout time.Duration
	cardStyle       string
	persistTimeout  func(d time.Duration) error // writes relay.approval_timeout

	dmChannel string // the operator's DM channel, opened on first NotifyOperator
}

// NotifyOperator DMs the operator directly — the out-of-band channel for
// conditions no in-flight turn will report, like a connector that needs the
// operator's browser to re-authorize. Best-effort beside the feed line: it
// fails (never blocks) when the relay isn't configured.
func (m *discordRelayManager) NotifyOperator(text string) error {
	m.mu.Lock()
	svc, op, dm := m.svc, strings.TrimSpace(m.operatorID), m.dmChannel
	m.mu.Unlock()
	if svc == nil || op == "" {
		return fmt.Errorf("discord relay not configured (no bot or no operator)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if dm == "" {
		id, err := svc.OpenDM(ctx, op)
		if err != nil {
			return fmt.Errorf("open operator DM: %w", err)
		}
		m.mu.Lock()
		m.dmChannel = id
		m.mu.Unlock()
		dm = id
	}
	_, err := svc.SendMessage(ctx, dm, text)
	return err
}

// realStart wires a fresh relay (bound to operatorID) + gateway and runs the
// gateway under ctx. Returns once started (the gateway connects asynchronously).
func (m *discordRelayManager) realStart(ctx context.Context, operatorID string) error {
	rl := newRelay(m.runner, m.sender, operatorID)
	rl.approver = m.approver
	if m.approver != nil {
		// A prompt that degrades to text surfaces on the operator feed, and the
		// card knobs follow the config across restarts and token swaps.
		m.approver.notify = m.notify
		if m.approvalTimeout > 0 {
			m.approver.timeout = m.approvalTimeout
		}
		if m.cardStyle != "" {
			m.approver.cardStyle = m.cardStyle
		}
	}
	if m.broker != nil {
		rl.confirm = m.broker.Confirm // gated tools route through the per-category broker (TEN-231)
	}
	rl.ingest = m.ingest // surface inbound DMs in the shared activity feed (TEN-232)
	rl.degraded = m.degraded
	if m.dataDir != "" {
		// Operator file ingest: DMed attachments land in <dataDir>/relay-inbox
		// and the turn text carries the saved-path manifest.
		rl.files = newAttachmentIngestor(m.dataDir)
		rl.files.keep = m.inboxKeep
		// External-automation approvals: ext-approve button verdicts land under
		// <dataDir>/approvals/<ns> (extapprove.go).
		rl.dataDir = m.dataDir
	}
	rl.Start(ctx)
	gw := &discord.Gateway{
		Token:         m.token,
		GetURL:        m.svc.GatewayURL,
		OnMessage:     rl.handleInbound,
		OnInteraction: rl.handleInteraction,
		OnReady:       rl.setBotID,
		OnFatal: func(e error) {
			m.track(lifecycle.Failed, e)
			if m.notify != nil {
				m.notify("discord relay stopped: " + e.Error())
			}
		},
	}
	// Run returns nil once ctx ends; an error is a fatal close, which OnFatal
	// already told the operator and Run's Connection activity records as failed.
	go gw.Run(ctx)
	if m.notify != nil {
		m.notify("discord relay: connecting (operator " + operatorID + ")")
	}
	return nil
}

// Enable starts the relay. FAILS LOUDLY with no operator id (never a silent bot
// that answers no one) or with Discord unconfigured.
func (m *discordRelayManager) Enable() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return nil
	}
	if strings.TrimSpace(m.operatorID) == "" {
		return fmt.Errorf("no operator set — run `/relay allow <your-discord-user-id>` first")
	}
	if m.runner == nil {
		return fmt.Errorf("discord not configured — run `/skill configure discord <bot-token>`")
	}
	start := m.start
	if start == nil {
		start = m.realStart
	}
	dctx, cancel := context.WithCancel(m.base)
	if err := start(dctx, m.operatorID); err != nil {
		cancel()
		return err
	}
	m.running = true
	m.cancel = cancel
	m.track(lifecycle.Running, nil)
	if m.persist != nil {
		if err := m.persist(true, m.operatorID, m.allowExec); err != nil {
			return errNotSaved("relay: turning the relay on", err)
		}
	}
	return nil
}

// Disable stops a running relay (cancel its context) and persists "off".
func (m *discordRelayManager) Disable() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		m.cancel()
		m.running = false
		m.track(lifecycle.Stopped, nil)
	}
	if m.persist != nil {
		return m.persist(false, m.operatorID, m.allowExec)
	}
	return nil
}

// Shutdown stops a running relay for PROCESS teardown (serve SIGTERM/Ctrl-C,
// exit) WITHOUT persisting an "off" choice. A restart is not the operator
// disabling the relay, so the saved enabled=true must survive — otherwise
// every graceful serve stop silently wrote relay.enabled=false and the bot
// came back offline. Mirrors dashboardManager.Shutdown (same bug, same fix).
func (m *discordRelayManager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		m.cancel()
		m.running = false
		m.track(lifecycle.Stopped, nil)
	}
}

// track files the relay's state in the Components view (lifecycle).
func (m *discordRelayManager) track(st lifecycle.State, err error) {
	lifecycle.Track(m.base, "discord relay", lifecycle.Connector, "discord", st, err)
}

// ExecOn reports whether offsite exec mode is on.
func (m *discordRelayManager) ExecOn() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.allowExec
}

// execLocked is the Discord broker's lock (approvalBroker.locked): with exec
// mode off it refuses what relayExecLocks names, before any tool rule or
// relay.permissions mode is consulted.
func (m *discordRelayManager) execLocked(action, cat string) bool {
	return !m.ExecOn() && relayExecLocks(action, cat)
}

// Status reports whether the relay is running, whether an operator id is set,
// and whether offsite exec mode is on.
func (m *discordRelayManager) Status() (running bool, operatorSet bool, execOn bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running, strings.TrimSpace(m.operatorID) != "", m.allowExec
}

// SetExec switches offsite exec mode and persists it. It takes effect on the
// next gated call, including one in a turn already running: the Discord
// broker's lock (execLocked) reads the mode on every call. The tools stay
// visible either way (TEN-229); the broker refuses what exec mode keeps closed.
func (m *discordRelayManager) SetExec(on bool) error { return m.setExecFrom(on, "tui") }

// setExecFrom is SetExec from surface (tui or dashboard), which the change's
// Security record names.
func (m *discordRelayManager) setExecFrom(on bool, surface string) error {
	if m.runner == nil {
		return fmt.Errorf("discord not configured — run `/skill configure discord <bot-token>` first")
	}
	m.mu.Lock()
	m.allowExec = on
	running, opID := m.running, m.operatorID
	m.mu.Unlock()
	auditRelayExec(on, surface)
	if m.persist != nil {
		return m.persist(running, opID, on)
	}
	return nil
}

// ReconfigureAndStart rebuilds with a new token, sets the operator, and starts
// the relay. Called after /configure discord completes (token + operator_id
// both provided). If operatorID is empty, falls back to Reconfigure-only
// (preserves the old behavior for token-only updates).
func (m *discordRelayManager) ReconfigureAndStart(token, operatorID string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("discord: token required")
	}
	if strings.TrimSpace(operatorID) != "" {
		if err := m.SetOperator(operatorID); err != nil {
			return fmt.Errorf("discord: set operator: %w", err)
		}
	}
	if err := m.Reconfigure(token); err != nil {
		return fmt.Errorf("discord: rebuild: %w", err)
	}
	// Auto-enable if operator is set and relay isn't already running.
	running, _, _ := m.Status()
	if !running && strings.TrimSpace(m.operatorID) != "" {
		if err := m.Enable(); err != nil {
			return fmt.Errorf("discord: auto-enable: %w", err)
		}
	}
	return nil
}

// Reconfigure rebuilds the Discord agent + gateway with a new token, hot-
// swapping the old one if the relay was running. Called after a successful
// /skill configure discord. If the relay was off, it just swaps the internals
// (so the next /relay on uses the new token). If it was on, it stops the old
// gateway, rebuilds, and restarts.
func (m *discordRelayManager) Reconfigure(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("discord: token required")
	}
	if m.buildFn == nil {
		return fmt.Errorf("discord: reconfigure not wired (no buildFn)")
	}

	// Stop the old gateway if running (cancel its context).
	m.mu.Lock()
	wasRunning := m.running
	if wasRunning && m.cancel != nil {
		m.cancel()
	}
	m.running = false
	m.cancel = nil
	m.mu.Unlock()

	// Rebuild outside the lock — buildFn calls discord.Open (network).
	runner, svc, appr, snd, err := m.buildFn(token)
	if err != nil {
		if m.notify != nil {
			m.notify("discord relay: reconfigure FAILED — " + err.Error())
		}
		return fmt.Errorf("discord: rebuild agent: %w", err)
	}

	m.mu.Lock()
	m.runner = runner
	m.svc = svc
	m.approver = appr
	m.sender = snd
	m.token = token
	m.mu.Unlock()

	if wasRunning {
		// Restart with the new token.
		start := m.start
		if start == nil {
			start = m.realStart
		}
		m.mu.Lock()
		dctx, cancel := context.WithCancel(m.base)
		if err := start(dctx, m.operatorID); err != nil {
			cancel()
			m.mu.Unlock()
			return fmt.Errorf("discord: restart relay: %w", err)
		}
		m.running = true
		m.cancel = cancel
		m.mu.Unlock()
		if m.notify != nil {
			m.notify("discord relay: reconnected with new token")
		}
	}
	return nil
}

// Perms exposes the per-category permission broker (TEN-231) so /relay
// permissions drives it with the SAME ask|allow|deny syntax as /permissions.
// nil when Discord is unconfigured (no broker wired).
func (m *discordRelayManager) Perms() tui.PermissionControl {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.broker == nil {
		return nil
	}
	return m.broker
}

// askOperator is the broker's "ask"-mode backend: it raises a per-action button
// approval to the operator in Discord via the LIVE approver (re-read under the
// lock so a Reconfigure token swap is picked up). It denies when the relay
// isn't built or the operator can't be reached — fail closed — and says
// Expired when the card timed out, so the record isn't an operator's deny.
func (m *discordRelayManager) askOperator(ctx context.Context, action, detail string) tui.ApprovalDecision {
	m.mu.Lock()
	appr := m.approver
	m.mu.Unlock()
	if appr == nil {
		return tui.DenyOnce
	}
	return appr.decide(ctx, action, detail)
}

// Configured reports whether a relay agent is built (a bot token is set) — the
// relay can actually run. Used by the dashboard Access page (TEN-208) to gate
// the relay controls (vs. the broker, which exists even without a token).
func (m *discordRelayManager) Configured() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runner != nil
}

// OperatorID returns the configured operator id under the lock (for display on
// the dashboard Access page, TEN-208).
func (m *discordRelayManager) OperatorID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.operatorID
}

// SetApprovalTimeout changes how long an approval card waits before
// auto-denying, applies it to the live approver, and persists it. Bounded so a
// typo can't make dangerous actions hang for a day or auto-deny before a human
// could possibly tap.
func (m *discordRelayManager) SetApprovalTimeout(d time.Duration) error {
	if d < 30*time.Second || d > 24*time.Hour {
		return fmt.Errorf("approval timeout must be between 30s and 24h (got %s)", d)
	}
	m.mu.Lock()
	m.approvalTimeout = d
	if m.approver != nil {
		m.approver.timeout = d
	}
	persist := m.persistTimeout
	m.mu.Unlock()
	if persist != nil {
		return persist(d)
	}
	return nil
}

// ApprovalTimeout reports the effective card timeout (the default when unset).
func (m *discordRelayManager) ApprovalTimeout() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.approvalTimeout > 0 {
		return m.approvalTimeout
	}
	return approvalTimeout
}

// relayApprovalTimeout reads relay.approval_timeout (a Go duration such as
// "10m"). Unset or unparseable ⇒ 0 ⇒ the approver's default.
func relayApprovalTimeout(lc *launchConfig) time.Duration {
	if lc == nil {
		return 0
	}
	d, err := time.ParseDuration(strings.TrimSpace(lc.Relay.ApprovalTimeout))
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// relayCardStyle reads relay.card_style; only "legacy" is an override (anything
// else ⇒ "" ⇒ the v2 default with automatic legacy fallback).
func relayCardStyle(lc *launchConfig) string {
	if lc == nil {
		return ""
	}
	if s := strings.ToLower(strings.TrimSpace(lc.Relay.CardStyle)); s == cardStyleLegacy {
		return s
	}
	return ""
}

// SetOperator records the single operator's Discord user id (and persists it).
func (m *discordRelayManager) SetOperator(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("operator id required (your numeric Discord user id)")
	}
	m.mu.Lock()
	m.operatorID = id
	running := m.running
	allowExec := m.allowExec
	m.mu.Unlock()
	if m.persist != nil {
		return m.persist(running, id, allowExec)
	}
	return nil
}
