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
	fullTools   []model.ToolSpec
	fullDisp    agent.ToolDispatcher
	sysPrompt   string
	log         *slog.Logger
}

const discordSysSuffix = " You are reachable over Discord while the operator is away from the machine. " +
	"This is an OFFSITE session: the tools you are OFFERED each turn are the only actions available — if " +
	"something isn't offered (e.g. exec / write / destructive when exec mode is off, or team / orchestra " +
	"ever), say so plainly rather than pretending to do it. Any dangerous action requires the operator's " +
	"per-action approval on their phone (a button tap that can also time out and deny), so prefer the " +
	"smallest action that answers the ask. Keep answers concise for a phone screen."

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

	// discord_* tools route to the approver (not the shared TUI broker). The
	// rest go through the restricted surface — which, in exec mode, lets gated
	// dangerous tools through to d.fullDisp, where originConfirm (wired at the
	// mux) routes their approval to THIS approver via the offsite-stamped ctx.
	ddisc := discord.NewDispatcher(svc, discord.Policy{Confirm: approver.Confirm})
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
	token    string
	log      *slog.Logger
	notify   func(string)
	persist  func(enabled bool, operatorID string, allowExec bool) error

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
	rl.Start(ctx)
	gw := &discord.Gateway{
		Token:         m.token,
		GetURL:        m.svc.GatewayURL,
		OnMessage:     rl.handleInbound,
		OnInteraction: rl.handleInteraction,
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

// SetExec flips offsite exec mode live (the dedicated agent's dynamic registry +
// dispatcher honor the shared gate immediately) and persists the choice. Exec
// mode unlocks the gated dangerous tools offsite — each still requires a
// per-action button approval — but never team/orchestra.
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
