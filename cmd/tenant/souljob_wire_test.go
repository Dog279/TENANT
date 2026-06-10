package main

import (
	"context"
	"path/filepath"
	"testing"

	"tenant/internal/memory/soul"
)

// The most operationally likely failure: the operator arms SoulNudge before
// capturing a fitness baseline. evalSoulScorer must fail closed (error, no
// regression verdict) — and it reads the baseline FIRST, so it never triggers
// the expensive live run when the baseline is absent.
func TestEvalSoulScorer_MissingBaselineFailsClosed(t *testing.T) {
	s := evalSoulScorer{
		baselinePath: filepath.Join(t.TempDir(), "fitness.json"), // does not exist
	}
	regressed, delta, err := s.Score(context.Background(), &soul.Soul{Agent: soul.Agent{ID: "main"}})
	if err == nil {
		t.Fatal("missing baseline must return an error (fail closed)")
	}
	if regressed || delta != 0 {
		t.Errorf("on error, expected (false, 0), got (%v, %v)", regressed, delta)
	}
}
