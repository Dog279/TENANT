package eval

import (
	"fmt"
	"strings"
)

// TaskResult is one task's scoring outcome. Phase 1 shipped binary
// pass/fail; Phase 2 adds graded scores from the judge; Phase 4 adds
// pass^k aggregation and bootstrap CI. The shape stays additive across
// phases — old consumers ignore new fields.
type TaskResult struct {
	TaskID         string   `json:"task_id"`
	Category       string   `json:"category"`
	Passed         bool     `json:"passed"`
	Score          float64  `json:"score"` // 0-100
	Failures       []string `json:"failures,omitempty"`
	ElapsedMS      int64    `json:"elapsed_ms"`
	JudgeScore     int      `json:"judge_score,omitempty"`     // 1-5 anchored, 0 if no judge ran
	JudgeReasoning string   `json:"judge_reasoning,omitempty"` // judge's one-sentence rationale
	// Ungraded marks a task whose gate passed but whose judge was unusable
	// even after a retry (TEN-197). Excluded from pass/fail aggregates,
	// baselines, and baseline pairing — grader infrastructure must never
	// move the score or the trend.
	Ungraded bool `json:"ungraded,omitempty"`
	// Skipped marks a task this run's environment cannot attempt: a
	// must_call tool is absent from the live toolset (TEN-198). Same
	// exclusion discipline as Ungraded; SkipReason names the tool.
	Skipped    bool   `json:"skipped,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
}

// ScoreFixture runs the deterministic gate against a fixture and
// returns the result. No LLM judge in Phase 1 — that lands in Phase 2.
func ScoreFixture(t *Task) TaskResult {
	res := TaskResult{
		TaskID:   t.ID,
		Category: t.Category,
		Passed:   true,
		Score:    100,
	}

	gate := evalGate(t.Expected, fixtureCalls(t.Fixture), t.Fixture.Response)
	if len(gate) > 0 {
		res.Passed = false
		res.Score = 0
		res.Failures = gate
	}
	return res
}

// fixtureCalls converts the fixture's tool calls into the same shape
// the live runner will use — keeps the gate logic uniform across modes.
func fixtureCalls(f *Fixture) []callRecord {
	if f == nil {
		return nil
	}
	out := make([]callRecord, len(f.ToolCalls))
	for i, c := range f.ToolCalls {
		out[i] = callRecord{tool: c.Tool, args: c.Args}
	}
	return out
}

// callRecord is the internal-only shape the scorer reads. Decoupled
// from FixtureToolCall so Phase 2 can populate it from agent.Event
// stream without changing the scorer's signature.
type callRecord struct {
	tool string
	args string // JSON; substring-matched against ExpectedToolCall.ArgsContain
}

// evalGate runs the must_call / must_not_call / response_substring_any
// checks. Returns a list of human-readable failure descriptions; empty
// list means the gate passed.
func evalGate(exp Expected, calls []callRecord, response string) []string {
	var fail []string

	// must_call: every expected call must match SOME real call.
	for _, want := range exp.MustCall {
		if !findMatchingCall(want, calls) {
			fail = append(fail, fmt.Sprintf("must_call: %s with args containing %v not found", want.Tool, want.ArgsContain))
		}
	}

	// must_not_call: none of these may appear.
	if len(exp.MustNotCall) > 0 {
		banned := make(map[string]struct{}, len(exp.MustNotCall))
		for _, b := range exp.MustNotCall {
			banned[b] = struct{}{}
		}
		for _, c := range calls {
			if _, hit := banned[c.tool]; hit {
				fail = append(fail, fmt.Sprintf("must_not_call: %s was called (forbidden)", c.tool))
			}
		}
	}

	// response_substring_any: at least one of the listed substrings must appear (case-insensitive).
	if len(exp.ResponseSubstrAny) > 0 {
		lowResp := strings.ToLower(response)
		matched := false
		for _, s := range exp.ResponseSubstrAny {
			if strings.Contains(lowResp, strings.ToLower(s)) {
				matched = true
				break
			}
		}
		if !matched {
			fail = append(fail, fmt.Sprintf("response_substring_any: none of %v found in response", exp.ResponseSubstrAny))
		}
	}

	return fail
}

// findMatchingCall returns true when at least one real call has the
// right tool name AND its args string contains every required substring.
func findMatchingCall(want ExpectedToolCall, calls []callRecord) bool {
	for _, c := range calls {
		if c.tool != want.Tool {
			continue
		}
		if argsContainAll(c.args, want.ArgsContain) {
			return true
		}
	}
	return false
}

// argsContainAll checks that every required substring is present in the
// args (case-insensitive). If args is valid JSON we still substring-
// match the raw text — JSON-equivalence would be over-strict for what
// the rubric is asking ("did the call mention X?").
func argsContainAll(args string, needles []string) bool {
	if len(needles) == 0 {
		return true
	}
	low := strings.ToLower(args)
	for _, n := range needles {
		if !strings.Contains(low, strings.ToLower(n)) {
			return false
		}
	}
	return true
}
