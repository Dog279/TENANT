package main

// souljob_wire.go is the production fitness scorer for SoulNudgeJob (TEN-16): it
// runs the fitness suite with a CANDIDATE soul and compares to the committed
// fitness baseline (captured with the current soul), so a candidate must not
// regress to pass the gate. Model-gated — registered only when a real cadence is
// set, suppressed while the model is degraded, and it FAILS CLOSED if no fitness
// baseline exists yet (so nothing is proposed until the operator captures one).

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"tenant/internal/eval"
	"tenant/internal/improve"
	"tenant/internal/memory/soul"
)

var _ improve.SoulScorer = evalSoulScorer{}

type evalSoulScorer struct {
	c            *commonFlags
	pf           *pluginFlags
	jOpts        evalJudgeOpts
	baselinePath string // baselines/fitness.json
	log          *slog.Logger
}

// Score implements improve.SoulScorer.
func (s evalSoulScorer) Score(ctx context.Context, candidate *soul.Soul) (bool, float64, error) {
	// Read the baseline FIRST — fail fast before the expensive live run if the
	// operator hasn't captured a fitness baseline yet.
	data, err := os.ReadFile(s.baselinePath)
	if err != nil {
		return false, 0, fmt.Errorf("no fitness baseline at %s (capture one first): %w", s.baselinePath, err)
	}
	base, err := eval.ReadBaseline(data)
	if err != nil {
		return false, 0, fmt.Errorf("parse fitness baseline: %w", err)
	}
	rep, err := runEvalToReportWithSoul(ctx, s.c, s.pf, eval.SubsetFitness, s.jOpts, candidate)
	if err != nil {
		return false, 0, fmt.Errorf("run fitness with candidate soul: %w", err)
	}
	rr := eval.CompareToBaseline(base, rep, eval.CompareOptions{})
	return rr.Regressed, rr.Delta, nil
}
