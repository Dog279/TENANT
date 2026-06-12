package eval

// skip_test.go pins the TEN-198 contract: a live task requiring a tool the
// environment doesn't have is SKIPPED — recorded with a reason, excluded
// from pass/fail aggregates, baselines, and baseline pairing. ~20 of 57
// catalog tasks need plugins eval never auto-enables (interactive auth);
// scoring them zero measured the environment, not the agent.

import (
	"context"
	"strings"
	"testing"
)

func liveTask(id string, mustCall ...string) *Task {
	t := &Task{
		ID:       id,
		Category: "fitness/test",
		Mode:     ModeLive,
		Prompt:   "do it",
	}
	for _, tool := range mustCall {
		t.Expected.MustCall = append(t.Expected.MustCall, ExpectedToolCall{Tool: tool})
	}
	return t
}

func TestRunOne_SkipsWhenToolUnavailable(t *testing.T) {
	h := &Harness{AvailableTools: map[string]struct{}{"web_search": {}}}
	res := h.runOne(context.Background(), liveTask("t1", "imessage_read_chat"))
	if !res.Skipped {
		t.Fatalf("want Skipped, got %+v", res)
	}
	if !strings.Contains(res.SkipReason, "imessage_read_chat") {
		t.Errorf("SkipReason should name the missing tool, got %q", res.SkipReason)
	}
	if res.Passed {
		t.Error("skipped must not count as passed")
	}
}

func TestRunOne_RunsWhenToolsPresent(t *testing.T) {
	h := &Harness{
		AvailableTools: map[string]struct{}{"web_search": {}},
		AgentFactory: func(context.Context, string) (AgentRunner, func() error, error) {
			return ten197Runner{}, nil, nil
		},
	}
	res := h.runOne(context.Background(), liveTask("t2", "web_search"))
	if res.Skipped {
		t.Fatalf("tools present — must run, got skip: %q", res.SkipReason)
	}
}

func TestRunOne_NoMustCallNeverSkips(t *testing.T) {
	h := &Harness{
		AvailableTools: map[string]struct{}{}, // empty set, nothing available
		AgentFactory: func(context.Context, string) (AgentRunner, func() error, error) {
			return ten197Runner{}, nil, nil
		},
	}
	res := h.runOne(context.Background(), liveTask("t3"))
	if res.Skipped {
		t.Fatal("a task with no must_call is runnable regardless of toolset")
	}
}

func TestRunOne_NilSetDisablesSkipping(t *testing.T) {
	h := &Harness{
		AgentFactory: func(context.Context, string) (AgentRunner, func() error, error) {
			return ten197Runner{}, nil, nil
		},
	}
	res := h.runOne(context.Background(), liveTask("t4", "imessage_read_chat"))
	if res.Skipped {
		t.Fatal("nil AvailableTools must preserve pre-TEN-198 behavior (no skipping)")
	}
}

func TestRunOne_FixtureNeverSkips(t *testing.T) {
	task := &Task{
		ID:       "t5",
		Category: "smoke/test",
		Mode:     ModeFixture,
		Fixture: &Fixture{
			Response:  "done",
			ToolCalls: []FixtureToolCall{{Tool: "imessage_read_chat", Args: "{}"}},
		},
	}
	task.Expected.MustCall = []ExpectedToolCall{{Tool: "imessage_read_chat"}}
	h := &Harness{AvailableTools: map[string]struct{}{}} // nothing available
	res := h.runOne(context.Background(), task)
	if res.Skipped {
		t.Fatal("fixture tasks carry their own canned calls — never environment-skipped")
	}
	if !res.Passed {
		t.Fatalf("fixture should pass its own gate, got %+v", res)
	}
}

// Skipped tasks are invisible to every consumer that judges the agent.
func TestSkipped_ExcludedEverywhere(t *testing.T) {
	skipped := TaskResult{TaskID: "limbo", Category: "c", Skipped: true, SkipReason: "tool unavailable: x_post"}
	rep := &Report{
		Subset: SubsetFull,
		Results: []TaskResult{
			{TaskID: "good", Category: "c", Passed: true, Score: 100},
			{TaskID: "bad", Category: "c", Passed: false, Score: 0},
			skipped,
		},
	}
	rep.Aggregates = aggregate(rep.Results, 0)

	if rep.Aggregates.SkippedCount != 1 || rep.Aggregates.PassCount != 1 || rep.Aggregates.FailCount != 1 {
		t.Fatalf("aggregate = %+v, want pass 1 / fail 1 / skipped 1", rep.Aggregates)
	}
	if rep.Aggregates.Overall != 50 {
		t.Fatalf("overall = %.1f, want 50 (skipped excluded from the average)", rep.Aggregates.Overall)
	}
	if ids := FailedTaskIDs(rep); len(ids) != 1 || ids[0] != "bad" {
		t.Errorf("FailedTaskIDs = %v, want [bad] only", ids)
	}

	b := NewBaseline(rep, "now", "judge", "v")
	if _, ok := b.TaskScores["limbo"]; ok {
		t.Error("NewBaseline must not capture skipped tasks")
	}

	base := &Baseline{TaskScores: map[string]float64{"good": 100, "bad": 0, "limbo": 100}}
	rr := CompareToBaseline(base, rep, CompareOptions{})
	if rr.PairedCount != 2 {
		t.Fatalf("paired = %d, want 2 (skipped must not pair)", rr.PairedCount)
	}

	var sb strings.Builder
	WriteTerminal(&sb, rep)
	if !strings.Contains(sb.String(), "Skipped: 1") {
		t.Errorf("terminal should show skipped count, got:\n%s", sb.String())
	}
}
