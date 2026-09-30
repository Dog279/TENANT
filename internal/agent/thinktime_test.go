package agent_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/model"
	"tenant/internal/runtrace"
)

// A turn tells the feed when each model call starts and how long it took, and
// its end splits its time into thinking and tools, on the event, the log line
// and the run record (TEN-342). The thinking marks themselves stay out of the
// log book.
func TestTurnTimesThinkingAndTools(t *testing.T) {
	runs, err := runtrace.Open(":memory:", runtrace.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer runs.Close()
	var mu sync.Mutex
	var seen []agent.Event
	tools := []model.ToolSpec{{Name: "slow", Parameters: json.RawMessage(`{}`)}}
	disp := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) {
		time.Sleep(40 * time.Millisecond)
		return "ok", false, nil
	})
	a, fake, _, _ := buildAgentFull(t, model.Profile{}, tools, disp, nil, runs, func(c *agent.Config) {
		c.Observer = func(e agent.Event) { mu.Lock(); seen = append(seen, e); mu.Unlock() }
	})
	fake.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		time.Sleep(30 * time.Millisecond)
		if len(fake.Generated) == 1 {
			return &model.GenerateResponse{ToolCalls: []model.ToolCall{{ID: "c1", Name: "slow", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		return &model.GenerateResponse{Text: "done"}, nil
	}
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "q"})
	if err != nil {
		t.Fatal(err)
	}

	var thinking, thought []agent.Event
	var end agent.Event
	for _, e := range seen {
		switch e.Kind {
		case agent.EventThinking:
			if len(thinking) != len(thought) {
				t.Fatalf("a model call started before the last one returned: %+v", seen)
			}
			thinking = append(thinking, e)
		case agent.EventThought:
			thought = append(thought, e)
		case agent.EventTurnEnd:
			end = e
		}
		if e.Kind == agent.EventThinking || e.Kind == agent.EventThought {
			if _, ok := agent.EntryFor(e); ok {
				t.Errorf("%s reached the log book", e.Kind)
			}
		}
	}
	if len(thinking) != 2 || len(thought) != 2 {
		t.Fatalf("thinking %d, thought %d: want 2 each", len(thinking), len(thought))
	}
	for i, e := range thought {
		if e.Iter != i+1 || e.Duration < 25*time.Millisecond || e.IsErr || e.TraceID != res.TraceID {
			t.Errorf("thought %d = %+v", i, e)
		}
	}
	// Each total is rounded to the millisecond apart, so their sum may pass the
	// turn's rounded duration by a hair.
	if end.Think < 50*time.Millisecond || end.Tools < 35*time.Millisecond || end.Think+end.Tools > end.Duration+2*time.Millisecond {
		t.Fatalf("turn end: duration %v, think %v, tools %v", end.Duration, end.Think, end.Tools)
	}
	if en, _ := agent.EntryFor(end); en.Field("think") != end.Think.String() || en.Field("tools") != end.Tools.String() {
		t.Errorf("turn end line: %s", en.Line())
	}
	run, err := runs.Get(context.Background(), res.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ThinkMS < 50 || run.ToolsMS < 35 || run.ThinkMS+run.ToolsMS > run.LatencyMS {
		t.Fatalf("run record: latency %d ms, think %d ms, tools %d ms", run.LatencyMS, run.ThinkMS, run.ToolsMS)
	}

	// A model call that fails still ends its thinking, marked failed.
	seen = nil
	fake.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return nil, context.DeadlineExceeded
	}
	_, _ = a.Turn(context.Background(), agent.TurnRequest{UserQuery: "again"})
	var failed bool
	for _, e := range seen {
		failed = failed || (e.Kind == agent.EventThought && e.IsErr)
	}
	if !failed {
		t.Fatalf("no failed thought: %+v", seen)
	}
}

// A scheduled job's tool rows name the tool cron:<tool> in a log view, whether
// the agent wrote the row or it came from the job's mirrored events; the
// stored tool field keeps the plain name. A turn's rows are shown as written.
func TestRowMsgNamesACronJobsTools(t *testing.T) {
	cron := event.WithLog(context.Background(), agent.Turns, "cron")
	done := *agent.ToolDone.Build(cron, "agent", "tenant-cron", "iter", 1, "tool", "web_search", "duration", 1200*time.Millisecond)
	if got := agent.RowMsg(done); got != "Tool cron:web_search finished in 1.2s" {
		t.Errorf("cron tool row = %q", got)
	}
	for _, f := range done.Data {
		if f.Key == "tool" && f.Text() != "web_search" {
			t.Errorf("RowMsg changed the stored event: %+v", done.Data)
		}
	}

	turn := *agent.ToolDone.Build(context.Background(), "agent", "main", "iter", 1, "tool", "web_search", "duration", 1200*time.Millisecond)
	if got := agent.RowMsg(turn); got != turn.Msg || got != "Tool web_search finished in 1.2s" {
		t.Errorf("a turn's tool row = %q", got)
	}

	en, _ := agent.EntryFor(agent.Event{Kind: agent.EventToolCall, Tool: "wiki_search", Args: "{}", Agent: "tenant-cron", Source: "cron"})
	if got := agent.RowMsg(logs.EventFromEntry(en)); got != "tool call cron:wiki_search" {
		t.Errorf("mirrored cron tool call = %q", got)
	}

	start := *agent.TurnRun.Build(cron, "agent", "tenant-cron")
	if got := agent.RowMsg(start); got != start.Msg {
		t.Errorf("a cron row without a tool = %q, want %q", got, start.Msg)
	}
}
