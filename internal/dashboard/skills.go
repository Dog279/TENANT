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
	// SetTrust trusts or distrusts a skill (TEN-298): only a trusted skill is
	// offered to the agent.
	SetTrust(name string, trusted bool) (bool, error)
}

// SkillView is one skill's render-ready state. Status is
// "live" | "proposed" | "tombstoned".
type SkillView struct {
	Name        string
	Description string
	Status      string
	Enabled     bool
	// Untrusted skills wait for the operator's review: what the agent saved
	// itself, and imported packages (TEN-298). The zero value is trusted.
	Untrusted     bool
	Origin        string   // operator, agent, induction, seed, import
	Source        string   // an imported skill's package
	RequiresTools []string // tools it expects; offered only where they are enabled
	Recipe        string   // the instructions, shown for review
}

// Action is the POST target for verb on this skill. An agent names the skills
// it saves, so the name is path-escaped: a "?" or "/" in it can't aim the
// form, and the confirm=yes its confirm step sends, at another route (F1).
func (v SkillView) Action(verb string) string {
	return "/skills/" + url.PathEscape(v.Name) + "/" + verb
}

// SetSkills installs the skill control after construction (mirrors SetCron).
func (s *Server) SetSkills(c SkillControl) { s.skills = c }

func (s *Server) mountSkillsSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /skills", s.handleSkillsPage)
	// Accepting skills without you, forgetting one and trusting one confirm
	// (TEN-211); off and "stop trusting" don't. Both "on" and "trusted"
	// accept without asking, so only "off" skips the step.
	mux.HandleFunc("POST /skills/auto", needConfirm(confirmUnless("mode", "off"), s.handleSkillsAutoForm))
	mux.HandleFunc("POST /skills/{name}/accept", s.handleSkillAcceptForm)
	mux.HandleFunc("POST /skills/{name}/enable", s.handleSkillEnableForm)
	mux.HandleFunc("POST /skills/{name}/forget", needConfirm(nil, s.handleSkillForgetForm))
	mux.HandleFunc("POST /skills/{name}/trust", needConfirm(confirmUnless("trusted", "", "false"), s.handleSkillTrustForm))
}

type skillsPageData struct {
	layoutData
	Configured bool
	Proposed   []SkillView
	Untrusted  []SkillView // live but untrusted: waiting for review (TEN-298)
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
			if sk.Untrusted {
				d.Untrusted = append(d.Untrusted, sk)
				continue
			}
			d.Live = append(d.Live, sk)
		}
	}
	s.render(w, s.tmpl.skills, d)
}

// autoWords says what each auto-accept mode does, for its flash.
var autoWords = map[string]string{
	"off":     "You accept each new skill yourself.",
	"trusted": "New skills are accepted without you while your recent feedback says your agent is doing well.",
	"on":      "New skills are always accepted without you.",
}

func (s *Server) handleSkillsAutoForm(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		http.Redirect(w, r, "/skills", http.StatusSeeOther)
		return
	}
	mode := strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	if autoWords[mode] == "" {
		redirectFlash(w, r, "/skills", "", "Pick Off, Trusted or On.")
		return
	}
	if err := s.skills.SetAutoAccept(mode); err != nil {
		redirectFlash(w, r, "/skills", "", s.flashFor(r.Context(), "skills auto-accept", err, "Couldn't change how new skills are accepted."))
		return
	}
	redirectFlash(w, r, "/skills", autoWords[mode], "")
}

func (s *Server) handleSkillAcceptForm(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		http.Redirect(w, r, "/skills", http.StatusSeeOther)
		return
	}
	name := r.PathValue("name")
	if _, err := s.skills.Accept(name); err != nil {
		redirectFlash(w, r, "/skills", "", s.flashFor(r.Context(), "skill accept", err, "Couldn't accept that skill."))
		return
	}
	redirectFlash(w, r, "/skills", "Skill accepted: it's live now.", "")
}

func (s *Server) handleSkillEnableForm(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		http.Redirect(w, r, "/skills", http.StatusSeeOther)
		return
	}
	name := r.PathValue("name")
	on := r.FormValue("enabled") == "true"
	if _, err := s.skills.SetEnabled(name, on); err != nil {
		redirectFlash(w, r, "/skills", "", s.flashFor(r.Context(), "skill enable", err, "Couldn't update that skill."))
		return
	}
	redirectFlash(w, r, "/skills", "", "")
}

func (s *Server) handleSkillForgetForm(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		http.Redirect(w, r, "/skills", http.StatusSeeOther)
		return
	}
	name := r.PathValue("name")
	if _, err := s.skills.Forget(name); err != nil {
		redirectFlash(w, r, "/skills", "", s.flashFor(r.Context(), "skill forget", err, "Couldn't remove that skill."))
		return
	}
	redirectFlash(w, r, "/skills", "Skill removed.", "")
}

func (s *Server) handleSkillTrustForm(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		http.Redirect(w, r, "/skills", http.StatusSeeOther)
		return
	}
	name := r.PathValue("name")
	trusted := r.FormValue("trusted") == "true"
	ok, err := s.skills.SetTrust(name, trusted)
	switch {
	case err != nil:
		redirectFlash(w, r, "/skills", "", s.flashFor(r.Context(), "skill trust", err, "Couldn't change whether your agent uses that skill."))
	case !ok:
		redirectFlash(w, r, "/skills", "", "There's no skill named "+name+".")
	case trusted:
		redirectFlash(w, r, "/skills", "Skill trusted: your agent can use it now.", "")
	default:
		redirectFlash(w, r, "/skills", "Skill untrusted: your agent won't use it until you trust it again.", "")
	}
}
