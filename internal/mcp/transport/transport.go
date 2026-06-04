// Package transport abstracts the bidirectional byte channel over
// which MCP frames flow. The session layer is transport-agnostic;
// stdio, streamable HTTP, and (future) WebSocket all satisfy this
// interface.
package transport

import (
	"context"
	"errors"
)

// Transport moves opaque frame bytes between peers. Each frame is
// a complete JSON-RPC message. Framing (newline-delimited, SSE
// chunked, etc.) is the transport's responsibility.
//
// Implementations MUST be safe for concurrent Send. Recv is
// single-reader by contract — the session owns the read loop.
type Transport interface {
	// Send writes one frame. Returns ErrClosed if the transport
	// has been closed. ctx cancellation should abort if practical;
	// stdio cannot abort an in-flight write so ctx is advisory.
	Send(ctx context.Context, frame []byte) error

	// Recv blocks until the next inbound frame arrives. Returns
	// io.EOF on clean peer shutdown, ErrClosed after Close, or the
	// underlying I/O error otherwise.
	Recv(ctx context.Context) ([]byte, error)

	// Close tears down the transport. Idempotent. Pending Send /
	// Recv calls unblock with ErrClosed.
	Close() error
}

var (
	// ErrClosed is returned by Send/Recv after Close.
	ErrClosed = errors.New("transport: closed")

	// ErrFrameTooLarge protects against unbounded reads from a
	// misbehaving peer.
	ErrFrameTooLarge = errors.New("transport: frame exceeds size limit")
)
