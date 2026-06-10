package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tenant/internal/eval"
	"tenant/internal/improve"
)

// resolveEvalCadence picks the nightly-eval interval (TEN-157): an explicitly-set
// --eval-every flag wins; else the persisted improve.eval_every (a duration
// string like "24h"); an empty or MALFORMED config value fails CLOSED to 0 (off)
// so a config typo can't brick launch or silently mis-arm the gate. log may be nil.
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
			log.Warn("ignoring malformed improve.eval_every; nightly eval stays off", "value", s, "err", err)
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
	log          *slog.Logger
}

// newEvalNightlyJob builds the nightly job from the live command's config:
// full subset, planner-default judge, artifacts under <data>/eval-artifacts,
// baseline at baselines/full.json (checked only if present).
func newEvalNightlyJob(c *commonFlags, pf *pluginFlags, log *slog.Logger) *evalNightlyJob {
	if log == nil {
		log = slog.Default()
	}
	return &evalNightlyJob{
		c:            c,
		pf:           pf,
		subset:       eval.SubsetFull,
		artifactDir:  filepath.Join(c.dataDir, "eval-artifacts"),
		baselinePath: filepath.Join("baselines", "full.json"),
		log:          log,
	}
}

func (j *evalNightlyJob) Name() string { return "eval-nightly" }

// Run executes the eval once, writes the artifact, and checks the baseline.
// Per the Job contract, an error here is recorded but does not stop the
// scheduler. eval never mutates durable state, so Changed is always false.
func (j *evalNightlyJob) Run(ctx context.Context) (improve.JobResult, error) {
	rep, err := runEvalToReport(ctx, j.c, j.pf, j.subset, j.jOpts)
	if err != nil {
		return improve.JobResult{Summary: "eval-nightly: run failed: " + err.Error()}, err
	}
	total := rep.Aggregates.PassCount + rep.Aggregates.FailCount

	// Timestamped JSON artifact. A write failure is logged, not fatal — the
	// run's value is the score, not the file.
	artifact := ""
	if mkerr := os.MkdirAll(j.artifactDir, 0o755); mkerr == nil {
		path := filepath.Join(j.artifactDir, "eval-"+time.Now().UTC().Format("20060102-150405")+".json")
		if f, ferr := os.Create(path); ferr == nil {
			_ = eval.WriteJSON(f, rep)
			_ = f.Close()
			artifact = path
		} else {
			j.log.Warn("eval-nightly: artifact write failed", "err", ferr)
		}
	}

	summary := fmt.Sprintf("eval-nightly %s: overall %.1f, passed %d/%d",
		rep.Subset, rep.Aggregates.Overall, rep.Aggregates.PassCount, total)
	regressed, haveBaseline := false, false
	var delta, ciHigh float64
	if data, rerr := os.ReadFile(j.baselinePath); rerr == nil {
		if base, berr := eval.ReadBaseline(data); berr == nil {
			rr := eval.CompareToBaseline(base, rep, eval.CompareOptions{})
			regressed, haveBaseline, delta, ciHigh = rr.Regressed, true, rr.Delta, rr.CIHigh
			if regressed {
				summary += fmt.Sprintf(" — REGRESSION (Δ %.1f, CI hi %.1f)", rr.Delta, rr.CIHigh)
			} else {
				summary += fmt.Sprintf(" — no regression (Δ %.1f)", rr.Delta)
			}
		} else {
			j.log.Warn("eval-nightly: baseline parse failed; skipping check", "err", berr)
		}
	}

	// Append a compact trend line (TEN-158): the durable record of THIS run's
	// regression verdict (delta/ci_high), which the heavy per-run artifacts don't
	// keep. Non-fatal — mirrors the artifact write.
	appendEvalTrend(j.artifactDir, evalTrendEntry{
		TS: time.Now().UTC().Format(time.RFC3339), Subset: string(rep.Subset),
		Overall: rep.Aggregates.Overall, Passed: rep.Aggregates.PassCount, Total: total,
		HasBaseline: haveBaseline, Regressed: regressed, Delta: delta, CIHigh: ciHigh,
		Artifact: filepath.Base(artifact),
	}, j.log)

	return improve.JobResult{
		Summary: summary,
		Changed: false,
		Details: map[string]any{
			"overall":   rep.Aggregates.Overall,
			"passed":    rep.Aggregates.PassCount,
			"total":     total,
			"artifact":  artifact,
			"regressed": regressed,
		},
	}, nil
}
