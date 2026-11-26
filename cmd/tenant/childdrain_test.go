package main

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// Before a child drops its link for an update, the calls it is running get to
// finish (TEN-552): childDrain returns when none is left, and no later than
// its limit or the end of the link.
func TestChildDrainWaitsForRunningCalls(t *testing.T) {
	defer func(d time.Duration) { childUpdateDrain = d }(childUpdateDrain)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var n atomic.Int32
	running := func() int { return int(n.Load()) }
	took := func(ctx context.Context) time.Duration {
		start := time.Now()
		childDrain(ctx, running, log)
		return time.Since(start)
	}

	childUpdateDrain = time.Minute
	if d := took(context.Background()); d > 50*time.Millisecond {
		t.Errorf("with no call running it waited %s", d)
	}

	n.Store(2)
	time.AfterFunc(300*time.Millisecond, func() { n.Store(1) })
	time.AfterFunc(600*time.Millisecond, func() { n.Store(0) })
	if d := took(context.Background()); d < 600*time.Millisecond || d > 5*time.Second {
		t.Errorf("with calls that ended after 0.6 s it waited %s", d)
	}

	n.Store(1) // a call that does not end
	childUpdateDrain = 300 * time.Millisecond
	if d := took(context.Background()); d < 300*time.Millisecond || d > 5*time.Second {
		t.Errorf("with a limit of 0.3 s it waited %s", d)
	}

	childUpdateDrain = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	if d := took(ctx); d > 5*time.Second {
		t.Errorf("after the link ended it still waited %s", d)
	}
}
