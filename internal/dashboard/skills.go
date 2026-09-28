package dashboard

// skills.go is the server-rendered Skills page (TEN-202): the web view of the
// T4 skill library. It mirrors the TUI's /skills + /skills auto for a
// non-technical operator — make the human-in-the-loop gate obvious (a proposed
// skill gets a one-click Accept), let them turn lessons on/off, and explain the
// auto-accept modes in plain words.
//
// Same shape as cron.go/eval.go: a SkillControl interface (satisfied in
// cmd/tenant by dashSkill), unconditional nil-guarded mounting, and form
// mutations answered by formDone: in place for a Datastar post, a 303
// otherwise. Skill text renders through html/template auto-escaping — a
// hostile induced skill name/description cannot inject markup.

import (
	"net/http"
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

// SetSkills installs the skill control after construction (mirrors SetCron).
func (s *Server) SetSkills(c SkillControl) { s.skills = c }

func (s *Server) mountSkillsSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /skills", s.handleSkillsPage)
	// Accepting skills without you, forgetting one and trusting one confirm
	// (TEN-211); off and "stop trusting" don't. Both "on" and "trusted"
	// accept without asking, so only "off" skips the step. An agent names
	// the skills it saves, so the name is a form field and every URL here is
	// fixed: a name can't aim a form at another route (F1).
	mux.HandleFunc("POST /skills/auto", needConfirm(confirmUnless("mode", "off"), s.handleSkillsAutoForm))
	mux.HandleFunc("POST /skills/accept", s.handleSkillAcceptForm)
	mux.HandleFunc("POST /skills/enable", s.handleSkillEnableForm)
	mux.HandleFunc("POST /skills/forget", needConfirm(nil, s.handleSkillForgetForm))
	mux.HandleFunc("POST /skills/trust", needConfirm(confirmUnless("trusted", "", "false"), s.handleSkillTrustForm))
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
	d := s.skillsData()
	d.Err = r.URL.Query().Get("err")
	d.Msg = r.URL.Query().Get("msg")
	s.render(w, s.tmpl.skills, d)
}

func (s *Server) skillsData() skillsPageData {
	d := skillsPageData{layoutData: layoutData{Title: "Skills", Page: "skills"}}
	if s.skills == nil {
		return d
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
	return d
}

// skillsDone is every Skills form's answer: in place for a Datastar post,
// with #skills-body re-rendered; a 303 with the flash otherwise.
func (s *Server) skillsDone(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	s.formDone(w, r, "/skills", msg, errMsg, func() (string, error) {
		return part(s.tmpl.skills, "skills-body", s.skillsData())
	})
}

// autoWords says what each auto-accept mode does, for its flash.
var autoWords = map[string]string{
	"off":     "You accept each new skill yourself.",
	"trusted": "New skills are accepted without you while your recent feedback says your agent is doing well.",
	"on":      "New skills are always accepted without you.",
}

func (s *Server) handleSkillsAutoForm(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		s.skillsDone(w, r, "", "")
		return
	}
	mode := strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	if autoWords[mode] == "" {
		s.skillsDone(w, r, "", "Pick Off, Trusted or On.")
		return
	}
	if err := s.skills.SetAutoAccept(mode); err != nil {
		s.skillsDone(w, r, "", s.flashFor(r.Context(), "skills auto-accept", err, "Couldn't change how new skills are accepted."))
		return
	}
	s.skillsDone(w, r, autoWords[mode], "")
}

func (s *Server) handleSkillAcceptForm(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		s.skillsDone(w, r, "", "")
		return
	}
	if _, err := s.skills.Accept(r.FormValue("name")); err != nil {
		s.skillsDone(w, r, "", s.flashFor(r.Context(), "skill accept", err, "Couldn't accept that skill."))
		return
	}
	s.skillsDone(w, r, "Skill accepted: it's live now.", "")
}

func (s *Server) handleSkillEnableForm(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		s.skillsDone(w, r, "", "")
		return
	}
	on := r.FormValue("enabled") == "true"
	if _, err := s.skills.SetEnabled(r.FormValue("name"), on); err != nil {
		s.skillsDone(w, r, "", s.flashFor(r.Context(), "skill enable", err, "Couldn't update that skill."))
		return
	}
	if on {
		s.skillsDone(w, r, "Skill turned on.", "")
		return
	}
	s.skillsDone(w, r, "Skill turned off: your agent won't use it until you turn it on.", "")
}

func (s *Server) handleSkillForgetForm(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		s.skillsDone(w, r, "", "")
		return
	}
	if _, err := s.skills.Forget(r.FormValue("name")); err != nil {
		s.skillsDone(w, r, "", s.flashFor(r.Context(), "skill forget", err, "Couldn't remove that skill."))
		return
	}
	s.skillsDone(w, r, "Skill removed.", "")
}

func (s *Server) handleSkillTrustForm(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		s.skillsDone(w, r, "", "")
		return
	}
	name := r.FormValue("name")
	trusted := r.FormValue("trusted") == "true"
	ok, err := s.skills.SetTrust(name, trusted)
	switch {
	case err != nil:
		s.skillsDone(w, r, "", s.flashFor(r.Context(), "skill trust", err, "Couldn't change whether your agent uses that skill."))
	case !ok:
		s.skillsDone(w, r, "", "There's no skill named "+name+".")
	case trusted:
		s.skillsDone(w, r, "Skill trusted: your agent can use it now.", "")
	default:
		s.skillsDone(w, r, "Skill untrusted: your agent won't use it until you trust it again.", "")
	}
}
