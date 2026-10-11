package tui

// Session commands: /whoami, /help, /quit, /clear, /cls, /mouse, /dashboard, /tailscale.

import (
	"fmt"
	"strings"
	"tenant/internal/cmdreg"

	tea "github.com/charmbracelet/bubbletea"
)

// slashWhoami runs /whoami, /who.
func slashWhoami(m *model, line string) tea.Cmd {
	// Runtime introspection — answers "what model am I on?" / "which
	// agent am I?" from CONFIG, not from the LLM. The model itself
	// is unreliable at self-identification (it pulls from training
	// data + system prompt context, not runtime). This is the
	// authoritative answer; trust this over the model's answer.
	var b strings.Builder
	fmt.Fprintf(&b, "agent:    %s\n", m.cfg.AgentID)
	fmt.Fprintf(&b, "backend:  %s\n", m.cfg.Backend)
	fmt.Fprintf(&b, "model:    %s\n", m.cfg.Model)
	// Don't render endpoint here — it's not load-bearing for the
	// "who am I" question and we never want it in the chat scroll
	// where it could be Ctrl-Y'd back into a transcript.
	m.sysChat(strings.TrimRight(b.String(), "\n"))
	return nil
}

// slashHelp runs /help, /?.
func slashHelp(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// Cascading help: bare /help shows the index of categories; /help all
	// dumps every command (legacy); /help <category> expands one section.
	sub := strings.TrimSpace(arg)
	switch strings.ToLower(sub) {
	case "":
		m.sysChatStyled(renderHelpIndex())
	case "all", "everything", "full":
		m.sysChatStyled(renderHelp())
	default:
		sec := helpSectionByID(sub)
		if sec == nil {
			m.sysChat(fmt.Sprintf("no help category named %q — type /help to see the categories", sub))
			return nil
		}
		m.sysChatStyled(renderHelpSection(sec))
	}
	return nil
}

// slashQuit runs /quit, /exit.
func slashQuit(m *model, line string) tea.Cmd {
	return tea.Quit
}

// slashClear runs /clear.
func slashClear(m *model, line string) tea.Cmd {
	// Wipe the agent's working context (fresh conversation) AND the screen.
	// Durable memory is preserved. (TEN-181)
	if m.busy {
		m.sysChat("can't clear context mid-turn — let it finish or /cancel first")
		return nil
	}
	if m.cfg.Agent == nil {
		m.sysChat("context clearing is not available in this session")
		return nil
	}
	n := m.cfg.Agent.ClearContext()
	m.clearFeed()
	m.sysChat(fmt.Sprintf("context cleared (%d message(s)) — fresh start. Facts, episodes, and the archive are preserved; recall still works.", n))
	return nil
}

// slashCls runs /cls.
func slashCls(m *model, line string) tea.Cmd {
	// Clear just the screen/scrollback; the agent's context is untouched. (TEN-181)
	m.clearFeed()
	m.sysChat("screen cleared (context kept — use /clear to also reset the agent's context)")
	return nil
}

// slashFeed runs /feed: show or hide the activity pane, and keep the choice
// (TEN-514). No argument toggles.
func slashFeed(m *model, line string) tea.Cmd {
	on := m.feedOff // toggle by default
	switch args := strings.Fields(line)[1:]; strings.Join(args, " ") {
	case "on":
		on = true
	case "off":
		on = false
	case "":
	default:
		m.sysChat("usage: /feed on|off   (Alt+F toggles it too)")
		return nil
	}
	m.setFeed(on, true)
	if on {
		m.sysChat("activity feed shown")
	} else {
		m.sysChat("activity feed hidden — /feed on or Alt+F brings it back")
	}
	return nil
}

// slashMouse runs /mouse.
func slashMouse(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// Toggle mouse wheel-capture. ON (default): wheel scrolls the TUI panes
	// and you stay in the alt screen — to SELECT text, hold the terminal's
	// bypass modifier while dragging (⌥ Option on macOS, Shift on most Linux
	// terminals). OFF: plain drag selects, but the wheel scrolls the
	// terminal's scrollback instead of the TUI. (TEN-181)
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "on":
		m.mouseOn, m.selectMode = true, false
		m.sysChat("🖱 wheel scrolling ON (default). To copy: press Ctrl+S (select mode — plain drag works in any terminal), or your terminal's bypass drag (iTerm2 ⌥-drag, Apple Terminal Fn-drag, Linux Shift-drag).")
		return tea.EnableMouseCellMotion
	case "off":
		m.mouseOn, m.selectMode = false, false
		m.sysChat("🖱 mouse capture OFF — plain drag selects/copies. Note: the wheel now scrolls the TERMINAL, not the TUI (PgUp/PgDn / Shift+↑/↓ still scroll the panes). `/mouse on` to restore.")
		return tea.DisableMouse
	case "", "toggle":
		m.mouseOn = !m.mouseOn
		m.selectMode = false
		if m.mouseOn {
			m.sysChat("🖱 wheel scrolling ON. Copy text with Ctrl+S select mode (or iTerm2 ⌥-drag / Apple Terminal Fn-drag).")
			return tea.EnableMouseCellMotion
		}
		m.sysChat("🖱 mouse capture OFF — plain drag selects. Wheel scrolls the terminal; use PgUp/PgDn for the panes. `/mouse on` to restore.")
		return tea.DisableMouse
	default:
		m.sysChat("usage: /mouse on|off   (on = wheel scrolls the TUI, Ctrl+S to select text; off = plain-drag selection)")
	}
	return nil
}

// slashDashboard runs /dashboard.
func slashDashboard(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// Start/stop the web control panel live; the choice persists.
	if m.cfg.Dash == nil {
		m.sysChat("dashboard not available in this session")
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "", "status":
		if running, addr := m.cfg.Dash.Status(); running {
			hint := ""
			if m.cfg.Tailscale != nil {
				hint = "   ·   /tailscale serve to reach it from your other devices"
			}
			m.sysChat("dashboard: running at http://" + addr + hint)
		} else {
			m.sysChat("dashboard: stopped")
		}
	case "on", "start", "enable":
		addr, err := m.cfg.Dash.Enable()
		if err != nil {
			m.sysChat("dashboard: " + err.Error())
		} else {
			msg := "dashboard: serving at http://" + addr
			if d, ok := m.cfg.Dash.(interface{ AuthToken() string }); ok && d.AuthToken() != "" {
				msg += "\ndashboard auth token: " + d.AuthToken()
			}
			m.sysChat(msg)
		}
	case "off", "stop", "disable":
		if err := m.cfg.Dash.Disable(); err != nil {
			m.sysChat("dashboard: " + err.Error())
		} else {
			m.sysChat("dashboard: stopped")
		}
	default:
		m.sysChat("usage: /dashboard [status|on|off]")
	}
	return nil
}

// slashTailscale runs /tailscale, /ts.
func slashTailscale(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// Publish the loopback dashboard onto the tailnet via `tailscale serve`
	// (TEN-233). Reverse proxy — the dashboard bind stays 127.0.0.1.
	if m.cfg.Tailscale == nil {
		m.sysChat("tailscale control not available in this session")
		return nil
	}
	fields := strings.Fields(arg)
	sub := ""
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
	}
	serveOff := sub == "serve" && len(fields) > 1 && strings.ToLower(fields[1]) == "off"
	switch {
	case sub == "" || sub == "status":
		st, _ := m.cfg.Tailscale.Status()
		switch {
		case !st.Installed:
			m.sysChat("tailscale: CLI not found on this machine — install Tailscale, then /tailscale serve")
		case !st.LoggedIn:
			state := st.State
			if strings.TrimSpace(state) == "" {
				state = "unknown"
			}
			m.sysChat("tailscale: installed but not connected (state: " + state + ") — run `tailscale up` or log in via the app, then /tailscale serve")
		case st.Serving:
			m.sysChat("tailscale: connected as " + st.DNSName + " · dashboard published at " + st.URL + "  (/tailscale serve off to stop)")
		default:
			m.sysChat("tailscale: connected as " + st.DNSName + " · not publishing — run /tailscale serve to reach the dashboard from your tailnet")
		}
	case serveOff || sub == "off" || sub == "stop" || sub == "reset":
		if err := m.cfg.Tailscale.Unserve(); err != nil {
			m.sysChat("tailscale: " + err.Error())
		} else {
			m.sysChat("tailscale: serve stopped — the dashboard is no longer published to your tailnet")
		}
	case sub == "serve" || sub == "on" || sub == "start":
		url, err := m.cfg.Tailscale.Serve()
		if err != nil {
			m.sysChat("tailscale: " + err.Error())
			return nil
		}
		msg := "🔗 dashboard published to your tailnet: " + url +
			"\n   use the dashboard auth token printed at launch to sign in. /tailscale serve off to stop."
		if m.cfg.Dash != nil {
			if running, _ := m.cfg.Dash.Status(); !running {
				msg += "\n⚠ the dashboard itself is currently stopped — run /dashboard on, or this URL will 502."
			}
		}
		m.sysChat(msg)
	default:
		m.sysChat("usage: /tailscale [status | serve | serve off]")
	}
	return nil
}

// sessionCommands are the session, screen and web-panel commands.
func sessionCommands() []*slashCommand {
	return []*slashCommand{
		{Names: []string{"/image", "/img"}, Section: "session", Run: slashImage,
			Summary: "attach an image file to your next message",
			Rows:    []cmdreg.Row{{Syntax: "/image <path>", Desc: "attach an image file to your next message (or press Ctrl+V to paste one; Alt+V on Windows)"}}},
		{Names: []string{"/whoami", "/who"}, Section: "session", Run: slashWhoami,
			Summary: "show the agent ID, active backend and model (runtime truth — trust over the model's self-answer)"},
		{Names: []string{"/dashboard"}, Section: "session", Run: slashDashboard,
			Summary: "start/stop the web control panel",
			Rows:    []cmdreg.Row{{Syntax: "/dashboard [on|off|status]", Desc: "start/stop the web control panel (auto-launches by default; choice persists)"}}},
		{Names: []string{"/tailscale", "/ts"}, Section: "session", Run: slashTailscale,
			Summary: "publish the dashboard to your tailnet",
			Rows:    []cmdreg.Row{{Syntax: "/tailscale [serve|serve off|status]", Desc: "publish the dashboard to your tailnet over HTTPS via tailscale serve (reach it from other devices)"}}},
		{Names: []string{"/help", "/?"}, Section: "session", Run: slashHelp,
			Summary: "show the categories, one category, or everything",
			Task:    "list the categories; /help <category> opens one",
			Rows: []cmdreg.Row{
				{Syntax: "/help", Desc: "show top-level categories"},
				{Syntax: "/help <category>", Desc: "show commands in one category (e.g. /help agents)"},
				{Syntax: "/help all", Desc: "every command in every category"},
			}},
		{Names: []string{"/quit", "/exit"}, Section: "session", Run: slashQuit,
			Summary: "close the app (the ONLY way out — Ctrl-C/Esc don't quit)",
			Task:    "close the app (Ctrl-C and Esc only stop a turn)",
			Rows:    []cmdreg.Row{{Syntax: "/exit, /quit", Desc: "close the app (the ONLY way out — Ctrl-C/Esc don't quit)"}}},
		{Names: []string{"/clear"}, Section: "session", Run: slashClear,
			Summary: "wipe the agent's context (fresh conversation) + screen; keeps facts/episodes/archive",
			Task:    "start a fresh conversation (memory is kept)"},
		{Names: []string{"/cls"}, Section: "session", Run: slashCls,
			Summary: "clear just the screen/scrollback (context untouched)"},
		{Names: []string{"/mouse"}, Section: "session", Run: slashMouse,
			Summary: "wheel capture on or off",
			Rows:    []cmdreg.Row{{Syntax: "/mouse on|off", Desc: "on (default) = wheel scrolls the TUI; off = plain-drag selection (wheel scrolls terminal)"}}},
		{Names: []string{"/feed"}, Section: "session", Run: slashFeed,
			Summary: "show or hide the activity feed (Alt+F); the choice persists",
			Rows:    []cmdreg.Row{{Syntax: "/feed on|off", Desc: "show or hide the activity feed on the right (Alt+F toggles it too); the choice persists"}}},
	}
}
