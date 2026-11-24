package main

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/dashboard"
	"tenant/internal/lifecycle"
	"tenant/internal/logs"
	"tenant/internal/logs/logapi"
	"tenant/internal/memory/working"
	"tenant/internal/runtrace"
)

// dashboardManager owns the lifecycle of the web control panel so the
// operator can start/stop it at runtime via the TUI's /dashboard command
// (TEN-86). It implements tui.DashboardControl.
//
// Each Enable builds a FRESH dashboard.Server: an http.Server can't be
// reused after Shutdown, so a stop/start cycle constructs a new one. The
// server runs under a child of the base context; Disable cancels that child
// and waits for its graceful shutdown to finish. The listener is reserved
// synchronously, so bind errors are returned by Enable.
type dashboardManager struct {
	base         context.Context
	cfg          dashboard.Config
	runner       dashboard.AgentRunner
	transcript   func() []working.Message // the conversation so far, for the Chat page (nil-safe)
	tools        dashboard.ToolControl
	mem          dashboard.MemoryControl       // TEN-88: memory curator surface (nil-safe)
	cron         dashboard.CronControl         // recurring-job admin surface (nil-safe)
	githubRepos  dashboard.GitHubReposControl  // watched GitHub repos + per-repo write (nil-safe)
	secrets      dashboard.SecretsControl      // write-only API-key admin surface (nil-safe)
	eval         dashboard.EvalControl         // TEN-201: eval/quality surface (nil-safe)
	skills       dashboard.SkillControl        // TEN-202: skill library surface (nil-safe)
	models       dashboard.ModelControl        // TEN-204: model backends surface (nil-safe)
	mcp          dashboard.MCPControl          // TEN-205: remote MCP connectors surface (nil-safe)
	integrations dashboard.IntegrationsControl // TEN-206: integration-config surface (nil-safe)
	access       dashboard.AccessControl       // TEN-208: iMessage + Discord access surface (nil-safe)
	approvals    dashboard.ApprovalControl     // TEN-194: headless approval queue surface (serve mode; nil-safe)
	rules        dashboard.RuleControl         // per-tool permission rules card on the Tools page (nil-safe)
	safety       dashboard.SafetyControl       // TEN-207: the main broker's category modes (nil-safe)
	safetyFacts  func() dashboard.SafetyFacts  // who commands run as, and whether sandboxed (nil-safe)
	agents       dashboard.AgentControl        // TEN-209: named helpers (nil-safe)
	goal         dashboard.GoalControl         // TEN-212: the goal loop; nil under serve
	research     dashboard.ResearchControl     // TEN-210: deep research (nil-safe)
	fleet        dashboard.FleetControl        // TEN-533: the fleet's machines (nil-safe)
	broker       *agent.Broker
	logs         *logs.Hub          // the unified log book: its folders and ring (nil-safe)
	runs         *runtrace.Store    // run records, for the log book's timelines (nil-safe)
	security     logapi.SecurityLog // the Security log's verify and audit export
	alerts       logapi.AlertConfig // the alert rules in config.json
	log          *slog.Logger
	notify       func(string)             // feed sink (pushSys) for async status
	persist      func(enabled bool) error // record the on/off choice to config

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
	addr    string
	srv     *dashboard.Server // the running server; nil when stopped
	// tailnetHost is the tailnet DNS name `tailscale serve` publishes the
	// dashboard under (TEN-332). Kept here so a stop/start still trusts it.
	tailnetHost string
	// lan is the dashboard's second listener, for the local network
	// (dashboardlan.go); nil where it is not offered.
	lan *dashLAN
}

// dashBootWait is how long a serve that has just started waits for the
// dashboard's address while something still holds it.
var dashBootWait = 10 * time.Second

const listenRetryEvery = 200 * time.Millisecond

// listenWait is net.Listen that waits out an address in use, for up to
// wait. Any other error, and an address still taken after wait, is returned
// as it is.
func listenWait(ctx context.Context, addr string, wait time.Duration) (net.Listener, error) {
	deadline := time.Now().Add(wait)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil || !addrInUse(err) || !time.Now().Before(deadline) {
			return ln, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(listenRetryEvery):
		}
	}
}

// Enable starts the dashboard if it isn't already running and persists the
// "on" choice. Returns the bind address. A bind/startup failure is reported
// to the feed from the serving goroutine (Enable itself doesn't block on it).
func (m *dashboardManager) Enable() (string, error) { return m.enable(0) }

// EnableAtBoot is Enable for a serve that has just been started. A restart
// starts the new process while the one it replaces may still hold the
// address: `tenant service restart` on a Windows logon task ends the old
// process and starts the new one a fraction of a second later, and the new
// one then ran without a dashboard, so `tenant update` could not see it come
// up. An address in use is therefore waited out for dashBootWait before it
// counts as a failure; nothing else is retried.
func (m *dashboardManager) EnableAtBoot() (string, error) { return m.enable(dashBootWait) }

func (m *dashboardManager) enable(wait time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return m.addr, nil
	}
	ln, err := listenWait(m.base, m.cfg.Addr, wait)
	if err != nil {
		m.track(lifecycle.Failed, err)
		return m.cfg.Addr, err
	}
	srv := dashboard.New(m.cfg, m.runner, m.tools, m.mem, m.broker, m.log)
	if m.transcript != nil {
		srv.SetTranscript(m.transcript)
	}
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
	if m.githubRepos != nil {
		srv.SetGitHubRepos(m.githubRepos)
	}
	if m.access != nil {
		srv.SetAccess(m.access)
	}
	if m.approvals != nil {
		srv.SetApprovals(m.approvals)
	}
	if m.rules != nil {
		srv.SetRules(m.rules)
	}
	if m.safety != nil {
		srv.SetSafety(m.safety, m.safetyFacts)
	}
	if m.agents != nil {
		srv.SetAgents(m.agents)
	}
	if m.goal != nil {
		srv.SetGoal(m.goal)
	}
	if m.research != nil {
		srv.SetResearch(m.research)
	}
	if m.fleet != nil {
		srv.SetFleet(m.fleet)
	}
	if m.logs != nil {
		srv.SetLogs(m.logs)
	}
	if m.runs != nil {
		srv.SetRunRecords(m.runs.Get)
	}
	srv.SetSecurityLog(m.security)
	srv.SetAlertRules(m.alerts)
	srv.SetTrustedHosts(m.trustedHostsLocked())
	dctx, cancel := context.WithCancel(m.base)
	done := make(chan struct{})
	go func() {
		err := srv.Serve(dctx, ln)
		close(done)
		if err != nil {
			m.mu.Lock()
			current := m.done == done
			if current {
				m.stopLANLocked() // it served this server, which is gone
				m.running = false
				m.cancel = nil
				m.done = nil
				m.srv = nil
			}
			m.mu.Unlock()
			if current {
				m.track(lifecycle.Failed, err)
			}
			if current && m.notify != nil {
				m.notify("dashboard: stopped: " + err.Error())
			}
		}
	}()
	m.running = true
	m.cancel = cancel
	m.done = done
	m.srv = srv
	m.addr = ln.Addr().String()
	m.track(lifecycle.Running, nil)
	// The local network is served while the dashboard is, when the operator
	// asked for it (config.json, --dashboard-lan, /dashboard serve lan).
	if m.lan != nil && m.lan.want {
		said := ""
		if err := m.startLANLocked(); err != nil {
			said = "dashboard: not served to the local network: " + err.Error()
		} else {
			lanRecord("on", m.lan.bound, surfaceCLI)
			said = m.lanSaidLocked()
		}
		if m.notify != nil {
			go m.notify(said)
		}
	}
	if m.persist != nil {
		if err := m.persist(true); err != nil {
			return m.addr, err
		}
	}
	return m.addr, nil
}

// Disable stops a running dashboard (graceful shutdown via ctx cancel) and
// persists the "off" choice — this is the operator deciding to turn it off
// (/dashboard off). A no-op when already stopped.
func (m *dashboardManager) Disable() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
	if m.persist != nil {
		return m.persist(false)
	}
	return nil
}

// Shutdown stops the dashboard for PROCESS teardown (serve SIGTERM/Ctrl-C, exit)
// WITHOUT persisting an "off" choice. A restart is not the operator disabling
// the dashboard, so the saved enabled=true must survive — otherwise every
// `tenant serve` shutdown silently wrote dashboard.enabled=false and the panel
// came back off (and corrupted the choice the TUI reads). A no-op when stopped.
func (m *dashboardManager) Shutdown() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
	return nil
}

// stopLocked halts a running server. Caller must hold m.mu.
func (m *dashboardManager) stopLocked() {
	if m.running {
		m.stopLANLocked() // the second listener never outlives the dashboard
		m.cancel()
		<-m.done
		m.running = false
		m.cancel = nil
		m.done = nil
		m.srv = nil
		m.track(lifecycle.Stopped, nil)
	}
}

// track files the dashboard's state in the Components view (lifecycle).
func (m *dashboardManager) track(st lifecycle.State, err error) {
	lifecycle.Track(m.base, "dashboard", lifecycle.Server, "dashboard", st, err)
}

// SetTailnetHost trusts the tailnet DNS name `tailscale serve` publishes the
// dashboard under, or stops trusting it when host is "" (TEN-332). Tailscale's
// proxy keeps the browser's Host, which the listener-derived allowlist would
// reject. Applies to the running server and to any server started later.
func (m *dashboardManager) SetTailnetHost(host string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tailnetHost = host
	if m.srv != nil {
		m.srv.SetTrustedHosts(m.trustedHostsLocked())
	}
}

// trustedHostsLocked lists the runtime-trusted Host authorities. Caller must
// hold m.mu.
func (m *dashboardManager) trustedHostsLocked() []string {
	if m.tailnetHost == "" {
		return nil
	}
	return []string{m.tailnetHost}
}

// Status reports whether the dashboard is running and its bind address.
func (m *dashboardManager) Status() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	addr := m.addr
	if addr == "" {
		addr = m.cfg.Addr
	}
	return m.running, addr
}

// AuthToken lets launch/status UX show the credential next to the local URL.
// It is intentionally optional so alternate DashboardControl implementations
// and test fakes remain source-compatible.
func (m *dashboardManager) AuthToken() string { return m.cfg.Auth }
