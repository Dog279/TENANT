// Package transport abstracts the bidirectional byte channel over
// which MCP frames flow. The session layer is transport-agnostic;
// stdio, streamable HTTP, and WebSocket all satisfy this
// interface.
//
// # WebSocket liveness
//
// A WebSocket link (WS) is held open for days with nothing to say for
// most of them, so it must notice by itself that the other end is gone.
// That is the transport's job, done with WebSocket ping and pong frames
// inside Recv, and not the job of a JSON-RPC ping sent by the layer
// above. The rule, the same on both ends:
//
//   - Any byte from the peer proves it alive.
//   - After one ping interval with no byte, Recv sends a ping (an empty
//     control frame: two bytes, six from the dialing side) and goes on
//     reading. The peer's Recv answers it with a pong.
//   - If nothing has arrived by the end of the second interval, Recv
//     fails with an error wrapping os.ErrDeadlineExceeded and the
//     connection is closed. A peer that has gone silent, for whatever
//     reason, is therefore gone within two intervals: one minute at the
//     default of thirty seconds.
//
// The deadline is a read deadline on the connection, set before every
// read, so the check cannot be forgotten or outlived, and it does not
// depend on the peer doing anything. Each end pings on its own account
// and trusts only what it reads.
//
// It costs no timer and no goroutine. The one goroutine blocked in Recv
// is woken by its own read deadline, writes the ping, and goes back to
// reading. To keep two idle ends from both pinging, the accepting side
// waits a quarter of an interval longer than the dialing side before it
// pings (and that much less afterwards, so its limit is still two
// intervals). On a quiet link the dialing side's ping always arrives
// first: per interval the dialing side writes one ping and the accepting
// side one pong, and that is all the traffic there is.
//
// Writes have the mirror-image rule: a write that makes no progress for
// the write timeout ends the link, so a peer that stops reading cannot
// hold a sender. The reader itself never waits for a sender. A ping or a
// pong that falls due while a Send is in flight is skipped: the frame
// being sent is itself proof of life to the peer, and that Send's write
// deadline is watching the link meanwhile.
//
// A JSON-RPC ping remains useful to the layer above as a question to the
// peer's session ("are you answering requests"), but nothing depends on
// it being sent.
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
