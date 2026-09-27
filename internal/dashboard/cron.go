package dashboard

// cron.go is the server-rendered Cron admin section: list / add / enable /
// disable / run / delete recurring jobs. It mirrors the Memory curator's
// shape — a CronControl interface (satisfied in cmd/tenant), unconditional
// route mounting with nil-guarded handlers, and form/303 mutations so every
// view is a plain GET that httptest can assert on and a browser refresh can't
// double-submit. All job fields render through html/template's auto-escaping
// (no template.HTML), so a hostile job name/prompt cannot inject markup.
//
// Every route here is registered on the server mux, which Run() wraps in
// secure() (auth + same-origin + fail-closed bind policy) — so the whole Cron
// surface rides the dashboard's existing auth. Mutations are POST only.

import (
	"net/http"
	"net/url"
	"strings"
)

// CronControl is the runtime cron surface the dashboard drives. cmd/tenant
// adapts its cron manager to it (see dashCron there). A nil control renders an
// "isn't configured" state rather than 404ing the nav link.
type CronControl interface {
	Jobs() []CronJobView
	Add(spec CronAddSpec) error
	Remove(id string) error
	SetEnabled(id string, on bool) error
	RunNow(id string) error
	// ExecEnabled reports the GLOBAL cron-exec kill-switch. Distinct from a job's
	// own Exec opt-in: a job marked exec still runs read/comms-safe while this is
	// off, which silently strips gated tools (e.g. discord_send_message) from it.
	ExecEnabled() bool
	// SetExec flips that kill-switch and persists it. Takes effect live.
	SetExec(on bool) error
}

// CronAddSpec is the input to Add. Kind is "" (prompt) / "prompt" / "shell";
// Exec opts a prompt job into the dangerous tool surface; TZ is optional.
type CronAddSpec struct {
	Name   string
	Spec   string
	Prompt string
	Kind   string
	Exec   bool
	TZ     string
}

// CronJobView is one job's render-ready state. Time fields are pre-formatted
// strings ("" when unset).
type CronJobView struct {
	ID         string
	Name       string
	Spec       string
	Prompt     string
	Enabled    bool
	Kind       string
	Exec       bool
	TZ         string
	NextRun    string
	LastRun    string
	LastStatus string
}

// SetCron installs the cron control after construction (mirrors the optional
// MemoryControl). Safe to call before the server starts serving; handlers
// nil-guard until it's set.
func (s *Server) SetCron(c CronControl) { s.cron = c }

// mountCronSSR registers the Cron page + its form actions. Mounted
// unconditionally; handlers nil-guard on s.cron.
func (s *Server) mountCronSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /cron", s.handleCronPage)
	mux.HandleFunc("POST /cron/add", s.handleCronAddForm)
	mux.HandleFunc("POST /cron/{id}/enable", s.handleCronEnableForm)
	mux.HandleFunc("POST /cron/{id}/delete", s.handleCronDeleteForm)
	mux.HandleFunc("POST /cron/{id}/run", s.handleCronRunForm)
	mux.HandleFunc("POST /cron/exec", s.handleCronExecForm)
}

type cronPageData struct {
	layoutData
	Configured bool
	ExecOn     bool
	Jobs       []CronJobView
	Err        string
	Msg        string
}

func (s *Server) handleCronPage(w http.ResponseWriter, r *http.Request) {
	d := cronPageData{layoutData: layoutData{Title: "Cron", Page: "cron"}}
	d.Err = r.URL.Query().Get("err")
	d.Msg = r.URL.Query().Get("msg")
	if s.cron == nil {
		s.render(w, s.tmpl.cron, d)
		return
	}
	d.Configured = true
	d.Jobs = s.cron.Jobs()
	d.ExecOn = s.cron.ExecEnabled()
	s.render(w, s.tmpl.cron, d)
}

func (s *Server) handleCronAddForm(w http.ResponseWriter, r *http.Request) {
	if s.cron == nil {
		http.Redirect(w, r, "/cron", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	spec := strings.TrimSpace(r.FormValue("spec"))
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	kind := strings.TrimSpace(r.FormValue("kind"))
	tz := strings.TrimSpace(r.FormValue("tz"))
	exec := r.FormValue("exec") == "true" || r.FormValue("exec") == "on"
	if spec == "" || prompt == "" {
		redirectCron(w, r, "", "Schedule and prompt are both required.")
		return
	}
	if err := s.cron.Add(CronAddSpec{Name: name, Spec: spec, Prompt: prompt, Kind: kind, Exec: exec, TZ: tz}); err != nil {
		ActionFailed.Log(r.Context(), "action", "cron add", "err", err)
		redirectCron(w, r, "", "Couldn't schedule that job: "+err.Error())
		return
	}
	redirectCron(w, r, "Job scheduled.", "")
}

// handleCronExecForm flips the GLOBAL cron-exec kill-switch. Previously this was
// reachable only from the TUI (/cron exec on|off), which forced an operator onto
// the console for a setting that gates every unattended job.
func (s *Server) handleCronExecForm(w http.ResponseWriter, r *http.Request) {
	if s.cron == nil {
		http.Redirect(w, r, "/cron", http.StatusSeeOther)
		return
	}
	on := r.FormValue("enabled") == "true"
	if err := s.cron.SetExec(on); err != nil {
		redirectCron(w, r, "", "Couldn't change cron-exec: "+err.Error())
		return
	}
	if on {
		redirectCron(w, r, "cron-exec ENABLED - shell and exec-opted-in jobs may now use dangerous tools unattended.", "")
		return
	}
	redirectCron(w, r, "cron-exec disabled - dangerous jobs are inert again.", "")
}

func (s *Server) handleCronEnableForm(w http.ResponseWriter, r *http.Request) {
	if s.cron == nil {
		http.Redirect(w, r, "/cron", http.StatusSeeOther)
		return
	}
	id := r.PathValue("id")
	on := r.FormValue("enabled") == "true"
	if err := s.cron.SetEnabled(id, on); err != nil {
		ActionFailed.Log(r.Context(), "action", "cron set-enabled", "target", id, "err", err)
		redirectCron(w, r, "", "Couldn't update that job: "+err.Error())
		return
	}
	redirectCron(w, r, "", "")
}

func (s *Server) handleCronDeleteForm(w http.ResponseWriter, r *http.Request) {
	if s.cron == nil {
		http.Redirect(w, r, "/cron", http.StatusSeeOther)
		return
	}
	id := r.PathValue("id")
	if err := s.cron.Remove(id); err != nil {
		ActionFailed.Log(r.Context(), "action", "cron remove", "target", id, "err", err)
		redirectCron(w, r, "", "Couldn't remove that job: "+err.Error())
		return
	}
	redirectCron(w, r, "Job removed.", "")
}

func (s *Server) handleCronRunForm(w http.ResponseWriter, r *http.Request) {
	if s.cron == nil {
		http.Redirect(w, r, "/cron", http.StatusSeeOther)
		return
	}
	id := r.PathValue("id")
	if err := s.cron.RunNow(id); err != nil {
		ActionFailed.Log(r.Context(), "action", "cron run", "target", id, "err", err)
		redirectCron(w, r, "", "Couldn't run that job: "+err.Error())
		return
	}
	redirectCron(w, r, "Running now — the result will appear in the activity feed.", "")
}

// redirectCron 303-redirects back to /cron with an optional flash message/error.
func redirectCron(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	target := "/cron"
	switch {
	case errMsg != "":
		target += "?err=" + url.QueryEscape(errMsg)
	case msg != "":
		target += "?msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
