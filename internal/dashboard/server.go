// Package dashboard is Tenant's web control panel: an HTTP server that
// fans the agent's live turn events out to browser clients and exposes a
// REST surface for tools and agent status. This file is the Wave-1
// skeleton — it serves an embedded placeholder SPA and a health check.
// Wave 2 adds REST (rest.go), WebSocket (ws.go), and auth/TLS (auth.go)
// in their own files, mounting handlers through the seams documented in
// the CONTRACT block below.
package dashboard

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/httplimits"
	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/logapi"
)

const maxDashboardRequestBodyBytes = 1 << 20

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

// Config is the dashboard's static security and listener configuration.
type Config struct {
	Addr         string   // listen address, e.g. 127.0.0.1:8770
	TLSCert      string   // PEM cert path; empty = plaintext (Wave 2)
	TLSKey       string   // PEM key path (Wave 2)
	Auth         string   // bearer/cookie authentication secret; required by Run
	AllowedHosts []string // additional trusted HTTP Host authorities (host or host:port)
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
	githubRepos  GitHubReposControl  // watched GitHub repos + per-repo write; nil = section hidden
	secrets      SecretsControl      // write-only API-key admin; nil = section renders "not configured"
	eval         EvalControl         // TEN-201: eval/quality page; nil = "not configured"
	skills       SkillControl        // TEN-202: skill library page; nil = "not configured"
	models       ModelControl        // TEN-204: model backends page; nil = "not configured"
	mcp          MCPControl          // TEN-205: remote MCP connectors page; nil = "not configured"
	integrations IntegrationsControl // TEN-206: integration-config page; nil = "not configured"
	access       AccessControl       // TEN-208: iMessage + Discord access admin; nil = "not configured"
	approvals    ApprovalControl     // TEN-194: headless approval surface (serve mode); nil = TUI drains its own prompts
	logs         *logs.Hub           // unified log book: retained feed (backfill + replay) + on-disk folders; nil = Logs unavailable
	logbook      *logapi.API         // the log book's API v2 over logs (logbook.go)
	rules        RuleControl         // per-tool permission rules (Tools page card); nil = card hidden
	safety       SafetyControl       // TEN-207: the main broker's category modes; nil = "isn't available"
	safetyFacts  func() SafetyFacts  // who commands run as, and whether sandboxed (nil-safe)
	agents       AgentControl        // TEN-209: named helpers; nil = "isn't available"
	goal         GoalControl         // TEN-212: the goal loop; nil under serve
	research     ResearchControl     // TEN-210: deep research; nil = "isn't available"
	broker       *agent.Broker
	mux          *http.ServeMux
	log          *slog.Logger
	// coord serializes Turn execution across ALL /ws connections (TEN-80):
	// the agent is a single shared instance, so only one turn may run at a
	// time server-wide. See session.go.
	coord *wsCoordinator
	// tmpl holds the server-rendered page templates (TEN-107).
	tmpl *ssrTemplates

	// trusted holds Host authorities trusted at runtime on top of cfg, such as
	// the tailnet name `tailscale serve` publishes the dashboard under
	// (TEN-332). Set through SetTrustedHosts.
	trustedMu sync.RWMutex
	trusted   []string

	// sessions are the signed-in browsers: the sign-in cookie holds one of
	// these random ids, never the key (auth_pages.go).
	sessMu   sync.Mutex
	sessions map[string]time.Time // id → expiry
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
	s.coord = newWSCoordinator(runner, broker)
	s.tmpl = parseSSR()
	s.routes()
	return s
}

// Handler exposes the routed mux with request-body and finite-response limits
// applied. It is used by tests and wrapped by secure() in the production start
// path.
func (s *Server) Handler() http.Handler {
	return s.handlerWithWriteTimeout(httplimits.ResponseWriteTimeout)
}

func (s *Server) handlerWithWriteTimeout(writeTimeout time.Duration) http.Handler {
	return httplimits.Protect(s.mux, maxDashboardRequestBodyBytes, writeTimeout, isDashboardStream)
}

func isDashboardStream(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch r.URL.Path {
	case "/events", "/logs/tail", "/ws", "/research/live":
		return true
	default:
		return logapi.Streaming(r)
	}
}

func newDashboardHTTPServer(addr string, handler http.Handler, tlsCfg *tls.Config) *http.Server {
	srv := &http.Server{Addr: addr, Handler: handler, TLSConfig: tlsCfg}
	httplimits.Configure(srv)
	return srv
}

// serverErrors files the HTTP server's own error log (TLS handshake failures,
// handler panics) as ServerError, under the server's Serving activity. A
// panic's stack, after its first line, goes in its own field.
type serverErrors struct{ ctx context.Context }

func (w serverErrors) Write(p []byte) (int, error) {
	msg, stack, _ := strings.Cut(strings.TrimSpace(string(p)), "\n")
	ServerError.Log(w.ctx, "err", msg, "stack", stack)
	return len(p), nil
}

// observe records each request as a Request activity in the diagnostic
// channel: its method, route pattern and status. Never the path, the query
// or the body, which can carry IDs and tokens. The status strip's poll
// (every page, every 20s) is left out: it would bury the requests that matter.
func (s *Server) observe(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ui/status" {
			h.ServeHTTP(w, r)
			return
		}
		ctx, act := event.Start(r.Context(), Request, "method", r.Method, "route", s.route(r))
		sw := &statusWriter{ResponseWriter: w}
		returned := false
		defer func() {
			var err error
			switch {
			case !returned: // net/http recovers the panic and logs it (ServerError)
				sw.status, err = http.StatusInternalServerError, errors.New("the handler panicked")
			case sw.status == 0:
				sw.status = http.StatusOK
			case sw.status >= 500:
				err = errors.New(http.StatusText(sw.status))
			}
			act.End(&err, "status", sw.status)
		}()
		h.ServeHTTP(sw, r.WithContext(ctx))
		returned = true
	})
}

// route is r's route pattern as the mux matches it, without the method.
func (s *Server) route(r *http.Request) string {
	_, p := s.mux.Handler(r)
	if _, path, ok := strings.Cut(p, " "); ok {
		p = strings.TrimSpace(path)
	}
	if p == "" {
		return "(no route)"
	}
	return p
}

// statusWriter keeps a response's status. Flush and Hijack pass through, for
// the live streams and the WebSocket upgrade.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.status = http.StatusSwitchingProtocols
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

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
	s.mountLogbook(s.mux) // the log book's API v2 (logbook.go)
	s.mountLogsSSR(s.mux) // its Logs page (ssr_logs.go)

	// Memory Curator REST surface (TEN-89, memory_rest.go). Nil-safe: a server
	// constructed without a MemoryControl serves without the memory routes.
	if s.mem != nil {
		s.mountMemoryREST(s.mux)
		s.mountFactInspectorREST(s.mux)
	}

	// Auth entry points (TEN-106): PSK→cookie so browser navigation works without
	// a Bearer header. checkAuth exempts these three paths.
	s.mux.HandleFunc("GET /settings", s.handleSettingsPage)
	s.mux.HandleFunc("POST /auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /auth/logout", s.handleLogout)

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

	// Dashboard v2 pages (TEN-207/209/210/212). Same nil-safe pattern; SetSafety
	// / SetAgents / SetGoal / SetResearch wire after construction.
	s.mountSafetySSR(s.mux)
	s.mountAgentsSSR(s.mux)
	s.mountGoalSSR(s.mux)
	s.mountResearchSSR(s.mux)
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
	if err := s.checkBindPolicy(); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve serves on an already-bound listener until ctx is canceled. Accepting
// the listener separately lets lifecycle owners reserve an ephemeral port
// synchronously and makes shutdown completion observable without bind races.
func (s *Server) Serve(ctx context.Context, ln net.Listener) (err error) {
	defer ln.Close()
	ctx, act := event.Start(ctx, Serving, "addr", ln.Addr().String(), "tls", s.cfg.TLSCert != "" && s.cfg.TLSKey != "")
	defer act.End(&err)
	// Fail-closed: refuse a non-loopback bind without TLS+auth (TEN-79).
	if err := s.checkBindPolicy(); err != nil {
		return err
	}
	tlsCfg, err := s.tlsConfig()
	if err != nil {
		return err
	}
	// served(): secure() wraps the routed mux with bearer auth + same-origin
	// CORS; observe() records each request, rejected ones too.
	srv := newDashboardHTTPServer(s.cfg.Addr, s.served(), tlsCfg)
	srv.ErrorLog = log.New(serverErrors{ctx}, "", 0)

	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		// Bounded graceful drain so a hung WS handler can't wedge exit.
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		close(shutdownDone)
	}()

	if tlsCfg != nil {
		err = srv.ServeTLS(ln, "", "") // cert+key already in TLSConfig
	} else {
		err = srv.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone // let Shutdown finish before returning
		return nil
	}
	return err
}
