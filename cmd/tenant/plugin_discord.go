package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"tenant/internal/model"
	"tenant/internal/plugins/discord"
	"time"
)

// discordPlugin declares the Discord plugin (REST: read, send, react). The
// offsite relay built on the same token is a channel, not this plugin.
func discordPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:           "discord",
		OnConfigured: onDiscordConfigured,
		Connector:    true,
		// Discord is a paste-a-token service.
		Keys:        []keySpec{{CredID: "skill:discord:token", Name: "Discord bot token", Category: "Integration", Required: true, Kind: keyDirect}},
		Flags:       bindDiscordFlags,
		On:          func(pf *pluginFlags) bool { return discordFlagsOf(pf).on },
		ApplyConfig: applyDiscordConfig,
		Grants:      discordGrants,
		Launch:      launchDiscord,
		Title:       "Discord (bot integration)",
		Version:     1,
		Tools:       func() []model.ToolSpec { return discord.NewDispatcher(nil, discord.Policy{}).Tools() },
		Configure:   discordSkillKind,
		SetupOrder:  6,
		// Discord's action ids are prose (plugins/discord/dispatch.go). They
		// once missed the category table and fell through to destructive,
		// which on the host broker is ask-only: a main-agent Discord send
		// prompted an approval nobody was there to answer and auto-denied
		// after 5 minutes. A send is a send. (2026-09-19)
		Actions:  map[string]string{discord.ActionSendMessage: catSend, discord.ActionReact: catSend},
		StubHint: "relaunch with --discord --discord-bot-token=<token>",
		Doctor: []doctorCheck{
			// The Discord bot token is resolvable and reachable when enabled.
			{"discord (if enabled)", checkDiscord},
		},
	}
}

// discordFlags are the Discord plugin's launch flags.
type discordFlags struct {
	on        bool
	token     string
	allowSend bool
}

func discordFlagsOf(pf *pluginFlags) *discordFlags { return flagsOf[discordFlags](pf, "discord") }

func bindDiscordFlags(fs *flag.FlagSet) any {
	f := &discordFlags{}
	fs.BoolVar(&f.on, "discord", false, "enable the Discord plugin (REST: read/send/react)")
	fs.StringVar(&f.token, "discord-bot-token", "", "Discord bot token (or $DISCORD_BOT_TOKEN)")
	fs.BoolVar(&f.allowSend, "discord-allow-send", false, "permit discord_send_message/discord_react (posts publicly as the bot)")
	return f
}

func applyDiscordConfig(pf *pluginFlags, s savedConfig) {
	f := discordFlagsOf(pf)
	if !s.set["discord"] {
		f.on = true
	}
	if !s.set["discord-bot-token"] {
		f.token = firstNonEmpty(f.token, s.secret("token"))
	}
}

// discordGrants: the allow flag ungates the tool and pre-approves the send
// category, so "--discord-allow-send" means what it says on every surface
// instead of only the ones with their own approver (2026-09-19).
func discordGrants(pf *pluginFlags) map[string]string {
	if discordFlagsOf(pf).allowSend {
		return map[string]string{catSend: "--discord-allow-send"}
	}
	return nil
}

// launchDiscord builds the Discord plugin from the launch flags (the token
// from the flag, else $DISCORD_BOT_TOKEN).
func launchDiscord(_ context.Context, e *pluginEnv) ([]plugin, func(), error) {
	f := discordFlagsOf(e.pf)
	if !f.on {
		return nil, nil, nil
	}
	token := f.token
	if token == "" {
		token = os.Getenv("DISCORD_BOT_TOKEN")
	}
	svc, err := discord.Open(discord.Config{Token: token})
	if err != nil {
		return nil, nil, fmt.Errorf("discord plugin: %w", err)
	}
	return []plugin{discord.NewDispatcher(svc, discord.Policy{AllowSend: e.gateAllow(f.allowSend), Confirm: e.confirm})}, nil, nil
}

// checkDiscord verifies the Discord plugin can talk to discord.com when
// it's enabled. Tiers:
//
//	skip   — plugin not configured / not enabled
//	FAIL   — token unresolvable (env var empty, secret missing)
//	WARN   — token present but /users/@me 4xx (likely revoked / wrong token)
//	OK     — /users/@me returns the bot's identity
//
// Best-effort and offline-tolerant: a network error during the probe
// degrades to WARN (we know the token resolved; can't confirm Discord
// is reachable). The plugin still works offline-tolerant via the
// activator pattern at first /enable.
func checkDiscord(ctx context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "discord plugin not configured"}
	}
	dcfg := e.lc.Skills["discord"]
	if dcfg == nil || !dcfg.Enabled {
		return checkResult{Status: statusSkip, Detail: "discord plugin not enabled"}
	}
	// Resolve token. Env var wins over stored secret (matches
	// pluginFlags resolution at toolmux.go).
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		token = e.resolveSecret(skillSecretID("discord", "token"), authCfg{Stored: true})
	}
	if token == "" {
		return checkResult{Status: statusFail,
			Detail: "discord enabled but bot token is missing",
			Fix:    "set $DISCORD_BOT_TOKEN, or run `tenant setup` and re-enter under Discord"}
	}
	// Probe /users/@me with a short timeout — confirms the token is
	// valid AND Discord is reachable. Best-effort: a network error
	// becomes WARN, not FAIL, so an offline diagnostic still passes
	// the meaningful checks.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(probeCtx, "GET", "https://discord.com/api/v10/users/@me", nil)
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/tenant-mcp/tenant, 1.0)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return checkResult{Status: statusWarn,
			Detail: "token present but could not reach discord.com: " + err.Error(),
			Fix:    "if intentional offline, ignore; else check network / firewall"}
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return checkResult{Status: statusFail,
			Detail: "discord returned 401 Unauthorized — token is invalid or revoked",
			Fix:    "regenerate the bot token at https://discord.com/developers/applications and rerun `tenant setup`"}
	}
	if resp.StatusCode >= 400 {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("discord /users/@me returned HTTP %d", resp.StatusCode),
			Fix:    "check the Developer Portal for the bot's status"}
	}
	return checkResult{Status: statusOK, Detail: "bot token valid; discord.com reachable"}
}

// onDiscordConfigured hot-rebuilds the Discord relay with the new token and
// auto-starts it bound to the operator's user ID. It runs only after the
// probe passed (token verified) and only when the relay callback is wired.
// It also persists Relay.Enabled=true + Relay.OperatorID so the relay
// auto-boots on every future launch. A failure is a warning, not a roll-back:
// the token and operator_id are already stored on disk and will be used on
// next launch.
func onDiscordConfigured(sc *skillCfgControl, lc *launchConfig, creds *credentials, out *strings.Builder) {
	if sc.onDiscordConfigured == nil {
		return
	}
	newToken := creds.Get(skillSecretID("discord", "token"))
	operatorID := ""
	if lc.Skills["discord"] != nil {
		operatorID = lc.Skills["discord"].Settings["operator_id"]
	}
	if operatorID != "" {
		lc.Relay.OperatorID = operatorID
		lc.Relay.Enabled = true
		if err := lc.Save(sc.cfgDir); err != nil {
			fmt.Fprintf(out, "  ! could not persist relay config: %v\n", err)
		}
	}
	if err := sc.onDiscordConfigured(newToken, operatorID); err != nil {
		fmt.Fprintf(out, "  ! relay: %v\n", err)
	}
}

// discordRelayToken is the bot token the offsite relay connects with: the
// one /configure discord stored.
func discordRelayToken(cfgDir string) string {
	return resolveSecret(cfgDir, skillSecretID("discord", "token"), authCfg{Stored: true})
}
