package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/memory/archive"
	"tenant/internal/model"
)

// A turn cancelled while Tenant is stopping (TEN-499) does no salvage
// synthesis: the planner is not called again, the cancel is the result's
// error, and the archive records the interrupted turn. Any other cancel
// keeps the salvage.
func TestStoppingSkipsSalvage(t *testing.T) {
	for _, stopping := range []bool{true, false} {
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		// The tool call is what gets cut: the trace has one entry when the
		// loop sees the cancel, which is what makes a salvage worth trying.
		cut := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) {
			cancel()
			return "ok", false, nil
		})
		a, fake, _, _ := buildAgentFull(t, model.Profile{}, []model.ToolSpec{{Name: "search"}}, cut, nil, nil, func(c *agent.Config) {
			c.Archive = archive.NewWriter(dir)
			if stopping {
				c.Stopping = func() bool { return true }
			}
		})
		fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
			return &model.GenerateResponse{
				ToolCalls:    []model.ToolCall{{ID: "x", Name: "search", Arguments: json.RawMessage(`{}`)}},
				FinishReason: "tool_calls",
			}, nil
		}
		res, err := a.Turn(ctx, agent.TurnRequest{UserQuery: "x"})
		if err != nil {
			t.Fatalf("stopping=%v: Turn: %v", stopping, err)
		}
		if stopping {
			if !errors.Is(res.Error, context.Canceled) || res.Truncated || len(fake.Generated) != 1 {
				t.Errorf("stopping: Error = %v, Truncated = %v, planner calls = %d; want the cancel, no salvage", res.Error, res.Truncated, len(fake.Generated))
			}
		} else if res.Error != nil || !res.Truncated || len(fake.Generated) != 2 {
			t.Errorf("not stopping: Error = %v, Truncated = %v, planner calls = %d; want the salvage answer as today", res.Error, res.Truncated, len(fake.Generated))
		}
		interrupted := false
		for e, err := range archive.NewReader(dir).Stream(archive.Filter{}) {
			if err != nil {
				t.Fatal(err)
			}
			if e.Metadata["kind"] == "turn_interrupted" {
				interrupted = e.Role == "system" && e.Metadata["reason"] == "stop" && e.Metadata["tools_run"] == float64(1)
			}
		}
		if interrupted != stopping {
			t.Errorf("stopping=%v: archive has a turn_interrupted event: %v", stopping, interrupted)
		}
	}
}
