package dashboard

// models.go is the server-rendered Models page (TEN-204): the web view of the
// configured LLM backends. It mirrors the TUI's /model — see which brain is
// active, switch it, add a cloud model, tune the loop ceiling — for a
// non-technical operator ("Which AI brain is my agent using?").
//
// Same shape as cron.go/eval.go: a ModelControl interface (satisfied in
// cmd/tenant by dashModel), unconditional nil-guarded mounting, form/303
// mutations. Every method is synchronous (no browser) — unlike MCP connect.
// The add-cloud key travels INBOUND only (body-only POST), reusing the
// write-only secret discipline of keys.go: it's never read back or rendered.

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ModelControl is the runtime model surface the dashboard drives. cmd/tenant
// adapts modelControl to it (see dashModel). A nil control renders an "isn't
// configured" state. Models() is read; the rest mutate and apply live.
type ModelControl interface {
	Models() []ModelView
	Use(name string) (string, error) // switch the active backend
	AddCloud(kind, apiKey string) (string, error)
	Remove(name string) (string, error)
	LoopCeiling() int
	SetLoopCeiling(n int) (string, error)
	ReloadKeys() (string, error)
}

// ModelView is one backend's render-ready state.
type ModelView struct {
	Name     string
	Kind     string
	Model    string
	Active   bool
	Degraded bool
}

// SetModels installs the model control after construction (mirrors SetCron).
func (s *Server) SetModels(c ModelControl) { s.models = c }

func (s *Server) mountModelsSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /models", s.handleModelsPage)
	mux.HandleFunc("POST /models/use", s.handleModelUseForm)
	mux.HandleFunc("POST /models/add", s.handleModelAddForm)
	mux.HandleFunc("POST /models/remove", s.handleModelRemoveForm)
	mux.HandleFunc("POST /models/ceiling", s.handleModelCeilingForm)
	mux.HandleFunc("POST /models/reload", s.handleModelReloadForm)
}

type modelsPageData struct {
	layoutData
	Configured bool
	Models     []ModelView
	Ceiling    int
	Err        string
	Msg        string
}

func (s *Server) handleModelsPage(w http.ResponseWriter, r *http.Request) {
	d := modelsPageData{layoutData: layoutData{Title: "Model", Page: "models"}}
	d.Err = r.URL.Query().Get("err")
	d.Msg = r.URL.Query().Get("msg")
	if s.models == nil {
		s.render(w, s.tmpl.models, d)
		return
	}
	d.Configured = true
	d.Models = s.models.Models()
	d.Ceiling = s.models.LoopCeiling()
	s.render(w, s.tmpl.models, d)
}

func (s *Server) handleModelUseForm(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		http.Redirect(w, r, "/models", http.StatusSeeOther)
		return
	}
	// Confirm guard: switching the active model changes every answer, so the
	// form must carry confirm=yes (the template's two-step). Without it we
	// bounce back rather than silently swapping.
	if r.FormValue("confirm") != "yes" {
		redirectModels(w, r, "", "Switch not confirmed.")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	status, err := s.models.Use(name)
	if err != nil {
		redirectModels(w, r, "", "Couldn't switch model: "+err.Error())
		return
	}
	redirectModels(w, r, status, "")
}

func (s *Server) handleModelAddForm(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		http.Redirect(w, r, "/models", http.StatusSeeOther)
		return
	}
	kind := strings.TrimSpace(r.FormValue("kind"))
	key := strings.TrimSpace(r.FormValue("key"))
	if kind == "" || key == "" {
		redirectModels(w, r, "", "Pick a provider and paste its API key.")
		return
	}
	status, err := s.models.AddCloud(kind, key)
	if err != nil {
		redirectModels(w, r, "", "Couldn't add that model: "+err.Error())
		return
	}
	redirectModels(w, r, status, "")
}

func (s *Server) handleModelRemoveForm(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		http.Redirect(w, r, "/models", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	status, err := s.models.Remove(name)
	if err != nil {
		redirectModels(w, r, "", "Couldn't remove that model: "+err.Error())
		return
	}
	redirectModels(w, r, status, "")
}

func (s *Server) handleModelCeilingForm(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		http.Redirect(w, r, "/models", http.StatusSeeOther)
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("ceiling")))
	if err != nil || n <= 0 {
		redirectModels(w, r, "", "Enter a whole number greater than zero.")
		return
	}
	status, serr := s.models.SetLoopCeiling(n)
	if serr != nil {
		redirectModels(w, r, "", "Couldn't update the limit: "+serr.Error())
		return
	}
	redirectModels(w, r, status, "")
}

func (s *Server) handleModelReloadForm(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		http.Redirect(w, r, "/models", http.StatusSeeOther)
		return
	}
	status, err := s.models.ReloadKeys()
	if err != nil {
		redirectModels(w, r, "", "Couldn't reload keys: "+err.Error())
		return
	}
	redirectModels(w, r, status, "")
}

func redirectModels(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	target := "/models"
	switch {
	case errMsg != "":
		target += "?err=" + url.QueryEscape(errMsg)
	case msg != "":
		target += "?msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
