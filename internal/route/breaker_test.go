package route_test

import (
	"testing"
	"time"

	"tenant/internal/route"
)

func TestBreakerClosedOpenHalfOpenClosed(t *testing.T) {
	t.Parallel()
	b := route.NewBreaker(3, 60*time.Second)
	now := time.Unix(1000, 0)
	first, ok := b.Acquire(now)
	if !ok || b.State(now) != route.BreakerClosed {
		t.Fatal("new breaker must be closed and admit calls")
	}
	b.RecordSuccess(first)
	for i := 1; i <= 2; i++ {
		permit, ok := b.Acquire(now)
		if !ok || b.RecordFailure(permit, now.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("failure %d tripped breaker before threshold", i)
		}
		if got := b.State(now.Add(time.Duration(i) * time.Second)); got != route.BreakerClosed {
			t.Fatalf("state after failure %d = %q, want closed", i, got)
		}
	}
	opened := now.Add(3 * time.Second)
	permit, ok := b.Acquire(now)
	if !ok || !b.RecordFailure(permit, opened) {
		t.Fatal("third consecutive failure did not trip breaker")
	}
	if got := b.State(opened); got != route.BreakerOpen {
		t.Fatalf("state = %q, want open", got)
	}
	if _, ok := b.Acquire(opened.Add(60*time.Second - time.Nanosecond)); ok {
		t.Fatal("breaker admitted a call before the exact 60s boundary")
	}
	halfAt := opened.Add(60 * time.Second)
	if got := b.State(halfAt); got != route.BreakerHalfOpen {
		t.Fatalf("state at cooldown boundary = %q, want half-open", got)
	}
	probe, ok := b.Acquire(halfAt)
	if !ok {
		t.Fatal("first half-open probe was not admitted")
	}
	if _, ok := b.Acquire(halfAt); ok {
		t.Fatal("second concurrent half-open probe was admitted")
	}
	b.RecordSuccess(probe)
	if got := b.State(halfAt); got != route.BreakerClosed {
		t.Fatalf("successful probe did not close breaker: state=%q", got)
	}
	if got := b.Trips(); got != 1 {
		t.Fatalf("trips = %d, want 1", got)
	}
}

func TestBreakerFailedHalfOpenProbeReopens(t *testing.T) {
	t.Parallel()
	b := route.NewBreaker(3, time.Minute)
	now := time.Unix(2000, 0)
	for range 3 {
		permit, ok := b.Acquire(now)
		if !ok {
			t.Fatal("closed breaker rejected setup call")
		}
		b.RecordFailure(permit, now)
	}
	probeAt := now.Add(time.Minute)
	probe, ok := b.Acquire(probeAt)
	if !ok {
		t.Fatal("half-open probe was not admitted")
	}
	if !b.RecordFailure(probe, probeAt.Add(time.Second)) {
		t.Fatal("failed half-open probe did not count as a breaker trip")
	}
	if got := b.State(probeAt.Add(time.Second)); got != route.BreakerOpen {
		t.Fatalf("state = %q, want reopened", got)
	}
	if got := b.Trips(); got != 2 {
		t.Fatalf("trips = %d, want 2", got)
	}
	if _, ok := b.Acquire(probeAt.Add(59 * time.Second)); ok {
		t.Fatal("reopened breaker used the original cooldown timestamp")
	}
}

func TestBreakerThresholdIsConsecutive(t *testing.T) {
	t.Parallel()
	b := route.NewBreaker(3, time.Minute)
	now := time.Unix(3000, 0)
	fail := func() bool {
		permit, ok := b.Acquire(now)
		return ok && b.RecordFailure(permit, now)
	}
	fail()
	fail()
	permit, ok := b.Acquire(now)
	if !ok {
		t.Fatal("closed breaker rejected success")
	}
	b.RecordSuccess(permit)
	if fail() || fail() {
		t.Fatal("two failures after a success tripped the breaker")
	}
	if got := b.State(now); got != route.BreakerClosed {
		t.Fatalf("state = %q, want closed", got)
	}
	if !fail() {
		t.Fatal("third consecutive failure did not trip")
	}
}

func TestBreakerIgnoresStaleCompletionsAcrossTrip(t *testing.T) {
	t.Parallel()
	b := route.NewBreaker(3, time.Minute)
	now := time.Unix(4000, 0)
	staleSuccess, _ := b.Acquire(now)
	staleFailure, _ := b.Acquire(now)
	for range 3 {
		permit, _ := b.Acquire(now)
		b.RecordFailure(permit, now)
	}
	b.RecordSuccess(staleSuccess)
	if got := b.State(now); got != route.BreakerOpen {
		t.Fatalf("stale success changed open state to %q", got)
	}
	probeAt := now.Add(time.Minute)
	probe, ok := b.Acquire(probeAt)
	if !ok {
		t.Fatal("half-open probe not admitted")
	}
	if b.RecordFailure(staleFailure, probeAt) {
		t.Fatal("stale failure was mistaken for half-open probe")
	}
	if got := b.State(probeAt); got != route.BreakerHalfOpen {
		t.Fatalf("stale failure changed half-open state to %q", got)
	}
	b.RecordSuccess(probe)
}
