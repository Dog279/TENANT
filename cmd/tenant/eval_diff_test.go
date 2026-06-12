package main

// eval_diff_test.go covers the TEN-198 movers diff: improved/declined
// tables with decliner autopsy, skipped/ungraded never pairing, and
// newest-artifact selection.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/eval"
)

func writeDiffFixtures(t *testing.T) (baselinePath, artifactDir string) {
	t.Helper()
	dir := t.TempDir()
	artifactDir = filepath.Join(dir, "eval-artifacts")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rep := eval.Report{
		Subset: eval.SubsetFull,
		Results: []eval.TaskResult{
			{TaskID: "up", Category: "fitness/web", Passed: true, Score: 100},
			{TaskID: "down", Category: "fitness/sql", Passed: false, Score: 0,
				Failures: []string{"agent run error: endpoint down"}, JudgeReasoning: "n/a"},
			{TaskID: "flat", Category: "fitness/os", Passed: false, Score: 0},
			{TaskID: "limbo", Category: "fitness/mem", Ungraded: true},
			{TaskID: "ghost", Category: "fitness/x", Skipped: true, SkipReason: "tool unavailable: x_post"},
		},
	}
	rep.Aggregates = eval.Aggregates{Overall: 50, PassCount: 1, FailCount: 2}

	// An older artifact that must NOT be picked, then the newest.
	old := filepath.Join(artifactDir, "eval-20260601-000000.json")
	if err := os.WriteFile(old, []byte(`{"subset":"full","results":[],"aggregates":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(rep)
	if err := os.WriteFile(filepath.Join(artifactDir, "eval-20260612-120000.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	base := eval.Baseline{
		SchemaVersion: 1,
		Subset:        eval.SubsetFull,
		CapturedAt:    "2026-06-10T00:00:00Z",
		Overall:       40,
		TaskScores: map[string]float64{
			"up": 50, "down": 50, "flat": 0, "limbo": 80, "ghost": 70,
		},
	}
	bdata, _ := json.Marshal(base)
	baselinePath = filepath.Join(dir, "base.json")
	if err := os.WriteFile(baselinePath, bdata, 0o644); err != nil {
		t.Fatal(err)
	}
	return baselinePath, artifactDir
}

func TestRenderBaselineDiff(t *testing.T) {
	baselinePath, artifactDir := writeDiffFixtures(t)
	out, err := renderBaselineDiff(baselinePath, artifactDir)
	if err != nil {
		t.Fatalf("renderBaselineDiff: %v", err)
	}

	for _, want := range []string{
		"eval-20260612-120000.json",    // newest artifact picked, not the older one
		"+50.0", "up", "[fitness/web]", // improved mover
		"-50.0", "down", // declined mover
		"agent run error: endpoint down", // decliner autopsy inline
		"paired: 3",                      // up, down, flat — limbo/ghost excluded
		"stuck at 0 in both",
		"ungraded now: 1",
		"skipped now: 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "limbo") || strings.Contains(out, "ghost") {
		t.Errorf("ungraded/skipped tasks must not appear as movers:\n%s", out)
	}
}

func TestRenderBaselineDiff_Errors(t *testing.T) {
	if _, err := renderBaselineDiff("", t.TempDir()); err == nil {
		t.Error("empty artifact dir must error with guidance")
	}
	_, artifactDir := writeDiffFixtures(t)
	if _, err := renderBaselineDiff(filepath.Join(t.TempDir(), "nope.json"), artifactDir); err == nil {
		t.Error("missing baseline must error with guidance")
	}
}
