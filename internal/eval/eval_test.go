package eval

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"tenant/internal/model"
	"tenant/internal/model/testllm"
)

// TestLoadHarness_EmbeddedSmokeTasks: the shipped smoke tasks must load
// cleanly. If a YAML breaks, this test catches it before the binary
// ships. Phase 1 DoD: 5 smoke tasks.
func TestLoadHarness_EmbeddedSmokeTasks(t *testing.T) {
	h, err := LoadHarness(EmbeddedTasks, nil)
	if err != nil {
		t.Fatalf("load embedded harness: %v", err)
	}
	got := len(h.FilterSubset(SubsetSmoke))
	if got != 5 {
		t.Fatalf("smoke subset: want 5 tasks, got %d", got)
	}
}

// TestRun_AllSmokeTasksPass: every shipped smoke task must pass against
// its own fixture. This is the Phase 1 DoD's deterministic check —
// `tenant eval --subset smoke` MUST exit clean against a known-good
// build.
func TestRun_AllSmokeTasksPass(t *testing.T) {
	h, err := LoadHarness(EmbeddedTasks, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rep, err := h.Run(context.Background(), SubsetSmoke)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !AllPassed(rep) {
		t.Fatalf("smoke subset failed: %d/%d failures: %v",
			rep.Aggregates.FailCount,
			rep.Aggregates.PassCount+rep.Aggregates.FailCount,
			FailedTaskIDs(rep))
	}
	if rep.Aggregates.Overall != 100 {
		t.Errorf("overall: want 100, got %.1f", rep.Aggregates.Overall)
	}
}

// TestLoadTask_RejectsUnknownFields catches typos in task YAML at load
// time instead of silently dropping the field. Confirms KnownFields(true)
// is wired correctly.
func TestLoadTask_RejectsUnknownFields(t *testing.T) {
	yaml := []byte(`
id: typo-task
category: test
subset: smoke
mode: fixture
prompt: "x"
fixturee:  # intentional typo
  tool_calls: []
  response: "y"
expected: {}
`)
	_, err := LoadTask(yaml, "typo.yaml")
	if err == nil {
		t.Fatal("want error for unknown field 'fixturee', got nil")
	}
}

// TestLoadTask_RequiresPrompt: a task without a prompt is malformed.
func TestLoadTask_RequiresPrompt(t *testing.T) {
	yaml := []byte(`
id: no-prompt
category: test
subset: smoke
mode: fixture
fixture:
  tool_calls: []
  response: "y"
expected: {}
`)
	_, err := LoadTask(yaml, "no-prompt.yaml")
	if err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("want prompt-required error, got %v", err)
	}
}

// TestLoadTask_LiveModeRequiresRubric: Phase 2 accepts live tasks but
// they MUST carry an anchored rubric — fixture mode was free-form by
// design, live mode demands the rubric so the judge has anchors to
// score against.
func TestLoadTask_LiveModeRequiresRubric(t *testing.T) {
	yaml := []byte(`
id: live-task-no-rubric
category: test
subset: fitness
mode: live
prompt: "x"
expected: {}
`)
	_, err := LoadTask(yaml, "live-no-rubric.yaml")
	if err == nil || !strings.Contains(err.Error(), "rubric") {
		t.Fatalf("want rubric-required error, got %v", err)
	}
}

// TestLoadTask_LiveModeWithRubric: a well-formed live task loads.
func TestLoadTask_LiveModeWithRubric(t *testing.T) {
	yaml := []byte(`
id: live-task-good
category: test
subset: fitness
mode: live
prompt: "what is 2+2"
expected:
  rubric:
    criterion: "Answer correctly identifies 4."
    anchors:
      1: "Wrong answer or refusal"
      3: "Says 4 but adds noise"
      5: "Just says 4 with no padding"
  rubric_min_score: 3
`)
	tk, err := LoadTask(yaml, "live-good.yaml")
	if err != nil {
		t.Fatalf("want load OK, got %v", err)
	}
	if tk.Mode != ModeLive {
		t.Errorf("mode: want live, got %q", tk.Mode)
	}
	if tk.Expected.Rubric == nil || tk.Expected.Rubric.Criterion == "" {
		t.Errorf("rubric not loaded: %+v", tk.Expected.Rubric)
	}
}

// TestScoreFixture_MustCallMatch: a tool call with all required arg
// substrings should match.
func TestScoreFixture_MustCallMatch(t *testing.T) {
	task := &Task{
		ID: "t", Category: "c", Subset: SubsetSmoke, Mode: ModeFixture,
		Prompt:   "x",
		Fixture:  &Fixture{ToolCalls: []FixtureToolCall{{Tool: "web_navigate", Args: `{"url":"https://example.com"}`}}, Response: "ok"},
		Expected: Expected{MustCall: []ExpectedToolCall{{Tool: "web_navigate", ArgsContain: []string{"example.com"}}}, ResponseSubstrAny: []string{"ok"}},
	}
	res := ScoreFixture(task)
	if !res.Passed {
		t.Fatalf("want pass, got failures: %v", res.Failures)
	}
}

// TestScoreFixture_MustNotCallTriggers: a forbidden tool's presence
// must fail the gate.
func TestScoreFixture_MustNotCallTriggers(t *testing.T) {
	task := &Task{
		ID: "t", Category: "c", Subset: SubsetSmoke, Mode: ModeFixture,
		Prompt:   "x",
		Fixture:  &Fixture{ToolCalls: []FixtureToolCall{{Tool: "sql_exec", Args: `{}`}}, Response: "ok"},
		Expected: Expected{MustNotCall: []string{"sql_exec"}, ResponseSubstrAny: []string{"ok"}},
	}
	res := ScoreFixture(task)
	if res.Passed {
		t.Fatal("want fail, got pass")
	}
	joined := strings.Join(res.Failures, "; ")
	if !strings.Contains(joined, "sql_exec") {
		t.Errorf("want failure to name sql_exec, got %q", joined)
	}
}

// TestScoreFixture_SubstringAnyMissing: response without any of the
// required substrings should fail.
func TestScoreFixture_SubstringAnyMissing(t *testing.T) {
	task := &Task{
		ID: "t", Category: "c", Subset: SubsetSmoke, Mode: ModeFixture,
		Prompt:   "x",
		Fixture:  &Fixture{ToolCalls: nil, Response: "totally unrelated"},
		Expected: Expected{ResponseSubstrAny: []string{"foo", "bar"}},
	}
	res := ScoreFixture(task)
	if res.Passed {
		t.Fatal("want fail, got pass")
	}
}

// TestRun_FilterSubset confirms that running with one subset doesn't
// pull in tasks from another. Critical for the smoke=fast guarantee.
func TestRun_FilterSubset(t *testing.T) {
	// Synthetic FS with one smoke + one fitness task.
	fs := fstest.MapFS{
		"tasks/smoke/a.yaml": &fstest.MapFile{Data: []byte(`
id: s-1
category: test
subset: smoke
mode: fixture
prompt: x
fixture:
  tool_calls: []
  response: ok
expected: {response_substring_any: [ok]}
`)},
		"tasks/fitness/b.yaml": &fstest.MapFile{Data: []byte(`
id: f-1
category: test
subset: fitness
mode: fixture
prompt: x
fixture:
  tool_calls: []
  response: ok
expected: {response_substring_any: [ok]}
`)},
	}
	h, err := LoadHarness(fs, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := len(h.FilterSubset(SubsetSmoke)); got != 1 {
		t.Errorf("smoke: want 1, got %d", got)
	}
	if got := len(h.FilterSubset(SubsetFitness)); got != 1 {
		t.Errorf("fitness: want 1, got %d", got)
	}
}

// TestScoreFixture_ArgsContainPartial: args_contain is AND-of-substrings.
// A call missing one required substring must fail the gate. Regression
// guard against a refactor accidentally turning AND into OR.
func TestScoreFixture_ArgsContainPartial(t *testing.T) {
	task := &Task{
		ID: "t", Category: "c", Subset: SubsetSmoke, Mode: ModeFixture,
		Prompt:  "x",
		Fixture: &Fixture{ToolCalls: []FixtureToolCall{{Tool: "web_navigate", Args: `{"url":"https://example.com"}`}}, Response: "ok"},
		Expected: Expected{
			MustCall:          []ExpectedToolCall{{Tool: "web_navigate", ArgsContain: []string{"example.com", "missing-substring"}}},
			ResponseSubstrAny: []string{"ok"},
		},
	}
	res := ScoreFixture(task)
	if res.Passed {
		t.Fatal("want fail (one required substring absent), got pass")
	}
}

// TestScoreFixture_ArgsContainCaseInsensitive: needle matching ignores
// case so an agent calling web_navigate with "URL":"..." still matches
// args_contain ["url"]. Mirrors how operators write rubric assertions.
func TestScoreFixture_ArgsContainCaseInsensitive(t *testing.T) {
	task := &Task{
		ID: "t", Category: "c", Subset: SubsetSmoke, Mode: ModeFixture,
		Prompt:  "x",
		Fixture: &Fixture{ToolCalls: []FixtureToolCall{{Tool: "web_navigate", Args: `{"URL":"X"}`}}, Response: "ok"},
		Expected: Expected{
			MustCall:          []ExpectedToolCall{{Tool: "web_navigate", ArgsContain: []string{"url"}}},
			ResponseSubstrAny: []string{"ok"},
		},
	}
	res := ScoreFixture(task)
	if !res.Passed {
		t.Fatalf("want pass (case-insensitive needle match), got failures: %v", res.Failures)
	}
}

// TestAggregate_Direct exercises the aggregator outside the Run flow.
// Phase 4 builds bootstrap CI on this exact shape — locking the
// behavior in now means later changes are visible diff.
func TestAggregate_Direct(t *testing.T) {
	results := []TaskResult{
		{TaskID: "a", Category: "cat1", Passed: true, Score: 100},
		{TaskID: "b", Category: "cat1", Passed: false, Score: 0},
		{TaskID: "c", Category: "cat2", Passed: true, Score: 100},
	}
	ag := aggregate(results, 123)
	if ag.PassCount != 2 || ag.FailCount != 1 {
		t.Errorf("counts: want 2 pass / 1 fail, got %d / %d", ag.PassCount, ag.FailCount)
	}
	if ag.Overall < 66.0 || ag.Overall > 67.0 {
		t.Errorf("overall: want ~66.67, got %.2f", ag.Overall)
	}
	if ag.PerCategory["cat1"] != 50.0 {
		t.Errorf("cat1: want 50.0, got %.2f", ag.PerCategory["cat1"])
	}
	if ag.PerCategory["cat2"] != 100.0 {
		t.Errorf("cat2: want 100.0, got %.2f", ag.PerCategory["cat2"])
	}
	if ag.TotalElapsed != 123 {
		t.Errorf("elapsed: want 123, got %d", ag.TotalElapsed)
	}
}

// TestAggregate_EmptyResults: zero tasks must not panic or produce NaN.
func TestAggregate_EmptyResults(t *testing.T) {
	ag := aggregate(nil, 0)
	if ag.PassCount != 0 || ag.FailCount != 0 || ag.Overall != 0 {
		t.Errorf("empty aggregate not zero: %+v", ag)
	}
}

// TestRun_CtxCancellation: an already-cancelled context produces an
// empty Results slice (no tasks ran). Basic concurrency hygiene; Phase 5
// agent runs will rely on this contract.
func TestRun_CtxCancellation(t *testing.T) {
	h, err := LoadHarness(EmbeddedTasks, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	rep, err := h.Run(ctx, SubsetSmoke)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Results) != 0 {
		t.Errorf("cancelled ctx: want 0 results, got %d", len(rep.Results))
	}
}

// TestLoadTask_DefaultsApplied: rollouts=0 and missing weight are
// filled in by the validator, not surfaced as-is.
func TestLoadTask_DefaultsApplied(t *testing.T) {
	yaml := []byte(`
id: defaults-test
category: test
subset: smoke
mode: fixture
prompt: x
fixture:
  tool_calls: []
  response: ok
expected: {response_substring_any: [ok]}
`)
	tk, err := LoadTask(yaml, "defaults.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if tk.Rollouts != 1 {
		t.Errorf("rollouts default: want 1, got %d", tk.Rollouts)
	}
	if tk.Weight != 1.0 {
		t.Errorf("weight default: want 1.0, got %.2f", tk.Weight)
	}
}

// TestLoadTask_InvalidSubset: subset must be one of the known constants.
// A typo here would silently produce a task that's never picked up by
// any FilterSubset call.
func TestLoadTask_InvalidSubset(t *testing.T) {
	yaml := []byte(`
id: bad-subset
category: test
subset: bogus
mode: fixture
prompt: x
fixture:
  tool_calls: []
  response: ok
expected: {}
`)
	_, err := LoadTask(yaml, "bogus.yaml")
	if err == nil || !strings.Contains(err.Error(), "subset") {
		t.Fatalf("want invalid-subset error, got %v", err)
	}
}

// TestSubset_IsValid catches typos at the CLI layer. Mirror of the
// validation surface used by cmdEval.
func TestSubset_IsValid(t *testing.T) {
	for _, s := range []Subset{SubsetSmoke, SubsetFitness, SubsetFull} {
		if !s.IsValid() {
			t.Errorf("want valid: %s", s)
		}
	}
	for _, s := range []Subset{"", "bogus", "Smoke"} { // case-sensitive
		if Subset(s).IsValid() {
			t.Errorf("want invalid: %q", s)
		}
	}
}

// fakeRunner is a deterministic AgentRunner used by live-mode tests.
// Returns canned response + calls per (taskID → fixture) map.
type fakeRunner struct {
	responses map[string]string
	calls     map[string][]FixtureToolCall
	err       error
}

func (r *fakeRunner) Run(_ context.Context, prompt string) (string, []FixtureToolCall, error) {
	if r.err != nil {
		return "", nil, r.err
	}
	// In a real runner the prompt is sent to the agent; here we route
	// by prompt-prefix to the canned response so a single fakeRunner
	// instance can serve multiple tasks deterministically.
	for k, resp := range r.responses {
		if strings.Contains(prompt, k) {
			return resp, r.calls[k], nil
		}
	}
	return "", nil, nil
}

// TestRunLive_GateAndJudgePass: a live task with a fixture-grade
// AgentRunner + FixtureJudge that gives a passing score should yield
// Passed=true and Score in the 70-100 band per the linear-anchor
// formula in runLive.
func TestRunLive_GateAndJudgePass(t *testing.T) {
	task := &Task{
		ID: "lp", Category: "c", Subset: SubsetFitness, Mode: ModeLive,
		Prompt: "what is on example.com",
		Expected: Expected{
			MustCall: []ExpectedToolCall{{Tool: "web_navigate", ArgsContain: []string{"example.com"}}},
			Rubric: &Rubric{
				Criterion: "Mentions example.com",
				Anchors:   map[int]string{1: "no", 3: "ok", 5: "great"},
			},
			RubricMinScore: 3,
		},
	}
	runner := &fakeRunner{
		responses: map[string]string{"example.com": "Example Domain is a placeholder"},
		calls: map[string][]FixtureToolCall{"example.com": {
			{Tool: "web_navigate", Args: `{"url":"https://example.com"}`},
		}},
	}
	h := &Harness{
		Tasks:        []*Task{task},
		AgentFactory: func(_ context.Context, _ string) (AgentRunner, func() error, error) { return runner, nil, nil },
		Judge:        &FixtureJudge{Score: 5, Reasoning: "matches"},
	}
	rep, err := h.Run(context.Background(), SubsetFitness)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Aggregates.PassCount != 1 {
		t.Fatalf("want pass count 1, got %d (failures: %v)", rep.Aggregates.PassCount, rep.Results[0].Failures)
	}
	if rep.Results[0].JudgeScore != 5 {
		t.Errorf("judge score: want 5, got %d", rep.Results[0].JudgeScore)
	}
	if rep.Results[0].Score < 70 {
		t.Errorf("score: want >=70 (gate+rubric passed), got %.1f", rep.Results[0].Score)
	}
}

// TestRunLive_GatePassRubricFail: gate passes, judge returns below min.
// Score should land at 50 per the v1 plan ("gate passed but missed rubric").
func TestRunLive_GatePassRubricFail(t *testing.T) {
	task := &Task{
		ID: "rf", Category: "c", Subset: SubsetFitness, Mode: ModeLive,
		Prompt: "what is on example.com",
		Expected: Expected{
			MustCall:       []ExpectedToolCall{{Tool: "web_navigate", ArgsContain: []string{"example.com"}}},
			Rubric:         &Rubric{Criterion: "x", Anchors: map[int]string{1: "no", 5: "yes"}},
			RubricMinScore: 4,
		},
	}
	runner := &fakeRunner{
		responses: map[string]string{"example.com": "ok"},
		calls: map[string][]FixtureToolCall{"example.com": {
			{Tool: "web_navigate", Args: `{"url":"https://example.com"}`},
		}},
	}
	h := &Harness{
		Tasks:        []*Task{task},
		AgentFactory: func(_ context.Context, _ string) (AgentRunner, func() error, error) { return runner, nil, nil },
		Judge:        &FixtureJudge{Score: 2, Reasoning: "weak"},
	}
	rep, _ := h.Run(context.Background(), SubsetFitness)
	if rep.Aggregates.PassCount != 0 {
		t.Fatalf("want 0 pass (rubric below min), got %d", rep.Aggregates.PassCount)
	}
	if rep.Results[0].Score != 50 {
		t.Errorf("score: want 50, got %.1f", rep.Results[0].Score)
	}
}

// TestRunLive_GateFailSkipsJudge: a failed gate must NOT invoke the
// judge — wasted tokens. This test uses a FixtureJudge with a panic-on-
// call wrapper-via-counter to confirm the judge wasn't consulted.
func TestRunLive_GateFailSkipsJudge(t *testing.T) {
	task := &Task{
		ID: "gf", Category: "c", Subset: SubsetFitness, Mode: ModeLive,
		Prompt: "x",
		Expected: Expected{
			MustCall: []ExpectedToolCall{{Tool: "web_navigate", ArgsContain: []string{"never-matches"}}},
			Rubric:   &Rubric{Criterion: "x"}, RubricMinScore: 3,
		},
	}
	runner := &fakeRunner{
		responses: map[string]string{"x": "ok"},
		calls:     map[string][]FixtureToolCall{"x": {{Tool: "web_navigate", Args: `{}`}}},
	}
	called := 0
	h := &Harness{
		Tasks:        []*Task{task},
		AgentFactory: func(_ context.Context, _ string) (AgentRunner, func() error, error) { return runner, nil, nil },
		Judge:        &countingJudge{n: &called},
	}
	rep, _ := h.Run(context.Background(), SubsetFitness)
	if rep.Aggregates.FailCount != 1 {
		t.Errorf("want 1 fail, got %d", rep.Aggregates.FailCount)
	}
	if called != 0 {
		t.Errorf("judge called %d times; want 0 (gate failed, skip judge)", called)
	}
}

type countingJudge struct{ n *int }

func (c *countingJudge) Grade(_ context.Context, _ *Task, _ string, _ []FixtureToolCall) (JudgeResult, error) {
	*c.n++
	return JudgeResult{Score: 5}, nil
}

// TestRunLive_NoFactoryFails: live tasks without AgentFactory configured
// must fail with a clear message, not silently pass.
func TestRunLive_NoFactoryFails(t *testing.T) {
	task := &Task{
		ID: "nf", Category: "c", Subset: SubsetFitness, Mode: ModeLive,
		Prompt:   "x",
		Expected: Expected{Rubric: &Rubric{Criterion: "x"}, RubricMinScore: 3},
	}
	h := &Harness{Tasks: []*Task{task}} // no AgentFactory
	rep, _ := h.Run(context.Background(), SubsetFitness)
	if rep.Aggregates.FailCount != 1 {
		t.Fatalf("want fail, got pass")
	}
	if !strings.Contains(strings.Join(rep.Results[0].Failures, ";"), "AgentFactory") {
		t.Errorf("expected AgentFactory error, got: %v", rep.Results[0].Failures)
	}
}

// TestJudge_ParseFenced: judges sometimes wrap JSON in markdown fences
// despite the prompt. The parser must extract anyway.
func TestJudge_ParseFenced(t *testing.T) {
	raw := "```json\n{\"score\": 4, \"reasoning\": \"clear answer\"}\n```"
	jr, err := parseJudgeOutput(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if jr.Score != 4 {
		t.Errorf("score: want 4, got %d", jr.Score)
	}
}

// TestJudge_ParseScrape: when JSON is malformed, fall back to scraping
// the integer score.
func TestJudge_ParseScrape(t *testing.T) {
	raw := `The score is 5 because the answer matches the criterion well.`
	jr, _ := parseJudgeOutput(raw)
	// scrapeScore won't match "is 5"; this falls back to score=0
	// (the fail-safe path). Confirm we don't crash.
	if jr.Score < 0 || jr.Score > 5 {
		t.Errorf("score out of range: %d", jr.Score)
	}
}

// TestJudge_ParseClampsHighScore: a judge returning score=99 (out of
// the 1-5 range) clamps to 5, not panics or returns 99.
func TestJudge_ParseClampsHighScore(t *testing.T) {
	raw := `{"score": 99, "reasoning": "great"}`
	jr, _ := parseJudgeOutput(raw)
	if jr.Score != 5 {
		t.Errorf("clamp: want 5, got %d", jr.Score)
	}
}

// TestRubric_AnchorRender: anchored prompt renders deterministically.
func TestRubric_AnchorRender(t *testing.T) {
	task := &Task{
		Prompt: "P",
		Expected: Expected{Rubric: &Rubric{
			Criterion: "C",
			Anchors:   map[int]string{1: "low", 3: "mid", 5: "high"},
		}},
	}
	out := renderJudgePrompt(task, "RESP", []FixtureToolCall{{Tool: "t", Args: "{}"}})
	for _, want := range []string{"P", "RESP", "C", "low", "mid", "high", "t {}"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered prompt missing %q\n---\n%s", want, out)
		}
	}
}

// TestRunWith_JobCallableAPI is the Phase 5 contract test: a Job can
// call h.RunWith(ctx, sub, factory, judge) without touching shared
// Harness state, and gets back its own *Report. This is the API the
// SoulNudgeJob, SkillInductionJob, distill.Supersede, and future GEPA
// will all consume.
//
// Worked SoulNudgeJob-style example: factory A models "current Soul"
// (returns "ok"), factory B models "candidate Soul" (returns "great").
// The Job runs both and compares — if B scores higher, accept the
// proposed edit.
func TestRunWith_JobCallableAPI(t *testing.T) {
	task := &Task{
		ID: "t", Category: "c", Subset: SubsetFitness, Mode: ModeLive,
		Prompt: "x",
		Expected: Expected{
			Rubric:         &Rubric{Criterion: "x", Anchors: map[int]string{1: "no", 5: "yes"}},
			RubricMinScore: 3,
		},
	}
	h := &Harness{Tasks: []*Task{task}}

	currentFactory := func(_ context.Context, _ string) (AgentRunner, func() error, error) {
		return &fakeRunner{responses: map[string]string{"x": "ok"}}, nil, nil
	}
	candidateFactory := func(_ context.Context, _ string) (AgentRunner, func() error, error) {
		return &fakeRunner{responses: map[string]string{"x": "great"}}, nil, nil
	}
	judge := &FixtureJudge{Score: 4} // both pass; the Job is interested in score deltas

	currentRep, err := h.RunWith(context.Background(), SubsetFitness, currentFactory, judge)
	if err != nil {
		t.Fatalf("RunWith current: %v", err)
	}
	candidateRep, err := h.RunWith(context.Background(), SubsetFitness, candidateFactory, judge)
	if err != nil {
		t.Fatalf("RunWith candidate: %v", err)
	}

	// Both should pass (judge gave 4, min was 3) — the SoulNudgeJob
	// would now compare Aggregates.Overall between currentRep and
	// candidateRep, accept if candidate ≥ current.
	if currentRep.Aggregates.PassCount != 1 || candidateRep.Aggregates.PassCount != 1 {
		t.Errorf("expected both to pass: current=%d, candidate=%d",
			currentRep.Aggregates.PassCount, candidateRep.Aggregates.PassCount)
	}
	// Harness defaults must be unchanged after RunWith calls.
	if h.AgentFactory != nil || h.Judge != nil {
		t.Error("RunWith mutated Harness defaults — concurrent callers would race")
	}
}

// TestRunWith_RestoresHarnessDefaults: RunWith must restore the
// Harness's AgentFactory/Judge to their pre-call values even when
// the inner Run errors out.
func TestRunWith_RestoresHarnessDefaults(t *testing.T) {
	origFactory := func(_ context.Context, _ string) (AgentRunner, func() error, error) {
		return nil, nil, nil
	}
	origJudge := &FixtureJudge{Score: 1}
	h := &Harness{
		Tasks:        nil, // empty subset → no work, but defaults must survive
		AgentFactory: origFactory,
		Judge:        origJudge,
	}
	override := &FixtureJudge{Score: 5}
	_, _ = h.RunWith(context.Background(), SubsetFitness, nil, override)
	// Function pointers can't be compared with ==; check the judge by identity.
	if h.Judge != origJudge {
		t.Errorf("Judge not restored: want %p, got %p", origJudge, h.Judge)
	}
	// AgentFactory is a func — verify by calling and checking pointer behavior is unchanged
	if h.AgentFactory == nil {
		t.Error("AgentFactory cleared, not restored")
	}
}

// TestRunWith_ConcurrentCallsSerialize: two goroutines calling
// RunWith on the same Harness must not race. The runMu serializes
// them; both get correct results.
func TestRunWith_ConcurrentCallsSerialize(t *testing.T) {
	task := &Task{
		ID: "t", Category: "c", Subset: SubsetFitness, Mode: ModeLive,
		Prompt:   "x",
		Expected: Expected{Rubric: &Rubric{Criterion: "x"}, RubricMinScore: 3},
	}
	h := &Harness{Tasks: []*Task{task}}

	factoryA := func(_ context.Context, _ string) (AgentRunner, func() error, error) {
		return &fakeRunner{responses: map[string]string{"x": "A"}}, nil, nil
	}
	factoryB := func(_ context.Context, _ string) (AgentRunner, func() error, error) {
		return &fakeRunner{responses: map[string]string{"x": "B"}}, nil, nil
	}
	judge := &FixtureJudge{Score: 4}

	done := make(chan error, 2)
	go func() {
		_, err := h.RunWith(context.Background(), SubsetFitness, factoryA, judge)
		done <- err
	}()
	go func() {
		_, err := h.RunWith(context.Background(), SubsetFitness, factoryB, judge)
		done <- err
	}()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent RunWith: %v", err)
		}
	}
}

// TestWriteJSON_SchemaVersion: external consumers depend on schema_version
// being present. Don't accidentally remove it.
func TestWriteJSON_SchemaVersion(t *testing.T) {
	rep := &Report{SchemaVersion: SchemaVersion, Subset: SubsetSmoke, Aggregates: Aggregates{PerCategory: map[string]float64{}, PerSubset: map[string]float64{}}}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, rep); err != nil {
		t.Fatalf("write json: %v", err)
	}
	if !strings.Contains(buf.String(), `"schema_version": 1`) {
		t.Errorf("schema_version missing from JSON output:\n%s", buf.String())
	}
}

// TEN-31: the memory-recall seed schema (injected_facts / injected_episodes on
// Task — the "fixture_setup" the ticket specified) must parse from YAML and be
// validated. This is the dedicated parse test the ticket's acceptance called
// for; previously the schema was only exercised indirectly via LoadHarness.
func TestLoadTask_FixtureSetup(t *testing.T) {
	yaml := []byte(`
id: mem-recall-seeded
category: test
subset: fitness
mode: live
prompt: "what's my deploy command?"
injected_facts:
  - text: "The deploy command is 'make ship'."
    confidence: 0.95
    source: test-seed
  - text: "The staging URL is https://staging.example.com."
injected_episodes:
  - prompt: "how do I deploy?"
    response: "Run 'make ship' from the repo root."
    tags: [deploy, ops]
    outcome: success
expected:
  rubric:
    criterion: "Recalls the seeded deploy command."
    anchors:
      1: "Doesn't recall it"
      3: "Partial / hedged"
      5: "States 'make ship' plainly"
  rubric_min_score: 3
`)
	tk, err := LoadTask(yaml, "mem-seeded.yaml")
	if err != nil {
		t.Fatalf("want load OK, got %v", err)
	}

	if got := len(tk.InjectedFacts); got != 2 {
		t.Fatalf("injected_facts: want 2, got %d", got)
	}
	f0 := tk.InjectedFacts[0]
	if f0.Text != "The deploy command is 'make ship'." {
		t.Errorf("fact[0].Text = %q", f0.Text)
	}
	if f0.Confidence != 0.95 {
		t.Errorf("fact[0].Confidence = %v, want 0.95", f0.Confidence)
	}
	if f0.Source != "test-seed" {
		t.Errorf("fact[0].Source = %q, want test-seed", f0.Source)
	}
	// Optional fields are allowed to be absent on the second fact.
	if f1 := tk.InjectedFacts[1]; f1.Confidence != 0 || f1.Source != "" {
		t.Errorf("fact[1] optional fields should default to zero, got conf=%v src=%q", f1.Confidence, f1.Source)
	}

	if got := len(tk.InjectedEpisodes); got != 1 {
		t.Fatalf("injected_episodes: want 1, got %d", got)
	}
	ep := tk.InjectedEpisodes[0]
	if ep.Prompt != "how do I deploy?" || ep.Response != "Run 'make ship' from the repo root." {
		t.Errorf("episode prompt/response not parsed: %+v", ep)
	}
	if len(ep.Tags) != 2 || ep.Tags[0] != "deploy" || ep.Tags[1] != "ops" {
		t.Errorf("episode tags = %v, want [deploy ops]", ep.Tags)
	}
	if ep.Outcome != "success" {
		t.Errorf("episode outcome = %q, want success", ep.Outcome)
	}
}

// TEN-31: an injected_fact with no text is a malformed seed — validate() must
// reject it rather than silently seeding an empty fact.
func TestLoadTask_FixtureSetup_RejectsEmptyFactText(t *testing.T) {
	yaml := []byte(`
id: mem-bad-fact
category: test
subset: fitness
mode: live
prompt: "x"
injected_facts:
  - confidence: 0.5
expected:
  rubric:
    criterion: "c"
    anchors: {1: "a", 3: "b", 5: "c"}
  rubric_min_score: 3
`)
	if _, err := LoadTask(yaml, "bad-fact.yaml"); err == nil {
		t.Fatal("want error for injected_fact with empty text, got nil")
	} else if !strings.Contains(err.Error(), "injected_facts") {
		t.Errorf("error should name injected_facts, got: %v", err)
	}
}

// TEN-31: an injected_episode missing its response is malformed — validate()
// must reject it (an episode needs both prompt and response to be replayable).
func TestLoadTask_FixtureSetup_RejectsEpisodeMissingResponse(t *testing.T) {
	yaml := []byte(`
id: mem-bad-episode
category: test
subset: fitness
mode: live
prompt: "x"
injected_episodes:
  - prompt: "no response here"
expected:
  rubric:
    criterion: "c"
    anchors: {1: "a", 3: "b", 5: "c"}
  rubric_min_score: 3
`)
	if _, err := LoadTask(yaml, "bad-episode.yaml"); err == nil {
		t.Fatal("want error for injected_episode missing response, got nil")
	} else if !strings.Contains(err.Error(), "injected_episodes") {
		t.Errorf("error should name injected_episodes, got: %v", err)
	}
}

// TEN-219: the judge must request a generous completion budget so a thinking
// model (the default self-judge is GLM-4.6) can finish its hidden reasoning
// before emitting the verdict — 1000 went empty on the multi-tool chain.
func TestJudge_GenerousTokenBudget(t *testing.T) {
	var gotMaxTokens int
	fake := testllm.New()
	fake.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
		gotMaxTokens = req.MaxTokens
		return &model.GenerateResponse{Text: `{"score": 4, "reasoning": "ok"}`}, nil
	}
	j := &LLMJudge{LLM: fake}
	task := &Task{Expected: Expected{Rubric: &Rubric{Criterion: "c"}}}
	if _, err := j.Grade(context.Background(), task, "resp", nil); err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if gotMaxTokens < 2000 {
		t.Errorf("judge MaxTokens = %d, want a generous budget (>=2000) for thinking models (TEN-219)", gotMaxTokens)
	}
}

// TEN-219: when the first attempt returns empty (budget exhausted mid-reasoning),
// the retry must ESCALATE the budget — re-sending the same one would just fail
// again — and recover the verdict.
func TestJudge_RetryEscalatesBudgetAndRecovers(t *testing.T) {
	var budgets []int
	fake := testllm.New()
	fake.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
		budgets = append(budgets, req.MaxTokens)
		if len(budgets) == 1 {
			return &model.GenerateResponse{Text: ""}, nil // thinking model burned the budget
		}
		return &model.GenerateResponse{Text: `{"score": 5, "reasoning": "great"}`}, nil
	}
	j := &LLMJudge{LLM: fake}
	task := &Task{Expected: Expected{Rubric: &Rubric{Criterion: "c"}}}
	jr, err := j.Grade(context.Background(), task, "resp", nil)
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if jr.Score != 5 {
		t.Errorf("score = %d, want 5 (recovered on retry)", jr.Score)
	}
	if len(budgets) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(budgets))
	}
	if budgets[1] <= budgets[0] {
		t.Errorf("retry budget %d did not escalate past the first %d (TEN-219)", budgets[1], budgets[0])
	}
}

// TEN-219: empty output on BOTH attempts is genuinely unusable — Grade returns
// an error so the harness records the task UNGRADED (never scores the agent 0
// for the judge's failure, TEN-197).
func TestJudge_EmptyBothAttemptsErrors(t *testing.T) {
	fake := testllm.New()
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: ""}, nil
	}
	j := &LLMJudge{LLM: fake}
	task := &Task{Expected: Expected{Rubric: &Rubric{Criterion: "c"}}}
	if _, err := j.Grade(context.Background(), task, "resp", nil); err == nil {
		t.Fatal("want UNGRADED error when the judge is empty on both attempts, got nil")
	}
}
