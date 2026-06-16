package main

import (
	"context"
	"log/slog"
	"sync"

	"tenant/internal/agent"
	"tenant/internal/dashboard"
)

// dashboardManager owns the lifecycle of the web control panel so the
// operator can start/stop it at runtime via the TUI's /dashboard command
// (TEN-86). It implements tui.DashboardControl.
//
// Each Enable builds a FRESH dashboard.Server: an http.Server can't be
// reused after Shutdown, so a stop/start cycle constructs a new one. The
// server runs under a child of the base context; Disable cancels that child
// to trigger the server's graceful shutdown. A startup/bind error can't be
// returned synchronously (ListenAndServe blocks), so it surfaces to the feed
// via notify and flips running back to false.
type dashboardManager struct {
	base         context.Context
	cfg          dashboard.Config
	runner       dashboard.AgentRunner
	tools        dashboard.ToolControl
	mem          dashboard.MemoryControl       // TEN-88: memory curator surface (nil-safe)
	cron         dashboard.CronControl         // recurring-job admin surface (nil-safe)
	secrets      dashboard.SecretsControl      // write-only API-key admin surface (nil-safe)
	eval         dashboard.EvalControl         // TEN-201: eval/quality surface (nil-safe)
	skills       dashboard.SkillControl        // TEN-202: skill library surface (nil-safe)
	models       dashboard.ModelControl        // TEN-204: model backends surface (nil-safe)
	mcp          dashboard.MCPControl          // TEN-205: remote MCP connectors surface (nil-safe)
	integrations dashboard.IntegrationsControl // TEN-206: integration-config surface (nil-safe)
	access       dashboard.AccessControl       // TEN-208: iMessage + Discord access surface (nil-safe)
	broker       *agent.Broker
	evlog        *agent.EventLog // TEN-238: retained activity-feed event log (nil-safe)
	log          *slog.Logger
	notify       func(string)             // feed sink (pushSys) for async status
	persist      func(enabled bool) error // record the on/off choice to config

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
}

// Enable starts the dashboard if it isn't already running and persists the
// "on" choice. Returns the bind address. A bind/startup failure is reported
// to the feed from the serving goroutine (Enable itself doesn't block on it).
func (m *dashboardManager) Enable() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return m.cfg.Addr, nil
	}
	srv := dashboard.New(m.cfg, m.runner, m.tools, m.mem, m.broker, m.log)
	if m.cron != nil {
		srv.SetCron(m.cron)
	}
	if m.secrets != nil {
		srv.SetSecrets(m.secrets)
	}
	if m.eval != nil {
		srv.SetEval(m.eval)
	}
	if m.skills != nil {
		srv.SetSkills(m.skills)
	}
	if m.models != nil {
		srv.SetModels(m.models)
	}
	if m.mcp != nil {
		srv.SetMCP(m.mcp)
	}
	if m.integrations != nil {
		srv.SetIntegrations(m.integrations)
	}
	if m.access != nil {
		srv.SetAccess(m.access)
	}
	if m.evlog != nil {
		srv.SetEventLog(m.evlog)
	}
	dctx, cancel := context.WithCancel(m.base)
	go func() {
		if err := srv.Run(dctx); err != nil {
			m.mu.Lock()
			m.running = false
			m.mu.Unlock()
			if m.notify != nil {
				m.notify("dashboard: stopped: " + err.Error())
			}
		}
	}()
	m.running = true
	m.cancel = cancel
	if m.persist != nil {
		if err := m.persist(true); err != nil {
			return m.cfg.Addr, err
		}
	}
	return m.cfg.Addr, nil
}

// Disable stops a running dashboard (graceful shutdown via ctx cancel) and
// persists the "off" choice. A no-op when already stopped.
func (m *dashboardManager) Disable() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		m.cancel()
		m.running = false
	}
	if m.persist != nil {
		return m.persist(false)
	}
	return nil
}

// Status reports whether the dashboard is running and its bind address.
func (m *dashboardManager) Status() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running, m.cfg.Addr
}
