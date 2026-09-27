package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/gsuite"
)

// gsuitePlugin declares Google Workspace: Gmail, Calendar and Drive.
func gsuitePlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:          "gsuite",
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
	svc, err := gsuite.Open(gcfg)
	if err != nil {
		return nil, nil, fmt.Errorf("gsuite plugin: %w", err)
	}
	return []plugin{gsuite.NewDispatcher(svc, gsuite.Policy{AllowSend: e.gateAllow(f.allowSend), Confirm: e.confirm})}, nil, nil
}
