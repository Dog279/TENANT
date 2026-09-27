package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/memory/archive"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
	"tenant/internal/runtrace"
)

func TestAgent_TurnPersistsRunAndEpisodeLineage(t *testing.T) {
	runs, err := runtrace.Open(":memory:", runtrace.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer runs.Close()
	noop := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "", false, nil })
	arcDir := t.TempDir()
	a, fake, _, episodes := buildAgentFull(t, model.Profile{}, nil, noop, nil, runs, func(c *agent.Config) { c.Archive = archive.NewWriter(arcDir) })
	fake.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "done", Usage: model.Usage{PromptTokens: 9, CompletionTokens: 3}}, nil
	}
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "trace this", RouteSurface: agent.RouteSurfaceTUI, ParentTraceID: "spawner"})
	if err != nil {
		t.Fatal(err)
	}
	if res.TraceID == "" {
		t.Fatal("turn result missing trace id")
	}
	run, err := runs.Get(context.Background(), res.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != "success" || run.Source != "tui" || run.PromptTokens != 9 || run.CompletionTokens != 3 {
		t.Fatalf("run = %+v", run)
	}
	if run.ParentTraceID != "spawner" {
		t.Fatalf("run parent = %q, want the spawning turn's trace", run.ParentTraceID)
	}
	archived := 0
	for ev, err := range archive.NewReader(arcDir).Stream(archive.Filter{}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.TraceID != res.TraceID {
			t.Fatalf("archive event %s trace = %q, want %q", ev.Role, ev.TraceID, res.TraceID)
		}
		archived++
	}
	if archived == 0 {
		t.Fatal("turn archived nothing")
	}
	if run.Model == "" || run.Provider == "" || run.CompletedAt == nil {
		t.Fatalf("run missing model/provider/completion: %+v", run)
	}
	eps, err := episodes.List(context.Background(), episodic.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].SourceRunID != res.TraceID {
		t.Fatalf("episode lineage = %+v, want run %s", eps, res.TraceID)
	}
}

// Every event a turn raises carries the turn's trace ID, not just turn_start,
// so each log line joins its runs.db record (TEN-362).
func TestAgent_TurnEventsCarryTraceID(t *testing.T) {
	tools := []model.ToolSpec{{Name: "search", Parameters: json.RawMessage(`{}`)}}
	var mu sync.Mutex
	var events []agent.Event
	a, fake, _, _ := buildAgentFull(t, model.Profile{}, tools, dispatchOK("hit"), nil, nil, func(c *agent.Config) {
		c.Observer = func(e agent.Event) { mu.Lock(); events = append(events, e); mu.Unlock() }
	})
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		if len(fake.Generated) == 1 {
			return &model.GenerateResponse{ToolCalls: []model.ToolCall{{ID: "c1", Name: "search", Arguments: json.RawMessage(`{}`)}}, FinishReason: "tool_calls"}, nil
		}
		return &model.GenerateResponse{Text: "done", FinishReason: "stop"}, nil
	}
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "find it"})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	kinds := map[agent.EventKind]bool{}
	for _, e := range events {
		kinds[e.Kind] = true
		if e.TraceID != res.TraceID {
			t.Errorf("%s event trace = %q, want the turn's %q", e.Kind, e.TraceID, res.TraceID)
		}
	}
	for _, k := range []agent.EventKind{agent.EventTurnStart, agent.EventToolCall, agent.EventToolResult, agent.EventFinal} {
		if !kinds[k] {
			t.Errorf("no %s event observed (got %v)", k, kinds)
		}
	}
}

// A mid-turn Interject must cancel the in-flight planner call, fold the
// message into the working set, and let the loop re-plan and finish — without
// failing the turn or burning the loop budget.
func TestAgent_Interject_FoldedAndResumes(t *testing.T) {
	noop := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "", false, nil })
	a, fake, ws, _ := buildAgent(t, model.Profile{}, nil, noop)

	started := make(chan struct{})
	var calls int32
	fake.GenerateFn = func(ctx context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(started)
			<-ctx.Done() // wait for the interjection to cancel this call
			return nil, ctx.Err()
		}
		// Second call: the interjection must now be visible in the prompt.
		sawInterjection := false
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "also say hello") {
				sawInterjection = true
			}
		}
		if !sawInterjection {
			t.Errorf("re-plan prompt missing the interjection: %+v", req.Messages)
		}
		return &model.GenerateResponse{Text: "done", FinishReason: "stop"}, nil
	}

	type result struct {
		r   *agent.TurnResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "do the big task"})
		done <- result{r, err}
	}()

	select {
	case <-started:
		a.Interject("also say hello")
	case got := <-done:
		t.Fatalf("Turn returned before the planner ran: r=%+v err=%v", got.r, got.err)
	case <-time.After(5 * time.Second):
		t.Fatal("planner was never called")
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Turn errored after interjection: %v", got.err)
		}
		if got.r.Response != "done" {
			t.Fatalf("Response = %q, want done (turn should resume + finish)", got.r.Response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Turn did not complete after interjection")
	}

	found := false
	for _, m := range ws.Messages() {
		if strings.Contains(m.Content, "also say hello") {
			found = true
		}
	}
	if !found {
		t.Fatal("interjection was not folded into the working set")
	}
	if atomic.LoadInt32(&calls) < 2 {
		t.Fatalf("planner called %d times, want ≥2 (cancel + re-plan)", calls)
	}
}

// A mid-turn context overflow (llama.cpp's "Failed to tokenize prompt", now
// classified as ErrContextOverflow) must not kill the turn: the agent compacts
// the working set once and retries the iteration, and the turn finishes. Before
// the fix the planner error propagated straight out as "planner generate
// iter=N" — exactly the failure the operator hit on a llama.cpp server.
func TestAgent_ContextOverflow_CompactsAndRetries(t *testing.T) {
	noop := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "", false, nil })
	fake := testllm.New()
	var calls int32
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return nil, fmt.Errorf("model: %w", model.ErrContextOverflow)
		}
		return &model.GenerateResponse{Text: "recovered", FinishReason: "stop"}, nil
	}

	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	router := model.NewRouter(reg, slog.New(slog.NewTextHandler(testWriter{t: t}, nil)))
	router.RegisterBackend("vllm", func(_ context.Context, _ model.Profile, _ *slog.Logger) (any, error) {
		return fake, nil
	})

	comp := &fakeCompactor{}
	estore, err := episodic.Open(filepath.Join(t.TempDir(), "ep.db"))
	if err != nil {
		t.Fatalf("episodic.Open: %v", err)
	}
	t.Cleanup(func() { _ = estore.Close() })

	a, err := agent.New(agent.Config{
		AgentID:    "test-agent",
		SessionID:  "test-sess",
		Router:     router,
		Working:    working.New(),
		Archive:    archive.NewWriter(t.TempDir()),
		Episodic:   estore,
		Tools:      agent.NewStaticRegistry(),
		Dispatcher: noop,
		Compactor:  comp,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	r, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "do the thing"})
	if err != nil {
		t.Fatalf("Turn errored instead of recovering from overflow: %v", err)
	}
	if r.Response != "recovered" {
		t.Fatalf("Response = %q, want recovered (turn should compact-and-retry)", r.Response)
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("planner called %d times, want >=2 (overflow + retry)", got)
	}
	if got := atomic.LoadInt32(&comp.calls); got < 1 {
		t.Fatal("compactor was not invoked on overflow")
	}
}

// fakeCompactor is a minimal agent.Compactor: it records invocations and shrinks
// the working set to its last message so the retry sees a smaller context.
type fakeCompactor struct{ calls int32 }

func (c *fakeCompactor) Compact(_ context.Context, msgs []working.Message) ([]working.Message, bool, error) {
	atomic.AddInt32(&c.calls, 1)
	if len(msgs) <= 1 {
		return msgs, false, nil
	}
	return msgs[len(msgs)-1:], true, nil
}

// --- Test plumbing ---

// buildAgent wires a fresh agent against a fake LLM + embedder. The
// fake is returned so tests can script Generate behavior per iteration.
func buildAgent(t *testing.T, profile model.Profile, tools []model.ToolSpec, dispatcher agent.ToolDispatcher) (*agent.Agent, *testllm.Fake, *working.Set, *episodic.Store) {
	return buildAgentFull(t, profile, tools, dispatcher, nil, nil)
}

func buildAgentWithEgress(t *testing.T, profile model.Profile, tools []model.ToolSpec, dispatcher agent.ToolDispatcher, confirm func(context.Context, string, string) bool) (*agent.Agent, *testllm.Fake, *working.Set, *episodic.Store) {
	return buildAgentFull(t, profile, tools, dispatcher, confirm, nil)
}

func buildAgentWithRuns(t *testing.T, profile model.Profile, tools []model.ToolSpec, dispatcher agent.ToolDispatcher, runs *runtrace.Store) (*agent.Agent, *testllm.Fake, *working.Set, *episodic.Store) {
	return buildAgentFull(t, profile, tools, dispatcher, nil, runs)
}

func buildAgentFull(t *testing.T, profile model.Profile, tools []model.ToolSpec, dispatcher agent.ToolDispatcher, confirm func(context.Context, string, string) bool, runs *runtrace.Store, mutate ...func(*agent.Config)) (*agent.Agent, *testllm.Fake, *working.Set, *episodic.Store) {
	t.Helper()
	fake := testllm.New()

	// Build a registry containing the profile so the router can resolve it.
	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	// Replace any existing planner/embedder profile by adding our test
	// profile through the disk-override path is awkward — instead,
	// pin the role to our test profile via a small extension below.

	router := model.NewRouter(reg, slog.New(slog.NewTextHandler(testWriter{t: t}, nil)))
	// Shipped profiles all use the "vllm" backend; route them to our
	// fake regardless of the test-passed profile (which is only used
	// for budget shapes, not backend dispatch).
	router.RegisterBackend("vllm", func(_ context.Context, _ model.Profile, _ *slog.Logger) (any, error) {
		return fake, nil
	})
	_ = profile

	// Insert our test profile into the registry by overriding role
	// preference. We do this by registering a profile that matches the
	// shipped one. The simpler path: use a shipped profile ID and patch
	// its fields via the test's local copy. But Router.PinRole only
	// accepts already-registered IDs.
	//
	// Workaround: use a built-in shipped profile, but replace the
	// backend factory so it returns our fake. The fields of the
	// shipped profile (PlanLoopCeiling, MaxParallelTools, etc.) are
	// what we set in test calls. For tests needing custom Profile
	// values, callers should override at the Profile-passing layer
	// (not the Router).
	//
	// For these tests, qwen3.6-72b is the planner default and qwen3-
	// embedding-8b is the embedder. Both ship with sensible values.

	working := working.New()

	estore, err := episodic.Open(filepath.Join(t.TempDir(), "ep.db"))
	if err != nil {
		t.Fatalf("episodic.Open: %v", err)
	}
	t.Cleanup(func() { _ = estore.Close() })

	arch := archive.NewWriter(t.TempDir())

	registry := agent.NewStaticRegistry()
	for _, tool := range tools {
		registry.Register(tool)
	}

	cfg := agent.Config{
		AgentID:                "test-agent",
		SessionID:              "test-sess",
		Router:                 router,
		Working:                working,
		Archive:                arch,
		Episodic:               estore,
		Runs:                   runs,
		Tools:                  registry,
		Dispatcher:             dispatcher,
		ConfirmSensitiveEgress: confirm,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a, fake, working, estore
}

// noopCompactor is a Compactor that changes nothing. It exists only to make the
// post-turn compaction defer RUN (the defer short-circuits when Compactor is nil),
// so a regression test can exercise the defer on an error turn.
type noopCompactor struct{}

func (noopCompactor) Compact(_ context.Context, msgs []working.Message) ([]working.Message, bool, error) {
	return msgs, false, nil
}

// TestAgent_TurnErrorWithCompactorDoesNotPanic locks in the fix for a crash:
// Turn's `result` is a NAMED return (TEN-300 named it so the run-trace finish
// defer can read the outcome). The planner/assemble error paths do `return nil,
// err`, which nils the named result before defers run — and the post-turn
// compaction defer dereferenced result.Reports. A spawned sub-agent (always given
// a Compactor) whose planner generate errored panicked the whole process. It must
// now return (nil, err) cleanly with the compaction defer active.
func TestAgent_TurnErrorWithCompactorDoesNotPanic(t *testing.T) {
	genErr := errors.New("simulated planner generate failure")
	fake := testllm.New()
	fake.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return nil, genErr
	}
	fake.GenerateStreamFn = func(context.Context, model.GenerateRequest) (<-chan model.StreamChunk, error) {
		return nil, genErr
	}

	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	router := model.NewRouter(reg, slog.New(slog.NewTextHandler(testWriter{t: t}, nil)))
	router.RegisterBackend("vllm", func(context.Context, model.Profile, *slog.Logger) (any, error) {
		return fake, nil
	})

	estore, err := episodic.Open(filepath.Join(t.TempDir(), "ep.db"))
	if err != nil {
		t.Fatalf("episodic.Open: %v", err)
	}
	t.Cleanup(func() { _ = estore.Close() })

	a, err := agent.New(agent.Config{
		AgentID:    "test-agent",
		SessionID:  "test-sess",
		Router:     router,
		Working:    working.New(),
		Archive:    archive.NewWriter(t.TempDir()),
		Episodic:   estore,
		Tools:      agent.NewStaticRegistry(),
		Dispatcher: dispatchOK(""),
		Compactor:  noopCompactor{}, // MUST be non-nil for the compaction defer to run
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	// Before the fix this panicked with a nil dereference in the compaction defer.
	res, turnErr := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "do one thing"})
	if turnErr == nil {
		t.Fatal("expected the planner-generate error to surface as the turn error")
	}
	if res != nil {
		t.Fatalf("an error turn must return a nil result, got %+v", res)
	}
}

// buildStreamingAgent mirrors buildAgent but enables Stream + an
// Observer (for the TUI live-feed path).
func buildStreamingAgent(t *testing.T, fake *testllm.Fake, obs func(agent.Event), tools []model.ToolSpec, dispatcher agent.ToolDispatcher) *agent.Agent {
	t.Helper()
	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	router := model.NewRouter(reg, slog.New(slog.NewTextHandler(testWriter{t: t}, nil)))
	router.RegisterBackend("vllm", func(_ context.Context, _ model.Profile, _ *slog.Logger) (any, error) {
		return fake, nil
	})
	estore, err := episodic.Open(filepath.Join(t.TempDir(), "ep.db"))
	if err != nil {
		t.Fatalf("episodic.Open: %v", err)
	}
	t.Cleanup(func() { _ = estore.Close() })
	registry := agent.NewStaticRegistry()
	for _, tool := range tools {
		registry.Register(tool)
	}
	a, err := agent.New(agent.Config{
		AgentID: "test-agent", SessionID: "test-sess", Router: router,
		Working: working.New(), Archive: archive.NewWriter(t.TempDir()),
		Episodic: estore, Tools: registry, Dispatcher: dispatcher,
		Stream: true, Observer: obs,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

func TestAgent_Turn_StreamingEmitsTokens(t *testing.T) {
	fake := testllm.New()
	fake.GenerateStreamFn = func(_ context.Context, _ model.GenerateRequest) (<-chan model.StreamChunk, error) {
		ch := make(chan model.StreamChunk, 4)
		ch <- model.StreamChunk{Delta: "Hello"}
		ch <- model.StreamChunk{Delta: " world"}
		ch <- model.StreamChunk{Delta: "!", FinishReason: "stop"}
		close(ch)
		return ch, nil
	}
	var mu sync.Mutex
	var tokens []string
	kinds := map[agent.EventKind]int{}
	obs := func(e agent.Event) {
		mu.Lock()
		defer mu.Unlock()
		kinds[e.Kind]++
		if e.Kind == agent.EventToken {
			tokens = append(tokens, e.Text)
		}
	}
	noTools := agent.DispatcherFunc(func(_ context.Context, _ model.ToolCall) (string, bool, error) { return "", false, nil })
	a := buildStreamingAgent(t, fake, obs, nil, noTools)
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "hi"})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Response != "Hello world!" {
		t.Fatalf("response = %q, want %q", res.Response, "Hello world!")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(tokens, "|") != "Hello| world|!" {
		t.Errorf("tokens = %v, want [Hello, ' world', !]", tokens)
	}
	for _, k := range []agent.EventKind{agent.EventTurnStart, agent.EventMemory, agent.EventToken, agent.EventFinal} {
		if kinds[k] == 0 {
			t.Errorf("missing event kind %q", k)
		}
	}
	if len(fake.Streamed) == 0 || len(fake.Generated) != 0 {
		t.Errorf("expected streaming path (Streamed=%d Generated=%d)", len(fake.Streamed), len(fake.Generated))
	}
}

func TestAgent_Turn_StreamingHandlesToolCall(t *testing.T) {
	addTool := model.ToolSpec{Name: "echo", Description: "echo", Parameters: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`)}
	call := 0
	fake := testllm.New()
	fake.GenerateStreamFn = func(_ context.Context, _ model.GenerateRequest) (<-chan model.StreamChunk, error) {
		call++
		ch := make(chan model.StreamChunk, 2)
		if call == 1 {
			// First turn: a tool call (arrives as a ToolCallDelta).
			ch <- model.StreamChunk{ToolCallDelta: &model.ToolCall{Name: "echo", Arguments: json.RawMessage(`{"x":"hi"}`)}, FinishReason: "tool_calls"}
		} else {
			ch <- model.StreamChunk{Delta: "done", FinishReason: "stop"}
		}
		close(ch)
		return ch, nil
	}
	disp := agent.DispatcherFunc(func(_ context.Context, c model.ToolCall) (string, bool, error) {
		return "echoed:" + string(c.Arguments), false, nil
	})
	var mu sync.Mutex
	kinds := map[agent.EventKind]int{}
	var toolName, toolResult string
	obs := func(e agent.Event) {
		mu.Lock()
		defer mu.Unlock()
		kinds[e.Kind]++
		if e.Kind == agent.EventToolCall {
			toolName = e.Tool
		}
		if e.Kind == agent.EventToolResult {
			toolResult = e.Result
		}
	}
	a := buildStreamingAgent(t, fake, obs, []model.ToolSpec{addTool}, disp)
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "echo hi"})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Response != "done" {
		t.Fatalf("response = %q, want done", res.Response)
	}
	mu.Lock()
	defer mu.Unlock()
	if toolName != "echo" || !strings.Contains(toolResult, "echoed:") {
		t.Errorf("tool events wrong: name=%q result=%q", toolName, toolResult)
	}
	if kinds[agent.EventToolCall] == 0 || kinds[agent.EventToolResult] == 0 {
		t.Errorf("missing tool events: %v", kinds)
	}
}

// testWriter sends log writes to t.Logf so test logs only appear on
// failure. Implements io.Writer.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// --- Tests ---

func TestAgent_NewRequiresFields(t *testing.T) {
	cases := map[string]agent.Config{
		"missing AgentID":    {},
		"missing Router":     {AgentID: "a"},
		"missing Working":    {AgentID: "a", Router: &model.Router{}},
		"missing Tools":      {AgentID: "a", Router: &model.Router{}, Working: working.New()},
		"missing Dispatcher": {AgentID: "a", Router: &model.Router{}, Working: working.New(), Tools: agent.NewStaticRegistry()},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := agent.New(cfg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestAgent_NewFillsDefaults(t *testing.T) {
	reg, _ := model.NewRegistry("")
	router := model.NewRouter(reg, nil)
	disp := agent.DispatcherFunc(func(_ context.Context, _ model.ToolCall) (string, bool, error) {
		return "", false, nil
	})
	a, err := agent.New(agent.Config{
		AgentID: "a", Router: router, Working: working.New(),
		Tools: agent.NewStaticRegistry(), Dispatcher: disp,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a == nil {
		t.Fatal("nil agent")
	}
}

func TestAgent_TurnRequiresUserQuery(t *testing.T) {
	a, _, _, _ := buildAgent(t, model.Profile{}, nil, dispatchOK("ok"))
	_, err := a.Turn(context.Background(), agent.TurnRequest{})
	if err == nil {
		t.Fatal("expected error on empty UserQuery")
	}
}

func TestAgent_RouteHookIsAsyncAndLateTagsReachEpisode(t *testing.T) {
	fake := testllm.New()
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "unchanged answer", FinishReason: "stop"}, nil
	}
	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	router := model.NewRouter(reg, slog.Default())
	router.RegisterBackend("vllm", func(_ context.Context, _ model.Profile, _ *slog.Logger) (any, error) {
		return fake, nil
	})
	estore, err := episodic.Open(filepath.Join(t.TempDir(), "ep.db"))
	if err != nil {
		t.Fatalf("episodic.Open: %v", err)
	}
	t.Cleanup(func() { _ = estore.Close() })

	started := make(chan agent.RouteRequest, 1)
	release := make(chan struct{})
	events := make(chan agent.Event, 8)
	disp := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) {
		return "", false, nil
	})
	a, err := agent.New(agent.Config{
		AgentID: "route-agent", SessionID: "route-session", Router: router,
		Working: working.New(), Episodic: estore, Tools: agent.NewStaticRegistry(), Dispatcher: disp,
		Observer: func(e agent.Event) { events <- e },
		RouteHook: func(_ context.Context, req agent.RouteRequest) agent.RouteObservation {
			started <- req
			<-release
			return agent.RouteObservation{
				Text: "shadow · big · complexity 4",
				Tags: []string{"route:label=big", "route:complexity=4", "route:mode=shadow"},
			}
		},
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	type turnResult struct {
		res *agent.TurnResult
		err error
	}
	done := make(chan turnResult, 1)
	go func() {
		res, turnErr := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "typed prompt"})
		done <- turnResult{res: res, err: turnErr}
	}()

	var hookReq agent.RouteRequest
	select {
	case hookReq = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("route hook was not launched")
	}
	if hookReq.AgentID != "route-agent" || hookReq.SessionID != "route-session" || hookReq.UserQuery != "typed prompt" {
		t.Fatalf("route request = %+v", hookReq)
	}
	var turnTrace string
	select {
	case got := <-done:
		if got.err != nil || got.res == nil || got.res.Response != "unchanged answer" {
			t.Fatalf("Turn while hook blocked: res=%+v err=%v", got.res, got.err)
		}
		turnTrace = got.res.TraceID
	case <-time.After(2 * time.Second):
		t.Fatal("Turn waited for the route hook")
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		eps, recentErr := estore.Recent(context.Background(), "route-agent", 1)
		if recentErr != nil {
			t.Fatalf("Recent: %v", recentErr)
		}
		if len(eps) == 1 && strings.Join(eps[0].Tags, ",") == "route:label=big,route:complexity=4,route:mode=shadow" {
			break
		}
		if time.Now().After(deadline) {
			if len(eps) == 1 {
				t.Fatalf("late route tags = %v", eps[0].Tags)
			}
			t.Fatal("episode was not persisted")
		}
		time.Sleep(10 * time.Millisecond)
	}

	foundRoute := false
	for !foundRoute {
		select {
		case e := <-events:
			if e.Kind == agent.EventRoute {
				foundRoute = e.Text == "shadow · big · complexity 4"
				// Late, but still the turn's own line (TEN-362).
				if e.TraceID != turnTrace {
					t.Errorf("late EventRoute trace = %q, want %q", e.TraceID, turnTrace)
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatal("EventRoute was not emitted")
		}
	}
}

func TestAgent_RouteHookPanicNeverFailsTurn(t *testing.T) {
	fake := testllm.New()
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "ok", FinishReason: "stop"}, nil
	}
	reg, _ := model.NewRegistry("")
	router := model.NewRouter(reg, slog.Default())
	router.RegisterBackend("vllm", func(_ context.Context, _ model.Profile, _ *slog.Logger) (any, error) { return fake, nil })
	disp := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "", false, nil })
	a, err := agent.New(agent.Config{
		AgentID: "a", Router: router, Working: working.New(), Tools: agent.NewStaticRegistry(), Dispatcher: disp,
		RouteHook: func(context.Context, agent.RouteRequest) agent.RouteObservation { panic("boom") },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "hello"})
	if err != nil || res.Response != "ok" {
		t.Fatalf("route panic changed turn: res=%+v err=%v", res, err)
	}
}

func TestAgent_Turn_SingleIterationNoTools(t *testing.T) {
	a, fake, ws, _ := buildAgent(t, model.Profile{}, nil, dispatchOK("unused"))
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "hello back", FinishReason: "stop"}, nil
	}

	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "hi"})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Response != "hello back" {
		t.Errorf("Response = %q, want %q", res.Response, "hello back")
	}
	if res.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1", res.Iterations)
	}
	// Working set should have user + assistant.
	msgs := ws.Messages()
	if len(msgs) != 2 {
		t.Fatalf("working set len = %d, want 2", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Errorf("working set order wrong: %+v", msgs)
	}
}

func TestAgent_Turn_LoopsWithToolCall(t *testing.T) {
	tools := []model.ToolSpec{
		{Name: "search", Description: "search the web", Parameters: json.RawMessage(`{}`)},
	}
	calls := 0
	disp := agent.DispatcherFunc(func(_ context.Context, c model.ToolCall) (string, bool, error) {
		calls++
		if c.Name != "search" {
			return "", true, errors.New("wrong tool")
		}
		return "result for: " + string(c.Arguments), false, nil
	})
	a, fake, ws, _ := buildAgent(t, model.Profile{}, tools, disp)

	// Script: iter 1 → tool call, iter 2 → final.
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		switch len(fake.Generated) {
		case 1:
			return &model.GenerateResponse{
				ToolCalls: []model.ToolCall{{
					ID: "call_1", Name: "search",
					Arguments: json.RawMessage(`{"q":"go"}`),
				}},
				FinishReason: "tool_calls",
			}, nil
		case 2:
			return &model.GenerateResponse{Text: "Go is great.", FinishReason: "stop"}, nil
		default:
			return nil, errors.New("unexpected iteration")
		}
	}

	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "tell me about Go"})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Iterations != 2 {
		t.Errorf("Iterations = %d, want 2", res.Iterations)
	}
	if calls != 1 {
		t.Errorf("dispatch called %d times, want 1", calls)
	}
	if res.Response != "Go is great." {
		t.Errorf("Response = %q", res.Response)
	}
	if len(res.ToolTrace) != 1 {
		t.Errorf("ToolTrace len = %d, want 1", len(res.ToolTrace))
	}
	// Working set should have: user / assistant(tool_call) / tool / assistant(final).
	msgs := ws.Messages()
	if len(msgs) != 4 {
		t.Fatalf("working set len = %d, want 4: %+v", len(msgs), msgs)
	}
	wantRoles := []string{"user", "assistant", "tool", "assistant"}
	for i, want := range wantRoles {
		if msgs[i].Role != want {
			t.Errorf("msg[%d] role = %q, want %q", i, msgs[i].Role, want)
		}
	}
}

func TestAgent_Turn_LoopCeilingForcesSynthesis(t *testing.T) {
	tools := []model.ToolSpec{
		{Name: "search", Parameters: json.RawMessage(`{}`)},
	}
	a, fake, _, _ := buildAgent(t, model.Profile{}, tools, dispatchOK("noop"))

	// Always emit tool calls — never finish naturally. Vary the args each
	// iteration so this exercises the CEILING path, not the oscillation guard
	// (TEN-261), which would otherwise trip on an identical repeated call.
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{
			ToolCalls: []model.ToolCall{{
				ID:        "call",
				Name:      "search",
				Arguments: json.RawMessage(fmt.Sprintf(`{"n":%d}`, len(fake.Generated))),
			}},
			FinishReason: "tool_calls",
		}, nil
	}
	// On the synthesis call (no tools in request), the LLM should be
	// asked to produce text. Detect by checking req.Tools is empty in
	// the fake's GenerateFn. We override partway through... simpler
	// approach: use a counter and switch behavior when iteration > 10
	// (the default ceiling for qwen3.6-72b).
	original := fake.GenerateFn
	fake.GenerateFn = func(ctx context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
		if len(req.Tools) == 0 {
			// Synthesis call.
			return &model.GenerateResponse{Text: "best-effort final answer", FinishReason: "stop"}, nil
		}
		return original(ctx, req)
	}

	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "loop forever"})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if !res.Truncated {
		t.Error("Truncated should be true after ceiling hit")
	}
	if !errors.Is(res.Error, agent.ErrLoopCeilingReached) {
		t.Errorf("Error = %v, want ErrLoopCeilingReached", res.Error)
	}
	if res.Response != "best-effort final answer" {
		t.Errorf("Response = %q, want synthesis output", res.Response)
	}
}

func TestAgent_Turn_ValidationFailureRetriesOnce(t *testing.T) {
	tools := []model.ToolSpec{
		{Name: "search", Parameters: json.RawMessage(`{"required":["q"]}`)},
	}
	a, fake, _, _ := buildAgent(t, model.Profile{}, tools, dispatchOK("found it"))

	// Iter 1: invalid (missing required "q"). Iter 2: valid. Iter 3: final.
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		switch len(fake.Generated) {
		case 1:
			return &model.GenerateResponse{
				ToolCalls: []model.ToolCall{{
					ID: "bad", Name: "search", Arguments: json.RawMessage(`{}`),
				}},
				FinishReason: "tool_calls",
			}, nil
		case 2:
			return &model.GenerateResponse{
				ToolCalls: []model.ToolCall{{
					ID: "good", Name: "search", Arguments: json.RawMessage(`{"q":"go"}`),
				}},
				FinishReason: "tool_calls",
			}, nil
		case 3:
			return &model.GenerateResponse{Text: "done", FinishReason: "stop"}, nil
		}
		return nil, errors.New("unexpected iteration")
	}

	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "search for go"})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Response != "done" {
		t.Errorf("Response = %q", res.Response)
	}
	if res.Iterations != 3 {
		t.Errorf("Iterations = %d, want 3", res.Iterations)
	}
}

func TestAgent_Turn_BailsOnConsecutiveValidationFailures(t *testing.T) {
	tools := []model.ToolSpec{
		{Name: "search", Parameters: json.RawMessage(`{"required":["q"]}`)},
	}
	a, fake, _, _ := buildAgent(t, model.Profile{}, tools, dispatchOK("never"))

	// Always emit invalid calls.
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{
			ToolCalls: []model.ToolCall{{
				ID: "bad", Name: "search", Arguments: json.RawMessage(`{}`),
			}},
			FinishReason: "tool_calls",
		}, nil
	}

	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "x"})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if !errors.Is(res.Error, agent.ErrTooManyValidationFailures) {
		t.Errorf("Error = %v, want ErrTooManyValidationFailures", res.Error)
	}
}

func TestAgent_Turn_UnknownToolFailsValidation(t *testing.T) {
	// Register only "search". Model will call "unknown".
	tools := []model.ToolSpec{{Name: "search"}}
	a, fake, ws, _ := buildAgent(t, model.Profile{}, tools, dispatchOK("noop"))

	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		switch len(fake.Generated) {
		case 1:
			return &model.GenerateResponse{
				ToolCalls:    []model.ToolCall{{ID: "x", Name: "unknown", Arguments: json.RawMessage(`{}`)}},
				FinishReason: "tool_calls",
			}, nil
		case 2:
			return &model.GenerateResponse{Text: "ok, moving on", FinishReason: "stop"}, nil
		}
		return nil, errors.New("unexpected iter")
	}
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Response != "ok, moving on" {
		t.Errorf("Response = %q", res.Response)
	}
	// Working set should contain the validation-error tool message.
	msgs := ws.Messages()
	hasValidationError := false
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, "unknown tool") {
			hasValidationError = true
		}
	}
	if !hasValidationError {
		t.Errorf("working set missing validation error tool message: %+v", msgs)
	}
}

func TestAgent_Turn_PersistsEpisode(t *testing.T) {
	a, fake, _, estore := buildAgent(t, model.Profile{}, nil, dispatchOK("noop"))
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "ok", FinishReason: "stop"}, nil
	}
	_, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "first turn"})
	if err != nil {
		t.Fatal(err)
	}
	n, _ := estore.Count(context.Background(), false)
	if n != 1 {
		t.Errorf("episode count = %d, want 1", n)
	}
}

func TestAgent_Turn_RespectsContextCancellation(t *testing.T) {
	a, fake, _, _ := buildAgent(t, model.Profile{}, []model.ToolSpec{{Name: "search"}}, dispatchOK("noop"))

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after first generation.
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		cancel()
		return &model.GenerateResponse{
			ToolCalls:    []model.ToolCall{{ID: "x", Name: "search", Arguments: json.RawMessage(`{}`)}},
			FinishReason: "tool_calls",
		}, nil
	}
	res, err := a.Turn(ctx, agent.TurnRequest{UserQuery: "x"})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if !errors.Is(res.Error, context.Canceled) {
		t.Errorf("Error = %v, want context.Canceled", res.Error)
	}
}

func TestAgent_Turn_ParallelToolDispatch(t *testing.T) {
	tools := []model.ToolSpec{
		{Name: "search", Parameters: json.RawMessage(`{}`)},
	}
	var mu sync.Mutex
	concurrent := 0
	maxConcurrent := 0
	disp := agent.DispatcherFunc(func(_ context.Context, _ model.ToolCall) (string, bool, error) {
		mu.Lock()
		concurrent++
		if concurrent > maxConcurrent {
			maxConcurrent = concurrent
		}
		mu.Unlock()
		// brief sleep would help observe overlap but tests should
		// stay fast; correctness here is about the cap, not timing.
		mu.Lock()
		concurrent--
		mu.Unlock()
		return "ok", false, nil
	})
	a, fake, _, _ := buildAgent(t, model.Profile{}, tools, disp)
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		switch len(fake.Generated) {
		case 1:
			// Emit 5 tool calls in one iteration.
			calls := make([]model.ToolCall, 5)
			for i := range calls {
				calls[i] = model.ToolCall{ID: "x", Name: "search", Arguments: json.RawMessage(`{}`)}
			}
			return &model.GenerateResponse{ToolCalls: calls, FinishReason: "tool_calls"}, nil
		case 2:
			return &model.GenerateResponse{Text: "done", FinishReason: "stop"}, nil
		}
		return nil, errors.New("unexpected iter")
	}
	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolTrace) != 5 {
		t.Errorf("ToolTrace len = %d, want 5", len(res.ToolTrace))
	}
}

// --- helpers ---

func dispatchOK(s string) agent.ToolDispatcher {
	return agent.DispatcherFunc(func(_ context.Context, _ model.ToolCall) (string, bool, error) {
		return s, false, nil
	})
}

// --- StaticRegistry tests ---

func TestStaticRegistry_RegisterAndGet(t *testing.T) {
	r := agent.NewStaticRegistry()
	r.Register(model.ToolSpec{Name: "a", Description: "tool a"})
	r.Register(model.ToolSpec{Name: "b", Description: "tool b"})

	a, ok := r.Get("a")
	if !ok || a.Name != "a" {
		t.Errorf("Get(a) = %+v ok=%v", a, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("Get(nope) should miss")
	}
	if len(r.All()) != 2 {
		t.Errorf("All() len = %d, want 2", len(r.All()))
	}
}

func TestStaticRegistry_RegisterReplaces(t *testing.T) {
	r := agent.NewStaticRegistry()
	r.Register(model.ToolSpec{Name: "x", Description: "v1"})
	r.Register(model.ToolSpec{Name: "x", Description: "v2"})
	got, _ := r.Get("x")
	if got.Description != "v2" {
		t.Errorf("Description = %q, want v2", got.Description)
	}
	if len(r.All()) != 1 {
		t.Errorf("All() len = %d, want 1 (replace, not duplicate)", len(r.All()))
	}
}

func TestStaticRegistry_SearchReturnsK(t *testing.T) {
	r := agent.NewStaticRegistry()
	for _, name := range []string{"a", "b", "c", "d"} {
		r.Register(model.ToolSpec{Name: name})
	}
	got, err := r.Search(context.Background(), nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("Search(2) len = %d, want 2", len(got))
	}
}

// TEN-215: a transient stream drop — a hosted provider's load balancer
// recycling the pooled HTTP/2 connection with a GOAWAY, caught mid-stream — must
// be retried on a fresh connection, not kill the turn. The planner call is
// idempotent and nothing is committed until plan() returns, so re-issuing is safe.
func TestAgent_RetriesTransientStreamDrop(t *testing.T) {
	noop := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "", false, nil })
	fake := testllm.New()

	var calls atomic.Int32
	fake.GenerateStreamFn = func(_ context.Context, _ model.GenerateRequest) (<-chan model.StreamChunk, error) {
		n := calls.Add(1)
		ch := make(chan model.StreamChunk, 2)
		if n == 1 {
			// First attempt dies mid-stream with a GOAWAY-shaped read error.
			ch <- model.StreamChunk{Error: fmt.Errorf("%w: stream read: http2: server sent GOAWAY and no streams", model.ErrInternal)}
			close(ch)
			return ch, nil
		}
		ch <- model.StreamChunk{Delta: "recovered answer", FinishReason: "stop"}
		close(ch)
		return ch, nil
	}
	a := buildStreamingAgent(t, fake, func(agent.Event) {}, nil, noop)

	res, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "hi"})
	if err != nil {
		t.Fatalf("Turn errored despite a retryable stream drop: %v", err)
	}
	if got := calls.Load(); got < 2 {
		t.Errorf("expected a retry (>=2 planner calls), got %d", got)
	}
	if !strings.Contains(res.Response, "recovered answer") {
		t.Errorf("expected the retried response, got %q", res.Response)
	}
}

// TEN-215: a NON-transient error (e.g. an invalid request) must NOT be retried —
// re-issuing can't help and would just burn the budget.
func TestAgent_DoesNotRetryNonTransientError(t *testing.T) {
	noop := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "", false, nil })
	fake := testllm.New()

	var calls atomic.Int32
	fake.GenerateStreamFn = func(_ context.Context, _ model.GenerateRequest) (<-chan model.StreamChunk, error) {
		calls.Add(1)
		ch := make(chan model.StreamChunk, 1)
		ch <- model.StreamChunk{Error: fmt.Errorf("%w: bad tool schema", model.ErrInvalidRequest)}
		close(ch)
		return ch, nil
	}
	a := buildStreamingAgent(t, fake, func(agent.Event) {}, nil, noop)

	if _, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "hi"}); err == nil {
		t.Fatal("expected the turn to fail on a non-transient error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("non-transient error must not be retried; got %d planner calls", got)
	}
}

// TEN-215: a cancelled context stops the retry loop immediately rather than
// sleeping through the backoff and re-issuing.
func TestAgent_StopsRetryOnCancel(t *testing.T) {
	noop := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "", false, nil })
	fake := testllm.New()

	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	fake.GenerateStreamFn = func(_ context.Context, _ model.GenerateRequest) (<-chan model.StreamChunk, error) {
		calls.Add(1)
		cancel() // user interrupts mid-turn
		ch := make(chan model.StreamChunk, 1)
		ch <- model.StreamChunk{Error: fmt.Errorf("%w: stream read: http2: server sent GOAWAY", model.ErrInternal)}
		close(ch)
		return ch, nil
	}
	a := buildStreamingAgent(t, fake, func(agent.Event) {}, nil, noop)

	if _, err := a.Turn(ctx, agent.TurnRequest{UserQuery: "hi"}); err != nil {
		_ = err // a cancelled turn may surface as a clean stop or an error; either is acceptable
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("a cancelled context must not be retried; got %d planner calls", got)
	}
}

// TEN-216: TurnRequest.LoopCeiling overrides the profile's PlanLoopCeiling for a
// single turn — what /goal uses to iterate freely without touching the global
// ceiling. A fake that tool-calls forever lets us count loop iterations
// (one EventToolCall per iteration) and confirm the override sets them exactly.
func TestAgent_LoopCeilingOverride(t *testing.T) {
	tool := model.ToolSpec{Name: "noop", Description: "no-op", Parameters: json.RawMessage(`{"type":"object"}`)}
	disp := agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "ok", false, nil })

	run := func(ceiling int) int32 {
		fake := testllm.New()
		var planN atomic.Int32
		fake.GenerateStreamFn = func(_ context.Context, req model.GenerateRequest) (<-chan model.StreamChunk, error) {
			ch := make(chan model.StreamChunk, 1)
			if len(req.Tools) > 0 {
				// In the plan loop (tools present) keep calling a tool — never finish.
				// Vary the args each iteration so this exercises the CEILING, not the
				// oscillation guard (TEN-261), which trips on an identical repeat.
				n := planN.Add(1)
				ch <- model.StreamChunk{ToolCallDelta: &model.ToolCall{ID: "1", Name: "noop", Arguments: json.RawMessage(fmt.Sprintf(`{"n":%d}`, n))}, FinishReason: "tool_calls"}
			} else {
				// Forced synthesis runs tools-off — let it terminate cleanly.
				ch <- model.StreamChunk{Delta: "final", FinishReason: "stop"}
			}
			close(ch)
			return ch, nil
		}
		var toolCalls atomic.Int32
		obs := func(e agent.Event) {
			if e.Kind == agent.EventToolCall {
				toolCalls.Add(1)
			}
		}
		a := buildStreamingAgent(t, fake, obs, []model.ToolSpec{tool}, disp)
		// A ceiling hit forces synthesis; it may surface as a truncated result
		// rather than an error. Either is fine — we assert on iteration count.
		_, _ = a.Turn(context.Background(), agent.TurnRequest{UserQuery: "go", LoopCeiling: ceiling})
		return toolCalls.Load()
	}

	if got := run(2); got != 2 {
		t.Errorf("LoopCeiling=2 ran %d tool-call iterations, want 2", got)
	}
	if got := run(5); got != 5 {
		t.Errorf("LoopCeiling=5 ran %d tool-call iterations, want 5", got)
	}
}
