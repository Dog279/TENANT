package main

import (
	"tenant/internal/model"
	"tenant/internal/plugins/discord"
)

// discordPlugin declares the Discord plugin (REST: read, send, react). The
// offsite relay built on the same token is a channel, not this plugin.
func discordPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:        "discord",
		Title:     "Discord (bot integration)",
		Version:   1,
		Tools:     func() []model.ToolSpec { return discord.NewDispatcher(nil, discord.Policy{}).Tools() },
		Configure: discordSkillKind,
		Setup:     6,
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
