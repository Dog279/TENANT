package dashboard

// ssr.go is TEN-107: server-rendered dashboard pages (stdlib html/template — no
// JS, no build step). Mounted at the root — the TEN-110 cutover moved these off
// the /page/ migration prefix and deleted the JS SPA. Datastar reactivity +
// real-time SSE arrive in TEN-108 / TEN-109. All page logic lives in Go, where
// the rest of the project lives — one language, one binary, air-gapped
// (templates embedded).

import (
	"bytes"
	"context"
	"embed"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

//go:embed templates
var ssrFS embed.FS

// embedAsset reads a required embedded file (panics if missing — a build/packaging bug).
func embedAsset(path string) []byte {
	b, err := ssrFS.ReadFile(path)
	if err != nil {
		panic("dashboard: embedded " + path + " missing: " + err.Error())
	}
	return b
}

// ssrCSS / datastarJS are embedded assets served at /styles.css and
// /datastar.js. Datastar (the vendored, MIT hypermedia runtime) is the ONLY JS
// shipped — there is no hand-written JS; all logic lives in Go (TEN-108).
var (
	ssrCSS     = embedAsset("templates/styles.css")
	datastarJS = embedAsset("templates/datastar.js")
)

// ssrTemplates is the set of parsed page templates (each = layout + one page).
// toolRow is the standalone tool-row partial used both inline (Tools page) and
// as the fragment patched over SSE on a toggle.
type ssrTemplates struct {
	dashboard     *template.Template
	tools         *template.Template
	activity      *template.Template
	chat          *template.Template
	toolRow       *template.Template
	memory        *template.Template // Memory overview (TEN-111)
	memSoul       *template.Template
	memFacts      *template.Template
	memRemoved    *template.Template
	memTemporal   *template.Template
	memProvenance *template.Template
	memFact       *template.Template // provenance inspector (TEN-297)
	cron          *template.Template // recurring-job admin section
	keys          *template.Template // write-only API-key settings (TEN-145)
	eval          *template.Template // eval & quality page (TEN-201)
	skills        *template.Template // skill library page (TEN-202)
	models        *template.Template // model backends page (TEN-204)
	mcp           *template.Template // remote MCP connectors page (TEN-205)
	integrations  *template.Template // integration-config page (TEN-206)
	access        *template.Template // iMessage + Discord access page (TEN-208)
	ui            *template.Template // ui.html alone: the shared partials patched over SSE (TEN-211)
	safety        *template.Template // approvals & safety page (TEN-207)
	approvalQ     *template.Template // the approval queue partial, patched over /events
	agents        *template.Template // helpers page (TEN-209)
	goal          *template.Template // autopilot page (TEN-212)
	research      *template.Template // research page (TEN-210)
	researchRun   *template.Template // one past research run
	researchParts *template.Template // the research partials, patched over /research/live
}

// parseSSR parses every set with the ui.html partials and their funcs
// (TEN-211), so any page can use ui-confirm, ui-flash and the rest.
func parseSSR() *ssrTemplates {
	must := func(files ...string) *template.Template {
		return template.Must(template.New(path.Base(files[0])).Funcs(uiFuncs).ParseFS(ssrFS, append(files, "templates/ui.html")...))
	}
	return &ssrTemplates{
		dashboard:     must("templates/layout.html", "templates/dashboard.html"),
		tools:         must("templates/layout.html", "templates/tools.html", "templates/toolrow.html"),
		activity:      must("templates/layout.html", "templates/activity.html"),
		chat:          must("templates/layout.html", "templates/chat.html"),
		toolRow:       must("templates/toolrow.html"),
		memory:        must("templates/layout.html", "templates/memnav.html", "templates/memory.html"),
		memSoul:       must("templates/layout.html", "templates/memnav.html", "templates/memory_soul.html"),
		memFacts:      must("templates/layout.html", "templates/memnav.html", "templates/memory_facts.html"),
		memRemoved:    must("templates/layout.html", "templates/memnav.html", "templates/memory_removed.html"),
		memTemporal:   must("templates/layout.html", "templates/memnav.html", "templates/memory_temporal.html"),
		memProvenance: must("templates/layout.html", "templates/memnav.html", "templates/memory_provenance.html"),
		memFact:       must("templates/layout.html", "templates/memnav.html", "templates/memory_fact.html"),
		cron:          must("templates/layout.html", "templates/cron.html"),
		keys:          must("templates/layout.html", "templates/keys.html"),
		eval:          must("templates/layout.html", "templates/eval.html"),
		skills:        must("templates/layout.html", "templates/skills.html"),
		models:        must("templates/layout.html", "templates/models.html"),
		mcp:           must("templates/layout.html", "templates/mcp.html"),
		integrations:  must("templates/layout.html", "templates/integrations.html"),
		access:        must("templates/layout.html", "templates/access.html"),
		ui:            template.Must(template.New("ui.html").Funcs(uiFuncs).ParseFS(ssrFS, "templates/ui.html")),
		safety:        must("templates/layout.html", "templates/safety.html", "templates/approvalq.html"),
		approvalQ:     must("templates/approvalq.html"),
		agents:        must("templates/layout.html", "templates/agents.html"),
		goal:          must("templates/layout.html", "templates/goal.html"),
		research:      must("templates/layout.html", "templates/research.html", "templates/research_parts.html"),
		researchRun:   must("templates/layout.html", "templates/research_run.html", "templates/research_parts.html"),
		researchParts: must("templates/research_parts.html"),
	}
}

// mountSSR registers the page routes + their form actions + the stylesheet at
// the root. The TEN-110 cutover moved these off the /page/ migration prefix; the
// SSR dashboard now owns GET / (GET /{$}) with no JS SPA behind it.
func (s *Server) mountSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /styles.css", s.handleSSRCSS)
	mux.HandleFunc("GET /datastar.js", s.handleDatastarJS)
	mux.HandleFunc("GET /{$}", s.handleDashboardPage)
	mux.HandleFunc("GET /tools", s.handleToolsPage)
	mux.HandleFunc("GET /activity", s.handleActivityPage)
	mux.HandleFunc("GET /chat", s.handleChatPage)
	mux.HandleFunc("GET /ui/status", s.handleUIStatus) // the status strip every page polls (TEN-211)

	// Real-time event stream (chat transcript + activity feed) — TEN-109.
	mux.HandleFunc("GET /events", s.handleEventsSSE)
	mux.HandleFunc("GET /activity/events", s.handleActivitySSE)                 // log-hub backfill + replay stream
	mux.HandleFunc("GET /activity/files/{source}/{name}", s.handleActivityFile) // one on-disk log file, plain text

	// Memory curator pages (TEN-111). Mounted unconditionally and nil-guarded:
	// a server without a MemoryControl renders an "isn't configured" state
	// rather than 404ing the nav link.
	mux.HandleFunc("GET /memory", s.handleMemoryPage)
	mux.HandleFunc("GET /memory/soul", s.handleMemorySoulPage)
	mux.HandleFunc("GET /memory/facts", s.handleMemoryFactsPage)
	mux.HandleFunc("GET /memory/facts/removed", s.handleMemoryRemovedPage)
	mux.HandleFunc("GET /memory/facts/temporal", s.handleMemoryTemporalPage)
	mux.HandleFunc("GET /memory/provenance", s.handleMemoryProvenancePage)
	mux.HandleFunc("GET /memory/facts/{id}", s.handleMemoryFactPage) // the provenance inspector (TEN-297)

	mux.HandleFunc("POST /tools/{name}/toggle", s.handleToolToggleForm)
	mux.HandleFunc("POST /plugins/{label}/toggle", s.handlePluginToggleForm)
	// Giving the agent its tools that change things, a rule that lets a tool
	// run without asking, and removing a rule that made one ask or blocked
	// it all confirm (TEN-211). The remove form posts the rule's current
	// mode, so a missing one confirms too.
	mux.HandleFunc("POST /posture", needConfirm(confirmUnless("allow_send", "", "false"), s.handlePostureForm))
	mux.HandleFunc("POST /tools/rules/add", needConfirm(confirmUnless("mode", "ask", "deny"), s.handleRuleAddForm))
	mux.HandleFunc("POST /tools/rules/remove", needConfirm(func(r *http.Request) bool {
		return s.ruleMode(strings.TrimSpace(r.FormValue("pattern"))) != "allow"
	}, s.handleRuleRemoveForm))
	mux.HandleFunc("POST /chat/send", s.handleChatSend)
	mux.HandleFunc("POST /chat/interject", s.handleChatInterject)
	mux.HandleFunc("POST /chat/stop", s.handleChatStop)

	// Memory mutations (form/303) — TEN-111.
	mux.HandleFunc("POST /memory/soul/edit", s.handleMemorySoulEditForm)
	mux.HandleFunc("POST /memory/facts/resolve", s.handleMemoryResolveForm)
	mux.HandleFunc("POST /memory/facts/{id}/delete", s.handleMemoryFactDeleteForm)
	mux.HandleFunc("POST /memory/facts/{id}/restore", s.handleMemoryFactRestoreForm)
	mux.HandleFunc("POST /memory/facts/{id}/edit", s.handleMemoryFactEditForm)
	mux.HandleFunc("POST /memory/facts/{id}/invalidate", s.handleMemoryFactInvalidateForm)
	mux.HandleFunc("POST /memory/facts/{id}/revalidate", s.handleMemoryFactRevalidateForm)
	mux.HandleFunc("POST /memory/facts/{id}/restore-version", s.handleMemoryFactRestoreVersionForm)
}

func (s *Server) handleSSRCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write(ssrCSS)
}

func (s *Server) handleDatastarJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(datastarJS)
}

// renderToolRow renders the #tool-<name> fragment for one tool (the unit
// Datastar patches into the DOM after a toggle).
func (s *Server) renderToolRow(t ToolInfo) (string, error) {
	var buf bytes.Buffer
	if err := s.tmpl.toolRow.ExecuteTemplate(&buf, "toolrow", t); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Label is the tool's name as the page shows it. A remote service's tool is
// named "mcp:<service>:<tool>"; its group already names the service, so the
// row shows only the tool.
func (t ToolInfo) Label() string {
	if rest, ok := strings.CutPrefix(t.Name, "mcp:"); ok {
		if i := strings.LastIndexByte(rest, ':'); i >= 0 && i < len(rest)-1 {
			return rest[i+1:]
		}
	}
	return t.Name
}

// ToggleURL is the row's toggle target. A remote tool server names its own
// tools, so the name is path-escaped: a raw "../" in it would aim the button
// at another dashboard route (F1).
func (t ToolInfo) ToggleURL() string {
	return "/tools/" + url.PathEscape(t.Name) + "/toggle?enabled=" + strconv.FormatBool(!t.Enabled)
}

// findTool returns the current state of the named tool (zero value if gone).
func (s *Server) findTool(name string) ToolInfo {
	for _, t := range s.toolList() {
		if t.Name == name {
			return t
		}
	}
	return ToolInfo{Name: name}
}

// render executes t into a buffer first (so a template error can't half-write a
// 200) then flushes. On error it logs and 500s.
func (s *Server) render(w http.ResponseWriter, t *template.Template, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		RenderFailed.Log(context.Background(), "page", t.Name(), "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// layoutData is the shell data every page embeds (active nav + title). Sub is
// the active second-level tab, used by the Memory sub-nav (empty elsewhere).
type layoutData struct {
	Title string
	Page  string
	Sub   string
}

func (s *Server) toolList() []ToolInfo {
	if s.tools == nil {
		return nil
	}
	return s.tools.ToolList()
}

// shortName is an integration's name without the description its label
// carries: "Web browsing" for "Web browsing (Chrome + optional …)".
func shortName(label string) string {
	for _, sep := range []string{" (", " — ", " - "} {
		if i := strings.Index(label, sep); i > 0 {
			label = label[:i]
		}
	}
	return label
}

// navLink is a name that links to its page.
type navLink struct{ Name, Href string }

type dashboardData struct {
	layoutData
	Plugins      []navLink // the connected services, each to its page
	ToolsEnabled int
	ToolsTotal   int
	WorkingCount int
	HasMemory    bool
	ActTools     int // the tools that change things that are on (actTools)
	// Quality (TEN-200): plain status-board summary of the eval gate. HasQuality
	// is false when no EvalControl is wired or no check has run yet.
	HasQuality   bool
	QualityScore float64
	QualityTrend string // "up" | "steady" | "down"
	// Skills learned (live count) — 0/absent when no SkillControl is wired.
	HasSkills     bool
	SkillsLive    int
	SkillsWaiting int
	// Active model (TEN-204) — "" when no ModelControl is wired.
	ActiveModel   string
	ModelDegraded bool
	// Alerts fired in the last day (TEN-399), and the newest one's message.
	Alerts      int
	AlertLatest string
	// ModelTrouble is how long ago a model call last failed, within the
	// hour; "" with none.
	ModelTrouble string
	// Waiting is how many actions wait for the operator's OK (D13): the
	// card answers "is anything waiting?" even when the answer is no.
	Waiting int
}

func (s *Server) handleDashboardPage(w http.ResponseWriter, r *http.Request) {
	tools := s.toolList()
	enabled := 0
	for _, t := range tools {
		if t.Enabled {
			enabled++
		}
	}
	// Connected services are the integrations set up and the remote services
	// connected, by name: not every plugin built in, set up or not (UX-5).
	var services []navLink
	if s.integrations != nil {
		for _, v := range s.integrations.Integrations() {
			if v.Configured {
				services = append(services, navLink{shortName(v.Label), "/integrations"})
			}
		}
	}
	if s.mcp != nil {
		for _, v := range s.mcp.Servers() {
			if v.Enabled {
				services = append(services, navLink{strings.TrimPrefix(v.Label, "mcp:"), "/mcp"})
			}
		}
	}
	d := dashboardData{
		layoutData:   layoutData{Title: "Overview", Page: "dashboard"},
		Plugins:      services,
		ToolsEnabled: enabled,
		ToolsTotal:   len(tools),
		ActTools:     actTools(tools),
	}
	if s.mem != nil {
		d.HasMemory = true
		d.WorkingCount = s.mem.WorkingCount()
	}
	if s.eval != nil {
		if sch := s.eval.Schedule(); sch.HasRun {
			d.HasQuality = true
			d.QualityScore = sch.LastScore
			d.QualityTrend = sch.Trend
		}
	}
	if s.skills != nil {
		d.HasSkills = true
		for _, sk := range s.skills.Skills() {
			switch sk.Status {
			case "proposed":
				d.SkillsWaiting++
			case "tombstoned":
			default:
				d.SkillsLive++
			}
		}
	}
	if s.models != nil {
		for _, m := range s.models.Models() {
			if m.Active {
				d.ActiveModel = m.Name
				if m.Model != "" {
					d.ActiveModel = m.Name + " · " + m.Model
				}
				d.ModelDegraded = m.Degraded
				break
			}
		}
	}
	d.Alerts, d.AlertLatest = s.recentAlerts(r.Context())
	if at := s.modelTrouble(r.Context()); !at.IsZero() {
		d.ModelTrouble = relAge(at)
	}
	d.Waiting = s.statusView().Approvals
	s.render(w, s.tmpl.dashboard, d)
}

type toolGroup struct {
	Plugin string // "" for tools no plugin owns
	Label  string // the group's heading
	AllOn  bool
	Tools  []ToolInfo
}

// groupLabel is a tool group's heading: a remote service's plugin
// ("mcp:<service>") is named as one, and tools no plugin owns are "Other
// tools".
func groupLabel(plugin string) string {
	if plugin == "" {
		return "Other tools"
	}
	if svc, ok := strings.CutPrefix(plugin, "mcp:"); ok {
		return svc + " (remote service)"
	}
	return plugin
}

type toolsData struct {
	layoutData
	Groups    []toolGroup
	AllowSend bool // every tool that changes things is on
	ActTools  int  // how many of them are on
	// Per-tool permission rules card (RuleControl); RulesOn=false hides it.
	RulesOn  bool
	Rules    []ToolRuleRow
	Msg, Err string
}

func (s *Server) handleToolsPage(w http.ResponseWriter, r *http.Request) {
	tools := s.toolList()
	var order []string
	groups := map[string]*toolGroup{}
	for _, t := range tools {
		p := t.Plugin
		g, ok := groups[p]
		if !ok {
			g = &toolGroup{Plugin: p, Label: groupLabel(p), AllOn: true}
			groups[p] = g
			order = append(order, p)
		}
		g.Tools = append(g.Tools, t)
		if !t.Enabled {
			g.AllOn = false
		}
	}
	out := make([]toolGroup, 0, len(order))
	for _, p := range order {
		out = append(out, *groups[p])
	}
	d := toolsData{
		layoutData: layoutData{Title: "Tools", Page: "tools"},
		Groups:     out,
		AllowSend:  posture(tools),
		ActTools:   actTools(tools),
		Msg:        r.URL.Query().Get("msg"),
		Err:        r.URL.Query().Get("err"),
	}
	if s.rules != nil {
		d.RulesOn = true
		d.Rules = s.rules.Rules()
	}
	s.render(w, s.tmpl.tools, d)
}

// handleActivityPage + the activity SSE live in ssr_activity.go (TEN-238).
