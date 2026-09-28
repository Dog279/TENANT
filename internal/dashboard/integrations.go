package dashboard

// integrations.go is the server-rendered Integrations page (TEN-206): connect
// the agent to outside tools (Google, Atlassian, Discord, web-search, …). It
// folds the TUI's /configure skill-config surface into the web, on the hybrid
// model: SAVE stores credentials synchronously (non-blocking, doesn't auto-
// probe), TEST runs the probe ASYNC (it may open a sign-in browser on the
// host), DISCONNECT clears the credential — all status reads work from
// anywhere. Secret fields travel INBOUND only (body-only POST), never read
// back, mirroring keys.go. Every form is answered by formDone: in place for a
// Datastar post, else a 303.

import (
	"bytes"
	"context"
	"net/http"
	"strings"
)

// IntegrationsControl is the runtime integration-config surface the dashboard
// drives. cmd/tenant adapts the skill-config control to it (see
// dashIntegrations). A nil control renders an "isn't configured" state.
type IntegrationsControl interface {
	Integrations() []IntegrationView
	Fields(id string) ([]IntegrationField, error)
	Configure(id string, values map[string]string) (string, error) // store creds, no auto-probe
	Probe(id string) (string, error)                               // test; MAY block on host OAuth
	Disconnect(id string) (string, error)                          // clear the stored credential
}

// IntegrationView is one integration's render-ready status.
type IntegrationView struct {
	ID         string
	Label      string
	Configured bool
	Enabled    bool
	SetupHint  string
}

// IntegrationField is one configurable field (Secret ⇒ password input;
// Options non-empty ⇒ a picker).
type IntegrationField struct {
	Key          string
	Prompt       string
	Secret       bool
	Required     bool
	Default      string
	Options      []string
	OptionLabels []string
}

// GitHubRepoRow is one watched repository as the Integrations page renders it.
type GitHubRepoRow struct {
	Slug    string // owner/name
	Write   bool   // the agent may modify it (writes are still gated per action)
	Primary bool   // the default target when a tool omits its repo argument
}

// GitHubReposControl drives the watched-repository list. It is separate from
// IntegrationsControl because a repo list is not a credential: the PAT is saved
// through the normal integration card, while read/write is a per-repo SAFETY
// choice the operator revisits often.
type GitHubReposControl interface {
	Repos() ([]GitHubRepoRow, error)
	SetWrite(repo string, write bool) error
	Add(repo string, write bool) error
	Remove(repo string) error
}

// SetGitHubRepos installs the watched-repo control (nil = section hidden).
func (s *Server) SetGitHubRepos(c GitHubReposControl) { s.githubRepos = c }

// SetIntegrations installs the control after construction (mirrors SetCron).
func (s *Server) SetIntegrations(c IntegrationsControl) { s.integrations = c }

func (s *Server) mountIntegrationsSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /integrations", s.handleIntegrationsPage)
	// The integration's id is a form field, not a path segment, so an
	// in-place post's URL is a literal (F1).
	mux.HandleFunc("POST /integrations/save", s.handleIntegrationSaveForm)
	mux.HandleFunc("POST /integrations/test", s.handleIntegrationTestForm)
	mux.HandleFunc("POST /integrations/disconnect", needConfirm(nil, s.handleIntegrationDisconnectForm))
	// The repo travels as a form value, not a path param: an owner/name slug
	// contains a slash and would not match a {repo} segment.
	// A new repository starts watch-only; adding one writable confirms, like
	// Allow changes does.
	mux.HandleFunc("POST /integrations/github/repos/add", needConfirm(confirmUnless("mode", "", "read"), s.handleGitHubRepoAddForm))
	// write lets the agent change a repository: it confirms; read doesn't.
	mux.HandleFunc("POST /integrations/github/repos/mode", needConfirm(confirmUnless("mode", "read"), s.handleGitHubRepoModeForm))
	mux.HandleFunc("POST /integrations/github/repos/remove", s.handleGitHubRepoRemoveForm)
}

// integrationCard pairs an integration with its field schema for the template.
type integrationCard struct {
	IntegrationView
	Fields []IntegrationField
}

type integrationsPageData struct {
	layoutData
	Configured bool
	Cards      []integrationCard
	// GitHubConfigured reports whether the watched-repo control exists at all;
	// GitHubRepos may legitimately be empty (nothing watched yet).
	GitHubConfigured bool
	GitHubRepos      []GitHubRepoRow
	Err              string
	Msg              string
}

func (s *Server) integrationsData(ctx context.Context) integrationsPageData {
	d := integrationsPageData{layoutData: layoutData{Title: "Integrations", Page: "integrations"}}
	// The watched-repo list is an INDEPENDENT control, so it's read whether or
	// not the integrations control is there: otherwise the whole GitHub
	// section vanished whenever the integrations control happened to be absent.
	if s.githubRepos != nil {
		d.GitHubConfigured = true
		if repos, err := s.githubRepos.Repos(); err == nil {
			d.GitHubRepos = repos
		} else {
			// Surface it rather than rendering an empty list, which would read as
			// "nothing watched" when the config is actually unreadable.
			d.Err = s.flashFor(ctx, "github repos list", err, "Couldn't read the watched repositories. The Logs page has the reason.")
		}
	}
	if s.integrations != nil {
		d.Configured = true
		for _, iv := range s.integrations.Integrations() {
			card := integrationCard{IntegrationView: iv}
			if f, err := s.integrations.Fields(iv.ID); err == nil {
				card.Fields = f
			}
			d.Cards = append(d.Cards, card)
		}
	}
	return d
}

func (s *Server) handleIntegrationsPage(w http.ResponseWriter, r *http.Request) {
	d := s.integrationsData(r.Context())
	if d.Err == "" { // an unreadable repo list outranks the flash
		d.Err = r.URL.Query().Get("err")
	}
	d.Msg = r.URL.Query().Get("msg")
	s.render(w, s.tmpl.integrations, d)
}

// integrationsDone is every Integrations form's answer (DESIGN.md "Forms that
// don't reload"): in place for a Datastar post, with #integrations-body
// re-rendered; a 303 with the flash otherwise.
func (s *Server) integrationsDone(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	s.formDone(w, r, "/integrations", msg, errMsg, func() (string, error) {
		var buf bytes.Buffer
		if err := s.tmpl.integrations.ExecuteTemplate(&buf, "integrations-body", s.integrationsData(r.Context())); err != nil {
			return "", err
		}
		return buf.String(), nil
	})
}

// modeWrite maps the dropdown value onto the write flag. Anything other than
// the explicit "write" is read-only: an unrecognised value must fail SAFE.
func modeWrite(v string) bool { return strings.TrimSpace(strings.ToLower(v)) == "write" }

func (s *Server) handleGitHubRepoAddForm(w http.ResponseWriter, r *http.Request) {
	if s.githubRepos == nil {
		s.integrationsDone(w, r, "", "")
		return
	}
	repo := strings.TrimSpace(r.FormValue("repo"))
	if repo == "" {
		s.integrationsDone(w, r, "", "Enter a repository as owner/name.")
		return
	}
	// A ":write" (or ":w", ":rw") suffix would make it writable past the
	// confirm step; an owner/name never contains a colon.
	if strings.Contains(repo, ":") {
		s.integrationsDone(w, r, "", "Enter the repository as owner/name. It starts watch-only; choose Allow changes on its row to let your agent change it.")
		return
	}
	write := modeWrite(r.FormValue("mode"))
	if err := s.githubRepos.Add(repo, write); err != nil {
		s.integrationsDone(w, r, "", s.flashFor(r.Context(), "github repo add", err, "Couldn't watch that repository. Check it's written as owner/name."))
		return
	}
	s.integrationsDone(w, r, "Watching "+repo+". "+restartToApply, "")
}

func (s *Server) handleGitHubRepoModeForm(w http.ResponseWriter, r *http.Request) {
	if s.githubRepos == nil {
		s.integrationsDone(w, r, "", "")
		return
	}
	repo := strings.TrimSpace(r.FormValue("repo"))
	write := modeWrite(r.FormValue("mode"))
	if err := s.githubRepos.SetWrite(repo, write); err != nil {
		s.integrationsDone(w, r, "", s.flashFor(r.Context(), "github repo mode", err, "Couldn't change access for "+repo+"."))
		return
	}
	if write {
		s.integrationsDone(w, r, "Your agent may now change "+repo+". It still asks you before each change.", "")
		return
	}
	s.integrationsDone(w, r, repo+" is watch-only now: still reported on, and changes to it are refused.", "")
}

func (s *Server) handleGitHubRepoRemoveForm(w http.ResponseWriter, r *http.Request) {
	if s.githubRepos == nil {
		s.integrationsDone(w, r, "", "")
		return
	}
	repo := strings.TrimSpace(r.FormValue("repo"))
	if err := s.githubRepos.Remove(repo); err != nil {
		s.integrationsDone(w, r, "", s.flashFor(r.Context(), "github repo remove", err, "Couldn't stop watching "+repo+". Is it on the list?"))
		return
	}
	s.integrationsDone(w, r, "Stopped watching "+repo+".", "")
}

// restartToApply says when a watched-repository change reaches a running
// Tenant.
const restartToApply = "The change applies when Tenant next starts."

// handleIntegrationSaveForm stores the submitted field values (synchronous —
// credential storage is fast and must not auto-probe, which could open a
// host browser and block the request). Every field travels in the body only
// (F2), and a failure's reason, which can quote a secret, goes to the log
// masked, never into the redirect URL (F3).
func (s *Server) handleIntegrationSaveForm(w http.ResponseWriter, r *http.Request) {
	if s.integrations == nil {
		s.integrationsDone(w, r, "", "")
		return
	}
	id := strings.TrimSpace(r.FormValue("integration"))
	fields, err := s.integrations.Fields(id)
	if err != nil {
		s.integrationsDone(w, r, "", "Unknown integration.")
		return
	}
	values := map[string]string{}
	for _, f := range fields {
		v, ok := bodyOnly(r, f.Key)
		if !ok {
			KeyInURL.Log(r.Context(), "cred", id+"."+f.Key)
			s.integrationsDone(w, r, "", secretInURL)
			return
		}
		if v = strings.TrimSpace(v); v != "" {
			values[f.Key] = v
		}
	}
	if len(values) == 0 {
		s.integrationsDone(w, r, "", "Nothing to save: fill in at least one field.")
		return
	}
	if _, cerr := s.integrations.Configure(id, values); cerr != nil {
		KeyNotSaved.Log(r.Context(), "cred", id, "op", "saved", "err", cerr)
		s.integrationsDone(w, r, "", "Couldn't save that. The Logs page has the reason.")
		return
	}
	s.integrationsDone(w, r, "Saved. Use Test connection to check it works.", "")
}

// handleIntegrationTestForm runs the probe ASYNC: some probes (Atlassian,
// Google) open a sign-in browser on the host and block for the OAuth flow,
// which must not wedge the HTTP request (hybrid model — sign in at the host).
// The probe outlives the request, so its log record keeps the request's
// values without its cancellation.
func (s *Server) handleIntegrationTestForm(w http.ResponseWriter, r *http.Request) {
	if s.integrations == nil {
		s.integrationsDone(w, r, "", "")
		return
	}
	id := strings.TrimSpace(r.FormValue("integration"))
	ctx := context.WithoutCancel(r.Context())
	go func() {
		if _, err := s.integrations.Probe(id); err != nil {
			ProbeFailed.Log(ctx, "integration", id, "err", err)
		}
	}()
	s.integrationsDone(w, r, "Testing "+id+". If it needs a sign-in, a window opens on the computer running Tenant. Refresh to see the result.", "")
}

func (s *Server) handleIntegrationDisconnectForm(w http.ResponseWriter, r *http.Request) {
	if s.integrations == nil {
		s.integrationsDone(w, r, "", "")
		return
	}
	id := strings.TrimSpace(r.FormValue("integration"))
	if _, err := s.integrations.Disconnect(id); err != nil {
		s.integrationsDone(w, r, "", s.flashFor(r.Context(), "integration disconnect", err, "Couldn't disconnect "+id+"."))
		return
	}
	s.integrationsDone(w, r, "Disconnected "+id+". Its saved sign-in is gone.", "")
}
