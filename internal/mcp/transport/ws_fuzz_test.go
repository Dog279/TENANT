package transport_test

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"runtime"
	"testing"

	"github.com/gobwas/ws"

	"tenant/internal/mcp/transport"
)

// A hostile peer writes whatever bytes it likes. Whatever they are, Recv must
// not panic, must come to an end, must never return a message over the limit,
// and must not allocate in proportion to what a frame merely claims.

const fuzzMax = 1024 // the frame limit under fuzz: small, so that a claim over it is cheap to make

// fuzzSeeds are the frames a peer may send and the ones it may not, as bytes.
// masked says which end wrote them.
func fuzzSeeds(masked bool) [][]byte {
	fr := func(op ws.OpCode, fin bool, p string) []byte {
		return encode(ws.NewFrame(op, fin, []byte(p)), masked)
	}
	hdr := func(h ws.Header) []byte {
		h.Masked = masked
		return header(h)
	}
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	msb := header(ws.Header{Fin: true, OpCode: ws.OpBinary, Length: 1 << 62})
	msb[2] |= 0x80
	return [][]byte{
		nil,
		fr(ws.OpBinary, true, `{"jsonrpc":"2.0","id":1,"method":"ping"}`),
		fr(ws.OpBinary, true, ""),
		cat(fr(ws.OpBinary, false, "a"), fr(ws.OpContinuation, false, "b"), fr(ws.OpContinuation, true, "c")),
		cat(fr(ws.OpBinary, false, "a"), fr(ws.OpPing, true, "p"), fr(ws.OpContinuation, true, "c")),
		cat(fr(ws.OpPing, true, "are you there"), fr(ws.OpPong, true, ""), fr(ws.OpBinary, true, "x")),
		fr(ws.OpClose, true, string(ws.NewCloseFrameBody(ws.StatusNormalClosure, "bye"))),
		fr(ws.OpClose, true, string(ws.NewCloseFrameBody(ws.StatusMessageTooBig, ""))),
		fr(ws.OpClose, true, "\x03"),
		cat(fr(ws.OpClose, true, ""), fr(ws.OpBinary, true, "after the close")),
		fr(ws.OpText, true, "text"),
		fr(ws.OpContinuation, true, "orphan"),
		cat(fr(ws.OpBinary, false, "a"), fr(ws.OpBinary, true, "b")),
		fr(0x3, true, "reserved"),
		fr(0xb, true, "reserved control"),
		fr(ws.OpBinary, true, string(bytes.Repeat([]byte("m"), fuzzMax))),
		hdr(ws.Header{Fin: true, OpCode: ws.OpBinary, Length: fuzzMax + 1}),
		hdr(ws.Header{Fin: true, OpCode: ws.OpBinary, Length: 65535}),
		hdr(ws.Header{Fin: true, OpCode: ws.OpBinary, Length: 1 << 31}),
		hdr(ws.Header{Fin: true, OpCode: ws.OpBinary, Length: 1<<63 - 1}),
		cat(fr(ws.OpBinary, false, string(bytes.Repeat([]byte("m"), fuzzMax))), hdr(ws.Header{Fin: true, OpCode: ws.OpContinuation, Length: 1})),
		hdr(ws.Header{Fin: true, OpCode: ws.OpPing, Length: 126}),
		hdr(ws.Header{OpCode: ws.OpPing}),
		hdr(ws.Header{Fin: true, Rsv: ws.Rsv(true, true, true), OpCode: ws.OpBinary}),
		msb,
		encode(ws.NewBinaryFrame([]byte("wrong masking")), !masked),
		fr(ws.OpBinary, true, "cut short")[:5],
		{0x82},
		{0x82, 0x7e, 0x00},
		{0x82, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		[]byte("GET /link HTTP/1.1\r\nHost: example\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"),
		[]byte("GET /link HTTP/1.1\r\nHost: example\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
			"Sec-WebSocket-Extensions: permessage-deflate\r\nSec-WebSocket-Protocol: a, b\r\n\r\n\x82\x00"),
		[]byte("GET / HTTP/1.1\r\nHost: example\r\n\r\n"),
		[]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n\x82\x02hi"),
		[]byte("HTTP/1.1 101 Switching Protocols\r\nSec-WebSocket-Extensions: permessage-deflate; a=\"b\r\n"),
		[]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"),
		append([]byte("HTTP/1.1 101 "), bytes.Repeat([]byte("a"), 9000)...),
	}
}

// drain calls Recv on tr until it fails and checks every promise on the way.
// script is everything the peer will ever send, so its length bounds the work.
func drain(t *testing.T, tr *transport.WS, script []byte) {
	t.Helper()
	// Every message costs the peer at least a two-byte header.
	for n := 0; ; n++ {
		if n > len(script) {
			t.Fatalf("Recv returned %d messages from %d bytes: it is not consuming its input", n, len(script))
		}
		msg, err := tr.Recv(bg)
		if err != nil {
			break
		}
		if len(msg) > fuzzMax {
			t.Fatalf("Recv returned a %d-byte message; the limit is %d", len(msg), fuzzMax)
		}
		if msg == nil {
			t.Fatal("Recv returned a nil message and no error")
		}
	}
	// The end is final.
	if _, err := tr.Recv(bg); err == nil {
		t.Fatal("Recv worked again after it had failed")
	}
	_ = tr.Close()
}

// ceiling fails the test if run allocated more than a hostile script of that
// size can be allowed to cost: a fixed amount for the transport itself, a
// little per byte sent (the worst a peer can do per byte is a stream of pings,
// each answered), and a few times the frame limit for one message on its way
// to that limit. A frame's claimed length appears nowhere in it.
func ceiling(t *testing.T, script []byte, run func()) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	run()
	runtime.ReadMemStats(&after)
	limit := uint64(32<<10 + 512*len(script) + 8*fuzzMax)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > limit {
		t.Fatalf("%d bytes from the peer made the transport allocate %d bytes; the ceiling is %d", len(script), grew, limit)
	}
}

// FuzzWSAccepting plays a hostile dialing peer against the accepting side:
// first as frames on an upgraded connection, then as the HTTP request that
// asks for the upgrade and whatever follows it.
func FuzzWSAccepting(f *testing.F) {
	for _, seed := range fuzzSeeds(true) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, script []byte) {
		ceiling(t, script, func() {
			drain(t, transport.NewWSServer(newScriptConn(script), transport.WSConfig{MaxFrameSize: fuzzMax}), script)
		})

		br := bufio.NewReader(bytes.NewReader(script))
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		rest, _ := io.ReadAll(br)
		conn := newScriptConn(nil)
		tr, err := transport.UpgradeWSServer(&hijackable{conn: conn, buffered: rest}, req, transport.WSConfig{MaxFrameSize: fuzzMax})
		if err != nil {
			return
		}
		if len(rest) > 0 {
			t.Fatalf("the upgrade succeeded with %d bytes sent ahead of the answer", len(rest))
		}
		drain(t, tr, nil)
	})
}

// FuzzWSDialing plays a hostile accepting peer against the dialing side: first
// as frames on an upgraded connection, then as the answer to the upgrade
// request and whatever follows it.
func FuzzWSDialing(f *testing.F) {
	for _, seed := range fuzzSeeds(false) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, script []byte) {
		ceiling(t, script, func() {
			drain(t, transport.NewWSClient(newScriptConn(script), transport.WSConfig{MaxFrameSize: fuzzMax}), script)
		})

		conn := newScriptConn(script)
		tr, err := transport.UpgradeWSClient(bg, conn, "ws://example/link", transport.WSConfig{MaxFrameSize: fuzzMax})
		if conn.read > 4096 && err != nil {
			t.Fatalf("the dialing side read %d bytes of a handshake answer it refused; the limit is 4096", conn.read)
		}
		if err != nil {
			return
		}
		// Reaching here takes an answer with the right accept value for a key
		// the dialing side chose at random, so it does not happen; if it ever
		// does, the frames after the answer get the same treatment.
		drain(t, tr, script)
	})
}

// hijackable is an http.ResponseWriter whose connection can be taken over, as
// a real server's can. buffered is what the server had already read past the
// request.
type hijackable struct {
	conn     net.Conn
	buffered []byte
	header   http.Header
}

func (h *hijackable) Header() http.Header {
	if h.header == nil {
		h.header = http.Header{}
	}
	return h.header
}
func (h *hijackable) Write(p []byte) (int, error) { return len(p), nil }
func (h *hijackable) WriteHeader(int)             {}
func (h *hijackable) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	br := bufio.NewReader(io.MultiReader(bytes.NewReader(h.buffered), h.conn))
	_, _ = br.Peek(1) // a real server's reader has these bytes buffered already
	return h.conn, bufio.NewReadWriter(br, bufio.NewWriter(h.conn)), nil
}
