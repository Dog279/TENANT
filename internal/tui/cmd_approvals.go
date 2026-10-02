package tui

// Safety and approval commands: /approve, /deny, /permissions, and the offsite surfaces /relay and /imessage.

import (
	"fmt"
	"strings"
	"tenant/internal/cmdreg"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// slashApprove runs /approve, /approve!.
func slashApprove(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	m.resolveApproval(arg)
	return nil
}

// slashDeny runs /deny, /reject.
func slashDeny(m *model, line string) tea.Cmd {
	m.sendDecision(DenyOnce)
	return nil
}

// slashPermissions runs /permissions, /perms.
func slashPermissions(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	if m.cfg.Perms == nil {
		m.sysChat("permissions are not available in this session")
		return nil
	}
	f := strings.Fields(arg)
	switch {
	case len(f) == 0:
		m.sysChatStyled(m.renderPermissions())
	case strings.ToLower(f[0]) == "set" && len(f) >= 3:
		cat, mode := strings.ToLower(f[1]), strings.ToLower(f[2])
		ok, err := m.cfg.Perms.SetPermission(cat, mode)
		switch {
		case err != nil:
			m.sysChat("error: " + err.Error())
		case !ok:
			m.sysChat("no such category " + f[1] + " (see /permissions)")
		default:
			m.sysChat("set " + cat + " → " + mode)
		}
	case (strings.ToLower(f[0]) == "add" || strings.ToLower(f[0]) == "rule") && len(f) >= 3:
		rc, ok := m.cfg.Perms.(ToolRuleControl)
		if !ok {
			m.sysChat("tool rules are not available in this session")
			return nil
		}
		// A command rule has spaces: /permissions add os_exec(brew install *) allow
		// (TEN-491); the mode is the last word, the pattern everything before it.
		pattern, mode := strings.Join(f[1:len(f)-1], " "), strings.ToLower(f[len(f)-1])
		if err := rc.SetRule(pattern, mode); err != nil {
			m.sysChat("error: " + err.Error())
			return nil
		}
		m.sysChat("rule: " + pattern + " → " + mode + "   (checked before the category on every surface; most specific pattern wins)")
	case (strings.ToLower(f[0]) == "rm" || strings.ToLower(f[0]) == "remove" || strings.ToLower(f[0]) == "del") && len(f) >= 2:
		rc, ok := m.cfg.Perms.(ToolRuleControl)
		if !ok {
			m.sysChat("tool rules are not available in this session")
			return nil
		}
		pattern := strings.Join(f[1:], " ") // a command rule has spaces
		removed, err := rc.RemoveRule(pattern)
		switch {
		case err != nil:
			m.sysChat("error: " + err.Error())
		case !removed:
			m.sysChat("no rule " + pattern + " (see /permissions)")
		default:
			m.sysChat("removed rule " + pattern)
		}
	default:
		m.sysChat("usage: /permissions   |   /permissions set <category> <ask|allow|deny>   |   /permissions add <tool|pattern|os_exec(<prefix> *)> <ask|allow|deny>   |   /permissions rm <pattern>")
	}
	return nil
}

// slashRelay runs /relay.
func slashRelay(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// Start/stop the offsite Discord relay live; the choice persists.
	if m.cfg.Relay == nil {
		m.sysChat("discord relay not available in this session")
		return nil
	}
	fields := strings.Fields(arg)
	sub, rest := "", ""
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arg), fields[0]))
	}
	switch sub {
	case "", "status":
		running, opset, execOn := m.cfg.Relay.Status()
		state := "stopped"
		if running {
			state = "running"
		}
		who := "no operator set — /relay allow <id>"
		if opset {
			who = "operator set"
		}
		mode := "exec off: read/research/Discord comms only"
		if execOn {
			mode = "EXEC ON: dangerous tools follow /relay permissions"
		}
		m.sysChat("discord relay: " + state + " (" + who + "; " + mode + ")  ·  approvals auto-deny after " + m.cfg.Relay.ApprovalTimeout().String() + "  ·  /relay permissions for per-category gating")
	case "permissions", "perms", "permission":
		pc := m.cfg.Relay.Perms()
		if pc == nil {
			m.sysChat("relay permissions unavailable (discord not configured — /skill configure discord)")
			return nil
		}
		pf := strings.Fields(rest)
		switch {
		case len(pf) == 0:
			plain, styled := renderPermissionsFor(pc, "Discord relay permissions", "/relay permissions")
			if _, _, execOn := m.cfg.Relay.Status(); !execOn {
				note := "exec mode is off: exec, write, destructive, sandbox and send (Discord posts excepted) are refused whatever these modes say — /relay exec on to unlock"
				plain += note + "\n"
				styled += cErr.Render(note) + "\n"
			}
			m.sysChatStyled(plain, styled)
		case strings.ToLower(pf[0]) == "set" && len(pf) >= 3:
			cat, mode := strings.ToLower(pf[1]), strings.ToLower(pf[2])
			ok, err := pc.SetPermission(cat, mode)
			switch {
			case err != nil:
				m.sysChat("relay: " + err.Error())
			case !ok:
				m.sysChat("no such category " + pf[1] + " (see /relay permissions)")
			default:
				m.sysChat("🔐 relay " + cat + " → " + mode)
			}
		default:
			m.sysChat("usage: /relay permissions   |   /relay permissions set <category> <ask|allow|deny>   (ask = button-approve in Discord)")
		}
	case "allow":
		if rest == "" {
			m.sysChat("usage: /relay allow <your-discord-user-id>")
			return nil
		}
		if err := m.cfg.Relay.SetOperator(rest); err != nil {
			m.sysChat("relay: " + err.Error())
		} else {
			m.sysChat("relay: operator set to " + rest + " — run /relay on to start")
		}
	case "on", "start", "enable":
		if err := m.cfg.Relay.Enable(); err != nil {
			m.sysChat("relay: " + err.Error())
		} else {
			m.sysChat("relay: started — DM your bot from the allowed account")
		}
	case "off", "stop", "disable":
		if err := m.cfg.Relay.Disable(); err != nil {
			m.sysChat("relay: " + err.Error())
		} else {
			m.sysChat("relay: stopped")
		}
	case "exec":
		on, ok := parseOnOff(rest)
		if !ok {
			m.sysChat("usage: /relay exec on|off  (on = unlock dangerous tools offsite, each following /relay permissions)")
			return nil
		}
		if err := m.cfg.Relay.SetExec(on); err != nil {
			m.sysChat("relay: " + err.Error())
		} else if on {
			m.sysChat("relay: EXEC MODE ON — offsite exec/write/destructive/sandbox/send are now reachable, each following /relay permissions (ask = your button approval). /relay exec off to revoke.")
		} else {
			m.sysChat("relay: exec mode off — offsite exec/write/destructive/sandbox/send are refused again (Discord posts still follow /relay permissions)")
		}
	case "timeout":
		d, err := time.ParseDuration(rest)
		if rest == "" || err != nil {
			m.sysChat("usage: /relay timeout <duration>  (e.g. 10m — how long an approval card waits for your tap before auto-denying; currently " + m.cfg.Relay.ApprovalTimeout().String() + ")")
			return nil
		}
		if err := m.cfg.Relay.SetApprovalTimeout(d); err != nil {
			m.sysChat("relay: " + err.Error())
		} else {
			m.sysChat("relay: approval cards now auto-deny after " + d.String())
		}
	default:
		m.sysChat("usage: /relay [status|allow <id>|on|off|exec on|off|timeout <duration>|permissions [set <cat> <mode>]]")
	}
	return nil
}

// slashIMessage runs /imessage, /imsg.
func slashIMessage(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// Manage the deny-by-default allowlist of handles permitted to drive
	// the agent over iMessage. Edits persist. Empty list = nobody.
	if m.cfg.IMessage == nil {
		m.sysChat("imessage allowlist not available in this session")
		return nil
	}
	fields := strings.Fields(arg)
	sub, rest := "", ""
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arg), fields[0]))
	}
	switch sub {
	case "", "list", "status":
		m.sysChatStyled(m.renderIMessageAllow())
	case "allow", "add":
		if rest == "" {
			m.sysChat("usage: /imessage allow <phone-or-email>")
			return nil
		}
		norm, added, err := m.cfg.IMessage.Allow(rest)
		switch {
		case err != nil:
			m.sysChat("imessage: " + err.Error())
		case !added:
			m.sysChat("imessage: " + norm + " is already on the allowlist")
		default:
			m.sysChat("imessage: allowed " + norm + " — may drive the agent over iMessage once inbound is live")
		}
	case "deny", "remove", "rm", "del":
		if rest == "" {
			m.sysChat("usage: /imessage deny <phone-or-email>")
			return nil
		}
		norm, removed, err := m.cfg.IMessage.Deny(rest)
		switch {
		case err != nil:
			m.sysChat("imessage: " + err.Error())
		case !removed:
			m.sysChat("imessage: " + norm + " was not on the allowlist")
		default:
			m.sysChat("imessage: removed " + norm + " from the allowlist")
		}
	case "clear":
		n, err := m.cfg.IMessage.Clear()
		if err != nil {
			m.sysChat("imessage: " + err.Error())
		} else {
			m.sysChat(fmt.Sprintf("imessage: cleared %d handle(s) — allowlist is now empty (deny-by-default: nobody can drive the agent)", n))
		}
	case "on", "enable", "start":
		// Start the autonomous responder live (TEN-230): poll chat.db, reply
		// to allowed handles. Persists so it auto-starts next launch.
		status, err := m.cfg.IMessage.SetResponder(true)
		if err != nil {
			m.sysChat("imessage: " + err.Error())
		} else {
			m.sysChat("📲 " + status)
		}
	case "off", "disable", "stop":
		status, err := m.cfg.IMessage.SetResponder(false)
		if err != nil {
			m.sysChat("imessage: " + err.Error())
		} else {
			m.sysChat("📴 " + status)
		}
	case "permissions", "perms", "permission":
		// Same model + syntax as the global /permissions, scoped to the
		// iMessage responder's gated tools (TEN-230): per-category ask|allow|
		// deny. "ask" prompts the operator at this TUI.
		pc := m.cfg.IMessage.Perms()
		if pc == nil {
			m.sysChat("imessage permissions unavailable here (responder is macOS-only)")
			return nil
		}
		pf := strings.Fields(rest)
		switch {
		case len(pf) == 0:
			plain, styled := renderPermissionsFor(pc, "iMessage permissions", "/imessage permissions")
			m.sysChatStyled(plain, styled)
		case strings.ToLower(pf[0]) == "set" && len(pf) >= 3:
			cat, mode := strings.ToLower(pf[1]), strings.ToLower(pf[2])
			ok, err := pc.SetPermission(cat, mode)
			switch {
			case err != nil:
				m.sysChat("imessage: " + err.Error())
			case !ok:
				m.sysChat("no such category " + pf[1] + " (see /imessage permissions)")
			default:
				m.sysChat("🔐 imessage " + cat + " → " + mode)
			}
		default:
			m.sysChat("usage: /imessage permissions   |   /imessage permissions set <category> <ask|allow|deny>")
		}
	default:
		m.sysChat("usage: /imessage [list | on | off | permissions [set <cat> <mode>] | allow <handle> | deny <handle> | clear]")
	}
	return nil
}

// renderIMessageAllow formats the iMessage drive-allowlist as (plain, styled).
// An empty list is rendered as an explicit deny-by-default notice so the
// operator is never misled into thinking "empty == open."
func (m *model) renderIMessageAllow() (string, string) {
	handles := m.cfg.IMessage.AllowList()
	var p, s strings.Builder
	const title = "iMessage drive-allowlist"
	state := "responder: OFF  (turn on with /imessage on)"
	if m.cfg.IMessage.ResponderOn() {
		state = "responder: ON  (replying to allowed handles; /imessage off to stop)"
	}
	p.WriteString(title + "\n  " + state + "\n")
	s.WriteString(cHeading.Render(title) + "\n  " + cDim.Render(state) + "\n")
	if len(handles) == 0 {
		const empty = "(empty) — deny-by-default: nobody can drive the agent over iMessage."
		const hint = "Add one with /imessage allow <phone-or-email>."
		p.WriteString("  " + empty + "\n  " + hint)
		s.WriteString("  " + cOffMark.Render(empty) + "\n  " + cDim.Render(hint))
		return p.String(), s.String()
	}
	for _, h := range handles {
		p.WriteString("  • " + h + "\n")
		s.WriteString("  " + cOnMark.Render("●") + " " + cName.Render(h) + "\n")
	}
	hint := fmt.Sprintf("%d handle(s) allowed — only these may drive the agent once inbound iMessage is live.", len(handles))
	p.WriteString("  " + hint)
	s.WriteString("  " + cDim.Render(hint))
	return p.String(), s.String()
}

func (m *model) resolveApproval(arg string) {
	var d ApprovalDecision
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "", "once":
		d = ApproveOnce
	case "session":
		d = ApproveSession
	case "always":
		d = ApproveAlways
	default:
		m.sysChat("usage: /approve [session|always]")
		return
	}
	m.sendDecision(d)
}

// renderPermissions builds the /permissions view as (plain, styled).
func (m *model) renderPermissions() (string, string) {
	return renderPermissionsFor(m.cfg.Perms, "Permissions", "/permissions")
}

// renderPermissionsFor renders any PermissionControl's per-category modes — used
// by both /permissions (global) and /imessage permissions (offsite), so they
// look identical (TEN-230).
func renderPermissionsFor(pc PermissionControl, header, cmd string) (string, string) {
	infos := pc.Permissions()
	var p, s strings.Builder
	p.WriteString(header + "  (ask / allow / deny)   ·   " + cmd + " set <category> <mode>\n")
	s.WriteString(cHeading.Render(header) + cDim.Render("  ask / allow / deny   ·   "+cmd+" set <category> <mode>") + "\n")
	anyNew := false
	for _, in := range infos {
		mode := in.Mode
		if in.New { // an update added it and it was never chosen (TEN-343)
			mode += " (new)"
			anyNew = true
		}
		p.WriteString(fmt.Sprintf("  %-12s %-11s  %s\n", in.Category, mode, in.Desc))
		ms := cSys // ask = yellow
		switch in.Mode {
		case "allow":
			ms = cOnMark
		case "deny":
			ms = cErr
		}
		s.WriteString("  " + cKey.Render(fmt.Sprintf("%-12s", in.Category)) + " " +
			ms.Render(fmt.Sprintf("%-11s", mode)) + "  " + cDim.Render(in.Desc) + "\n")
	}
	if anyNew {
		note := "(new) = added by an update and never chosen, so it asks first. " + cmd + " set <category> <mode> to choose."
		p.WriteString("  " + note + "\n")
		s.WriteString("  " + cDim.Render(note) + "\n")
	}
	// Per-tool rules (shared by every surface) — the layer for tools the
	// categories don't know: a connector's tools, an unusual action id.
	if rc, ok := pc.(ToolRuleControl); ok {
		rules := rc.Rules()
		manage := "/permissions add <tool|pattern> <mode> · /permissions rm <pattern>"
		p.WriteString("Tool rules  (checked before the category; most specific wins)   ·   " + manage + "\n")
		s.WriteString(cHeading.Render("Tool rules") + cDim.Render("  checked before the category; most specific wins   ·   "+manage) + "\n")
		if len(rules) == 0 {
			p.WriteString("  (none — a tool the categories don't know prompts every time; add its name to allow, deny, or always ask)\n")
			s.WriteString("  " + cDim.Render("(none — a tool the categories don't know prompts every time; add its name to allow, deny, or always ask)") + "\n")
		}
		for _, r := range rules {
			p.WriteString(fmt.Sprintf("  %-40s %-5s  %s\n", r.Pattern, r.Mode, r.Kind))
			ms := cSys
			switch r.Mode {
			case "allow":
				ms = cOnMark
			case "deny":
				ms = cErr
			}
			s.WriteString("  " + cKey.Render(fmt.Sprintf("%-40s", r.Pattern)) + " " +
				ms.Render(fmt.Sprintf("%-5s", r.Mode)) + "  " + cDim.Render(r.Kind) + "\n")
		}
	}
	return strings.TrimRight(p.String(), "\n"), strings.TrimRight(s.String(), "\n")
}

// approvalCommands are the permission modes, approvals and the offsite
// surfaces' controls.
func approvalCommands() []*slashCommand {
	return []*slashCommand{
		{Names: []string{"/permissions", "/perms"}, Section: "safety", Run: slashPermissions,
			Summary: "per-category ask/allow/deny and per-tool rules",
			Rows: []cmdreg.Row{
				{Syntax: "/permissions", Desc: "view per-category safety (ask/allow/deny) + per-tool rules"},
				{Syntax: "/permissions set <cat> <mode>", Desc: "e.g. /permissions set exec allow"},
				{Syntax: "/permissions add <tool|pattern> <mode>", Desc: "per-tool rule: a name (any connector), an exact id, or a glob — delete_* deny, mcp:host:port:* allow"},
				{Syntax: "/permissions rm <pattern>", Desc: "remove a tool rule"},
			}},
		{Names: []string{"/approve", "/approve!"}, Section: "safety", Run: slashApprove,
			Summary: "approve a paused dangerous action",
			Rows:    []cmdreg.Row{{Syntax: "/approve [session|always]", Desc: "approve a paused dangerous action"}}},
		{Names: []string{"/deny", "/reject"}, Section: "safety", Run: slashDeny,
			Summary: "reject a paused dangerous action"},
		{Names: []string{"/relay"}, Section: "safety", Run: slashRelay,
			Summary: "the offsite Discord relay: status, operator, on/off, exec, timeout, permissions",
			Rows: []cmdreg.Row{
				{Syntax: "/relay [status]", Desc: "whether the Discord relay runs, its operator, and whether exec is on"},
				{Syntax: "/relay on | off", Desc: "start/stop the relay live (the choice persists)"},
				{Syntax: "/relay allow <discord-user-id>", Desc: "set the one operator whose DMs drive the agent"},
				{Syntax: "/relay exec on|off", Desc: "unlock dangerous tools offsite, each following /relay permissions (default off)"},
				{Syntax: "/relay timeout <duration>", Desc: "how long an approval card waits for your tap before auto-denying"},
				{Syntax: "/relay permissions [set <cat> <mode>]", Desc: "per-category ask|allow|deny for the relay (ask = button-approve in Discord)"},
			}},
		{Names: []string{"/imessage", "/imsg"}, Section: "safety", Run: slashIMessage,
			Summary: "the iMessage responder: allowlist, on/off, permissions",
			Rows: []cmdreg.Row{
				{Syntax: "/imessage [list]", Desc: "view the iMessage allowlist + responder state"},
				{Syntax: "/imessage on | off", Desc: "start/stop the autonomous responder (reply to texts) live"},
				{Syntax: "/imessage permissions", Desc: "per-category ask|allow|deny for the responder's tools (like /permissions)"},
				{Syntax: "/imessage allow <handle>", Desc: "permit a phone/email to drive the agent over iMessage"},
				{Syntax: "/imessage deny <handle> | clear", Desc: "remove one handle, or empty the allowlist"},
			}},
	}
}
