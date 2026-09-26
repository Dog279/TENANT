package tui

// Federation and connector commands: /peer, /mcp, /github.

import (
	"fmt"
	"strings"
	"tenant/internal/cmdreg"

	tea "github.com/charmbracelet/bubbletea"
)

// slashMCP runs /mcp.
func slashMCP(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// Connect/manage remote MCP connector servers (TEN-164). `/mcp add
	// <url>` runs the OAuth browser flow off the UI goroutine (a tea.Cmd
	// closure) so the TUI stays responsive; tools come up gated.
	return m.handleMCP(arg)
}

// slashGitHub runs /github, /gh.
func slashGitHub(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// Watched repositories + per-repo write authorization. The token and the
	// repo set come from /configure github; this is the day-to-day switch.
	return m.handleGitHub(arg)
}

// slashPeer runs /peer.
func slashPeer(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// Federation peers (TEN-183/188): list/show/remove + invite. Share
	// editing is `/configure peer <name>`.
	return m.handlePeer(arg)
}

// handleMCP powers /mcp: connect/list/remove remote MCP connector servers
// (TEN-164). `add` runs the OAuth browser flow as a tea.Cmd closure (off the UI
// goroutine, mirroring the /configure async pattern); its result returns via
// sysChatMsg. list/remove are synchronous.
func (m *model) handleMCP(arg string) tea.Cmd {
	if m.cfg.MCP == nil {
		m.sysChat("remote MCP connectors not available in this session")
		return nil
	}
	fields := strings.Fields(arg)
	sub, rest := "", ""
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arg), fields[0]))
	}
	switch sub {
	case "", "list":
		servers := m.cfg.MCP.List()
		if len(servers) == 0 {
			m.sysChat("no remote MCP servers connected. Add one:  /mcp add https://mcp.atlassian.com/v1/mcp")
			return nil
		}
		var b strings.Builder
		b.WriteString("Remote MCP connectors:\n")
		for _, s := range servers {
			var state string
			switch {
			case s.NeedsReauth:
				state = "⚠ NEEDS RE-AUTH"
				if !s.AuthSince.IsZero() {
					state += " since " + s.AuthSince.Local().Format("Jan 2 15:04")
				}
				state += " — its tools fail until you reconnect:  /mcp add " + s.URL
			case s.Enabled:
				state = fmt.Sprintf("on, %d tools", s.ToolCount)
			case s.ToolCount > 0:
				state = fmt.Sprintf("connected, %d tools (disabled)", s.ToolCount)
			default:
				state = "registered — /enable " + s.Label + " to connect"
			}
			trust := s.Trust
			if trust == "" {
				trust = "ask"
			}
			b.WriteString(fmt.Sprintf("  • %-26s %s  [%s]  trust: %s\n", s.Label, s.URL, state, trust))
		}
		b.WriteString("  (trust: ask = each gated call prompts · allow = runs without prompts · deny = refused.  /mcp trust <label> <mode>)")
		m.sysChat(strings.TrimRight(b.String(), "\n"))
		return nil
	case "add":
		url := strings.TrimSpace(rest)
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			m.sysChat("usage: /mcp add <https://server/mcp>   (e.g. https://mcp.atlassian.com/v1/mcp)")
			return nil
		}
		m.sysChat("🌐 connecting to " + url + " — a browser will open to authorize. If the server shows two screens (approve, then pick a site), click through once each; don't re-click.")
		ctl := m.cfg.MCP
		return func() tea.Msg {
			info, err := ctl.Add(url) // BLOCKS: DCR + browser + callback
			if err != nil {
				return sysChatMsg{text: "✗ MCP connect failed: " + err.Error()}
			}
			return sysChatMsg{text: fmt.Sprintf("✓ connected %s — %d tools live, trust: ask (each gated call prompts).  /mcp trust %s allow  to run them without prompts; /tools to review; /disable %s to turn off", info.Label, info.ToolCount, info.Label, info.Label)}
		}
	case "trust", "allow", "deny", "ask":
		target, mode := "", ""
		if sub == "trust" {
			parts := strings.Fields(rest)
			if len(parts) == 2 {
				target, mode = parts[0], parts[1]
			}
		} else { // shorthand: /mcp allow <label>
			target, mode = strings.TrimSpace(rest), sub
		}
		if target == "" || mode == "" {
			m.sysChat("usage: /mcp trust <url|label> <ask|allow|deny>   (ask = each gated call prompts · allow = no per-call prompts · deny = refuse + hide)")
			return nil
		}
		info, err := m.cfg.MCP.SetTrust(target, mode)
		if err != nil {
			m.sysChat("✗ " + err.Error())
			return nil
		}
		m.sysChat(fmt.Sprintf("✓ %s trust: %s (%d tools)", info.Label, info.Trust, info.ToolCount))
		return nil
	case "remove", "rm", "forget":
		target := strings.TrimSpace(rest)
		if target == "" {
			m.sysChat("usage: /mcp remove <url|label>")
			return nil
		}
		removed, err := m.cfg.MCP.Remove(target)
		switch {
		case err != nil:
			m.sysChat("✗ " + err.Error())
		case !removed:
			m.sysChat("no remote MCP server matched " + target)
		default:
			m.sysChat("✓ removed " + target)
		}
		return nil
	default:
		m.sysChat("usage: /mcp [list] | /mcp add <url> | /mcp trust <url|label> <ask|allow|deny> | /mcp remove <url|label>")
		return nil
	}
}

// handlePeer drives `/peer` (federation pairing, TEN-183/188): list/show/remove.
// Share editing is `/configure peer <name>`; pairing-by-address is the `invite`
// subcommand (push + Approve/Deny — designed, building next).
// handleGitHub drives the watch list. Read access is not adjustable here: a
// watched repo is always reportable, which is the point of watching it. The
// only lever is write, per repo.
func (m *model) handleGitHub(arg string) tea.Cmd {
	if m.cfg.GitHub == nil {
		m.sysChat("github is not available in this session - run `/configure github` first")
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
		repos, err := m.cfg.GitHub.Repos()
		if err != nil {
			m.sysChat("github: " + err.Error())
			return nil
		}
		if len(repos) == 0 {
			m.sysChat("no repositories watched yet - add one with `/github add owner/name`")
			return nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "watched repositories (%d):\n", len(repos))
		for _, r := range repos {
			mode := "watch-only"
			if r.Write {
				mode = "WRITABLE"
			}
			tag := ""
			if r.Primary {
				tag = "  (primary - the default when a tool omits its repo)"
			}
			fmt.Fprintf(&b, "  %-40s %s%s\n", r.Slug, mode, tag)
		}
		b.WriteString("\nevery watched repo is reported on; only WRITABLE ones can be modified,\n")
		b.WriteString("and writes there are still gated per action.")
		m.sysChat(strings.TrimRight(b.String(), "\n"))
	case "write", "rw":
		if rest == "" {
			m.sysChat("usage: /github write <owner/name>")
			return nil
		}
		info, err := m.cfg.GitHub.SetWrite(rest, true)
		if err != nil {
			m.sysChat("github: " + err.Error())
			return nil
		}
		m.sysChat(info.Slug + ": WRITABLE - the agent may modify it; each write is still gated. Run `/enable github` to apply to the running session.")
	case "readonly", "read", "ro":
		if rest == "" {
			m.sysChat("usage: /github readonly <owner/name>")
			return nil
		}
		info, err := m.cfg.GitHub.SetWrite(rest, false)
		if err != nil {
			m.sysChat("github: " + err.Error())
			return nil
		}
		m.sysChat(info.Slug + ": watch-only - still reported on, writes now refused in code. Run `/enable github` to apply to the running session.")
	case "add":
		f := strings.Fields(rest)
		if len(f) == 0 {
			m.sysChat("usage: /github add <owner/name> [write]")
			return nil
		}
		write := len(f) > 1 && strings.EqualFold(f[1], "write")
		info, err := m.cfg.GitHub.Add(f[0], write)
		if err != nil {
			m.sysChat("github: " + err.Error())
			return nil
		}
		mode := "watch-only"
		if info.Write {
			mode = "WRITABLE"
		}
		m.sysChat("watching " + info.Slug + " (" + mode + "). Make sure the PAT covers it, then `/enable github`.")
	case "remove", "rm", "forget":
		if rest == "" {
			m.sysChat("usage: /github remove <owner/name>")
			return nil
		}
		ok, err := m.cfg.GitHub.Remove(rest)
		if err != nil {
			m.sysChat("github: " + err.Error())
			return nil
		}
		if !ok {
			m.sysChat("github: " + rest + " was not watched")
			return nil
		}
		m.sysChat("stopped watching " + rest)
	default:
		m.sysChat("usage: /github [list] | write <owner/name> | readonly <owner/name> | add <owner/name> [write] | remove <owner/name>")
	}
	return nil
}

func (m *model) handlePeer(arg string) tea.Cmd {
	if m.cfg.Peer == nil {
		m.sysChat("peers not available in this session")
		return nil
	}
	fields := strings.Fields(arg)
	sub, rest := "", ""
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arg), fields[0]))
	}
	switch sub {
	case "", "list":
		peers := m.cfg.Peer.List()
		if len(peers) == 0 {
			m.sysChat("no peers paired yet. Pair with `/peer invite <name> <ip|url>` (they Approve/Deny), or the `tenant peer` CLI.")
			return nil
		}
		health := peerHealthByName(m.cfg.Peer.Health()) // TEN-250: live status
		var b strings.Builder
		b.WriteString("Federation peers:\n")
		for _, p := range peers {
			role := "accept"
			if p.Dial {
				role = "dial"
			}
			h := health[p.Name]
			b.WriteString(fmt.Sprintf("  %s %-16s %-6s %-26s [token %s] share: %s\n",
				peerStatusDot(h.State), p.Name, role, dashOr(p.URL), p.TokenState, renderShareInline(p.Share)))
			if d := strings.TrimSpace(h.Detail); d != "" {
				b.WriteString("      " + cDim.Render(d) + "\n")
			}
		}
		b.WriteString("\n" + cDim.Render(peerStatusDot("alive")+" alive  "+peerStatusDot("dead")+" unreachable  "+peerStatusDot("unknown")+" unknown"))
		b.WriteString("\nDetailed status:  /peer status   ·   Edit sharing:  /configure peer <name>")
		m.sysChat(strings.TrimRight(b.String(), "\n"))
		return nil
	case "status":
		peers := m.cfg.Peer.List()
		if len(peers) == 0 {
			m.sysChat("no peers paired yet. Pair with `/peer invite <name> <ip|url>` (they Approve/Deny).")
			return nil
		}
		m.sysChat(renderPeerStatus(peers, peerHealthByName(m.cfg.Peer.Health())))
		return nil
	case "show":
		name := strings.TrimSpace(rest)
		p, ok := m.cfg.Peer.Show(name)
		if !ok {
			m.sysChat("no peer named " + name + " (/peer to list)")
			return nil
		}
		role := "accept (they dial us)"
		if p.Dial {
			role = "dial (we dial them)"
		}
		m.sysChat(fmt.Sprintf("peer %s\n  instance: %s\n  role: %s\n  url: %s\n  token: %s\n  share: %s",
			p.Name, dashOr(p.InstanceID), role, dashOr(p.URL), p.TokenState, renderShareInline(p.Share)))
		return nil
	case "remove", "rm":
		name := strings.TrimSpace(rest)
		ok, err := m.cfg.Peer.Remove(name)
		if err != nil {
			m.sysChat("remove failed: " + err.Error())
			return nil
		}
		if !ok {
			m.sysChat("no peer named " + name)
			return nil
		}
		m.sysChat("✓ removed peer " + name)
		return nil
	case "reconnect":
		// Bring paired peers' shared tools live in the running agent (e.g. after
		// pairing mid-session or if a peer's listener came up late).
		n, err := m.cfg.Peer.Reconnect()
		if err != nil {
			m.sysChat("peer reconnect failed: " + err.Error())
			return nil
		}
		if n == 0 {
			m.sysChat("no dialable peers to reconnect (you pair by inviting one with /peer invite).")
			return nil
		}
		m.sysChat(fmt.Sprintf("🔌 reconnecting to %d peer(s) — their shared knowledge folds into your own search shortly. wiki_search now also covers connected peers (peer hits flagged trust-but-verify).", n))
		return nil
	case "stats":
		// Drift tracking: how each peer's federated search has behaved this
		// session — a peer drifting toward all-denied/all-errored is visible here.
		m.sysChat(renderPeerStats(m.cfg.Peer.Stats()))
		return nil
	case "rename", "alias":
		f := strings.Fields(rest)
		if len(f) != 2 {
			m.sysChat("usage: /peer rename <current-name> <new-name>")
			return nil
		}
		if err := m.cfg.Peer.Rename(f[0], f[1]); err != nil {
			m.sysChat("rename failed: " + err.Error())
			return nil
		}
		m.sysChat("✓ renamed peer " + f[0] + " → " + f[1])
		return nil
	case "serve":
		// Start/restart the peer listener so others can reach us AND inbound
		// invites prompt right here in the TUI (TEN-239 follow-up).
		bound, err := m.cfg.Peer.Serve(strings.TrimSpace(rest))
		if err != nil {
			m.sysChat("peer serve failed: " + err.Error())
			return nil
		}
		m.sysChat("✓ peer listener on " + bound + "\n  peers can now `/peer invite <name> https://<this-host>:<port>`; their request will prompt here to Approve/Deny.")
		return nil
	case "invite":
		// TEN-239 push-invite: `/peer invite <name> <ip|url>`. Mint+show the PIN
		// synchronously, then run the (blocking) Approve/Deny request off the UI
		// goroutine so the TUI stays responsive.
		f := strings.Fields(rest)
		if len(f) < 2 {
			m.sysChat("usage: /peer invite <name> <ip|url>   (the peer's operator Approves/Denies + matches the PIN)")
			return nil
		}
		label, url := f[0], f[1]
		pin, run, err := m.cfg.Peer.Invite(label, url)
		if err != nil {
			m.sysChat("invite failed: " + err.Error())
			return nil
		}
		m.sysChat(fmt.Sprintf("📡 inviting %q at %s\n→ Tell their operator this PIN to approve:  %s\n(waiting for Approve — up to 3 min)", label, url, pin))
		return func() tea.Msg {
			msg, rerr := run(m.ctx)
			if rerr != nil {
				return sysChatMsg{text: "✗ pairing failed: " + rerr.Error()}
			}
			return sysChatMsg{text: "✓ " + msg + " — set sharing with /configure peer " + label}
		}
	default:
		m.sysChat("usage: /peer serve [addr] | /peer invite <name> <ip|url> | /peer reconnect | /peer rename <old> <new> | /peer [list] | /peer show <name> | /peer remove <name>   (sharing: /configure peer <name>)")
		return nil
	}
}

// peerShareDesc describes each sharable capability, shown in the /configure peer
// editor (mirrors the per-category descriptions in the global /permissions view).
var peerShareDesc = map[string]string{
	"wiki":   "your shared wiki / knowledge base (read)",
	"memory": "your shared + public memory — facts & episodes (read)",
	"skills": "propose your skills to this peer (Phase 4)",
	"exec":   "run commands on your machine (Phase 2 — still operator-gated)",
	"llm":    "use your model for a turn (Phase 2 — still operator-gated)",
}

// handleConfigurePeer drives `/configure peer <name>` — the per-peer share
// editor, presented like the global /permissions view: each sharable item is
// allow or deny (deny-by-default). No args shows the table; edits mirror
// /permissions syntax: `set <item> <allow|deny>` (also tolerates `<item>
// <allow|deny>` and the legacy `<item>=<on|off>`).
func (m *model) handleConfigurePeer(args []string) {
	if m.cfg.Peer == nil {
		m.sysChat("peers not available in this session")
		return
	}
	if len(args) == 0 {
		peers := m.cfg.Peer.List()
		if len(peers) == 0 {
			m.sysChat("no peers paired yet — pair one first (see `/peer`).")
			return
		}
		names := make([]string, 0, len(peers))
		for _, p := range peers {
			names = append(names, p.Name)
		}
		m.sysChat("usage: /configure peer <name>                      (show sharing)\n" +
			"       /configure peer <name> set <item> <allow|deny>\npeers: " + strings.Join(names, ", "))
		return
	}
	name := args[0]
	p, ok := m.cfg.Peer.Show(name)
	if !ok {
		m.sysChat("no peer named " + name + " (/peer to list)")
		return
	}
	rest := args[1:]
	applied := 0
	switch {
	case len(rest) == 0:
		// show only
	case strings.ToLower(rest[0]) == "set":
		if len(rest) < 3 {
			m.sysChat("usage: /configure peer " + name + " set <item> <allow|deny>")
			return
		}
		if !m.applyPeerShare(name, rest[1], rest[2]) {
			return
		}
		applied++
	case len(rest) == 2 && !strings.Contains(rest[0], "="):
		if !m.applyPeerShare(name, rest[0], rest[1]) {
			return
		}
		applied++
	default: // legacy lenient: item=mode pairs
		for _, kv := range rest {
			key, val, found := strings.Cut(kv, "=")
			if !found {
				m.sysChat("usage: /configure peer " + name + " set <item> <allow|deny>")
				return
			}
			if !m.applyPeerShare(name, key, val) {
				return
			}
			applied++
		}
	}
	if applied > 0 {
		if up, ok := m.cfg.Peer.Show(name); ok {
			p = up
		}
	}
	plain, styled := renderPeerShare(name, p.Share, applied)
	m.sysChatStyled(plain, styled)
}

// applyPeerShare sets one share item to allow/deny; on error it posts a message
// and returns false.
func (m *model) applyPeerShare(name, item, mode string) bool {
	allow, perr := parseAllow(mode)
	if perr != nil {
		m.sysChat(item + ": " + perr.Error())
		return false
	}
	if _, serr := m.cfg.Peer.SetShare(name, strings.ToLower(strings.TrimSpace(item)), allow); serr != nil {
		m.sysChat("set " + item + ": " + serr.Error())
		return false
	}
	return true
}

// renderPeerShare renders a peer's share policy in the same style as the global
// /permissions view (colored mode + description), as (plain, styled).
func renderPeerShare(name string, share map[string]bool, applied int) (string, string) {
	var p, s strings.Builder
	head := name + " sharing"
	if applied > 0 {
		head = "✓ updated " + name + " sharing"
	}
	hint := "  (allow / deny)   ·   /configure peer " + name + " set <item> <mode>"
	p.WriteString(head + hint + "\n")
	s.WriteString(cHeading.Render(head) + cDim.Render(hint) + "\n")
	for _, item := range PeerShareCaps {
		mode := "deny"
		if share[item] {
			mode = "allow"
		}
		desc := peerShareDesc[item]
		p.WriteString(fmt.Sprintf("  %-8s %-5s  %s\n", item, mode, desc))
		ms := cErr // deny = red
		if mode == "allow" {
			ms = cOnMark
		}
		s.WriteString("  " + cKey.Render(fmt.Sprintf("%-8s", item)) + " " +
			ms.Render(fmt.Sprintf("%-5s", mode)) + "  " + cDim.Render(desc) + "\n")
	}
	return strings.TrimRight(p.String(), "\n"), strings.TrimRight(s.String(), "\n")
}

// renderShareInline lists the allowed capabilities, or "(none)".
func renderShareInline(share map[string]bool) string {
	on := make([]string, 0, len(PeerShareCaps))
	for _, c := range PeerShareCaps {
		if share[c] {
			on = append(on, c)
		}
	}
	if len(on) == 0 {
		return "(none)"
	}
	return strings.Join(on, ",")
}

func dashOr(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// peerStatusDot renders the colored liveness indicator for /peer (TEN-250):
// green ● alive, red ● unreachable, dim ○ unknown.
func peerStatusDot(state string) string {
	switch state {
	case "alive":
		return cOK.Render("●")
	case "dead":
		return cErr.Render("●")
	default:
		return cDim.Render("○")
	}
}

// peerHealthByName indexes a Health() snapshot by peer name for the list render;
// a peer with no entry resolves to the zero value (State "" → unknown dot).
func peerHealthByName(hs []PeerHealth) map[string]PeerHealth {
	m := make(map[string]PeerHealth, len(hs))
	for _, h := range hs {
		m[h.Name] = h
	}
	return m
}

// renderPeerStatus is the /peer status view (TEN-251): per peer, a colored
// liveness dot + both directions of sharing, so a glance shows whether each
// peer is talking AND how it can talk.
//
//	● work          alive — seen 3s ago (12ms)
//	    we  → them:  wiki, memory
//	    them → we:   wiki, memory, skills
func renderPeerStatus(peers []PeerInfo, health map[string]PeerHealth) string {
	stateWord := map[string]string{"alive": "alive", "dead": "unreachable", "unknown": "unknown"}
	var b strings.Builder
	b.WriteString("Peer status:\n")
	for _, p := range peers {
		h := health[p.Name]
		word := stateWord[h.State]
		if word == "" {
			word = "unknown"
		}
		line := fmt.Sprintf("  %s %-16s %s", peerStatusDot(h.State), p.Name, word)
		if d := strings.TrimSpace(h.Detail); d != "" {
			line += cDim.Render(" — " + d)
		}
		b.WriteString(line + "\n")
		// Outbound: what WE grant THEM (our local share policy).
		b.WriteString("      " + cDim.Render("we  → them: ") + renderShareInline(p.Share) + "\n")
		// Inbound: what THEY grant US (learned from their peer_hello).
		them := "(unknown — we don't dial this peer)"
		if h.TheirShareKnown {
			them = renderShareList(h.TheirShare)
		}
		b.WriteString("      " + cDim.Render("them → we:  ") + them + "\n")
	}
	b.WriteString("\n" + cDim.Render(peerStatusDot("alive")+" alive  "+peerStatusDot("dead")+" unreachable  "+peerStatusDot("unknown")+" unknown"))
	return strings.TrimRight(b.String(), "\n")
}

// renderShareList formats a capability list (the peer's grant to us) like
// renderShareInline does for our own policy. Empty ⇒ "(none)".
func renderShareList(caps []string) string {
	if len(caps) == 0 {
		return "(none)"
	}
	return strings.Join(caps, ",")
}

// renderPeerStats formats the federated-search drift tally (/peer stats).
func renderPeerStats(stats []PeerFedStat) string {
	if len(stats) == 0 {
		return "No federated searches yet this session. Once a connected peer is queried via wiki_search/memory_search, per-peer hit/deny/error counts show here (drift tracking)."
	}
	var b strings.Builder
	b.WriteString("Federation drift (this session):\n")
	b.WriteString(fmt.Sprintf("  %-16s %7s %5s %6s %6s %5s  %s\n", "peer", "queries", "hits", "denied", "errors", "empty", "bytes"))
	for _, s := range stats {
		b.WriteString(fmt.Sprintf("  %-16s %7d %5d %6d %6d %5d  %d\n",
			s.Peer, s.Queries, s.Hits, s.Denied, s.Errors, s.Empty, s.Bytes))
	}
	b.WriteString("\ndenied = peer's share gate refused · errors = offline/transport · empty = allowed but no match")
	return b.String()
}

func parseAllow(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "allow", "true", "yes", "1":
		return true, nil
	case "off", "deny", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("want on|off (allow|deny), got %q", s)
}

// peerCommands are the MCP connector, federation and GitHub commands.
func peerCommands() []*slashCommand {
	return []*slashCommand{
		{Names: []string{"/mcp"}, Section: "mcp", Run: slashMCP,
			Summary: "connect, trust and remove remote MCP servers",
			Rows: []cmdreg.Row{
				{Syntax: "/mcp", Desc: "list connected remote MCP servers + state"},
				{Syntax: "/mcp add <url>", Desc: "connect an MCP tool server (a browser opens only if it asks for sign-in) — its tools arrive with trust: ask"},
				{Syntax: "/mcp trust <label> <ask|allow|deny>", Desc: "live trust for a server's tools: allow = no per-call prompts, deny = refuse + hide"},
				{Syntax: "/mcp remove <url>", Desc: "disconnect a remote MCP server and forget it"},
			}},
		{Names: []string{"/peer"}, Section: "mcp", Run: slashPeer,
			Summary: "federate with other Tenants: serve, invite, rename, reconnect, stats",
			Rows: []cmdreg.Row{
				{Syntax: "/peer serve [addr]", Desc: "start the peer listener (default 0.0.0.0:9100, TLS) so peers can reach you + invites prompt here"},
				{Syntax: "/peer invite <name> <ip|url>", Desc: "pair with a peer by address — they Approve/Deny + match a PIN"},
				{Syntax: "/peer rename <old> <new>", Desc: "relabel a peer (long hostname → readable)"},
				{Syntax: "/peer reconnect", Desc: "fold paired peers' shared knowledge into your search now (after pairing mid-session)"},
				{Syntax: "/peer stats", Desc: "per-peer federated-search tally (drift tracking: queries/hits/denied/errors)"},
				{Syntax: "/peer", Desc: "list federation peers + their share policy (also: /peer show|remove)"},
			}},
		{Names: []string{"/github", "/gh"}, Section: "mcp", Run: slashGitHub,
			Summary: "watched repos and which the agent may write to",
			Rows: []cmdreg.Row{
				{Syntax: "/github", Desc: "list watched repos + which are writable (also: /github add|remove)"},
				{Syntax: "/github write <owner/name>", Desc: "let the agent modify this repo (writes still gated per action)"},
				{Syntax: "/github readonly <owner/name>", Desc: "watch + report only - writes refused in code"},
			}},
	}
}
