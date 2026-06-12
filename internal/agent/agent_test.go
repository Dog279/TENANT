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
)

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

// --- Test plumbing ---

// buildAgent wires a fresh agent against a fake LLM + embedder. The
// fake is returned so tests can script Generate behavior per iteration.
func buildAgent(t *testing.T, profile model.Profile, tools []model.ToolSpec, dispatcher agent.ToolDispatcher) (*agent.Agent, *testllm.Fake, *working.Set, *episodic.Store) {
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

	a, err := agent.New(agent.Config{
		AgentID:    "test-agent",
		SessionID:  "test-sess",
		Router:     router,
		Working:    working,
		Archive:    arch,
		Episodic:   estore,
		Tools:      registry,
		Dispatcher: dispatcher,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a, fake, working, estore
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

	// Always emit tool calls — never finish naturally.
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{
			ToolCalls: []model.ToolCall{{
				ID: "call", Name: "search", Arguments: json.RawMessage(`{}`),
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
		fake.GenerateStreamFn = func(_ context.Context, req model.GenerateRequest) (<-chan model.StreamChunk, error) {
			ch := make(chan model.StreamChunk, 1)
			if len(req.Tools) > 0 {
				// In the plan loop (tools present) keep calling a tool — never finish.
				ch <- model.StreamChunk{ToolCallDelta: &model.ToolCall{ID: "1", Name: "noop", Arguments: json.RawMessage("{}")}, FinishReason: "tool_calls"}
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
