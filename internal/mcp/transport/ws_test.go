package transport_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gobwas/ws"

	"tenant/internal/mcp/transport"
)

// --- helpers ---

const waitLong = 5 * time.Second // how long a test waits for something that should be immediate

// scriptConn is a connection whose peer has already said everything it will
// ever say: reads replay the script and then report end of stream, writes are
// counted and thrown away. No goroutine, no clock.
type scriptConn struct {
	r     *bytes.Reader
	read  int // bytes the transport took from the script
	wrote int // bytes the transport wrote
}

func newScriptConn(script []byte) *scriptConn { return &scriptConn{r: bytes.NewReader(script)} }

func (c *scriptConn) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}
func (c *scriptConn) Write(p []byte) (int, error)      { c.wrote += len(p); return len(p), nil }
func (c *scriptConn) Close() error                     { return nil }
func (c *scriptConn) LocalAddr() net.Addr              { return nil }
func (c *scriptConn) RemoteAddr() net.Addr             { return nil }
func (c *scriptConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptConn) SetWriteDeadline(time.Time) error { return nil }

// trackConn counts the connections that are still open and notes every write.
type trackConn struct {
	net.Conn
	open   *atomic.Int32
	once   sync.Once
	mu     sync.Mutex
	writes []written // one per Write
}

// written is what one Write looked like: its first byte and its size.
type written struct {
	first byte
	size  int
}

func (c *trackConn) Close() error {
	c.once.Do(func() { c.open.Add(-1) })
	return c.Conn.Close()
}

func (c *trackConn) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.mu.Lock()
		c.writes = append(c.writes, written{p[0], len(p)})
		c.mu.Unlock()
	}
	return c.Conn.Write(p)
}

// tally returns how many writes started with each first byte, and the largest
// write that started with it.
func (c *trackConn) tally() (count, largest map[byte]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	count, largest = map[byte]int{}, map[byte]int{}
	for _, w := range c.writes {
		count[w.first]++
		largest[w.first] = max(largest[w.first], w.size)
	}
	return count, largest
}

func track(conn net.Conn, open *atomic.Int32) *trackConn {
	open.Add(1)
	return &trackConn{Conn: conn, open: open}
}

// trackListener wraps every accepted connection in a trackConn.
type trackListener struct {
	net.Listener
	open *atomic.Int32
	mu   sync.Mutex
	last *trackConn
}

func (l *trackListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := track(c, l.open)
	l.mu.Lock()
	l.last = tc
	l.mu.Unlock()
	return tc, nil
}

// peer is the hand-driven end of a link over net.Pipe. The test writes raw
// frames into it, well formed or not, and reads back what the transport sent.
type peer struct {
	t      *testing.T
	conn   net.Conn
	client bool          // this end plays the dialing side, so it masks what it writes
	got    chan ws.Frame // every frame the transport wrote, payload unmasked, header as sent
}

func newPeer(t *testing.T, conn net.Conn, client bool) *peer {
	p := &peer{t: t, conn: conn, client: client, got: make(chan ws.Frame, 4096)}
	go func() {
		defer close(p.got)
		for {
			f, err := ws.ReadFrame(conn)
			if err != nil {
				return
			}
			if f.Header.Masked {
				ws.Cipher(f.Payload, f.Header.Mask, 0)
			}
			p.got <- f
		}
	}()
	return p
}

// write sends raw bytes to the transport. net.Pipe has no buffer, so this
// returns once the transport has read them, or fails the test if it never does.
func (p *peer) write(b []byte) error {
	_ = p.conn.SetWriteDeadline(time.Now().Add(waitLong))
	_, err := p.conn.Write(b)
	return err
}

// frame encodes one frame the way this end must: masked if it plays the client.
func (p *peer) frame(op ws.OpCode, fin bool, payload []byte) []byte {
	return encode(ws.NewFrame(op, fin, bytes.Clone(payload)), p.client)
}

func encode(f ws.Frame, mask bool) []byte {
	if mask {
		f = ws.MaskFrameInPlace(f)
	}
	return ws.MustCompileFrame(f)
}

// header encodes a bare header, with whatever length it claims.
func header(h ws.Header) []byte {
	var b bytes.Buffer
	if err := ws.WriteHeader(&b, h); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// next returns the next frame the transport wrote.
func (p *peer) next() ws.Frame {
	p.t.Helper()
	select {
	case f, ok := <-p.got:
		if !ok {
			p.t.Fatal("the transport closed the connection before writing the expected frame")
		}
		return f
	case <-time.After(waitLong):
		p.t.Fatal("the transport wrote no frame")
	}
	panic("unreachable")
}

// closeStatus waits for the transport's close frame and returns its status.
func (p *peer) closeStatus() ws.StatusCode {
	p.t.Helper()
	for {
		f := p.next()
		if f.Header.OpCode == ws.OpClose {
			code, _ := ws.ParseCloseFrameData(f.Payload)
			return code
		}
	}
}

// pipeLink returns a transport for one end of a net.Pipe and the peer that
// plays the other end. asServer picks the accepting side for the transport.
func pipeLink(t *testing.T, asServer bool, cfg transport.WSConfig) (*transport.WS, *peer) {
	t.Helper()
	a, b := net.Pipe()
	var tr *transport.WS
	if asServer {
		tr = transport.NewWSServer(a, cfg)
	} else {
		tr = transport.NewWSClient(a, cfg)
	}
	p := newPeer(t, b, asServer)
	t.Cleanup(func() {
		_ = b.Close()
		_ = tr.Close()
	})
	return tr, p
}

// rawPipe returns a transport for one end of a net.Pipe and the other end,
// bare. Nothing reads the bare end unless the test does, so at cleanup it is
// closed first: the transport's goodbye must not wait on a reader that is not there.
func rawPipe(t *testing.T, asServer bool, cfg transport.WSConfig) (*transport.WS, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	return rawPipeOn(t, a, b, asServer, cfg), b
}

func rawPipeOn(t *testing.T, a, b net.Conn, asServer bool, cfg transport.WSConfig) *transport.WS {
	t.Helper()
	tr := transport.NewWSClient(a, cfg)
	if asServer {
		tr = transport.NewWSServer(a, cfg)
	}
	t.Cleanup(func() {
		_ = b.Close()
		_ = tr.Close()
	})
	return tr
}

type result struct {
	msg []byte
	err error
}

// recvAsync runs one Recv in the background.
func recvAsync(ctx context.Context, tr transport.Transport) <-chan result {
	ch := make(chan result, 1)
	go func() {
		msg, err := tr.Recv(ctx)
		ch <- result{msg, err}
	}()
	return ch
}

func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitLong):
		t.Fatalf("timed out waiting for %s", what)
	}
	panic("unreachable")
}

var bg = context.Background()

// sides runs a test once with the transport as the accepting side and once as
// the dialing side.
func sides(t *testing.T, fn func(t *testing.T, asServer bool)) {
	t.Helper()
	t.Run("accepting", func(t *testing.T) { fn(t, true) })
	t.Run("dialing", func(t *testing.T) { fn(t, false) })
}

// --- refusals ---

// Every frame the RFC or this link forbids ends Recv with an error, and the
// peer is told why with a close status when one fits.
func TestWS_RefusesBadFrames(t *testing.T) {
	const max = 1024
	big := bytes.Repeat([]byte("x"), max)
	cases := []struct {
		name   string
		script func(p *peer) []byte
		is     error  // what the error must wrap, or
		says   string // what it must say
		status ws.StatusCode
	}{
		{"reserved opcode", func(p *peer) []byte { return p.frame(0x3, true, []byte("x")) },
			ws.ErrProtocolOpCodeReserved, "", ws.StatusProtocolError},
		{"reserved bit with no extension", func(p *peer) []byte {
			return header(ws.Header{Fin: true, Rsv: ws.Rsv(true, false, false), OpCode: ws.OpBinary, Masked: p.client})
		}, ws.ErrProtocolNonZeroRsv, "", ws.StatusProtocolError},
		{"continuation with nothing to continue", func(p *peer) []byte { return p.frame(ws.OpContinuation, true, []byte("x")) },
			ws.ErrProtocolContinuationUnexpected, "", ws.StatusProtocolError},
		{"new message inside a fragmented one", func(p *peer) []byte {
			return append(p.frame(ws.OpBinary, false, []byte("a")), p.frame(ws.OpBinary, true, []byte("b"))...)
		}, ws.ErrProtocolContinuationExpected, "", ws.StatusProtocolError},
		{"control frame over 125 bytes", func(p *peer) []byte {
			return header(ws.Header{Fin: true, OpCode: ws.OpPing, Masked: p.client, Length: 126})
		}, ws.ErrProtocolControlPayloadOverflow, "", ws.StatusProtocolError},
		{"fragmented control frame", func(p *peer) []byte {
			return header(ws.Header{OpCode: ws.OpPing, Masked: p.client})
		}, ws.ErrProtocolControlNotFinal, "", ws.StatusProtocolError},
		{"length with the top bit set", func(p *peer) []byte {
			b := header(ws.Header{Fin: true, OpCode: ws.OpBinary, Length: 1 << 62})
			b[2] |= 0x80
			return b
		}, ws.ErrHeaderLengthMSB, "", 0},
		{"text frame", func(p *peer) []byte { return p.frame(ws.OpText, true, []byte(`{}`)) },
			nil, "text frame", ws.StatusUnsupportedData},
		{"text frame as the first fragment", func(p *peer) []byte { return p.frame(ws.OpText, false, []byte(`{`)) },
			nil, "text frame", ws.StatusUnsupportedData},
		{"one frame over the limit", func(p *peer) []byte {
			return header(ws.Header{Fin: true, OpCode: ws.OpBinary, Masked: p.client, Length: max + 1})
		}, transport.ErrFrameTooLarge, "", ws.StatusMessageTooBig},
		{"a frame claiming 2^63-1 bytes", func(p *peer) []byte {
			return header(ws.Header{Fin: true, OpCode: ws.OpBinary, Masked: p.client, Length: 1<<63 - 1})
		}, transport.ErrFrameTooLarge, "", ws.StatusMessageTooBig},
		{"fragments that add up over the limit", func(p *peer) []byte {
			return append(p.frame(ws.OpBinary, false, big),
				header(ws.Header{Fin: true, OpCode: ws.OpContinuation, Masked: p.client, Length: 1})...)
		}, transport.ErrFrameTooLarge, "", ws.StatusMessageTooBig},
		{"many small fragments over the limit", func(p *peer) []byte {
			var b []byte
			for i := 0; i < max/100+1; i++ {
				op := ws.OpContinuation
				if i == 0 {
					op = ws.OpBinary
				}
				b = append(b, p.frame(op, false, big[:100])...)
			}
			return b
		}, transport.ErrFrameTooLarge, "", ws.StatusMessageTooBig},
		{"an HTTP request where a frame belongs", func(p *peer) []byte { return []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n") },
			ws.ErrProtocolOpCodeReserved, "", ws.StatusProtocolError},
	}
	sides(t, func(t *testing.T, asServer bool) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tr, p := pipeLink(t, asServer, transport.WSConfig{MaxFrameSize: max})
				got := recvAsync(bg, tr)
				_ = p.write(tc.script(p)) // the transport may hang up before it has read all of it
				refused := func(err error) bool {
					if tc.is != nil {
						return errors.Is(err, tc.is)
					}
					return err != nil && strings.Contains(err.Error(), tc.says)
				}
				r := await(t, got, "Recv")
				if !refused(r.err) {
					t.Fatalf("Recv error = %v, want %v%s", r.err, tc.is, tc.says)
				}
				if tc.status != 0 {
					if code := p.closeStatus(); code != tc.status {
						t.Errorf("close status = %d, want %d", code, tc.status)
					}
				}
				// The refusal is final: the transport is down for both directions.
				if _, err := tr.Recv(bg); !refused(err) {
					t.Errorf("second Recv = %v, want the same error", err)
				}
				if err := tr.Send(bg, []byte("x")); !errors.Is(err, transport.ErrClosed) {
					t.Errorf("Send after a refusal = %v, want ErrClosed", err)
				}
			})
		}
	})
}

// A client's frames must be masked and a server's must not be (RFC 6455 5.1).
func TestWS_RefusesWrongMasking(t *testing.T) {
	t.Run("unmasked frame from the dialing side", func(t *testing.T) {
		tr, p := pipeLink(t, true, transport.WSConfig{})
		got := recvAsync(bg, tr)
		_ = p.write(encode(ws.NewBinaryFrame([]byte("x")), false))
		if r := await(t, got, "Recv"); !errors.Is(r.err, ws.ErrProtocolMaskRequired) {
			t.Fatalf("Recv error = %v, want %v", r.err, ws.ErrProtocolMaskRequired)
		}
		if code := p.closeStatus(); code != ws.StatusProtocolError {
			t.Errorf("close status = %d, want %d", code, ws.StatusProtocolError)
		}
	})
	t.Run("masked frame from the accepting side", func(t *testing.T) {
		tr, p := pipeLink(t, false, transport.WSConfig{})
		got := recvAsync(bg, tr)
		_ = p.write(encode(ws.NewBinaryFrame([]byte("x")), true))
		if r := await(t, got, "Recv"); !errors.Is(r.err, ws.ErrProtocolMaskUnexpected) {
			t.Fatalf("Recv error = %v, want %v", r.err, ws.ErrProtocolMaskUnexpected)
		}
		if code := p.closeStatus(); code != ws.StatusProtocolError {
			t.Errorf("close status = %d, want %d", code, ws.StatusProtocolError)
		}
	})
}

// A header that claims more than the limit is refused from the header alone:
// nothing the size of the claim is allocated, whatever the claim.
func TestWS_OversizeClaimCostsNothing(t *testing.T) {
	const max = 1 << 20
	claims := []int64{max + 1, 1 << 31, 1 << 40, 1<<63 - 1}
	for _, claim := range claims {
		script := header(ws.Header{Fin: true, OpCode: ws.OpBinary, Masked: true, Length: claim})
		conn := newScriptConn(script)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		tr := transport.NewWSServer(conn, transport.WSConfig{MaxFrameSize: max})
		_, err := tr.Recv(bg)
		runtime.ReadMemStats(&after)
		if !errors.Is(err, transport.ErrFrameTooLarge) {
			t.Fatalf("claim %d: Recv error = %v, want ErrFrameTooLarge", claim, err)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<10 {
			t.Errorf("claim %d: refusing it allocated %d bytes", claim, grew)
		}
	}
}

// A frame within the limit is allocated once, at its claimed size, and a
// fragmented message never holds more than a small multiple of the limit.
func TestWS_FragmentedMessageStaysNearTheLimit(t *testing.T) {
	const max = 1 << 20
	var script []byte
	chunk := bytes.Repeat([]byte("y"), 1000)
	for sent := 0; sent+len(chunk) <= max; sent += len(chunk) {
		op := ws.OpContinuation
		if sent == 0 {
			op = ws.OpBinary
		}
		script = append(script, encode(ws.NewFrame(op, false, bytes.Clone(chunk)), true)...)
	}
	script = append(script, encode(ws.NewFrame(ws.OpContinuation, true, nil), true)...)
	conn := newScriptConn(script)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	tr := transport.NewWSServer(conn, transport.WSConfig{MaxFrameSize: max})
	msg, err := tr.Recv(bg)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if want := max / len(chunk) * len(chunk); len(msg) != want {
		t.Fatalf("message is %d bytes, want %d", len(msg), want)
	}
	// Growing a slice a fragment at a time copies it now and then; the total
	// ever allocated stays a small multiple of the message.
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8*max {
		t.Errorf("a %d-byte message in 1000-byte fragments allocated %d bytes in total", len(msg), grew)
	}
}

func TestWS_RefusesOversizeOutbound(t *testing.T) {
	sides(t, func(t *testing.T, asServer bool) {
		const max = 1024
		tr, p := pipeLink(t, asServer, transport.WSConfig{MaxFrameSize: max})
		if err := tr.Send(bg, make([]byte, max+1)); !errors.Is(err, transport.ErrFrameTooLarge) {
			t.Fatalf("Send over the limit = %v, want ErrFrameTooLarge", err)
		}
		// Nothing went out, and the link is still good: the limit itself fits.
		if err := tr.Send(bg, bytes.Repeat([]byte("z"), max)); err != nil {
			t.Fatalf("Send at the limit: %v", err)
		}
		f := p.next()
		if f.Header.OpCode != ws.OpBinary || len(f.Payload) != max {
			t.Fatalf("first frame on the wire: opcode %v, %d bytes; want the %d-byte message", f.Header.OpCode, len(f.Payload), max)
		}
	})
}

// --- the opening handshake ---

// wsServer is an HTTP server whose one handler upgrades to the transport.
type wsServer struct {
	*httptest.Server
	open     atomic.Int32       // connections accepted and not yet closed
	accepted chan *transport.WS // one per successful upgrade
	failed   chan error         // one per refused upgrade
	ln       *trackListener
}

func newWSServer(t *testing.T, cfg transport.WSConfig, useTLS bool) *wsServer {
	t.Helper()
	s := &wsServer{accepted: make(chan *transport.WS, 16), failed: make(chan error, 16)}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr, err := transport.UpgradeWSServer(w, r, cfg)
		if err != nil {
			s.failed <- err
			return
		}
		s.accepted <- tr
	}))
	s.ln = &trackListener{Listener: s.Listener, open: &s.open}
	s.Listener = s.ln
	if useTLS {
		s.StartTLS()
	} else {
		s.Start()
	}
	t.Cleanup(s.Close)
	return s
}

func (s *wsServer) addr() string { return s.Listener.Addr().String() }

// dial connects to the server the way the link will: the caller makes the
// connection, TLS included, and hands it to the transport for the upgrade.
func (s *wsServer) dial(t *testing.T, open *atomic.Int32) net.Conn {
	t.Helper()
	var conn net.Conn
	var err error
	if s.TLS != nil {
		conf := s.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		conn, err = tls.Dial("tcp", s.addr(), conf)
	} else {
		conn, err = net.Dial("tcp", s.addr())
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if open != nil {
		return track(conn, open)
	}
	return conn
}

// link returns both ends of an upgraded connection to s.
func (s *wsServer) link(t *testing.T, cfg transport.WSConfig, open *atomic.Int32) (client, server *transport.WS) {
	t.Helper()
	scheme := "ws"
	if s.TLS != nil {
		scheme = "wss"
	}
	client, err := transport.UpgradeWSClient(bg, s.dial(t, open), scheme+"://"+s.addr()+"/link", cfg)
	if err != nil {
		t.Fatalf("UpgradeWSClient: %v", err)
	}
	select {
	case server = <-s.accepted:
	case err := <-s.failed:
		t.Fatalf("UpgradeWSServer: %v", err)
	case <-time.After(waitLong):
		t.Fatal("the server never upgraded")
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

// waitClosed fails the test unless every tracked connection gets closed.
func waitClosed(t *testing.T, open *atomic.Int32, what string) {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for open.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d connection(s) left open", what, open.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A request that is not a WebSocket upgrade is answered with an HTTP error and
// its connection is closed, not left hijacked and open.
func TestWSUpgradeServer_RefusesAPlainRequest(t *testing.T) {
	s := newWSServer(t, transport.WSConfig{}, false)
	conn, err := net.Dial("tcp", s.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /link HTTP/1.1\r\nHost: %s\r\n\r\n", s.addr())
	if err := await(t, s.failed, "the refusal"); !errors.Is(err, ws.ErrHandshakeBadUpgrade) {
		t.Fatalf("UpgradeWSServer error = %v, want %v", err, ws.ErrHandshakeBadUpgrade)
	}
	_ = conn.SetReadDeadline(time.Now().Add(waitLong))
	reply, err := io.ReadAll(conn) // returns only when the server closes its end
	if err != nil {
		t.Fatalf("the server left the refused connection open: %v", err)
	}
	if !bytes.HasPrefix(reply, []byte("HTTP/1.1 400 ")) {
		t.Errorf("reply = %q, want a 400", reply)
	}
	waitClosed(t, &s.open, "after a refused upgrade")
}

// A client may not send frames until it has seen the server's answer. One that
// does is cut off: those bytes were read by the HTTP server, not by the link.
func TestWSUpgradeServer_RefusesDataBeforeTheAnswer(t *testing.T) {
	s := newWSServer(t, transport.WSConfig{}, false)
	conn, err := net.Dial("tcp", s.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := fmt.Sprintf("GET /link HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", s.addr())
	early := encode(ws.NewBinaryFrame([]byte("too soon")), true)
	if _, err := conn.Write(append([]byte(req), early...)); err != nil {
		t.Fatal(err)
	}
	if err := await(t, s.failed, "the refusal"); !strings.Contains(err.Error(), "before the handshake") {
		t.Fatalf("UpgradeWSServer error = %v, want the early-data refusal", err)
	}
	waitClosed(t, &s.open, "after early data")
}

// rawServer accepts one TCP connection and hands it to serve.
func rawServer(t *testing.T, serve func(conn net.Conn)) (addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		serve(conn)
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return ln.Addr().String()
}

// readRequest reads the client's upgrade request and returns the accept value
// a real server would answer with.
func readRequest(conn net.Conn) (accept string, err error) {
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return "", err
	}
	sum := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

func TestWSUpgradeClient_Refusals(t *testing.T) {
	until := func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) } // hold the connection until the client hangs up
	cases := []struct {
		name  string
		serve func(conn net.Conn)
		check func(t *testing.T, err error)
	}{
		{"an ordinary HTTP answer", func(conn net.Conn) {
			_, _ = readRequest(conn)
			_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
			until(conn)
		}, func(t *testing.T, err error) {
			if !errors.Is(err, ws.StatusError(200)) {
				t.Errorf("error = %v, want the 200 status", err)
			}
		}},
		{"the wrong accept key", func(conn net.Conn) {
			_, _ = readRequest(conn)
			_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
				"Sec-WebSocket-Accept: AAAAAAAAAAAAAAAAAAAAAAAAAAA=\r\n\r\n")
			until(conn)
		}, func(t *testing.T, err error) {
			if !errors.Is(err, ws.ErrHandshakeBadSecAccept) {
				t.Errorf("error = %v, want %v", err, ws.ErrHandshakeBadSecAccept)
			}
		}},
		{"an extension nobody asked for", func(conn net.Conn) {
			accept, _ := readRequest(conn)
			_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
				"Sec-WebSocket-Accept: "+accept+"\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n")
			until(conn)
		}, func(t *testing.T, err error) {
			if !errors.Is(err, ws.ErrHandshakeBadExtensions) {
				t.Errorf("error = %v, want %v", err, ws.ErrHandshakeBadExtensions)
			}
		}},
		{"frames where the answer belongs", func(conn net.Conn) {
			_, _ = readRequest(conn)
			_, _ = conn.Write(encode(ws.NewBinaryFrame([]byte("hello")), false))
			until(conn)
		}, func(t *testing.T, err error) {
			// Seven bytes with no line end: the client waits for the rest of a
			// status line, for as long as it waits for anything.
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("error = %v, want a deadline error", err)
			}
		}},
		{"a header line that never ends", func(conn net.Conn) {
			_, _ = readRequest(conn)
			junk := bytes.Repeat([]byte("a"), 64<<10)
			_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nX-Filler: ")
			for {
				if _, err := conn.Write(junk); err != nil {
					return
				}
			}
		}, func(t *testing.T, err error) {
			if !strings.Contains(err.Error(), "longer than 4096 bytes") {
				t.Errorf("error = %v, want the size refusal", err)
			}
		}},
		{"no answer at all", func(conn net.Conn) { until(conn) }, func(t *testing.T, err error) {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("error = %v, want a deadline error", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := rawServer(t, tc.serve)
			raw, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			var open atomic.Int32
			conn := &countConn{Conn: track(raw, &open)}
			start := time.Now()
			tr, err := transport.UpgradeWSClient(bg, conn, "ws://"+addr+"/link", transport.WSConfig{PingInterval: 150 * time.Millisecond})
			if err == nil {
				_ = tr.Close()
				t.Fatal("the handshake succeeded")
			}
			tc.check(t, err)
			if took := time.Since(start); took > 2*time.Second {
				t.Errorf("the refusal took %v", took)
			}
			// However much the server sends, the client reads a bounded amount.
			if n := conn.read.Load(); n > 4096 {
				t.Errorf("the client read %d bytes of handshake answer, want at most 4096", n)
			}
			waitClosed(t, &open, "after a refused handshake")
		})
	}
}

// countConn counts the bytes read from a connection.
type countConn struct {
	net.Conn
	read atomic.Int64
}

func (c *countConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

func TestWSUpgradeClient_RefusesABadURL(t *testing.T) {
	for _, u := range []string{"", "/link", "127.0.0.1:1/link", "ws://host with space/link", "ws://host/\r\nX-Injected: 1"} {
		a, b := net.Pipe()
		var open atomic.Int32
		tr, err := transport.UpgradeWSClient(bg, track(a, &open), u, transport.WSConfig{})
		if err == nil {
			_ = tr.Close()
			t.Errorf("URL %q was accepted", u)
		}
		if open.Load() != 0 {
			t.Errorf("URL %q: the connection was left open", u)
		}
		_ = b.Close()
	}
}

func TestWSUpgradeClient_ContextCancels(t *testing.T) {
	addr := rawServer(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	var open atomic.Int32
	ctx, cancel := context.WithCancel(bg)
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err = transport.UpgradeWSClient(ctx, track(raw, &open), "ws://"+addr+"/link", transport.WSConfig{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("cancelling took %v", took)
	}
	waitClosed(t, &open, "after a cancelled handshake")
}

// A server may send its first frame in the same packet as its answer. Those
// bytes were read with the answer and must reach Recv.
func TestWSUpgradeClient_KeepsFramesSentWithTheAnswer(t *testing.T) {
	addr := rawServer(t, func(conn net.Conn) {
		accept, err := readRequest(conn)
		if err != nil {
			return
		}
		answer := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"
		first := encode(ws.NewBinaryFrame([]byte("first")), false)
		second := encode(ws.NewBinaryFrame([]byte("second")), false)
		_, _ = conn.Write(append(append([]byte(answer), first...), second[:3]...))
		time.Sleep(20 * time.Millisecond)
		_, _ = conn.Write(second[3:])
		_, _ = io.Copy(io.Discard, conn)
	})
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := transport.UpgradeWSClient(bg, raw, "ws://"+addr+"/link", transport.WSConfig{})
	if err != nil {
		t.Fatalf("UpgradeWSClient: %v", err)
	}
	defer tr.Close()
	for _, want := range []string{"first", "second"} {
		msg, err := tr.Recv(bg)
		if err != nil || string(msg) != want {
			t.Fatalf("Recv = %q, %v; want %q", msg, err, want)
		}
	}
}

// Bytes that are not frames, after a real upgrade on a real socket.
func TestWS_GarbageAfterTheUpgrade(t *testing.T) {
	t.Run("at the accepting side", func(t *testing.T) {
		s := newWSServer(t, transport.WSConfig{}, false)
		conn := s.dial(t, nil)
		defer conn.Close()
		// Upgrade properly, then stop speaking WebSocket.
		br, _, err := ws.Dialer{}.Upgrade(conn, mustURL("ws://"+s.addr()+"/link"))
		if err != nil {
			t.Fatal(err)
		}
		if br != nil {
			ws.PutReader(br)
		}
		tr := await(t, s.accepted, "the upgrade")
		defer tr.Close()
		_, _ = conn.Write(bytes.Repeat([]byte{0xff, 0x00, 0x13, 0x37}, 64))
		if _, err := tr.Recv(bg); err == nil || err == io.EOF {
			t.Fatalf("Recv = %v, want a refusal", err)
		}
		waitClosed(t, &s.open, "after garbage")
	})
	t.Run("at the dialing side", func(t *testing.T) {
		addr := rawServer(t, func(conn net.Conn) {
			accept, err := readRequest(conn)
			if err != nil {
				return
			}
			_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+accept+"\r\n\r\n")
			_, _ = conn.Write(bytes.Repeat([]byte{0xff, 0x00, 0x13, 0x37}, 64))
			_, _ = io.Copy(io.Discard, conn)
		})
		raw, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		var open atomic.Int32
		tr, err := transport.UpgradeWSClient(bg, track(raw, &open), "ws://"+addr+"/link", transport.WSConfig{})
		if err != nil {
			t.Fatalf("UpgradeWSClient: %v", err)
		}
		defer tr.Close()
		if _, err := tr.Recv(bg); err == nil || err == io.EOF {
			t.Fatalf("Recv = %v, want a refusal", err)
		}
		waitClosed(t, &open, "after garbage")
	})
}

// --- frames in, frames out ---

func TestWS_RoundTripSizes(t *testing.T) {
	sides(t, func(t *testing.T, asServer bool) {
		const max = 200 << 10
		tr, p := pipeLink(t, asServer, transport.WSConfig{MaxFrameSize: max})
		// The sizes where the header changes shape, where a write is split, and the limit.
		for _, n := range []int{0, 1, 125, 126, 65535, 65536, 32<<10 - 1, 32 << 10, 32<<10 + 1, max} {
			want := make([]byte, n)
			for i := range want {
				want[i] = byte(i*7 + n)
			}
			// Out: one unfragmented binary frame, masked only from the dialing side.
			sent := bytes.Clone(want)
			if err := tr.Send(bg, sent); err != nil {
				t.Fatalf("Send %d bytes: %v", n, err)
			}
			if !bytes.Equal(sent, want) {
				t.Fatalf("Send changed the caller's %d-byte buffer", n)
			}
			f := p.next()
			if f.Header.OpCode != ws.OpBinary || !f.Header.Fin || f.Header.Rsv != 0 {
				t.Fatalf("%d bytes went out as %+v", n, f.Header)
			}
			if f.Header.Masked == asServer {
				t.Fatalf("%d bytes: masked = %v on the wire from the %s side", n, f.Header.Masked, map[bool]string{true: "accepting", false: "dialing"}[asServer])
			}
			if !bytes.Equal(f.Payload, want) {
				t.Fatalf("%d bytes arrived changed", n)
			}
			// In.
			got := recvAsync(bg, tr)
			if err := p.write(p.frame(ws.OpBinary, true, want)); err != nil {
				t.Fatalf("peer write %d bytes: %v", n, err)
			}
			r := await(t, got, "Recv")
			if r.err != nil || !bytes.Equal(r.msg, want) {
				t.Fatalf("Recv of %d bytes: %d bytes, %v", n, len(r.msg), r.err)
			}
			if r.msg == nil {
				t.Fatalf("Recv of %d bytes returned a nil frame", n)
			}
		}
	})
}

// A fragmented message is put back together, and the control frames a peer may
// slip between its fragments are handled without disturbing it.
func TestWS_FragmentsAndControlFramesBetweenThem(t *testing.T) {
	sides(t, func(t *testing.T, asServer bool) {
		tr, p := pipeLink(t, asServer, transport.WSConfig{})
		got := recvAsync(bg, tr)
		for _, b := range [][]byte{
			p.frame(ws.OpBinary, false, []byte("one ")),
			p.frame(ws.OpPing, true, []byte("are you there")),
			p.frame(ws.OpContinuation, false, nil),
			p.frame(ws.OpPong, true, []byte("nobody asked")),
			p.frame(ws.OpContinuation, false, []byte("two ")),
			p.frame(ws.OpContinuation, true, []byte("three")),
		} {
			if err := p.write(b); err != nil {
				t.Fatalf("peer write: %v", err)
			}
		}
		if r := await(t, got, "Recv"); r.err != nil || string(r.msg) != "one two three" {
			t.Fatalf("Recv = %q, %v", r.msg, r.err)
		}
		// The ping was answered with its own payload.
		if f := p.next(); f.Header.OpCode != ws.OpPong || string(f.Payload) != "are you there" {
			t.Fatalf("answer to the ping: opcode %v, payload %q", f.Header.OpCode, f.Payload)
		}
	})
}

func TestWS_PeerClose(t *testing.T) {
	sides(t, func(t *testing.T, asServer bool) {
		body := func(code ws.StatusCode) []byte { return ws.NewCloseFrameBody(code, "bye") }
		for _, tc := range []struct {
			name    string
			payload []byte
			clean   bool
		}{
			{"normal closure", body(ws.StatusNormalClosure), true},
			{"going away", body(ws.StatusGoingAway), true},
			{"no status", nil, true},
			{"message too big", body(ws.StatusMessageTooBig), false},
			{"protocol error", body(ws.StatusProtocolError), false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tr, p := pipeLink(t, asServer, transport.WSConfig{})
				got := recvAsync(bg, tr)
				if err := p.write(p.frame(ws.OpClose, true, tc.payload)); err != nil {
					t.Fatal(err)
				}
				r := await(t, got, "Recv")
				if tc.clean && r.err != io.EOF {
					t.Fatalf("Recv = %v, want io.EOF", r.err)
				}
				if !tc.clean {
					code, _ := ws.ParseCloseFrameData(tc.payload)
					if r.err == nil || r.err == io.EOF || !strings.Contains(r.err.Error(), fmt.Sprint(int(code))) {
						t.Fatalf("Recv = %v, want an error naming status %d", r.err, code)
					}
					// The peer's reason is its own text; it stays out of the error.
					if strings.Contains(r.err.Error(), "bye") {
						t.Errorf("the error carries the peer's text: %v", r.err)
					}
				}
				// The close is answered, and the link is over.
				if f := p.next(); f.Header.OpCode != ws.OpClose {
					t.Fatalf("answer to the close: opcode %v", f.Header.OpCode)
				}
				if err := tr.Send(bg, []byte("x")); !errors.Is(err, transport.ErrClosed) {
					t.Errorf("Send after the peer closed = %v, want ErrClosed", err)
				}
			})
		}
	})
}

// A connection that just ends is not a clean shutdown: only a close frame is.
func TestWS_DroppedConnectionIsNotEOF(t *testing.T) {
	sides(t, func(t *testing.T, asServer bool) {
		for _, tc := range []struct {
			name   string
			script func(p *peer) []byte
		}{
			{"between frames", func(p *peer) []byte { return nil }},
			{"inside a header", func(p *peer) []byte { return p.frame(ws.OpBinary, true, bytes.Repeat([]byte("x"), 300))[:3] }},
			{"before a payload", func(p *peer) []byte {
				return header(ws.Header{Fin: true, OpCode: ws.OpBinary, Masked: p.client, Length: 10})
			}},
			{"inside a payload", func(p *peer) []byte { return p.frame(ws.OpBinary, true, []byte("0123456789"))[:9] }},
			{"inside a ping", func(p *peer) []byte { return p.frame(ws.OpPing, true, []byte("0123456789"))[:9] }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tr, p := pipeLink(t, asServer, transport.WSConfig{})
				got := recvAsync(bg, tr)
				if b := tc.script(p); len(b) > 0 {
					if err := p.write(b); err != nil {
						t.Fatal(err)
					}
				}
				_ = p.conn.Close()
				if r := await(t, got, "Recv"); !errors.Is(r.err, io.ErrUnexpectedEOF) {
					t.Fatalf("Recv = %v, want io.ErrUnexpectedEOF", r.err)
				}
			})
		}
	})
}

func TestWS_Close(t *testing.T) {
	sides(t, func(t *testing.T, asServer bool) {
		t.Run("says goodbye and is idempotent", func(t *testing.T) {
			tr, p := pipeLink(t, asServer, transport.WSConfig{})
			if err := tr.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if code := p.closeStatus(); code != ws.StatusNormalClosure {
				t.Errorf("close status = %d, want %d", code, ws.StatusNormalClosure)
			}
			if _, ok := <-p.got; ok {
				t.Error("something followed the close frame")
			}
			if err := tr.Close(); err != nil {
				t.Errorf("second Close: %v", err)
			}
			if err := tr.Send(bg, []byte("x")); !errors.Is(err, transport.ErrClosed) {
				t.Errorf("Send after Close = %v, want ErrClosed", err)
			}
			if _, err := tr.Recv(bg); !errors.Is(err, transport.ErrClosed) {
				t.Errorf("Recv after Close = %v, want ErrClosed", err)
			}
		})
		t.Run("unblocks a pending Recv", func(t *testing.T) {
			tr, _ := pipeLink(t, asServer, transport.WSConfig{})
			got := recvAsync(bg, tr)
			time.Sleep(20 * time.Millisecond)
			_ = tr.Close()
			if r := await(t, got, "Recv"); !errors.Is(r.err, transport.ErrClosed) {
				t.Fatalf("pending Recv = %v, want ErrClosed", r.err)
			}
		})
		t.Run("unblocks a pending Send and one waiting behind it", func(t *testing.T) {
			tr, _ := rawPipe(t, asServer, transport.WSConfig{}) // nobody reads the far end, so a write never completes
			errs := make(chan error, 2)
			for i := 0; i < 2; i++ {
				go func() { errs <- tr.Send(bg, []byte("stuck")) }()
			}
			time.Sleep(20 * time.Millisecond)
			start := time.Now()
			_ = tr.Close()
			// Close gives the frame on its way out one second, then closes under it.
			near(t, "Close with a Send blocked", time.Since(start), 900*time.Millisecond, time.Second)
			for i := 0; i < 2; i++ {
				if err := await(t, errs, "Send"); !errors.Is(err, transport.ErrClosed) {
					t.Fatalf("pending Send = %v, want ErrClosed", err)
				}
			}
		})
		// A Send that has just delivered its frame may not have let go of the
		// write lock yet. The goodbye waits for it instead of being dropped.
		t.Run("says goodbye right behind a Send", func(t *testing.T) {
			for i := 0; i < 200; i++ {
				a, b := net.Pipe()
				tr := rawPipeOn(t, a, b, asServer, transport.WSConfig{})
				p := newPeer(t, b, asServer)
				go func() { _ = tr.Send(bg, []byte("last words")) }()
				if f := p.next(); string(f.Payload) != "last words" {
					t.Fatalf("frame on the wire = %q", f.Payload)
				}
				_ = tr.Close() // the peer has the frame; the sender may still hold the lock
				if f := p.next(); f.Header.OpCode != ws.OpClose {
					t.Fatalf("round %d: after the last frame came opcode %v, not a close frame", i, f.Header.OpCode)
				}
			}
		})
		// A failure closes the connection too; Close afterwards has nothing to say.
		t.Run("after a failure", func(t *testing.T) {
			tr, p := pipeLink(t, asServer, transport.WSConfig{})
			got := recvAsync(bg, tr)
			_ = p.write(p.frame(ws.OpText, true, []byte("x")))
			if r := await(t, got, "Recv"); r.err == nil || !strings.Contains(r.err.Error(), "text frame") {
				t.Fatalf("Recv = %v", r.err)
			}
			if err := tr.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if _, err := tr.Recv(bg); !errors.Is(err, transport.ErrClosed) {
				t.Errorf("Recv after Close = %v, want ErrClosed", err)
			}
		})
	})
}

func TestWS_ContextCancellation(t *testing.T) {
	sides(t, func(t *testing.T, asServer bool) {
		t.Run("aborts a blocked Recv", func(t *testing.T) {
			tr, p := pipeLink(t, asServer, transport.WSConfig{})
			ctx, cancel := context.WithCancel(bg)
			got := recvAsync(ctx, tr)
			time.Sleep(20 * time.Millisecond)
			start := time.Now()
			cancel()
			if r := await(t, got, "Recv"); !errors.Is(r.err, context.Canceled) {
				t.Fatalf("Recv = %v, want context.Canceled", r.err)
			}
			if took := time.Since(start); took > time.Second {
				t.Errorf("Recv took %v to notice", took)
			}
			// The read may have been cut inside a frame, so reading is over.
			if _, err := tr.Recv(bg); !errors.Is(err, context.Canceled) {
				t.Errorf("Recv afterwards = %v, want the same error", err)
			}
			// The connection is not: a session cancels its reader and then
			// closes, and that goodbye must still go out.
			if err := tr.Send(bg, []byte("still open")); err != nil {
				t.Errorf("Send afterwards: %v", err)
			}
			if f := p.next(); string(f.Payload) != "still open" {
				t.Errorf("frame on the wire = %q", f.Payload)
			}
			_ = tr.Close()
			if code := p.closeStatus(); code != ws.StatusNormalClosure {
				t.Errorf("close status = %d, want %d", code, ws.StatusNormalClosure)
			}
			if _, err := tr.Recv(bg); !errors.Is(err, transport.ErrClosed) {
				t.Errorf("Recv after Close = %v, want ErrClosed", err)
			}
		})
		t.Run("Recv with a context that is already done", func(t *testing.T) {
			tr, _ := pipeLink(t, asServer, transport.WSConfig{})
			ctx, cancel := context.WithCancel(bg)
			cancel()
			if _, err := tr.Recv(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Recv = %v, want context.Canceled", err)
			}
		})
		t.Run("aborts a Send blocked on a peer that is not reading", func(t *testing.T) {
			tr, _ := rawPipe(t, asServer, transport.WSConfig{})
			ctx, cancel := context.WithCancel(bg)
			errs := make(chan error, 1)
			go func() { errs <- tr.Send(ctx, []byte("stuck")) }()
			time.Sleep(20 * time.Millisecond)
			start := time.Now()
			cancel()
			if err := await(t, errs, "Send"); !errors.Is(err, context.Canceled) {
				t.Fatalf("Send = %v, want context.Canceled", err)
			}
			if took := time.Since(start); took > time.Second {
				t.Errorf("Send took %v to notice", took)
			}
			// Part of a frame may be on the wire: nothing can follow it.
			if err := tr.Send(bg, []byte("x")); !errors.Is(err, transport.ErrClosed) {
				t.Errorf("Send afterwards = %v, want ErrClosed", err)
			}
		})
		t.Run("a Send that has not started leaves the link alone", func(t *testing.T) {
			tr, p := pipeLink(t, asServer, transport.WSConfig{})
			done, cancel := context.WithCancel(bg)
			cancel()
			if err := tr.Send(done, []byte("never sent")); !errors.Is(err, context.Canceled) {
				t.Fatalf("Send with a finished context = %v, want context.Canceled", err)
			}
			if err := tr.Send(bg, []byte("sent")); err != nil {
				t.Fatalf("Send afterwards: %v", err)
			}
			if f := p.next(); string(f.Payload) != "sent" {
				t.Fatalf("first frame on the wire = %q", f.Payload)
			}
		})
		t.Run("a Send waiting its turn leaves the link alone", func(t *testing.T) {
			tr, b := rawPipe(t, asServer, transport.WSConfig{})
			first := make(chan error, 1)
			go func() { first <- tr.Send(bg, []byte("first")) }() // blocks: nobody reads yet
			time.Sleep(20 * time.Millisecond)
			ctx, cancel := context.WithCancel(bg)
			second := make(chan error, 1)
			go func() { second <- tr.Send(ctx, []byte("second")) }() // waits behind the first
			time.Sleep(20 * time.Millisecond)
			cancel()
			if err := await(t, second, "the waiting Send"); !errors.Is(err, context.Canceled) {
				t.Fatalf("waiting Send = %v, want context.Canceled", err)
			}
			// Now the peer reads: the first Send completes, untouched.
			p := newPeer(t, b, asServer)
			if err := await(t, first, "the first Send"); err != nil {
				t.Fatalf("first Send: %v", err)
			}
			if f := p.next(); string(f.Payload) != "first" {
				t.Fatalf("frame on the wire = %q", f.Payload)
			}
		})
	})
}

// Concurrent senders never interleave their frames.
func TestWS_ConcurrentSend(t *testing.T) {
	sides(t, func(t *testing.T, asServer bool) {
		const senders, each = 16, 40
		tr, p := pipeLink(t, asServer, transport.WSConfig{})
		var wg sync.WaitGroup
		for s := 0; s < senders; s++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < each; i++ {
					// Some long enough to be written in more than one piece.
					msg := bytes.Repeat([]byte{byte('a' + s)}, 1+(i%4)*20000)
					if err := tr.Send(bg, msg); err != nil {
						t.Errorf("Send: %v", err)
						return
					}
				}
			}()
		}
		for i := 0; i < senders*each; i++ {
			f := p.next()
			if f.Header.OpCode != ws.OpBinary || len(f.Payload) == 0 {
				t.Fatalf("frame %d: opcode %v, %d bytes", i, f.Header.OpCode, len(f.Payload))
			}
			if !bytes.Equal(f.Payload, bytes.Repeat(f.Payload[:1], len(f.Payload))) {
				t.Fatalf("frame %d mixes two senders", i)
			}
		}
		wg.Wait()
	})
}

// --- liveness ---

// near fails the test unless a duration is at least lo and at most hi plus the
// slack a loaded machine needs. Timers never fire early, so lo is exact.
func near(t *testing.T, what string, got, lo, hi time.Duration) {
	t.Helper()
	if got < lo || got > hi+750*time.Millisecond {
		t.Errorf("%s took %v, want between %v and %v", what, got, lo, hi)
	}
}

// A peer that goes silent is pinged after one interval and given up on at two.
func TestWS_SilentPeerDetectedWithinTwoIntervals(t *testing.T) {
	const interval = 200 * time.Millisecond
	sides(t, func(t *testing.T, asServer bool) {
		tr, p := pipeLink(t, asServer, transport.WSConfig{PingInterval: interval})
		start := time.Now()
		got := recvAsync(bg, tr)
		// The peer reads what it is sent and answers nothing.
		f := p.next()
		pinged := time.Since(start)
		if f.Header.OpCode != ws.OpPing || len(f.Payload) != 0 {
			t.Fatalf("first frame to a silent peer: opcode %v, %d bytes; want an empty ping", f.Header.OpCode, len(f.Payload))
		}
		r := await(t, got, "Recv")
		gaveUp := time.Since(start)
		if !errors.Is(r.err, os.ErrDeadlineExceeded) {
			t.Fatalf("Recv = %v, want a deadline error", r.err)
		}
		// The dialing side pings at one interval; the accepting side a quarter later.
		if asServer {
			near(t, "the ping", pinged, interval+interval/4, interval+interval/4)
		} else {
			near(t, "the ping", pinged, interval, interval)
		}
		near(t, "giving up", gaveUp, 2*interval-interval/10, 2*interval)
		if err := tr.Send(bg, []byte("x")); !errors.Is(err, transport.ErrClosed) {
			t.Errorf("Send to a dead link = %v, want ErrClosed", err)
		}
	})
}

// The same when the peer does not even take the ping off the wire: the ping's
// own write may wait no longer than the patience that is left.
func TestWS_PeerThatStopsReadingAndWritingIsDetected(t *testing.T) {
	const interval = 200 * time.Millisecond
	sides(t, func(t *testing.T, asServer bool) {
		tr, _ := rawPipe(t, asServer, transport.WSConfig{PingInterval: interval}) // WriteTimeout stays at its 10 s default
		start := time.Now()
		_, err := tr.Recv(bg)
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Recv = %v, want a deadline error", err)
		}
		near(t, "giving up", time.Since(start), 2*interval-interval/10, 2*interval)
	})
}

// Anything the peer sends counts as a sign of life: a pong, a ping of its own,
// or data. A link whose peer answers stays up for as long as it does.
func TestWS_AnsweringPeerStaysAlive(t *testing.T) {
	const interval = 250 * time.Millisecond
	sides(t, func(t *testing.T, asServer bool) {
		for _, answer := range []ws.OpCode{ws.OpPong, ws.OpPing, ws.OpBinary} {
			t.Run(fmt.Sprintf("answers with opcode %d", answer), func(t *testing.T) {
				tr, b := rawPipe(t, asServer, transport.WSConfig{PingInterval: interval})
				p := &peer{t: t, conn: b, client: asServer}
				msgs := make(chan result, 64)
				go func() {
					for {
						msg, err := tr.Recv(bg)
						msgs <- result{msg, err}
						if err != nil {
							return
						}
					}
				}()
				// Answer three pings in a row: the link outlives its two-interval limit.
				pings := 0
				for pings < 3 {
					_ = b.SetReadDeadline(time.Now().Add(waitLong))
					f, err := ws.ReadFrame(b)
					if err != nil {
						t.Fatalf("after %d pings the link ended: %v", pings, err)
					}
					if f.Header.OpCode != ws.OpPing {
						continue // the transport's pong to our own ping
					}
					pings++
					if err := p.write(p.frame(answer, true, nil)); err != nil {
						t.Fatalf("answering ping %d: %v", pings, err)
					}
				}
				select {
				case r := <-msgs:
					if r.err != nil {
						t.Fatalf("Recv ended on a live link: %v", r.err)
					}
				default:
				}
			})
		}
	})
}

// A peer that stops reading cannot hold a sender past the write deadline.
func TestWS_WriteDeadline(t *testing.T) {
	const patience = 150 * time.Millisecond
	t.Run("over a pipe", func(t *testing.T) {
		sides(t, func(t *testing.T, asServer bool) {
			tr, _ := rawPipe(t, asServer, transport.WSConfig{WriteTimeout: patience})
			start := time.Now()
			err := tr.Send(bg, []byte("anyone?"))
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("Send = %v, want a deadline error", err)
			}
			near(t, "the write deadline", time.Since(start), patience-patience/10, patience)
			if err := tr.Send(bg, []byte("x")); !errors.Is(err, transport.ErrClosed) {
				t.Errorf("Send after a write timed out = %v, want ErrClosed", err)
			}
			// The reader learns why the link went down, not just that it did.
			if _, err := tr.Recv(bg); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("Recv after a write timed out = %v, want the write's deadline error", err)
			}
		})
	})
	// A real socket has buffers: sends succeed until they fill, then one waits.
	t.Run("over TCP", func(t *testing.T) {
		s := newWSServer(t, transport.WSConfig{}, false)
		client, _ := s.link(t, transport.WSConfig{WriteTimeout: patience, MaxFrameSize: 1 << 20}, nil)
		msg := make([]byte, 1<<20)
		var err error
		var took time.Duration
		for i := 0; i < 4096 && err == nil; i++ { // the server never calls Recv
			start := time.Now()
			err = client.Send(bg, msg)
			took = time.Since(start)
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Send to a peer that never reads = %v, want a deadline error", err)
		}
		// The deadline is per piece written, so the last Send may have got a few
		// pieces out before it stalled; it still ends within a few of them.
		if took > 5*time.Second {
			t.Errorf("the stalled Send took %v", took)
		}
	})
}

// The write deadline measures a stalled peer, not a large frame: a peer that
// keeps reading, however slowly, is never cut off for being slow.
func TestWS_SlowReaderIsNotAStalledReader(t *testing.T) {
	const patience = 400 * time.Millisecond
	tr, b := rawPipe(t, false, transport.WSConfig{WriteTimeout: patience, PingInterval: time.Hour})
	msg := bytes.Repeat([]byte("s"), 300<<10) // ten pieces
	go func() {
		// Take a piece, rest for a good part of the deadline, take the next.
		buf := make([]byte, 64<<10)
		for {
			if _, err := b.Read(buf); err != nil {
				return
			}
			time.Sleep(patience / 8)
		}
	}()
	start := time.Now()
	if err := tr.Send(bg, msg); err != nil {
		t.Fatalf("Send to a slow reader: %v", err)
	}
	if took := time.Since(start); took < patience {
		t.Skipf("the slow reader finished in %v, inside one deadline: nothing was shown", took)
	}
}

// While this end is busy sending one long frame, a peer with nothing to say is
// not given up on: the frame it is reading is why it is quiet.
func TestWS_LongSendDoesNotLookLikeADeadPeer(t *testing.T) {
	const interval = 100 * time.Millisecond
	tr, b := rawPipe(t, false, transport.WSConfig{PingInterval: interval, WriteTimeout: 2 * time.Second})
	got := recvAsync(bg, tr)
	go func() {
		// Read the frame slowly enough that it takes about five intervals.
		buf := make([]byte, 64<<10)
		for {
			if _, err := b.Read(buf); err != nil {
				return
			}
			time.Sleep(interval / 2)
		}
	}()
	if err := tr.Send(bg, make([]byte, 320<<10)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case r := <-got:
		t.Fatalf("Recv gave up on a peer that was reading all along: %v", r.err)
	default:
	}
}

// Two transports facing each other, idle: the dialing side pings once per
// interval, the accepting side answers, and nothing else is written. That is
// the whole idle cost of a link: one small write per end per interval.
func TestWS_IdleLinkCost(t *testing.T) {
	const interval = 400 * time.Millisecond
	const intervals = 4
	for _, useTLS := range []bool{false, true} {
		t.Run(map[bool]string{false: "tcp", true: "tls"}[useTLS], func(t *testing.T) {
			cfg := transport.WSConfig{PingInterval: interval}
			s := newWSServer(t, cfg, useTLS)
			var open atomic.Int32
			clientConn := s.dial(t, &open).(*trackConn)
			client, err := transport.UpgradeWSClient(bg, clientConn, "ws://"+s.addr()+"/link", cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			server := await(t, s.accepted, "the upgrade")
			defer server.Close()
			s.ln.mu.Lock()
			serverConn := s.ln.last
			s.ln.mu.Unlock()

			before := runtime.NumGoroutine()
			fromServer, fromClient := recvAsync(bg, client), recvAsync(bg, server)
			time.Sleep(20 * time.Millisecond)
			// The transport starts no goroutine: the two Recv calls are all there is.
			if n := runtime.NumGoroutine() - before; n > 2 {
				t.Errorf("an idle link holds %d goroutines beyond the two callers of Recv", n-2)
			}
			time.Sleep(intervals*interval + interval/2)

			// Still up after several times the two-interval limit, both ways.
			if err := client.Send(bg, []byte("to the server")); err != nil {
				t.Fatalf("Send from the dialing side after idling: %v", err)
			}
			if r := await(t, fromClient, "the server's Recv"); r.err != nil || string(r.msg) != "to the server" {
				t.Fatalf("the accepting side's Recv = %q, %v", r.msg, r.err)
			}
			if err := server.Send(bg, []byte("to the client")); err != nil {
				t.Fatalf("Send from the accepting side after idling: %v", err)
			}
			if r := await(t, fromServer, "the client's Recv"); r.err != nil || string(r.msg) != "to the client" {
				t.Fatalf("the dialing side's Recv = %q, %v", r.msg, r.err)
			}
			if useTLS {
				// Under TLS the socket carries records, not frames, so the frames
				// are counted on TCP. What TLS adds to the test is a connection
				// whose reads timed out again and again and still works.
				return
			}

			const ping, pong, binary = 0x89, 0x8a, 0x82
			c, cSize := clientConn.tally()
			sv, svSize := serverConn.tally()
			// Each end's first write was its half of the handshake ('G', 'H'); drop it.
			delete(c, 'G')
			delete(sv, 'H')
			t.Logf("idle for %d intervals: dialing side wrote %d pings, accepting side %d pongs", intervals, c[ping], sv[pong])
			if c[ping] < intervals-2 || c[ping] > intervals+1 {
				t.Errorf("the dialing side sent %d pings in %d intervals", c[ping], intervals)
			}
			if sv[pong] != c[ping] {
				t.Errorf("the accepting side answered %d of %d pings", sv[pong], c[ping])
			}
			// The accepting side pings only if the dialing side's ping is a quarter
			// of an interval late. On a quiet link that is never; a machine that
			// stalls the test for 100 ms may make it once.
			if sv[ping] > 1 || c[pong] != sv[ping] {
				t.Errorf("the accepting side pinged %d times and was answered %d times", sv[ping], c[pong])
			}
			// Nothing else was written: the one message each way, and keepalives.
			delete(c, pong)
			delete(sv, ping)
			if c[binary] != 1 || sv[binary] != 1 || len(c) != 2 || len(sv) != 2 {
				t.Errorf("unexpected writes: dialing side %v, accepting side %v", c, sv)
			}
			// And each of those writes is as small as a frame gets: a masked
			// header from the dialing side, a bare one from the accepting side.
			if cSize[ping] != 6 || svSize[pong] != 2 {
				t.Errorf("a ping is %d bytes on the wire and a pong %d; want 6 and 2", cSize[ping], svSize[pong])
			}
		})
	}
}

// A peer that pings and then stops reading is owed an answer for no longer
// than the write timeout.
func TestWS_PingFromAPeerThatNeverReads(t *testing.T) {
	const patience = 150 * time.Millisecond
	sides(t, func(t *testing.T, asServer bool) {
		tr, far := rawPipe(t, asServer, transport.WSConfig{WriteTimeout: patience})
		got := recvAsync(bg, tr)
		p := &peer{t: t, conn: far, client: asServer}
		start := time.Now()
		if err := p.write(p.frame(ws.OpPing, true, []byte("answer me"))); err != nil {
			t.Fatal(err)
		}
		if r := await(t, got, "Recv"); !errors.Is(r.err, os.ErrDeadlineExceeded) {
			t.Fatalf("Recv = %v, want a deadline error", r.err)
		}
		near(t, "giving up on the pong", time.Since(start), patience-patience/10, patience)
	})
}

// A context that ends between two pieces of a frame leaves half a frame on the
// wire. Nothing can follow that, so the link ends, and the reader is told why.
func TestWS_ContextEndingInsideAFrameEndsTheLink(t *testing.T) {
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(bg)
	tr := rawPipeOn(t, cancelConn{a, cancel}, b, false, transport.WSConfig{})
	go func() { _, _ = io.Copy(io.Discard, b) }()
	if err := tr.Send(ctx, make([]byte, 100<<10)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Send = %v, want context.Canceled", err)
	}
	if err := tr.Send(bg, []byte("x")); !errors.Is(err, transport.ErrClosed) {
		t.Errorf("Send afterwards = %v, want ErrClosed", err)
	}
	_, err := tr.Recv(bg)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "partway through a frame") {
		t.Errorf("Recv afterwards = %v, want the reason the link ended", err)
	}
}

// cancelConn ends a context as soon as one write has gone through.
type cancelConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c cancelConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.cancel()
	return n, err
}

// A response whose connection cannot be taken over (HTTP/2, or a wrapper that
// hides it) is answered with a 500 and an error.
func TestWSUpgradeServer_NeedsAConnectionItCanTakeOver(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example/link", nil)
	if _, err := transport.UpgradeWSServer(rec, req, transport.WSConfig{}); !errors.Is(err, ws.ErrNotHijacker) {
		t.Fatalf("UpgradeWSServer = %v, want %v", err, ws.ErrNotHijacker)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// A connection whose reads can report data and a timeout together (crypto/tls
// does, when the deadline passes while it reads a trailing alert) must have
// the data believed and the timeout forgotten.
func TestWS_DataThatArrivesWithATimeoutCounts(t *testing.T) {
	a, b := net.Pipe()
	tr := rawPipeOn(t, lateConn{a}, b, true, transport.WSConfig{})
	got := recvAsync(bg, tr)
	p := &peer{t: t, conn: b, client: true}
	if err := p.write(p.frame(ws.OpBinary, true, []byte("in time"))); err != nil {
		t.Fatal(err)
	}
	if r := await(t, got, "Recv"); r.err != nil || string(r.msg) != "in time" {
		t.Fatalf("Recv = %q, %v", r.msg, r.err)
	}
}

// lateConn reports a timeout along with every successful read.
type lateConn struct{ net.Conn }

func (c lateConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && err == nil {
		err = os.ErrDeadlineExceeded
	}
	return n, err
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
