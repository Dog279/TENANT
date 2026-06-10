# Write-Only API-Key Settings Page

> **STATUS: IMPLEMENTED (2026-06-08, TEN-145).** Built from this design →
> implement → adversarial security re-review (clean: 0 CRITICAL/HIGH/MED) →
> verify. Gates green: full `go test ./...` (38 pkgs), gofmt, vet,
> `GOOS=windows`/`linux` cross-compile.
>
> **Shipped:** `internal/dashboard/keys.go` (SecretsControl + ServiceKeyView +
> handlers + `SetSecrets` setter), `internal/dashboard/templates/keys.html`,
> `cmd/tenant/keyscatalog.go` (catalog + lookup guard), `cmd/tenant/keysmgr.go`
> (`dashKeys` adapter + `clearProviderStored`); wiring in `server.go`/`ssr.go`/
> `layout.html` (nav)/`dashboardmgr.go`/`commands.go`. `dashboard.New` signature
> unchanged. Tests: `internal/dashboard/keys_test.go` + `cmd/tenant/keysmgr_test.go`.
>
> **Divergences from this design, forced by verifying against real read-paths:**
> - The skill-config framework (`skillKinds`) is an **empty stub** in production,
>   so skill/web keys are written **directly to credentials.json** (not via
>   `SkillConfigure`, which the design assumed). Only LLM providers go through
>   `AddCloudModel`.
> - **X and iMessage excluded**: their secrets are env/flag-only — NOT read from
>   credentials.json — so a GUI field would be a silent no-op.
> - **Google Workspace excluded**: its credential is a serialized OAuth-token JSON
>   blob from an OAuth flow, not a paste-able API key (a paste field would
>   mislead). Final catalog = 6 LLM providers + 3 web search + Discord token = **10**.
> - EnvShadow is **informational only** (not "env wins") — resolution precedence
>   differs per service, so the page shows env presence as an FYI and never
>   suppresses the stored-key controls.
>
> **Follow-up — LIVE key reload (2026-06-08, TEN-147): no restart to rotate a key.**
> A 24/7 runtime can't restart on every key rotation, so keys now resolve live:
> - **Web-search keys → lazy**: `web.Config` gained `*KeyFunc` resolvers
>   (`braveKey()/tavilyKey()/jinaKey()` accessors) wired to `credKey`, re-read on
>   EVERY search/read. Rotating Brave/Tavily/Jina (env or credentials.json) is
>   picked up with zero trigger.
> - **LLM provider keys → hot-swap**: `modelControl.ReloadKeys()` re-resolves the
>   ACTIVE provider's key and `SetProfiles` in place (reusing the `/model use`
>   path; clears the degraded gate on a reachable ✓). Triggered by: the settings
>   page (`dashKeys.maybeReloadProvider`, async, also recovers a missing-key
>   degrade), a new `/model reload` command, and a `watchCredentials` goroutine
>   that polls credentials.json mtime (skips while degraded so an unrelated edit
>   can't swap echo→a down provider). All callers serialize on `mc.mu`; the
>   shared `*model.Router` propagates to main agent + cron + relay + dashboard.
> - **Correctness fix:** `atomicWrite` now uses a UNIQUE temp file (os.CreateTemp)
>   instead of a fixed `.tmp`, so the new concurrent-writer surface (settings page
>   + watcher + /model) can't collide mid-write. Adversarial review = clean.
> - Known minor: a credentials-file change triggers a ~4s reachability probe under
>   `mc.mu`, briefly stalling concurrent `/model` commands (rare; acceptable).
> - Not live: the Discord *service* token (relay agent captures it at build) — a
>   rotation there still needs a relay restart. Documented.


Implementation-ready design for the API-key management page in the Tenant web GUI. Mounted at `/settings/keys`. Additive only — reuses `credentials.json`, the existing guarded write flows, and the nil-safe SSR section pattern established by `internal/dashboard/cron.go` and the memory page. Does **not** change `dashboard.New`'s signature.

## Goal

Let an authenticated operator add, replace, and remove the API keys/secrets that Tenant uses (LLM providers, web-search backends, integration skills) from the dashboard — **without any read path that can ever surface a secret value**. The page shows *presence and provenance* (set / not set / from-env / required) and accepts secrets one-way inbound. There is no value, no last-4, and no masked hint anywhere on the read side.

## Decisions

1. **Write-only is absolute.** `ServiceKeyView` carries booleans only — no value, no last-4, no `maskSecret`. This overrides the original brief's "optional last-4": a last-4 forces the read path to touch the secret, breaking the invariant. `maskSecret` (first-3+last-3) also leaks structured-token prefixes (`sk-…`, bearer prefixes); it is deliberately not used here.
2. **Fixed catalog is the mutable id space.** The page derives rows from a static `keyCatalog`, never by iterating raw `creds.Secrets`. A hostile POST can only target a catalog `CredID`; arbitrary store keys cannot be written.
3. **Reuse, don't reinvent the write path.** All mutations go through existing guarded flows (`AddCloudModel`, `SkillConfigure`/`SkillClear`, `creds.set`/`creds.save`) which preserve creds-first ordering and the `0600 atomicWrite`. No new secret-write path is introduced.
4. **Mirror the cron SSR section** for routing, nil-safety, setter injection, and templates — but **do not** copy cron's flash behaviour: cron reflects `err.Error()` into the flash, which leaks `clipSecret`'d secret prefixes. Keys handlers use fixed generic flash strings only (see C2).
5. **Path is `/settings/keys`** (per task spec). It does not collide with the existing `GET /settings` PSK-login route — the keys paths are strictly more specific. It is **not** added to the `checkAuth` exemption switch.
6. **No-key services are excluded entirely.** DuckDuckGo and local backends (vllm/ollama/llamacpp/echo) are neither rendered nor mutable. Embeddings fold into the LLM provider row (reuses the embedder-role provider key — no separate row). ~13 actionable rows.
7. **Body-only secret intake.** Handlers read `r.PostForm.Get("value")` after `r.ParseForm()`, never `r.FormValue` (which merges the query string and would let a secret land in a URL/log). See C1.

## Service catalog

Single source of truth: new file `cmd/tenant/keyscatalog.go`.

| Name | Category | CredID | Required* | Kind |
|------|----------|--------|-----------|------|
| OpenAI | LLM provider | `openai` | yes | provider |
| Anthropic | LLM provider | `anthropic` | yes | provider |
| Grok | LLM provider | `grok` | yes | provider |
| Zai coding | LLM provider | `zai` | yes | provider |
| Zai CN | LLM provider | `zai-coding-cn` | yes | provider |
| Zai metered | LLM provider | `zai-metered` | yes | provider |
| Tavily | Web search | `tavily` | no | web |
| Brave | Web search | `brave_search` | no | web |
| Jina Reader | Web search | `jina` | no | web |
| Discord | Integration | `skill:discord:token` | yes | skill |
| X (Twitter) | Integration | `skill:x:bearer` | no | skill |
| Google Workspace | Integration | `skill:gsuite:oauth_token` | yes | skill |
| iMessage (BlueBubbles) | Integration | `skill:imessage:password` | no | skill |

\* `Required` means "required *if you want this service*" — surfaced as a red badge only when the service is unset. It never forces a key.

```go
package main

type keyKind int

const (
	kindProvider  keyKind = iota // creds id == provider kind id; AddCloudModel flips authCfg.Stored
	kindWebSearch                // bare web id in credentials.json
	kindSkill                    // skill:<id>:<field>
)

type keySpec struct {
	CredID   string   // canonical store id
	Name     string   // display label
	Category string   // "LLM provider" | "Web search" | "Integration"
	EnvVars  []string // env that shadows the stored key at runtime
	Required bool
	Kind     keyKind
}

// Ordered: LLM providers, web search, integrations. No-key services excluded.
var keyCatalog = []keySpec{
	{CredID: "openai", Name: "OpenAI", Category: "LLM provider", EnvVars: []string{"OPENAI_API_KEY"}, Required: true, Kind: kindProvider},
	{CredID: "anthropic", Name: "Anthropic", Category: "LLM provider", EnvVars: []string{"ANTHROPIC_API_KEY"}, Required: true, Kind: kindProvider},
	{CredID: "grok", Name: "Grok", Category: "LLM provider", EnvVars: []string{"XAI_API_KEY"}, Required: true, Kind: kindProvider},
	{CredID: "zai", Name: "Zai coding", Category: "LLM provider", EnvVars: []string{"ZAI_API_KEY"}, Required: true, Kind: kindProvider},
	{CredID: "zai-coding-cn", Name: "Zai CN", Category: "LLM provider", EnvVars: []string{"ZAI_API_KEY"}, Required: true, Kind: kindProvider},
	{CredID: "zai-metered", Name: "Zai metered", Category: "LLM provider", EnvVars: []string{"ZAI_API_KEY"}, Required: true, Kind: kindProvider},
	{CredID: "tavily", Name: "Tavily", Category: "Web search", EnvVars: []string{"TAVILY_API_KEY", "TAVILY_KEY"}, Kind: kindWebSearch},
	{CredID: "brave_search", Name: "Brave", Category: "Web search", EnvVars: []string{"BRAVE_SEARCH_API_KEY", "BRAVE_API_KEY"}, Kind: kindWebSearch},
	{CredID: "jina", Name: "Jina Reader", Category: "Web search", EnvVars: []string{"JINA_API_KEY", "JINA_KEY"}, Kind: kindWebSearch},
	{CredID: "skill:discord:token", Name: "Discord", Category: "Integration", EnvVars: []string{"DISCORD_BOT_TOKEN"}, Required: true, Kind: kindSkill},
	{CredID: "skill:x:bearer", Name: "X (Twitter)", Category: "Integration", EnvVars: []string{"X_BEARER_TOKEN"}, Kind: kindSkill},
	{CredID: "skill:gsuite:oauth_token", Name: "Google Workspace", Category: "Integration", Required: true, Kind: kindSkill},
	{CredID: "skill:imessage:password", Name: "iMessage (BlueBubbles)", Category: "Integration", EnvVars: []string{"BLUEBUBBLES_URL", "BLUEBUBBLES_PASSWORD"}, Kind: kindSkill},
}

// lookupKeySpec is the single mutation chokepoint: only catalog ids are mutable.
func lookupKeySpec(credID string) (keySpec, bool) {
	for _, s := range keyCatalog {
		if s.CredID == credID {
			return s, true
		}
	}
	return keySpec{}, false
}

// splitSkillCredID turns "skill:discord:token" -> ("discord", "token").
func splitSkillCredID(credID string) (id, field string) {
	parts := strings.SplitN(credID, ":", 3) // ["skill","discord","token"]
	if len(parts) != 3 {
		return "", ""
	}
	return parts[1], parts[2]
}
```

## Data model & control interface

New file `internal/dashboard/keys.go` — the security core. **No value field exists on the read side.**

```go
package dashboard

// SecretsControl is the runtime surface the keys page drives. Presence-only on
// read; secrets travel one-way inbound through SetSecret. A nil control makes
// the page render a "not configured" notice (never a 404, never a panic).
type SecretsControl interface {
	List() []ServiceKeyView // catalog ⨝ store; booleans only, NEVER a value
	SetSecret(credID, value string) error
	RemoveSecret(credID string) error
}

// ServiceKeyView is render-ready and deliberately carries NO secret value and
// NO masked hint — write-only means the read path cannot express the secret.
type ServiceKeyView struct {
	CredID    string
	Name      string
	Category  string
	EnvVars   []string
	Set       bool // creds.get(credID) != "" — presence only
	EnvShadow bool // an EnvVar is present in the environment (env wins at runtime)
	Required  bool
}

// ServiceKeyGroup lets the template iterate by category with no logic in HTML.
type ServiceKeyGroup struct {
	Category string
	Rows     []ServiceKeyView
}

func (s *Server) SetSecrets(c SecretsControl) { s.secrets = c }
```

### cmd/tenant adapter

New file `cmd/tenant/keysmgr.go` — adapter over the credentials store, mirroring `dashCron`. **Pinned to the real live signatures** (verified):

- `(mc *modelControl) AddCloudModel(kindID, apiKey string) (string, error)` — returns `(string, error)`.
- `forgetProviderSecret(cfgDir, name string)` — returns nothing; **deletes the creds map entry only, does NOT clear `authCfg.Stored`** (verified `modelctl.go:406-415`).
- `(sc *skillCfgControl) SkillConfigure(args []string, noEnable bool) (string, error)` — flat `args`, first elem is the skill id, rest are `value` or `key=value`.
- `(sc *skillCfgControl) SkillClear(id, fieldKey string) (string, error)`.
- `(c *credentials) get/set/save`, `loadCredentials(cfgDir)` (`launchconfig.go:446-480`).

```go
package main

import (
	"fmt"
	"os"

	"tenant/internal/dashboard" // adjust to the real module path
)

type dashKeys struct {
	cfgDir string
	mc     *modelControl
	sc     *skillCfgControl
}

var _ dashboard.SecretsControl = dashKeys{}

// List cross-references the fixed catalog against the store + environment.
// It reads only presence (get(id) != "") — never the value — into the view.
func (k dashKeys) List() []dashboard.ServiceKeyView {
	creds, _ := loadCredentials(k.cfgDir) // missing file => empty map, no error
	out := make([]dashboard.ServiceKeyView, 0, len(keyCatalog))
	for _, spec := range keyCatalog {
		envShadow := false
		for _, ev := range spec.EnvVars {
			if os.Getenv(ev) != "" {
				envShadow = true
				break
			}
		}
		out = append(out, dashboard.ServiceKeyView{
			CredID:    spec.CredID,
			Name:      spec.Name,
			Category:  spec.Category,
			EnvVars:   spec.EnvVars,
			Set:       creds.get(spec.CredID) != "",
			EnvShadow: envShadow,
			Required:  spec.Required,
		})
	}
	return out
}

func (k dashKeys) SetSecret(credID, value string) error {
	spec, ok := lookupKeySpec(credID) // guard: only catalog ids are mutable
	if !ok {
		return fmt.Errorf("unknown key")
	}
	switch spec.Kind {
	case kindProvider:
		// creds-first ordering + flips authCfg{Mode:"apikey",Stored:true}; idempotent.
		_, err := k.mc.AddCloudModel(spec.CredID, value)
		return err
	case kindSkill:
		id, field := splitSkillCredID(spec.CredID)
		// SkillConfigure(args []string, noEnable bool): args[0]=id, rest=key=value.
		_, err := k.sc.SkillConfigure([]string{id, field + "=" + value}, false)
		return err
	case kindWebSearch:
		creds, err := loadCredentials(k.cfgDir)
		if err != nil {
			return err
		}
		creds.set(spec.CredID, value)
		return creds.save(k.cfgDir) // 0600 atomicWrite
	}
	return fmt.Errorf("unknown key")
}

func (k dashKeys) RemoveSecret(credID string) error {
	spec, ok := lookupKeySpec(credID)
	if !ok {
		return fmt.Errorf("unknown key")
	}
	switch spec.Kind {
	case kindProvider:
		// forgetProviderSecret deletes the creds entry but does NOT clear Stored;
		// clearProviderStored finishes the job (see H1).
		forgetProviderSecret(k.cfgDir, spec.CredID)
		return clearProviderStored(k.cfgDir, spec.CredID)
	case kindSkill:
		id, field := splitSkillCredID(spec.CredID)
		_, err := k.sc.SkillClear(id, field) // auto-disables a skill if a required field is cleared
		return err
	case kindWebSearch:
		creds, err := loadCredentials(k.cfgDir)
		if err != nil {
			return err
		}
		delete(creds.Secrets, spec.CredID)
		return creds.save(k.cfgDir)
	}
	return fmt.Errorf("unknown key")
}
```

`clearProviderStored(cfgDir, kindID)` is a small new helper in `keysmgr.go` (or `modelctl.go`): load `launchConfig`, set the matching provider's `Auth.Stored = false`, and `save(cfgDir)`. Without it, `config.json` keeps `Stored:true` with no secret behind it and `resolveSecret` returns `""`, silently breaking the provider (H1). The `value` parameter is never logged or interpolated into any error.

## Dashboard wiring

All edits mirror the cron SSR section. Fully additive.

1. **Server field** (`server.go`, beside `mem`/`cron`):
   ```go
   secrets SecretsControl // write-only API-key admin; nil => "not configured"
   ```

2. **Setter** (`keys.go`): `func (s *Server) SetSecrets(c SecretsControl) { s.secrets = c }` (mirrors `SetCron`, `cron.go:62`).

3. **Mount method** (`keys.go`, mirrors `mountCronSSR`, `cron.go:66`):
   ```go
   func (s *Server) mountSecretsSSR(mux *http.ServeMux) {
       mux.HandleFunc("GET /settings/keys", s.handleKeysPage)
       mux.HandleFunc("POST /settings/keys/{id}/set", s.handleKeysSetForm)
       mux.HandleFunc("POST /settings/keys/{id}/remove", s.handleKeysRemoveForm)
   }
   ```

4. **routes()** (`server.go`): add `s.mountSecretsSSR(s.mux)` beside `s.mountCronSSR(s.mux)`, **unconditionally** (handlers nil-guard, per the SSR contract).

5. **Templates** (`ssr.go`): add `keys *template.Template` to `ssrTemplates`; in `parseSSR` add `keys: must("templates/layout.html", "templates/keys.html")`. Embedded via the existing `//go:embed templates`.

6. **Nav** (`layout.html`, after the Cron link):
   ```html
   <a href="/settings/keys" class="{{if eq .Page "keys"}}on{{end}}">Keys</a>
   ```

7. **Runtime install** (`dashboardmgr.go` `Enable`, after construction): add field `secrets dashboard.SecretsControl` to `dashboardManager`, then beside the `SetCron` call:
   ```go
   if m.secrets != nil { srv.SetSecrets(m.secrets) }
   ```
   In `commands.go` (the `dashboardManager` literal, ~2077-2094), construct `dashKeys{cfgDir: …, mc: …, sc: …}` and assign it to the `secrets:` field.

8. **No auth changes.** Every route on `s.mux` is wrapped by `secure()` in `Run()` (auth + same-origin + fail-closed bind). `/settings/keys*` is **not** added to the `checkAuth` exemption switch.

### Handlers (`keys.go`)

```go
func (s *Server) handleKeysPage(w http.ResponseWriter, r *http.Request) {
	d := keysPageData{layoutData: layoutData{Title: "Keys", Page: "keys"}}
	d.Msg = r.URL.Query().Get("msg")
	d.Err = r.URL.Query().Get("err")
	if s.secrets == nil {
		d.Configured = false
		s.render(w, s.tmpl.keys, d)
		return
	}
	d.Configured = true
	d.Groups = groupByCategory(s.secrets.List()) // catalog order
	s.render(w, s.tmpl.keys, d)
}

func (s *Server) handleKeysSetForm(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
		return
	}
	id := r.PathValue("id")
	// C1: body-only intake. Never r.FormValue (which merges the query string).
	if err := r.ParseForm(); err != nil {
		redirectKeys(w, r, "", "Couldn't read that request.")
		return
	}
	if r.URL.Query().Has("value") { // reject ?value= entirely
		s.log.Warn("dashboard: key set rejected query value", "credID", id)
		redirectKeys(w, r, "", "Submit the key in the form, not the URL.")
		return
	}
	value := r.PostForm.Get("value")
	if value == "" {
		redirectKeys(w, r, "", "Paste a key first.")
		return
	}
	if err := s.secrets.SetSecret(id, value); err != nil {
		// C2: never reflect err.Error(); fixed generic flash. Log credID only.
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
	// H2: security-toned copy — disk is cleared but memory isn't until restart.
	redirectKeys(w, r, "Key removed from disk. The running agent keeps the old key in memory until restart — restart to fully revoke.", "")
}

// redirectKeys 303s back with a server-generated flash. It NEVER carries
// err.Error() or any submitted value — only fixed strings produced above.
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
```

`keysPageData`:
```go
type keysPageData struct {
	layoutData
	Configured bool
	Groups     []ServiceKeyGroup
	Msg, Err   string
}
```

## Templates & write-only UI

New file `internal/dashboard/templates/keys.html`, `{{define "content"}}…{{end}}`, rendered through `layout.html`. Every dynamic field via `{{.Field}}` (html/template auto-escape) — **never `template.HTML`** — so a hostile catalog name/id cannot inject markup.

Structure:
- Top flash banner: `{{if .Msg}}…{{end}}` / `{{if .Err}}…{{end}}`.
- `{{if not .Configured}}` → "Key management isn't configured." notice, then stop.
- One restart note: "Changes apply to the running agent after a restart (secrets are resolved once at launch)."
- `{{range .Groups}}` → category heading `{{.Category}}`, then `{{range .Rows}}` one row each.

Per-row states (write-only; **no value field is ever rendered, no input pre-filled, no masked hint**):

- **Env-shadowed** (`.EnvShadow`): blue badge `From env ${{first env var}}` (env wins at runtime). If a stored copy *also* exists (`.Set`), **still render the Remove form** with copy "Env var is active; a stored copy also exists — remove it from disk" (M3). The Add/Replace input is suppressed while env shadows.
- **Set / stored** (`.Set`, not env-shadowed): green **"Stored"** badge + a password "Replace" input posting to the `set` route (overwrites) + a separate "Remove" form. No value shown.
- **Unset** (not `.Set`): if `.Required`, a red "Required" badge, else a grey "Not set" badge; password input + "Add" button.

Illustrative row markup:
```html
<div class="keyrow">
  <span class="name">{{.Name}}</span>

  {{if .EnvShadow}}
    {{if .EnvVars}}<span class="badge env">From env ${{index .EnvVars 0}}</span>{{end}}
    {{if .Set}}
      <form method="POST" action="/settings/keys/{{.CredID}}/remove">
        <button type="submit">Remove stored copy</button>
      </form>
    {{end}}
  {{else if .Set}}
    <span class="badge ok">Stored</span>
    <form method="POST" action="/settings/keys/{{.CredID}}/set" autocomplete="off">
      <input type="password" name="value" autocomplete="new-password" placeholder="Replace key">
      <button type="submit">Replace</button>
    </form>
    <form method="POST" action="/settings/keys/{{.CredID}}/remove">
      <button type="submit">Remove</button>
    </form>
  {{else}}
    {{if .Required}}<span class="badge req">Required</span>{{else}}<span class="badge off">Not set</span>{{end}}
    <form method="POST" action="/settings/keys/{{.CredID}}/set" autocomplete="off">
      <input type="password" name="value" autocomplete="new-password" placeholder="Paste key">
      <button type="submit">Add</button>
    </form>
  {{end}}
</div>
```

- `index .EnvVars 0` is guarded by `{{if .EnvVars}}` so an env-shadowed row with an empty `EnvVars` (impossible in the current catalog, latent foot-gun otherwise) cannot panic the template (M3).
- Helper text under inputs: "Stored in credentials.json (0600)."
- Inputs use `type="password"`, `autocomplete="new-password"`, no `value=` attribute; forms use `autocomplete="off"`. Both Add and Replace inputs carry `autocomplete="new-password"` (L2). There is no read path that could populate a `value=`.

## Security invariants & residual risks

### Invariants (no-leak guarantees)

1. **No read path for secret values exists.** `ServiceKeyView` has no value/last-4/mask field; no GET/JSON/template/handler/Datastar-patch path ever calls `creds.get` into a response. `List()` only assigns `get(id) != ""` to a bool. (Overrides the brief's "optional last-4".)
2. **Secrets travel inbound only, in the POST body.** Handlers call `r.ParseForm()` then `r.PostForm.Get("value")` — never `r.FormValue` (which merges query params). A request carrying `?value=` is rejected outright. A secret can never land in a URL, query string, path, `Referer`, browser history, or access/proxy log. *(C1 mitigation.)*
3. **Flash is fixed and server-generated.** Handlers never interpolate `err.Error()` (or the submitted value) into the flash/redirect — only constant strings (`"Couldn't store that key."`, etc.). Raw errors are logged server-side with `credID` only, never the value. This deliberately diverges from cron's `err.Error()` reflection (`cron.go:112/127/141/155`), whose underlying flows carry `clipSecret(value)` (`modelctl.go:268`) — i.e. the first 6 chars of a live key. *(C2 mitigation.)*
4. **POST-only mutations, PRG/303.** Auth + same-origin + fail-closed bind are inherited from `secure()`. `/settings/keys*` is not exempted in `checkAuth`.
5. **Mutable id space is the fixed catalog.** `SetSecret`/`RemoveSecret` reject any `credID` not in `keyCatalog` via `lookupKeySpec`, so a hostile or traversal-shaped POST (`/settings/keys/..%2f..%2fopenai/set`) performs no write. This guard is the single chokepoint.
6. **0600 preserved; creds-first ordering.** All writes go through existing guarded flows (`AddCloudModel`, `SkillConfigure`, `creds.save`) → the `atomicWrite` 0600 path (`launchconfig.go:503`). The `.tmp` file is created 0600 then renamed; `config.json` (0644, non-secret) only ever receives `Stored:true`, never a value.
7. **Deletes are complete.** Provider remove runs `forgetProviderSecret` **and** `clearProviderStored` so `config.json` does not retain `Stored:true` with no secret (H1). Skill remove via `SkillClear` auto-disables a skill whose required field was cleared. Web remove deletes the map entry and saves.
8. **Auto-escaped rendering.** All dynamic fields via `{{.Field}}`; never `template.HTML`. Hostile catalog name/id renders as text.
9. **Runtime honesty.** Set flash states a restart is required; Remove flash explicitly states the running agent keeps the old key in memory until restart (H2).
10. **CSRF posture.** Mutating routes require same-origin: keys POST handlers reject Origin-less / non-same-origin requests rather than relying solely on `checkOrigin`'s permissive Origin-less allowance. The `SameSite=Strict` auth cookie remains the primary defense; the WS bearer-in-query fallback is scoped to `/ws` only and does not apply here. *(M1 mitigation — see residuals for the optional token.)*

### Residual risks

- **R1 — No hot-revoke on most paths (accepted).** LLM-gen/embed keys, web-search keys, and the Discord token are resolved once at launch and held in memory; removing them on disk does not stop the live process until restart. Mitigated by explicit Remove copy (#9). If the provider router exposes a live rebuild hook, trigger it on provider remove for immediate revocation on that path; web-search/discord cannot hot-revoke. Documented, not silently broken.
- **R2 — No CSRF token (accepted, defense-in-depth optional).** Defense rests on `SameSite=Strict` cookie + enforced same-origin on the mutating routes (#10). A per-render HMAC CSRF hidden field (keyed on the session/PSK) validated in the POST handlers would add depth on the highest-value write surface, but is not required for v1.
- **R3 — Stale `.tmp` reuse (low probability).** `atomicWrite` writes `path+".tmp"` at the requested 0600 then renames; a prior crash could leave a looser-perm `.tmp` that gets reused. Optional hardening: `os.OpenFile(tmp, O_CREATE|O_TRUNC|O_WRONLY, 0600)` or a defensive `os.Remove(tmp)` first. No new world-readable file is introduced by this design.
- **R4 — Signature drift.** The adapter is pinned to verified live signatures; if `AddCloudModel`/`SkillConfigure`/`SkillClear`/`forgetProviderSecret`/`creds.save` change, re-pin the adapter. The `string` returns from the model/skill calls are intentionally discarded.

## Additivity & cross-platform notes

- **Purely additive.** New files only for the catalog, adapter, control, template, and test; edits to `server.go`, `ssr.go`, `layout.html`, `dashboardmgr.go`, `commands.go` add fields/lines beside the existing cron wiring. `dashboard.New`'s signature is unchanged — injection is via the `SetSecrets` setter, exactly like `SetCron`.
- **Nil-safe.** A nil `SecretsControl` renders the "not configured" notice (200, never 404/panic); nil-control POSTs 303 without panicking. Matches the cron/memory section pattern.
- **Cross-platform.** No OS-specific code. The catalog includes iMessage (BlueBubbles), but the page only reads/writes the `skill:imessage:password` credential string — it does not invoke any macOS-only native transport, so it builds and runs identically on Windows/Linux. Windows build stability is preserved (additive-only constraint honored).

## Test plan

New file `internal/dashboard/keys_test.go`, modeled on `cron_test.go` — drive through `s.Handler().ServeHTTP` (raw mux, bypasses `secure()`), with a fake `SecretsControl`.

1. **nil control** → `GET /settings/keys` renders the "not configured" notice with 200 (no 404).
2. **populated fake** → page lists rows grouped by category in catalog order; Stored / Not set / Required / env badges render per state.
3. **XSS escape** → a hostile `CredID`/`Name` (e.g. `<script>`) is HTML-escaped: raw markup absent, `&lt;` present.
4. **set/remove happy path** → `POST /settings/keys/{id}/set` and `/remove` return 303 and reach the fake with the correct `credID`/value.
5. **C1 regression** → `POST /settings/keys/openai/set?value=sk-LEAKED` with an empty body returns 303 and does **NOT** set the key; `sk-LEAKED` appears in no response body and no `Location` header.
6. **C2 regression** → make the fake's `SetSecret` return an error whose message embeds a `clipSecret`-style 6-char prefix of the value; assert no prefix of the value appears in the response body **or** the `Location` header — only the fixed generic flash string.
7. **nil-control POST** → set/remove 303 without panicking.
8. **catalog guard (L1)** → unknown id and `id="../../etc"` both 303 with a generic error and perform no write on the fake.
9. **provider-remove completeness (H1)** → integration-style test (or fake assertion) that after a provider remove, `config.json` has `Stored:false` (no orphaned `Stored:true`).
10. **write-only regression (the core)** → feed every catalog secret into the fake as a set value, render the page, and scan the full response body for any fed-in value — none may appear.

## Out of scope

- Reading, displaying, masking, or last-4 of any secret value (write-only is absolute).
- Live/hot key rotation without restart for web-search and Discord paths (R1).
- A general key-value secrets editor over arbitrary `creds.Secrets` (the catalog is the fixed mutable surface).
- No-key services (DuckDuckGo, vllm/ollama/llamacpp/echo) — excluded from the catalog.
- New auth mechanisms; the page inherits `secure()` and the existing `SameSite=Strict` cookie. An optional CSRF token is a future hardening (R2).
- Changing `dashboard.New`'s signature or the credentials/launch-config file formats.
