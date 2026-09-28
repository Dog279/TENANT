package dashboard

// agents.go is the "Helpers" page (TEN-209): the named specialists the agent
// hands work to, each with its own model and instructions. The foundation
// lays the contract; WP-agents builds the page on it.

import "net/http"

// AgentControl is the named-helper surface. cmd/tenant adapts the terminal
// app's agentControl (dashAgents); nil renders the "isn't available" state.
type AgentControl interface {
	Agents() ([]AgentProfileView, error)
	Soul(name string) (string, error) // full instructions, for the edit panel only
	Add(name, provider, model, description string) error
	SetModel(name, provider, model string) error // instructions and description kept
	SetSoul(name, soul string) error             // "" clears
	Rename(oldName, newName string) error
	Remove(name string) error
}

// AgentProfileView is one helper, render-ready.
type AgentProfileView struct {
	Name, Provider, Model, Description string // Provider "" = uses your main model
	SendsTo                            string // hosted-API provider its work goes to; "" = self-hosted or the main model
	HasSoul                            bool
	Builtin                            bool // shipped and not customized: edit only
	Customized                         bool // a customized built-in: Reset restores it
	Valid                              bool
}

// SetAgents installs the helper control after construction.
func (s *Server) SetAgents(c AgentControl) { s.agents = c }

func (s *Server) mountAgentsSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /agents", s.handleAgentsPage)
}

type agentsPageData struct {
	layoutData
	Configured bool
	Msg, Err   string
}

func (s *Server) handleAgentsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, s.tmpl.agents, agentsPageData{
		layoutData: layoutData{Title: "Helpers", Page: "agents"},
		Configured: s.agents != nil,
		Msg:        r.URL.Query().Get("msg"),
		Err:        r.URL.Query().Get("err"),
	})
}
