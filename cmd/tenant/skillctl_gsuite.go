package main

// TEN-65: gsuite catalog entry + validators + probe.
//
// gsuite is the first production platform in skillKinds (TEN-64
// shipped the framework with an empty catalog). The probe re-uses the
// existing gsuite plugin's auth code (`gsuite.Open` + `Gmail.Search`)
// rather than re-rolling OAuth, so credential rotation here uses the
// EXACT same code paths as the runtime plugin.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"os"
	"os/exec"
	refpkg "reflect"
	"runtime"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"tenant/internal/plugins/gsuite"
)

// gsuiteSkillKind builds the catalog entry. Constructed by init in
// this file (no other init in the package — the skillKinds map is
// otherwise a plain literal). Per-platform tickets each add their
// own kind via this same pattern.
func gsuiteSkillKind() skillKind {
	return skillKind{
		ID:    "gsuite",
		Label: "Google Workspace (Gmail, Calendar, Drive) — business / SA + DWD",
		Wired: true,
		SetupHint: "tenant targets BUSINESS Google Workspace deployments. Three auth modes, in order of fit:\n" +
			"  • sa     — service-account JSON + Domain-Wide Delegation. The canonical pattern: IT admin creates an SA, authorizes DWD once in admin.google.com, tenant impersonates per-user subjects. (Recommended for ANY production / multi-user setup.)\n" +
			"  • gcloud — reuses an existing `gcloud auth application-default login` session. Dev-machine only.\n" +
			"  • oauth  — operator-supplied Desktop App OAuth client (advanced; for personal @gmail accounts where the operator owns a Cloud project).",
		Fields: []skillKindField{
			{
				Key:    "auth",
				Prompt: "How does this install authenticate to Google Workspace?",
				Required: true,
				Default:  "sa", // business primary
				Options:  []string{"sa", "gcloud", "oauth"},
				OptionLabels: []string{
					"Service Account + Domain-Wide Delegation (recommended — business / multi-user)",
					"gcloud CLI Application Default Credentials (dev machine only)",
					"OAuth Desktop App client (advanced — personal @gmail)",
				},
				Validate: validateOneOf("sa", "gcloud", "oauth"),
				NoteAfter: gsuiteAuthNote,
			},
			{
				Key:    "sa_json",
				Prompt: "Path to the service-account JSON key (download from console.cloud.google.com)",
				Required: true,
				Validate: validateSAJSON,
				ShowIf:   func(v map[string]string) bool { return v["auth"] == "sa" },
			},
			{
				Key:    "subject",
				Prompt: "Email of the user the service account will impersonate (Domain-Wide Delegation)",
				Required: true,
				Validate: validateEmailRFC5322,
				ShowIf:   func(v map[string]string) bool { return v["auth"] == "sa" },
			},
			{
				Key:    "oauth_creds_json",
				Prompt: "Path to OAuth client JSON (console.cloud.google.com → APIs & Services → Credentials → Desktop App)",
				Required: true,
				Validate: validateOAuthCredsJSON,
				ShowIf:   func(v map[string]string) bool { return v["auth"] == "oauth" },
			},
		},
		Probe: probeGSuite,
		// TEN-71: oauth path opens a browser for up to 5 min. gcloud +
		// sa probes complete in seconds, but the timeout is per-skill
		// not per-mode, so we set the worst-case here. probeGSuite uses
		// its own internal context for the fast paths.
		ProbeTimeout: 5 * time.Minute,
	}
}

// validateSAJSON returns nil when path is empty (sa_json is optional
// at the framework level — probe enforces sa-mode requirement). When
// set, the file must exist, parse as JSON, AND carry the
// `"type": "service_account"` marker that confirms it's a Google key
// rather than some other JSON file.
func validateSAJSON(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file not found at %q (check the path)", path)
		}
		return fmt.Errorf("cannot read %q: %v", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%q is a directory — expected a JSON file", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %q: %v", path, err)
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return fmt.Errorf("%q is not valid JSON: %v", path, err)
	}
	if probe.Type != "service_account" {
		return fmt.Errorf("%q is not a service-account key (missing 'type: service_account')", path)
	}
	return nil
}

// saClientID pulls the numeric OAuth client ID out of a service-account key.
// That's the value a Workspace admin must authorize under Domain-Wide
// Delegation — the SA email won't match in that console. Best-effort:
// returns "" if the bytes don't parse (the caller is already on an error
// path and the message degrades gracefully).
func saClientID(saJSON []byte) string {
	var v struct {
		ClientID string `json:"client_id"`
	}
	_ = json.Unmarshal(saJSON, &v)
	return v.ClientID
}

// validateOAuthCredsJSON checks the path resolves to a valid Desktop
// App OAuth client JSON (the file Google Cloud Console hands over
// after creating an OAuth client of type "Desktop app"). Empty path
// passes validation — ShowIf filters this field for non-oauth modes.
func validateOAuthCredsJSON(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file not found at %q (check the path)", path)
		}
		return fmt.Errorf("cannot read %q: %v", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%q is a directory — expected a JSON file", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %q: %v", path, err)
	}
	// Reuse the plugin's parser — single source of truth for the JSON
	// shape, including the "installed vs web" branching.
	if _, err := gsuite.ParseOAuthClientFile(b, []string{"placeholder"}); err != nil {
		return err
	}
	return nil
}

// validateEmailRFC5322 returns nil for empty (subject is optional at
// the framework level; probe enforces sa-mode requirement). When set,
// must parse as a real address. Uses net/mail rather than a regex —
// the stdlib parser handles edge cases (quoted local-parts, IDN, etc.)
// that a hand-rolled regex won't.
func validateEmailRFC5322(s string) error {
	if s == "" {
		return nil
	}
	if _, err := mail.ParseAddress(s); err != nil {
		return fmt.Errorf("not a valid email address: %v", err)
	}
	return nil
}

// gsuiteHTTPDoer matches gsuite.Config.HTTP (the package's internal
// httpDoer interface) by structural shape. *http.Client satisfies it
// for production; a test's httptest-backed doer satisfies it for unit
// tests.
type gsuiteHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// gsuiteProbeDeps is the test seam. Production callers pass a zero
// value (all defaults: real http.DefaultClient + real exec.gcloud).
// Tests inject an httptest-backed doer + a fake gcloud runner.
type gsuiteProbeDeps struct {
	HTTP             gsuiteHTTPDoer
	Run              func(ctx context.Context, name string, args ...string) ([]byte, error)
	GcloudCheck      func() error           // override `gcloud not on PATH` check for tests
	OAuthOpenBrowser func(url string) error // test seam: stub the browser open for OAuth tests (TEN-71)
	// GetEmbeddedOAuth, when set, returns the maintainer-owned OAuth
	// client JSON bytes. nil ⇒ probe only uses operator-supplied JSON
	// (matches the pre-embedded-creds behavior + the unit-test default).
	GetEmbeddedOAuth func() ([]byte, error)
}

// probeGSuite verifies the configured auth works against Google by
// minting a token and making one cheap Gmail list call. Reuses the
// production plugin's auth code — credential rotation hits the same
// failure modes the live agent would.
//
// Returns:
//   - "gcloud ADC authenticated" on gcloud success (we don't fetch
//     userinfo because the runtime scopes don't include 'email')
//   - "<subject> (impersonated)" on sa success
//
// gcloud-mode pre-check: verify the `gcloud` binary is on PATH before
// shelling out (mirrors the TEN-25 `exec.LookPath` discipline so the
// failure mode is a clean error, not an exec-failed surprise).
func probeGSuite(ctx context.Context, creds *credentials, settings map[string]string, save func() error) (string, error) {
	return probeGSuiteWith(ctx, creds, settings, save, gsuiteProbeDeps{})
}

// probeGSuiteWith is the implementation. Tests call this directly
// with their httptest-backed deps; production callers reach it via
// probeGSuite with a zero-valued deps.
func probeGSuiteWith(ctx context.Context, creds *credentials, settings map[string]string, save func() error, deps gsuiteProbeDeps) (string, error) {
	auth := settings["auth"]
	if auth == "" {
		auth = "gcloud" // matches catalog default
	}
	switch auth {
	case "gcloud":
		// gcloud-mode pre-check. Tests inject GcloudCheck to bypass
		// the real PATH lookup when simulating gcloud-success on a CI
		// box where gcloud isn't installed.
		check := deps.GcloudCheck
		if check == nil {
			check = func() error {
				if _, err := exec.LookPath("gcloud"); err != nil {
					return errors.New("gcloud not found on PATH — install Cloud SDK, OR switch to auth=sa")
				}
				return nil
			}
		}
		if err := check(); err != nil {
			return "", err
		}
		cfg := gsuite.Config{Auth: "gcloud"}
		if deps.HTTP != nil {
			cfg.HTTP = deps.HTTP
		}
		if deps.Run != nil {
			cfg.Run = deps.Run
		}
		svc, err := gsuite.Open(cfg)
		if err != nil {
			return "", fmt.Errorf("gsuite.Open: %w", err)
		}
		if _, err := svc.Gmail.Search(ctx, "", 1); err != nil {
			return "", fmt.Errorf("Gmail probe failed (run `gcloud auth application-default login` if not done): %w", err)
		}
		return "gcloud ADC authenticated", nil

	case "sa":
		saPath := settings["sa_json"]
		subject := settings["subject"]
		if saPath == "" {
			return "", errors.New("auth=sa requires sa_json (path to service-account JSON key)")
		}
		if subject == "" {
			return "", errors.New("auth=sa requires subject (the user to impersonate via domain-wide delegation)")
		}
		// Re-run the file check at probe time — operator may have
		// deleted/moved the file since configure.
		if err := validateSAJSON(saPath); err != nil {
			return "", fmt.Errorf("sa_json: %v", err)
		}
		saBytes, err := os.ReadFile(saPath)
		if err != nil {
			return "", fmt.Errorf("read sa_json %q: %v", saPath, err)
		}
		cfg := gsuite.Config{
			Auth:    "sa",
			SAJSON:  saBytes,
			Subject: subject,
		}
		if deps.HTTP != nil {
			cfg.HTTP = deps.HTTP
		}
		svc, err := gsuite.Open(cfg)
		if err != nil {
			return "", fmt.Errorf("gsuite.Open: %w", err)
		}
		if _, err := svc.Gmail.Search(ctx, "", 1); err != nil {
			// unauthorized_client is Google's signal that this SA's client
			// ID isn't authorized for the requested scopes under Domain-Wide
			// Delegation — a Workspace Admin-console action, not a tenant
			// bug. Hand the operator the exact client ID + scopes to paste
			// (the raw 401 is cryptic, and an SA reused across projects is a
			// common trap). Keeps the "Gmail probe failed" prefix the
			// catalog/tests key on.
			if strings.Contains(err.Error(), "unauthorized_client") {
				return "", fmt.Errorf("Gmail probe failed: Google rejected impersonation of %s with unauthorized_client — "+
					"this service account's client ID isn't authorized for the requested scopes under Domain-Wide Delegation.\n"+
					"Authorize it in admin.google.com → Security → Access and data control → API controls → Domain-wide delegation → Add new:\n"+
					"  Client ID (numeric — NOT the SA email): %s\n"+
					"  Scopes (comma-separated): %s\n"+
					"  Add the read/write scopes too if you'll send/modify/create: %s\n"+
					"DWD matches scope strings EXACTLY and is per-client-ID: if you set delegation up for a DIFFERENT SA in a prior project, this SA's client ID still needs adding. "+
					"Wait ~1 min for propagation, then re-run `/skill probe gsuite`.\nunderlying: %w",
					subject, saClientID(saBytes),
					strings.Join(gsuite.ScopesFor(false), ","),
					strings.Join(gsuite.ScopesFor(true), ","),
					err)
			}
			return "", fmt.Errorf("Gmail probe failed (verify domain-wide delegation is configured for %s): %w", subject, err)
		}
		return fmt.Sprintf("%s (impersonated)", subject), nil

	case "oauth":
		// TEN-71: browser-based OAuth code-grant flow.
		// Resolution order:
		//   1. operator-supplied oauth_creds_json (their own OAuth client)
		//   2. maintainer-owned embedded creds under cfgDir (zero-Cloud
		//      path for end users)
		//   3. error
		var oauthCredsBytes []byte
		credsPath := settings["oauth_creds_json"]
		if credsPath != "" {
			b, err := os.ReadFile(credsPath)
			if err != nil {
				return "", fmt.Errorf("read oauth_creds_json %q: %v", credsPath, err)
			}
			oauthCredsBytes = b
		} else if deps.GetEmbeddedOAuth != nil {
			b, err := deps.GetEmbeddedOAuth()
			if err != nil {
				return "", fmt.Errorf("read embedded oauth creds: %v", err)
			}
			oauthCredsBytes = b
		}
		if len(oauthCredsBytes) == 0 {
			return "", errors.New("auth=oauth needs either oauth_creds_json setting OR a maintainer-installed OAuth client (run `tenant oauth-setup gsuite <path>` to install one)")
		}
		// Pull cached token from credentials.json (if any). We persist
		// the full *oauth2.Token JSON in one slot so refresh rotations
		// are atomic — wins over splitting into refresh_token /
		// access_token / expiry separately.
		var cachedTok *oauth2.Token
		if raw := creds.get(skillSecretID("gsuite", "oauth_token")); raw != "" {
			var t oauth2.Token
			if err := json.Unmarshal([]byte(raw), &t); err == nil {
				cachedTok = &t
			}
			// If parse fails, treat as no cache; flow re-prompts browser.
		}
		// Save callback writes the new/rotated token back into creds
		// AND persists creds to disk (so a crash mid-probe doesn't
		// strand the operator with a token they can't reuse).
		saveTok := func(t *oauth2.Token) error {
			b, err := json.Marshal(t)
			if err != nil {
				return err
			}
			creds.set(skillSecretID("gsuite", "oauth_token"), string(b))
			if save != nil {
				return save()
			}
			return nil
		}
		cfg := gsuite.Config{
			Auth:       "oauth",
			OAuthCreds: oauthCredsBytes,
			OAuthToken: cachedTok,
			OAuthSave:  saveTok,
		}
		if deps.HTTP != nil {
			cfg.HTTP = deps.HTTP
		}
		// Test seam: an httptest server pretends to be accounts.google.com
		// + oauth2.googleapis.com; tests provide a browser stub that POSTs
		// the callback URL directly.
		if deps.OAuthOpenBrowser != nil {
			cfg.OAuthOpenBrowser = deps.OAuthOpenBrowser
		}
		svc, err := gsuite.Open(cfg)
		if err != nil {
			return "", fmt.Errorf("gsuite.Open: %w", err)
		}
		if _, err := svc.Gmail.Search(ctx, "", 1); err != nil {
			return "", fmt.Errorf("Gmail probe failed: %w", err)
		}
		return "OAuth user authenticated (token cached)", nil

	default:
		// validateOneOf catches this at the configure-time validation
		// phase; the case branch exists as defense-in-depth.
		return "", fmt.Errorf("unknown auth mode %q (expected sa, gcloud, or oauth)", auth)
	}
}

// makeGsuiteAuthNote builds the post-pick guidance closure for the
// auth field. cfgDir lets the oauth branch detect maintainer-owned
// embedded OAuth credentials and skip the Cloud Console walkthrough
// entirely when they exist.
//
// Each branch:
//   1. Surfaces clear guidance on what to do next (handhold UX).
//   2. Pre-checks blocking prerequisites and ABORTS if not met (so we
//      don't pretend to configure a broken setup).
//   3. For oauth: if embedded creds exist, just confirms "signing
//      you in." If not, opens Cloud Console + walks through OAuth
//      client creation.
func makeGsuiteAuthNote(cfgDir string) func(string) (string, bool) {
	return func(value string) (string, bool) {
		return gsuiteAuthNoteFor(value, cfgDir)
	}
}

// gsuiteAuthNote is the default-cfgDir variant used by the catalog
// before adaptGSuiteForCfgDir runs (e.g. tests that hit skillKinds
// directly). Production runs through makeGsuiteAuthNote.
func gsuiteAuthNote(value string) (string, bool) {
	return gsuiteAuthNoteFor(value, "")
}

func gsuiteAuthNoteFor(value, cfgDir string) (string, bool) {
	switch value {
	case "gcloud":
		// Hard prereq: gcloud CLI installed. Without it, every gcloud-
		// mode probe fails — no point storing config. Abort cleanly.
		if _, err := exec.LookPath("gcloud"); err != nil {
			return "✗ gcloud CLI not installed. To use this mode:\n" +
				"   1. Install the Google Cloud SDK: https://cloud.google.com/sdk/docs/install\n" +
				"   2. Run: gcloud auth application-default login\n" +
				"   3. Then re-run /configure gsuite and pick gcloud again.\n\n" +
				"💡 Easier path: re-run /configure gsuite and pick 'oauth' instead — it doesn't need the CLI.", true
		}
		// Pre-check: is ADC already done? Quick token-fetch — if it
		// succeeds, the operator is good to go.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := exec.CommandContext(ctx, "gcloud", "auth", "application-default", "print-access-token").Output(); err == nil {
			return "✓ gcloud ADC already set up — proceeding to probe.", false
		}
		// ADC not done. Print the exact command — non-blocking (the
		// probe will surface a useful error if they probe before
		// running the command).
		return "⚠ gcloud is installed but ADC isn't set up yet. Run this in another terminal:\n" +
			"    gcloud auth application-default login\n" +
			"This opens a browser to authorize. Once it says 'Credentials saved', /skill probe gsuite will succeed.", false

	case "sa":
		return "→ next: provide your service-account JSON path, then the email of the user to impersonate.\n" +
			"Requires Google Workspace admin access — Domain-Wide Delegation must be authorized in admin.google.com → Security → API controls.\n" +
			"💡 Don't have admin access? Cancel (/cancel) and re-run /configure gsuite with 'oauth' — it works with your own Google account.", false

	case "oauth":
		// FAST PATH: when maintainer-owned embedded creds exist under
		// cfgDir, skip the Cloud Console walkthrough — the operator
		// just authorizes their Google account in a browser. Zero
		// Cloud setup. The oauth_creds_json field is also hidden via
		// adaptGSuiteForCfgDir.
		if cfgDir != "" && gsuite.HasEmbeddedOAuth(cfgDir) {
			return "→ Signing you in with Google. A browser will open in a moment — pick the account you want to use (works with @gmail.com or Workspace accounts) and click 'Allow'.\n" +
				"If you see a 'Google hasn't verified this app' warning, that's expected for an internally-distributed app: click 'Advanced' → 'Go to tenant (unsafe)'. Your data stays between you and Google.", false
		}
		// SLOW PATH: no embedded creds. ABORT the session cleanly
		// rather than redirecting the operator to Cloud Console (the
		// actual complaint in the field). Google REQUIRES some kind of
		// pre-registered OAuth client to access Gmail/Calendar/Drive —
		// there's no zero-setup path that works for these scopes
		// without either (a) a verified maintainer-published client
		// shipped with the binary, or (b) a proxy service like
		// Composio. Don't auto-open Cloud Console; tell the operator
		// honestly that setup is needed and let them pick their path.
		return "✗ This binary doesn't have a pre-built OAuth client for Gmail/Calendar/Drive yet.\n\n" +
			"Google requires every app accessing those APIs to register one — there's no honest zero-setup path. Pick your move:\n\n" +
			"  • If you maintain this build: run `tenant oauth-setup gsuite` (no args) for an interactive walkthrough that creates the OAuth client AND installs it for all future users. One-time, ~5 min in Google Cloud Console.\n\n" +
			"  • If you're an end user: ask whoever shipped you this binary to do the maintainer step above. After they do, your next `/configure gsuite` skips all setup and goes straight to sign-in.\n\n" +
			"  • If you want to try alternative paths (e.g. Composio proxy, which does removes the Cloud Console requirement at the cost of a third-party dep): open an issue or check docs/SETUP-GSUITE-OAUTH.md.\n\n" +
			"Aborting now — I won't redirect you to Cloud Console without your consent.", true
	}
	return "", false
}

// openBrowser opens a URL in the system default browser. Best-effort:
// failure is silent (the NoteAfter message already gives the operator
// a copy-pasteable command, so a browser-open failure doesn't break
// the flow).
func openBrowser(rawURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	case "darwin":
		cmd = exec.Command("open", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	return cmd.Start() // detached; don't wait
}

// adaptGSuiteForCfgDir mutates the gsuite catalog entry so it picks up
// runtime state from the operator's tenant config directory. Two
// adaptations:
//
//  1. When the maintainer has dropped embedded OAuth credentials at
//     <cfgDir>/oauth_client.json, the oauth_creds_json field is HIDDEN
//     in the interactive flow — operators just pick "oauth" and the
//     probe uses the embedded creds. Zero Cloud Console for the user.
//  2. The auth field's NoteAfter closure rebinds with cfgDir so the
//     oauth branch can detect embedded creds and skip the "create an
//     OAuth client" handhold when they exist.
//
// Called once from newSkillCfgControl.
func adaptGSuiteForCfgDir(k skillKind, cfgDir string) skillKind {
	hasEmbedded := gsuite.HasEmbeddedOAuth(cfgDir)
	fields := make([]skillKindField, len(k.Fields))
	copy(fields, k.Fields)
	for i, f := range fields {
		switch f.Key {
		case "oauth_creds_json":
			if hasEmbedded {
				// Hide the field — operator never sees the prompt.
				// (NOTE: ShowIf checks BOTH auth=="oauth" AND no embed.)
				origShowIf := f.ShowIf
				fields[i].ShowIf = func(v map[string]string) bool {
					if origShowIf != nil && !origShowIf(v) {
						return false
					}
					return false // always hide when embedded present
				}
				fields[i].Required = false
			}
		case "auth":
			// Rebind NoteAfter with cfgDir context so the oauth branch
			// can detect embedded creds and skip the Cloud Console
			// walkthrough.
			fields[i].NoteAfter = makeGsuiteAuthNote(cfgDir)
		}
	}
	k.Fields = fields
	// Replace Probe with a closure that injects the cfgDir-aware
	// embedded-creds reader. ONLY do this when the current Probe is
	// the well-known production probeGSuite — tests that pre-install
	// their own Probe (via skillKinds["gsuite"].Probe = ...) keep it.
	if isProductionGSuiteProbe(k.Probe) {
		k.Probe = func(ctx context.Context, c *credentials, s map[string]string, save func() error) (string, error) {
			identity, err := probeGSuiteWith(ctx, c, s, save, gsuiteProbeDeps{
				GetEmbeddedOAuth: func() ([]byte, error) {
					return gsuite.LoadEmbeddedOAuth(cfgDir)
				},
			})
			if err != nil {
				return identity, err
			}
			// TEN-72 follow-up: when the operator just provided an
			// oauth_creds_json path AND no embedded client existed yet,
			// AUTO-INSTALL it at the well-known cfgDir location so
			// future /configure gsuite runs skip the prompt entirely.
			// Saves the operator from having to also run
			// `tenant oauth-setup gsuite` in a separate terminal.
			if path := s["oauth_creds_json"]; path != "" && !gsuite.HasEmbeddedOAuth(cfgDir) {
				if instErr := installGSuiteOAuthSilent(cfgDir, path); instErr == nil {
					identity += " — also installed for future sessions, next /configure gsuite will skip Cloud setup"
				}
				// Install failure is non-fatal — probe already succeeded
				// for THIS session; future sessions just won't get the
				// fast path. Don't fail loud.
			}
			return identity, nil
		}
	}
	return k
}

// isProductionGSuiteProbe reports whether p is the package-level
// probeGSuite function (vs. a test-installed override). Uses reflect-
// based function identity which is the only way to compare Go funcs
// for "same underlying function." Tests can rely on this to install
// their own probe and have it survive adaptGSuiteForCfgDir.
func isProductionGSuiteProbe(p skillProbe) bool {
	if p == nil {
		return false
	}
	return refpkg.ValueOf(p).Pointer() == refpkg.ValueOf(skillProbe(probeGSuite)).Pointer()
}

// init wires gsuite into the production catalog. The only init() in
// this package (audit P2 contract from TEN-64). Per-platform tickets
// each register via this same shape — keeps the catalog discoverable
// (grep for `skillKinds["<id>"]` finds every platform).
func init() {
	skillKinds["gsuite"] = gsuiteSkillKind()
}
