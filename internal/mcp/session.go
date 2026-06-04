package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"tenant/internal/mcp/transport"
)

// ErrSessionClosed is returned by Call and Notify after Close.
var ErrSessionClosed = errors.New("mcp: session closed")

// Handler dispatches inbound requests and notifications. The session
// invokes one goroutine per inbound message, so implementations MUST
// be safe for concurrent use. Long-running handlers should respect
// ctx cancellation — the session cancels ctx when shutting down.
type Handler interface {
	HandleRequest(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error)
	HandleNotification(ctx context.Context, method string, params json.RawMessage)
}

// nopHandler is the default when Session.Handler is unset. Requests
// fail with method-not-found; notifications are silently dropped.
type nopHandler struct{}

func (nopHandler) HandleRequest(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error) {
	return nil, NewError(ErrMethodNotFound, "no handler installed for "+method)
}
func (nopHandler) HandleNotification(_ context.Context, _ string, _ json.RawMessage) {}

// Session is a long-lived full-duplex MCP conversation over a
// single Transport. It owns the read pump, response routing table,
// and outbound serialization. Both Client and Server embed a
// Session; direction is just a matter of who calls who.
type Session struct {
	t       transport.Transport
	handler Handler

	nextID  atomic.Int64
	pending sync.Map // string(id raw) -> chan *Message

	// inflight tracks cancel funcs for inbound requests we are
	// currently handling, keyed by string(request id raw), so an
	// incoming notifications/cancelled can stop the right handler.
	inflight sync.Map // string(id raw) -> context.CancelFunc
	// progressSinks routes incoming notifications/progress to the
	// CallWithProgress caller, keyed by string(progressToken raw).
	progressSinks sync.Map // string(token raw) -> func(ProgressParams)

	bufPool sync.Pool

	closeOnce sync.Once
	closeCh   chan struct{}
	closeErr  error

	pumpCancel context.CancelFunc
}

// NewSession wraps t with a routing session. Handler is optional —
// useful for pure-client sessions that never receive requests.
func NewSession(t transport.Transport, handler Handler) *Session {
	if handler == nil {
		handler = nopHandler{}
	}
	return &Session{
		t:       t,
		handler: handler,
		closeCh: make(chan struct{}),
		bufPool: sync.Pool{New: func() any { return new(bytes.Buffer) }},
	}
}

// Start launches the read pump. Returns immediately. The pump runs
// until the transport closes, Close is called, or ctx is cancelled.
func (s *Session) Start(ctx context.Context) {
	pumpCtx, cancel := context.WithCancel(ctx)
	s.pumpCancel = cancel
	go s.readPump(pumpCtx)
}

// Done returns a channel closed when the session shuts down.
// Read s.Err() afterward to learn why.
func (s *Session) Done() <-chan struct{} { return s.closeCh }

// Err returns the cause of session shutdown, or nil if still running.
func (s *Session) Err() error {
	select {
	case <-s.closeCh:
		return s.closeErr
	default:
		return nil
	}
}

// Call sends a request and waits for the matching response. The
// returned bytes are the raw JSON result — caller unmarshals into
// the type they expect. Use CallTyped for typed convenience.
func (s *Session) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := NewIntID(s.nextID.Add(1))
	key := string(id.Raw())
	ch := make(chan *Message, 1)
	s.pending.Store(key, ch)
	defer s.pending.Delete(key)

	if err := s.send(ctx, &Message{ID: &id, Method: method, Params: params}); err != nil {
		return nil, fmt.Errorf("mcp: send %s: %w", method, err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		// Tell the peer to stop working on this request (MCP cancel
		// pattern). Best-effort + fire-and-forget — the caller's ctx is
		// already done, so use a background ctx for the notify itself.
		s.sendCancelled(id.Raw(), ctx.Err())
		return nil, ctx.Err()
	case <-s.closeCh:
		return nil, s.closeErr
	}
}

// CallWithProgress is Call plus interim progress: it tags the request
// with a progressToken and invokes onProgress for each
// notifications/progress the peer sends for this call. onProgress MUST
// be cheap and non-blocking — it runs on the read pump. The token is
// deregistered when the call returns.
func (s *Session) CallWithProgress(ctx context.Context, method string, params json.RawMessage,
	onProgress func(ProgressParams)) (json.RawMessage, error) {
	if onProgress == nil {
		return s.Call(ctx, method, params)
	}
	token := NewIntID(s.nextID.Add(1)).Raw()
	s.progressSinks.Store(string(token), onProgress)
	defer s.progressSinks.Delete(string(token))

	tagged, err := withProgressToken(params, token)
	if err != nil {
		return nil, err
	}
	return s.Call(ctx, method, tagged)
}

// SendProgress emits a notifications/progress for an in-flight request.
// A request handler reads the token via ProgressTokenFrom(params) and
// reports progress with this. Fire-and-forget (a dropped progress
// notification is non-fatal).
func (s *Session) SendProgress(ctx context.Context, token json.RawMessage, progress float64, total *float64, message string) error {
	b, err := json.Marshal(ProgressParams{ProgressToken: token, Progress: progress, Total: total, Message: message})
	if err != nil {
		return err
	}
	return s.Notify(ctx, MethodProgress, b)
}

func (s *Session) sendCancelled(requestID json.RawMessage, cause error) {
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	b, err := json.Marshal(CancelledParams{RequestID: requestID, Reason: reason})
	if err != nil {
		return
	}
	// Background ctx: the originating call's ctx is already cancelled.
	_ = s.Notify(context.Background(), MethodCancelled, b)
}

// Notify fires a notification (no response). Returns the underlying
// transport error on failure.
func (s *Session) Notify(ctx context.Context, method string, params json.RawMessage) error {
	return s.send(ctx, &Message{Method: method, Params: params})
}

// Close terminates the session. Idempotent. In-flight Calls return
// ErrSessionClosed.
func (s *Session) Close() error {
	s.shutdown(ErrSessionClosed)
	return s.t.Close()
}

// CallTyped marshals params, calls method, and unmarshals the result
// into Resp. Convenience over the raw byte interface.
func CallTyped[Resp any](ctx context.Context, s *Session, method string, params any) (Resp, error) {
	var zero Resp
	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return zero, fmt.Errorf("mcp: marshal params for %s: %w", method, err)
		}
		paramsRaw = b
	}
	respRaw, err := s.Call(ctx, method, paramsRaw)
	if err != nil {
		return zero, err
	}
	if len(respRaw) == 0 {
		return zero, nil
	}
	var resp Resp
	if err := json.Unmarshal(respRaw, &resp); err != nil {
		return zero, fmt.Errorf("mcp: unmarshal result of %s: %w", method, err)
	}
	return resp, nil
}

// --- internal ---

func (s *Session) send(ctx context.Context, msg *Message) error {
	msg.Jsonrpc = jsonrpcVersion
	buf := s.bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer s.bufPool.Put(buf)

	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false) // MCP carries JSON, not HTML; skip the escape pass
	if err := enc.Encode(msg); err != nil {
		return err
	}
	// json.Encoder appends a trailing newline; let the transport own framing.
	return s.t.Send(ctx, bytes.TrimRight(buf.Bytes(), "\n"))
}

func (s *Session) readPump(ctx context.Context) {
	defer s.Close()
	for {
		if ctx.Err() != nil {
			return
		}
		raw, err := s.t.Recv(ctx)
		if err != nil {
			if err == io.EOF || errors.Is(err, transport.ErrClosed) {
				s.shutdown(ErrSessionClosed)
			} else {
				s.shutdown(fmt.Errorf("mcp: read: %w", err))
			}
			return
		}
		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			// Malformed frame — log + continue. Killing the session for
			// one bad frame would be a denial-of-service for a flaky peer.
			// TODO(observability): emit metric mcp_malformed_frames_total
			continue
		}
		s.dispatch(ctx, &msg)
	}
}

func (s *Session) dispatch(ctx context.Context, msg *Message) {
	switch {
	case msg.IsResponse():
		s.routeResponse(msg)
	case msg.IsRequest():
		go s.handleRequest(ctx, msg)
	case msg.IsNotification():
		// Protocol-level notifications are consumed here, not forwarded
		// to the application handler.
		switch msg.Method {
		case MethodCancelled:
			s.handleCancelled(msg.Params)
		case MethodProgress:
			s.handleProgress(msg.Params)
		default:
			go s.handler.HandleNotification(ctx, msg.Method, msg.Params)
		}
	default:
		// Frame that is neither — peer bug. Drop silently.
	}
}

// handleCancelled cancels the in-flight handler for the named request,
// if we are still working on it.
func (s *Session) handleCancelled(params json.RawMessage) {
	var p CancelledParams
	if json.Unmarshal(params, &p) != nil || len(p.RequestID) == 0 {
		return
	}
	if v, ok := s.inflight.Load(string(p.RequestID)); ok {
		v.(context.CancelFunc)()
	}
}

// handleProgress routes a progress notification to the matching
// CallWithProgress sink. Runs on the read pump — sinks must be cheap.
func (s *Session) handleProgress(params json.RawMessage) {
	var p ProgressParams
	if json.Unmarshal(params, &p) != nil || len(p.ProgressToken) == 0 {
		return
	}
	if v, ok := s.progressSinks.Load(string(p.ProgressToken)); ok {
		v.(func(ProgressParams))(p)
	}
}

func (s *Session) routeResponse(msg *Message) {
	key := string(msg.ID.Raw())
	v, ok := s.pending.LoadAndDelete(key)
	if !ok {
		return // unmatched response — caller already cancelled
	}
	select {
	case v.(chan *Message) <- msg:
	default:
		// caller cancelled between LoadAndDelete and select; drop
	}
}

func (s *Session) handleRequest(ctx context.Context, msg *Message) {
	// Per-request cancellable ctx so an inbound notifications/cancelled
	// can stop this handler. Keyed by the request id the peer will name.
	reqCtx, cancel := context.WithCancel(ctx)
	key := string(msg.ID.Raw())
	s.inflight.Store(key, cancel)
	defer func() {
		s.inflight.Delete(key)
		cancel()
	}()

	result, err := s.handler.HandleRequest(reqCtx, msg.Method, msg.Params)
	resp := &Message{ID: msg.ID}
	if err != nil {
		var rpcErr *Error
		if errors.As(err, &rpcErr) {
			resp.Error = rpcErr
		} else {
			resp.Error = NewError(ErrInternalError, err.Error())
		}
	} else {
		resp.Result = result
	}
	// If the request was cancelled (peer sent notifications/cancelled),
	// the MCP cancel pattern says don't send a response — the caller has
	// moved on. Send with the parent ctx so a legit completion isn't
	// aborted by the per-request cancel.
	if reqCtx.Err() != nil {
		return
	}
	_ = s.send(ctx, resp)
}

// withProgressToken injects `_meta.progressToken` into an object
// params payload (creating the object if params is empty). Errors if
// params is present but not a JSON object.
func withProgressToken(params, token json.RawMessage) (json.RawMessage, error) {
	obj := map[string]json.RawMessage{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &obj); err != nil {
			return nil, fmt.Errorf("mcp: progress requires object params: %w", err)
		}
	}
	meta, _ := json.Marshal(map[string]json.RawMessage{"progressToken": token})
	obj["_meta"] = meta
	return json.Marshal(obj)
}

func (s *Session) shutdown(cause error) {
	s.closeOnce.Do(func() {
		s.closeErr = cause
		close(s.closeCh)
		if s.pumpCancel != nil {
			s.pumpCancel()
		}
	})
}
