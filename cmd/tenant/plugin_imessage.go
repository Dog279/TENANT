package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/imessage"
)

// imessagePlugin declares iMessage: native chat.db on macOS, or a BlueBubbles
// server. Configured by flags (--imessage, --bb-url) or `tenant setup`.
func imessagePlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:          "imessage",
		Flags:       bindIMessageFlags,
		On:          func(pf *pluginFlags) bool { return imessageFlagsOf(pf).on },
		ApplyConfig: applyIMessageConfig,
		Grants:      imessageGrants,
		Launch:      launchIMessage,
		Title:       "iMessage (via BlueBubbles)",
		Version:     1,
		Tools:       func() []model.ToolSpec { return imessage.NewDispatcher(nil, imessage.Policy{}).Tools() },
		Configure: func() skillKind {
			return skillKind{ID: "imessage", Label: "iMessage (via BlueBubbles)", Wired: true, Fields: []skillKindField{
				{Key: "url", Prompt: "BlueBubbles server URL"},
				{Key: "password", Prompt: "BlueBubbles password", Secret: true},
			}}
		},
		SetupOnly:  true,
		SetupOrder: 5,
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

// imessageFlags are iMessage's launch flags.
type imessageFlags struct {
	on        bool
	url       string
	pass      string
	private   bool
	allowSend bool
}

func imessageFlagsOf(pf *pluginFlags) *imessageFlags {
	return flagsOf[imessageFlags](pf, "imessage")
}

func bindIMessageFlags(fs *flag.FlagSet) any {
	f := &imessageFlags{}
	fs.BoolVar(&f.on, "imessage", false, "enable iMessage (native chat.db on macOS; --bb-url switches to BlueBubbles)")
	fs.StringVar(&f.url, "bb-url", "", "BlueBubbles URL (or $BLUEBUBBLES_URL)")
	fs.StringVar(&f.pass, "bb-password", "", "BlueBubbles password (or $BLUEBUBBLES_PASSWORD)")
	fs.BoolVar(&f.private, "bb-private-api", false, "use BlueBubbles private-api send method")
	fs.BoolVar(&f.allowSend, "imessage-allow-send", false, "permit imessage_send/new_chat")
	return f
}

func applyIMessageConfig(pf *pluginFlags, s savedConfig) {
	f := imessageFlagsOf(pf)
	if !s.set["imessage"] {
		f.on = true
	}
	if !s.set["bb-url"] {
		f.url = firstNonEmpty(f.url, s.get("url"))
	}
	if !s.set["bb-password"] {
		f.pass = firstNonEmpty(f.pass, s.secret("password"))
	}
}

func imessageGrants(pf *pluginFlags) map[string]string {
	if imessageFlagsOf(pf).allowSend {
		return map[string]string{catSend: "--imessage-allow-send"}
	}
	return nil
}

// launchIMessage picks the transport: a BlueBubbles URL (flag or env) is an
// explicit opt-in to the server bridge; otherwise the default is the native
// macOS transport (chat.db read + AppleScript send, no server). OpenNative
// returns a "macOS only" error off darwin.
func launchIMessage(_ context.Context, e *pluginEnv) ([]plugin, func(), error) {
	f := imessageFlagsOf(e.pf)
	if !f.on {
		return nil, nil, nil
	}
	url, pass := f.url, f.pass
	if url == "" {
		url = os.Getenv("BLUEBUBBLES_URL")
	}
	if pass == "" {
		pass = os.Getenv("BLUEBUBBLES_PASSWORD")
	}
	pol := imessage.Policy{AllowSend: e.gateAllow(f.allowSend), Confirm: e.confirm}
	if url != "" {
		svc, err := imessage.Open(imessage.Config{URL: url, Password: pass, PrivateAPI: f.private})
		if err != nil {
			return nil, nil, fmt.Errorf("imessage plugin: %w", err)
		}
		return []plugin{imessage.NewDispatcher(svc, pol)}, nil, nil
	}
	nat, err := imessage.OpenNative(imessage.NativeConfig{})
	if err != nil {
		return nil, nil, fmt.Errorf("imessage plugin: %w", err)
	}
	return []plugin{imessage.NewDispatcher(nat, pol)}, func() { _ = nat.Close() }, nil
}
