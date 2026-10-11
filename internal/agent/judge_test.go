package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/memory/episodic"
	"tenant/internal/model"
)

// fakeJudge triages every prompt as task (or not) and rules with verdicts
// in order, repeating the last one.
type fakeJudge struct {
	mu       sync.Mutex
	task     bool
	verdicts []agent.Verdict
	checkErr error
	max      int
	triaged  int
	checked  []agent.Exchange
}

func (j *fakeJudge) Triage(context.Context, string) (bool, agent.JudgeUsage, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.triaged++
	return j.task, agent.JudgeUsage{Model: "judge", InputTokens: 50}, nil
}

func (j *fakeJudge) Check(_ context.Context, ex agent.Exchange) (agent.Verdict, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.checkErr != nil {
		return agent.Verdict{}, j.checkErr
	}
	j.checked = append(j.checked, ex)
	v := j.verdicts[min(len(j.checked), len(j.verdicts))-1]
	v.Use = agent.JudgeUsage{Model: "judge", InputTokens: 200}
	return v, nil
}

func (j *fakeJudge) MaxRounds() int { return j.max }

var notDone = agent.Verdict{Done: false, Summary: "every_part 0.09", Nudge: "finish the second part"}
var done = agent.Verdict{Done: true, Summary: "done 0.90"}

// judgedAgent builds an agent with j as its judge, a planner that answers
// with answers in order (repeating the last), and an event recorder.
func judgedAgent(t *testing.T, j *fakeJudge, answers ...string) (*agent.Agent, *[]agent.Event, *int) {
	t.Helper()
	var mu sync.Mutex
	var events []agent.Event
	a, fake, _, _ := buildAgentFull(t, model.Profile{}, nil, agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "", false, nil }), nil, nil,
		func(c *agent.Config) {
			c.Judge = j
			c.Observer = func(e agent.Event) { mu.Lock(); events = append(events, e); mu.Unlock() }
		})
	calls := 0
	fake.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
		calls++
		return &model.GenerateResponse{Text: answers[min(calls, len(answers))-1]}, nil
	}
	return a, &events, &calls
}

// judgedAgentStore is judgedAgent returning the agent's episodic store.
func judgedAgentStore(t *testing.T, j *fakeJudge, answers ...string) (*agent.Agent, *episodic.Store, *int) {
	t.Helper()
	a, fake, _, store := buildAgentFull(t, model.Profile{}, nil, agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "", false, nil }), nil, nil,
		func(c *agent.Config) { c.Judge = j })
	calls := 0
	fake.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
		calls++
		return &model.GenerateResponse{Text: answers[min(calls, len(answers))-1]}, nil
	}
	return a, store, &calls
}

func judgeTexts(events []agent.Event) []string {
	var out []string
	for _, e := range events {
		if e.Kind == agent.EventJudge {
			out = append(out, e.Text)
		}
	}
	return out
}

// Chat isn't checked: triage says no task, the answer goes out as is.
func TestJudge_ChatIsNotChecked(t *testing.T) {
	j := &fakeJudge{task: false, verdicts: []agent.Verdict{notDone}, max: 3}
	a, _, calls := judgedAgent(t, j, "hi there")
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "hey"})
	if err != nil || res.Response != "hi there" || len(j.checked) != 0 || *calls != 1 {
		t.Fatalf("response %q, checks %d, planner calls %d, err %v", res.Response, len(j.checked), *calls, err)
	}
}

// A task the judge finds unfinished goes back once with the nudge, and the
// second answer, judged done, goes out. The judge sees the ask and the answer.
func TestJudge_SendsBackThenPasses(t *testing.T) {
	j := &fakeJudge{task: true, verdicts: []agent.Verdict{notDone, done}, max: 3}
	a, events, calls := judgedAgent(t, j, "2+2 is 4.", "2+2 is 4. Paris is the capital of France.")
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "what is 2+2, and the capital of France?"})
	if err != nil || *calls != 2 || !strings.Contains(res.Response, "Paris") || strings.Contains(res.Response, "not confirmed") {
		t.Fatalf("response %q, planner calls %d, err %v", res.Response, *calls, err)
	}
	if len(j.checked) != 2 || !strings.Contains(j.checked[0].Request, "capital of France") || j.checked[0].Answer != "2+2 is 4." {
		t.Fatalf("checks %+v", j.checked)
	}
	got := strings.Join(judgeTexts(*events), " | ")
	if !strings.Contains(got, "sent back (1/3): every_part 0.09") || !strings.Contains(got, "done ✓ done 0.90") {
		t.Fatalf("judge events: %s", got)
	}
}

// Out of rounds: the last answer goes out with the note.
func TestJudge_NotConfirmedAfterMaxRounds(t *testing.T) {
	j := &fakeJudge{task: true, verdicts: []agent.Verdict{notDone}, max: 3}
	a, _, calls := judgedAgent(t, j, "half done")
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "do both things"})
	if err != nil || *calls != 4 || !strings.HasSuffix(res.Response, "— judge: not confirmed (every_part 0.09)") {
		t.Fatalf("response %q, planner calls %d, err %v", res.Response, *calls, err)
	}
}

// A send-back doesn't use up the loop: with a ceiling of 1, the turn still
// gets its three rounds, then goes out with the note, which is for the
// reader only (memory keeps the model's own answer).
func TestJudge_SendBackIsAFreeIteration(t *testing.T) {
	j := &fakeJudge{task: true, verdicts: []agent.Verdict{notDone}, max: 3}
	a, store, calls := judgedAgentStore(t, j, "half done")
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "do both things", LoopCeiling: 1})
	if err != nil || *calls != 4 || res.Truncated || !strings.Contains(res.Response, "not confirmed") {
		t.Fatalf("response %q, calls %d, truncated %v, err %v", res.Response, *calls, res.Truncated, err)
	}
	eps, err := store.Recent(context.Background(), "test-agent", 5)
	if err != nil || len(eps) == 0 || strings.Contains(eps[0].Response, "judge:") {
		t.Fatalf("episode %+v (err %v): the note was saved as the model's words", eps, err)
	}
}

// An answer the loop forced (here the ceiling, after tool calls that kept
// going) is checked too, and gets the note when it isn't done.
func TestJudge_ForcedAnswerGetsTheNote(t *testing.T) {
	j := &fakeJudge{task: true, verdicts: []agent.Verdict{notDone}, max: 3}
	look := model.ToolSpec{Name: "look", Description: "look", Parameters: []byte(`{"type":"object","properties":{}}`)}
	a, fake, _, _ := buildAgentFull(t, model.Profile{}, []model.ToolSpec{look},
		agent.DispatcherFunc(func(_ context.Context, c model.ToolCall) (string, bool, error) { return "saw " + c.ID, false, nil }), nil, nil,
		func(c *agent.Config) { c.Judge = j })
	n := 0
	fake.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
		if len(req.Tools) == 0 { // the forced synthesis
			return &model.GenerateResponse{Text: "partial findings"}, nil
		}
		n++
		return &model.GenerateResponse{ToolCalls: []model.ToolCall{{ID: "c" + strings.Repeat("x", n), Name: "look", Arguments: []byte(`{}`)}}}, nil
	}
	res, _ := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "investigate", LoopCeiling: 2})
	if !res.Truncated || !strings.HasPrefix(res.Response, "partial findings") || !strings.Contains(res.Response, "judge: not confirmed") || len(j.checked) != 1 {
		t.Fatalf("response %q, truncated %v, checks %d", res.Response, res.Truncated, len(j.checked))
	}
}

// A judge that fails lets the answer out unchecked, and says so once per
// outage, not every turn: with no cooldown, the second turn is judged again,
// fails again, and isn't reported again.
func TestJudge_FailsOpenAndLogsOnce(t *testing.T) {
	t.Cleanup(agent.SetJudgeCooldownForTest(0))
	j := &fakeJudge{task: true, checkErr: errors.New("connection refused"), max: 3}
	a, events, _ := judgedAgent(t, j, "the answer")
	for i := 0; i < 2; i++ {
		res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "do the thing"})
		if err != nil || res.Response != "the answer" {
			t.Fatalf("turn %d: response %q, err %v", i, res.Response, err)
		}
	}
	n := 0
	for _, e := range *events {
		if e.Kind == agent.EventJudge && e.IsErr {
			n++
		}
	}
	if n != 1 || j.triaged != 2 {
		t.Fatalf("unavailable reported %d times over %d judged turns, want once over 2", n, j.triaged)
	}
}

// NoJudge (a /goal run) and the judge's usage: no triage at all; a judged
// turn counts the judge's tokens as usage events.
func TestJudge_NoJudgeAndUsage(t *testing.T) {
	j := &fakeJudge{task: true, verdicts: []agent.Verdict{done}, max: 3}
	a, events, _ := judgedAgent(t, j, "the answer")
	if _, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "goal step", NoJudge: true}); err != nil || j.triaged != 0 {
		t.Fatalf("NoJudge turn triaged %d times (err %v)", j.triaged, err)
	}
	if _, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "do the thing"}); err != nil {
		t.Fatal(err)
	}
	judgeTokens := 0
	for _, e := range *events {
		if e.Kind == agent.EventUsage && e.CompletionTokens == 0 {
			judgeTokens += e.PromptTokens
		}
	}
	if judgeTokens != 250 {
		t.Fatalf("judge usage counted %d tokens, want 250 (triage 50 + check 200)", judgeTokens)
	}
}

// When the operator denied an action (the tool says "blocked: …"), the judge
// doesn't send the work back to ask them again: the answer goes out with the
// note. (Live, before this rule: four approval prompts for one refused rm.)
func TestJudge_NoSendBackAfterADenial(t *testing.T) {
	j := &fakeJudge{task: true, verdicts: []agent.Verdict{notDone}, max: 3}
	rm := model.ToolSpec{Name: "rm", Description: "rm", Parameters: []byte(`{"type":"object","properties":{}}`)}
	a, fake, _, _ := buildAgentFull(t, model.Profile{}, []model.ToolSpec{rm},
		agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) {
			return "blocked: this command wasn't approved (denied, timed out, or refused by a safety setting).", true, nil
		}), nil, nil, func(c *agent.Config) { c.Judge = j })
	calls := 0
	fake.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
		calls++
		if calls == 1 {
			return &model.GenerateResponse{ToolCalls: []model.ToolCall{{ID: "c1", Name: "rm", Arguments: []byte(`{}`)}}}, nil
		}
		return &model.GenerateResponse{Text: "I couldn't delete it: the deletion wasn't approved."}, nil
	}
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "delete report-1.txt"})
	if err != nil || calls != 2 || len(j.checked) != 1 || !strings.Contains(res.Response, "judge: not confirmed") {
		t.Fatalf("response %q, planner calls %d, checks %d, err %v", res.Response, calls, len(j.checked), err)
	}
}
