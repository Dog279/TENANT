package dashboard

// rest.go is TEN-77: the dashboard's JSON control surface. It reads s.tools
// (ToolList/SetEnabled/SetPluginEnabled/Plugins) for tool state and toggles,
// and reports best-effort status. mountREST is wired into routes() during
// Wave-2 integration (see the CONTRACT block in server.go).

import (
	"encoding/json"
	"net/http"
)

// restToolView is one tool as the REST API renders it: ToolInfo plus the
// `destructive` flag. The flag is the plugin's authoritative gate class
// (ToolInfo.Gated) — set when the tool's handler gates a send/destructive
// action. The JSON key stays `destructive` (the frontend badge reads it).
type restToolView struct {
	Name        string `json:"name"`
	Plugin      string `json:"plugin"`
	Enabled     bool   `json:"enabled"`
	Destructive bool   `json:"destructive"`
}

// restStatus is the GET /api/status payload. Kept deliberately small and
// extensible: the agent runner exposes no model/goal/version, so we report
// only what's truthfully available rather than fabricating fields.
type restStatus struct {
	Plugins      []string `json:"plugins"`
	ToolsEnabled int      `json:"tools_enabled"`
	ToolsTotal   int      `json:"tools_total"`
	Status       string   `json:"status"`
}

// restToggleRequest is the body of the POST toggle endpoints.
type restToggleRequest struct {
	Enabled bool `json:"enabled"`
}

// restToggleResponse is the success body of the POST toggle endpoints,
// echoing SetEnabled/SetPluginEnabled's (count, scope) return.
type restToggleResponse struct {
	Changed int    `json:"changed"`
	Scope   string `json:"scope"`
}

// restPostureRequest is the body of POST /api/posture: the desired send
// posture. true = Allow-send (enable every gated tool); false = Read-only
// (disable every gated tool).
type restPostureRequest struct {
	AllowSend bool `json:"allow_send"`
}

// restPostureResponse reports the resulting posture. GET returns only
// allow_send; POST also reports how many gated tools were toggled (changed)
// vs. skipped because SetEnabled errored (best-effort — see handleSetPosture).
type restPostureResponse struct {
	AllowSend bool `json:"allow_send"`
	Changed   int  `json:"changed,omitempty"`
	Skipped   int  `json:"skipped,omitempty"`
}

// mountREST registers the JSON control API on mux using Go 1.22
// method+pattern routing. Called from routes() at integration time.
func (s *Server) mountREST(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/tools", s.handleTools)
	mux.HandleFunc("POST /api/tools/{name}", s.handleSetTool)
	mux.HandleFunc("POST /api/plugins/{label}", s.handleSetPlugin)
	mux.HandleFunc("GET /api/posture", s.handleGetPosture)
	mux.HandleFunc("POST /api/posture", s.handleSetPosture)
}

// handleStatus reports plugins and tool counts. Best-effort and extensible.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	tools := s.tools.ToolList()
	enabled := 0
	for _, t := range tools {
		if t.Enabled {
			enabled++
		}
	}
	writeJSON(w, http.StatusOK, restStatus{
		Plugins:      s.tools.Plugins(),
		ToolsEnabled: enabled,
		ToolsTotal:   len(tools),
		Status:       "ok",
	})
}

// handleTools returns every tool with its authoritative `destructive`
// flag, sourced from the plugin's gate class (ToolInfo.Gated).
func (s *Server) handleTools(w http.ResponseWriter, _ *http.Request) {
	tools := s.tools.ToolList()
	views := make([]restToolView, 0, len(tools))
	for _, t := range tools {
		views = append(views, restToolView{
			Name:        t.Name,
			Plugin:      t.Plugin,
			Enabled:     t.Enabled,
			Destructive: t.Gated,
		})
	}
	writeJSON(w, http.StatusOK, views)
}

// handleSetTool toggles a single tool: POST /api/tools/{name} {"enabled":bool}.
func (s *Server) handleSetTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	req, ok := decodeToggle(w, r)
	if !ok {
		return
	}
	changed, scope, err := s.tools.SetEnabled(name, req.Enabled)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, restToggleResponse{Changed: changed, Scope: scope})
}

// handleSetPlugin toggles a plugin: POST /api/plugins/{label} {"enabled":bool}.
func (s *Server) handleSetPlugin(w http.ResponseWriter, r *http.Request) {
	label := r.PathValue("label")
	req, ok := decodeToggle(w, r)
	if !ok {
		return
	}
	changed, scope, err := s.tools.SetPluginEnabled(label, req.Enabled)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, restToggleResponse{Changed: changed, Scope: scope})
}

// handleGetPosture reports the current send posture: GET /api/posture ->
// {"allow_send":bool}. Convention: posture is derived from the GATED tools
// (ToolInfo.Gated, the authoritative gate class). allow_send is true iff
// there is at least one gated tool AND every gated tool is enabled — i.e.
// "all gated tools enabled" == send mode. With zero gated tools there is
// nothing to gate, so allow_send is false (Read-only).
func (s *Server) handleGetPosture(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, restPostureResponse{AllowSend: posture(s.tools.ToolList())})
}

// posture computes allow_send from a tool list per the convention documented
// on handleGetPosture: at least one gated tool, all gated tools enabled.
func posture(tools []ToolInfo) bool {
	gated := 0
	for _, t := range tools {
		if !t.Gated {
			continue
		}
		gated++
		if !t.Enabled {
			return false
		}
	}
	return gated > 0
}

// handleSetPosture drives the posture: POST /api/posture {"allow_send":bool}
// flips every gated tool to allow_send via SetEnabled. Per-tool failures are
// best-effort — e.g. enabling a gated tool of an unconfigured plugin can error
// through the lazy activator — so we skip the failures rather than failing the
// whole request, and report {allow_send, changed, skipped}. Non-gated tools
// are left untouched. The change expands/contracts agent capability, so it's
// audited via s.log.
func (s *Server) handleSetPosture(w http.ResponseWriter, r *http.Request) {
	var req restPostureRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	changed, skipped := 0, 0
	for _, t := range s.tools.ToolList() {
		if !t.Gated {
			continue
		}
		if _, _, err := s.tools.SetEnabled(t.Name, req.AllowSend); err != nil {
			skipped++
			s.log.Warn("dashboard: posture skipped gated tool",
				"tool", t.Name, "allow_send", req.AllowSend, "err", err)
			continue
		}
		changed++
	}

	s.log.Info("dashboard: posture changed",
		"allow_send", req.AllowSend, "changed", changed, "skipped", skipped)
	writeJSON(w, http.StatusOK, restPostureResponse{
		AllowSend: req.AllowSend,
		Changed:   changed,
		Skipped:   skipped,
	})
}

// decodeToggle reads and validates a toggle body, writing a 400 and
// returning ok=false on malformed input. DisallowUnknownFields keeps the
// contract tight so a typo'd field is a client error, not a silent no-op.
func decodeToggle(w http.ResponseWriter, r *http.Request) (restToggleRequest, bool) {
	var req restToggleRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return restToggleRequest{}, false
	}
	return req, true
}

// writeJSON encodes v as the JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError emits a JSON error envelope: {"error": msg}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
