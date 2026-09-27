package main

import (
	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/web"
)

// webPlugin declares the web plugin: the agent browses the live web in a
// Chrome session. Every agent drives its own browser, so web is registered
// per agent (team.go addWebTool), never in the shared stub catalog.
func webPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:        "web",
		Title:     "Web browsing",
		Version:   1,
		Tools:     func() []model.ToolSpec { return web.NewDispatcher(nil, web.Policy{}, "").Tools() },
		Configure: webSkillKind,
		Setup:     8,
		Actions: map[string]string{
			"web_interact": catWeb,         // click / fill / select
			"web_transact": catDestructive, // a purchase- or delete-shaped click
		},
		PerAgent: true,
		Doctor: []doctorCheck{
			// Chrome is detectable when the web plugin will be used.
			{"chrome (if web enabled)", checkChrome},
		},
		CLI: []*cliCommand{
			{Names: []string{"web"}, Section: "plugins", Run: cmdWeb,
				Summary: "the agent browses the live web (read/explore)",
				Rows:    []cmdreg.Row{{Syntax: `web "<task>"`, Desc: "the agent browses the live web (read/explore)"}}},
		},
	}
}
