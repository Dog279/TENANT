package model

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// scriptLLM is a fake LLM: Generate returns genErr (or a success), GenerateStream
// returns streamErr at call time (or emits chunks). Counts calls.
type scriptLLM struct {
	name      string
	genErr    error
	genText   string
	streamErr error
	chunks    []StreamChunk
	calls     int
}

func (s *scriptLLM) Generate(_ context.Context, _ GenerateRequest) (*GenerateResponse, error) {
	s.calls++
	if s.genErr != nil {
		return nil, s.genErr
	}
	return &GenerateResponse{Text: s.genText}, nil
}
func (s *scriptLLM) GenerateStream(_ context.Context, _ GenerateRequest) (<-chan StreamChunk, error) {
	s.calls++
	if s.streamErr != nil {
		return nil, s.streamErr
	}
	ch := make(chan StreamChunk, len(s.chunks)+1)
	for _, c := range s.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}
func (s *scriptLLM) TokenCount(_ context.Context, text string) (int, error) { return len(text), nil }

func newFB(t *testing.T, links ...*scriptLLM) (*FallbackLLM, *[]FailoverEvent) {
	t.Helper()
	var events []FailoverEvent
	fl := make([]fbLink, len(links))
	for i, l := range links {
		fl[i] = fbLink{llm: l, label: l.name}
	}
	fb := NewFallbackLLM(fl, func(ev FailoverEvent) { events = append(events, ev) })
	return fb, &events
}

func TestFailoverAction_Matrix(t *testing.T) {
	yes := []error{ErrRateLimited, ErrInsufficientBalance, ErrEndpointDown, ErrInternal,
		fmt.Errorf("wrapped: %w", ErrRateLimited)}
	no := []error{nil, ErrContextOverflow, ErrInvalidRequest, ErrCancelled, context.Canceled,
		fmt.Errorf("some raw error")}
	for _, e := range yes {
		if !FailoverAction(e) {
			t.Errorf("FailoverAction(%v) = false, want true", e)
		}
	}
	for _, e := range no {
		if FailoverAction(e) {
			t.Errorf("FailoverAction(%v) = true, want false (fail-closed)", e)
		}
	}
}

func TestFallback_Generate_FailsOverOnRateLimit(t *testing.T) {
	primary := &scriptLLM{name: "zai", genErr: ErrRateLimited}
	backup := &scriptLLM{name: "qwen", genText: "answer from qwen"}
	fb, events := newFB(t, primary, backup)

	resp, err := fb.Generate(context.Background(), GenerateRequest{})
	if err != nil || resp.Text != "answer from qwen" {
		t.Fatalf("should fail over to qwen: resp=%+v err=%v", resp, err)
	}
	if primary.calls != 1 || backup.calls != 1 {
		t.Errorf("expected one call each, got primary=%d backup=%d", primary.calls, backup.calls)
	}
	if len(*events) != 1 || (*events)[0].From != "zai" || (*events)[0].To != "qwen" {
		t.Errorf("expected a zai→qwen failover event, got %+v", *events)
	}
}

func TestFallback_Generate_NoFailoverOnInvalidRequest(t *testing.T) {
	primary := &scriptLLM{name: "zai", genErr: ErrInvalidRequest}
	backup := &scriptLLM{name: "qwen", genText: "should not be reached"}
	fb, events := newFB(t, primary, backup)

	if _, err := fb.Generate(context.Background(), GenerateRequest{}); err == nil {
		t.Fatal("invalid-request should surface, not fail over")
	}
	if backup.calls != 0 {
		t.Errorf("backup must NOT be called on a non-failover error; calls=%d", backup.calls)
	}
	if len(*events) != 0 {
		t.Errorf("no failover event expected; got %+v", *events)
	}
}

func TestFallback_Generate_PrimarySuccessSkipsChain(t *testing.T) {
	primary := &scriptLLM{name: "zai", genText: "ok"}
	backup := &scriptLLM{name: "qwen"}
	fb, _ := newFB(t, primary, backup)
	if _, err := fb.Generate(context.Background(), GenerateRequest{}); err != nil {
		t.Fatal(err)
	}
	if backup.calls != 0 {
		t.Errorf("primary success must not call the chain; backup.calls=%d", backup.calls)
	}
}

func TestFallback_Generate_AllExhausted(t *testing.T) {
	primary := &scriptLLM{name: "zai", genErr: ErrRateLimited}
	backup := &scriptLLM{name: "qwen", genErr: ErrEndpointDown}
	fb, _ := newFB(t, primary, backup)
	_, err := fb.Generate(context.Background(), GenerateRequest{})
	if err == nil {
		t.Fatal("all links failing should return the last error (→ caller degrades to echo)")
	}
}

func TestFallback_Cooldown_SkipsThenRecovers(t *testing.T) {
	primary := &scriptLLM{name: "zai", genErr: ErrRateLimited}
	backup := &scriptLLM{name: "qwen", genText: "qwen"}
	fb, _ := newFB(t, primary, backup)
	now := time.Unix(1000, 0)
	fb.now = func() time.Time { return now }

	// Call 1: primary rate-limited → fail over, primary cooled down.
	fb.Generate(context.Background(), GenerateRequest{})
	if primary.calls != 1 {
		t.Fatalf("primary should be tried once on call 1; got %d", primary.calls)
	}
	// Call 2 (still within cooldown): primary is skipped → backup tried first.
	fb.Generate(context.Background(), GenerateRequest{})
	if primary.calls != 1 {
		t.Errorf("cooled primary should be skipped on call 2; primary.calls=%d (want 1)", primary.calls)
	}
	if backup.calls != 2 {
		t.Errorf("backup should serve both calls; backup.calls=%d (want 2)", backup.calls)
	}
	// Heal the primary + advance past the cooldown → primary retried first.
	primary.genErr = nil
	primary.genText = "zai back"
	now = now.Add(31 * time.Second)
	resp, _ := fb.Generate(context.Background(), GenerateRequest{})
	if primary.calls != 2 || resp.Text != "zai back" {
		t.Errorf("after cooldown lapse the preferred primary should be retried first; primary.calls=%d resp=%q", primary.calls, resp.Text)
	}
}

func TestFallback_Stream_FailsOverBeforeFirstToken(t *testing.T) {
	// (a) call-time error on primary → fail over.
	primary := &scriptLLM{name: "zai", streamErr: ErrRateLimited}
	backup := &scriptLLM{name: "qwen", chunks: []StreamChunk{{Delta: "hello"}, {Delta: " world"}}}
	fb, events := newFB(t, primary, backup)
	got := drain(t, fb)
	if got != "hello world" {
		t.Errorf("call-time stream error should fail over to qwen; got %q", got)
	}
	if len(*events) != 1 {
		t.Errorf("expected one failover event; got %+v", *events)
	}

	// (b) pre-token error chunk on primary → fail over.
	primary2 := &scriptLLM{name: "zai", chunks: []StreamChunk{{Error: ErrEndpointDown}}}
	backup2 := &scriptLLM{name: "qwen", chunks: []StreamChunk{{Delta: "recovered"}}}
	fb2, _ := newFB(t, primary2, backup2)
	if got := drain(t, fb2); got != "recovered" {
		t.Errorf("pre-token error chunk should fail over; got %q", got)
	}
}

func TestFallback_Stream_NoFailoverAfterFirstToken(t *testing.T) {
	// THE streaming invariant: a mid-stream error AFTER tokens are emitted must
	// propagate, NOT fail over (failing over would splice a 2nd model's tokens
	// onto a half-streamed answer).
	primary := &scriptLLM{name: "zai", chunks: []StreamChunk{{Delta: "partial "}, {Error: ErrInternal}}}
	backup := &scriptLLM{name: "qwen", chunks: []StreamChunk{{Delta: "SHOULD NOT APPEAR"}}}
	fb, events := newFB(t, primary, backup)

	ch, _ := fb.GenerateStream(context.Background(), GenerateRequest{})
	var text string
	var sawErr bool
	for c := range ch {
		text += c.Delta
		if c.Error != nil {
			sawErr = true
		}
	}
	if text != "partial " || !sawErr {
		t.Errorf("post-token error must propagate as-is; text=%q sawErr=%v", text, sawErr)
	}
	if backup.calls != 0 {
		t.Errorf("must NOT fail over after a token was emitted; backup.calls=%d", backup.calls)
	}
	if len(*events) != 0 {
		t.Errorf("no failover event after first token; got %+v", *events)
	}
}

func drain(t *testing.T, fb *FallbackLLM) string {
	t.Helper()
	ch, err := fb.GenerateStream(context.Background(), GenerateRequest{})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	var text string
	for c := range ch {
		text += c.Delta
		if c.Error != nil {
			t.Fatalf("unexpected stream error: %v", c.Error)
		}
	}
	return text
}
