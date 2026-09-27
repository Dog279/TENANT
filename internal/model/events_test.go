package model_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"tenant/internal/logs/event"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
	"tenant/internal/runtrace"
)

// capture keeps the catalogued events written while it is installed, with
// the trace of the context each was written under.
type capture struct {
	mu     sync.Mutex
	events []*event.Event
}

func (c *capture) Emit(ctx context.Context, e *event.Event) {
	e.Corr.TraceID = runtrace.IDFromContext(ctx)
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

func (c *capture) Audit(ctx context.Context, e *event.Event) error { c.Emit(ctx, e); return nil }

// lines renders the model events as "id opcode profile trace", one each.
func (c *capture) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		if e.Provider != "model" {
			continue
		}
		var profile, tokens string
		for _, f := range e.Data {
			switch f.Key {
			case "profile", "from":
				profile = f.Text()
			case "completion_tokens":
				tokens = " tokens=" + f.Text()
			}
		}
		out = append(out, fmt.Sprintf("%d %s %s %s%s", e.ID, e.Opcode, profile, e.Corr.TraceID, tokens))
	}
	return out
}

// installCapture makes a capture the event emitter for the test. Not for
// parallel tests: the emitter is process-wide.
func installCapture(t *testing.T) *capture {
	c := &capture{}
	event.Install(c)
	t.Cleanup(func() { event.Install(nil) })
	return c
}

// router binds the planner to "primary", falling back to "backup", each a
// fake backend.
func router(t *testing.T, primary, backup *testllm.Fake) *model.Router {
	t.Helper()
	reg := model.NewEmptyRegistry()
	for _, id := range []string{"primary", "backup"} {
		if err := reg.Add(model.Profile{ID: id, Role: model.RolePlanner, Backend: "fake", Model: "m-" + id, Endpoint: "http://" + id, ContextLength: 1000}); err != nil {
			t.Fatal(err)
		}
	}
	r := model.NewRouter(reg, slog.Default())
	r.RegisterBackend("fake", func(_ context.Context, p model.Profile, _ *slog.Logger) (any, error) {
		if p.ID == "backup" {
			return backup, nil
		}
		return primary, nil
	})
	if err := r.PinRole(model.RolePlanner, "primary"); err != nil {
		t.Fatal(err)
	}
	r.SetFallbackChain(model.RolePlanner, []string{"backup"})
	return r
}

// Each call is an activity under the turn in ctx: a down primary fails its
// activity and is reported unavailable, the failover is a warning, and the
// backup's call stops with its tokens.
func TestModelCallsAreActivitiesUnderTheTurn(t *testing.T) {
	c := installCapture(t)
	primary, backup := testllm.New(), testllm.New()
	primary.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return nil, fmt.Errorf("%w: connection refused", model.ErrEndpointDown)
	}
	backup.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "ok", Usage: model.Usage{PromptTokens: 5, CompletionTokens: 7}}, nil
	}
	ctx := runtrace.WithSpan(context.Background(), runtrace.NewSpan("t1"))
	llm, _, err := router(t, primary, backup).LLMForRole(ctx, model.RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := llm.Generate(ctx, model.GenerateRequest{}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"9001 start primary t1",
		"3001  primary t1",
		"9001 fail primary t1",
		"2001  primary t1",
		"9001 start backup t1",
		"9002  backup t1 tokens=7",
		"9001 stop backup t1",
	}
	if got := c.lines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if start, fail := c.events[0], c.events[2]; start.Channel != event.Diagnostic || fail.Level != event.Error ||
		start.Corr.ActivityID == "" || fail.Corr.ActivityID != start.Corr.ActivityID {
		t.Fatalf("start %+v / fail %+v: want one diagnostic activity, failing at error", start, fail)
	}
}

// A streamed call's activity ends when its stream does, and a cancelled call
// stops rather than fails.
func TestStreamedCallEndsWithItsStream(t *testing.T) {
	c := installCapture(t)
	primary := testllm.New()
	primary.GenerateStreamFn = func(context.Context, model.GenerateRequest) (<-chan model.StreamChunk, error) {
		ch := make(chan model.StreamChunk, 2)
		ch <- model.StreamChunk{Delta: "hi"}
		ch <- model.StreamChunk{FinishReason: "stop", Usage: &model.Usage{PromptTokens: 1, CompletionTokens: 2}}
		close(ch)
		return ch, nil
	}
	primary.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return nil, fmt.Errorf("%w: interrupted", model.ErrCancelled)
	}
	r := router(t, primary, testllm.New())
	r.SetFallbackChain(model.RolePlanner, nil)
	llm, _, err := r.LLMForRole(context.Background(), model.RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := llm.GenerateStream(context.Background(), model.GenerateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	_, _ = llm.Generate(context.Background(), model.GenerateRequest{})
	want := "9001 start primary |9002  primary  tokens=2|9001 stop primary |9001 start primary |9001 stop primary "
	if got := strings.Join(c.lines(), "|"); got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
}
