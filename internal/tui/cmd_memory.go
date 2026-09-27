package tui

// Memory commands: /memory, /compress, /expand, and the /ack and /undo feedback signals.

import (
	"fmt"
	"strings"
	"tenant/internal/cmdreg"

	tea "github.com/charmbracelet/bubbletea"
)

// slashMemory runs /memory, /mem.
func slashMemory(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	m.sysChat(m.handleMemory(arg))
	return nil
}

// slashCompress runs /compress, /compact.
func slashCompress(m *model, line string) tea.Cmd {
	if m.cfg.Agent == nil {
		m.sysChat("compaction is not available in this session")
		return nil
	}
	m.sysChat("compacting context…")
	ag, ctx := m.cfg.Agent, m.ctx
	return func() tea.Msg {
		before, after, err := ag.CompactNow(ctx)
		return compactDoneMsg{before: before, after: after, err: err}
	}
}

// slashExpand runs /expand.
func slashExpand(m *model, line string) tea.Cmd {
	// Rehydrate the latest compaction summary's source span from the archive
	// (read-only) — "paged out, not lost." (TEN-104)
	if m.cfg.Agent == nil {
		m.sysChat("expansion is not available in this session")
		return nil
	}
	ag, ctx := m.cfg.Agent, m.ctx
	m.sysChat("expanding the latest compaction…")
	return func() tea.Msg {
		exp, err := ag.ExpandLatestCompaction(ctx)
		return expandDoneMsg{exp: exp, err: err}
	}
}

// slashAck runs /ack.
func slashAck(m *model, line string) tea.Cmd {
	// TEN-151: mark the last turn as good (the real-world success signal).
	if m.cfg.Feedback == nil {
		m.sysChat("feedback isn't available in this session")
		return nil
	}
	if st, err := m.cfg.Feedback.Ack(); err != nil {
		m.sysChat("✗ " + err.Error())
	} else {
		m.sysChat("✓ " + st)
	}
	return nil
}

// slashUndo runs /undo.
func slashUndo(m *model, line string) tea.Cmd {
	// TEN-151: mark the last turn as bad (suspends "trusted" auto-accept).
	if m.cfg.Feedback == nil {
		m.sysChat("feedback isn't available in this session")
		return nil
	}
	if st, err := m.cfg.Feedback.Undo(); err != nil {
		m.sysChat("✗ " + err.Error())
	} else {
		m.sysChat("✓ " + st)
	}
	return nil
}

// handleMemory routes /memory subcommands to the MemoryControl.
func (m *model) handleMemory(arg string) string {
	if m.cfg.Memory == nil {
		return "memory controls are not available in this session"
	}
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return m.cfg.Memory.Stats()
	}
	sub := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(arg, fields[0]))
	switch sub {
	case "stats":
		return m.cfg.Memory.Stats()
	case "search":
		if rest == "" {
			return "usage: /memory search <query>"
		}
		return m.cfg.Memory.Search(rest)
	case "facts":
		return m.cfg.Memory.Facts(rest)
	case "recent":
		n := 10
		if rest != "" {
			fmt.Sscan(rest, &n)
		}
		return m.cfg.Memory.Recent(n)
	case "forget":
		if rest == "" {
			return "usage: /memory forget fact:<id> | ep:<id>"
		}
		return m.cfg.Memory.Forget(rest)
	case "soul":
		sf := strings.Fields(rest)
		if len(sf) == 0 {
			return m.cfg.Memory.SoulView()
		}
		if strings.ToLower(sf[0]) == "import" {
			p := strings.TrimSpace(strings.TrimPrefix(rest, sf[0]))
			if p == "" {
				return "usage: /memory soul import <path.md>"
			}
			return m.cfg.Memory.SoulImport(p)
		}
		return "the soul is view-only here — use `/memory soul import <path.md>` or edit the TOML in the OS"
	case "rules":
		sf := strings.Fields(rest)
		if len(sf) == 0 {
			return m.cfg.Memory.RulesView()
		}
		if strings.ToLower(sf[0]) == "import" {
			p := strings.TrimSpace(strings.TrimPrefix(rest, sf[0]))
			if p == "" {
				return "usage: /memory rules import <path.md|folder>"
			}
			return m.cfg.Memory.SoulImport(p) // rules ARE the soul's operating instructions
		}
		return "rules are the soul's operating instructions — `/memory rules` to view, `/memory rules import <path>` to set"
	case "distill":
		return m.cfg.Memory.Distill()
	case "profile", "user":
		if strings.EqualFold(strings.TrimSpace(rest), "refresh") {
			return m.cfg.Memory.ProfileRefresh()
		}
		return m.cfg.Memory.ProfileView()
	default:
		return "unknown /memory subcommand " + sub + " (stats/search/facts/recent/forget/soul/rules/profile/distill)"
	}
}

// memoryCommands are the memory, compaction and feedback commands.
func memoryCommands() []*slashCommand {
	return []*slashCommand{
		{Names: []string{"/memory", "/mem"}, Section: "memory", Run: slashMemory,
			Summary: "stats, search, facts, recent turns, forget, soul, rules, profile, distill",
			Rows: []cmdreg.Row{
				{Syntax: "/memory", Desc: "stats: episodes, facts, skills, soul"},
				{Syntax: "/memory search <q>", Desc: "hybrid search over facts + episodes"},
				{Syntax: "/memory facts [q]", Desc: "list distilled facts (T3)"},
				{Syntax: "/memory recent [n]", Desc: "recent episodes (T2)"},
				{Syntax: "/memory forget fact:<id>|ep:<id>", Desc: "hide a fact or episode from recall (tenant privacy forget deletes for good)"},
				{Syntax: "/memory soul", Desc: "view the soul (T0)"},
				{Syntax: "/memory soul import <path>", Desc: "replace rules from a .md file or folder"},
				{Syntax: "/memory rules", Desc: "view operating rules (the instructions)"},
				{Syntax: "/memory rules import <path>", Desc: "set rules (same as soul import)"},
				{Syntax: "/memory profile [refresh]", Desc: "view / rebuild the learned user model"},
				{Syntax: "/memory distill", Desc: "run a distillation pass now"},
			}},
		{Names: []string{"/compress", "/compact"}, Section: "memory", Run: slashCompress,
			Summary: "summarize old turns to free up context now"},
		{Names: []string{"/expand"}, Section: "memory", Run: slashExpand,
			Summary: "bring the latest compacted span back from the archive (read-only)"},
		{Names: []string{"/ack"}, Section: "skills", Run: slashAck,
			Summary: "mark the last turn as good — the self-improvement success signal"},
		{Names: []string{"/undo"}, Section: "skills", Run: slashUndo,
			Summary: "mark the last turn as bad (suspends trusted auto-accept)"},
	}
}
