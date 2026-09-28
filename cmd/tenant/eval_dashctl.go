package main

// eval_dashctl.go adapts the eval surface to dashboard.EvalControl (TEN-201):
// the web Quality page reuses the SAME machinery the TUI /eval does
// (evalTUIControl for mutations, readEvalTrend for the series, renderBaselineDiff
// for the movers), exposing STRUCTURED views instead of the TUI's preformatted
// strings. No business logic forks here — it's presentation mapping only.

import (
	"os"
	"path/filepath"
	"time"

	"tenant/internal/dashboard"
)

type dashEval struct {
	ev    evalTUIControl
	prog  *evalProgress // live done/total while a check runs (nil in TUI mode)
	judge judgeCtl      // TEN-91: eval LLM-judge surface (same machinery as the TUI /judge command)
}

func (d dashEval) artifactDir() string { return filepath.Join(d.ev.dataDir, "eval-artifacts") }

// Judge surface (TEN-91) — delegates to the shared judgeCtl so the web Quality
// page, the TUI /judge command, `tenant eval`, and the nightly gate all read/
// write one persisted setting.
// JudgeStatus says who grades the checks in the Quality page's words; the
// TUI's Current() names its commands instead.
func (d dashEval) JudgeStatus() string {
	lc, err := loadLaunchConfig(d.judge.cfgDir)
	if err != nil || lc.Improve.Judge == "" {
		return "Your main model grades the checks."
	}
	ic := lc.Improve
	kind := ic.JudgeKind
	if kind == "" {
		kind = "anthropic"
	}
	s := ic.Judge + " from " + kind + " grades the checks."
	if env := resolveJudgeKeyEnv(kind, ic.JudgeKeyEnv); env != "" && os.Getenv(env) == "" {
		s += " Its API key (" + env + ") isn't set on the computer running Tenant, so grading fails until it is."
	}
	return s
}
func (d dashEval) SetJudge(kind, model, endpoint string) (string, error) {
	return d.judge.Set(kind, model, endpoint)
}
func (d dashEval) ClearJudge() error { return d.judge.Clear() }

// Schedule builds the structured schedule + last-run summary the home board
// and the Quality page render. Mirrors evalTUIControl.Status()'s logic but as
// data, not a string.
func (d dashEval) Schedule() dashboard.EvalScheduleView {
	var v dashboard.EvalScheduleView
	if running, done, total, task, mins := d.prog.view(); running {
		v.Running, v.RunDone, v.RunTotal, v.RunTask, v.RunMins = true, done, total, task, mins
		if total > 0 {
			v.RunPercent = done * 100 / total
		}
	}
	if d.ev.sched != nil {
		v.Desc = d.ev.sched.Desc()
		v.Live = true
	} else if lc, err := loadLaunchConfig(d.ev.cfgDir); err == nil {
		_, _, v.Desc = resolveEvalDue(false, 0, lc.Improve.EvalEvery, lc.Improve.EvalAt, nil)
	}
	if v.Desc == "" {
		v.Desc = "off"
	}
	entries, err := readEvalTrend(d.artifactDir())
	if err == nil && len(entries) > 0 {
		last := entries[len(entries)-1]
		v.HasRun = true
		v.LastScore = last.Overall
		v.LastPass = last.Passed
		v.LastTotal = last.Total
		v.Skipped = last.Skipped
		v.Ungraded = last.Ungraded
		if ts, perr := time.Parse(time.RFC3339, last.TS); perr == nil {
			v.LastWhen = ts.Local().Format("2006-01-02 15:04")
		} else {
			v.LastWhen = last.TS
		}
		switch {
		case last.HasBaseline && last.Regressed:
			v.Trend = "down"
		case last.HasBaseline && last.Delta > 0:
			v.Trend = "up"
		default:
			v.Trend = "steady"
		}
	}
	return v
}

func (d dashEval) Trend() []dashboard.EvalTrendPoint {
	entries, err := readEvalTrend(d.artifactDir())
	if err != nil {
		return nil
	}
	out := make([]dashboard.EvalTrendPoint, 0, len(entries))
	for _, e := range entries {
		when := e.TS
		if ts, perr := time.Parse(time.RFC3339, e.TS); perr == nil {
			when = ts.Local().Format("01-02 15:04")
		}
		out = append(out, dashboard.EvalTrendPoint{
			When:      when,
			Score:     e.Overall,
			Regressed: e.Regressed,
			HasBase:   e.HasBaseline,
		})
	}
	return out
}

func (d dashEval) Diff() (string, error)             { return renderBaselineDiff("", d.artifactDir()) }
func (d dashEval) SetEvery(s string) (string, error) { return d.ev.SetEvery(s) }
func (d dashEval) SetAt(s string) (string, error)    { return d.ev.SetAt(s) }
func (d dashEval) Off() (string, error)              { return d.ev.Off() }

// RunNow queues a check through the TUI's RunNow and says so in the page's
// words: that status names the feed and trend.jsonl.
func (d dashEval) RunNow() (string, error) {
	queued := d.ev.sched != nil && d.ev.sched.Pending()
	if _, err := d.ev.RunNow(); err != nil {
		return "", dashboard.PlainError("Quality checks are off on this Tenant, so one can't start from here.")
	}
	if queued {
		return "A quality check is already queued. It starts within a minute; the result appears here.", nil
	}
	return "Quality check queued. It starts within a minute; the result appears here.", nil
}
