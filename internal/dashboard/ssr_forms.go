package dashboard

// ssr_forms.go is TEN-107: the form-action handlers behind the server-rendered
// pages. Each is a POST that mutates state through the SAME ToolControl methods
// the REST API uses, then 303-redirects (POST/redirect/GET) so a browser refresh
// can't double-submit. Values come from r.FormValue, not JSON. The posture and
// rule forms answer a Datastar post in place instead (toolsDone).

import "net/http"

// toolsView is the Tools page's data: the tools by group, the posture, and
// the rules card.
func (s *Server) toolsView() toolsData {
	tools := s.toolList()
	var order []string
	groups := map[string]*toolGroup{}
	for _, t := range tools {
		g, ok := groups[t.Plugin]
		if !ok {
			g = &toolGroup{Plugin: t.Plugin, Label: groupLabel(t.Plugin), AllOn: true}
			groups[t.Plugin] = g
			order = append(order, t.Plugin)
		}
		g.Tools = append(g.Tools, t)
		g.AllOn = g.AllOn && t.Enabled
	}
	d := toolsData{
		layoutData: layoutData{Title: "Tools", Page: "tools"},
		Groups:     make([]toolGroup, 0, len(order)),
		AllowSend:  posture(tools),
		ActTools:   actTools(tools),
	}
	for _, p := range order {
		d.Groups = append(d.Groups, *groups[p])
	}
	if s.rules != nil {
		d.RulesOn = true
		d.Rules = s.rules.Rules()
	}
	return d
}

// toolsDone answers the posture and rule forms: in place for a Datastar
// post, with #tools-body re-rendered; a 303 with the flash otherwise.
func (s *Server) toolsDone(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	s.formDone(w, r, "/tools", msg, errMsg, func() (string, error) {
		return part(s.tmpl.tools, "tools-body", s.toolsView())
	})
}

// handleToolToggleForm toggles a tool. A Datastar request (data-on-click) gets a
// single-row SSE patch (no full reload); a plain form POST falls back to 303.
func (s *Server) handleToolToggleForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.tools != nil {
		// r.FormValue reads the ?enabled= query param too (ParseForm merges them).
		if _, _, err := s.tools.SetEnabled(name, r.FormValue("enabled") == "true"); err != nil {
			ActionFailed.Log(r.Context(), "action", "tool toggle", "target", name, "err", err)
		}
	}
	if r.Header.Get("Datastar-Request") != "" {
		if row, err := s.renderToolRow(s.findTool(name)); err == nil {
			if perr := writeDatastarPatch(w, row); perr != nil {
				s.log.Debug("dashboard: ssr tool toggle patch not sent", "tool", name, "err", perr) // the page went away
			}
			return
		}
	}
	http.Redirect(w, r, "/tools", http.StatusSeeOther)
}

func (s *Server) handlePluginToggleForm(w http.ResponseWriter, r *http.Request) {
	if s.tools != nil {
		label := r.PathValue("label")
		if _, _, err := s.tools.SetPluginEnabled(label, r.FormValue("enabled") == "true"); err != nil {
			ActionFailed.Log(r.Context(), "action", "plugin toggle", "target", label, "err", err)
		}
	}
	http.Redirect(w, r, "/tools", http.StatusSeeOther)
}

// handlePostureForm flips every gated tool to allow_send — mirrors the REST
// handleSetPosture; per-tool failures are best-effort + logged, never fatal.
func (s *Server) handlePostureForm(w http.ResponseWriter, r *http.Request) {
	if s.tools == nil {
		s.toolsDone(w, r, "", "")
		return
	}
	allow := r.FormValue("allow_send") == "true"
	for _, t := range s.tools.ToolList() {
		if !t.Gated {
			continue
		}
		if _, _, err := s.tools.SetEnabled(t.Name, allow); err != nil {
			ActionFailed.Log(r.Context(), "action", "posture", "target", t.Name, "err", err)
		}
	}
	if allow {
		s.toolsDone(w, r, "Tools that change things are on. Your safety settings still decide which of them ask you first.", "")
		return
	}
	s.toolsDone(w, r, "Tools that change things are off.", "")
}
