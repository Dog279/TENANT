package dashboard

// skills.go is the server-rendered Skills page (TEN-202): the web view of the
// T4 skill library. It mirrors the TUI's /skills + /skills auto for a
// non-technical operator — make the human-in-the-loop gate obvious (a proposed
// skill gets a one-click Accept), let them turn lessons on/off, and explain the
// auto-accept modes in plain words.
//
// Same shape as cron.go/eval.go: a SkillControl interface (satisfied in
// cmd/tenant by dashSkill), unconditional nil-guarded mounting, form/303
// mutations. Skill text renders through html/template auto-escaping — a hostile
// induced skill name/description cannot inject markup.

import (
	"net/http"
	"net/url"
	"strings"
)

// SkillControl is the runtime skill surface the dashboard drives. A nil control
// renders an "isn't configured" state. Skills() returns structured views;
// mutations return (changed, error) or error.
type SkillControl interface {
	Skills() []SkillView
	Accept(name string) (bool, error)
	SetEnabled(name string, on bool) (bool, error)
	Forget(name string) (bool, error)
	AutoAcceptMode() string
	SetAutoAccept(mode string) error
}

// SkillView is one skill's render-ready state. Status is
// "live" | "proposed" | "tombstoned".
type SkillView struct {
	Name        string
	Description string
	Status      string
	Enabled     bool
}

// SetSkills installs the skill control after construction (mirrors SetCron).
func (s *Server) SetSkills(c SkillControl) { s.skills = c }

func (s *Server) mountSkillsSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /skills", s.handleSkillsPage)
	mux.HandleFunc("POST /skills/auto", s.handleSkillsAutoForm)
	mux.HandleFunc("POST /skills/{name}/accept", s.handleSkillAcceptForm)
	mux.HandleFunc("POST /skills/{name}/enable", s.handleSkillEnableForm)
	mux.HandleFunc("POST /skills/{name}/forget", s.handleSkillForgetForm)
}

type skillsPageData struct {
	layoutData
	Configured bool
	Proposed   []SkillView
	Live       []SkillView
	AutoMode   string
	Err        string
	Msg        string
}

func (s *Server) handleSkillsPage(w http.ResponseWriter, r *http.Request) {
	d := skillsPageData{layoutData: layoutData{Title: "Skills", Page: "skills"}}
	d.Err = r.URL.Query().Get("err")
	d.Msg = r.URL.Query().Get("msg")
	if s.skills == nil {
		s.render(w, s.tmpl.skills, d)
		return
	}
	d.Configured = true
	d.AutoMode = s.skills.AutoAcceptMode()
	if d.AutoMode == "" {
		d.AutoMode = "off"
	}
	for _, sk := range s.skills.Skills() {
		switch sk.Status {
		case "proposed":
			d.Proposed = append(d.Proposed, sk)
		case "tombstoned":
			// hidden from the page; forgetting is terminal
		default:
			d.Live = append(d.Live, sk)
		}
	}
	s.render(w, s.tmpl.skills, d)
}

func (s *Server) handleSkillsAutoForm(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		http.Redirect(w, r, "/skills", http.StatusSeeOther)
		return
	}
	mode := strings.TrimSpace(r.FormValue("mode"))
	if err := s.skills.SetAutoAccept(mode); err != nil {
		redirectSkills(w, r, "", "Couldn't change auto-accept: "+err.Error())
		return
	}
	redirectSkills(w, r, "Auto-accept set to "+mode+".", "")
}

func (s *Server) handleSkillAcceptForm(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		http.Redirect(w, r, "/skills", http.StatusSeeOther)
		return
	}
	name := r.PathValue("name")
	if _, err := s.skills.Accept(name); err != nil {
		redirectSkills(w, r, "", "Couldn't accept that skill: "+err.Error())
		return
	}
	redirectSkills(w, r, "Skill accepted — it's live now.", "")
}

func (s *Server) handleSkillEnableForm(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		http.Redirect(w, r, "/skills", http.StatusSeeOther)
		return
	}
	name := r.PathValue("name")
	on := r.FormValue("enabled") == "true"
	if _, err := s.skills.SetEnabled(name, on); err != nil {
		redirectSkills(w, r, "", "Couldn't update that skill: "+err.Error())
		return
	}
	redirectSkills(w, r, "", "")
}

func (s *Server) handleSkillForgetForm(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		http.Redirect(w, r, "/skills", http.StatusSeeOther)
		return
	}
	name := r.PathValue("name")
	if _, err := s.skills.Forget(name); err != nil {
		redirectSkills(w, r, "", "Couldn't remove that skill: "+err.Error())
		return
	}
	redirectSkills(w, r, "Skill removed.", "")
}

func redirectSkills(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	target := "/skills"
	switch {
	case errMsg != "":
		target += "?err=" + url.QueryEscape(errMsg)
	case msg != "":
		target += "?msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
