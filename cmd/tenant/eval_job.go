package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"tenant/internal/eval"
	"tenant/internal/improve"
)

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
	regressed := false
	if data, rerr := os.ReadFile(j.baselinePath); rerr == nil {
		if base, berr := eval.ReadBaseline(data); berr == nil {
			rr := eval.CompareToBaseline(base, rep, eval.CompareOptions{})
			regressed = rr.Regressed
			if regressed {
				summary += fmt.Sprintf(" — REGRESSION (Δ %.1f, CI hi %.1f)", rr.Delta, rr.CIHigh)
			} else {
				summary += fmt.Sprintf(" — no regression (Δ %.1f)", rr.Delta)
			}
		} else {
			j.log.Warn("eval-nightly: baseline parse failed; skipping check", "err", berr)
		}
	}

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
