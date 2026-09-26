package tui

// Tool and skill commands: /tools, /enable, /disable, /skills (the T4 library) and /skill (integration setup).

import (
	"fmt"
	"strconv"
	"strings"
	"tenant/internal/cmdreg"

	tea "github.com/charmbracelet/bubbletea"
)

// slashTools runs /tools.
func slashTools(m *model, line string) tea.Cmd {
	m.sysChatStyled(m.renderToolList())
	return nil
}

// slashSkills runs /skills.
func slashSkills(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// TEN-64: /skill (singular) was previously an alias for /skills
	// (plural, T4 skill memory library). It now routes to the
	// integration-config surface (configure API keys for gsuite,
	// discord, etc.). Use the plural `/skills` for the T4 library.
	m.sysChatStyled(m.handleSkills(arg))
	return nil
}

// slashSkill runs /skill.
func slashSkill(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// /skill (singular) — integration-config surface. Distinct from
	// /skills (plural) which manages the T4 skill memory library.
	// TEN-64 framework; per-platform catalog entries land in TEN-65+.
	if m.cfg.SkillConfig == nil {
		m.sysChat("/skill is not available in this session")
		return nil
	}
	sub, rest := firstField(arg)
	switch strings.ToLower(sub) {
	case "", "list":
		m.sysChat(renderSkillList(m.cfg.SkillConfig.SkillList()))
	case "show":
		id := strings.TrimSpace(rest)
		if id == "" {
			m.sysChat("usage: /skill show <id>   (see /skill list)")
			return nil
		}
		out, err := m.cfg.SkillConfig.SkillShow(id)
		if err != nil {
			m.sysChat(err.Error())
		} else {
			m.sysChat(out)
		}
	case "configure", "config", "set":
		// Parse optional --no-enable flag; the rest are args to
		// SkillConfigure (id + positional/kv).
		fields := strings.Fields(rest)
		noEnable := false
		remaining := make([]string, 0, len(fields))
		for _, f := range fields {
			if f == "--no-enable" {
				noEnable = true
				continue
			}
			remaining = append(remaining, f)
		}
		if len(remaining) == 0 {
			m.sysChat("usage: /skill configure <id> <value>   |   /skill configure <id> key=value [key=value …]   [--no-enable]")
			return nil
		}
		out, err := m.cfg.SkillConfig.SkillConfigure(remaining, noEnable)
		if err != nil {
			m.sysChat("✗ " + err.Error())
		} else {
			m.sysChat(out)
		}
	case "probe":
		id := strings.TrimSpace(rest)
		if id == "" {
			m.sysChat("usage: /skill probe <id>")
			return nil
		}
		out, err := m.cfg.SkillConfig.SkillProbe(id)
		if err != nil {
			m.sysChat("✗ " + err.Error())
		} else {
			m.sysChat(out)
		}
	case "clear":
		fields := strings.Fields(rest)
		if len(fields) != 2 {
			m.sysChat("usage: /skill clear <id> <field>")
			return nil
		}
		out, err := m.cfg.SkillConfig.SkillClear(fields[0], fields[1])
		if err != nil {
			m.sysChat("✗ " + err.Error())
		} else {
			m.sysChat(out)
		}
	default:
		m.sysChat("unknown /skill subcommand " + sub + " — try list, show, configure, probe, clear")
	}
	return nil
}

// slashEnable runs /enable, /disable.
func slashEnable(m *model, line string) tea.Cmd {
	_, cmd, arg := slashParts(line)
	on := cmd == "/enable"
	if m.cfg.Tools == nil {
		m.sysChat("no tools are configured (launch with --wiki-dir/--sql-db/--os/… )")
		return nil
	}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		m.sysChat("usage: " + cmd + " <tool>   |   " + cmd + " skill <plugin>   (see /tools)")
		return nil
	}
	verb := map[bool]string{true: "enabled", false: "disabled"}[on]
	// `/enable skill <name>` — explicit categorical form. Forces a
	// plugin-label sweep and rejects single-tool names so a typo
	// surfaces as a clean error instead of silently no-op'ing the
	// smart-match path.
	fields := strings.Fields(arg)
	if len(fields) >= 2 && strings.ToLower(fields[0]) == "skill" {
		label := fields[1]
		n, _, err := m.cfg.Tools.SetPluginEnabled(label, on)
		switch {
		case err != nil:
			m.sysChat("could not " + verb + " skill " + label + ": " + err.Error())
		case n == 0:
			plugins := m.cfg.Tools.Plugins()
			hint := ""
			if len(plugins) > 0 {
				hint = " — try: " + strings.Join(plugins, ", ")
			}
			m.sysChat("no skill named " + label + hint)
		default:
			m.sysChat(fmt.Sprintf("%s %d tool(s) in skill %q", verb, n, label))
		}
		return nil
	}
	// `/enable <name>` — smart-match: exact tool name first, fall
	// back to plugin label. Kept for back-compat and the muscle-
	// memory case (`/enable gsuite` still works).
	n, scope, err := m.cfg.Tools.SetEnabled(arg, on)
	switch {
	case err != nil:
		m.sysChat("could not enable " + arg + ": " + err.Error())
	case n == 0:
		m.sysChat("no tool or plugin named " + arg + " (see /tools)")
	default:
		m.sysChat(fmt.Sprintf("%s %d %s tool(s) [%s]", verb, n, scope, arg))
	}
	return nil
}

// handleSkills runs /skills subcommands and returns (plain, styled) for
// the chat. Only the list view is styled; action results are plain.
func (m *model) handleSkills(arg string) (string, string) {
	if m.cfg.Skills == nil {
		return "skills are not available in this session", ""
	}
	fields := strings.Fields(arg)
	if len(fields) == 0 { // bare /skills → list
		list := m.cfg.Skills.SkillList()
		if len(list) == 0 {
			return "no skills yet — the agent saves them via skill_save, or `/skills add <name> | <desc> | <recipe>`", ""
		}
		on := 0
		for _, s := range list {
			if s.Enabled {
				on++
			}
		}
		var p, st strings.Builder
		head := fmt.Sprintf("Skills (%d, %d on)", len(list), on)
		p.WriteString(head + "  ● on  ○ off\n")
		st.WriteString(cHeading.Render(head) + cDim.Render("  ● on  ○ off") + "\n")
		for _, s := range list {
			status := ""
			if s.Status != "live" {
				status = " [" + s.Status + "]"
			}
			p.WriteString(fmt.Sprintf("  %s %s%s — %s\n", mark(s.Enabled), s.Name, status, s.Description))
			name := cName.Render(s.Name)
			if !s.Enabled {
				name = cDim.Render(s.Name)
			}
			st.WriteString("  " + markStyled(s.Enabled) + " " + name + cSys.Render(status) + cDim.Render(" — "+s.Description) + "\n")
		}
		return strings.TrimRight(p.String(), "\n"), strings.TrimRight(st.String(), "\n")
	}
	sub := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(arg, fields[0]))
	switch sub {
	case "add":
		parts := strings.SplitN(rest, "|", 3)
		if len(parts) != 3 {
			return "usage: /skills add <name> | <description> | <recipe>", ""
		}
		name := strings.TrimSpace(parts[0])
		if err := m.cfg.Skills.AddSkill(name, strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])); err != nil {
			return "add failed: " + err.Error(), ""
		}
		return "saved skill " + name, ""
	case "auto":
		// TEN-152: graduate induced skills from manual review to auto-accept.
		mode := strings.ToLower(strings.TrimSpace(rest))
		if mode == "" {
			cur := m.cfg.Skills.AutoAcceptMode()
			if cur == "" {
				cur = "off"
			}
			return "skills auto-accept: " + cur +
				"\n  off     = every induced skill waits for /skills accept" +
				"\n  on      = accept every new induced skill immediately" +
				"\n  trusted = accept only while your recent feedback is healthy (acks, no undos)" +
				"\n  set with: /skills auto <off|on|trusted>", ""
		}
		if err := m.cfg.Skills.SetAutoAccept(mode); err != nil {
			return "error: " + err.Error(), ""
		}
		return "skills auto-accept set to " + mode + " (applies on the next induction run)", ""
	case "enable", "disable":
		on := sub == "enable"
		if rest == "" {
			return "usage: /skills " + sub + " <name>", ""
		}
		ok, err := m.cfg.Skills.SetSkillEnabled(rest, on)
		if err != nil {
			return "error: " + err.Error(), ""
		}
		if !ok {
			return "no skill named " + rest, ""
		}
		return sub + "d skill " + rest, ""
	case "forget":
		ok, err := m.cfg.Skills.ForgetSkill(rest)
		if err != nil {
			return "error: " + err.Error(), ""
		}
		if !ok {
			return "no skill named " + rest, ""
		}
		return "forgot skill " + rest, ""
	case "accept":
		ok, err := m.cfg.Skills.AcceptSkill(rest)
		if err != nil {
			return "error: " + err.Error(), ""
		}
		if !ok {
			return "no proposed skill named " + rest, ""
		}
		return "accepted skill " + rest, ""
	case "seed":
		// /skills seed <bundle> — bulk-install a named bundle of starter
		// skills. Today only the "gstack" bundle (port of Garry Tan's YC
		// operating discipline). Idempotent — safe to re-run.
		bundle := strings.ToLower(rest)
		if bundle == "" {
			return "usage: /skills seed gstack   (installs Garry Tan's CEO/founder-mode skill bundle)", ""
		}
		if m.cfg.SkillSeeds == nil {
			return "skill seeding not wired in this session", ""
		}
		n, err := m.cfg.SkillSeeds(bundle)
		if err != nil {
			return "seed failed: " + err.Error(), ""
		}
		return fmt.Sprintf("seeded %d skill(s) from bundle %q", n, bundle), ""
	case "history", "log":
		// /skills history <name> — show every prior snapshot of a skill,
		// newest first. The current live row is NOT in history; entries
		// are the predecessors. Empty history = the skill has never been
		// edited (its current row is v1).
		if rest == "" {
			return "usage: /skills history <name>", ""
		}
		entries, err := m.cfg.Skills.SkillHistory(rest)
		if err != nil {
			return "history error: " + err.Error(), ""
		}
		if len(entries) == 0 {
			return fmt.Sprintf("skill %q has no edit history (current row is v1)", rest), ""
		}
		var p strings.Builder
		fmt.Fprintf(&p, "%d prior version(s) of %q (newest first):\n", len(entries), rest)
		for _, h := range entries {
			when := h.ChangedAt.Local().Format("2006-01-02 15:04")
			fmt.Fprintf(&p, "  v%d  %s  via %-9s  status=%s  desc=%q\n",
				h.Version, when, h.ChangeSource, h.PriorStatus, clipForSkillHistory(h.PriorDescription, 60))
		}
		p.WriteString("\n(use /skills diff <name> [vN] · /skills revert <name> vN)")
		return strings.TrimRight(p.String(), "\n"), ""
	case "diff":
		// /skills diff <name> [vN]
		// Default N = the most recent prior version (1 step back). Shows a
		// side-by-side description+recipe diff so operators can SEE what
		// changed without exporting + comparing files manually.
		if rest == "" {
			return "usage: /skills diff <name> [vN]   (default: most recent prior version)", ""
		}
		dfields := strings.Fields(rest)
		name := dfields[0]
		version := 0 // 0 = "most recent prior"
		if len(dfields) >= 2 {
			n, perr := parseSkillVersion(dfields[1])
			if perr != nil {
				return "version must be a positive integer (or vN), e.g. /skills diff " + name + " v3", ""
			}
			version = n
		}
		// Resolve "most recent prior" to a concrete version number.
		if version == 0 {
			entries, err := m.cfg.Skills.SkillHistory(name)
			if err != nil {
				return "diff error: " + err.Error(), ""
			}
			if len(entries) == 0 {
				return fmt.Sprintf("skill %q has no prior versions to diff against", name), ""
			}
			version = entries[0].Version
		}
		entries, err := m.cfg.Skills.SkillHistory(name)
		if err != nil {
			return "diff error: " + err.Error(), ""
		}
		var prior *SkillHistoryEntry
		for i := range entries {
			if entries[i].Version == version {
				prior = &entries[i]
				break
			}
		}
		if prior == nil {
			return fmt.Sprintf("no v%d in history of %q (try /skills history %s)", version, name, name), ""
		}
		cur, err := m.cfg.Skills.SkillCurrent(name)
		if err != nil {
			return "diff error: " + err.Error(), ""
		}
		if cur == nil {
			return fmt.Sprintf("skill %q not found", name), ""
		}
		return renderSkillDiff(prior, cur, version), ""
	case "revert":
		// /skills revert <name> vN — restore (description, recipe, status)
		// from a prior version. Current state gets snapshotted into history
		// (as a "revert" entry) before being overwritten — so reverts are
		// themselves reversible. Embedding is refreshed against the
		// restored description.
		if rest == "" {
			return "usage: /skills revert <name> vN", ""
		}
		rfields := strings.Fields(rest)
		if len(rfields) < 2 {
			return "usage: /skills revert <name> vN", ""
		}
		name := rfields[0]
		version, perr := parseSkillVersion(rfields[1])
		if perr != nil || version < 1 {
			return "version must be a positive integer (or vN), e.g. /skills revert " + name + " v2", ""
		}
		if err := m.cfg.Skills.SkillRevert(name, version); err != nil {
			return "revert error: " + err.Error(), ""
		}
		return fmt.Sprintf("reverted %q to v%d (current state saved as new history entry)", name, version), ""
	default:
		return "unknown /skills subcommand " + sub + " (add/enable/disable/forget/accept/seed/history/diff/revert)", ""
	}
}

// parseSkillVersion accepts "N" or "vN" (case-insensitive) so the operator
// can type whichever feels natural. Returns the integer N.
func parseSkillVersion(s string) (int, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid version %q", s)
	}
	return n, nil
}

// clipForSkillHistory truncates the description column in the history
// listing so each row stays one line.
func clipForSkillHistory(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

// renderSkillDiff produces a readable side-by-side view of one prior
// version vs. the current live state. Description shown inline; recipe
// is the bulk so we use a "===" separator instead of a per-line diff
// (which would require a diff library and isn't worth the dependency).
func renderSkillDiff(prior *SkillHistoryEntry, cur *SkillSnapshot, version int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== diff %q v%d → current ===\n\n", cur.Name, version)
	fmt.Fprintf(&b, "status:      %s  →  %s\n", prior.PriorStatus, cur.Status)
	fmt.Fprintf(&b, "description (v%d):\n  %s\n\n", version, prior.PriorDescription)
	fmt.Fprintf(&b, "description (now):\n  %s\n\n", cur.Description)
	fmt.Fprintf(&b, "--- recipe v%d (%d chars) ---\n%s\n\n", version, len(prior.PriorRecipe), prior.PriorRecipe)
	fmt.Fprintf(&b, "--- recipe now (%d chars) ---\n%s", len(cur.Recipe), cur.Recipe)
	return b.String()
}

// mark / markStyled render the on/off glyph (plain and colored).
func mark(on bool) string {
	if on {
		return "●"
	}
	return "○"
}

func markStyled(on bool) string {
	if on {
		return cOnMark.Render("●")
	}
	return cOffMark.Render("○")
}

func (m *model) renderToolList() (string, string) {
	if m.cfg.Tools == nil {
		return "no tools configured", ""
	}
	list := m.cfg.Tools.ToolList()
	if len(list) == 0 {
		return "no tools loaded (this is a memory-only chat)", ""
	}
	var p, s strings.Builder
	p.WriteString("Tools  ● on  ○ off   ·   /enable|/disable <tool>   |   /enable skill <plugin>\n")
	s.WriteString(cHeading.Render("Tools") + cDim.Render("  ● on  ○ off   ·   /enable|/disable <tool>   |   /enable skill <plugin>") + "\n")

	// Tool-ranking status (TEN-225): the per-turn "tool ranking ON/OFF" line
	// also scrolls off the activity feed, so surface the last turn's decision
	// here on demand — this is the persistent place to check whether the full
	// tool catalog is hitting the prompt and why.
	if rr, ok := m.cfg.Tools.(toolRankingReporter); ok {
		var line string
		if ranked, surfaced, catalog, reason, have := rr.RankingStatus(); !have {
			line = "Tool ranking: not yet measured — send a message first"
		} else if ranked {
			line = fmt.Sprintf("Tool ranking: ON — %d of %d tools surfaced last turn", surfaced, catalog)
		} else {
			line = fmt.Sprintf("Tool ranking: OFF — full catalog of %d tools surfaced (%s)", catalog, reason)
		}
		p.WriteString(line + "\n")
		s.WriteString(cDim.Render(line) + "\n")
	}

	seen := map[string]bool{}
	for _, t := range list {
		if seen[t.Plugin] {
			continue
		}
		seen[t.Plugin] = true
		// Collect this plugin's tools (preserve order) + enabled count.
		var tools []ToolInfo
		on := 0
		for _, u := range list {
			if u.Plugin == t.Plugin {
				tools = append(tools, u)
				if u.Enabled {
					on++
				}
			}
		}
		count := fmt.Sprintf("(%d/%d on)", on, len(tools))
		p.WriteString(fmt.Sprintf("\n%s %s\n", t.Plugin, count))
		s.WriteString("\n" + cKey.Render(t.Plugin) + " " + cDim.Render(count) + "\n")
		for _, u := range tools {
			p.WriteString("    " + mark(u.Enabled) + " " + u.Name + "\n")
			name := cName.Render(u.Name)
			if !u.Enabled {
				name = cDim.Render(u.Name)
			}
			s.WriteString("    " + markStyled(u.Enabled) + " " + name + "\n")
		}
	}
	return strings.TrimRight(p.String(), "\n"), strings.TrimRight(s.String(), "\n")
}

// firstField splits "<word> <rest>" on the first whitespace run. Used
// by the /skill dispatcher to peel a subcommand off the arg string.
func firstField(s string) (word, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i:])
	}
	return s, ""
}

// renderSkillList formats the /skill list output. Legacy entries
// (wizard-only skills — path/flag-driven, not in the /configure framework)
// get a trailing "[legacy]" marker so operators know to use `tenant setup`.
// Audit P1: this surface keeps /skill list actionable even when the
// new catalog is empty.
func renderSkillList(infos []SkillConfigInfo) string {
	if len(infos) == 0 {
		return "no skills available (build is missing the skill catalog)"
	}
	var b strings.Builder
	b.WriteString("Skills (✓ configured, ● enabled):\n")
	for _, s := range infos {
		mark := "○"
		if s.Configured {
			mark = "✓"
		}
		state := ""
		if s.Enabled {
			state = " ●"
		}
		legacy := ""
		if s.Legacy {
			legacy = "  [legacy — use `tenant setup`]"
		}
		fmt.Fprintf(&b, "  %s %s — %s%s%s\n", mark, s.ID, s.Label, state, legacy)
	}
	return strings.TrimRight(b.String(), "\n")
}

// skillCommands are the tool toggles, the T4 skill library and integration
// setup.
func skillCommands() []*slashCommand {
	return []*slashCommand{
		{Names: []string{"/tools"}, Section: "tools", Run: slashTools,
			Summary: "list all tools and their on/off state"},
		{Names: []string{"/enable", "/disable"}, Section: "tools", Run: slashEnable,
			Summary: "turn a tool or a whole plugin on or off, live",
			Rows: []cmdreg.Row{
				{Syntax: "/enable <name>", Desc: "turn on a tool or whole plugin (smart match — e.g. /enable os, /enable os_sysinfo)"},
				{Syntax: "/enable skill <plugin>", Desc: "explicit categorical toggle — turn on every tool in a plugin (e.g. /enable skill gsuite)"},
				{Syntax: "/disable <name>", Desc: "turn off — it leaves the prompt entirely (no context/compute)"},
				{Syntax: "/disable skill <plugin>", Desc: "categorical disable — mirror of /enable skill"},
			}},
		{Names: []string{"/skill"}, Section: "tools", Run: slashSkill,
			Summary: "integration setup: list, show, configure, probe, clear",
			Rows: []cmdreg.Row{
				{Syntax: "/skill list", Desc: "list configurable integrations (gsuite, discord, …) and their state"},
				{Syntax: "/skill show <id>", Desc: "show one integration's fields + masked values"},
				{Syntax: "/skill configure <id> <args>", Desc: "alias of /configure — set an integration's API key/auth + probe + auto-enable"},
				{Syntax: "/skill probe <id>", Desc: "re-test an integration's credentials without changing config"},
				{Syntax: "/skill clear <id> <field>", Desc: "remove a stored credential (auto-disables if required field cleared)"},
			}},
		{Names: []string{"/skills"}, Section: "skills", Run: slashSkills,
			Summary: "the T4 skill library: list, add, manage, history, diff, revert, auto-accept",
			Rows: []cmdreg.Row{
				{Syntax: "/skills", Desc: "list reusable skill recipes"},
				{Syntax: "/skills add <name> | <desc> | <recipe>", Desc: "save a skill"},
				{Syntax: "/skills enable|disable|forget|accept <name>", Desc: "manage a skill"},
				{Syntax: "/skills history <name>", Desc: "audit log of prior versions (Option A: skill changelog)"},
				{Syntax: "/skills diff <name> [vN]", Desc: "diff current vs a prior version (default: most recent prior)"},
				{Syntax: "/skills revert <name> vN", Desc: "restore a prior version (current state saved as new history entry)"},
				{Syntax: "/skills auto [off|on|trusted]", Desc: "auto-accept induced skills (trusted = only while feedback is healthy)"},
				{Syntax: "/skills seed gstack", Desc: "install Garry Tan's CEO/founder-mode skill bundle (5 skills)"},
			}},
	}
}
