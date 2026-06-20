package mcp_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/mcp"
	"tenant/internal/mcp/transport"
)

// deafTransport accepts every Send but never produces an inbound
// frame: Recv blocks until Close. It models a peer that opens the
// connection but never answers the handshake — the exact hang
// Initialize must defend against.
type deafTransport struct {
	closeOnce sync.Once
	closed    chan struct{}
}

func newDeafTransport() *deafTransport {
	return &deafTransport{closed: make(chan struct{})}
}

func (d *deafTransport) Send(ctx context.Context, _ []byte) error {
	select {
	case <-d.closed:
		return transport.ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil // swallow — peer never replies
	}
}

func (d *deafTransport) Recv(ctx context.Context) ([]byte, error) {
	select {
	case <-d.closed:
		return nil, transport.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *deafTransport) Close() error {
	d.closeOnce.Do(func() { close(d.closed) })
	return nil
}

// TestInitialize_DefaultTimeout: a peer that never replies must make
// Initialize fail fast with a clear timeout error rather than hang,
// even when the caller's ctx carries no deadline.
func TestInitialize_DefaultTimeout(t *testing.T) {
	prev := mcp.DefaultInitializeTimeout
	mcp.DefaultInitializeTimeout = 100 * time.Millisecond
	defer func() { mcp.DefaultInitializeTimeout = prev }()

	tr := newDeafTransport()
	client := mcp.NewClient(tr)
	client.Start(context.Background())
	defer client.Close()

	done := make(chan error, 1)
	go func() {
		// Deadline-less ctx: the default timeout is what must fire.
		_, err := client.Initialize(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected timeout error, got nil")
		}
		if !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "peer unresponsive") {
			t.Fatalf("expected clear timeout error, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Initialize hung past the default timeout window")
	}
}

// TestInitialize_RespectsCallerDeadline: when the caller already
// bounds ctx, that deadline is honored (and not silently widened to
// the default), surfacing as a context error from the round-trip.
func TestInitialize_RespectsCallerDeadline(t *testing.T) {
	prev := mcp.DefaultInitializeTimeout
	mcp.DefaultInitializeTimeout = 10 * time.Second // long, to prove the caller deadline wins
	defer func() { mcp.DefaultInitializeTimeout = prev }()

	tr := newDeafTransport()
	client := mcp.NewClient(tr)
	client.Start(context.Background())
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.Initialize(ctx)
	if err == nil {
		t.Fatal("expected deadline error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("caller deadline not honored — took %s (default would be 10s)", elapsed)
	}
}
