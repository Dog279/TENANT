package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/model"
)

// fakeDispatcher returns canned outcomes per-call in sequence so a test
// can simulate "fail once, succeed on retry" easily.
type fakeDispatcher struct {
	mu    sync.Mutex
	queue []dispatchOutcome
	calls int
}

type dispatchOutcome struct {
	result string
	isErr  bool
	err    error
}

func (f *fakeDispatcher) Dispatch(_ context.Context, _ model.ToolCall) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.queue) == 0 {
		return "exhausted", true, nil
	}
	o := f.queue[0]
	f.queue = f.queue[1:]
	return o.result, o.isErr, o.err
}

// TestRetryDecorator_PassthroughOnSuccess — a first-call success returns
// immediately, no retry, no event. Anything else means we're adding
// latency to the happy path which is the whole reason ToolDispatcher
// is on the hot path.
func TestRetryDecorator_PassthroughOnSuccess(t *testing.T) {
	inner := &fakeDispatcher{queue: []dispatchOutcome{
		{result: "page text", isErr: false, err: nil},
	}}
	var events []Event
	rd := &RetryDecorator{
		Inner: inner, Eligible: DefaultEligibleTransient,
		Backoff: time.Millisecond, MaxRetries: 1,
		Observer: func(e Event) { events = append(events, e) },
	}
	result, isErr, err := rd.Dispatch(context.Background(), model.ToolCall{Name: "web_navigate"})
	if err != nil || isErr || result != "page text" {
		t.Errorf("passthrough broken: result=%q isErr=%v err=%v", result, isErr, err)
	}
	if inner.calls != 1 {
		t.Errorf("expected 1 call, got %d", inner.calls)
	}
	if len(events) != 0 {
		t.Errorf("happy path should emit no events, got %+v", events)
	}
}

// TestRetryDecorator_RetriesWrappedStringError — the load-bearing test
// from the tertiary review. Tenant plugins wrap transports into
// (string, true, nil); the decorator MUST inspect the result string,
// not just err, or it's a no-op against the very failures it targets.
// Simulates: web_navigate first returns "navigation failed: context
// deadline exceeded" (true, nil), then succeeds.
func TestRetryDecorator_RetriesWrappedStringError(t *testing.T) {
	inner := &fakeDispatcher{queue: []dispatchOutcome{
		{result: "navigation failed: context deadline exceeded", isErr: true, err: nil},
		{result: "<page text>", isErr: false, err: nil},
	}}
	var events []Event
	rd := &RetryDecorator{
		Inner: inner, Eligible: DefaultEligibleTransient,
		Backoff: time.Millisecond, MaxRetries: 1,
		Observer: func(e Event) { events = append(events, e) },
	}
	result, isErr, err := rd.Dispatch(context.Background(), model.ToolCall{Name: "web_navigate"})
	if err != nil || isErr || result != "<page text>" {
		t.Errorf("retry didn't recover: result=%q isErr=%v err=%v", result, isErr, err)
	}
	if inner.calls != 2 {
		t.Errorf("expected 2 calls (initial + 1 retry), got %d", inner.calls)
	}
	if len(events) != 1 || events[0].Kind != EventRetry {
		t.Errorf("expected one EventRetry, got %+v", events)
	}
	if !strings.Contains(events[0].Text, "attempt 1/1") || !strings.Contains(events[0].Text, "deadline exceeded") {
		t.Errorf("event text should name attempt and reason, got %q", events[0].Text)
	}
}

// TestRetryDecorator_RetriesTypedDeadlineExceeded — typed err path.
// When the inner returns context.DeadlineExceeded directly, retry.
func TestRetryDecorator_RetriesTypedDeadlineExceeded(t *testing.T) {
	inner := &fakeDispatcher{queue: []dispatchOutcome{
		{err: context.DeadlineExceeded},
		{result: "ok", isErr: false, err: nil},
	}}
	rd := &RetryDecorator{
		Inner: inner, Eligible: DefaultEligibleTransient,
		Backoff: time.Millisecond, MaxRetries: 1,
	}
	result, _, err := rd.Dispatch(context.Background(), model.ToolCall{Name: "sql_query"})
	if err != nil || result != "ok" {
		t.Errorf("typed-deadline retry didn't recover: result=%q err=%v", result, err)
	}
	if inner.calls != 2 {
		t.Errorf("expected 2 calls, got %d", inner.calls)
	}
}

// TestRetryDecorator_NoRetryForNonTransientString — a string-wrapped
// error that's NOT in the transient substring set passes through
// unchanged on first call. E.g. "schema error: column missing" — the
// model has useful judgment about that and should see it.
func TestRetryDecorator_NoRetryForNonTransientString(t *testing.T) {
	inner := &fakeDispatcher{queue: []dispatchOutcome{
		{result: "schema error: column missing", isErr: true, err: nil},
	}}
	rd := &RetryDecorator{
		Inner: inner, Eligible: DefaultEligibleTransient,
		Backoff: time.Millisecond, MaxRetries: 1,
	}
	_, isErr, err := rd.Dispatch(context.Background(), model.ToolCall{Name: "sql_query"})
	if err != nil || !isErr {
		t.Errorf("non-transient err should surface as-is: isErr=%v err=%v", isErr, err)
	}
	if inner.calls != 1 {
		t.Errorf("expected 1 call (no retry), got %d", inner.calls)
	}
}

// TestRetryDecorator_HardDenyForMutatingTools — even with a perfectly
// transient error, a mutating tool MUST never retry. Retrying a failed
// INSERT could silently double-insert; retrying a failed form-fill
// could submit twice. This is the highest-stakes drift guard.
func TestRetryDecorator_HardDenyForMutatingTools(t *testing.T) {
	denied := []string{
		"sql_exec", "web_click", "web_fill", "web_select",
		"x_post", "imessage_send", "gsuite_send", "memory_remember",
		"discord_send_message", "discord_react",
	}
	for _, name := range denied {
		t.Run(name, func(t *testing.T) {
			inner := &fakeDispatcher{queue: []dispatchOutcome{
				// Even a textbook-transient error — must NOT retry.
				{result: "context deadline exceeded", isErr: true, err: nil},
				{result: "would have succeeded", isErr: false, err: nil},
			}}
			rd := &RetryDecorator{
				Inner: inner, Eligible: DefaultEligibleTransient,
				Backoff: time.Millisecond, MaxRetries: 3,
			}
			result, isErr, err := rd.Dispatch(context.Background(), model.ToolCall{Name: name})
			if !isErr || err != nil {
				t.Errorf("%s: expected original error to surface; isErr=%v err=%v", name, isErr, err)
			}
			if !strings.Contains(result, "context deadline exceeded") {
				t.Errorf("%s: expected original result text, got %q", name, result)
			}
			if inner.calls != 1 {
				t.Errorf("%s: expected NO retry (calls=%d) — mutating tool MUST NOT retry", name, inner.calls)
			}
		})
	}
}

// TestRetryDecorator_GivesUpAfterMaxRetries — bounded by MaxRetries.
// Two failures in a row with MaxRetries=1 should produce: try, retry,
// surface the second failure. Three calls would be a bug.
func TestRetryDecorator_GivesUpAfterMaxRetries(t *testing.T) {
	inner := &fakeDispatcher{queue: []dispatchOutcome{
		{result: "connection refused", isErr: true, err: nil},
		{result: "connection refused", isErr: true, err: nil},
		{result: "would never get here", isErr: false, err: nil},
	}}
	rd := &RetryDecorator{
		Inner: inner, Eligible: DefaultEligibleTransient,
		Backoff: time.Millisecond, MaxRetries: 1,
	}
	result, isErr, _ := rd.Dispatch(context.Background(), model.ToolCall{Name: "web_navigate"})
	if !isErr || !strings.Contains(result, "connection refused") {
		t.Errorf("expected last failure to surface: %q isErr=%v", result, isErr)
	}
	if inner.calls != 2 {
		t.Errorf("expected exactly 2 calls (initial + 1 retry), got %d", inner.calls)
	}
}

// TestRetryDecorator_RespectsContextCancellation — if the caller cancels
// the context during our backoff sleep, we return ctx.Err() promptly,
// NOT after the backoff completes. Bounded-compute invariant.
func TestRetryDecorator_RespectsContextCancellation(t *testing.T) {
	inner := &fakeDispatcher{queue: []dispatchOutcome{
		{result: "context deadline exceeded", isErr: true, err: nil},
	}}
	rd := &RetryDecorator{
		Inner: inner, Eligible: DefaultEligibleTransient,
		Backoff: 10 * time.Second, // would be a noticeable delay if we waited
		MaxRetries: 1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, _, err := rd.Dispatch(ctx, model.ToolCall{Name: "web_navigate"})
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected ctx.Canceled, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("decorator waited for backoff after ctx cancel; elapsed=%v", elapsed)
	}
}

// TestRetryDecorator_NoRetryOnCtxCanceledFromInner — if the inner call
// returns ctx.Canceled (caller already gave up), we MUST NOT retry —
// that would defeat the cancellation.
func TestRetryDecorator_NoRetryOnCtxCanceledFromInner(t *testing.T) {
	inner := &fakeDispatcher{queue: []dispatchOutcome{
		{err: context.Canceled},
		{result: "would not get here", isErr: false, err: nil},
	}}
	rd := &RetryDecorator{
		Inner: inner, Eligible: DefaultEligibleTransient,
		Backoff: time.Millisecond, MaxRetries: 1,
	}
	_, _, err := rd.Dispatch(context.Background(), model.ToolCall{Name: "web_navigate"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected ctx.Canceled to pass through, got %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("expected NO retry on canceled err (calls=%d)", inner.calls)
	}
}

// TestRetryDecorator_OptOutPaths — zero-value decorator and nil
// Eligible must be safe passthroughs. The decorator must NEVER change
// behavior unless explicitly configured for retry.
func TestRetryDecorator_OptOutPaths(t *testing.T) {
	t.Run("nil Eligible passes through", func(t *testing.T) {
		inner := &fakeDispatcher{queue: []dispatchOutcome{
			{result: "context deadline exceeded", isErr: true, err: nil},
		}}
		rd := &RetryDecorator{Inner: inner, MaxRetries: 5}
		_, isErr, _ := rd.Dispatch(context.Background(), model.ToolCall{Name: "web_navigate"})
		if !isErr || inner.calls != 1 {
			t.Errorf("nil Eligible should be passthrough; calls=%d isErr=%v", inner.calls, isErr)
		}
	})
	t.Run("MaxRetries=0 passes through", func(t *testing.T) {
		inner := &fakeDispatcher{queue: []dispatchOutcome{
			{result: "context deadline exceeded", isErr: true, err: nil},
		}}
		rd := &RetryDecorator{Inner: inner, Eligible: DefaultEligibleTransient, MaxRetries: 0}
		_, isErr, _ := rd.Dispatch(context.Background(), model.ToolCall{Name: "web_navigate"})
		if !isErr || inner.calls != 1 {
			t.Errorf("MaxRetries=0 should be passthrough; calls=%d isErr=%v", inner.calls, isErr)
		}
	})
	t.Run("nil Inner is defensive error not panic", func(t *testing.T) {
		rd := &RetryDecorator{Eligible: DefaultEligibleTransient, MaxRetries: 1}
		_, _, err := rd.Dispatch(context.Background(), model.ToolCall{Name: "x"})
		if err == nil {
			t.Error("nil Inner should error, not panic or silent-success")
		}
	})
}

// TestDefaultEligibleTransient_Coverage — table-driven coverage of the
// policy. Each row encodes one row of the decision matrix; failures
// here mean a class of error is being mis-classified.
func TestDefaultEligibleTransient_Coverage(t *testing.T) {
	cases := []struct {
		name, tool, result string
		isErr              bool
		err                error
		want               bool
		why                string
	}{
		// Mutating tools — always false regardless of error shape.
		{"mutating + transient string", "sql_exec", "database is locked", true, nil, false, "mutating must never retry"},
		{"mutating + typed err", "x_post", "", false, context.DeadlineExceeded, false, "mutating must never retry"},
		// Typed err path.
		{"deadline exceeded typed", "web_navigate", "", false, context.DeadlineExceeded, true, "transient timeout"},
		{"canceled typed", "web_navigate", "", false, context.Canceled, false, "caller asked us to stop"},
		{"other typed err", "web_navigate", "", false, errors.New("EPERM"), false, "unknown err — model can see it"},
		// String-wrapped transient set.
		{"deadline string", "web_navigate", "navigation failed: context deadline exceeded", true, nil, true, "transient wrapped"},
		{"refused string", "web_navigate", "navigation failed: connection refused", true, nil, true, "transient wrapped"},
		{"sqlite busy string", "sql_query", "query error: database is locked", true, nil, true, "SQLITE_BUSY shape"},
		{"reset string", "web_navigate", "navigation failed: connection reset by peer", true, nil, true, "transient wrapped"},
		// Non-transient string failures — model can act on these.
		{"schema error", "sql_query", "query error: no such column foo", true, nil, false, "schema mistake; model's problem"},
		{"bad locator", "web_navigate", "no clickable element matched", true, nil, false, "agent mistake; model's problem"},
		// Edge cases.
		{"success - never eligible", "web_navigate", "page text", false, nil, false, "success must not retry"},
		{"empty everything", "web_navigate", "", false, nil, false, "no signal → no retry"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DefaultEligibleTransient(c.tool, c.result, c.isErr, c.err)
			if got != c.want {
				t.Errorf("%s\n  want %v, got %v\n  reasoning: %s", c.name, c.want, got, c.why)
			}
		})
	}
}
