package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/imessage"
)

// imessagePlugin declares iMessage: native chat.db on macOS, or a BlueBubbles
// server. Configured by flags (--imessage, --bb-url) or `tenant setup`.
func imessagePlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:          "imessage",
		Connector:   true,
		Flags:       bindIMessageFlags,
		On:          func(pf *pluginFlags) bool { return imessageFlagsOf(pf).on },
		ApplyConfig: applyIMessageConfig,
		Grants:      imessageGrants,
		Launch:      launchIMessage,
		Relaunch:    true, // config edits apply on /disable → /enable (TEN-483)
		Title:       "iMessage",
		Version:     1,
		Tools:       func() []model.ToolSpec { return imessage.NewDispatcher(nil, imessage.Policy{}).Tools() },
		Configure: func() skillKind {
			return skillKind{ID: "imessage", Label: "iMessage", Wired: true, Fields: []skillKindField{
				{Key: "url", Prompt: "BlueBubbles server URL (leave blank on a Mac to read Messages directly)"},
				{Key: "password", Prompt: "BlueBubbles password (blank on a Mac without BlueBubbles)", Secret: true},
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
				Summary: "the agent uses iMessage (Messages on this Mac, or a BlueBubbles server)",
				Rows:    []cmdreg.Row{{Syntax: `imessage "<task>"`, Desc: "the agent uses iMessage: Messages on this Mac, or BlueBubbles with --bb-url (read; --allow-send to act)"}}},
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
	b, _, err := openIMessage(f)
	if err != nil {
		return nil, nil, fmt.Errorf("imessage plugin: %w", err)
	}
	pol := imessage.Policy{AllowSend: e.gateAllow(f.allowSend), Confirm: e.confirm}
	return []plugin{imessage.NewDispatcher(b, pol)}, func() { _ = b.Close() }, nil
}

// openIMessage opens the iMessage transport for the tools, `tenant imessage`
// and the responder alike (TEN-486): BlueBubbles when a URL is set (flag,
// saved config, or $BLUEBUBBLES_URL), else native chat.db. desc names it.
func openIMessage(f *imessageFlags) (b imessage.Backend, desc string, err error) {
	url, pass := f.url, f.pass
	if url == "" {
		url = os.Getenv("BLUEBUBBLES_URL")
	}
	if pass == "" {
		pass = os.Getenv("BLUEBUBBLES_PASSWORD")
	}
	desc = "native (chat.db + AppleScript)"
	if url != "" {
		desc = "BlueBubbles " + url
	}
	b, err = imessage.OpenBackend(imessage.Config{URL: url, Password: pass, PrivateAPI: f.private})
	return b, desc, err
}

// imessageBBURL is the configured BlueBubbles URL, "" meaning native: saved
// config, else $BLUEBUBBLES_URL — openIMessage's rule, for callers that have
// only the config (doctor, the authority surface, the dashboard).
func imessageBBURL(lc *launchConfig) string {
	if lc != nil {
		if ic := lc.Skills["imessage"]; ic != nil {
			if u := strings.TrimSpace(ic.Settings["url"]); u != "" {
				return u
			}
		}
	}
	return os.Getenv("BLUEBUBBLES_URL")
}

// checkIMessage verifies the iMessage transport is usable (TEN-68). The
// native macOS transport reads ~/Library/Messages/chat.db (requires Full
// Disk Access) and sends via osascript (Automation access, prompted on
// first send). Tiers:
//
//	skip — imessage not configured; or native on a non-macOS host; nothing
//	       to probe locally without a BlueBubbles URL
//	OK   — BlueBubbles URL configured (server bridge), OR native chat.db is
//	       readable (FDA granted)
//	WARN — native selected but chat.db can't be read yet (FDA not granted)
//	       or osascript is missing — both come with an actionable fix
//
// The FDA probe opens chat.db read-only, which forces a real read, so a
// macOS TCC denial surfaces here rather than at first use. It's a WARN
// (not FAIL) because a present-but-unconfigured skill shouldn't flip the
// doctor exit code; the fix text is explicit.
func checkIMessage(_ context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "imessage not configured"}
	}
	icfg := e.lc.Skills["imessage"]
	if icfg == nil {
		return checkResult{Status: statusSkip, Detail: "imessage not configured"}
	}
	url := imessageBBURL(e.lc)
	if url != "" {
		return checkResult{Status: statusOK, Detail: "BlueBubbles transport configured (url=" + url + ")"}
	}
	// Native transport is macOS-only.
	if runtime.GOOS != "darwin" {
		return checkResult{Status: statusSkip,
			Detail: "native iMessage is macOS-only; set a BlueBubbles --bb-url to use iMessage on this host"}
	}
	if _, err := exec.LookPath("osascript"); err != nil {
		return checkResult{Status: statusWarn,
			Detail: "osascript not found — iMessage sends will fail",
			Fix:    "osascript ships with macOS at /usr/bin/osascript; verify your PATH / system integrity"}
	}
	nat, err := imessage.OpenNative(imessage.NativeConfig{})
	if err != nil {
		return checkResult{Status: statusWarn,
			Detail: "native transport selected but Messages chat.db is unreadable: " + err.Error(),
			Fix:    "grant Full Disk Access to your terminal (or the tenant binary) in System Settings → Privacy & Security → Full Disk Access, then rerun"}
	}
	_ = nat.Close()
	return checkResult{Status: statusOK,
		Detail: "native chat.db readable (Full Disk Access granted); Automation is prompted on first send"}
}
