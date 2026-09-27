package main

import (
	"context"
	"fmt"
	"os"

	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/gsuite"
)

// gsuitePlugin declares Google Workspace: Gmail, Calendar and Drive.
func gsuitePlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:        "gsuite",
		Title:     "Google Workspace (Gmail, Calendar, Drive)",
		Version:   1,
		Tools:     func() []model.ToolSpec { return gsuite.NewDispatcher(nil, gsuite.Policy{}).Tools() },
		Configure: gsuiteSkillKind,
		Setup:     3,
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
	gcfg := gsuite.Config{Auth: auth, AllowSend: e.pf.gsuiteAllowSend}
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
		credsPath := sk.Settings["oauth_creds_json"]
		if credsPath == "" {
			return nil, nil, fmt.Errorf("gsuite plugin: auth=oauth needs oauth_creds_json — re-run `/configure gsuite`")
		}
		oauthCredsBytes, err := os.ReadFile(credsPath)
		if err != nil {
			return nil, nil, fmt.Errorf("gsuite plugin: read oauth creds: %w", err)
		}
		gcfg.OAuthCreds = oauthCredsBytes
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
		AllowSend: e.gateAllow(e.pf.gsuiteAllowSend),
		Confirm:   e.confirm,
	}), nil, nil
}
