package eval

import (
	"context"
	"errors"
	"fmt"

	"tenant/internal/model"
)

// AgentRunner is the seam between eval and the agent runtime. The eval
// package intentionally does NOT import internal/agent — keeping the
// dependency graph clean lets eval be reusable by future tools (Job
// fitness loops, GEPA, external benchmark runners) without dragging
// the full agent construction surface.
//
// Implementations live in cmd/tenant/eval.go (production: real agent +
// Router) and in internal/eval/eval_test.go (test: deterministic fake).
type AgentRunner interface {
	// Run executes one task prompt against an isolated agent and
	// reports back what tools were called and what the agent said.
	Run(ctx context.Context, prompt string) (response string, calls []FixtureToolCall, err error)
}

// AgentFactory constructs a fresh AgentRunner for one task. The
// returned cleanup func is called after the task completes (or fails)
// so resource cleanup is the operator's responsibility, not the
// harness's. Phase 5's `eval.Run(ctx, h, factory, sub)` calls this
// once per task per rollout — never reused — so SoulNudgeJob's
// "candidate Soul" injection works cleanly.
type AgentFactory func(ctx context.Context, taskID string) (AgentRunner, func() error, error)

// runLive drives one live-mode task: build agent, run prompt, score
// against deterministic gate, then score against rubric (if both gate
// and judge are configured). The path mirrors fixture mode's gate so
// the scorer logic stays single-source.
// classifyRunError maps a run-level error to an infrastructure class, or ""
// when the failure is (as far as we can tell) the agent's own. Typed model
// errors survive the wrap chain (fmt.Errorf %w all the way up through
// "agent: planner generate iter=N"), so this is errors.Is, not string
// scraping. Context timeouts stay agent failures on purpose: the per-task
// deadline firing usually means the agent looped or hung, which IS quality.
func classifyRunError(err error) string {
	switch {
	case errors.Is(err, model.ErrInsufficientBalance):
		return "quota"
	case errors.Is(err, model.ErrEndpointDown):
		return "endpoint"
	case errors.Is(err, model.ErrRateLimited):
		return "ratelimit"
	case errors.Is(err, model.ErrInternal):
		// Every provider in the failover chain returned 5xx. The fallback
		// engine already treats this as an availability class worth failing
		// over; the score treats it the same way.
		return "provider"
	}
	return ""
}

func (h *Harness) runLive(ctx context.Context, t *Task) TaskResult {
	res := TaskResult{
		TaskID:   t.ID,
		Category: t.Category,
		Passed:   true,
		Score:    100,
	}
	if h.AgentFactory == nil {
		res.Passed = false
		res.Score = 0
		res.Failures = []string{"live-mode task requires Harness.AgentFactory; none configured"}
		return res
	}
	runner, cleanup, err := h.AgentFactory(ctx, t.ID)
	if err != nil {
		// The harness could not even build the agent - nothing about the
		// agent was measured. Same exclusion discipline as TEN-197.
		res.Ungraded = true
		res.ErrorClass = "harness"
		res.Passed = false
		res.Score = 0
		res.Failures = []string{fmt.Sprintf("UNGRADED (harness) - agent factory error: %v", err)}
		return res
	}
	if cleanup != nil {
		defer cleanup()
	}

	response, calls, err := runner.Run(ctx, t.Prompt)
	if err != nil {
		if class := classifyRunError(err); class != "" {
			// Provider outage, not agent failure: quota exhaustion, endpoint
			// down, rate limiting. Excluded from the mean and the baselines,
			// surfaced per class so the trend says "12 ungraded: quota"
			// instead of dropping 23 points.
			res.Ungraded = true
			res.ErrorClass = class
			res.Passed = false
			res.Score = 0
			res.Failures = []string{fmt.Sprintf("UNGRADED (%s) - %v", class, err)}
			return res
		}
		res.Passed = false
		res.Score = 0
		res.Failures = []string{fmt.Sprintf("agent run error: %v", err)}
		return res
	}

	// Deterministic gate (same logic as fixture mode).
	callRecs := make([]callRecord, len(calls))
	for i, c := range calls {
		callRecs[i] = callRecord{tool: c.Tool, args: c.Args}
	}
	gate := evalGate(t.Expected, callRecs, response)
	if len(gate) > 0 {
		res.Passed = false
		res.Score = 0
		res.Failures = gate
		return res // skip judge — save tokens
	}

	// Judge — only if rubric + judge are configured.
	if t.Expected.Rubric != nil && h.Judge != nil {
		jr, err := h.Judge.Grade(ctx, t, response, calls)
		if err != nil {
			// UNGRADED (TEN-197): the gate passed but the judge was unusable
			// even after its retry. Scoring the agent here (the old 50) moved
			// the trend for the GRADER's failure — first live run had 20/49
			// failures of this class. The task is excluded from pass/fail
			// aggregates and baselines, surfaced via Aggregates.UngradedCount.
			res.Ungraded = true
			// Name WHY the judge was unusable when the cause is infra: the 08-24
			// run reported 39 x "judge" when the truth was "judge out of quota" -
			// which the operator can fix, unlike a mystery grader failure.
			if c := classifyRunError(err); c != "" {
				res.ErrorClass = "judge-" + c
			}
			res.Passed = false
			res.Score = 0 // never aggregated; value is moot by construction
			res.JudgeScore = 0
			res.Failures = []string{fmt.Sprintf("UNGRADED — judge unusable after retry: %v (gate passed)", err)}
			return res
		}
		res.JudgeScore = jr.Score
		res.JudgeReasoning = jr.Reasoning
		min := t.Expected.RubricMinScore
		if min == 0 {
			min = 3 // safe default
		}
		if jr.Score < min {
			res.Passed = false
			// Score scales between 50 (gate passed, missed rubric) and 100
			// (gate passed AND rubric perfect) — see plan v1 §Scoring.
			res.Score = 50
			res.Failures = []string{fmt.Sprintf("rubric score %d < min %d (%s)", jr.Score, min, jr.Reasoning)}
		} else {
			// Linear scale from rubric_min..5 ↦ 70..100.
			span := 5 - min
			if span <= 0 {
				res.Score = 100
			} else {
				res.Score = 70 + 30*float64(jr.Score-min)/float64(span)
			}
		}
	}
	return res
}
