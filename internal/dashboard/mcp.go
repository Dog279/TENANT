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
}

// MCPServerView is one connected server's render-ready state.
type MCPServerView struct {
	Label     string
	URL       string
	Enabled   bool
	ToolCount int
}

// SetMCP installs the MCP control after construction (mirrors SetCron).
func (s *Server) SetMCP(c MCPControl) { s.mcp = c }

func (s *Server) mountMCPSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /mcp", s.handleMCPPage)
	mux.HandleFunc("POST /mcp/connect", s.handleMCPConnectForm)
	mux.HandleFunc("POST /mcp/remove", s.handleMCPRemoveForm)
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
// the connected server shows up in the list on refresh.
func (s *Server) handleMCPConnectForm(w http.ResponseWriter, r *http.Request) {
	if s.mcp == nil {
		http.Redirect(w, r, "/mcp", http.StatusSeeOther)
		return
	}
	rawURL := strings.TrimSpace(r.FormValue("url"))
	if rawURL == "" {
		redirectMCP(w, r, "", "Enter the service's MCP URL.")
		return
	}
	go func(u string) {
		if err := s.mcp.Connect(u); err != nil {
			s.log.Warn("dashboard: mcp connect", "url", u, "err", err)
		}
	}(rawURL)
	redirectMCP(w, r, "Opening a sign-in window on the computer running Tenant — approve it there, then refresh this page.", "")
}

func (s *Server) handleMCPRemoveForm(w http.ResponseWriter, r *http.Request) {
	if s.mcp == nil {
		http.Redirect(w, r, "/mcp", http.StatusSeeOther)
		return
	}
	target := strings.TrimSpace(r.FormValue("target"))
	if _, err := s.mcp.Remove(target); err != nil {
		redirectMCP(w, r, "", "Couldn't disconnect that service: "+err.Error())
		return
	}
	redirectMCP(w, r, "Service disconnected.", "")
}

func redirectMCP(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	target := "/mcp"
	switch {
	case errMsg != "":
		target += "?err=" + url.QueryEscape(errMsg)
	case msg != "":
		target += "?msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
