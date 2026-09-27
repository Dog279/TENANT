package main

import (
	"context"
	"fmt"

	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/atlassian"
)

// atlassianPlugin declares Atlassian: Jira search, read and board writes.
func atlassianPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:        "atlassian",
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
	return atlassian.NewDispatcher(svc, atlassian.Policy{AllowWrite: e.gateAllow(e.pf.atlassianAllowWrite), Confirm: e.confirm}), func() {}, nil
}
