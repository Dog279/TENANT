package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/logs/event/eventtest"
	"tenant/internal/model"
	"tenant/internal/runtrace"
)

// A fleet_* call names its machine in the run trace, in the events the
// activity rail is drawn from, and in the turns log's record of the call
// (TEN-531). Any other call names none, even with a host argument.
func TestAgent_AFleetCallNamesItsMachine(t *testing.T) {
	runs, err := runtrace.Open(":memory:", runtrace.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer runs.Close()
	rec := eventtest.Capture(t)
	tools := []model.ToolSpec{{Name: "fleet_exec", Parameters: json.RawMessage(`{}`)}, {Name: "search", Parameters: json.RawMessage(`{}`)}}
	var mu sync.Mutex
	var events []agent.Event
	a, fake, _, _ := buildAgentFull(t, model.Profile{MaxParallelTools: 1}, tools, dispatchOK("ok"), nil, runs, func(c *agent.Config) {
		c.Observer = func(e agent.Event) { mu.Lock(); events = append(events, e); mu.Unlock() }
	})
	fake.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		if len(fake.Generated) == 1 {
			return &model.GenerateResponse{ToolCalls: []model.ToolCall{
				{ID: "c1", Name: "fleet_exec", Arguments: json.RawMessage(`{"host":"web1","command":"uptime"}`)},
				{ID: "c2", Name: "search", Arguments: json.RawMessage(`{"host":"web1"}`)},
			}, FinishReason: "tool_calls"}, nil
		}
		return &model.GenerateResponse{Text: "done", FinishReason: "stop"}, nil
	}
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "uptime on web1"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"fleet_exec": "web1", "search": ""}

	run, err := runs.Get(context.Background(), res.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.ToolCalls) != 2 {
		t.Fatalf("the run trace has %d calls", len(run.ToolCalls))
	}
	for _, c := range run.ToolCalls {
		if c.Host != want[c.Name] {
			t.Errorf("the run trace's %s names host %q, want %q", c.Name, c.Host, want[c.Name])
		}
	}

	mu.Lock()
	seen := 0
	for _, e := range events {
		if e.Kind == agent.EventToolCall || e.Kind == agent.EventToolResult {
			seen++
			if e.Host != want[e.Tool] {
				t.Errorf("the %s event for %s names host %q, want %q", e.Kind, e.Tool, e.Host, want[e.Tool])
			}
		}
	}
	mu.Unlock()
	if seen != 4 {
		t.Errorf("saw %d tool call and result events, want 4", seen)
	}

	logged := 0
	for _, e := range rec.Events() {
		if e.Provider != agent.ToolDone.Spec().Provider || e.ID != agent.ToolDone.Spec().ID {
			continue
		}
		logged++
		fields := map[string]string{}
		for _, f := range e.Data {
			fields[f.Key] = fmt.Sprint(f.Value)
		}
		host, has := fields["host"]
		if tool := fields["tool"]; host != want[tool] || has != (want[tool] != "") {
			t.Errorf("the turns log's record of %s: host %q (present %v), want %q", tool, host, has, want[tool])
		}
	}
	if logged != 2 {
		t.Errorf("the turns log has %d tool-done records, want 2", logged)
	}
}
