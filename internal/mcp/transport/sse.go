package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// SSE is the HTTP Server-Sent-Events MCP server transport — the classic
// two-endpoint HTTP binding every pre-streamable MCP client speaks:
//
//	GET  {SSEPath}      long-lived event stream. Emits one `endpoint`
//	                    event naming the POST URL, then a `message`
//	                    event for every server→client frame.
//	POST {MessagePath}  client→server frames. Body is one JSON-RPC
//	                    message; answered 202 Accepted.
//
// One client session at a time (single-connection contract, like stdio):
// the transport's life is one GET stream. When that stream drops, Recv
// returns io.EOF — the same clean-shutdown signal stdin EOF gives — so
// Server.ServeTransport ends the session normally.
//
// SSE is itself an http.Handler: mount it on a mux or hand it straight
// to an http.Server. Serve the transport via Server.ServeTransport.
type SSE struct {
	ssePath     string
	messagePath string
	maxSize     int

	in      chan []byte   // POST bodies → Recv
	out     chan []byte   // Send → SSE stream
	done    chan struct{} // closed by Close
	sessEnd chan struct{} // closed when the live GET stream drops

	connected   atomic.Bool
	closed      atomic.Bool
	closeOnce   sync.Once
	sessEndOnce sync.Once
}

// SSEConfig configures an SSE transport. Zero value yields sensible
// defaults (/sse + /message, DefaultMaxFrameSize).
type SSEConfig struct {
	// SSEPath is the GET stream route. Default "/sse".
	SSEPath string
	// MessagePath is the POST route advertised in the endpoint event.
	// Default "/message".
	MessagePath string
	// MaxFrameSize caps an inbound POST body. Default DefaultMaxFrameSize.
	MaxFrameSize int
}

// NewSSE constructs an SSE transport. It does not start an HTTP server —
// the caller mounts it (it is an http.Handler) and runs ServeTransport.
func NewSSE(cfg SSEConfig) *SSE {
	if cfg.SSEPath == "" {
		cfg.SSEPath = "/sse"
	}
	if cfg.MessagePath == "" {
		cfg.MessagePath = "/message"
	}
	if cfg.MaxFrameSize == 0 {
		cfg.MaxFrameSize = DefaultMaxFrameSize
	}
	return &SSE{
		ssePath:     cfg.SSEPath,
		messagePath: cfg.MessagePath,
		maxSize:     cfg.MaxFrameSize,
		in:          make(chan []byte, 16),
		out:         make(chan []byte, 64),
		done:        make(chan struct{}),
		sessEnd:     make(chan struct{}),
	}
}

// ServeHTTP routes the two MCP HTTP endpoints.
func (s *SSE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == s.ssePath:
		s.handleSSE(w, r)
	case r.Method == http.MethodPost && r.URL.Path == s.messagePath:
		s.handleMessage(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleSSE owns the single server→client event stream.
func (s *SSE) handleSSE(w http.ResponseWriter, r *http.Request) {
	if s.closed.Load() {
		http.Error(w, "transport closed", http.StatusGone)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// Single-connection contract: refuse a second concurrent stream.
	if !s.connected.CompareAndSwap(false, true) {
		http.Error(w, "session already active", http.StatusConflict)
		return
	}
	defer func() {
		s.connected.Store(false)
		// The live stream dropping is this transport's EOF.
		s.sessEndOnce.Do(func() { close(s.sessEnd) })
	}()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// First event names the POST endpoint the client must use.
	fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", s.messagePath)
	flusher.Flush()

	for {
		select {
		case frame := <-s.out:
			// SSE data lines must not contain raw newlines; JSON-RPC
			// frames are single-line, but guard anyway by emitting the
			// frame as one data field.
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", frame)
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		}
	}
}

// handleMessage accepts one client→server frame.
func (s *SSE) handleMessage(w http.ResponseWriter, r *http.Request) {
	if s.closed.Load() {
		http.Error(w, "transport closed", http.StatusGone)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(s.maxSize)+1))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) > s.maxSize {
		http.Error(w, ErrFrameTooLarge.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	select {
	case s.in <- body:
		w.WriteHeader(http.StatusAccepted)
	case <-s.done:
		http.Error(w, "transport closed", http.StatusGone)
	case <-r.Context().Done():
	}
}

// Send queues one frame for the SSE stream. Buffered, so it does not
// block under normal load; a slow client applies backpressure.
func (s *SSE) Send(ctx context.Context, frame []byte) error {
	if s.closed.Load() {
		return ErrClosed
	}
	// Copy: the session may reuse the backing array after Send returns.
	cp := make([]byte, len(frame))
	copy(cp, frame)
	select {
	case s.out <- cp:
		return nil
	case <-s.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Recv blocks for the next inbound frame. Returns io.EOF when the live
// GET stream drops (clean client disconnect) and ErrClosed after Close.
func (s *SSE) Recv(ctx context.Context) ([]byte, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	select {
	case frame := <-s.in:
		return frame, nil
	case <-s.sessEnd:
		return nil, io.EOF
	case <-s.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close tears down the transport. Idempotent. Pending Send/Recv unblock
// with ErrClosed.
func (s *SSE) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.closeOnce.Do(func() { close(s.done) })
	return nil
}

var _ Transport = (*SSE)(nil)
