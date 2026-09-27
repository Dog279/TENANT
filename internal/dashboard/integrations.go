package dashboard

// integrations.go is the server-rendered Integrations page (TEN-206): connect
// the agent to outside tools (Google, Atlassian, Discord, web-search, …). It
// folds the TUI's /configure skill-config surface into the web, on the hybrid
// model: SAVE stores credentials synchronously (non-blocking, doesn't auto-
// probe), TEST runs the probe ASYNC (it may open a sign-in browser on the
// host), DISCONNECT clears the credential — all status reads work from
// anywhere. Secret fields travel INBOUND only (body-only POST), never read
// back, mirroring keys.go.

import (
	"net/http"
	"net/url"
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
	mux.HandleFunc("POST /integrations/{id}/save", s.handleIntegrationSaveForm)
	mux.HandleFunc("POST /integrations/{id}/test", s.handleIntegrationTestForm)
	mux.HandleFunc("POST /integrations/{id}/disconnect", s.handleIntegrationDisconnectForm)
	// The repo travels as a form value, not a path param: an owner/name slug
	// contains a slash and would not match a {repo} segment.
	mux.HandleFunc("POST /integrations/github/repos/add", s.handleGitHubRepoAddForm)
	mux.HandleFunc("POST /integrations/github/repos/mode", s.handleGitHubRepoModeForm)
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

func (s *Server) handleIntegrationsPage(w http.ResponseWriter, r *http.Request) {
	d := integrationsPageData{layoutData: layoutData{Title: "Integrations", Page: "integrations"}}
	d.Err = r.URL.Query().Get("err")
	d.Msg = r.URL.Query().Get("msg")
	// The watched-repo list is an INDEPENDENT control, so populate it BEFORE the
	// early return below. Doing it after meant the whole GitHub section vanished
	// whenever the integrations control happened to be absent.
	if s.githubRepos != nil {
		d.GitHubConfigured = true
		if repos, err := s.githubRepos.Repos(); err == nil {
			d.GitHubRepos = repos
		} else {
			// Surface it rather than rendering an empty list, which would read as
			// "nothing watched" when the config is actually unreadable.
			d.Err = "Couldn't read the watched repositories: " + err.Error()
		}
	}
	if s.integrations == nil {
		s.render(w, s.tmpl.integrations, d)
		return
	}
	d.Configured = true
	for _, iv := range s.integrations.Integrations() {
		card := integrationCard{IntegrationView: iv}
		if f, err := s.integrations.Fields(iv.ID); err == nil {
			card.Fields = f
		}
		d.Cards = append(d.Cards, card)
	}
	s.render(w, s.tmpl.integrations, d)
}

// modeWrite maps the dropdown value onto the write flag. Anything other than
// the explicit "write" is read-only: an unrecognised value must fail SAFE.
func modeWrite(v string) bool { return strings.TrimSpace(strings.ToLower(v)) == "write" }

func (s *Server) handleGitHubRepoAddForm(w http.ResponseWriter, r *http.Request) {
	if s.githubRepos == nil {
		http.Redirect(w, r, "/integrations", http.StatusSeeOther)
		return
	}
	repo := strings.TrimSpace(r.FormValue("repo"))
	if repo == "" {
		redirectIntegrations(w, r, "", "Enter a repository as owner/name.")
		return
	}
	write := modeWrite(r.FormValue("mode"))
	if err := s.githubRepos.Add(repo, write); err != nil {
		redirectIntegrations(w, r, "", "Couldn't watch that repository: "+err.Error())
		return
	}
	mode := "watch-only"
	if write {
		mode = "writable"
	}
	redirectIntegrations(w, r, "Watching "+repo+" ("+mode+"). Run /enable github to apply it to the running session.", "")
}

func (s *Server) handleGitHubRepoModeForm(w http.ResponseWriter, r *http.Request) {
	if s.githubRepos == nil {
		http.Redirect(w, r, "/integrations", http.StatusSeeOther)
		return
	}
	repo := strings.TrimSpace(r.FormValue("repo"))
	write := modeWrite(r.FormValue("mode"))
	if err := s.githubRepos.SetWrite(repo, write); err != nil {
		redirectIntegrations(w, r, "", "Couldn't change access for "+repo+": "+err.Error())
		return
	}
	if write {
		redirectIntegrations(w, r, repo+" is now WRITABLE - the agent may modify it, and each write is still gated.", "")
		return
	}
	redirectIntegrations(w, r, repo+" is now watch-only - still reported on, writes refused in code.", "")
}

func (s *Server) handleGitHubRepoRemoveForm(w http.ResponseWriter, r *http.Request) {
	if s.githubRepos == nil {
		http.Redirect(w, r, "/integrations", http.StatusSeeOther)
		return
	}
	repo := strings.TrimSpace(r.FormValue("repo"))
	if err := s.githubRepos.Remove(repo); err != nil {
		redirectIntegrations(w, r, "", "Couldn't stop watching "+repo+": "+err.Error())
		return
	}
	redirectIntegrations(w, r, "Stopped watching "+repo+".", "")
}

// handleIntegrationSaveForm stores the submitted field values (synchronous —
// credential storage is fast and must not auto-probe, which could open a
// host browser and block the request).
func (s *Server) handleIntegrationSaveForm(w http.ResponseWriter, r *http.Request) {
	if s.integrations == nil {
		http.Redirect(w, r, "/integrations", http.StatusSeeOther)
		return
	}
	id := r.PathValue("id")
	fields, err := s.integrations.Fields(id)
	if err != nil {
		redirectIntegrations(w, r, "", "Unknown integration.")
		return
	}
	values := map[string]string{}
	for _, f := range fields {
		if v := strings.TrimSpace(r.FormValue(f.Key)); v != "" {
			values[f.Key] = v
		}
	}
	if len(values) == 0 {
		redirectIntegrations(w, r, "", "Nothing to save — fill in at least one field.")
		return
	}
	status, cerr := s.integrations.Configure(id, values)
	if cerr != nil {
		redirectIntegrations(w, r, "", "Couldn't save "+id+": "+cerr.Error())
		return
	}
	redirectIntegrations(w, r, status+" — use Test to verify the connection.", "")
}

// handleIntegrationTestForm runs the probe ASYNC: some probes (Atlassian,
// Google) open a sign-in browser on the host and block for the OAuth flow,
// which must not wedge the HTTP request (hybrid model — sign in at the host).
func (s *Server) handleIntegrationTestForm(w http.ResponseWriter, r *http.Request) {
	if s.integrations == nil {
		http.Redirect(w, r, "/integrations", http.StatusSeeOther)
		return
	}
	id := r.PathValue("id")
	go func() {
		if _, err := s.integrations.Probe(id); err != nil {
			ProbeFailed.Log(r.Context(), "integration", id, "err", err)
		}
	}()
	redirectIntegrations(w, r, "Testing "+id+" — if it needs sign-in, a window opens on the computer running Tenant. Refresh to see the result.", "")
}

func (s *Server) handleIntegrationDisconnectForm(w http.ResponseWriter, r *http.Request) {
	if s.integrations == nil {
		http.Redirect(w, r, "/integrations", http.StatusSeeOther)
		return
	}
	id := r.PathValue("id")
	status, err := s.integrations.Disconnect(id)
	if err != nil {
		redirectIntegrations(w, r, "", "Couldn't disconnect "+id+": "+err.Error())
		return
	}
	redirectIntegrations(w, r, status, "")
}

func redirectIntegrations(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	target := "/integrations"
	switch {
	case errMsg != "":
		target += "?err=" + url.QueryEscape(errMsg)
	case msg != "":
		target += "?msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
