package dashboard

// ssr.go is TEN-107: server-rendered dashboard pages (stdlib html/template — no
// JS, no build step). Mounted at the root — the TEN-110 cutover moved these off
// the /page/ migration prefix and deleted the JS SPA. Datastar reactivity +
// real-time SSE arrive in TEN-108 / TEN-109. All page logic lives in Go, where
// the rest of the project lives — one language, one binary, air-gapped
// (templates embedded).

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
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
	cron          *template.Template // recurring-job admin section
	keys          *template.Template // write-only API-key settings (TEN-145)
	eval          *template.Template // eval & quality page (TEN-201)
	skills        *template.Template // skill library page (TEN-202)
}

func parseSSR() *ssrTemplates {
	must := func(files ...string) *template.Template {
		return template.Must(template.ParseFS(ssrFS, files...))
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
		cron:          must("templates/layout.html", "templates/cron.html"),
		keys:          must("templates/layout.html", "templates/keys.html"),
		eval:          must("templates/layout.html", "templates/eval.html"),
		skills:        must("templates/layout.html", "templates/skills.html"),
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

	// Real-time event stream (chat transcript + activity feed) — TEN-109.
	mux.HandleFunc("GET /events", s.handleEventsSSE)

	// Memory curator pages (TEN-111). Mounted unconditionally and nil-guarded:
	// a server without a MemoryControl renders an "isn't configured" state
	// rather than 404ing the nav link.
	mux.HandleFunc("GET /memory", s.handleMemoryPage)
	mux.HandleFunc("GET /memory/soul", s.handleMemorySoulPage)
	mux.HandleFunc("GET /memory/facts", s.handleMemoryFactsPage)
	mux.HandleFunc("GET /memory/facts/removed", s.handleMemoryRemovedPage)
	mux.HandleFunc("GET /memory/facts/temporal", s.handleMemoryTemporalPage)
	mux.HandleFunc("GET /memory/provenance", s.handleMemoryProvenancePage)

	mux.HandleFunc("POST /tools/{name}/toggle", s.handleToolToggleForm)
	mux.HandleFunc("POST /plugins/{label}/toggle", s.handlePluginToggleForm)
	mux.HandleFunc("POST /posture", s.handlePostureForm)
	mux.HandleFunc("POST /chat/send", s.handleChatSend)
	mux.HandleFunc("POST /chat/interject", s.handleChatInterject)
	mux.HandleFunc("POST /chat/stop", s.handleChatStop)

	// Memory mutations (form/303) — TEN-111.
	mux.HandleFunc("POST /memory/soul/edit", s.handleMemorySoulEditForm)
	mux.HandleFunc("POST /memory/facts/resolve", s.handleMemoryResolveForm)
	mux.HandleFunc("POST /memory/facts/{id}/delete", s.handleMemoryFactDeleteForm)
	mux.HandleFunc("POST /memory/facts/{id}/restore", s.handleMemoryFactRestoreForm)
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
		s.log.Error("dashboard: ssr render", "err", err)
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

type dashboardData struct {
	layoutData
	Plugins      []string
	ToolsEnabled int
	ToolsTotal   int
	WorkingCount int
	HasMemory    bool
	AllowSend    bool
	// Quality (TEN-200): plain status-board summary of the eval gate. HasQuality
	// is false when no EvalControl is wired or no check has run yet.
	HasQuality   bool
	QualityScore float64
	QualityTrend string // "up" | "steady" | "down"
	// Skills learned (live count) — 0/absent when no SkillControl is wired.
	HasSkills     bool
	SkillsLive    int
	SkillsWaiting int
}

func (s *Server) handleDashboardPage(w http.ResponseWriter, _ *http.Request) {
	tools := s.toolList()
	enabled := 0
	for _, t := range tools {
		if t.Enabled {
			enabled++
		}
	}
	var plugins []string
	if s.tools != nil {
		plugins = s.tools.Plugins()
	}
	d := dashboardData{
		layoutData:   layoutData{Title: "Overview", Page: "dashboard"},
		Plugins:      plugins,
		ToolsEnabled: enabled,
		ToolsTotal:   len(tools),
		AllowSend:    posture(tools),
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
	s.render(w, s.tmpl.dashboard, d)
}

type toolGroup struct {
	Plugin string
	AllOn  bool
	Tools  []ToolInfo
}
type toolsData struct {
	layoutData
	Groups    []toolGroup
	AllowSend bool
}

func (s *Server) handleToolsPage(w http.ResponseWriter, _ *http.Request) {
	tools := s.toolList()
	var order []string
	groups := map[string]*toolGroup{}
	for _, t := range tools {
		p := t.Plugin
		if p == "" {
			p = "(ungrouped)"
		}
		g, ok := groups[p]
		if !ok {
			g = &toolGroup{Plugin: p, AllOn: true}
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
	s.render(w, s.tmpl.tools, toolsData{
		layoutData: layoutData{Title: "Tools", Page: "tools"},
		Groups:     out,
		AllowSend:  posture(tools),
	})
}

func (s *Server) handleActivityPage(w http.ResponseWriter, _ *http.Request) {
	s.render(w, s.tmpl.activity, layoutData{Title: "Activity", Page: "activity"})
}
