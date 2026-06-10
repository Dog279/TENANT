package dashboard

// keys.go is the server-rendered, WRITE-ONLY API-key settings page (TEN-145):
// add / remove the keys Tenant uses (LLM providers, web-search backends,
// integrations). The security core is that a stored secret has NO read path —
// ServiceKeyView carries booleans only (no value, no last-4, no mask), and a
// secret travels INBOUND only, in the POST body. Mirrors the cron SSR section:
// a SecretsControl interface (satisfied in cmd/tenant), unconditional route
// mounting with nil-guarded handlers, form/303 mutations, html/template
// auto-escaping. Every route rides the existing secure() (auth + same-origin +
// fail-closed bind); mutations are POST only.

import (
	"net/http"
	"net/url"
	"strings"
)

// SecretsControl is the runtime surface the keys page drives. Presence-only on
// read; secrets travel one-way inbound through SetSecret. A nil control renders
// an "isn't configured" notice. cmd/tenant adapts the credentials store to it.
type SecretsControl interface {
	List() []ServiceKeyView // catalog ⨝ store; booleans only, NEVER a value
	SetSecret(credID, value string) error
	RemoveSecret(credID string) error
}

// ServiceKeyView is one service's render-ready state. It deliberately carries NO
// secret value and NO masked hint — write-only means the read path cannot
// express the secret.
type ServiceKeyView struct {
	CredID      string
	Name        string
	Category    string
	Set         bool // a key is stored for this service (presence only)
	EnvDetected bool // an env var for this key is present (informational)
	EnvVar      string
	Required    bool
}

// ServiceKeyGroup lets the template iterate by category with no logic in HTML.
type ServiceKeyGroup struct {
	Category string
	Rows     []ServiceKeyView
}

// SetSecrets installs the control after construction (mirrors SetCron). Safe to
// call before the server starts serving; handlers nil-guard until it's set.
func (s *Server) SetSecrets(c SecretsControl) { s.secrets = c }

func (s *Server) mountSecretsSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings/keys", s.handleKeysPage)
	mux.HandleFunc("POST /settings/keys/{id}/set", s.handleKeysSetForm)
	mux.HandleFunc("POST /settings/keys/{id}/remove", s.handleKeysRemoveForm)
}

type keysPageData struct {
	layoutData
	Configured bool
	Groups     []ServiceKeyGroup
	Msg        string
	Err        string
}

// groupByCategory preserves first-seen category order from the (catalog-ordered)
// view list.
func groupByCategory(views []ServiceKeyView) []ServiceKeyGroup {
	idx := map[string]int{}
	var groups []ServiceKeyGroup
	for _, v := range views {
		i, ok := idx[v.Category]
		if !ok {
			idx[v.Category] = len(groups)
			groups = append(groups, ServiceKeyGroup{Category: v.Category})
			i = len(groups) - 1
		}
		groups[i].Rows = append(groups[i].Rows, v)
	}
	return groups
}

func (s *Server) handleKeysPage(w http.ResponseWriter, r *http.Request) {
	d := keysPageData{layoutData: layoutData{Title: "Keys", Page: "keys"}}
	d.Msg = r.URL.Query().Get("msg")
	d.Err = r.URL.Query().Get("err")
	if s.secrets == nil {
		s.render(w, s.tmpl.keys, d)
		return
	}
	d.Configured = true
	d.Groups = groupByCategory(s.secrets.List())
	s.render(w, s.tmpl.keys, d)
}

func (s *Server) handleKeysSetForm(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
		return
	}
	id := r.PathValue("id")
	// Body-only intake: parse the form and read PostForm (NOT r.FormValue, which
	// merges the query string) so a secret can never land in a URL/log. Reject a
	// request that tries to smuggle the value through the query string.
	if err := r.ParseForm(); err != nil {
		redirectKeys(w, r, "", "Couldn't read that request.")
		return
	}
	if r.URL.Query().Has("value") {
		s.log.Warn("dashboard: key set rejected query value", "credID", id)
		redirectKeys(w, r, "", "Submit the key in the form, not the URL.")
		return
	}
	value := r.PostForm.Get("value")
	if strings.TrimSpace(value) == "" {
		redirectKeys(w, r, "", "Paste a key first.")
		return
	}
	if err := s.secrets.SetSecret(id, value); err != nil {
		// Never reflect err.Error() into the flash (it can carry key fragments);
		// log it server-side with the credID only.
		s.log.Warn("dashboard: key set", "credID", id, "err", err)
		redirectKeys(w, r, "", "Couldn't store that key.")
		return
	}
	redirectKeys(w, r, "Key stored. Restart the agent to apply it.", "")
}

func (s *Server) handleKeysRemoveForm(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
		return
	}
	id := r.PathValue("id")
	if err := s.secrets.RemoveSecret(id); err != nil {
		s.log.Warn("dashboard: key remove", "credID", id, "err", err)
		redirectKeys(w, r, "", "Couldn't remove that key.")
		return
	}
	redirectKeys(w, r, "Key removed from disk. The running agent keeps the old key in memory until restart.", "")
}

// redirectKeys 303s back with a server-generated flash. It NEVER carries
// err.Error() or any submitted value — only the fixed strings above.
func redirectKeys(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	target := "/settings/keys"
	switch {
	case errMsg != "":
		target += "?err=" + url.QueryEscape(errMsg)
	case msg != "":
		target += "?msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
