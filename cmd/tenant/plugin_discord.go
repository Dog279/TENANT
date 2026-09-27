package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"tenant/internal/model"
	"tenant/internal/plugins/discord"
)

// discordPlugin declares the Discord plugin (REST: read, send, react). The
// offsite relay built on the same token is a channel, not this plugin.
func discordPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:          "discord",
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
