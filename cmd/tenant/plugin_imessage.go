package main

import (
	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/imessage"
)

// imessagePlugin declares iMessage: native chat.db on macOS, or a BlueBubbles
// server. Configured by flags (--imessage, --bb-url) or `tenant setup`.
func imessagePlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:      "imessage",
		Title:   "iMessage (via BlueBubbles)",
		Version: 1,
		Tools:   func() []model.ToolSpec { return imessage.NewDispatcher(nil, imessage.Policy{}).Tools() },
		Configure: func() skillKind {
			return skillKind{ID: "imessage", Label: "iMessage (via BlueBubbles)", Wired: true, Fields: []skillKindField{
				{Key: "url", Prompt: "BlueBubbles server URL"},
				{Key: "password", Prompt: "BlueBubbles password", Secret: true},
			}}
		},
		SetupOnly: true,
		Setup:     5,
		// imessage_send and imessage_new_chat both raise imessage_send.
		Actions:  map[string]string{"imessage_send": catSend},
		StubHint: "relaunch with --imessage (native chat.db on macOS; or --bb-url for BlueBubbles)",
		Doctor: []doctorCheck{
			// TEN-68: iMessage transport readiness. Native (default on macOS)
			// reads chat.db — probe Full Disk Access by opening it read-only so
			// the FDA requirement surfaces before a live read/send. A
			// configured BlueBubbles URL is noted instead (server bridge).
			{"imessage (if configured)", checkIMessage},
		},
		CLI: []*cliCommand{
			{Names: []string{"imessage"}, Section: "plugins", Run: cmdIMessage,
				Summary: "the agent uses iMessage via BlueBubbles",
				Rows:    []cmdreg.Row{{Syntax: `imessage "<task>"`, Desc: "the agent uses iMessage via BlueBubbles (read; --allow-send to act)"}}},
		},
	}
}
