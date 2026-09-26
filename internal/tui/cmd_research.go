package tui

// Research commands: /research, /goal, /review, /cancel-clarify.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"tenant/internal/cmdreg"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// slashResearch runs /research, /deep, /research!, /deep!.
func slashResearch(m *model, line string) tea.Cmd {
	_, cmd, arg := slashParts(line)
	if m.cfg.Research == nil {
		m.sysChat("deep research is not available in this session")
		return nil
	}
	// /research! and /deep! are the skip-clarify shortcuts (C2 opt-out).
	// The implementation: dispatch via ResearchAfterClarify with the
	// raw question (no enrichment) — that path skips the clarifier.
	skipClarify := strings.HasSuffix(cmd, "!")
	arg = strings.TrimSpace(arg)
	// Sub-commands: history, show, replay, delete. Only valid on the
	// non-shortcut form (`/research!` is for "skip clarify on the
	// question that follows", not a sub-command host).
	if !skipClarify {
		if subcmd, rest, ok := splitFirstWord(arg); ok && isResearchSub(subcmd) {
			return m.handleResearchSub(subcmd, rest)
		}
	}
	if arg == "" {
		m.sysChat("usage:\n  /research <question>\n  /research! <question>   (skip clarification)\n  /research history [N]\n  /research show <id>\n  /research replay <id>\n  /research delete <id>")
		return nil
	}
	if m.busy {
		m.sysChat("busy — wait for the current turn/research to finish")
		return nil
	}
	// A new /research while a previous one is awaiting clarification:
	// drop the pending state and treat this as the active query.
	if m.pendingClarify != nil {
		m.pendingClarify = nil
		m.appendFeed(cSys.Render("⏏ dropped pending clarification"))
	}
	m.msgs = append(m.msgs, chatMsg{role: "user", content: cmd + " " + arg})
	m.busy = true
	m.interrupted = false
	m.chatFollow = true
	m.appendFeed(cSys.Render("🔎 deep research started — Esc to stop"))
	rc := m.cfg.Research
	// Reuse the per-turn cancel so Esc interrupts research too.
	rctx, cancel := context.WithCancel(m.ctx)
	m.turnCancel = cancel
	q := arg
	skip := skipClarify
	return func() tea.Msg {
		var (
			report string
			err    error
		)
		if skip {
			report, err = rc.ResearchAfterClarify(rctx, q)
		} else {
			report, err = rc.Research(rctx, q)
		}
		cancel()
		return researchDoneMsg{report: report, err: err}
	}
}

// slashGoal runs /goal.
func slashGoal(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// Claude-Code-style autonomous loop. Sub-commands:
	//   /goal <condition>     — set + start
	//   /goal show            — status
	//   /goal clear|stop|...  — stop
	if m.cfg.Goals == nil {
		m.sysChat("goal autonomous loop is not available in this session")
		return nil
	}
	arg = strings.TrimSpace(arg)
	// Sub-command sniff: distinguish "show / clear / stop / off / reset /
	// cancel" from a regular condition starting with those words.
	if first, rest, ok := splitFirstWord(arg); ok && isGoalSub(first) && rest == "" {
		switch strings.ToLower(first) {
		case "show", "status":
			st := m.cfg.Goals.Show()
			m.sysChat(renderGoalStatus(st))
		case "clear", "stop", "off", "reset", "cancel":
			m.sysChat("🎯 " + m.cfg.Goals.Clear())
		}
		return nil
	}
	if arg == "" {
		if m.cfg.Goals.Active() {
			m.sysChat(renderGoalStatus(m.cfg.Goals.Show()))
		} else {
			m.sysChat("usage:\n  /goal <condition>  — set + start the autonomous loop\n  /goal show         — current status\n  /goal clear        — stop the loop (aliases: stop, off, reset, cancel)")
		}
		return nil
	}
	if m.busy {
		m.sysChat("busy — wait for the current turn to finish, then set the goal")
		return nil
	}
	firstPrompt, status, err := m.cfg.Goals.Set(m.ctx, arg)
	if err != nil {
		m.sysChat("goal: " + err.Error())
		return nil
	}
	m.appendFeed(cOK.Render("🎯 goal set"))
	m.sysChat(status)
	// Kick off the first turn with the goal prompt — same path as if the
	// user had typed it themselves.
	m.ta.SetValue(firstPrompt)
	return m.submit()
}

// slashReview runs /review.
func slashReview(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// GStack Layer 3 cascading review. Usage:
	//   /review <plan.md>                — all reviewers
	//   /review <plan.md> ceo,eng        — comma-separated subset
	if m.cfg.Review == nil {
		m.sysChat("/review is not available in this session")
		return nil
	}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		m.sysChat("usage:\n  /review <plan.md>           — run CEO + Engineer + Designer reviews\n  /review <plan.md> ceo,eng   — run a subset (ceo, eng, design)")
		return nil
	}
	if m.busy {
		m.sysChat("busy — wait for the current turn to finish, then /review")
		return nil
	}
	planPath, rolesArg, _ := splitFirstWord(arg)
	var roles []string
	if r := strings.TrimSpace(rolesArg); r != "" {
		for _, x := range strings.Split(r, ",") {
			if x = strings.TrimSpace(x); x != "" {
				roles = append(roles, x)
			}
		}
	}
	m.appendFeed(cOK.Render("📝 review starting — " + planPath))
	report, err := m.cfg.Review.Review(m.ctx, planPath, roles)
	if err != nil {
		m.sysChat("review: " + err.Error())
		if strings.TrimSpace(report) != "" {
			m.sysChat(report)
		}
		return nil
	}
	m.appendFeed(cOK.Render("📝 review appended to " + planPath))
	m.sysChat(report)
	return nil
}

// slashCancelClarify runs /cancel-clarify.
func slashCancelClarify(m *model, line string) tea.Cmd {
	// C2: drop a pending clarification prompt without answering it.
	// Useful when the user changed their mind about /research-ing this
	// query and doesn't want their next typed message folded in.
	if m.pendingClarify == nil {
		m.sysChat("no clarification is pending")
		return nil
	}
	m.pendingClarify = nil
	m.sysChat("⏏ clarification dropped — your next message is a normal chat turn")
	return nil
}

// isResearchSub returns true if word is a known /research sub-command. Keeps
// `/research <question>` working as the default when the first word isn't a
// reserved keyword.
func isResearchSub(word string) bool {
	switch strings.ToLower(word) {
	case "history", "list", "show", "replay", "delete", "rm":
		return true
	}
	return false
}

// splitFirstWord returns ("first", "rest", true) for "first rest", or ("","",false)
// for empty input. The rest is left as-is (whitespace within preserved).
func splitFirstWord(s string) (first, rest string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i+1:]), true
	}
	return s, "", true
}

// isGoalSub returns true if word is a known /goal management sub-command.
// Anything else (including the empty string) is treated as a CONDITION,
// matching Claude Code's UX ("/goal write a test" sets the condition).
func isGoalSub(word string) bool {
	switch strings.ToLower(word) {
	case "show", "status", "clear", "stop", "off", "reset", "cancel":
		return true
	}
	return false
}

// renderGoalStatus formats a GoalStatus for the system chat. Concise; the
// status bar will eventually carry a one-liner overlay but this is the
// detailed view for /goal show.
func renderGoalStatus(st GoalStatus) string {
	if !st.Active {
		return "🎯 no goal active. set one with /goal <condition>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🎯 goal: %s\n", st.Condition)
	fmt.Fprintf(&b, "   turns: %d / %d", st.Turns, st.MaxTurns)
	switch {
	case st.GoalLoopCeiling > 0:
		fmt.Fprintf(&b, "   loop ceiling: %d/turn", st.GoalLoopCeiling)
	case st.GoalLoopCeiling < 0:
		b.WriteString("   loop ceiling: unlimited")
	}
	if st.ElapsedFmt != "" {
		fmt.Fprintf(&b, "   elapsed: %s", st.ElapsedFmt)
	}
	if st.Met {
		b.WriteString("   status: ✦ met")
	}
	b.WriteString("\n")
	if st.LastJudge != "" {
		fmt.Fprintf(&b, "   judge: %s", st.LastJudge)
	} else {
		b.WriteString("   judge: (no evaluation yet)")
	}
	return strings.TrimRight(b.String(), "\n")
}

// handleResearchSub routes /research history|show|replay|delete to the right
// implementation. The default-of-arg matches each sub-command's expectation.
func (m *model) handleResearchSub(sub, rest string) tea.Cmd {
	rc := m.cfg.Research
	switch strings.ToLower(sub) {
	case "history", "list":
		limit := 20 // sane default for the table
		if rest != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil && n > 0 {
				limit = n
			}
		}
		rows, err := rc.ResearchHistory(limit)
		if err != nil {
			m.sysChat("research history: " + err.Error())
			return nil
		}
		m.sysChatStyled(renderResearchHistory(rows))
		return nil
	case "show":
		if rest == "" {
			m.sysChat("usage: /research show <id>")
			return nil
		}
		text, err := rc.ResearchShow(rest)
		if err != nil {
			m.sysChat("research show: " + err.Error())
			return nil
		}
		// The past report goes into the chat pane like a system message —
		// reads cleanly alongside the live conversation.
		m.msgs = append(m.msgs, chatMsg{role: "assistant", content: text})
		m.refresh()
		return nil
	case "replay":
		if rest == "" {
			m.sysChat("usage: /research replay <id>")
			return nil
		}
		if m.busy {
			m.sysChat("busy — wait for the current turn/research to finish")
			return nil
		}
		m.msgs = append(m.msgs, chatMsg{role: "user", content: "/research replay " + rest})
		m.busy = true
		m.interrupted = false
		m.chatFollow = true
		m.appendFeed(cSys.Render("⟳ replaying " + rest + " — Esc to stop"))
		rctx, cancel := context.WithCancel(m.ctx)
		m.turnCancel = cancel
		id := rest
		return func() tea.Msg {
			report, err := rc.ResearchReplay(rctx, id)
			cancel()
			return researchDoneMsg{report: report, err: err}
		}
	case "delete", "rm":
		if rest == "" {
			m.sysChat("usage: /research delete <id>")
			return nil
		}
		if err := rc.ResearchDelete(rest); err != nil {
			m.sysChat("research delete: " + err.Error())
			return nil
		}
		m.sysChat("🗑  deleted " + rest)
		return nil
	}
	m.sysChat("unknown /research sub-command: " + sub)
	return nil
}

// renderResearchHistory formats the run list as a table. Plain version for
// copy/save; styled version for the chat pane.
func renderResearchHistory(rows []ResearchHistoryRow) (string, string) {
	if len(rows) == 0 {
		return "no past research runs yet — try /research <question>", ""
	}
	var plain strings.Builder
	fmt.Fprintf(&plain, "%d run(s):\n", len(rows))
	for _, r := range rows {
		dur := "—"
		if r.Duration > 0 {
			dur = r.Duration.Round(time.Second).String()
		}
		started := r.Started.Local().Format("01-02 15:04")
		extra := ""
		if r.ReplayOf != "" {
			extra = "  (replay of " + r.ReplayOf + ")"
		}
		fmt.Fprintf(&plain, "  %s  %-7s  %3dF/%dR  %5s  %s%s\n",
			started, r.Status, r.NumFinds, r.NumRefs, dur,
			clipForList(r.Question, 60), extra)
	}
	plain.WriteString("(use /research show <id>, /research replay <id>, /research delete <id>)")
	return plain.String(), ""
}

// clipForList keeps the question column to a fixed visible width.
func clipForList(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

// researchCommands are deep research, the goal loop and plan review.
func researchCommands() []*slashCommand {
	return []*slashCommand{
		{Names: []string{"/research", "/deep", "/research!", "/deep!"}, Section: "research", Run: slashResearch,
			Summary: "deep research: plan, parallel researchers, cited report",
			Rows: []cmdreg.Row{
				{Syntax: "/research <question>", Desc: "kick off a research run"},
				{Syntax: "/research! <question>", Desc: "skip the vague-query clarification step"},
				{Syntax: "/research history [N]", Desc: "list past runs (newest first)"},
				{Syntax: "/research show <id>", Desc: "show a past run's report + metadata"},
				{Syntax: "/research replay <id>", Desc: "re-run a past question against the current model"},
				{Syntax: "/research delete <id>", Desc: "purge a past run from disk"},
			}},
		{Names: []string{"/cancel-clarify"}, Section: "research", Run: slashCancelClarify,
			Summary: "drop a pending clarification prompt"},
		{Names: []string{"/goal"}, Section: "goal", Run: slashGoal,
			Summary: "set, show or clear the autonomous loop's stop condition",
			Rows: []cmdreg.Row{
				{Syntax: "/goal <condition>", Desc: "set + start the autonomous loop (e.g. /goal write a test for feature X)"},
				{Syntax: "/goal show", Desc: "current status — condition, turns used, judge's last verdict"},
				{Syntax: "/goal clear", Desc: "stop the loop (aliases: stop, off, reset, cancel)"},
			}},
		{Names: []string{"/review"}, Section: "review", Run: slashReview,
			Summary: "CEO / Engineer / Designer review appended to a plan file",
			Rows: []cmdreg.Row{
				{Syntax: "/review <plan.md>", Desc: "run all 3 reviewers; report appended to the file"},
				{Syntax: "/review <plan.md> ceo,eng", Desc: "subset (ceo, eng, design — comma-separated)"},
			}},
	}
}
