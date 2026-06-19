package agent_test

import (
	"context"
	"encoding/json"
	"errors"
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
	if got := atomic.LoadInt32(&planCalls); got > 5 {
		t.Fatalf("expected early break (~3 plan calls), got %d — ceiling (20) not short-circuited", got)
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
