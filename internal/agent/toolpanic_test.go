package agent_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/logs"
	"tenant/internal/model"
)

// A plugin's panic fails its own tool call instead of taking Tenant down: the
// model gets an error result and the log a critical record with the stack
// (TEN-365).
func TestAgent_Turn_ToolPanicBecomesAnErrorResult(t *testing.T) {
	hub := logs.NewMemory(100)
	prev := logs.Default()
	logs.Install(hub) // the catalogued record goes to the installed emitter
	t.Cleanup(func() { logs.Install(prev) })
	tools := []model.ToolSpec{{Name: "boom", Parameters: json.RawMessage(`{}`)}}
	disp := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) {
		panic("plugin bug")
	})
	a, fake, ws, _ := buildAgentFull(t, model.Profile{}, tools, disp, nil, nil, func(c *agent.Config) {
		c.Logger = slog.New(hub.Handler(nil))
	})
	fake.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		if len(fake.Generated) == 1 {
			return &model.GenerateResponse{
				ToolCalls:    []model.ToolCall{{ID: "c1", Name: "boom", Arguments: json.RawMessage(`{}`)}},
				FinishReason: "tool_calls",
			}, nil
		}
		return &model.GenerateResponse{Text: "carried on", FinishReason: "stop"}, nil
	}

	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "run boom"})
	if err != nil || res.Response != "carried on" {
		t.Fatalf("turn = %+v, %v", res, err)
	}
	var toolMsg string
	for _, m := range ws.Messages() {
		if m.Role == "tool" {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(toolMsg, "tool boom panicked: plugin bug") {
		t.Fatalf("the model saw %q, want the panic as an error result", toolMsg)
	}
	es, _ := hub.Snapshot()
	var crit []logs.Entry
	for _, e := range es {
		if e.Level == logs.LevelCritical {
			crit = append(crit, e)
		}
	}
	if len(crit) != 1 || crit[0].Field("tool") != "boom" || crit[0].Field("severity") != "critical" ||
		!strings.Contains(crit[0].Field("stack"), "goroutine ") {
		t.Fatalf("critical records = %+v", crit)
	}
}
