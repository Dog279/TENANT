package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/model"
)

// TEN-261: a weak model that locks onto the same tool call must break out of the
// plan loop early and synthesize from gathered results — not burn the ceiling.
func TestTurn_OscillationBreaksEarly(t *testing.T) {
	search := model.ToolSpec{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)}
	disp := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) {
		return "some result", false, nil
	})
	a, fake, _, _ := buildAgent(t, model.Profile{PlanLoopCeiling: 20}, []model.ToolSpec{search}, disp)

	var planCalls int32
	fake.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
		if len(req.Tools) == 0 { // forced synthesis runs with tools off
			return &model.GenerateResponse{Text: "synthesized from gathered results"}, nil
		}
		atomic.AddInt32(&planCalls, 1)
		return &model.GenerateResponse{ToolCalls: []model.ToolCall{
			{ID: "c1", Name: "search", Arguments: json.RawMessage(`{"q":"foo"}`)},
		}}, nil
	}

	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "find foo"})
	if err != nil {
		t.Fatalf("Turn returned error: %v", err)
	}
	if !errors.Is(res.Error, agent.ErrOscillationDetected) {
		t.Fatalf("expected ErrOscillationDetected, got %v", res.Error)
	}
	if res.Response == "" {
		t.Fatal("expected a synthesized response, got empty")
	}
	if got := atomic.LoadInt32(&planCalls); got != agent.DefaultRepeatGuard {
		t.Fatalf("expected a break after %d identical rounds, got %d plan calls (ceiling 20)", agent.DefaultRepeatGuard, got)
	}
}

// TEN-506: polling the same command is progress while its output changes (a
// job's CPU time ticking up), so the guard must leave it to the loop ceiling;
// and an operator can turn the guard off for identical polls too.
func TestTurn_RepeatGuardOnlyTripsOnIdenticalResults(t *testing.T) {
	for _, tc := range []struct {
		name     string
		guard    int
		changing bool
		capped   bool // the change is past the context cap: the model can't see it
		stuck    bool
	}{
		{"changing results", 0, true, false, false},
		{"identical results", 0, false, false, true},
		{"identical results, guard off", -1, false, false, false},
		{"identical results, guard 3", 3, false, false, true},
		{"change hidden past the cap", 0, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent.SetRepeatGuard(tc.guard)
			t.Cleanup(func() { agent.SetRepeatGuard(0) })
			var polls int32
			poll := model.ToolSpec{Name: "poll", Parameters: json.RawMessage(`{"type":"object"}`)}
			disp := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) {
				n := atomic.AddInt32(&polls, 1)
				if tc.capped {
					// The hidden tail grows every poll, so the full length (which
					// the truncation note reports) changes too: still a repeat.
					return "still running" + strings.Repeat(".", 200_000) + strings.Repeat("#", int(n)), false, nil
				}
				if tc.changing {
					return fmt.Sprintf("still running, CPU 0:%02d", n), false, nil
				}
				return "still running", false, nil
			})
			a, fake, _, _ := buildAgent(t, model.Profile{PlanLoopCeiling: 12}, []model.ToolSpec{poll}, disp)
			fake.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
				if len(req.Tools) == 0 {
					return &model.GenerateResponse{Text: "synthesized"}, nil
				}
				return &model.GenerateResponse{ToolCalls: []model.ToolCall{
					{ID: "c1", Name: "poll", Arguments: json.RawMessage(`{"pid":75721}`)},
				}}, nil
			}
			res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "wait for the job"})
			if err != nil {
				t.Fatalf("Turn: %v", err)
			}
			if got := errors.Is(res.Error, agent.ErrOscillationDetected); got != tc.stuck {
				t.Fatalf("stuck = %v, want %v (err=%v, polls=%d)", got, tc.stuck, res.Error, polls)
			}
			if !tc.stuck && !errors.Is(res.Error, agent.ErrLoopCeilingReached) {
				t.Fatalf("expected the loop ceiling to end the turn, got %v", res.Error)
			}
		})
	}
}

// Control: a model that calls a tool once then answers must complete normally —
// the oscillation guard must not fire on a healthy turn (frontier path).
func TestTurn_NoOscillationOnNormalFlow(t *testing.T) {
	search := model.ToolSpec{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)}
	disp := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) {
		return "result", false, nil
	})
	a, fake, _, _ := buildAgent(t, model.Profile{PlanLoopCeiling: 20}, []model.ToolSpec{search}, disp)

	var n int32
	fake.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return &model.GenerateResponse{ToolCalls: []model.ToolCall{
				{ID: "c1", Name: "search", Arguments: json.RawMessage(`{"q":"foo"}`)},
			}}, nil
		}
		return &model.GenerateResponse{Text: "here is your answer"}, nil
	}

	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "find foo"})
	if err != nil {
		t.Fatalf("Turn returned error: %v", err)
	}
	if errors.Is(res.Error, agent.ErrOscillationDetected) {
		t.Fatal("oscillation guard fired on a normal turn")
	}
	if res.Response != "here is your answer" {
		t.Fatalf("expected normal final answer, got %q (err=%v)", res.Response, res.Error)
	}
}
