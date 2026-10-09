package dashboard

// models.go is the server-rendered Models page (TEN-204): the web view of the
// configured LLM backends. It mirrors the TUI's /model — see which brain is
// active, switch it, add a cloud model, tune the loop ceiling — for a
// non-technical operator ("Which AI brain is my agent using?").
//
// Same shape as cron.go/eval.go: a ModelControl interface (satisfied in
// cmd/tenant by dashModel), unconditional nil-guarded mounting, and form
// mutations answered by formDone: in place for a Datastar post, else a 303.
// Every method is synchronous (no browser) — unlike MCP connect. Adding a
// server waits for its check, a few seconds (at most about 22).
// The add-cloud and add-server keys travel INBOUND only (body-only POST),
// reusing the write-only secret discipline of keys.go: never read back or
// rendered.

import (
	"context"
	"errors"
	"fmt"
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
	// AddServer checks a server on the network and adds it as a model
	// (its answer says what the check found). A failure the operator can fix
	// is a PlainError.
	AddServer(ctx context.Context, req ServerRequest) (ServerAdded, error)
	Remove(name string) (string, error)
	LoopCeiling() int
	SetLoopCeiling(n int) (string, error)
	// RepeatGuard / SetRepeatGuard: identical rounds (same calls, same
	// results) before a forced answer; <= 0 = off (TEN-506).
	RepeatGuard() int
	SetRepeatGuard(n int) (string, error)
	ReloadKeys() (string, error)
}

// ModelView is one backend's render-ready state.
type ModelView struct {
	Name     string
	Kind     string
	Model    string
	Active   bool
	Degraded bool
	// Address is a self-hosted server's address ("" for a hosted one), and
	// AutoModel says its model is found from the server at each start.
	Address   string
	AutoModel bool
}

// ServerRequest is the "Add a model from a server on your network" form.
// Only Address is required.
type ServerRequest struct {
	Address string // http(s)://host:port, with or without /v1
	Model   string // blank: the one the server offers
	Name    string // blank: named after the model
	APIKey  string // blank: the server takes none
}

// ServerAdded is what the check found and what was saved, in words.
type ServerAdded struct {
	Name   string
	Server string // what kind of server: "a llama.cpp server"
	Model  string
	Auto   bool   // the model was found: Tenant follows what the server serves
	Tools  string // native | text | none | unchecked: how the test message's tool call came back
	Format string // how tool calls are read, in words ("Qwen"): for text and unchecked
}

// answer is the add's toast, and whether it needs the operator's eye: a
// model whose tool calls weren't confirmed may not be able to use tools.
func (a ServerAdded) answer() (msg string, warn bool) {
	var b strings.Builder
	fmt.Fprintf(&b, "Added %s: %s running %s", a.Name, a.Server, a.Model)
	if a.Auto {
		b.WriteString(", found automatically")
	}
	b.WriteString(". ")
	switch a.Tools {
	case "native":
		b.WriteString("It returns tool calls the standard way. ")
	case "text":
		fmt.Fprintf(&b, "It writes tool calls as text, in the %s format, which your agent reads. ", a.Format)
	case "none":
		b.WriteString("But it answered a test message without using the tool it was offered, so it may not be able to use your agent's tools. Check that tool calling is on in the server's settings. ")
		warn = true
	default:
		fmt.Fprintf(&b, "Its test message didn't finish in time, so its tool calls weren't checked. Your agent will read them in the %s format. ", a.Format)
		warn = true
	}
	b.WriteString("Choose Use this model to switch your agent to it.")
	return b.String(), warn
}

// SetModels installs the model control after construction (mirrors SetCron).
func (s *Server) SetModels(c ModelControl) { s.models = c }

func (s *Server) mountModelsSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /models", s.handleModelsPage)
	// Switching changes every answer and removing loses the key: both go
	// through a confirm step (TEN-211, the reference adoption).
	mux.HandleFunc("POST /models/use", needConfirm(nil, s.handleModelUseForm))
	mux.HandleFunc("POST /models/add", s.handleModelAddForm)
	mux.HandleFunc("POST /models/add-server", s.handleModelServerForm)
	mux.HandleFunc("POST /models/remove", needConfirm(nil, s.handleModelRemoveForm))
	mux.HandleFunc("POST /models/ceiling", s.handleModelCeilingForm)
	mux.HandleFunc("POST /models/repeat", s.handleModelRepeatForm)
	mux.HandleFunc("POST /models/reload", s.handleModelReloadForm)
}

type modelsPageData struct {
	layoutData
	Configured bool
	Models     []ModelView
	Ceiling    int
	Repeat     int // repeat guard; 0 = off
	Err        string
	Msg        string
}

func (s *Server) modelsData() modelsPageData {
	d := modelsPageData{layoutData: layoutData{Title: "Model", Page: "models"}}
	if s.models != nil {
		d.Configured = true
		d.Models = s.models.Models()
		d.Ceiling = s.models.LoopCeiling()
		d.Repeat = max(s.models.RepeatGuard(), 0)
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
	return part(s.tmpl.models, "models-body", s.modelsData())
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

// couldntAddServer is the add-server failure the page shows when the
// control's error isn't one it can phrase.
const couldntAddServer = "Couldn't add that server. The Logs page has the reason."

// handleModelServerForm adds a model from a server on the network. The
// control checks the server first, so the answer waits for it. An optional
// key travels in the body only (F2); an error that isn't a PlainError can
// quote it, so with a key it goes to the log masked (F3). A server that
// was added comes back with the form emptied (createDone), and as an alert
// when its tool calls weren't confirmed.
func (s *Server) handleModelServerForm(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		s.modelsDone(w, r, "", "")
		return
	}
	key, ok := bodyOnly(r, "key")
	if !ok {
		KeyInURL.Log(r.Context(), "cred", "model server")
		s.modelsDone(w, r, "", secretInURL)
		return
	}
	req := ServerRequest{
		Address: strings.TrimSpace(r.FormValue("address")),
		Model:   strings.TrimSpace(r.FormValue("model")),
		Name:    strings.TrimSpace(r.FormValue("name")),
		APIKey:  strings.TrimSpace(key),
	}
	if req.Address == "" {
		s.modelsDone(w, r, "", "Enter the server's address, like http://my-server:8080.")
		return
	}
	added, err := s.models.AddServer(r.Context(), req)
	if err != nil {
		var pe PlainError
		if req.APIKey != "" && !errors.As(err, &pe) {
			KeyNotSaved.Log(r.Context(), "cred", "model server", "op", "added", "err", err)
			s.modelsDone(w, r, "", couldntAddServer)
			return
		}
		s.modelsDone(w, r, "", s.flashFor(r.Context(), "model server add", err, couldntAddServer))
		return
	}
	msg, warn := added.answer()
	if warn {
		s.createWarn(w, r, "/models", msg, s.modelsBody)
		return
	}
	s.createDone(w, r, "/models", msg, s.modelsBody)
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

// handleModelRepeatForm sets the repeat guard live (TEN-506); 0 turns it off.
func (s *Server) handleModelRepeatForm(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		s.modelsDone(w, r, "", "")
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("repeat")))
	if err != nil || n < 0 || n == 1 || n > 200 {
		s.modelsDone(w, r, "", "Enter 0 (off) or a whole number from 2 to 200.")
		return
	}
	msg := "Saved: the agent stops after " + strconv.Itoa(n) + " identical steps in a row."
	if n == 0 {
		n, msg = -1, "Saved: repeated steps never stop the agent; only the step limit does."
	}
	if _, serr := s.models.SetRepeatGuard(n); serr != nil {
		s.modelsDone(w, r, "", s.flashFor(r.Context(), "model repeat guard", serr, "Couldn't update the repeat guard."))
		return
	}
	s.modelsDone(w, r, msg, "")
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
