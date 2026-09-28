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
// So Connect stays a plain post that reloads the page: its answer is an
// instruction to act on at another computer, which a banner keeps and a
// fading toast doesn't. Trust and Disconnect answer in place (mcpDone).

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
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

// Address is the service's address as the page shows it: without a user,
// password, query or fragment, which is where an address carries a key (F7).
func (v MCPServerView) Address() string {
	u, err := url.Parse(v.URL)
	if err != nil {
		return "(an address Tenant can't read)"
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = nil, "", "", "", false
	return u.String()
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
	d := s.mcpData()
	d.Err = r.URL.Query().Get("err")
	d.Msg = r.URL.Query().Get("msg")
	s.render(w, s.tmpl.mcp, d)
}

func (s *Server) mcpData() mcpPageData {
	d := mcpPageData{layoutData: layoutData{Title: "Remote services", Page: "mcp"}}
	if s.mcp != nil {
		d.Configured = true
		d.Servers = s.mcp.Servers()
	}
	return d
}

// mcpDone answers Trust and Disconnect: in place for a Datastar post, with
// #mcp-body re-rendered; a 303 with the flash otherwise.
func (s *Server) mcpDone(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	s.formDone(w, r, "/mcp", msg, errMsg, func() (string, error) {
		var buf bytes.Buffer
		err := s.tmpl.mcp.ExecuteTemplate(&buf, "mcp-body", s.mcpData())
		return buf.String(), err
	})
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
	// Reconnect names the service: its address, which can carry a key, never
	// rides through the page; a new address comes in the body only (REVSEC-9).
	var rawURL string
	if target := strings.TrimSpace(r.FormValue("target")); target != "" {
		for _, v := range s.mcp.Servers() {
			if v.Label == target {
				rawURL = v.URL
			}
		}
		if rawURL == "" {
			redirectFlash(w, r, "/mcp", "", "That service isn't on the list any more. Connect it again below.")
			return
		}
	} else {
		v, ok := bodyOnly(r, "url")
		if !ok {
			KeyInURL.Log(r.Context(), "cred", "remote service address")
			redirectFlash(w, r, "/mcp", "", "Nothing was connected: enter the address in the form, not the page's URL.")
			return
		}
		rawURL = strings.TrimSpace(v)
	}
	if rawURL == "" {
		redirectFlash(w, r, "/mcp", "", "Enter the service's address.")
		return
	}
	// A connect that fails later shows only in the log, so an address that
	// can't work is refused here, where the page can say so (UX-7).
	if u, err := url.Parse(rawURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		redirectFlash(w, r, "/mcp", "", "That isn't a service address. It starts with https:// (or http:// for one on this computer), like https://tools.example.com/v1.")
		return
	}
	ctx := context.WithoutCancel(r.Context())
	go func(u string) {
		if err := s.mcp.Connect(u); err != nil {
			// The error quotes the address, which can carry a key: it stays
			// in the masked url field only (F7).
			ConnectorNotAdded.Log(ctx, "url", u, "err", strings.ReplaceAll(err.Error(), u, "(the address)"))
		}
	}(rawURL)
	redirectFlash(w, r, "/mcp", "Connecting. A local service with no sign-in appears here in a moment (refresh); one that needs a sign-in opens a window on the computer running Tenant: approve it there first.", "")
}

func (s *Server) handleMCPRemoveForm(w http.ResponseWriter, r *http.Request) {
	if s.mcp == nil {
		s.mcpDone(w, r, "", "")
		return
	}
	target := strings.TrimSpace(r.FormValue("target"))
	if _, err := s.mcp.Remove(target); err != nil {
		s.mcpDone(w, r, "", s.flashFor(r.Context(), "mcp remove", err, "Couldn't disconnect that service."))
		return
	}
	s.mcpDone(w, r, "Service disconnected.", "")
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
		s.mcpDone(w, r, "", "")
		return
	}
	target := strings.TrimSpace(r.FormValue("target"))
	mode := strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	if target == "" || trustWords[mode] == "" {
		s.mcpDone(w, r, "", "Pick Ask first, Don't ask or Block for a connected service.")
		return
	}
	if err := s.mcp.SetTrust(target, mode); err != nil {
		s.mcpDone(w, r, "", s.flashFor(r.Context(), "mcp trust", err, "Couldn't change how your agent uses that service."))
		return
	}
	s.mcpDone(w, r, target+": your agent now "+trustWords[mode]+".", "")
}
