package eval

import (
	"bytes"
	"testing"
)

// TestNewBaseline_SnapshotShape: NewBaseline captures per-task scores
// and overall as plain JSON-friendly values. Idempotent.
func TestNewBaseline_SnapshotShape(t *testing.T) {
	rep := &Report{
		Subset: SubsetFitness,
		Results: []TaskResult{
			{TaskID: "a", Score: 100},
			{TaskID: "b", Score: 50},
		},
		Aggregates: Aggregates{Overall: 75.0},
	}
	b := NewBaseline(rep, "2026-05-26T00:00:00Z", "judge-gemma4", "v1.0")
	if b.SchemaVersion != BaselineSchemaVersion {
		t.Errorf("schema: want %d, got %d", BaselineSchemaVersion, b.SchemaVersion)
	}
	if b.TaskScores["a"] != 100 || b.TaskScores["b"] != 50 {
		t.Errorf("task scores not captured: %v", b.TaskScores)
	}
	if b.Overall != 75.0 {
		t.Errorf("overall: want 75.0, got %.2f", b.Overall)
	}
}

// TestBaseline_RoundTrip: WriteTo → ReadBaseline preserves all fields.
func TestBaseline_RoundTrip(t *testing.T) {
	in := &Baseline{
		SchemaVersion: BaselineSchemaVersion,
		Subset:        SubsetFitness,
		CapturedAt:    "2026-05-26T00:00:00Z",
		JudgeProfile:  "judge-gemma4",
		TenantVersion: "v1.0",
		TaskScores:    map[string]float64{"a": 100, "b": 50},
		Overall:       75.0,
	}
	var buf bytes.Buffer
	if err := in.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	out, err := ReadBaseline(buf.Bytes())
	if err != nil {
		t.Fatalf("ReadBaseline: %v", err)
	}
	if out.TaskScores["a"] != 100 || out.Overall != 75 || out.JudgeProfile != "judge-gemma4" {
		t.Errorf("round-trip mismatch: %+v", out)
	}
}

// TestBaseline_SchemaVersionMismatch: a future-version baseline must
// fail to load loudly, not silently degrade.
func TestBaseline_SchemaVersionMismatch(t *testing.T) {
	data := []byte(`{"schema_version": 99, "subset": "fitness", "task_scores": {}, "overall": 0}`)
	if _, err := ReadBaseline(data); err == nil {
		t.Fatal("want schema-version error, got nil")
	}
}

// TestCompareToBaseline_NoRegressionOnEqual: baseline equals current →
// Δ=0, no regression. The 95% CI on Δ should bracket 0.
func TestCompareToBaseline_NoRegressionOnEqual(t *testing.T) {
	base := &Baseline{
		SchemaVersion: BaselineSchemaVersion,
		TaskScores:    map[string]float64{"a": 100, "b": 80, "c": 60, "d": 90, "e": 70},
	}
	current := &Report{Results: []TaskResult{
		{TaskID: "a", Score: 100}, {TaskID: "b", Score: 80},
		{TaskID: "c", Score: 60}, {TaskID: "d", Score: 90}, {TaskID: "e", Score: 70},
	}}
	rep := CompareToBaseline(base, current, CompareOptions{BootstrapN: 500, RandSeed: 42})
	if rep.Regressed {
		t.Errorf("equal scores flagged as regression: Δ=%.2f, CI=[%.2f, %.2f]", rep.Delta, rep.CILow, rep.CIHigh)
	}
	if rep.PairedCount != 5 {
		t.Errorf("paired count: want 5, got %d", rep.PairedCount)
	}
}

// TestCompareToBaseline_RegressionFlagged: every task drops by 20 →
// strong regression signal, both CI bounds below 0. This is the Phase 4
// DoD test ("deliberate one-line code change causes a regression flag").
func TestCompareToBaseline_RegressionFlagged(t *testing.T) {
	base := &Baseline{
		SchemaVersion: BaselineSchemaVersion,
		TaskScores:    map[string]float64{"a": 100, "b": 100, "c": 100, "d": 100, "e": 100, "f": 100, "g": 100, "h": 100, "i": 100, "j": 100},
	}
	current := &Report{Results: []TaskResult{
		{TaskID: "a", Score: 80}, {TaskID: "b", Score: 80},
		{TaskID: "c", Score: 80}, {TaskID: "d", Score: 80}, {TaskID: "e", Score: 80},
		{TaskID: "f", Score: 80}, {TaskID: "g", Score: 80}, {TaskID: "h", Score: 80},
		{TaskID: "i", Score: 80}, {TaskID: "j", Score: 80},
	}}
	rep := CompareToBaseline(base, current, CompareOptions{BootstrapN: 1000, RandSeed: 42})
	if !rep.Regressed {
		t.Fatalf("uniform -20 drop NOT flagged as regression: Δ=%.2f, CI=[%.2f, %.2f]", rep.Delta, rep.CILow, rep.CIHigh)
	}
	if rep.Delta > -19 || rep.Delta < -21 {
		t.Errorf("Δ: want ~-20, got %.2f", rep.Delta)
	}
}

// TestCompareToBaseline_MissingAndNewIDs: tasks added/removed since
// baseline are reported, not silently dropped.
func TestCompareToBaseline_MissingAndNewIDs(t *testing.T) {
	base := &Baseline{
		SchemaVersion: BaselineSchemaVersion,
		TaskScores:    map[string]float64{"a": 100, "b": 80, "removed": 50},
	}
	current := &Report{Results: []TaskResult{
		{TaskID: "a", Score: 100}, {TaskID: "b", Score: 80}, {TaskID: "added", Score: 90},
	}}
	rep := CompareToBaseline(base, current, CompareOptions{BootstrapN: 100, RandSeed: 1})
	if len(rep.MissingIDs) != 1 || rep.MissingIDs[0] != "removed" {
		t.Errorf("missing: %v", rep.MissingIDs)
	}
	if len(rep.NewIDs) != 1 || rep.NewIDs[0] != "added" {
		t.Errorf("new: %v", rep.NewIDs)
	}
}

// TestPassK: classic τ-Bench shape. 4 of 5 rollouts passed = 0.8.
func TestPassK(t *testing.T) {
	rollouts := []TaskResult{
		{Passed: true}, {Passed: true}, {Passed: true}, {Passed: true}, {Passed: false},
	}
	p, total := passK(rollouts)
	if p != 4 || total != 5 {
		t.Errorf("counts: want 4/5, got %d/%d", p, total)
	}
	if PassFraction(p, total) != 0.8 {
		t.Errorf("fraction: want 0.8, got %.2f", PassFraction(p, total))
	}
}

// TestPassFraction_EmptyTotal: no rollouts → fraction 0, no division-by-zero.
func TestPassFraction_EmptyTotal(t *testing.T) {
	if got := PassFraction(0, 0); got != 0 {
		t.Errorf("want 0, got %.2f", got)
	}
}
