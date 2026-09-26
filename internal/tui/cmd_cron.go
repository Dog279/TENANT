package tui

// Automation commands: /cron.

import (
	"fmt"
	"strings"
	"tenant/internal/cmdreg"

	tea "github.com/charmbracelet/bubbletea"
)

// slashCron runs /cron, /cronjob.
func slashCron(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// Manage recurring agent-prompt jobs. Each job runs UNATTENDED and
	// read/comms-safe on its schedule; definitions persist to config.json.
	if m.cfg.Cron == nil {
		m.sysChat("cron scheduling not available in this session")
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
		m.sysChatStyled(m.renderCronList())
	case "add", "new":
		// Form: /cron add [shell|exec] [tz=<zone>] <schedule> | <payload>
		// Leading flags precede the schedule; "|" separates the (space-
		// containing) schedule from the prompt/command.
		specPart, prompt, ok := strings.Cut(rest, "|")
		addSpec, perr := parseCronAdd(specPart)
		prompt = strings.TrimSpace(prompt)
		if perr != nil || !ok || addSpec.Spec == "" || prompt == "" {
			m.sysChat("usage: /cron add [shell|exec] [tz=<zone>] <schedule> | <payload>\n" +
				"  e.g. /cron add 0 9 * * 1-5 | run the test suite and summarize failures\n" +
				"  e.g. /cron add @every 30m | check the build status\n" +
				"  e.g. /cron add shell @daily | go test ./...        (DANGEROUS, needs cron-exec)\n" +
				"  e.g. /cron add exec tz=America/New_York 0 8 * * * | clean up temp files")
			return nil
		}
		addSpec.Prompt = prompt
		j, err := m.cfg.Cron.Add(addSpec)
		if err != nil {
			m.sysChat("cron: " + err.Error())
		} else {
			mode := j.Kind
			if j.Exec {
				mode += "+exec"
			}
			m.sysChat(fmt.Sprintf("cron: scheduled %s [%s] (%s) — next run %s", j.ID, mode, j.Spec, cronDash(j.NextRun)))
		}
	case "rm", "remove", "del", "delete":
		if rest == "" {
			m.sysChat("usage: /cron rm <id>")
			return nil
		}
		removed, err := m.cfg.Cron.Remove(rest)
		switch {
		case err != nil:
			m.sysChat("cron: " + err.Error())
		case !removed:
			m.sysChat("cron: no job with id " + rest)
		default:
			m.sysChat("cron: removed " + rest)
		}
	case "enable", "disable":
		if rest == "" {
			m.sysChat("usage: /cron " + sub + " <id>")
			return nil
		}
		on := sub == "enable"
		j, changed, err := m.cfg.Cron.SetEnabled(rest, on)
		switch {
		case err != nil:
			m.sysChat("cron: " + err.Error())
		case !changed:
			m.sysChat("cron: no change for " + rest + " (not found or already " + sub + "d)")
		case on:
			m.sysChat(fmt.Sprintf("cron: enabled %s — next run %s", j.ID, cronDash(j.NextRun)))
		default:
			m.sysChat("cron: disabled " + j.ID)
		}
	case "run":
		if rest == "" {
			m.sysChat("usage: /cron run <id>")
			return nil
		}
		if err := m.cfg.Cron.RunNow(rest); err != nil {
			m.sysChat("cron: " + err.Error())
		} else {
			m.sysChat("cron: running " + rest + " now — result will appear in the feed")
		}
	case "exec":
		// Global kill-switch for dangerous (shell / exec-opted-in) jobs.
		arg := strings.ToLower(strings.TrimSpace(rest))
		switch arg {
		case "", "status":
			if m.cfg.Cron.ExecEnabled() {
				m.sysChat("cron exec: ON — shell jobs and exec-opted-in jobs WILL run unattended")
			} else {
				m.sysChat("cron exec: OFF — shell/exec jobs are created but won't run (deny-by-default). Enable with /cron exec on")
			}
		case "on", "off":
			on := arg == "on"
			if err := m.cfg.Cron.SetExec(on); err != nil {
				m.sysChat("cron: " + err.Error())
			} else if on {
				m.sysChat("cron exec: ENABLED — shell + exec-opted-in jobs may now run UNATTENDED with dangerous tools (irreversible actions + config writes are still blocked). Disable with /cron exec off")
			} else {
				m.sysChat("cron exec: disabled — dangerous jobs are now inert (deny-by-default)")
			}
		default:
			m.sysChat("usage: /cron exec [on | off | status]")
		}
	default:
		m.sysChat("usage: /cron [list | add <schedule> | <prompt> | enable <id> | disable <id> | run <id> | rm <id> | exec on|off]")
	}
	return nil
}

// renderCronList formats the recurring-job list as (plain, styled).
func (m *model) renderCronList() (string, string) {
	jobs := m.cfg.Cron.Jobs()
	var p, s strings.Builder
	const title = "Cron jobs"
	execState := "exec: off (dangerous jobs inert)"
	execStyle := cDim
	if m.cfg.Cron.ExecEnabled() {
		execState = "exec: ON (shell/exec jobs run unattended)"
		execStyle = cErr
	}
	p.WriteString(title + "  [" + execState + "]\n")
	s.WriteString(cHeading.Render(title) + "  " + execStyle.Render("["+execState+"]") + "\n")
	if len(jobs) == 0 {
		const empty = "(no jobs) — recurring agent-prompt jobs run unattended, read/comms-safe."
		const hint = "Add one with /cron add <schedule> | <prompt>  (e.g. 0 9 * * 1-5 | run the tests)."
		p.WriteString("  " + empty + "\n  " + hint)
		s.WriteString("  " + cOffMark.Render(empty) + "\n  " + cDim.Render(hint))
		return p.String(), s.String()
	}
	for _, j := range jobs {
		name := j.Name
		if name == "" {
			name = j.ID
		}
		mark, markStyled := "○", cOffMark.Render("○")
		if j.Enabled {
			mark, markStyled = "●", cOnMark.Render("●")
		}
		mode := j.Kind
		if mode == "" {
			mode = "prompt"
		}
		if j.Exec {
			mode += "+exec"
		}
		if j.TZ != "" {
			mode += " " + j.TZ
		}
		head := fmt.Sprintf("%s %s  %s  [%s]  id:%s  next:%s", mark, name, j.Spec, mode, j.ID, cronDash(j.NextRun))
		p.WriteString("  " + head + "\n")
		modeStyle := cDim
		if j.Exec || j.Kind == "shell" {
			modeStyle = cErr // flag the dangerous surfaces
		}
		s.WriteString("  " + markStyled + " " + cName.Render(name) + "  " +
			cKey.Render(j.Spec) + "  " + modeStyle.Render("["+mode+"]") + " " +
			cDim.Render("id:"+j.ID+" next:"+cronDash(j.NextRun)) + "\n")

		prompt := "    " + cronClip(j.Prompt, 88)
		p.WriteString(prompt + "\n")
		s.WriteString("    " + cDim.Render(cronClip(j.Prompt, 88)) + "\n")

		if j.LastStatus != "" {
			st := "    last: " + j.LastStatus + " @ " + cronDash(j.LastRun)
			p.WriteString(st + "\n")
			style := cDim
			switch j.LastStatus {
			case "ok":
				style = cOK
			case "error":
				style = cErr
			}
			s.WriteString("    " + style.Render("last: "+j.LastStatus+" @ "+cronDash(j.LastRun)) + "\n")
		}
	}
	hint := fmt.Sprintf("%d job(s) — /cron run <id> to run now, /cron disable <id> to pause.", len(jobs))
	p.WriteString("  " + hint)
	s.WriteString("  " + cDim.Render(hint))
	return p.String(), s.String()
}

// parseCronAdd parses the part before the "|": optional leading flags
// (shell|exec|tz=<zone>) followed by the schedule. The schedule is whatever
// remains after the recognized leading flags.
func parseCronAdd(specPart string) (CronAddSpec, error) {
	var s CronAddSpec
	toks := strings.Fields(specPart)
	i := 0
	for ; i < len(toks); i++ {
		t := toks[i]
		switch {
		case strings.EqualFold(t, "shell"):
			s.Kind = "shell"
		case strings.EqualFold(t, "exec"):
			s.Exec = true
		case strings.HasPrefix(strings.ToLower(t), "tz="):
			s.TZ = t[len("tz="):]
		default:
			// First non-flag token starts the schedule.
			s.Spec = strings.TrimSpace(strings.Join(toks[i:], " "))
			return s, nil
		}
	}
	// No schedule found (only flags).
	return s, nil
}

// cronDash renders an empty schedule field as "-".
func cronDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// cronClip truncates s to n runes with an ellipsis, for compact job rows.
func cronClip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return string(r[:n-1]) + "…"
}

// cronCommands are the recurring-job commands.
func cronCommands() []*slashCommand {
	return []*slashCommand{
		{Names: []string{"/cron", "/cronjob"}, Section: "automation", Run: slashCron,
			Summary: "recurring agent-prompt jobs: list, add, enable, run, remove, exec switch",
			Rows: []cmdreg.Row{
				{Syntax: "/cron [list]", Desc: "view scheduled recurring jobs"},
				{Syntax: "/cron add <sched> | <prompt>", Desc: "schedule a job, e.g. 0 9 * * 1-5 | run the tests"},
				{Syntax: "/cron enable <id> | disable <id>", Desc: "pause or resume a job"},
				{Syntax: "/cron run <id>", Desc: "run a job once now (result appears in the feed)"},
				{Syntax: "/cron rm <id>", Desc: "delete a job"},
				{Syntax: "/cron exec on|off", Desc: "global kill-switch for dangerous shell/exec jobs (default off)"},
			}},
	}
}
