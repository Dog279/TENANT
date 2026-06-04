package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// skillField is one prompt in a skill's setup. Secret fields are stored in
// credentials.json (0600); the rest are non-secret settings in config.json.
type skillField struct {
	Key    string
	Prompt string
	Secret bool
}

// skillSpec describes a configurable integration the wizard can set up. These
// map onto the plugin flags in bindPluginFlags; applyPluginConfig wires the
// saved values back into pluginFlags at launch.
type skillSpec struct {
	ID     string
	Label  string
	Fields []skillField
	Note   string // shown after enabling — e.g. OAuth instructions
}

// skillSpecs is the catalog the wizard offers, in display order.
var skillSpecs = []skillSpec{
	{ID: "wiki", Label: "Wiki (markdown knowledge base)", Fields: []skillField{
		{Key: "dir", Prompt: "Wiki markdown directory"},
	}},
	{ID: "sql", Label: "SQL (SQLite database)", Fields: []skillField{
		{Key: "db", Prompt: "SQLite database file path"},
	}},
	{ID: "gsuite", Label: "Google Workspace (Gmail + Calendar + Drive) — business via SA + DWD", Fields: []skillField{
		{Key: "auth", Prompt: "Auth mode (sa|gcloud|oauth)"},
		{Key: "sa_json", Prompt: "Service-account JSON path (sa only — IT admin creates in console.cloud.google.com)"},
		{Key: "subject", Prompt: "Impersonated user email (sa only — the Workspace user to act as)"},
		{Key: "oauth_creds_json", Prompt: "OAuth client JSON path (oauth only — Desktop App from console.cloud.google.com)"},
	}, Note: "BUSINESS deployments: use auth=sa with Domain-Wide Delegation authorized in admin.google.com. " +
		"DEV machine: auth=gcloud reuses your `gcloud auth application-default login` session. " +
		"ADVANCED (personal @gmail): auth=oauth with your own Desktop App OAuth client."},
	{ID: "x", Label: "X / Twitter", Fields: []skillField{
		{Key: "bearer", Prompt: "X app bearer token", Secret: true},
	}, Note: "No bearer token? Run `tenant x --login` for cookie-based auth instead."},
	{ID: "imessage", Label: "iMessage (via BlueBubbles)", Fields: []skillField{
		{Key: "url", Prompt: "BlueBubbles server URL"},
		{Key: "password", Prompt: "BlueBubbles password", Secret: true},
	}},
	{ID: "discord", Label: "Discord (bot integration — REST: read/send/react)", Fields: []skillField{
		{Key: "token", Prompt: "Discord bot token", Secret: true},
	}, Note: "Create the bot at https://discord.com/developers/applications, then invite to a server with the `bot` scope. " +
		"Surface A (REST tools) only — inbound DMs/channel messages → agent are NOT supported in this build."},
	{ID: "web", Label: "Web browsing (drives Chrome)", Fields: nil},
	{ID: "os", Label: "OS access (sysinfo, files, gated shell)", Fields: nil},
}

// configureSkills runs the interactive skills step: for each integration, ask
// whether to enable it and capture its settings/secrets.
func configureSkills(in *bufio.Reader, cur map[string]*skillConfig, creds *credentials, cfgDir string) map[string]*skillConfig {
	if cur == nil {
		cur = map[string]*skillConfig{}
	}
	fmt.Fprintln(os.Stderr, "\nSkills / integrations (Enter to skip each):")
	for _, sp := range skillSpecs {
		existing := cur[sp.ID]
		def := "n"
		if existing != nil && existing.Enabled {
			def = "y"
		}
		if !yes(ask(in, fmt.Sprintf("Enable %s? [y/n]", sp.Label), def)) {
			if existing != nil {
				existing.Enabled = false
			}
			continue
		}
		sc := existing
		if sc == nil {
			sc = &skillConfig{Settings: map[string]string{}}
		}
		if sc.Settings == nil {
			sc.Settings = map[string]string{}
		}
		sc.Enabled = true
		for _, f := range sp.Fields {
			if f.Secret {
				curVal := ""
				if creds.get(skillSecretID(sp.ID, f.Key)) != "" {
					curVal = "(keep saved)"
				}
				v := ask(in, "  "+f.Prompt, curVal)
				if v != "" && v != "(keep saved)" {
					creds.set(skillSecretID(sp.ID, f.Key), v)
				}
			} else {
				sc.Settings[f.Key] = strings.TrimSpace(ask(in, "  "+f.Prompt, sc.Settings[f.Key]))
			}
		}
		if sp.Note != "" {
			fmt.Fprintf(os.Stderr, "    → %s\n", sp.Note)
		}
		cur[sp.ID] = sc
	}
	return cur
}

func skillSecretID(skill, field string) string { return "skill:" + skill + ":" + field }

func yes(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes", "true", "1":
		return true
	}
	return false
}

// applyPluginConfig fills pluginFlags from the saved skills config for any
// flag the operator did NOT pass explicitly — so `tenant tui` honors what
// `tenant setup` configured, without re-typing plugin flags. Explicit flags
// always win (checked via the shared FlagSet's Visit set).
func applyPluginConfig(c *commonFlags, pf *pluginFlags) {
	if c == nil || c.lc == nil || len(c.lc.Skills) == 0 {
		return
	}
	set := c.flagSet()
	get := func(skill, key string) string {
		if s := c.lc.Skills[skill]; s != nil && s.Settings != nil {
			return s.Settings[key]
		}
		return ""
	}
	enabled := func(skill string) bool {
		s := c.lc.Skills[skill]
		return s != nil && s.Enabled
	}
	secret := func(skill, field string) string {
		return resolveSecret(c.cfgDir, skillSecretID(skill, field), authCfg{Stored: true})
	}

	if enabled("wiki") && !set["wiki-dir"] {
		pf.wikiDir = firstNonEmpty(pf.wikiDir, get("wiki", "dir"))
	}
	if enabled("sql") && !set["sql-db"] {
		pf.sqlDB = firstNonEmpty(pf.sqlDB, get("sql", "db"))
	}
	if enabled("gsuite") {
		if !set["gsuite"] {
			pf.gsuite = true
		}
		if !set["gsuite-auth"] && get("gsuite", "auth") != "" {
			pf.gsuiteAuth = get("gsuite", "auth")
		}
		if !set["gsuite-sa-json"] {
			pf.gsuiteSAJSON = firstNonEmpty(pf.gsuiteSAJSON, get("gsuite", "sa_json"))
		}
		if !set["gsuite-subject"] {
			pf.gsuiteSubject = firstNonEmpty(pf.gsuiteSubject, get("gsuite", "subject"))
		}
	}
	if enabled("x") {
		if !set["x"] {
			pf.x = true
		}
		if !set["x-bearer"] {
			pf.xBearer = firstNonEmpty(pf.xBearer, secret("x", "bearer"))
		}
	}
	if enabled("imessage") {
		if !set["imessage"] {
			pf.imsg = true
		}
		if !set["bb-url"] {
			pf.imsgURL = firstNonEmpty(pf.imsgURL, get("imessage", "url"))
		}
		if !set["bb-password"] {
			pf.imsgPass = firstNonEmpty(pf.imsgPass, secret("imessage", "password"))
		}
	}
	if enabled("discord") {
		if !set["discord"] {
			pf.discord = true
		}
		if !set["discord-bot-token"] {
			pf.discordToken = firstNonEmpty(pf.discordToken, secret("discord", "token"))
		}
	}
	if enabled("web") && !set["web"] {
		pf.web = true
	}
	if enabled("os") && !set["os"] {
		pf.osEnable = true
	}
}
