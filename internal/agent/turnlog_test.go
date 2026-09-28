package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/logs"
	"tenant/internal/model"
)

// A turn is an activity: its start names the spawning turn as related, its
// end stops with a duration, or fails. A tool call's operational record is
// filed in the agent's own log (its logger's), not turns.
func TestTurnIsAnActivityAndToolRecordsFollowTheAgentsLog(t *testing.T) {
	hub := logs.NewMemory(100)
	prev := logs.Default()
	logs.Install(hub)
	t.Cleanup(func() { logs.Install(prev) })

	var mu sync.Mutex
	var seen []agent.Event
	tools := []model.ToolSpec{{Name: "echo", Parameters: json.RawMessage(`{}`)}}
	disp := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "ok", false, nil })
	a, fake, _, _ := buildAgentFull(t, model.Profile{}, tools, disp, nil, nil, func(c *agent.Config) {
		c.Logger = slog.New(hub.Handler(nil)).With("source", "subagents")
		c.Observer = func(e agent.Event) { mu.Lock(); seen = append(seen, e); mu.Unlock() }
	})
	fake.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		if len(fake.Generated) == 1 {
			return &model.GenerateResponse{ToolCalls: []model.ToolCall{{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		return &model.GenerateResponse{Text: "done"}, nil
	}
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "q", ParentTraceID: "parent-trace"})
	if err != nil {
		t.Fatal(err)
	}

	lines := map[agent.EventKind]logs.Entry{}
	for _, e := range seen {
		if en, ok := agent.EntryFor(e); ok {
			lines[e.Kind] = en
		}
	}
	start, end := lines[agent.EventTurnStart], lines[agent.EventTurnEnd]
	if start.Field("event") != "turns/1" || start.Field("opcode") != "start" || start.Field("related") != "parent-trace" || start.Field("trace") != res.TraceID {
		t.Errorf("turn start: %s", start.Line())
	}
	if end.Field("event") != "turns/1" || end.Field("opcode") != "stop" || end.Field("duration") == "" || end.Field("trace") != res.TraceID || end.Level != slog.LevelInfo {
		t.Errorf("turn end: %s", end.Line())
	}
	if r := lines[agent.EventToolResult]; r.Field("event") != "turns/9002" || r.Field("duration") == "" || r.Field("result") != "ok" {
		t.Errorf("tool result row: %s", r.Line())
	}

	es, _ := hub.Snapshot()
	var done *logs.Entry
	for i := range es {
		if es[i].Field("event") == "turns/1101" {
			done = &es[i]
		}
	}
	if done == nil || done.Source != "subagents" || done.Field("tool") != "echo" || done.Field("trace") != res.TraceID {
		t.Fatalf("the tool's operational record: %+v", es)
	}

	// A turn that errors ends with a fail at error level.
	seen = nil
	fake.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return nil, errors.New("backend down")
	}
	if _, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "again"}); err == nil {
		t.Fatal("want the planner's error")
	}
	var failed logs.Entry
	for _, e := range seen {
		if e.Kind == agent.EventTurnEnd {
			failed, _ = agent.EntryFor(e)
		}
	}
	if failed.Field("opcode") != "fail" || failed.Level != slog.LevelError || failed.Field("error") == "" || failed.Msg != "turn failed" {
		t.Errorf("failed turn end: %s", failed.Line())
	}
}
