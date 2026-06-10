package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/eval"
)

// TestEvalNightlyJob_SmokeArtifact runs the nightly job against the smoke
// subset (fixture mode — no model/tools/judge), so it deterministically
// exercises the Job mechanics: run -> write artifact -> JobResult. Live
// subsets need a model and are covered by the cmdEval path.
func TestEvalNightlyJob_SmokeArtifact(t *testing.T) {
	tmp := t.TempDir()
	j := &evalNightlyJob{
		c:            &commonFlags{dataDir: tmp},
		pf:           &pluginFlags{},
		subset:       eval.SubsetSmoke,
		artifactDir:  filepath.Join(tmp, "artifacts"),
		baselinePath: filepath.Join(tmp, "does-not-exist.json"), // no check
		log:          slog.Default(),
	}

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Changed {
		t.Error("eval job must report Changed=false (it observes, never mutates)")
	}
	if got, _ := res.Details["overall"].(float64); got != 100 {
		t.Errorf("overall = %v, want 100 (smoke fixtures all pass)", res.Details["overall"])
	}
	if got, _ := res.Details["regressed"].(bool); got {
		t.Error("regressed = true, want false (no baseline present)")
	}

	// Exactly one timestamped report artifact must have been written (the
	// nightly also appends trend.jsonl in the same dir — TEN-158 — which is not
	// a report artifact and is excluded here).
	entries, err := os.ReadDir(j.artifactDir)
	if err != nil {
		t.Fatalf("read artifact dir: %v", err)
	}
	reports, sawTrend := 0, false
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "eval-") && strings.HasSuffix(e.Name(), ".json"):
			reports++
		case e.Name() == "trend.jsonl":
			sawTrend = true
		}
	}
	if reports != 1 {
		t.Fatalf("want exactly 1 report artifact in %s, got %d (entries: %v)", j.artifactDir, reports, entries)
	}
	if !sawTrend {
		t.Error("nightly run should have appended trend.jsonl (TEN-158)")
	}
}
