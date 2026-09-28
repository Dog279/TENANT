package dashboard

// mcp.go is the server-rendered MCP connectors page (TEN-205): the web view of
// remote MCP servers the agent can reach. Hybrid model (operator decision):
// CONNECT happens on the machine running Tenant (the OAuth sign-in browser
// pops there) — so connect is kicked off ASYNC and the page tells you to
// approve at the host; LIST and REMOVE work from anywhere (synchronous, safe).
//
// Connect must not block the HTTP handler: AddRemoteMCP opens a browser and
// waits minutes for the OAuth dance. The handler launches it in a goroutine
// and redirects immediately; the new server appears in the list on refresh.

import (
	"context"
	"net/http"
	"strings"
)

// MCPControl is the runtime remote-MCP surface the dashboard drives. cmd/tenant
// adapts mcpTUIControl to it (see dashMCP). Connect MAY block (browser OAuth on
// the host) — callers run it off the request goroutine. A nil control renders
// an "isn't configured" state.
type MCPControl interface {
	Servers() []MCPServerView
	Connect(url string) error // may block on host-side browser OAuth
	Remove(target string) (bool, error)
	// SetTrust flips a connector's live trust mode: ask | allow | deny.
	SetTrust(target, mode string) error
}

// MCPServerView is one connected server's render-ready state.
type MCPServerView struct {
	Label     string
	URL       string
	Enabled   bool
	ToolCount int
	Trust     string // ask | allow | deny ("" renders as ask)
	// NeedsReauth: the connector's sign-in died; its tools fail until the
	// operator reconnects it (the Reconnect form re-runs the host-side OAuth).
	NeedsReauth bool
	AuthSince   string // rendered, e.g. "2026-09-18 14:02 UTC"; "" when unknown
	AuthErr     string
}

// SetMCP installs the MCP control after construction (mirrors SetCron).
func (s *Server) SetMCP(c MCPControl) { s.mcp = c }

func (s *Server) mountMCPSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /mcp", s.handleMCPPage)
	mux.HandleFunc("POST /mcp/connect", s.handleMCPConnectForm)
	// Disconnecting loses its sign-in, and allow lets its tools run without
	// asking: both confirm (TEN-211). Ask and deny don't.
	mux.HandleFunc("POST /mcp/remove", needConfirm(nil, s.handleMCPRemoveForm))
	mux.HandleFunc("POST /mcp/trust", needConfirm(confirmUnless("mode", "ask", "deny"), s.handleMCPTrustForm))
}

type mcpPageData struct {
	layoutData
	Configured bool
	Servers    []MCPServerView
	Err        string
	Msg        string
}

func (s *Server) handleMCPPage(w http.ResponseWriter, r *http.Request) {
	d := mcpPageData{layoutData: layoutData{Title: "Remote services", Page: "mcp"}}
	d.Err = r.URL.Query().Get("err")
	d.Msg = r.URL.Query().Get("msg")
	if s.mcp == nil {
		s.render(w, s.tmpl.mcp, d)
		return
	}
	d.Configured = true
	d.Servers = s.mcp.Servers()
	s.render(w, s.tmpl.mcp, d)
}

// handleMCPConnectForm kicks off the connect ASYNC — AddRemoteMCP opens a
// browser on the host and blocks for the OAuth flow, which must not wedge the
// HTTP request. The operator does this step at the host machine (hybrid model);
// the connected server shows up in the list on refresh. The connect outlives
// the request, so its log record keeps the request's values without its
// cancellation (F7).
func (s *Server) handleMCPConnectForm(w http.ResponseWriter, r *http.Request) {
	if s.mcp == nil {
		http.Redirect(w, r, "/mcp", http.StatusSeeOther)
		return
	}
	rawURL := strings.TrimSpace(r.FormValue("url"))
	if rawURL == "" {
		redirectFlash(w, r, "/mcp", "", "Enter the service's address.")
		return
	}
	ctx := context.WithoutCancel(r.Context())
	go func(u string) {
		if err := s.mcp.Connect(u); err != nil {
			ConnectorNotAdded.Log(ctx, "url", u, "err", err)
		}
	}(rawURL)
	redirectFlash(w, r, "/mcp", "Connecting. A local service with no sign-in appears here in a moment (refresh); one that needs a sign-in opens a window on the computer running Tenant: approve it there first.", "")
}

func (s *Server) handleMCPRemoveForm(w http.ResponseWriter, r *http.Request) {
	if s.mcp == nil {
		http.Redirect(w, r, "/mcp", http.StatusSeeOther)
		return
	}
	target := strings.TrimSpace(r.FormValue("target"))
	if _, err := s.mcp.Remove(target); err != nil {
		redirectFlash(w, r, "/mcp", "", s.flashFor(r.Context(), "mcp remove", err, "Couldn't disconnect that service."))
		return
	}
	redirectFlash(w, r, "/mcp", "Service disconnected.", "")
}

// trustWords says what each trust mode means for a service's tools.
var trustWords = map[string]string{
	"ask":   "asks you before it uses a tool that can change things",
	"allow": "uses its tools without asking you",
	"deny":  "can't use its tools",
}

// handleMCPTrustForm sets a connector's live trust mode (ask | allow | deny).
func (s *Server) handleMCPTrustForm(w http.ResponseWriter, r *http.Request) {
	if s.mcp == nil {
		http.Redirect(w, r, "/mcp", http.StatusSeeOther)
		return
	}
	target := strings.TrimSpace(r.FormValue("target"))
	mode := strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	if target == "" || trustWords[mode] == "" {
		redirectFlash(w, r, "/mcp", "", "Pick Ask first, Don't ask or Block for a connected service.")
		return
	}
	if err := s.mcp.SetTrust(target, mode); err != nil {
		redirectFlash(w, r, "/mcp", "", s.flashFor(r.Context(), "mcp trust", err, "Couldn't change how your agent uses that service."))
		return
	}
	redirectFlash(w, r, "/mcp", target+": your agent now "+trustWords[mode]+".", "")
}
