package eval

// judge_test.go pins the TEN-197 contract: a thinking model's empty or
// truncated verdict must never score the agent zero. The judge retries
// once, salvages truncated JSON, and an unusable judge yields UNGRADED —
// excluded from aggregates, baselines, and baseline pairing. Found live:
// 20 of 49 failures on the first real full run were grader failures, one
// of which discarded a verdict that literally said "score": 5.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"tenant/internal/model"
)

func TestParseJudgeOutput(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantScore int
		wantErr   bool
	}{
		{"clean JSON", `{"score": 4, "reasoning": "solid"}`, 4, false},
		{"fenced JSON", "```json\n{\"score\": 3, \"reasoning\": \"ok\"}\n```", 3, false},
		{
			// The live bug: truncation never closes the brace, so the
			// object regex can't see it — the score scrape must rescue it.
			"truncated JSON with score (the thrown-away PASS)",
			`{"score": 5, "reasoning": "All four tool calls were correct and the resp`,
			5, false,
		},
		{"score amid prose", `I'd give this a {"score": 2, "reasoning": "weak"} overall`, 2, false},
		{"empty output (thinking burn)", "", 0, true},
		{"whitespace only", "   \n\t  ", 0, true},
		{"garbage with no score", "the agent did fine I suppose", 0, true},
		{"clamp above 5", `{"score": 9, "reasoning": "overenthusiastic"}`, 5, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			jr, err := parseJudgeOutput(c.raw)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got score %d", jr.Score)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if jr.Score != c.wantScore {
				t.Fatalf("score = %d, want %d", jr.Score, c.wantScore)
			}
		})
	}
}

// scriptedLLM returns queued outputs/errors per Generate call — the
// minimal model.LLM for judge retry tests.
type scriptedLLM struct {
	outputs []string
	errs    []error
	calls   int
}

func (s *scriptedLLM) Generate(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	out := ""
	if i < len(s.outputs) {
		out = s.outputs[i]
	}
	return &model.GenerateResponse{Text: out}, nil
}

func (s *scriptedLLM) GenerateStream(context.Context, model.GenerateRequest) (<-chan model.StreamChunk, error) {
	return nil, errors.New("scriptedLLM: streaming not supported")
}

func (s *scriptedLLM) TokenCount(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}

func judgeTask() *Task {
	return &Task{
		ID:       "t-judge",
		Category: "fitness/test",
		Prompt:   "do the thing",
		Expected: Expected{Rubric: &Rubric{Criterion: "did it do the thing"}},
	}
}

func TestLLMJudge_RetryOnEmptyThenSuccess(t *testing.T) {
	llm := &scriptedLLM{outputs: []string{"", `{"score": 4, "reasoning": "fine"}`}}
	j := &LLMJudge{LLM: llm}
	jr, err := j.Grade(context.Background(), judgeTask(), "resp", nil)
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if jr.Score != 4 || llm.calls != 2 {
		t.Fatalf("score=%d calls=%d, want 4 after exactly 2 calls", jr.Score, llm.calls)
	}
}

func TestLLMJudge_RetryOnCallErrorThenSuccess(t *testing.T) {
	llm := &scriptedLLM{
		errs:    []error{errors.New("endpoint down"), nil},
		outputs: []string{"", `{"score": 3, "reasoning": "ok"}`},
	}
	j := &LLMJudge{LLM: llm}
	jr, err := j.Grade(context.Background(), judgeTask(), "resp", nil)
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if jr.Score != 3 || llm.calls != 2 {
		t.Fatalf("score=%d calls=%d, want 3 after 2 calls", jr.Score, llm.calls)
	}
}

func TestLLMJudge_UnusableAfterRetry(t *testing.T) {
	llm := &scriptedLLM{outputs: []string{"", ""}}
	j := &LLMJudge{LLM: llm}
	_, err := j.Grade(context.Background(), judgeTask(), "resp", nil)
	if err == nil {
		t.Fatal("want error after two empty outputs")
	}
	if llm.calls != 2 {
		t.Fatalf("calls=%d, want exactly 2 (one retry, no spinning)", llm.calls)
	}
	if !strings.Contains(err.Error(), "empty output") {
		t.Errorf("error should explain the empty output, got %v", err)
	}
}

// errJudge always fails — the runLive ungraded path's stand-in for a
// judge that's unusable after its retry.
type errJudge struct{}

func (errJudge) Grade(context.Context, *Task, string, []FixtureToolCall) (JudgeResult, error) {
	return JudgeResult{}, errors.New("judge: empty output (twice)")
}

type ten197Runner struct{}

func (ten197Runner) Run(context.Context, string) (string, []FixtureToolCall, error) {
	return "a perfectly fine answer", nil, nil
}

// A gate-passing task whose judge is unusable lands UNGRADED: not passed,
// not failed, never aggregated — and visibly marked.
func TestRunLive_JudgeUnusableMeansUngraded(t *testing.T) {
	h := &Harness{
		Judge: errJudge{},
		AgentFactory: func(context.Context, string) (AgentRunner, func() error, error) {
			return ten197Runner{}, nil, nil
		},
	}
	res := h.runLive(context.Background(), judgeTask())
	if !res.Ungraded {
		t.Fatalf("want Ungraded, got %+v", res)
	}
	if res.Passed {
		t.Error("ungraded must not count as passed")
	}
	if len(res.Failures) == 0 || !strings.Contains(res.Failures[0], "UNGRADED") {
		t.Errorf("failures should mark UNGRADED, got %v", res.Failures)
	}

	ag := aggregate([]TaskResult{
		{TaskID: "a", Category: "c", Passed: true, Score: 100},
		{TaskID: "b", Category: "c", Passed: false, Score: 0},
		res,
	}, 0)
	if ag.PassCount != 1 || ag.FailCount != 1 || ag.UngradedCount != 1 {
		t.Fatalf("aggregate = pass %d fail %d ungraded %d, want 1/1/1", ag.PassCount, ag.FailCount, ag.UngradedCount)
	}
	if ag.Overall != 50 {
		t.Fatalf("overall = %.1f, want 50 (ungraded excluded from the average)", ag.Overall)
	}
}

// Ungraded tasks are invisible to baselines: not captured, never paired.
func TestBaseline_ExcludesUngraded(t *testing.T) {
	rep := &Report{
		Subset: SubsetFull,
		Results: []TaskResult{
			{TaskID: "graded", Category: "c", Passed: true, Score: 100},
			{TaskID: "limbo", Category: "c", Ungraded: true, Score: 0},
		},
	}
	rep.Aggregates = aggregate(rep.Results, 0)

	b := NewBaseline(rep, "now", "judge", "v")
	if _, ok := b.TaskScores["limbo"]; ok {
		t.Error("NewBaseline must not capture ungraded tasks")
	}
	if _, ok := b.TaskScores["graded"]; !ok {
		t.Error("NewBaseline must keep graded tasks")
	}

	// Pairing: a baseline that knows both tasks pairs only the graded one —
	// otherwise the ungraded zero manufactures a fake regression.
	base := &Baseline{TaskScores: map[string]float64{"graded": 100, "limbo": 100}}
	rr := CompareToBaseline(base, rep, CompareOptions{})
	if rr.PairedCount != 1 {
		t.Fatalf("paired = %d, want 1 (ungraded must not pair)", rr.PairedCount)
	}
	if rr.Regressed {
		t.Error("no regression: the only paired task is unchanged")
	}
}

// The terminal summary surfaces grader trouble instead of hiding it in
// a smaller denominator.
func TestWriteTerminal_ShowsUngraded(t *testing.T) {
	rep := &Report{
		Subset: SubsetFull,
		Results: []TaskResult{
			{TaskID: "a", Category: "c", Passed: true, Score: 100},
			{TaskID: "b", Category: "c", Ungraded: true},
		},
	}
	rep.Aggregates = aggregate(rep.Results, 0)
	var sb strings.Builder
	WriteTerminal(&sb, rep)
	if !strings.Contains(sb.String(), "Ungraded: 1") {
		t.Errorf("terminal output should show ungraded count, got:\n%s", sb.String())
	}
	if got := FailedTaskIDs(rep); len(got) != 0 {
		t.Errorf("FailedTaskIDs should skip ungraded, got %v", got)
	}
	if !AllPassed(rep) {
		t.Error("AllPassed should hold: one pass, one ungraded, zero fails")
	}
}
