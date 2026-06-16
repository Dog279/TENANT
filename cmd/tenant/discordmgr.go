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

	"tenant/internal/agent"
	"tenant/internal/memory/archive"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/soul"
	"tenant/internal/memory/userprofile"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/plugins/discord"
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
	skills      agent.SkillRetriever
	compactor   agent.Compactor
	userProfile *userprofile.Profile
	// fullTools is the LIVE tool registry (the running mux), not a snapshot —
	// so MCP/Jira tools that come online after the relay starts are visible to
	// the Discord agent (TEN-229).
	fullTools agent.ToolRegistry
	fullDisp  agent.ToolDispatcher
	sysPrompt string
	log       *slog.Logger
	// gateConfirm is the per-category permission broker's Confirm (TEN-231): the
	// discord_* tools route through it (allow → run, deny → block, ask → button)
	// exactly like every other gated tool, instead of always prompting a button.
	// nil ⇒ fall back to the raw button approver (tests / unwired).
	gateConfirm offsiteConfirm
}

const discordSysSuffix = " You are reachable over Discord while the operator is away from the machine. " +
	"You have the same tools as the local TUI — use them freely. Any dangerous action requires the " +
	"operator's per-action approval (a button tap that can also time out and deny). Keep answers concise " +
	"for a phone screen."

// buildDiscordAgent constructs the dedicated Discord agent + Discord service +
// nonce approver + reply sender. Shares the long-term memory stores (continuity)
// but gets its OWN working set and the restricted, approver-wired tool surface.
func buildDiscordAgent(token string, d discordAgentDeps) (relayRunner, *discord.Service, *discordApprover, messageSender, *execGate, error) {
	svc, err := discord.Open(discord.Config{Token: token})
	if err != nil {
		return nil, nil, nil, nil, nil, err
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
	reg, restDisp, gate := restrictForDiscord(d.fullTools, d.fullDisp)
	disp := &discordRoutingDispatcher{discord: ddisc, rest: restDisp}

	ag, err := agent.New(agent.Config{
		AgentID:      "tenant-discord",
		Router:       d.router,
		SoulLive:     d.soulLive,
		Working:      working.New(),
		Archive:      d.archive,
		Episodic:     d.episodic,
		Semantic:     d.semantic,
		Tools:        reg,
		Dispatcher:   disp,
		Logger:       d.log,
		Skills:       d.skills,
		Compactor:    d.compactor,
		UserProfile:  d.userProfile,
		SystemPrompt: d.sysPrompt + discordSysSuffix,
	})
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	return ag, svc, approver, sender, gate, nil
}

// discordRelayManager owns the relay lifecycle. tui.RelayControl.
type discordRelayManager struct {
	base     context.Context
	runner   relayRunner
	svc      *discord.Service
	approver *discordApprover
	sender   messageSender
	gate     *execGate // offsite exec-mode switch (nil if discord unconfigured)
	// broker is the per-category permission broker (TEN-231) that fronts every
	// gated tool the Discord agent reaches. Its "ask" backend is askOperator (the
	// button approver). Stable across Reconfigure (only the approver swaps), so
	// /relay permissions modes survive a token hot-swap. nil ⇒ pre-TEN-231 path.
	broker   *approvalBroker
	token    string
	log      *slog.Logger
	notify   func(string)
	degraded func() bool // when true, the model is on the echo fallback; relay refuses turns
	persist  func(enabled bool, operatorID string, allowExec bool) error

	// buildFn rebuilds the Discord agent + gateway internals with a fresh token.
	// Set at the serve wiring point so Reconfigure can hot-swap after a live
	// /skill configure discord without restarting the process.
	buildFn func(token string) (relayRunner, *discord.Service, *discordApprover, messageSender, *execGate, error)

	// start is the test seam (real = realStart: wire+run gateway/relay).
	start func(ctx context.Context, operatorID string) error

	mu         sync.Mutex
	running    bool
	cancel     context.CancelFunc
	operatorID string
	allowExec  bool // last-set exec mode (mirrors gate; persisted)
}

// realStart wires a fresh relay (bound to operatorID) + gateway and runs the
// gateway under ctx. Returns once started (the gateway connects asynchronously).
func (m *discordRelayManager) realStart(ctx context.Context, operatorID string) error {
	rl := newRelay(m.runner, m.sender, operatorID, m.log)
	rl.approver = m.approver
	if m.broker != nil {
		rl.confirm = m.broker.Confirm // gated tools route through the per-category broker (TEN-231)
	}
	rl.degraded = m.degraded
	rl.Start(ctx)
	gw := &discord.Gateway{
		Token:         m.token,
		GetURL:        m.svc.GatewayURL,
		OnMessage:     rl.handleInbound,
		OnInteraction: rl.handleInteraction,
		OnReady:       rl.setBotID,
		Log:           m.log,
		OnFatal: func(e error) {
			if m.notify != nil {
				m.notify("discord relay stopped: " + e.Error())
			}
		},
	}
	go func() { _ = gw.Run(ctx) }()
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
	if m.persist != nil {
		_ = m.persist(true, m.operatorID, m.allowExec)
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
	}
	if m.persist != nil {
		return m.persist(false, m.operatorID, m.allowExec)
	}
	return nil
}

// Status reports whether the relay is running, whether an operator id is set,
// and whether offsite exec mode is on.
func (m *discordRelayManager) Status() (running bool, operatorSet bool, execOn bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running, strings.TrimSpace(m.operatorID) != "", m.allowExec
}

// SetExec persists the operator's offsite exec-mode preference and mirrors it
// onto the shared gate (which the cron runner still consults). NOTE: since
// TEN-229 the Discord agent sees the full live tool surface regardless of this
// flag — every dangerous call is gated per-action by the button approver — so
// for Discord this is a persisted preference, not a tool-visibility switch.
func (m *discordRelayManager) SetExec(on bool) error {
	if m.runner == nil {
		return fmt.Errorf("discord not configured — run `/skill configure discord <bot-token>` first")
	}
	m.mu.Lock()
	m.allowExec = on
	if m.gate != nil {
		m.gate.set(on)
	}
	running, opID := m.running, m.operatorID
	m.mu.Unlock()
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
	runner, svc, appr, snd, gate, err := m.buildFn(token)
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
	m.gate = gate
	m.token = token
	// Restore exec-mode preference on the new gate.
	if gate != nil && m.allowExec {
		gate.set(true)
	}
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
	if m.broker == nil {
		return nil
	}
	return m.broker
}

// askOperator is the broker's "ask"-mode backend: it raises a per-action button
// approval to the operator in Discord via the LIVE approver (re-read under the
// lock so a Reconfigure token swap is picked up). Returns false (deny) when the
// relay isn't built or the operator can't be reached — fail closed.
func (m *discordRelayManager) askOperator(ctx context.Context, action, detail string) bool {
	m.mu.Lock()
	appr := m.approver
	m.mu.Unlock()
	if appr == nil {
		return false
	}
	return appr.Confirm(ctx, action, detail)
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
