package main

import (
	"context"
	"flag"
	"fmt"

	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/atlassian"
)

// atlassianPlugin declares Atlassian: Jira search, read and board writes.
func atlassianPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:        "atlassian",
		Flags:     bindAtlassianFlags,
		On:        func(pf *pluginFlags) bool { return atlassianFlagsOf(pf).on },
		Launch:    launchAtlassian,
		Eval:      evalAtlassian,
		Title:     "Atlassian (Jira)",
		Version:   1,
		Tools:     func() []model.ToolSpec { return atlassian.NewDispatcher(nil, atlassian.Policy{}).Tools() },
		Configure: atlassianSkillKind,
		Actions:   map[string]string{"jira_write": catDestructive}, // create, comment, transition
		StubHint:  "run `/configure atlassian` (OAuth sign-in or API token), then `/enable atlassian`",
		Activate:  activateAtlassian,
		CLI: []*cliCommand{
			{Names: []string{"atlassian"}, Section: "connect", Run: cmdAtlassian, Sub: []string{"login"}, Flags: []string{"--site", "--client-id", "--oauth-callback"},
				Summary: "sign in to Atlassian (Jira, Confluence)",
				Rows:    []cmdreg.Row{{Syntax: "atlassian login --site <url> --client-id <id>", Desc: "sign in to Atlassian ($ATLASSIAN_CLIENT_SECRET holds the secret)"}}},
		},
	}
}

// activateAtlassian builds the real Jira plugin from /configure-saved
// settings (TEN-160).
func activateAtlassian(ctx context.Context, e *pluginEnv) (plugin, func(), error) {
	lc, err := loadLaunchConfig(e.c.cfgDir)
	if err != nil {
		return nil, nil, fmt.Errorf("atlassian plugin: load config: %w", err)
	}
	sk, ok := lc.Skills["atlassian"]
	if !ok || sk.Settings == nil || sk.Settings["site"] == "" {
		return nil, nil, fmt.Errorf("atlassian plugin: %w yet — run `/configure atlassian`", errNotConfigured)
	}
	creds, _ := loadCredentials(e.c.cfgDir)
	acfg := atlassian.Config{
		SiteURL:   sk.Settings["site"],
		Project:   sk.Settings["project"],
		TokenPath: atlassianTokenPath(e.c.cfgDir),
	}
	switch sk.Settings["auth"] {
	case "token":
		acfg.Email = sk.Settings["email"]
		acfg.APIToken = creds.Get(skillSecretID("atlassian", "api_token"))
	default: // oauth
		acfg.ClientID = sk.Settings["client_id"]
		acfg.ClientSecret = creds.Get(skillSecretID("atlassian", "client_secret"))
	}
	svc, err := atlassian.Open(ctx, acfg)
	if err != nil {
		return nil, nil, fmt.Errorf("atlassian plugin: %w", err)
	}
	return atlassian.NewDispatcher(svc, atlassian.Policy{AllowWrite: e.gateAllow(atlassianFlagsOf(e.pf).allowWrite), Confirm: e.confirm}), func() {}, nil
}

// atlassianFlags are Atlassian's launch flags.
type atlassianFlags struct {
	on         bool
	site       string
	email      string
	project    string
	allowWrite bool
	clientID   string
	callback   string
}

func atlassianFlagsOf(pf *pluginFlags) *atlassianFlags {
	return flagsOf[atlassianFlags](pf, "atlassian")
}

func bindAtlassianFlags(fs *flag.FlagSet) any {
	f := &atlassianFlags{}
	fs.BoolVar(&f.on, "atlassian", false, "enable Jira tools (Atlassian)")
	fs.StringVar(&f.site, "atlassian-site", "", "Atlassian site URL, e.g. https://you.atlassian.net (Path A)")
	fs.StringVar(&f.email, "atlassian-email", "", "Atlassian account email (Path A; token via $ATLASSIAN_TOKEN)")
	fs.StringVar(&f.project, "atlassian-project", "", "default Jira project key (e.g. TEN)")
	fs.BoolVar(&f.allowWrite, "atlassian-allow-write", false, "permit jira_create/comment/transition (writes to the board)")
	fs.StringVar(&f.clientID, "atlassian-client-id", "", "Atlassian OAuth app client id (Path B; secret via $ATLASSIAN_CLIENT_SECRET, login via `tenant atlassian login`)")
	fs.StringVar(&f.callback, "atlassian-oauth-callback", "", "OAuth callback bind addr (default 127.0.0.1:8765)")
	return f
}

// launchAtlassian builds the Jira plugin from the launch flags.
func launchAtlassian(ctx context.Context, e *pluginEnv) ([]plugin, func(), error) {
	f := atlassianFlagsOf(e.pf)
	if !f.on {
		return nil, nil, nil
	}
	svc, err := atlassian.Open(ctx, atlassian.Config{
		SiteURL:       f.site,
		Email:         f.email,
		Project:       f.project,
		ClientID:      f.clientID,
		TokenPath:     atlassianTokenPath(e.c.cfgDir),
		OAuthCallback: f.callback,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("atlassian plugin: %w", err)
	}
	return []plugin{atlassian.NewDispatcher(svc, atlassian.Policy{AllowWrite: e.gateAllow(f.allowWrite), Confirm: e.confirm})}, nil, nil
}

// evalAtlassian turns Jira on when /configure already set it up (a site and
// auth). The token path needs no browser.
func evalAtlassian(pf *pluginFlags, c *commonFlags, explicit map[string]bool) bool {
	f := atlassianFlagsOf(pf)
	if explicit["atlassian"] || f.on {
		return false
	}
	lc, err := loadLaunchConfig(c.cfgDir)
	if err != nil || lc == nil {
		return false
	}
	sk, ok := lc.Skills["atlassian"]
	if !ok || !sk.Enabled || sk.Settings == nil || sk.Settings["site"] == "" {
		return false
	}
	f.on = true
	f.site = sk.Settings["site"]
	f.project = sk.Settings["project"]
	f.allowWrite = true
	if sk.Settings["auth"] == "token" {
		// Token auth: load email + api_token from config/credentials.
		f.email = sk.Settings["email"]
		if creds, cerr := loadCredentials(c.cfgDir); cerr == nil {
			f.clientID = creds.Get(skillSecretID("atlassian", "client_id"))
		}
	} else {
		// OAuth path: needs client_id from config.
		f.clientID = sk.Settings["client_id"]
	}
	return true
}
