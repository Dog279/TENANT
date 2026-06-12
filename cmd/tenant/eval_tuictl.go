package main

// eval_tuictl.go adapts the nightly-eval schedule (TEN-196) to tui.EvalControl,
// so `/eval` views + re-tunes the cadence in-session and persists it to
// launchConfig the same way `/skills auto` persists improve.auto_accept.
// With the self-improve loop off this session (--self-improve=false), the
// schedule still persists — it arms the NEXT launch — and /eval says so.

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type evalTUIControl struct {
	sched   *evalSchedule // nil ⇒ no improve scheduler this session (persist-only)
	cfgDir  string
	dataDir string
}

// liveNote tells the operator whether a change applied to the running
// scheduler or only to config.
func (e evalTUIControl) liveNote() string {
	if e.sched == nil {
		return " (persisted — applies next launch; the self-improve loop is off this session)"
	}
	return " (live + persisted)"
}

// Status reports the current schedule, the last recorded run from the trend
// log, and how to change it.
func (e evalTUIControl) Status() string {
	desc, note := "off", ""
	if e.sched != nil {
		desc = e.sched.Desc()
		if e.sched.Pending() {
			note = " · one-shot queued (fires at the next scheduler tick, ≤1m)"
		}
	} else if lc, err := loadLaunchConfig(e.cfgDir); err == nil {
		_, _, desc = resolveEvalDue(false, 0, lc.Improve.EvalEvery, lc.Improve.EvalAt, nil)
		note = " — self-improve loop off this session; schedule applies next launch"
	}

	last := "never"
	dir := filepath.Join(e.dataDir, "eval-artifacts")
	if entries, err := readEvalTrend(dir); err == nil && len(entries) > 0 {
		le := entries[len(entries)-1]
		verdict := ""
		if le.HasBaseline {
			if le.Regressed {
				verdict = " — REGRESSION"
			} else {
				verdict = " — ok"
			}
		}
		when := le.TS
		if ts, perr := time.Parse(time.RFC3339, le.TS); perr == nil {
			when = ts.Local().Format("2006-01-02 15:04")
		}
		last = fmt.Sprintf("%s · %s %.1f (%d/%d)%s", when, le.Subset, le.Overall, le.Passed, le.Total, verdict)
	}
	return fmt.Sprintf("nightly eval: %s%s\nlast run: %s\nchange: /eval every <dur> | /eval at <HH:MM> | /eval off | /eval now | /eval trend",
		desc, note, last)
}

// SetEvery arms interval mode: one run per d, relaunch-proof (the clock is
// the trend log). Persisting clears eval_at — the anchor would otherwise win
// again at next launch (resolveEvalDue precedence).
func (e evalTUIControl) SetEvery(spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	d, err := time.ParseDuration(spec)
	if err != nil || d <= 0 {
		return "", fmt.Errorf("bad interval %q — use a Go duration like 24h or 6h30m", spec)
	}
	if err := e.persist(func(ic *improveConfig) { ic.EvalEvery, ic.EvalAt = spec, "" }); err != nil {
		return "", err
	}
	if e.sched != nil {
		e.sched.set(evalEveryDue(d), "every "+d.String())
	}
	return "nightly eval: every " + d.String() + e.liveNote(), nil
}

// SetAt arms the daily wall-clock anchor ("HH:MM", 24h, local).
func (e evalTUIControl) SetAt(spec string) (string, error) {
	hh, mm, ok := parseEvalAt(spec)
	if !ok {
		return "", fmt.Errorf("bad time %q — use the 24h wall clock, like 03:15", spec)
	}
	canon := fmt.Sprintf("%02d:%02d", hh, mm)
	if err := e.persist(func(ic *improveConfig) { ic.EvalAt = canon }); err != nil {
		return "", err
	}
	if e.sched != nil {
		e.sched.set(evalAtDue(hh, mm), "daily at "+canon)
	}
	return "nightly eval: daily at " + canon + e.liveNote(), nil
}

// Off disarms the schedule (both config keys cleared).
func (e evalTUIControl) Off() (string, error) {
	if err := e.persist(func(ic *improveConfig) { ic.EvalEvery, ic.EvalAt = "", "" }); err != nil {
		return "", err
	}
	if e.sched != nil {
		e.sched.set(nil, "off")
	}
	return "nightly eval: off" + e.liveNote(), nil
}

// RunNow queues a single immediate run on the improve scheduler. The
// scheduler's Paused hook still suppresses it while the model is degraded.
func (e evalTUIControl) RunNow() (string, error) {
	if e.sched == nil {
		return "", fmt.Errorf("the improve scheduler is off this session (--self-improve)")
	}
	if e.sched.Pending() {
		return "eval already queued — it fires at the next scheduler tick (a start line will appear in the feed)", nil
	}
	e.sched.ForceOnce()
	return "eval queued — a start line appears in the feed within the next minute; the run takes minutes and the result lands in the feed and trend.jsonl", nil
}

// Trend renders the last n trend entries (newest first).
func (e evalTUIControl) Trend(n int) string {
	entries, err := readEvalTrend(filepath.Join(e.dataDir, "eval-artifacts"))
	if err != nil {
		return "eval trend: " + err.Error()
	}
	return renderEvalTrend(entries, n)
}

// Diff renders the per-task movers analysis between the newest artifact
// and its baseline (TEN-198).
func (e evalTUIControl) Diff() (string, error) {
	return renderBaselineDiff("", filepath.Join(e.dataDir, "eval-artifacts"))
}

// persist mutates improveConfig in launchConfig and saves it — the same
// load-mutate-save idiom as skillControl.SetAutoAccept.
func (e evalTUIControl) persist(mut func(*improveConfig)) error {
	if e.cfgDir == "" {
		return fmt.Errorf("eval schedule isn't configurable in this session")
	}
	lc, err := loadLaunchConfig(e.cfgDir)
	if err != nil {
		return err
	}
	mut(&lc.Improve)
	return lc.save(e.cfgDir)
}
