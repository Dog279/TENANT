package dashboard

// rest.go is TEN-77: the dashboard's JSON control surface. It reads s.tools
// (ToolList/SetEnabled/SetPluginEnabled/Plugins) for tool state and toggles,
// and reports best-effort status. mountREST is wired into routes() during
// Wave-2 integration (see the CONTRACT block in server.go).

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tenant/internal/agent"
	"tenant/internal/logs"
	"tenant/internal/orchestra"
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
//
// The TEN-194 serve-liveness fields (turn_active/turn_age_secs/pending_approvals)
// are zero-valued in the interactive TUI (no turnGate, no headless approval
// drain) and only meaningful for a `tenant serve` daemon, where the doctor
// serve-mode check reads them to flag a stuck turn or a wedged approval.
type restStatus struct {
	Plugins      []string `json:"plugins"`
	ToolsEnabled int      `json:"tools_enabled"`
	ToolsTotal   int      `json:"tools_total"`
	Status       string   `json:"status"`

	TurnActive       bool                    `json:"turn_active"`
	TurnAgeSecs      int                     `json:"turn_age_secs,omitempty"`
	TurnQueueDepth   int                     `json:"turn_queue_depth"`
	TurnQueueAgeSecs int                     `json:"turn_queue_age_secs"`
	TurnQueue        []restTurnQueueEntry    `json:"turn_queue,omitempty"`
	PendingApprovals int                     `json:"pending_approvals"`
	TeamBudget       *orchestra.BudgetStatus `json:"team_budget,omitempty"`
}

// restTurnQueueEntry is the content-free queue projection. Position reflects
// scheduler dispatch order, not raw arrival order across priority classes.
type restTurnQueueEntry struct {
	Position int    `json:"position"`
	Source   string `json:"source"`
	AgeSecs  int    `json:"age_secs"`
	ETASecs  int    `json:"eta_secs"`
}

// liveness is the optional turn-state reporter a serve-mode runner (the
// turnGate) implements. The dashboard reads it via duck-typing so the minimal
// AgentRunner interface (Turn/Interject) stays unchanged.
type liveness interface {
	ActiveTurn() (active bool, age time.Duration)
}

type queueLiveness interface {
	TurnQueueStatus() agent.TurnQueueStatus
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
	// TEN-194: headless approval queue + structured activity projection.
	mux.HandleFunc("GET /api/approvals", s.handleApprovals)
	mux.HandleFunc("POST /api/approvals/{id}", s.handleApprovalDecide)
	mux.HandleFunc("GET /api/activity", s.handleActivity)
	mux.HandleFunc("POST /api/addon/event", s.handleAddonEvent)
	// The live logging API (rest_logs.go): sources = folders = tabs; anything
	// may write a line into a new source without a rebuild.
	mux.HandleFunc("GET /api/logs", s.handleLogSources)
	mux.HandleFunc("GET /api/logs/{source}", s.handleLogFiles)
	mux.HandleFunc("POST /api/logs/{source}", s.handleLogWrite)
	mux.HandleFunc("GET /api/logs/{source}/{name}", s.handleLogFile)
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
	st := restStatus{
		Plugins:      s.tools.Plugins(),
		ToolsEnabled: enabled,
		ToolsTotal:   len(tools),
		Status:       "ok",
	}
	// Serve-mode liveness (TEN-194): present only when a turnGate runner +
	// headless approval drain are wired; zero-valued otherwise.
	if lr, ok := s.agent.(liveness); ok && lr != nil {
		if active, age := lr.ActiveTurn(); active {
			st.TurnActive = true
			st.TurnAgeSecs = int(age.Seconds())
		}
	}
	if qr, ok := s.agent.(queueLiveness); ok && qr != nil {
		queue := qr.TurnQueueStatus()
		st.TurnQueueDepth = queue.Depth
		st.TurnQueueAgeSecs = int(queue.OldestAge.Seconds())
		for _, entry := range queue.Entries {
			st.TurnQueue = append(st.TurnQueue, restTurnQueueEntry{
				Position: entry.Position,
				Source:   string(entry.Source),
				AgeSecs:  int(entry.Age.Seconds()),
				ETASecs:  int(entry.ETA.Seconds()),
			})
		}
	}
	if s.approvals != nil {
		st.PendingApprovals = len(s.approvals.Pending())
	}
	if s.logs != nil {
		entries, _ := s.logs.Snapshot()
		for i := len(entries) - 1; i >= 0; i-- {
			if tb := teamBudgetOf(entries[i]); tb != nil {
				st.TeamBudget = tb
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, st)
}

// restActivityEvent is one retained entry as the JSON activity projection
// renders it (TEN-194): structured fields a programmatic attach client and the
// doctor liveness probe can read directly — NOT the Datastar HTML the SSR
// /activity feed streams. Mirrors the hub's lossless cursor. The original
// event-shaped fields (kind/agent/iter/tool/is_err/text) are kept for existing
// clients; source/level/msg/fields carry the unified log-book shape.
type restActivityEvent struct {
	Seq        uint64                  `json:"seq"`
	At         string                  `json:"at"` // RFC3339
	Kind       string                  `json:"kind"`
	Agent      string                  `json:"agent,omitempty"`
	Iter       int                     `json:"iter,omitempty"`
	Tool       string                  `json:"tool,omitempty"`
	IsErr      bool                    `json:"is_err,omitempty"`
	Text       string                  `json:"text,omitempty"`
	Source     string                  `json:"source"`
	Level      string                  `json:"level"`
	Msg        string                  `json:"msg"`
	Fields     map[string]string       `json:"fields,omitempty"`
	TeamBudget *orchestra.BudgetStatus `json:"team_budget,omitempty"`
}

type restActivityResponse struct {
	Events []restActivityEvent `json:"events"`
	Cursor uint64              `json:"cursor"` // pass back as ?since= for a gap-free tail
}

// restEntry projects one hub entry. kind is "log" for a plain record.
func restEntry(e logs.Entry) restActivityEvent {
	out := restActivityEvent{
		Seq: e.Seq, At: e.At.UTC().Format(time.RFC3339), Kind: e.Kind(),
		Agent: e.Field("agent"), Tool: e.Field("tool"), IsErr: e.IsErr(),
		Source: e.Source, Level: logs.LevelName(e.Level), Msg: e.Msg,
	}
	if out.Kind == "" {
		out.Kind = "log"
	}
	if it := e.Field("iter"); it != "" {
		out.Iter, _ = strconv.Atoi(it)
	}
	switch out.Kind {
	case "tool_call":
		out.Text = e.Field("args")
	case "tool_result":
		out.Text = e.Field("result")
	case "log":
		out.Text = e.Msg
	default:
		out.Text = e.Field("text")
		if out.Text == "" {
			out.Text = e.Msg
		}
	}
	out.TeamBudget = teamBudgetOf(e)
	if len(e.Fields) > 0 {
		out.Fields = make(map[string]string, len(e.Fields))
		for _, f := range e.Fields {
			if f.Key == "ev" || f.Key == "team_budget" {
				continue
			}
			out.Fields[f.Key] = f.Value
		}
	}
	return out
}

// handleActivity — GET /api/activity?since=<seq>[&source=x][&level=error][&trace=id]:
// the structured replay of the retained log. Returns entries with Seq > since
// plus the new head cursor. Empty (not an error) when no hub is wired.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	resp := restActivityResponse{Events: []restActivityEvent{}}
	if s.logs == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	var since uint64
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			since = n
		}
	}
	f := activityFilterFrom(r)
	entries, cursor := s.logs.Since(since)
	resp.Cursor = cursor
	for _, e := range entries {
		if !f.match(e) {
			continue
		}
		resp.Events = append(resp.Events, restEntry(e))
	}
	writeJSON(w, http.StatusOK, resp)
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

// restAddonEvent is the POST /api/addon/event request body: how an EXTERNAL
// add-on (a sidecar script, a watcher, custom tooling) reports one activity
// line into the shared audited feed. Auth is the same Bearer token as every
// other dashboard route; the event lands in the retained log (backfill) and
// the live broker (open feeds) exactly like native activity.
type restAddonEvent struct {
	Source string `json:"source"`          // add-on name, shown as the agent chip
	Text   string `json:"text"`            // the activity line
	Level  string `json:"level,omitempty"` // "error" marks the row as an error; anything else = info
}

// addonSourceOK validates the add-on name: short, filesystem/HTML-boring
// charset so a rogue caller can't smuggle markup or unbounded labels into the
// feed. (Rows are html-escaped at render regardless — defense in depth.)
func addonSourceOK(s string) bool {
	if len(s) == 0 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ' ':
		default:
			return false
		}
	}
	return true
}

func (s *Server) handleAddonEvent(w http.ResponseWriter, r *http.Request) {
	var req restAddonEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Source = strings.TrimSpace(req.Source)
	req.Text = strings.TrimSpace(req.Text)
	if !addonSourceOK(req.Source) {
		writeError(w, http.StatusBadRequest, "source: 1-40 chars of [A-Za-z0-9._ -] required")
		return
	}
	if req.Text == "" {
		writeError(w, http.StatusBadRequest, "text required")
		return
	}
	if len(req.Text) > 2000 {
		req.Text = req.Text[:2000]
	}
	if s.logs == nil && s.broker == nil {
		writeError(w, http.StatusServiceUnavailable, "activity feed not configured")
		return
	}
	// The add-on's name is its folder/tab; the broker still sees the event so
	// the live TUI feed shows it exactly as before.
	ev := agent.Event{Kind: agent.EventAddon, Agent: req.Source, Text: req.Text, IsErr: req.Level == "error"}
	if s.broker != nil {
		s.broker.Publish(ev)
	}
	if s.logs != nil {
		if e, ok := agent.EntryFor(ev); ok {
			s.logs.Log(e)
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "ok", "source": logs.NormalizeSource(req.Source)})
}

// teamBudgetOf decodes the team-budget snapshot an EventTeamBudget entry
// carries (agent.EntryFor stores it as JSON in the "team_budget" field).
func teamBudgetOf(e logs.Entry) *orchestra.BudgetStatus {
	raw := e.Field("team_budget")
	if raw == "" {
		return nil
	}
	var st orchestra.BudgetStatus
	if json.Unmarshal([]byte(raw), &st) != nil {
		return nil
	}
	return &st
}
