package transport

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync"
	"sync/atomic"
)

// sessionParam is the query parameter of the endpoint URL that carries the
// stream's session id, the name MCP's SSE clients already expect.
const sessionParam = "sessionId"

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
// Two checks keep a message tied to the client that opened the stream:
//
//   - the endpoint event names the POST URL with a random session id
//     ({MessagePath}?sessionId=…), and a POST without that id is refused, so
//     only whoever read the stream can send into its session;
//   - a POST must be application/json. A web page can send a cross-origin
//     text/plain POST with no questions asked; application/json makes the
//     browser ask first (a preflight), and this transport never says yes.
//
// Neither is authentication: any client that can reach the listener can open
// a stream of its own. Whoever mounts the transport decides who can reach it.
//
// SSE is itself an http.Handler: mount it on a mux or hand it straight
// to an http.Server. Serve the transport via Server.ServeTransport.
type SSE struct {
	ssePath     string
	messagePath string
	session     string // this stream's id, required on every POST
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
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		// crypto/rand does not fail on a supported platform; a guessable id
		// is worse than no transport.
		panic("transport: crypto/rand failed: " + err.Error())
	}
	return &SSE{
		ssePath:     cfg.SSEPath,
		messagePath: cfg.MessagePath,
		session:     hex.EncodeToString(id),
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

	// First event names the POST endpoint the client must use, with this
	// stream's session id: the client sends it back on every POST.
	fmt.Fprintf(w, "event: endpoint\ndata: %s?%s=%s\n\n", s.messagePath, sessionParam, s.session)
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
	if got := r.URL.Query().Get(sessionParam); subtle.ConstantTimeCompare([]byte(got), []byte(s.session)) != 1 {
		http.Error(w, "unknown session: POST to the endpoint the stream's first event named", http.StatusForbidden)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.maxSize))
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, ErrFrameTooLarge.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read error", http.StatusBadRequest)
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
