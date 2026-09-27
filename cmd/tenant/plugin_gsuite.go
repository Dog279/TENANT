package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"golang.org/x/oauth2"

	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/gsuite"
)

// gsuitePlugin declares Google Workspace: Gmail, Calendar and Drive.
func gsuitePlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:             "gsuite",
		AdaptConfigure: func(k skillKind, h configureHost) skillKind { return adaptGSuiteForCfgDir(k, h.cfgDir) },
		Connector:      true,
		ConfigSecrets:  func(cfg string) []string { return []string{gsuite.EmbeddedOAuthPath(cfg)} },
		// The service-account key and the OAuth client JSON the operator named;
		// the client JSON holds a client secret.
		PointedSecrets: func(pf *pluginFlags, settings map[string]string) []string {
			return []string{gsuiteFlagsOf(pf).saJSON, settings["sa_json"], settings["oauth_creds_json"]}
		},
		Flags:       bindGSuiteFlags,
		On:          func(pf *pluginFlags) bool { return gsuiteFlagsOf(pf).on },
		ApplyConfig: applyGSuiteConfig,
		ExpandPaths: expandGSuitePaths,
		Grants:      gsuiteGrants,
		Launch:      launchGSuite,
		Title:       "Google Workspace (Gmail, Calendar, Drive)",
		Version:     1,
		Tools:       func() []model.ToolSpec { return gsuite.NewDispatcher(nil, gsuite.Policy{}).Tools() },
		Configure:   gsuiteSkillKind,
		SetupOrder:  3,
		// Every gated gsuite tool (send, calendar and drive writes, trash)
		// raises the one gsuite_send action.
		Actions:  map[string]string{"gsuite_send": catSend},
		StubHint: "run `/configure gsuite` and pick a sign-in method (or relaunch with --gsuite gcloud/SA flags)",
		Activate: activateGSuite,
		Doctor: []doctorCheck{
			// TEN-65: gsuite auth + Gmail reachability when enabled.
			{"gsuite (if enabled)", checkGSuite},
		},
		CLI: []*cliCommand{
			{Names: []string{"gsuite"}, Section: "plugins", Run: cmdGSuite,
				Summary: "the agent uses Gmail + Calendar",
				Rows:    []cmdreg.Row{{Syntax: `gsuite "<task>"`, Desc: "the agent uses Gmail + Calendar (read; --allow-send to act)"}}},
			{Names: []string{"oauth-setup"}, Section: "connect", Run: cmdOAuthSetup, Sub: []string{"gsuite"},
				Summary: "set up an OAuth client for Google Workspace",
				Rows:    []cmdreg.Row{{Syntax: "oauth-setup gsuite", Desc: "set up the Google Workspace OAuth client (see docs/SETUP-GSUITE-OAUTH.md)"}}},
		},
	}
}

// activateGSuite builds the real dispatcher from the /configure-saved
// settings (launchConfig.Skills["gsuite"]) when the operator enables gsuite
// at runtime. Supports all three auth modes: sa (the business primary),
// gcloud (dev), and oauth (advanced personal).
func activateGSuite(_ context.Context, e *pluginEnv) (plugin, func(), error) {
	lc, err := loadLaunchConfig(e.c.cfgDir)
	if err != nil {
		return nil, nil, fmt.Errorf("gsuite plugin: load config: %w", err)
	}
	sk, ok := lc.Skills["gsuite"]
	if !ok || sk.Settings == nil || sk.Settings["auth"] == "" {
		return nil, nil, fmt.Errorf("gsuite plugin: %w yet — run `/configure gsuite`", errNotConfigured)
	}
	auth := sk.Settings["auth"]
	gcfg := gsuite.Config{Auth: auth, AllowSend: gsuiteFlagsOf(e.pf).allowSend}
	switch auth {
	case "sa":
		saPath := sk.Settings["sa_json"]
		subject := sk.Settings["subject"]
		if saPath == "" || subject == "" {
			return nil, nil, fmt.Errorf("gsuite plugin: auth=sa needs sa_json + subject — re-run `/configure gsuite`")
		}
		b, err := os.ReadFile(saPath)
		if err != nil {
			return nil, nil, fmt.Errorf("gsuite plugin: read sa json: %w", err)
		}
		gcfg.SAJSON = b
		gcfg.Subject = subject
	case "gcloud":
		// nothing extra — Open() shells out to gcloud.
	case "oauth":
		if err := gsuiteRuntimeOAuth(&gcfg, e.c.cfgDir, sk.Settings); err != nil {
			return nil, nil, fmt.Errorf("gsuite plugin: %w", err)
		}
	default:
		// Reached when config.json carries an auth this build doesn't
		// implement (e.g. a stale "composio"). Make it actionable rather
		// than just "unknown".
		return nil, nil, fmt.Errorf("gsuite plugin: auth %q isn't supported in this build — run `/configure gsuite` and pick sa, gcloud, or oauth", auth)
	}
	svc, err := gsuite.Open(gcfg)
	if err != nil {
		return nil, nil, fmt.Errorf("gsuite plugin: %w", err)
	}
	return gsuite.NewDispatcher(svc, gsuite.Policy{
		AllowSend: e.gateAllow(gsuiteFlagsOf(e.pf).allowSend),
		Confirm:   e.confirm,
	}), nil, nil
}

// gsuiteFlags are Google Workspace's launch flags.
type gsuiteFlags struct {
	on        bool
	auth      string
	saJSON    string
	subject   string
	allowSend bool
}

func gsuiteFlagsOf(pf *pluginFlags) *gsuiteFlags { return flagsOf[gsuiteFlags](pf, "gsuite") }

func bindGSuiteFlags(fs *flag.FlagSet) any {
	f := &gsuiteFlags{}
	fs.BoolVar(&f.on, "gsuite", false, "enable Gmail + Calendar")
	fs.StringVar(&f.auth, "gsuite-auth", "gcloud", "gsuite auth: gcloud|sa")
	fs.StringVar(&f.saJSON, "gsuite-sa-json", "", "gsuite service-account JSON (auth=sa)")
	fs.StringVar(&f.subject, "gsuite-subject", "", "gsuite impersonated user (auth=sa)")
	fs.BoolVar(&f.allowSend, "gsuite-allow-send", false, "permit gmail_send/calendar_create")
	return f
}

func applyGSuiteConfig(pf *pluginFlags, s savedConfig) {
	f := gsuiteFlagsOf(pf)
	if !s.set["gsuite"] {
		f.on = true
	}
	if !s.set["gsuite-auth"] && s.get("auth") != "" {
		f.auth = s.get("auth")
	}
	if !s.set["gsuite-sa-json"] {
		f.saJSON = firstNonEmpty(f.saJSON, s.get("sa_json"))
	}
	if !s.set["gsuite-subject"] {
		f.subject = firstNonEmpty(f.subject, s.get("subject"))
	}
}

func expandGSuitePaths(pf *pluginFlags) { f := gsuiteFlagsOf(pf); f.saJSON = expandPath(f.saJSON) }

func gsuiteGrants(pf *pluginFlags) map[string]string {
	if gsuiteFlagsOf(pf).allowSend {
		return map[string]string{catSend: "--gsuite-allow-send"}
	}
	return nil
}

// launchGSuite builds Google Workspace from the launch flags (gcloud or a
// service account).
func launchGSuite(_ context.Context, e *pluginEnv) ([]plugin, func(), error) {
	f := gsuiteFlagsOf(e.pf)
	if !f.on {
		return nil, nil, nil
	}
	gcfg := gsuite.Config{Auth: f.auth, Subject: f.subject, AllowSend: f.allowSend}
	if f.auth == "sa" {
		if f.saJSON == "" {
			return nil, nil, fmt.Errorf("gsuite plugin: --gsuite-auth sa needs --gsuite-sa-json")
		}
		b, err := os.ReadFile(f.saJSON)
		if err != nil {
			return nil, nil, fmt.Errorf("gsuite plugin: read sa json: %w", err)
		}
		gcfg.SAJSON = b
	}
	if f.auth == "oauth" {
		// OAuth has no launch flags of its own: it signs in with what
		// /configure saved, as the activator does.
		var settings map[string]string
		if lc, err := loadLaunchConfig(e.c.cfgDir); err == nil && lc.Skills["gsuite"] != nil {
			settings = lc.Skills["gsuite"].Settings
		}
		if err := gsuiteRuntimeOAuth(&gcfg, e.c.cfgDir, settings); err != nil {
			return nil, nil, fmt.Errorf("gsuite plugin: %w", err)
		}
	}
	svc, err := gsuite.Open(gcfg)
	if err != nil {
		return nil, nil, fmt.Errorf("gsuite plugin: %w", err)
	}
	return []plugin{gsuite.NewDispatcher(svc, gsuite.Policy{AllowSend: e.gateAllow(f.allowSend), Confirm: e.confirm})}, nil, nil
}

// gsuiteRuntimeOAuth fills cfg for a running plugin signed in with OAuth. It
// uses the same client and cached token as the probe. A refreshed token is
// saved back to credentials.json, which is re-read first so a secret stored
// since the plugin started survives. Before this, the launch had no OAuth
// branch, so a saved OAuth setup failed the launch. The activator read only
// oauth_creds_json: /enable failed with the installed client, and every
// sign-in ignored the cached token (D1).
func gsuiteRuntimeOAuth(cfg *gsuite.Config, cfgDir string, settings map[string]string) error {
	client, err := gsuiteOAuthClient(settings, func() ([]byte, error) { return gsuite.LoadEmbeddedOAuth(cfgDir) })
	if err != nil {
		return err
	}
	creds, _ := loadCredentials(cfgDir)
	cfg.OAuthCreds = client
	cfg.OAuthToken = gsuiteCachedToken(creds)
	cfg.OAuthSave = func(t *oauth2.Token) error {
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		c, err := loadCredentials(cfgDir)
		if err != nil {
			return err
		}
		c.Set(skillSecretID("gsuite", "oauth_token"), string(b))
		return c.Save(cfgDir)
	}
	return nil
}

// checkGSuite verifies the gsuite plugin's stored auth config can mint
// a token against Google. TEN-65: mirrors checkDiscord's tier shape.
//
//	skip   — gsuite not configured / not enabled
//	FAIL   — config invalid (auth mode unknown, sa_json missing/invalid,
//	         subject missing for sa mode)
//	WARN   — config valid but probe couldn't reach Google (offline tolerant)
//	OK     — Gmail.Search returns successfully (token mints + Gmail responds)
//
// Reuses probeGSuite from skillctl_gsuite.go so the doctor check and
// the `/skill probe gsuite` command share a code path. 5s timeout
// enforced via runProbe.
func checkGSuite(_ context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "gsuite plugin not configured"}
	}
	gcfg := e.lc.Skills["gsuite"]
	if gcfg == nil || !gcfg.Enabled {
		return checkResult{Status: statusSkip, Detail: "gsuite plugin not enabled"}
	}
	settings := gcfg.Settings
	if settings == nil {
		settings = map[string]string{}
	}
	// Don't load credentials separately — gsuite has no credentials.json
	// secrets (auth/sa_json/subject are all non-secret settings).
	creds, _ := e.readCredentials()

	auth := settings["auth"]
	if auth == "" {
		auth = "sa" // catalog default — business via SA + DWD is primary
	}
	switch auth {
	case "gcloud", "sa", "oauth":
		// fall through
	default:
		return checkResult{Status: statusFail,
			Detail: fmt.Sprintf("gsuite auth mode %q invalid (expected oauth, gcloud, or sa)", auth),
			Fix:    "run `/configure gsuite` and pick a sign-in method"}
	}
	if auth == "sa" {
		if settings["sa_json"] == "" || settings["subject"] == "" {
			return checkResult{Status: statusFail,
				Detail: "gsuite auth=sa requires sa_json + subject",
				Fix:    "run `/configure gsuite auth=sa sa_json=/path/to/key.json subject=user@example.com`"}
		}
		// Operator note (DWD scopes): write posture (--allow-send) now needs
		// gmail.modify+gmail.send, full calendar, and full drive. The
		// domain-wide-delegation client in the Admin console pins an explicit
		// scope allow-list — if it only lists the readonly scopes, every
		// write call fails with "unauthorized_client". Admin must add the
		// write scopes there. We can't detect that here without a live write.
	}
	if auth == "oauth" {
		// Need EITHER an operator-supplied creds path OR embedded creds
		// available (via go:embed compiled-in, or runtime file under
		// cfgDir from `tenant oauth-setup gsuite ...`). Caught here so
		// the probe doesn't fail with a confusing "no OAuth creds" later.
		hasOperatorCreds := settings["oauth_creds_json"] != ""
		hasEmbedded := gsuite.HasEmbeddedOAuth(e.c.cfgDir)
		if !hasOperatorCreds && !hasEmbedded {
			return checkResult{Status: statusFail,
				Detail: "gsuite auth=oauth needs an OAuth client (none configured)",
				Fix: "if you maintain this build: run `tenant oauth-setup gsuite <path-to-client.json>` (see docs/SETUP-GSUITE-OAUTH.md). " +
					"Otherwise: run `/configure gsuite` and provide an OAuth client JSON path, OR pick a different auth mode."}
		}
		// Operator note (scope migration): wiring in the official clients
		// broadened scopes (Drive read/write, gmail.modify, full calendar).
		// A token minted BEFORE the upgrade lacks them, so those calls fail
		// with "Insufficient Permission" until the user re-consents (the
		// browser flow re-runs once the cached token is cleared). We don't
		// detect this here (would need a tokeninfo round-trip per run).
	}

	// runProbe enforces the 5s timeout + goroutine-safe wrapping.
	identity, err := runProbe(skillKinds["gsuite"], creds, settings)
	if err != nil {
		return checkResult{Status: statusWarn,
			Detail: "gsuite config looks valid but probe failed: " + err.Error(),
			Fix:    "if intentional offline, ignore; else verify auth setup with `/skill probe gsuite`"}
	}
	return checkResult{Status: statusOK, Detail: "gsuite reachable — " + identity}
}
