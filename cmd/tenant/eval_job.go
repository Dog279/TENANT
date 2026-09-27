package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tenant/internal/agent"
	"tenant/internal/eval"
	"tenant/internal/fsperm"
	"tenant/internal/improve"
)

// resolveEvalCadence picks the nightly-eval interval (TEN-157): an explicitly-set
// --eval-every flag wins; else the persisted improve.eval_every (a duration
// string like "24h"); an empty or MALFORMED config value fails CLOSED to 0 (off)
// so a config typo can't brick launch or silently mis-arm the gate. A nil log
// keeps a malformed value quiet (a status read); otherwise it is reported.
func resolveEvalCadence(flagSet bool, flagVal time.Duration, cfgVal string, log *slog.Logger) time.Duration {
	if flagSet {
		return flagVal
	}
	s := strings.TrimSpace(cfgVal)
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		if log != nil {
			eval.ScheduleIgnored.Log(context.Background(), "value", s, "effect", "the job stays off", "err", err)
		}
		return 0
	}
	return d
}

// evalNightlyJob runs the eval suite on a cadence — the air-gapped appliance's
// "nightly CI", with no external cron. It writes a timestamped JSON report
// artifact under <data>/eval-artifacts and, when a baseline exists, checks for
// a regression via the paired-bootstrap CI. Implements improve.Job so it rides
// the same scheduler as distill/skill/profile.
//
// Caveat: a live full run builds its OWN router + tool mux (separate from the
// chat agent's) and takes minutes — register it on a long cadence (e.g. 24h)
// and only when the operator opts in (--eval-every).
type evalNightlyJob struct {
	c            *commonFlags
	pf           *pluginFlags
	subset       eval.Subset
	jOpts        evalJudgeOpts
	artifactDir  string
	baselinePath string // "" or missing → no regression check
	// events, when set, mirrors each eval task agent's tool activity to the
	// dashboard feed as agent "eval-<taskID>" - a 3-hour quality run was
	// otherwise a black box between "started" and the score line.
	events func(agent.Event)
	// progress, when set, is the live done/total state the Quality page
	// renders as a bar while a check is running.
	progress *evalProgress
}

// newEvalNightlyJob builds the nightly job from the live command's config:
// full subset, planner-default judge, artifacts under <data>/eval-artifacts,
// baseline at baselines/full.json (checked only if present).
func newEvalNightlyJob(c *commonFlags, pf *pluginFlags, events func(agent.Event), progress *evalProgress) *evalNightlyJob {
	return &evalNightlyJob{
		c:            c,
		pf:           pf,
		subset:       eval.SubsetFull,
		events:       events,
		progress:     progress,
		artifactDir:  filepath.Join(c.dataDir, "eval-artifacts"),
		baselinePath: resolveBaselinePath(filepath.Join(c.dataDir, "eval-artifacts"), "full"),
	}
}

func (j *evalNightlyJob) Name() string { return "eval-nightly" }

// Run executes the eval once, writes the artifact, and checks the baseline.
// Per the Job contract, an error here is recorded but does not stop the
// scheduler. eval never mutates durable state, so Changed is always false.
func (j *evalNightlyJob) Run(ctx context.Context) (improve.JobResult, error) {
	rep, err := runEvalToReport(ctx, j.c, j.pf, j.subset, j.jOpts, j.events, j.progress)
	if err != nil {
		return improve.JobResult{Summary: "eval-nightly: run failed: " + err.Error()}, err
	}
	total := rep.Aggregates.PassCount + rep.Aggregates.FailCount

	artifact := writeEvalArtifact(ctx, j.artifactDir, rep)
	e := trendEntryFor(ctx, rep, j.baselinePath, artifact)

	summary := fmt.Sprintf("eval-nightly %s: overall %.1f, passed %d/%d",
		rep.Subset, rep.Aggregates.Overall, rep.Aggregates.PassCount, total)
	for class, n := range e.InfraUngraded {
		if class != "judge" {
			summary += fmt.Sprintf(", %d ungraded:%s", n, class)
		}
	}
	if e.Ungraded > 0 {
		summary += fmt.Sprintf(", %d ungraded", e.Ungraded)
	}
	if e.Skipped > 0 {
		summary += fmt.Sprintf(", %d skipped", e.Skipped)
	}
	if e.HasBaseline {
		if e.Regressed {
			summary += fmt.Sprintf(" — REGRESSION (Δ %.1f, CI hi %.1f)", e.Delta, e.CIHigh)
		} else {
			summary += fmt.Sprintf(" — no regression (Δ %.1f)", e.Delta)
		}
	}

	// Append the compact trend line (TEN-158): the durable record of THIS
	// run's regression verdict (delta/ci_high), which the heavy per-run
	// artifacts don't keep. Non-fatal — mirrors the artifact write. It also
	// doubles as the cadence clock (TEN-196): latestTrendTime seeds the
	// scheduler so relaunches don't re-fire today's run.
	appendEvalTrend(ctx, j.artifactDir, e)

	return improve.JobResult{
		Summary: summary,
		Changed: false,
		Details: map[string]any{
			"overall":   rep.Aggregates.Overall,
			"passed":    rep.Aggregates.PassCount,
			"total":     total,
			"artifact":  artifact,
			"regressed": e.Regressed,
		},
	}, nil
}

// writeEvalArtifact writes the timestamped JSON report under dir and returns
// its path — "" on failure, which is logged and never fatal: the run's value
// is the score, not the file. Shared by the nightly job and
// `tenant eval --append-trend` (TEN-196).
func writeEvalArtifact(ctx context.Context, dir string, rep *eval.Report) string {
	// Owner-only like the rest of the data dir, so a run after `doctor --fix`
	// does not widen the tree again (TEN-333).
	if err := fsperm.MkdirAll(dir); err != nil {
		eval.NotSaved.Log(ctx, "record", "report", "path", dir, "err", err)
		return ""
	}
	path := filepath.Join(dir, "eval-"+time.Now().UTC().Format("20060102-150405")+".json")
	f, err := fsperm.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		eval.NotSaved.Log(ctx, "record", "report", "path", path, "err", err)
		return ""
	}
	err = eval.WriteJSON(f, rep)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		eval.NotSaved.Log(ctx, "record", "report", "path", path, "err", err)
		_ = os.Remove(path) // a torn report must not pass for a real one
		return ""
	}
	return path
}

// trendEntryFor builds the compact trend line for one finished run, including
// the regression verdict against the baseline at baselinePath. A missing or
// unreadable baseline is not an error — HasBaseline stays false. Shared by
// the nightly job and `tenant eval --append-trend` so both produce
// shape-identical lines (TEN-196).
func trendEntryFor(ctx context.Context, rep *eval.Report, baselinePath, artifact string) evalTrendEntry {
	e := evalTrendEntry{
		TS:            time.Now().UTC().Format(time.RFC3339),
		Subset:        string(rep.Subset),
		Overall:       rep.Aggregates.Overall,
		Passed:        rep.Aggregates.PassCount,
		Total:         rep.Aggregates.PassCount + rep.Aggregates.FailCount,
		Ungraded:      rep.Aggregates.UngradedCount,
		InfraUngraded: rep.Aggregates.UngradedByClass,
		Skipped:       rep.Aggregates.SkippedCount,
	}
	if artifact != "" { // filepath.Base("") is "." — keep the field empty instead
		e.Artifact = filepath.Base(artifact)
	}
	data, rerr := os.ReadFile(baselinePath)
	if rerr != nil {
		// The regression gate is DEAD without a baseline - say so every run,
		// because this exact silence hid an 84.9 -> 10.4 collapse behind a
		// "steady" arrow for five nights.
		eval.BaselineMissing.Log(ctx, "path", baselinePath)
	}
	if rerr == nil {
		if base, berr := eval.ReadBaseline(data); berr == nil {
			rr := eval.CompareToBaseline(base, rep, eval.CompareOptions{})
			e.HasBaseline, e.Regressed, e.Delta, e.CIHigh = true, rr.Regressed, rr.Delta, rr.CIHigh
			if rr.Regressed {
				eval.Regression.Log(ctx, "subset", rep.Subset, "delta", rr.Delta, "ci_high", rr.CIHigh, "overall", rep.Aggregates.Overall, "artifact", e.Artifact)
			}
		} else {
			eval.BaselineUnreadable.Log(ctx, "path", baselinePath, "err", berr)
		}
	}
	return e
}

// resolveBaselinePath finds the regression baseline for a subset. The
// canonical home is <data>/eval-artifacts/baseline.<subset>.json (written by
// `tenant eval --baseline-from <artifact>`); the legacy repo-relative
// baselines/<subset>.json is honored when it resolves from the process cwd.
// History: the nightly job shipped with ONLY the relative path, which never
// resolves under the scheduled task (serve.cmd sets no cwd) - so the gate
// was silently dead from 2026-08-19 on, and the dashboard said "steady"
// through an 84.9 -> 10.4 collapse. A missing baseline must be LOUD.
func resolveBaselinePath(artifactDir, subset string) string {
	canonical := filepath.Join(artifactDir, "baseline."+subset+".json")
	if _, err := os.Stat(canonical); err == nil {
		return canonical
	}
	legacy := filepath.Join("baselines", subset+".json")
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	// Neither exists yet: return the canonical path (the read will fail and
	// the caller reports the dead gate) so a later capture lands here.
	return canonical
}
