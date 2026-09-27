package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/model"
)

type errRunner struct{ err error }

func (r errRunner) Run(ctx context.Context, prompt string) (string, []FixtureToolCall, error) {
	return "", nil, r.err
}

// The 2026-08-24 night: 12 of 28 failures were quota exhaustion scored as 12
// zeros, moving Overall 64.8 -> 41.4 with no agent change. Infra failures must
// be ungraded and classed, never averaged.
func TestRunError_InfraClassesAreUngraded(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("agent: planner generate iter=1: %w: claudecode weekly token budget exhausted", model.ErrInsufficientBalance), "quota"},
		{fmt.Errorf("agent: planner generate iter=2: %w: Post \"http://model-host:8000\": connect refused", model.ErrEndpointDown), "endpoint"},
		{fmt.Errorf("agent: planner generate iter=1: %w: 429", model.ErrRateLimited), "ratelimit"},
	}
	for _, c := range cases {
		h := &Harness{AgentFactory: func(ctx context.Context, id string) (AgentRunner, func() error, error) {
			return errRunner{err: c.err}, func() error { return nil }, nil
		}}
		res := h.runLive(context.Background(), &Task{ID: "t", Mode: ModeLive})
		if !res.Ungraded {
			t.Errorf("%s: infra failure must be ungraded, got scored %v", c.want, res.Score)
		}
		if res.ErrorClass != c.want {
			t.Errorf("class = %q, want %q", res.ErrorClass, c.want)
		}
		if len(res.Failures) == 0 || !strings.Contains(res.Failures[0], "UNGRADED") {
			t.Errorf("%s: failure text should say UNGRADED: %v", c.want, res.Failures)
		}
	}
}

// A genuine agent failure keeps scoring zero - the carve-out must not become
// a blanket amnesty.
func TestRunError_AgentFailuresStillScoreZero(t *testing.T) {
	h := &Harness{AgentFactory: func(ctx context.Context, id string) (AgentRunner, func() error, error) {
		return errRunner{err: errors.New("agent: too many consecutive tool-call validation failures")}, func() error { return nil }, nil
	}}
	res := h.runLive(context.Background(), &Task{ID: "t", Mode: ModeLive})
	if res.Ungraded {
		t.Fatal("a plain agent failure must not be ungraded")
	}
	if res.Score != 0 || res.Passed {
		t.Fatalf("score=%v passed=%v, want 0/false", res.Score, res.Passed)
	}
	if res.ErrorClass != "" {
		t.Fatalf("agent failure must carry no infra class, got %q", res.ErrorClass)
	}
}

// The aggregate: ungraded-infra tasks vanish from the mean and appear in the
// per-class split; TEN-197 judge failures land under "judge".
func TestAggregate_UngradedByClass(t *testing.T) {
	results := []TaskResult{
		{TaskID: "a", Passed: true, Score: 100},
		{TaskID: "b", Passed: false, Score: 0},
		{TaskID: "c", Ungraded: true, ErrorClass: "quota"},
		{TaskID: "d", Ungraded: true, ErrorClass: "quota"},
		{TaskID: "e", Ungraded: true}, // judge-unusable, no class
	}
	ag := aggregate(results, 0)
	if ag.Overall != 50 {
		t.Fatalf("Overall = %v, want 50 (mean of the two graded tasks only)", ag.Overall)
	}
	if ag.UngradedCount != 3 {
		t.Fatalf("UngradedCount = %d, want 3", ag.UngradedCount)
	}
	if ag.UngradedByClass["quota"] != 2 || ag.UngradedByClass["judge"] != 1 {
		t.Fatalf("UngradedByClass = %v, want quota:2 judge:1", ag.UngradedByClass)
	}
}

// eventSink keeps the eval events written while it is installed.
type eventSink struct{ events []*event.Event }

func (s *eventSink) Emit(_ context.Context, e *event.Event) {
	if e.Provider == Events.Name {
		s.events = append(s.events, e)
	}
}
func (s *eventSink) Audit(ctx context.Context, e *event.Event) error { s.Emit(ctx, e); return nil }

// A harness error (the task's agent could not be built) is catalogued with
// its class, and its task still ends as an activity with its result.
func TestHarnessErrorIsCatalogued(t *testing.T) {
	if g := strings.Join(catalog.Group(Events.Log), "/"); g != "Subsystems/Eval" {
		t.Errorf("the eval log sits in %q of the viewer's tree", g)
	}
	s := &eventSink{}
	event.Install(s)
	defer event.Install(nil)
	h := &Harness{Tasks: []*Task{{ID: "t1", Mode: ModeLive}}, AgentFactory: func(context.Context, string) (AgentRunner, func() error, error) {
		return nil, nil, errors.New("no model")
	}}
	if _, err := h.Run(context.Background(), SubsetFull); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range s.events {
		got = append(got, fmt.Sprintf("%d %s %s", e.ID, e.Opcode, e.Msg))
	}
	want := []string{
		"1001 start Eval run of full, 1 tasks",
		"1002 start Eval task t1",
		"2001  Eval task t1 was not graded (harness): UNGRADED (harness) - agent factory error: no model",
		"1002 stop Eval task t1",
		"1001 stop Eval run of full, 1 tasks",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
