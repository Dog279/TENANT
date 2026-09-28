package dashboard

// models.go is the server-rendered Models page (TEN-204): the web view of the
// configured LLM backends. It mirrors the TUI's /model — see which brain is
// active, switch it, add a cloud model, tune the loop ceiling — for a
// non-technical operator ("Which AI brain is my agent using?").
//
// Same shape as cron.go/eval.go: a ModelControl interface (satisfied in
// cmd/tenant by dashModel), unconditional nil-guarded mounting, and form
// mutations answered by formDone: in place for a Datastar post, else a 303.
// Every method is synchronous (no browser) — unlike MCP connect.
// The add-cloud key travels INBOUND only (body-only POST), reusing the
// write-only secret discipline of keys.go: it's never read back or rendered.

import (
	"bytes"
	"net/http"
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
	// Switching changes every answer and removing loses the key: both go
	// through a confirm step (TEN-211, the reference adoption).
	mux.HandleFunc("POST /models/use", needConfirm(nil, s.handleModelUseForm))
	mux.HandleFunc("POST /models/add", s.handleModelAddForm)
	mux.HandleFunc("POST /models/remove", needConfirm(nil, s.handleModelRemoveForm))
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

func (s *Server) modelsData() modelsPageData {
	d := modelsPageData{layoutData: layoutData{Title: "Model", Page: "models"}}
	if s.models != nil {
		d.Configured = true
		d.Models = s.models.Models()
		d.Ceiling = s.models.LoopCeiling()
	}
	return d
}

func (s *Server) handleModelsPage(w http.ResponseWriter, r *http.Request) {
	d := s.modelsData()
	d.Err = r.URL.Query().Get("err")
	d.Msg = r.URL.Query().Get("msg")
	s.render(w, s.tmpl.models, d)
}

// modelsBody renders #models-body, the part a Models form changes.
func (s *Server) modelsBody() (string, error) {
	var buf bytes.Buffer
	if err := s.tmpl.models.ExecuteTemplate(&buf, "models-body", s.modelsData()); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// modelsDone is every Models form's answer (DESIGN.md "Forms that don't
// reload"): in place for a Datastar post, a 303 with the flash otherwise.
func (s *Server) modelsDone(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	s.formDone(w, r, "/models", msg, errMsg, s.modelsBody)
}

func (s *Server) handleModelUseForm(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		s.modelsDone(w, r, "", "")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	status, err := s.models.Use(name)
	if err != nil {
		s.modelsDone(w, r, "", s.flashFor(r.Context(), "model use", err, "Couldn't switch to that model."))
		return
	}
	// A "⚠" status means the switch happened but the model can't answer yet
	// (its warning is logged as SwapUnready): an alert, and the rows, which
	// changed.
	if strings.HasPrefix(status, "⚠") {
		s.formWarn(w, r, "/models", "Switched to "+name+", but it isn't ready to answer. The Logs page has the reason.", s.modelsBody)
		return
	}
	s.modelsDone(w, r, "Every answer now comes from "+name+".", "")
}

// handleModelAddForm adds a cloud model. Its key travels in the body only
// (F2), and a failure's reason, which can quote the key, goes to the log
// masked, never into the redirect URL (F3).
func (s *Server) handleModelAddForm(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		s.modelsDone(w, r, "", "")
		return
	}
	kind := strings.TrimSpace(r.FormValue("kind"))
	key, ok := bodyOnly(r, "key")
	if !ok {
		KeyInURL.Log(r.Context(), "cred", kind)
		s.modelsDone(w, r, "", secretInURL)
		return
	}
	key = strings.TrimSpace(key)
	if kind == "" || key == "" {
		s.modelsDone(w, r, "", "Pick a provider and paste its API key.")
		return
	}
	if _, err := s.models.AddCloud(kind, key); err != nil {
		KeyNotSaved.Log(r.Context(), "cred", kind, "op", "added", "err", err)
		s.modelsDone(w, r, "", "Couldn't add that model. The Logs page has the reason.")
		return
	}
	s.modelsDone(w, r, "Model added. Choose Use this model to switch your agent to it.", "")
}

func (s *Server) handleModelRemoveForm(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		s.modelsDone(w, r, "", "")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if _, err := s.models.Remove(name); err != nil {
		s.modelsDone(w, r, "", s.flashFor(r.Context(), "model remove", err, "Couldn't remove that model."))
		return
	}
	s.modelsDone(w, r, "Removed "+name+".", "")
}

func (s *Server) handleModelCeilingForm(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		s.modelsDone(w, r, "", "")
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("ceiling")))
	if err != nil || n <= 0 {
		s.modelsDone(w, r, "", "Enter a whole number greater than zero.")
		return
	}
	if _, serr := s.models.SetLoopCeiling(n); serr != nil {
		s.modelsDone(w, r, "", s.flashFor(r.Context(), "model step limit", serr, "Couldn't update the limit. Use a number from 1 to 200."))
		return
	}
	s.modelsDone(w, r, "Saved: your agent takes at most "+strconv.Itoa(n)+" steps per answer.", "")
}

func (s *Server) handleModelReloadForm(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		s.modelsDone(w, r, "", "")
		return
	}
	if _, err := s.models.ReloadKeys(); err != nil {
		s.modelsDone(w, r, "", s.flashFor(r.Context(), "model reload keys", err, "Couldn't reload the keys."))
		return
	}
	s.modelsDone(w, r, "Keys reloaded.", "")
}
