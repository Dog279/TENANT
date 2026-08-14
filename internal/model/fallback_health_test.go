package model

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TEN-282 health-gating tests. These reuse the package's scriptLLM fake (see
// fallback_test.go) but drive a controllable clock so a link can simulate being
// SLOW without any real wall-clock sleeps. The clock is the same one injected
// into FallbackLLM.now, so the latency the gate measures == clk advance during
// the wrapped call.

// fakeClock is a monotonic, manually-advanced clock shared between the test's
// FallbackLLM (via .now) and the latency-injecting link wrapper.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_000_000, 0)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// slowLink wraps a scriptLLM so that each Generate / GenerateStream call advances
// the shared clock by `latency` (simulating the call taking that long). The
// advance happens AFTER the inner call returns but the gate reads f.now() before
// and after the inner call, so the measured duration == latency.
type slowLink struct {
	inner   *scriptLLM
	clk     *fakeClock
	latency func() time.Duration // evaluated per call so a link can "recover"
}

func (s *slowLink) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	resp, err := s.inner.Generate(ctx, req)
	s.clk.advance(s.latency())
	return resp, err
}

func (s *slowLink) GenerateStream(ctx context.Context, req GenerateRequest) (<-chan StreamChunk, error) {
	ch, err := s.inner.GenerateStream(ctx, req)
	if err != nil {
		s.clk.advance(s.latency())
		return ch, err
	}
	// Re-emit the inner chunks, advancing the clock as the LAST chunk passes so
	// the full stream "took" latency from open to close.
	out := make(chan StreamChunk, cap(ch)+1)
	go func() {
		defer close(out)
		for c := range ch {
			out <- c
		}
		s.clk.advance(s.latency())
	}()
	return out, nil
}

func (s *slowLink) TokenCount(ctx context.Context, text string) (int, error) {
	return s.inner.TokenCount(ctx, text)
}

// newHealthFB builds a FallbackLLM over latency-injecting links sharing one
// clock, with health-gating configured, and returns the chain + captured events
// + the underlying scriptLLMs (for call-count assertions) + the clock.
func newHealthFB(t *testing.T, cfg HealthConfig, links ...*slowLink) (*FallbackLLM, *[]FailoverEvent) {
	t.Helper()
	var (
		mu     sync.Mutex
		events []FailoverEvent
	)
	fl := make([]fbLink, len(links))
	for i, l := range links {
		fl[i] = fbLink{llm: l, label: l.inner.name}
	}
	fb := NewFallbackLLM(fl, func(ev FailoverEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	if len(links) > 0 {
		fb.now = links[0].clk.now
	}
	fb.SetHealthGating(cfg)
	return fb, &events
}

// (a) A slow-but-succeeding link is proactively cooled down and the chain then
// prefers the next (fast) link.
func TestHealth_SlowLinkDemotedChainPrefersNext(t *testing.T) {
	clk := newClock()
	primary := &slowLink{
		inner:   &scriptLLM{name: "slow", genText: "slow-ok"},
		clk:     clk,
		latency: func() time.Duration { return 2 * time.Second }, // above threshold
	}
	backup := &slowLink{
		inner:   &scriptLLM{name: "fast", genText: "fast-ok"},
		clk:     clk,
		latency: func() time.Duration { return 50 * time.Millisecond },
	}
	cfg := HealthConfig{
		SlowThreshold: time.Second,
		MinSamples:    3,
		Alpha:         1.0, // no smoothing → EWMA == latest, trips as soon as MinSamples met
		Cooldown:      30 * time.Second,
	}
	fb, events := newHealthFB(t, cfg, primary, backup)

	// Serve MinSamples successful calls on the slow primary so its EWMA is armed.
	for i := 0; i < cfg.MinSamples; i++ {
		resp, err := fb.Generate(context.Background(), GenerateRequest{})
		if err != nil || resp.Text != "slow-ok" {
			t.Fatalf("call %d: expected slow primary to serve; resp=%+v err=%v", i, resp, err)
		}
	}
	// The MinSamples-th success crosses the threshold → primary cooled down.
	if primary.inner.calls != cfg.MinSamples {
		t.Fatalf("primary should have served all %d warmup calls; got %d", cfg.MinSamples, primary.inner.calls)
	}
	// Next call: primary is cooling → chain prefers the fast backup.
	resp, err := fb.Generate(context.Background(), GenerateRequest{})
	if err != nil || resp.Text != "fast-ok" {
		t.Fatalf("after demotion the chain should prefer the fast backup; resp=%+v err=%v", resp, err)
	}
	if primary.inner.calls != cfg.MinSamples {
		t.Errorf("cooled slow primary must be SKIPPED, not retried; primary.calls=%d (want %d)", primary.inner.calls, cfg.MinSamples)
	}
	if backup.inner.calls != 1 {
		t.Errorf("fast backup should have served exactly the post-demotion call; backup.calls=%d", backup.inner.calls)
	}
	// A synthetic "slow" demotion event should have been emitted.
	var sawSlow bool
	for _, ev := range *events {
		if ev.From == "slow" && ev.Err == nil {
			sawSlow = true
		}
	}
	if !sawSlow {
		t.Errorf("expected a slow-demotion event for the primary; got %+v", *events)
	}
}

// (b) A demoted link that has recovered (fast again) is restored after the
// cooldown lapses — never permanently demoted.
func TestHealth_RecoveredLinkRestoredAfterCooldown(t *testing.T) {
	clk := newClock()
	slow := time.Duration(2 * time.Second)
	primaryLatency := slow
	primary := &slowLink{
		inner:   &scriptLLM{name: "p", genText: "p-ok"},
		clk:     clk,
		latency: func() time.Duration { return primaryLatency },
	}
	backup := &slowLink{
		inner:   &scriptLLM{name: "b", genText: "b-ok"},
		clk:     clk,
		latency: func() time.Duration { return 10 * time.Millisecond },
	}
	cfg := HealthConfig{SlowThreshold: time.Second, MinSamples: 3, Alpha: 1.0, Cooldown: 30 * time.Second}
	fb, _ := newHealthFB(t, cfg, primary, backup)

	for i := 0; i < cfg.MinSamples; i++ {
		fb.Generate(context.Background(), GenerateRequest{})
	}
	// Primary now cooling. Confirm the backup serves while cooling.
	if resp, _ := fb.Generate(context.Background(), GenerateRequest{}); resp.Text != "b-ok" {
		t.Fatalf("while cooling, backup should serve; got %q", resp.Text)
	}
	pCallsAtDemotion := primary.inner.calls
	// Primary recovers (fast again) and the cooldown lapses.
	primaryLatency = 10 * time.Millisecond
	clk.advance(31 * time.Second)
	resp, err := fb.Generate(context.Background(), GenerateRequest{})
	if err != nil || resp.Text != "p-ok" {
		t.Fatalf("after cooldown the recovered primary should be re-probed first; resp=%+v err=%v", resp, err)
	}
	if primary.inner.calls != pCallsAtDemotion+1 {
		t.Errorf("primary should have been retried exactly once after recovery; calls=%d want=%d", primary.inner.calls, pCallsAtDemotion+1)
	}
	// And it should NOT immediately re-cool (it's fast now): one more call still primary.
	if resp, _ := fb.Generate(context.Background(), GenerateRequest{}); resp.Text != "p-ok" {
		t.Errorf("recovered fast primary should stay preferred; got %q", resp.Text)
	}
}

// (c) A healthy FAST chain is NEVER demoted, even over many calls, and the
// default zero-value config (gating off) likewise never demotes.
func TestHealth_FastChainNeverDemoted(t *testing.T) {
	run := func(t *testing.T, cfg HealthConfig, enabled bool) {
		clk := newClock()
		primary := &slowLink{
			inner:   &scriptLLM{name: "fast1", genText: "p"},
			clk:     clk,
			latency: func() time.Duration { return 20 * time.Millisecond }, // well under any threshold
		}
		backup := &slowLink{
			inner:   &scriptLLM{name: "fast2", genText: "b"},
			clk:     clk,
			latency: func() time.Duration { return 20 * time.Millisecond },
		}
		fb, events := newHealthFB(t, cfg, primary, backup)
		for i := 0; i < 50; i++ {
			resp, err := fb.Generate(context.Background(), GenerateRequest{})
			if err != nil || resp.Text != "p" {
				t.Fatalf("call %d: fast primary must always serve; resp=%+v err=%v", i, resp, err)
			}
		}
		if backup.inner.calls != 0 {
			t.Errorf("a healthy fast primary must never be demoted to the backup; backup.calls=%d", backup.inner.calls)
		}
		if len(*events) != 0 {
			t.Errorf("no failover/demotion events expected for a fast chain; got %+v", *events)
		}
		_ = enabled
	}
	t.Run("gating-enabled-but-fast", func(t *testing.T) {
		run(t, HealthConfig{SlowThreshold: time.Second, MinSamples: 3, Alpha: 0.5, Cooldown: 30 * time.Second}, true)
	})
	t.Run("gating-off-zero-config", func(t *testing.T) {
		run(t, HealthConfig{}, false) // default: SlowThreshold==0 ⇒ off
	})
}

// (d-fail-closed) Health-gating must not change the fail-closed invariant: a
// non-failover error (invalid request) still surfaces and does NOT fail over,
// even with gating enabled.
func TestHealth_PreservesFailClosed(t *testing.T) {
	clk := newClock()
	primary := &slowLink{
		inner:   &scriptLLM{name: "p", genErr: ErrInvalidRequest},
		clk:     clk,
		latency: func() time.Duration { return 10 * time.Millisecond },
	}
	backup := &slowLink{
		inner:   &scriptLLM{name: "b", genText: "should-not-appear"},
		clk:     clk,
		latency: func() time.Duration { return 10 * time.Millisecond },
	}
	fb, events := newHealthFB(t, HealthConfig{SlowThreshold: time.Second, MinSamples: 1, Alpha: 1, Cooldown: time.Second}, primary, backup)
	if _, err := fb.Generate(context.Background(), GenerateRequest{}); err == nil {
		t.Fatal("invalid-request must surface, not fail over (fail-closed) even with health-gating on")
	}
	if backup.inner.calls != 0 {
		t.Errorf("backup must NOT be called on a fail-closed error; calls=%d", backup.inner.calls)
	}
	if len(*events) != 0 {
		t.Errorf("no events expected on a fail-closed error; got %+v", *events)
	}
}

// (d-streaming) Health-gating must not change the streaming invariant: a slow
// stream still completes on the SAME link (no mid-stream switch after the first
// token), and a post-token error still propagates rather than failing over.
func TestHealth_PreservesStreamingInvariant(t *testing.T) {
	clk := newClock()
	// Primary streams two tokens then errors AFTER emitting — must NOT fail over.
	primary := &slowLink{
		inner:   &scriptLLM{name: "p", chunks: []StreamChunk{{Delta: "partial "}, {Error: ErrInternal}}},
		clk:     clk,
		latency: func() time.Duration { return 2 * time.Second }, // slow, but already committed
	}
	backup := &slowLink{
		inner:   &scriptLLM{name: "b", chunks: []StreamChunk{{Delta: "SHOULD NOT APPEAR"}}},
		clk:     clk,
		latency: func() time.Duration { return 10 * time.Millisecond },
	}
	fb, events := newHealthFB(t, HealthConfig{SlowThreshold: time.Second, MinSamples: 1, Alpha: 1, Cooldown: time.Second}, primary, backup)

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
	if backup.inner.calls != 0 {
		t.Errorf("must NOT fail over after a token was emitted even when slow; backup.calls=%d", backup.inner.calls)
	}
	if len(*events) != 0 {
		t.Errorf("no failover event after first token; got %+v", *events)
	}
}

// A slow STREAM (completes, emits tokens, but takes a long time) should arm the
// gate and demote on the next call — proving the latency path covers streaming,
// not just buffered Generate.
func TestHealth_SlowStreamDemotesNextCall(t *testing.T) {
	clk := newClock()
	primary := &slowLink{
		inner:   &scriptLLM{name: "p", chunks: []StreamChunk{{Delta: "ok"}}},
		clk:     clk,
		latency: func() time.Duration { return 2 * time.Second },
	}
	backup := &slowLink{
		inner:   &scriptLLM{name: "b", chunks: []StreamChunk{{Delta: "fast"}}},
		clk:     clk,
		latency: func() time.Duration { return 10 * time.Millisecond },
	}
	cfg := HealthConfig{SlowThreshold: time.Second, MinSamples: 2, Alpha: 1.0, Cooldown: 30 * time.Second}
	fb, _ := newHealthFB(t, cfg, primary, backup)

	for i := 0; i < cfg.MinSamples; i++ {
		got := drain(t, fb)
		if got != "ok" {
			t.Fatalf("warmup stream %d should come from the slow primary; got %q", i, got)
		}
	}
	// Next stream: primary cooling → backup serves.
	if got := drain(t, fb); got != "fast" {
		t.Errorf("after a slow-stream demotion the next stream should prefer the fast backup; got %q", got)
	}
}
