package dashboard

// agents.go is the "Helpers" page (TEN-209): the named specialists the agent
// hands work to, each with its own model and instructions. It mirrors the
// terminal app's /agents through the same control: list, add, change the
// model (the instructions stay), set or clear the instructions, rename,
// remove, and Reset a customized built-in. Built-ins can be edited but never
// renamed or removed. Plain form posts answered with a redirect; no stream.

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

// AgentControl is the named-helper surface. cmd/tenant adapts the terminal
// app's agentControl (dashAgents); nil renders the "isn't available" state.
// Errors worth showing come back as PlainError (D21).
type AgentControl interface {
	Agents() ([]AgentProfileView, error)
	Soul(name string) (string, error) // full instructions, for the edit panel only
	Add(name, provider, model, description string) error
	SetModel(name, provider, model string) error // instructions and description kept
	SetSoul(name, soul string) error             // "" clears
	Rename(oldName, newName string) error
	Remove(name string) error   // a customized built-in goes back to its shipped settings
	RestartNote() string        // how to restart Tenant here (a Windows service); "" = nothing to add
	SendsTo(kind string) string // who a provider of this kind sends a helper's work to; "" = self-hosted
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
	mux.HandleFunc("POST /agents/add", s.handleAgentAdd)
	mux.HandleFunc("POST /agents/model", s.handleAgentModel)
	mux.HandleFunc("POST /agents/rename", s.handleAgentRename)
	// Clearing the instructions, removing a helper and resetting a built-in
	// each lose something the page can't bring back (DESIGN.md §5 (a)).
	mux.HandleFunc("POST /agents/soul", needConfirm(func(r *http.Request) bool {
		return strings.TrimSpace(r.FormValue("soul")) == ""
	}, s.handleAgentSoul))
	mux.HandleFunc("POST /agents/remove", needConfirm(nil, s.handleAgentRemove))
	mux.HandleFunc("POST /agents/reset", needConfirm(nil, s.handleAgentReset))
}

type agentsPageData struct {
	layoutData
	Configured bool
	Agents     []AgentProfileView
	Providers  []ModelView       // the Model page's providers, which a helper can use
	SendsTo    map[string]string // provider name → who its work goes to, hosted ones only (D18)
	Edit       *AgentProfileView // ?edit=: the helper whose panel is open
	EditSoul   string
	LoadErr    string
	Msg, Err   string
}

func (s *Server) handleAgentsPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := agentsPageData{
		layoutData: layoutData{Title: "Helpers", Page: "agents"},
		Configured: s.agents != nil,
		Msg:        q.Get("msg"),
		Err:        q.Get("err"),
	}
	if s.agents == nil {
		s.render(w, s.tmpl.agents, d)
		return
	}
	if s.models != nil {
		d.Providers = s.models.Models()
		d.SendsTo = map[string]string{}
		for _, p := range d.Providers {
			if to := s.agents.SendsTo(p.Kind); to != "" {
				d.SendsTo[p.Name] = to
			}
		}
	}
	agents, err := s.agents.Agents()
	if err != nil {
		d.LoadErr = s.flashFor(r.Context(), "helpers list", err, "Couldn't read your helpers. Reload the page to try again.")
	}
	d.Agents = agents
	if i := slices.IndexFunc(agents, func(a AgentProfileView) bool { return a.Name == q.Get("edit") }); i >= 0 {
		// Without its current instructions the editor would offer an empty
		// box, and saving that would lose them: no panel instead.
		if soul, err := s.agents.Soul(agents[i].Name); err != nil {
			d.LoadErr = s.flashFor(r.Context(), "helper instructions", err, "Couldn't read this helper's instructions. Reload the page to try again.")
		} else {
			d.Edit, d.EditSoul = &agents[i], soul
		}
	}
	s.render(w, s.tmpl.agents, d)
}

// agentsForm reads a helper form's trimmed name, or answers the post itself:
// back to the page when the control is nil or the name is missing.
func (s *Server) agentsForm(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := strings.TrimSpace(r.FormValue("name"))
	if s.agents == nil || name == "" {
		http.Redirect(w, r, "/agents", http.StatusSeeOther)
		return "", false
	}
	return name, true
}

// editPage is the helper's own panel, where model and instruction saves land.
func editPage(name string) string { return "/agents?" + url.Values{"edit": {name}}.Encode() }

// learnsOnRestart is the note after an add or a rename: the main agent's list
// of helpers is written at launch (the terminal app behaves the same).
func (s *Server) learnsOnRestart(name string) string {
	msg := "Your main agent learns about new and renamed helpers when Tenant restarts; until then, ask for " + name + " by name in chat."
	if how := s.agents.RestartNote(); how != "" {
		msg += " " + how
	}
	return msg
}

func tooLongModel(model string) bool { return utf8.RuneCountInString(model) > 200 }

// isCustomized reports whether name is a customized built-in. Reset acts only
// on those and Remove never does: both end in the control's Remove, which
// would otherwise delete a helper you added, or reset one under "Removed".
func (s *Server) isCustomized(name string) (bool, error) {
	rows, err := s.agents.Agents()
	return slices.ContainsFunc(rows, func(a AgentProfileView) bool { return a.Name == name && a.Customized }), err
}

func (s *Server) handleAgentAdd(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentsForm(w, r)
	if !ok {
		return
	}
	provider := strings.TrimSpace(r.FormValue("provider"))
	model := strings.TrimSpace(r.FormValue("model"))
	desc := strings.TrimSpace(r.FormValue("description"))
	switch {
	case provider == "":
		redirectFlash(w, r, "/agents", "", "Pick a model provider for the helper.")
		return
	case utf8.RuneCountInString(name) > 64:
		redirectFlash(w, r, "/agents", "", "Keep the name to 64 characters or fewer.")
		return
	case utf8.RuneCountInString(desc) > 500:
		redirectFlash(w, r, "/agents", "", "Keep what it's for to 500 characters or fewer.")
		return
	case tooLongModel(model):
		redirectFlash(w, r, "/agents", "", "Keep the model name to 200 characters or fewer.")
		return
	}
	if err := s.agents.Add(name, provider, model, desc); err != nil {
		redirectFlash(w, r, "/agents", "", s.flashFor(r.Context(), "helper add", err, "Couldn't add that helper."))
		return
	}
	redirectFlash(w, r, "/agents", "Added "+name+". "+s.learnsOnRestart(name), "")
}

func (s *Server) handleAgentModel(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentsForm(w, r)
	if !ok {
		return
	}
	provider := strings.TrimSpace(r.FormValue("provider"))
	model := strings.TrimSpace(r.FormValue("model"))
	switch {
	case provider == "":
		redirectFlash(w, r, editPage(name), "", "Pick a model provider for the helper.")
		return
	case tooLongModel(model):
		redirectFlash(w, r, editPage(name), "", "Keep the model name to 200 characters or fewer.")
		return
	}
	if err := s.agents.SetModel(name, provider, model); err != nil {
		redirectFlash(w, r, editPage(name), "", s.flashFor(r.Context(), "helper model", err, "Couldn't change that helper's model."))
		return
	}
	uses := provider
	if model != "" {
		uses += " · " + model
	}
	redirectFlash(w, r, editPage(name), name+" will use "+uses+" from its next task.", "")
}

func (s *Server) handleAgentSoul(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentsForm(w, r)
	if !ok {
		return
	}
	soul := r.FormValue("soul")
	if err := s.agents.SetSoul(name, soul); err != nil {
		redirectFlash(w, r, editPage(name), "", s.flashFor(r.Context(), "helper instructions", err, "Couldn't save the instructions."))
		return
	}
	msg := "Saved " + name + "'s instructions."
	if strings.TrimSpace(soul) == "" {
		msg = "Cleared " + name + "'s instructions."
	}
	redirectFlash(w, r, editPage(name), msg, "")
}

func (s *Server) handleAgentRename(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentsForm(w, r)
	if !ok {
		return
	}
	to := strings.TrimSpace(r.FormValue("new_name"))
	switch {
	case to == "":
		redirectFlash(w, r, editPage(name), "", "Type the helper's new name.")
		return
	case utf8.RuneCountInString(to) > 64:
		redirectFlash(w, r, editPage(name), "", "Keep the name to 64 characters or fewer.")
		return
	}
	if err := s.agents.Rename(name, to); err != nil {
		redirectFlash(w, r, editPage(name), "", s.flashFor(r.Context(), "helper rename", err, "Couldn't rename that helper."))
		return
	}
	redirectFlash(w, r, "/agents", "Renamed "+name+" to "+to+". "+s.learnsOnRestart(to), "")
}

func (s *Server) handleAgentRemove(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentsForm(w, r)
	if !ok {
		return
	}
	custom, err := s.isCustomized(name)
	if err == nil && custom {
		redirectFlash(w, r, "/agents", "", "Built-in helpers can't be removed. Reset undoes your changes to one.")
		return
	}
	if err == nil {
		err = s.agents.Remove(name)
	}
	if err != nil {
		redirectFlash(w, r, "/agents", "", s.flashFor(r.Context(), "helper remove", err, "Couldn't remove that helper."))
		return
	}
	redirectFlash(w, r, "/agents", "Removed "+name+".", "")
}

// handleAgentReset undoes the changes to a customized built-in: the control's
// Remove deletes the override, so the shipped helper is back.
func (s *Server) handleAgentReset(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentsForm(w, r)
	if !ok {
		return
	}
	custom, err := s.isCustomized(name)
	if err == nil && !custom {
		redirectFlash(w, r, "/agents", "", "Only a changed built-in helper can be reset.")
		return
	}
	if err == nil {
		err = s.agents.Remove(name)
	}
	if err != nil {
		redirectFlash(w, r, "/agents", "", s.flashFor(r.Context(), "helper reset", err, "Couldn't reset that helper."))
		return
	}
	redirectFlash(w, r, "/agents", name+" is back to its built-in model and instructions.", "")
}
