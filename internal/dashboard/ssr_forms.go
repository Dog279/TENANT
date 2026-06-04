package dashboard

// ssr_forms.go is TEN-107: the form-action handlers behind the server-rendered
// pages. Each is a POST that mutates state through the SAME ToolControl methods
// the REST API uses, then 303-redirects (POST/redirect/GET) so a browser refresh
// can't double-submit. Values come from r.FormValue, not JSON.

import "net/http"

// handleToolToggleForm toggles a tool. A Datastar request (data-on-click) gets a
// single-row SSE patch (no full reload); a plain form POST falls back to 303.
func (s *Server) handleToolToggleForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.tools != nil {
		// r.FormValue reads the ?enabled= query param too (ParseForm merges them).
		if _, _, err := s.tools.SetEnabled(name, r.FormValue("enabled") == "true"); err != nil {
			s.log.Warn("dashboard: ssr tool toggle", "tool", name, "err", err)
		}
	}
	if r.Header.Get("Datastar-Request") != "" {
		if row, err := s.renderToolRow(s.findTool(name)); err == nil {
			if perr := writeDatastarPatch(w, row); perr != nil {
				s.log.Warn("dashboard: ssr tool toggle patch", "tool", name, "err", perr)
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
			s.log.Warn("dashboard: ssr plugin toggle", "plugin", label, "err", err)
		}
	}
	http.Redirect(w, r, "/tools", http.StatusSeeOther)
}

// handlePostureForm flips every gated tool to allow_send — mirrors the REST
// handleSetPosture; per-tool failures are best-effort + logged, never fatal.
func (s *Server) handlePostureForm(w http.ResponseWriter, r *http.Request) {
	if s.tools != nil {
		allow := r.FormValue("allow_send") == "true"
		for _, t := range s.tools.ToolList() {
			if !t.Gated {
				continue
			}
			if _, _, err := s.tools.SetEnabled(t.Name, allow); err != nil {
				s.log.Warn("dashboard: ssr posture", "tool", t.Name, "err", err)
			}
		}
	}
	http.Redirect(w, r, "/tools", http.StatusSeeOther)
}
