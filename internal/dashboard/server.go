// Package dashboard is Tenant's web control panel: an HTTP server that
// fans the agent's live turn events out to browser clients and exposes a
// REST surface for tools and agent status. This file is the Wave-1
// skeleton — it serves an embedded placeholder SPA and a health check.
// Wave 2 adds REST (rest.go), WebSocket (ws.go), and auth/TLS (auth.go)
// in their own files, mounting handlers through the seams documented in
// the CONTRACT block below.
package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"tenant/internal/agent"
)

// AgentRunner is the minimal slice of *agent.Agent the dashboard drives: a
// turn plus a mid-turn interjection. Per-turn cancellation is the caller's
// ctx — Wave 2's WS handler runs Turn under a per-connection cancelable
// context, exactly as the TUI does (there is no Cancel() method; a canceled
// ctx stops the turn). Kept as an interface so the dashboard doesn't take
// the whole concrete *agent.Agent and tests can supply a fake.
type AgentRunner interface {
	Turn(ctx context.Context, req agent.TurnRequest) (*agent.TurnResult, error)
	Interject(msg string)
}

// ToolControl is the runtime tool surface the dashboard's REST layer reads
// and toggles. It mirrors tui.ToolControl method-for-method; the wiring in
// cmd/tenant adapts the live tool mux to it (only the ToolList element type
// differs — see dashTools there). Defined here so the dashboard package
// stays decoupled from the terminal UI. Wave 2's mountREST uses it.
type ToolControl interface {
	ToolList() []ToolInfo
	SetEnabled(target string, on bool) (int, string, error)
	SetPluginEnabled(label string, on bool) (int, string, error)
	Plugins() []string
}

// ToolInfo is one tool's runtime state, structurally identical to
// tui.ToolInfo (Name/Plugin/Enabled/Gated). Gated is the authoritative
// send/destructive flag, surfaced by REST as `destructive`.
type ToolInfo struct {
	Name    string
	Plugin  string
	Enabled bool
	Gated   bool
}

// Config is the dashboard's static configuration, loaded from the
// `dashboard` block of launchConfig. TLSCert/TLSKey/Auth are wired in
// Wave 2 (TEN-79); they're carried here now so the config shape is stable.
type Config struct {
	Addr    string // listen address, e.g. 127.0.0.1:8770
	TLSCert string // PEM cert path; empty = plaintext (Wave 2)
	TLSKey  string // PEM key path (Wave 2)
	Auth    string // auth mode/token seam (Wave 2)
}

// Server is the dashboard HTTP server. Its exported/internal fields are the
// extension surface for Wave 2 (see CONTRACT below): broker for the WS
// event stream, agent for Turn/Interject, tools for REST, mux for route
// mounting, cfg for TLS/auth.
type Server struct {
	cfg          Config
	agent        AgentRunner
	tools        ToolControl
	mem          MemoryControl       // TEN-88: memory curator surface; nil = routes not mounted (TEN-89)
	cron         CronControl         // recurring-job admin; nil = section renders "not configured"
	secrets      SecretsControl      // write-only API-key admin; nil = section renders "not configured"
	eval         EvalControl         // TEN-201: eval/quality page; nil = "not configured"
	skills       SkillControl        // TEN-202: skill library page; nil = "not configured"
	models       ModelControl        // TEN-204: model backends page; nil = "not configured"
	mcp          MCPControl          // TEN-205: remote MCP connectors page; nil = "not configured"
	integrations IntegrationsControl // TEN-206: integration-config page; nil = "not configured"
	access       AccessControl       // TEN-208: iMessage + Discord access admin; nil = "not configured"
	broker       *agent.Broker
	mux          *http.ServeMux
	log          *slog.Logger
	// coord serializes Turn execution across ALL /ws connections (TEN-80):
	// the agent is a single shared instance, so only one turn may run at a
	// time server-wide. See session.go.
	coord *wsCoordinator
	// tmpl holds the server-rendered page templates (TEN-107).
	tmpl *ssrTemplates
}

// New constructs the dashboard server and builds its routes. A nil logger
// falls back to slog.Default(); agent/tools/mem may be nil for a bare
// health-only server (the Wave-2 handlers that need them aren't mounted
// yet). mem is the TEN-88 memory-curator surface; TEN-89 mounts its routes.
func New(cfg Config, runner AgentRunner, tools ToolControl, mem MemoryControl, broker *agent.Broker, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if broker == nil {
		broker = agent.NewBroker(0)
	}
	s := &Server{
		cfg:    cfg,
		agent:  runner,
		tools:  tools,
		mem:    mem,
		broker: broker,
		mux:    http.NewServeMux(),
		log:    log,
	}
	s.coord = newWSCoordinator(runner)
	s.tmpl = parseSSR()
	s.routes()
	return s
}

// Handler exposes the server's mux for tests (httptest) and for any caller
// that wants to wrap it (e.g. the Wave-2 secure() middleware in the start
// path). Equivalent to the routed *http.ServeMux.
func (s *Server) Handler() http.Handler { return s.mux }

// routes mounts the Wave-1 surface: health + the embedded SPA. Everything
// else is Wave 2.
//
// === CONTRACT (Wave 2 wiring) ====================================
// Wave 2 implements these in their OWN files in package dashboard; they
// compile against this Server and get mounted HERE during integration:
//
//	func (s *Server) mountREST(mux *http.ServeMux)  // TEN-77, rest.go
//	    reads s.tools (ToolList/SetEnabled/SetPluginEnabled/Plugins) and
//	    s.agent for status; registers GET/POST /api/... on mux.
//
//	func (s *Server) mountWS(mux *http.ServeMux)    // TEN-78, ws.go
//	    registers GET /ws; per connection calls ch, cancel := s.broker.
//	    Subscribe(), streams Events to the socket, and drives turns via
//	    s.agent.Turn / s.agent.Interject (cancel the turn with a per-conn
//	    context). cancel() on disconnect.
//
// Auth/TLS (TEN-79, auth.go):
//
//	func (s *Server) secure(h http.Handler) http.Handler
//	    wraps the routed mux with auth (s.cfg.Auth) — applied in Run()
//	    around s.mux before it's handed to http.Server.Handler.
//	func (s *Server) checkBindPolicy() error
//	    fail-closed guard: non-loopback s.cfg.Addr without TLS+Auth must
//	    error. Called at the TOP of Run(), before Listen.
//
// Integration (in this file): uncomment in routes() —
//
//	s.mountREST(s.mux)
//	s.mountWS(s.mux)
//
// and in Run(): handler = s.secure(s.mux); after checkBindPolicy().
// =================================================================
func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Wave 2 surfaces: REST control (rest.go) + chat/event WebSocket (ws.go).
	// Mounted before the catch-all static handler below; Go 1.22 mux
	// precedence (most-specific pattern wins) routes /api/... and /ws here.
	s.mountREST(s.mux)
	s.mountWS(s.mux)

	// Memory Curator REST surface (TEN-89, memory_rest.go). Nil-safe: a server
	// constructed without a MemoryControl serves without the memory routes.
	if s.mem != nil {
		s.mountMemoryREST(s.mux)
	}

	// Auth entry points (TEN-106): PSK→cookie so browser navigation works without
	// a Bearer header. checkAuth exempts these three paths.
	s.mux.HandleFunc("GET /settings", s.handleSettingsPage)
	s.mux.HandleFunc("POST /auth/login", s.handleLogin)
	s.mux.HandleFunc("GET /auth/logout", s.handleLogout)

	// Server-rendered pages (TEN-107), now mounted at the root. The TEN-110
	// cutover removed the old embedded JS SPA (assets/) and its GET / file
	// server: the SSR dashboard owns GET / via mountSSR's GET /{$}.
	s.mountSSR(s.mux)

	// Cron admin (recurring jobs). Mounted unconditionally + nil-guarded, like
	// the memory pages — SetCron wires the control after construction.
	s.mountCronSSR(s.mux)

	// Write-only API-key settings (TEN-145). Same nil-safe pattern; SetSecrets
	// wires the control after construction.
	s.mountSecretsSSR(s.mux)

	// Eval & Quality (TEN-201) + Skills (TEN-202) + Models (TEN-204). Same
	// nil-safe pattern; SetEval / SetSkills / SetModels wire after construction.
	s.mountEvalSSR(s.mux)
	s.mountSkillsSSR(s.mux)
	s.mountModelsSSR(s.mux)
	s.mountMCPSSR(s.mux)
	s.mountIntegrationsSSR(s.mux)
	s.mountAccessSSR(s.mux)
}

// handleHealthz reports liveness as 200 JSON {"status":"ok"}.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// Run serves until ctx is canceled, then shuts down gracefully. Mirrors the
// gateway's HTTP start pattern in cmd/tenant (goroutine ListenAndServe +
// ctx-driven Shutdown). Returns nil on a clean ctx-cancel shutdown.
func (s *Server) Run(ctx context.Context) error {
	// Fail-closed: refuse a non-loopback bind without TLS+auth (TEN-79).
	if err := s.checkBindPolicy(); err != nil {
		return err
	}
	tlsCfg, err := s.tlsConfig()
	if err != nil {
		return err
	}
	// secure() wraps the routed mux with bearer auth + same-origin CORS.
	srv := &http.Server{Addr: s.cfg.Addr, Handler: s.secure(s.mux), TLSConfig: tlsCfg}

	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		// Bounded graceful drain so a hung WS handler can't wedge exit.
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		close(shutdownDone)
	}()

	s.log.Info("dashboard serving", "addr", s.cfg.Addr, "tls", tlsCfg != nil)
	if tlsCfg != nil {
		err = srv.ListenAndServeTLS("", "") // cert+key already in TLSConfig
	} else {
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone // let Shutdown finish before returning
		return nil
	}
	return err
}
