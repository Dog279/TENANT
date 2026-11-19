package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
)

// Defaults for a WS transport.
const (
	// DefaultWSPingInterval is how long a link may be quiet before the peer is
	// pinged. A peer that stays quiet for twice this long is given up on.
	DefaultWSPingInterval = 30 * time.Second
	// DefaultWSWriteTimeout is how long one write may wait on a peer that has
	// stopped reading.
	DefaultWSWriteTimeout = 10 * time.Second
)

const (
	// wsChunk is the most that goes into one write, and so the most a peer has
	// to take off the wire within one write timeout.
	wsChunk = 32 << 10
	// wsHandshakeMax is how much of a server's handshake answer the dialing
	// side reads. An honest answer is about 130 bytes.
	wsHandshakeMax = 4096
	// wsCloseGrace is how long Close waits for a frame that is on its way out
	// before it closes the connection under it.
	wsCloseGrace = time.Second
)

// aLongTimeAgo is a deadline that has passed: setting it fails a blocked read
// or write at once.
var aLongTimeAgo = time.Unix(1, 0)

// WSConfig configures a WS transport. The zero value gives the defaults.
type WSConfig struct {
	// PingInterval is the length of quiet after which the peer is pinged; a
	// peer quiet for two of them is given up on. Default DefaultWSPingInterval.
	PingInterval time.Duration
	// WriteTimeout is how long a write may make no progress before the link is
	// given up on. It is applied to each 32 KiB piece of a frame, so it measures
	// a peer that has stopped reading and not the size of the frame. Keep it at
	// or below PingInterval. Default DefaultWSWriteTimeout.
	WriteTimeout time.Duration
	// MaxFrameSize is the largest message accepted in either direction, in
	// bytes. Default DefaultMaxFrameSize.
	MaxFrameSize int
}

func (c WSConfig) withDefaults() WSConfig {
	if c.PingInterval <= 0 {
		c.PingInterval = DefaultWSPingInterval
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = DefaultWSWriteTimeout
	}
	if c.MaxFrameSize <= 0 {
		c.MaxFrameSize = DefaultMaxFrameSize
	}
	return c
}

// WS is the WebSocket transport (RFC 6455) over a connection the caller has
// already made and secured: one binary WebSocket message per frame of the
// Transport contract. It carries the fleet link, one held socket per child
// machine, so it is built to sit idle cheaply and to notice a dead peer by
// itself; the package comment has the liveness rule.
//
// What it refuses, each one ending the link:
//   - a message larger than MaxFrameSize, judged from the length a frame
//     claims before anything is allocated, and summed over fragments;
//   - text frames: the link carries opaque bytes, and a text frame would oblige
//     both ends to check every message for valid UTF-8;
//   - anything RFC 6455 forbids: an unmasked frame from the dialing side or a
//     masked one from the accepting side, reserved bits (no extension is ever
//     negotiated, so no compression), reserved opcodes, fragments out of order,
//     control frames that are fragmented or over 125 bytes.
//
// An error from Recv ends the transport: the connection is closed, later Recv
// calls return the same error and Send returns ErrClosed. The one exception is
// a Recv cut short by its context. That ends the reading, since the cut can
// land inside a frame, and later Recv calls return the context's error; but
// the connection stays open, so that Send still works and Close can still say
// goodbye. A session that cancels its read loop and then closes relies on it.
//
// The transport starts no goroutine and no timer. Everything it does on its
// own (ping, pong, the answer to a close) is done by the caller's goroutine
// inside Recv.
type WS struct {
	conn  net.Conn
	br    *bufio.Reader // over wsReader, so every read of conn passes the liveness rule
	state ws.State      // which end this is: it decides the masking in both directions

	max      int
	wtimeout time.Duration
	patience [2]time.Duration // quiet allowed before the ping, and after it

	// wlock is the write lock. It is a channel so that a sender can stop waiting
	// for it when its context ends or the transport closes, and so that the
	// reader can try it without waiting.
	wlock chan struct{}

	done   chan struct{} // closed when the transport is down, by Close or by a failure
	down   sync.Once
	cause  error       // why it went down; set before done is closed
	closed atomic.Bool // Close was called

	// Owned by the one goroutine that calls Recv.
	rctx   context.Context // the context of the Recv in progress
	rerr   error           // the context error that ended reading
	silent bool            // the peer has been quiet for one period and has been pinged
}

var _ Transport = (*WS)(nil)

// NewWSClient wraps conn as the dialing side of a WebSocket whose opening
// handshake is already done. Frames it sends are masked. Use UpgradeWSClient
// to do the handshake too.
func NewWSClient(conn net.Conn, cfg WSConfig) *WS {
	return newWS(conn, nil, ws.StateClientSide, cfg)
}

// NewWSServer wraps conn as the accepting side of a WebSocket whose opening
// handshake is already done. Use UpgradeWSServer to do the handshake too.
func NewWSServer(conn net.Conn, cfg WSConfig) *WS {
	return newWS(conn, nil, ws.StateServerSide, cfg)
}

// newWS builds the transport. early holds bytes of the peer's first frames
// that were read off conn along with its handshake answer.
func newWS(conn net.Conn, early []byte, state ws.State, cfg WSConfig) *WS {
	cfg = cfg.withDefaults()
	w := &WS{
		conn:     conn,
		state:    state,
		max:      cfg.MaxFrameSize,
		wtimeout: cfg.WriteTimeout,
		patience: [2]time.Duration{cfg.PingInterval, cfg.PingInterval},
		wlock:    make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	if state == ws.StateServerSide {
		// The accepting side waits a quarter longer before it pings and that
		// much less afterwards. On a quiet link the dialing side's ping then
		// always arrives first, and this side only ever answers.
		quarter := cfg.PingInterval / 4
		w.patience = [2]time.Duration{cfg.PingInterval + quarter, cfg.PingInterval - quarter}
	}
	var src io.Reader = wsReader{w}
	if len(early) > 0 {
		src = io.MultiReader(bytes.NewReader(early), src)
	}
	w.br = bufio.NewReaderSize(src, 4096)
	return w
}

// UpgradeWSClient does the WebSocket opening handshake as the dialing side on
// conn, which the caller has already connected and secured, and returns the
// transport. rawURL names the endpoint ("wss://host:port/path"); its host and
// path go into the request, and nothing else of it is used.
//
// The answer must arrive within two ping intervals and fit in 4096 bytes, and
// ctx cuts the wait short. No extension and no subprotocol is offered, and an
// answer that selects one is refused. If the handshake fails, conn is closed.
func UpgradeWSClient(ctx context.Context, conn net.Conn, rawURL string, cfg WSConfig) (*WS, error) {
	cfg = cfg.withDefaults()
	u, err := url.Parse(rawURL)
	if err == nil && u.Host == "" {
		err = errors.New("the URL names no host")
	}
	var br *bufio.Reader
	if err == nil {
		_ = conn.SetWriteDeadline(time.Now().Add(cfg.WriteTimeout))
		_ = conn.SetReadDeadline(time.Now().Add(2 * cfg.PingInterval))
		stop := interruptOn(ctx, conn.SetDeadline)
		// gobwas reads the answer a line at a time with no limit on the length
		// of a line. The limit is put on the reader instead.
		limited := &io.LimitedReader{R: conn, N: wsHandshakeMax}
		br, _, err = ws.Dialer{}.Upgrade(struct {
			io.Reader
			io.Writer
		}{limited, conn}, u)
		stop()
		if errors.Is(err, io.EOF) && limited.N == 0 {
			err = fmt.Errorf("the answer is longer than %d bytes", wsHandshakeMax)
		}
	}
	if err != nil {
		_ = conn.Close()
		if cerr := ctx.Err(); cerr != nil {
			err = cerr
		}
		return nil, fmt.Errorf("transport: websocket handshake: %w", err)
	}
	// The server may have sent frames right behind its answer. They were read
	// with it and belong to Recv.
	var early []byte
	if br != nil {
		buffered, _ := br.Peek(br.Buffered())
		early = bytes.Clone(buffered)
		ws.PutReader(br)
	}
	return newWS(conn, early, ws.StateClientSide, cfg), nil
}

// UpgradeWSServer does the WebSocket opening handshake as the accepting side
// of an HTTP request and returns the transport over the request's connection.
// Who may connect, and on which path, is the caller's decision, made before
// the call. No extension and no subprotocol is accepted.
//
// When the request is not a valid upgrade the client is sent an HTTP error,
// the connection is closed and an error is returned; the caller writes nothing
// more. The request must be HTTP/1.1: an HTTP/2 connection cannot be taken
// over, so a TLS listener must not offer h2.
func UpgradeWSServer(w http.ResponseWriter, r *http.Request, cfg WSConfig) (*WS, error) {
	cfg = cfg.withDefaults()
	conn, rw, _, err := ws.HTTPUpgrader{Timeout: cfg.WriteTimeout}.Upgrade(r, w)
	if err == nil && rw.Reader.Buffered() > 0 {
		// Those bytes sit in the HTTP server's buffer, not on the connection,
		// and a client has to wait for the answer before it sends a frame.
		err = errors.New("the client sent data before the handshake was answered")
	}
	if err != nil {
		if conn != nil {
			_ = conn.Close() // gobwas leaves a connection it has taken over open when it refuses
		}
		return nil, fmt.Errorf("transport: websocket handshake: %w", err)
	}
	return newWS(conn, nil, ws.StateServerSide, cfg), nil
}

// Send writes frame as one binary message. A frame over MaxFrameSize is
// refused with ErrFrameTooLarge and nothing is written.
//
// A context that ends while Send waits its turn behind another sender stops
// the wait and leaves the link as it was. One that ends while the frame is
// being written stops the write too, but a frame cannot be taken back half
// written, so that ends the link: Send returns the context's error and the
// transport is closed. The same goes for a write that makes no progress for
// WriteTimeout, which returns an error wrapping os.ErrDeadlineExceeded.
func (w *WS) Send(ctx context.Context, frame []byte) error {
	if len(frame) > w.max {
		return ErrFrameTooLarge
	}
	select {
	case <-w.done:
		return ErrClosed
	default:
	}
	select {
	case w.wlock <- struct{}{}:
	case <-w.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-w.wlock }()
	// Runs before the lock is released: no stale deadline is left for the next writer.
	defer interruptOn(ctx, w.conn.SetWriteDeadline)()

	started, err := w.writeFrame(ctx, ws.OpBinary, frame, w.wtimeout)
	if err == nil || !started {
		return err
	}
	// The reader gets the whole story; the sender whose context ended gets that.
	err = fmt.Errorf("transport: websocket: %w", err)
	ret := err
	if cerr := ctx.Err(); cerr != nil {
		err, ret = fmt.Errorf("transport: websocket: a send was cancelled partway through a frame: %w", cerr), cerr
	}
	if !w.shutdown(err) {
		return ErrClosed // the transport went down under this write; the reader has the reason
	}
	return ret
}

// Recv returns the next message. It returns io.EOF when the peer closed the
// link in the ordinary way, ErrClosed after Close, ErrFrameTooLarge for a
// message over the limit, the context's error when ctx ended the wait, and an
// error wrapping os.ErrDeadlineExceeded when the peer was silent for two ping
// intervals. Any error is final; see WS.
func (w *WS) Recv(ctx context.Context) ([]byte, error) {
	select {
	case <-w.done:
		return nil, w.err()
	default:
	}
	if w.rerr != nil {
		return nil, w.rerr
	}
	w.rctx = ctx
	stop := interruptOn(ctx, w.conn.SetReadDeadline)
	msg, err := w.readMessage()
	stop()
	switch {
	case err == nil:
		return msg, nil
	case err == ctx.Err():
		// ponytail: reading cannot be resumed after this; keep the half-read
		// frame across calls if a caller ever needs to cancel one Recv and go on.
		w.rerr = err
		return nil, err
	case err != io.EOF && err != ErrFrameTooLarge:
		err = fmt.Errorf("transport: websocket: %w", err)
	}
	w.shutdown(err)
	return nil, w.err()
}

// Close sends a close frame and closes the connection. Pending Send and Recv
// calls return ErrClosed. Idempotent.
//
// A Send that is in the middle of a frame is given a second to finish, so
// that the goodbye can follow it; a Send still blocked after that is cut off
// and no close frame is sent. The close frame is best effort beyond that too:
// the operating system may discard it if this end still had unread data when
// the connection closed.
func (w *WS) Close() error {
	if w.closed.Swap(true) {
		return nil
	}
	grace := time.NewTimer(wsCloseGrace)
	defer grace.Stop()
	select {
	case w.wlock <- struct{}{}:
		_, _ = w.writeFrame(context.Background(), ws.OpClose, ws.NewCloseFrameBody(ws.StatusNormalClosure, ""), min(w.wtimeout, wsCloseGrace))
		w.shutdown(ErrClosed) // before the lock is released: nothing may follow a close frame
		<-w.wlock
	case <-w.done: // already down after a failure: there is no connection to say goodbye on
	case <-grace.C:
	}
	w.shutdown(ErrClosed)
	return nil
}

// shutdown takes the transport down because of cause and reports whether this
// call was the one that did it.
func (w *WS) shutdown(cause error) (first bool) {
	w.down.Do(func() {
		first = true
		w.cause = cause
		close(w.done)
		_ = w.conn.Close()
	})
	return first
}

// err is what Recv reports once the transport is down.
func (w *WS) err() error {
	if w.closed.Load() {
		return ErrClosed
	}
	return w.cause
}

// readMessage reads frames until a whole message has arrived, handling the
// control frames that come in between.
func (w *WS) readMessage() ([]byte, error) {
	msg := []byte{}
	state := w.state
	for {
		h, err := ws.ReadHeader(w.br)
		if err != nil {
			return nil, err
		}
		if err := ws.CheckHeader(h, state); err != nil {
			return nil, w.reject(ws.StatusProtocolError, err)
		}
		if h.OpCode.IsControl() {
			var buf [ws.MaxControlFramePayloadSize]byte
			p := buf[:h.Length] // CheckHeader holds a control frame to 125 bytes
			if _, err := io.ReadFull(w.br, p); err != nil {
				return nil, err
			}
			if h.Masked {
				ws.Cipher(p, h.Mask, 0)
			}
			switch h.OpCode {
			case ws.OpPing:
				if _, err := w.control(ws.OpPong, p, w.wtimeout); err != nil {
					return nil, err
				}
			case ws.OpClose:
				return nil, w.peerClosed(p)
			}
			continue // a pong needs no answer: its bytes already counted as a sign of life
		}
		if h.OpCode == ws.OpText {
			return nil, w.reject(ws.StatusUnsupportedData, errors.New("text frame refused: the link carries binary messages only"))
		}
		// The length is the peer's claim. It is checked before a byte is
		// allocated for it, against what is left of the limit.
		if h.Length > int64(w.max-len(msg)) {
			return nil, w.reject(ws.StatusMessageTooBig, ErrFrameTooLarge)
		}
		n := len(msg)
		msg = slices.Grow(msg, int(h.Length))[:n+int(h.Length)]
		if _, err := io.ReadFull(w.br, msg[n:]); err != nil {
			return nil, err
		}
		if h.Masked {
			ws.Cipher(msg[n:], h.Mask, 0)
		}
		if h.Fin {
			return msg, nil
		}
		state = state.Set(ws.StateFragmented)
	}
}

// reject ends the link over something the peer sent. The peer is told why, as
// far as a close status can say it, and err goes back to Recv's caller.
func (w *WS) reject(code ws.StatusCode, err error) error {
	_, _ = w.control(ws.OpClose, ws.NewCloseFrameBody(code, ""), w.wtimeout)
	return err
}

// peerClosed answers the peer's close frame and turns its status into Recv's
// result: io.EOF for an ordinary goodbye, an error naming the status
// otherwise. The reason text is the peer's own and is not repeated.
func (w *WS) peerClosed(payload []byte) error {
	code := ws.StatusNormalClosure
	if len(payload) >= 2 {
		code = ws.StatusCode(binary.BigEndian.Uint16(payload))
		payload = payload[:2]
	}
	_, _ = w.control(ws.OpClose, payload, w.wtimeout)
	if code == ws.StatusNormalClosure || code == ws.StatusGoingAway {
		return io.EOF
	}
	return fmt.Errorf("the peer closed the link with status %d", code)
}

// control writes a control frame from inside Recv, unless a Send holds the
// write lock: then it is skipped and sent is false. The reader never
// waits for a writer. If it did, two ends that each stopped reading in order
// to answer the other could wait on each other for good. Nothing is lost by
// skipping: the frame being sent is itself proof of life to the peer, and a
// link with a Send in flight is watched by that Send's write deadline.
func (w *WS) control(op ws.OpCode, payload []byte, timeout time.Duration) (sent bool, err error) {
	select {
	case w.wlock <- struct{}{}:
	default:
		return false, nil
	}
	defer func() { <-w.wlock }()
	_, err = w.writeFrame(context.Background(), op, payload, timeout)
	return true, err
}

// writeFrame writes one unfragmented frame. The caller holds the write lock.
// The frame goes out in pieces of at most wsChunk bytes, each with its own
// deadline. started reports whether anything may have reached the wire: when
// it is false the link is exactly as it was.
func (w *WS) writeFrame(ctx context.Context, op ws.OpCode, payload []byte, timeout time.Duration) (started bool, err error) {
	h := ws.Header{Fin: true, OpCode: op, Length: int64(len(payload))}
	if w.state == ws.StateClientSide {
		h.Masked, h.Mask = true, ws.NewMask()
	}
	// Header and payload leave in one write, and the payload is masked in this
	// buffer, never in the caller's.
	buf := bytes.NewBuffer(make([]byte, 0, ws.MaxHeaderSize+min(len(payload), wsChunk)))
	_ = ws.WriteHeader(buf, h) // cannot fail: the buffer takes any write and the length is an int
	for off := 0; ; buf.Reset() {
		n := min(len(payload)-off, wsChunk)
		buf.Write(payload[off : off+n])
		if h.Masked {
			ws.Cipher(buf.Bytes()[buf.Len()-n:], h.Mask, off)
		}
		_ = w.conn.SetWriteDeadline(time.Now().Add(timeout))
		// After the deadline is set, not before: a context that ended in
		// between has had its own deadline overwritten just above.
		if err := ctx.Err(); err != nil {
			return off > 0, err
		}
		if _, err := w.conn.Write(buf.Bytes()); err != nil {
			return true, fmt.Errorf("write: %w", err)
		}
		if off += n; off == len(payload) {
			return true, nil
		}
	}
}

// wsReader is the connection as Recv reads it: the liveness rule is applied
// to every read. See the package comment.
type wsReader struct{ w *WS }

func (r wsReader) Read(p []byte) (int, error) {
	w := r.w
	for {
		wait := w.patience[0]
		if w.silent {
			wait = w.patience[1]
		}
		_ = w.conn.SetReadDeadline(time.Now().Add(wait))
		// After the deadline is set, not before, for the same reason as in writeFrame.
		if err := w.rctx.Err(); err != nil {
			return 0, err
		}
		n, err := w.conn.Read(p)
		if err == io.EOF {
			err = io.ErrUnexpectedEOF // only a close frame ends the link cleanly
		}
		if n > 0 {
			w.silent = false
			if errors.Is(err, os.ErrDeadlineExceeded) {
				err = nil // crypto/tls can run out of time on a trailing alert after it has data to give
			}
			return n, err
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			return 0, err
		}
		if err := w.rctx.Err(); err != nil {
			return 0, err // the context moved the deadline
		}
		if w.silent {
			return 0, fmt.Errorf("no data from the peer for %v: %w", w.patience[0]+w.patience[1], os.ErrDeadlineExceeded)
		}
		// One period of quiet. Ask the peer to say something, and give the ping
		// no longer to leave than the peer has left to answer. If a Send is in
		// flight nothing is sent and the count does not start: the peer may be
		// quiet because it is busy reading that frame.
		sent, err := w.control(ws.OpPing, nil, min(w.wtimeout, w.patience[1]))
		if err != nil {
			return 0, err
		}
		w.silent = sent
	}
}

// interruptOn makes the end of ctx cut short a read or write that is blocked
// on the connection, by moving its deadline into the past. The returned stop
// ends the arrangement, and waits out a callback that has already started so
// that it cannot land on a later call.
func interruptOn(ctx context.Context, setDeadline func(time.Time) error) (stop func()) {
	if ctx.Done() == nil {
		return func() {}
	}
	ran := make(chan struct{})
	halt := context.AfterFunc(ctx, func() {
		defer close(ran)
		_ = setDeadline(aLongTimeAgo)
	})
	return func() {
		if !halt() {
			<-ran
		}
	}
}
