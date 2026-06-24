package eval

import (
	"encoding/json"
	"strings"
	"testing"
)

// TEN-286: pass^k is now wired through the runner. These tests cover the
// aggregation logic (the runner loop is exercised end-to-end by the existing
// fixture tests, which all default to Rollouts=1).

func TestAggregateRollouts_PassK(t *testing.T) {
	task := &Task{ID: "t1", Category: "cat", Rollouts: 3}
	// 2 of 3 rollouts pass; scores 100/0/100 → mean ~66.7.
	rollouts := []TaskResult{
		{TaskID: "t1", Category: "cat", Passed: true, Score: 100, ElapsedMS: 10},
		{TaskID: "t1", Category: "cat", Passed: false, Score: 0, ElapsedMS: 20},
		{TaskID: "t1", Category: "cat", Passed: true, Score: 100, ElapsedMS: 30},
	}
	r := aggregateRollouts(task, rollouts)
	if r.Rollouts != 3 || r.RolloutsPassed != 2 {
		t.Fatalf("rollouts=%d passed=%d, want 3/2", r.Rollouts, r.RolloutsPassed)
	}
	if r.Passed {
		t.Error("pass^k with 2/3 passing must NOT be a clean Passed (strict)")
	}
	if pf := PassFraction(r.RolloutsPassed, r.Rollouts); pf < 0.66 || pf > 0.67 {
		t.Errorf("pass^3 = %.3f, want ~0.667", pf)
	}
	if r.Score < 66 || r.Score > 67 {
		t.Errorf("mean score = %.1f, want ~66.7", r.Score)
	}
	if r.ElapsedMS != 60 { // summed across rollouts
		t.Errorf("elapsed = %d, want 60 (sum)", r.ElapsedMS)
	}
}

func TestAggregateRollouts_CleanPass(t *testing.T) {
	task := &Task{ID: "t", Category: "c", Rollouts: 3}
	rollouts := []TaskResult{
		{Passed: true, Score: 100}, {Passed: true, Score: 90}, {Passed: true, Score: 95},
	}
	r := aggregateRollouts(task, rollouts)
	if !r.Passed || r.RolloutsPassed != 3 || r.Rollouts != 3 {
		t.Errorf("3/3 should be a clean pass^3, got passed=%v %d/%d", r.Passed, r.RolloutsPassed, r.Rollouts)
	}
}

func TestAggregateRollouts_ExcludesSkippedUngraded(t *testing.T) {
	task := &Task{ID: "t2", Category: "cat", Rollouts: 3}
	// Only the first rollout is graded; the skipped + ungraded ones must not
	// dilute the denominator.
	rollouts := []TaskResult{
		{TaskID: "t2", Passed: true, Score: 100},
		{TaskID: "t2", Skipped: true, SkipReason: "tool gone"},
		{TaskID: "t2", Ungraded: true},
	}
	r := aggregateRollouts(task, rollouts)
	if r.Rollouts != 1 || r.RolloutsPassed != 1 {
		t.Fatalf("graded denom = %d/%d, want 1/1 (skipped+ungraded excluded)", r.RolloutsPassed, r.Rollouts)
	}
	if !r.Passed || r.Skipped || r.Ungraded {
		t.Errorf("1/1 graded should be a clean pass, not skipped/ungraded: %+v", r)
	}
}

func TestAggregateRollouts_AllExcluded(t *testing.T) {
	task := &Task{ID: "t3", Category: "cat", Rollouts: 2}
	rollouts := []TaskResult{
		{TaskID: "t3", Skipped: true, SkipReason: "tool gone"},
		{TaskID: "t3", Skipped: true, SkipReason: "tool gone"},
	}
	r := aggregateRollouts(task, rollouts)
	if !r.Skipped || r.Rollouts != 0 {
		t.Errorf("all-excluded should surface Skipped with no pass^k, got skipped=%v rollouts=%d", r.Skipped, r.Rollouts)
	}
}

func TestAggregateRollouts_NoRollouts(t *testing.T) {
	r := aggregateRollouts(&Task{ID: "t4", Category: "c"}, nil)
	if !r.Skipped || r.TaskID != "t4" {
		t.Errorf("zero rollouts (cancelled) should record a Skipped result, got %+v", r)
	}
}

// Baseline stability: a default single-rollout result must NOT emit the new
// pass^k fields, or every existing baseline.json would churn.
func TestTaskResult_SingleRolloutJSONUnchanged(t *testing.T) {
	b, err := json.Marshal(TaskResult{TaskID: "x", Category: "c", Passed: true, Score: 100})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "rollouts") {
		t.Errorf("single-rollout result must not emit rollouts fields (baseline stability): %s", b)
	}
}
