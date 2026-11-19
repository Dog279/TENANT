package fleet

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gobwas/ws"

	"tenant/internal/mcp"
	"tenant/internal/mcp/transport"
	"tenant/internal/model"
)

// Everything a child sends is untrusted. These tests are a child that says
// whatever it likes, with a real pinned certificate, beside an honest one:
// none of it may panic the master, leave a goroutine or a session behind,
// hold up the honest child's calls, or get anything approved.

// rawChild is a child that speaks the link by hand.
type rawChild struct {
	t    *testing.T
	conn net.Conn // under the WebSocket, for bytes that are not even frames
	ws   *transport.WS
	in   chan mcp.Message // what the master sent and the reader did not answer
	done chan struct{}    // the reader ended: the link is over
	err  error            // why, once done is closed
	stop context.CancelFunc
	asks sync.Map // request id -> chan mcp.Message: where ask waits for its answer
}

// dialRaw connects to the master as id and asks for the link. answer, when
// it is not nil, gets every message the master sends first, and says whether
// it dealt with it; a nil answer says hello as an honest child would.
func (r *rig) dialRaw(t *testing.T, id Identity, answer func(c *rawChild, msg mcp.Message) bool) (*rawChild, error) {
	t.Helper()
	tcp, err := net.DialTimeout("tcp", r.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn := tls.Client(newCounted(tcp, &r.dialed, nil), must(ChildTLS(id, r.id.Fingerprint))(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	link, err := transport.UpgradeWSClient(ctx, conn, "wss://"+r.addr+LinkPath, transport.WSConfig{PingInterval: testPing})
	if err != nil {
		return nil, err
	}
	c := &rawChild{t: t, conn: conn, ws: link, in: make(chan mcp.Message, 4096), done: make(chan struct{})}
	rctx, stop := context.WithCancel(context.Background())
	c.stop = stop
	go func() {
		defer close(c.done)
		for {
			raw, err := link.Recv(rctx)
			if err != nil {
				c.err = err
				return
			}
			var msg mcp.Message
			if json.Unmarshal(raw, &msg) != nil {
				continue
			}
			switch {
			case answer != nil && answer(c, msg):
			case answer == nil && msg.Method == methodHello:
				c.reply(msg, `{"link":1,"os":"linux","ceiling":"full"}`)
			case msg.Method == methodAddresses:
				c.reply(msg, `{}`)
			case msg.IsResponse():
				if ch, ok := c.asks.LoadAndDelete(string(msg.ID.Raw())); ok {
					ch.(chan mcp.Message) <- msg
				}
			default:
				select {
				case c.in <- msg:
				default: // a flood's answers: nobody reads them
				}
			}
		}
	}()
	t.Cleanup(func() {
		stop()
		_ = link.Close()
		<-c.done
	})
	return c, nil
}

// raw is dialRaw for a machine that must get its link.
func (r *rig) raw(t *testing.T, name string) (*rawChild, string) {
	t.Helper()
	mach, id := r.add(t, name, StateConfirmed)
	c, err := r.dialRaw(t, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.awaitOnline(t, mach.ID, true)
	return c, mach.ID
}

// send writes one frame; an error means the master has ended the link.
func (c *rawChild) send(frame string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.ws.Send(ctx, []byte(frame))
}

func (c *rawChild) reply(to mcp.Message, result string) {
	_ = c.send(`{"jsonrpc":"2.0","id":` + string(to.ID.Raw()) + `,"result":` + result + `}`)
}

// ask sends a request and returns the master's answer to it.
func (c *rawChild) ask(id int, method, params string) mcp.Message {
	c.t.Helper()
	frame := `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":` + strconv.Quote(method)
	if params != "" {
		frame += `,"params":` + params
	}
	answer := make(chan mcp.Message, 1)
	c.asks.Store(strconv.Itoa(id), answer)
	if err := c.send(frame + "}"); err != nil {
		c.t.Errorf("send %s: %v", method, err)
		return mcp.Message{}
	}
	select {
	case msg := <-answer:
		return msg
	case <-c.done:
		c.t.Errorf("the master ended the link instead of answering %s: %v", method, c.err)
	case <-time.After(waitLong):
		c.t.Errorf("the master did not answer %s", method)
	}
	return mcp.Message{}
}

// next is the next message from the master.
func (c *rawChild) next() mcp.Message {
	c.t.Helper()
	select {
	case msg := <-c.in:
		return msg
	case <-c.done:
		c.t.Fatalf("the master ended the link: %v", c.err)
	case <-time.After(waitLong):
		c.t.Fatal("nothing came from the master")
	}
	return mcp.Message{}
}

// ended waits for the master to end the link.
func (c *rawChild) ended() {
	c.t.Helper()
	select {
	case <-c.done:
	case <-time.After(waitLong):
		c.t.Fatal("the master did not end the link")
	}
}

// hostile is a master with an honest child, and a count of what was
// approved for anyone.
type hostile struct {
	*rig
	honest   *kid
	approved atomic.Int64
}

func newHostile(t *testing.T, tweak ...func(*Manager)) *hostile {
	t.Helper()
	h := &hostile{rig: newRig(t, tweak...)}
	h.honest = h.up(t, "honest")
	return h
}

// approve is a Confirm that counts and says yes: what must never be reached.
func (h *hostile) approve(context.Context, string, string) bool {
	h.approved.Add(1)
	return true
}

// intact checks what no hostile child may change: the honest child's calls
// go through, and nothing was approved.
func (h *hostile) intact(t *testing.T) {
	t.Helper()
	h.echo(t, h.honest.mach.ID)
	if n := h.approved.Load(); n != 0 {
		t.Errorf("%d approvals were given", n)
	}
}

// --- before the upgrade ---

// Who gets a link, and what everyone else is told instead.
func TestLinkEndpointRefusals(t *testing.T) {
	h := newHostile(t)
	// get asks for the link the way a child does and returns the answer of a
	// master that does not upgrade.
	get := func(cfg *tls.Config, path string, upgrade bool) (int, string, error) {
		client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}}
		req := must(http.NewRequest("GET", "https://"+h.addr+path, nil))(t)
		if upgrade {
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return resp.StatusCode, string(body), nil
	}
	as := func(id Identity) *tls.Config { return must(ChildTLS(id, h.id.Fingerprint))(t) }

	_, pendingID := h.add(t, "pending", StatePending)
	_, pausedID := h.add(t, "paused", StatePaused)
	_, revokedID := h.add(t, "revoked", StateRevoked)
	_, confirmedID := h.add(t, "confirmed", StateConfirmed)
	stranger := newIdentity(t)

	// A pending machine: "waiting for confirmation" and nothing else.
	if status, body, err := get(as(pendingID), LinkPath, true); err != nil || status != 425 || body != WaitingForConfirmation+"\n" {
		t.Errorf("pending: %d %q %v", status, body, err)
	}
	if status, body, err := get(as(pausedID), LinkPath, true); err != nil || status != 423 || body != "paused\n" {
		t.Errorf("paused: %d %q %v", status, body, err)
	}
	// A revoked certificate and one that was never pinned do not get as far
	// as HTTP: the handshake drops them.
	for name, id := range map[string]Identity{"revoked": revokedID, "stranger": stranger} {
		if status, _, err := get(as(id), LinkPath, true); err == nil {
			t.Errorf("%s: got an HTTP answer, %d", name, status)
		}
	}
	// Through the enroll endpoint's handshake, which lets any certificate in,
	// the link is still not to be had.
	enroll := must(childTLS(stranger, h.id.Fingerprint, enrollServerName))(t)
	if status, _, err := get(enroll, LinkPath, true); err != nil || status != 403 {
		t.Errorf("a stranger asking for the link through the enroll handshake: %d %v", status, err)
	}
	// A confirmed machine that does not ask for an upgrade gets none.
	if status, _, err := get(as(confirmedID), LinkPath, false); err != nil || status == 101 || status == 200 {
		t.Errorf("a plain GET of the link: %d %v", status, err)
	}
	if status, _, err := get(as(confirmedID), "/fleet/other", true); err != nil || status != 404 {
		t.Errorf("another path: %d %v", status, err)
	}
	for _, m := range must(h.mgr.Machines())(t) {
		if m.Online != (m.ID == h.honest.mach.ID) {
			t.Errorf("%s (%s) online = %v", m.Name, m.State, m.Online)
		}
	}

	// The handler's own answers, whatever the handshake let through: the
	// state can change between the two.
	serve := func(id *Identity) (int, string) {
		req := httptest.NewRequest("GET", LinkPath, nil)
		if id != nil {
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{id.Certificate.Leaf}}
		}
		rec := httptest.NewRecorder()
		h.mgr.serveLink(rec, req)
		return rec.Code, rec.Body.String()
	}
	for _, tc := range []struct {
		name   string
		id     *Identity
		status int
		body   string
	}{
		{"no certificate", nil, 403, "this machine is not part of the fleet\n"},
		{"stranger", &stranger, 403, "this machine is not part of the fleet\n"},
		{"pending", &pendingID, 425, "waiting for confirmation\n"},
		{"paused", &pausedID, 423, "paused\n"},
		{"revoked", &revokedID, 410, "revoked\n"},
	} {
		if status, body := serve(tc.id); status != tc.status || body != tc.body {
			t.Errorf("%s: %d %q, want %d %q", tc.name, status, body, tc.status, tc.body)
		}
	}
	// A confirmed machine over something that cannot be upgraded.
	if status, _ := serve(&confirmedID); status == 101 {
		t.Errorf("confirmed, not upgradable: %d", status)
	}
	good := must(os.ReadFile(StorePath(h.dir)))(t)
	writeRaw(t, StorePath(h.dir), []byte("{"))
	if status, body := serve(&confirmedID); status != 503 || body != "the fleet is not available\n" {
		t.Errorf("fleet.json unreadable: %d %q", status, body)
	}
	writeRaw(t, StorePath(h.dir), good)

	h.intact(t)

	// After Close nothing is upgraded.
	_ = h.mgr.Close()
	if status, _ := serve(&confirmedID); status == 101 {
		t.Error("a closed manager upgraded a connection")
	}
}

// A connection that dawdles before it is a link is cut off: one that says
// nothing at all, and one that proves a pinned certificate and then stops in
// the middle of its request.
func TestHostileSlowBeforeTheUpgrade(t *testing.T) {
	h := newHostile(t, func(m *Manager) { m.headerTime = 300 * time.Millisecond })
	_, id := h.add(t, "slow", StateConfirmed)

	cut := func(name string, conn net.Conn) {
		t.Helper()
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		start := time.Now()
		if _, err := io.Copy(io.Discard, conn); errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("%s: the master kept the connection for %v", name, time.Since(start))
		}
	}
	silent := must(net.Dial("tcp", h.addr))(t)
	cut("no handshake", silent)

	// The master's patience here is 300ms, and it applies to this test's own
	// handshake too. On a machine too busy for that the master has cut the
	// connection early, which is the thing under test: dial again, and do
	// not mind a write that finds the connection already gone.
	var conn *tls.Conn
	for deadline := time.Now().Add(waitLong); conn == nil; {
		c, err := tls.Dial("tcp", h.addr, must(ChildTLS(id, h.id.Fingerprint))(t))
		if err != nil && time.Now().After(deadline) {
			t.Fatal(err)
		}
		if err == nil {
			conn = c
		}
	}
	_, _ = conn.Write([]byte("GET " + LinkPath + " HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\n"))
	cut("half a request", conn)

	// Many at once take no more than their places, and those come back.
	var conns []net.Conn
	for range 40 {
		conns = append(conns, must(net.Dial("tcp", h.addr))(t))
	}
	h.intact(t) // an honest call goes through meanwhile
	for _, c := range conns {
		cut("one of many", c)
	}
	for deadline := time.Now().Add(waitLong); len(h.mgr.sem) != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d places before the upgrade were never given back", len(h.mgr.sem))
		}
	}
	h.intact(t)
}

// --- the hello ---

// A child whose hello is a lie, is broken, or never comes gets no link. One
// that claims a version from the future is taken at its word, and judged by
// what it then does.
func TestHostileHello(t *testing.T) {
	h := newHostile(t)
	for _, tc := range []struct{ name, result string }{
		{"a string for a version", `{"link":"1"}`},
		{"version zero", `{"link":0}`},
		{"a negative version", `{"link":-1}`},
		{"a version beyond any", `{"link":99999999999999999999}`},
		{"a version just past the limit", `{"link":1048577}`},
		{"a fraction", `{"link":1.5}`},
		{"no version", `{}`},
		{"an array", `[1]`},
		{"null", `null`},
		{"an error", ``},
		{"silence", `-`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := h
			if tc.result == "-" {
				// The one case that waits for the hello's time to run out has
				// a master of its own, which does not wait long. The others
				// answer at once and are not held to that.
				h = newHostile(t, func(m *Manager) { m.helloTime = 400 * time.Millisecond })
			}
			mach, id := h.add(t, "liar"+strconv.Itoa(len(must(h.store.Machines())(t))), StateConfirmed)
			c, err := h.dialRaw(t, id, func(c *rawChild, msg mcp.Message) bool {
				switch {
				case msg.Method != methodHello || tc.result == "-":
				case tc.result == "":
					_ = c.send(`{"jsonrpc":"2.0","id":` + string(msg.ID.Raw()) + `,"error":{"code":-32601,"message":"what hello"}}`)
				default:
					c.reply(msg, tc.result)
				}
				return true
			})
			if err != nil {
				t.Fatal(err)
			}
			c.ended()
			for deadline := time.Now().Add(waitLong); ; time.Sleep(5 * time.Millisecond) {
				m := h.status(t, mach.ID)
				if m.Online {
					t.Fatal("a machine without a hello is online")
				}
				if strings.Contains(m.LastError, "no hello") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("last error = %q", m.LastError)
				}
			}
			if _, err := h.call(t, mach.ID, "echo", `1`); !errors.Is(err, ErrOffline) {
				t.Errorf("call = %v", err)
			}
		})
	}
	t.Run("a version from the future, and things about itself that are not so", func(t *testing.T) {
		mach, id := h.add(t, "future", StateConfirmed)
		_, err := h.dialRaw(t, id, func(c *rawChild, msg mcp.Message) bool {
			if msg.Method == methodHello {
				c.reply(msg, `{"link":7,"os":"plan9","user":"\u0000root","version":"`+strings.Repeat("9", 500)+`","ceiling":"none"}`)
				return true
			}
			return false
		})
		if err != nil {
			t.Fatal(err)
		}
		m := h.awaitOnline(t, mach.ID, true)
		if m.Link != 7 || m.Reported != linuxBox {
			t.Errorf("listed as %+v: what it reported was not valid and must not have been kept", m)
		}
	})
	h.intact(t)
}

// --- frames the link does not have ---

// Bytes that are not JSON-RPC, notifications, results nobody asked for and
// requests for what only a master may call: each is dropped or refused, and
// a child that keeps at it loses the link.
func TestHostileFramesAreStrikes(t *testing.T) {
	h := newHostile(t)
	junk := []string{
		`not json`, ``, `[]`, `7`, `"x"`, `null`, `{}`, `{"jsonrpc":"2.0"}`, `{"method":5,"id":1}`,
		strings.Repeat("[", 20000),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"fleet/revoked"}`,
		`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":1,"progress":1}}`,
		`{"jsonrpc":"2.0","id":991,"result":{"text":"a result for a call never made"}}`,
		`{"jsonrpc":"2.0","id":"x","error":{"code":1,"message":"an error for a call never made"}}`,
	}
	if len(junk) >= maxStrikes {
		t.Fatal("the junk list is meant to stay under the strike limit")
	}
	c, id := h.raw(t, "bad")
	for _, frame := range junk {
		if err := c.send(frame); err != nil {
			t.Fatalf("send %q: %v", frame, err)
		}
	}
	// Under the limit the link stands, and still does what it is for.
	if got := c.ask(1, "ping", ""); got.Error != nil || string(got.Result) != `{}` {
		t.Errorf("ping after junk = %+v", got)
	}
	if m := h.status(t, id); !m.Online {
		t.Fatalf("the link ended after %d strikes: %s", len(junk), m.LastError)
	}
	h.intact(t)
	for range maxStrikes {
		if c.send(`junk`) != nil {
			break
		}
	}
	c.ended()
	if m := h.awaitOnline(t, id, false); !strings.Contains(m.LastError, "kept sending what the link does not allow") {
		t.Errorf("last error = %q", m.LastError)
	}
	h.intact(t)
}

// A child asks for what only a master may call: every one is refused by
// name, nothing runs, and each counts against it.
func TestHostileRequestsForTheMastersMethods(t *testing.T) {
	h := newHostile(t)
	c, id := h.raw(t, "bad")
	methods := []string{methodToolsCall, methodToolsList, methodResult, methodUpdate, methodHello, methodAddresses, noticeRevoked,
		mcp.MethodInitialize, "resources/list", "fleet/confirm2", "FLEET/CONFIRM", "x"}
	if len(methods) >= maxStrikes {
		t.Fatal("the list is meant to stay under the strike limit")
	}
	for i, method := range methods {
		got := c.ask(100+i, method, `{"call_id":"abc","name":"echo","action":"os_exec","addresses":["10.0.0.1:1"]}`)
		if got.Error == nil || got.Error.Code != mcp.ErrMethodNotFound || len(got.Result) != 0 {
			t.Errorf("%s was answered %+v, want method not found", method, got)
		}
	}
	if m := h.status(t, id); !m.Online {
		t.Fatal("the link ended early")
	}
	// It did not change the master's addresses, or anything else.
	if addrs := must(h.store.Addresses())(t); len(addrs) != 1 || addrs[0] != h.addr {
		t.Errorf("addresses = %v", addrs)
	}
	for i := range maxStrikes {
		if c.send(`{"jsonrpc":"2.0","id":`+strconv.Itoa(500+i)+`,"method":"tools/call"}`) != nil {
			break
		}
	}
	c.ended()
	h.awaitOnline(t, id, false)
	h.intact(t)
}

// A flood of requests the link does allow is paid for from a bucket: beyond
// it they are strikes, the link ends, and the master's goroutines never
// number more than the bucket lets them.
func TestHostileFlood(t *testing.T) {
	for name, frame := range map[string]string{
		"pings":         `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		"confirms":      `{"jsonrpc":"2.0","id":1,"method":"fleet/confirm","params":{"call_id":"abc","action":"os_exec","detail":"rm -rf /"}}`,
		"cancellations": `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHostile(t)
			c, id := h.raw(t, "bad")
			base := runtime.NumGoroutine()
			var peak atomic.Int64
			stop := make(chan struct{})
			sampled := make(chan struct{})
			go func() {
				defer close(sampled)
				for {
					select {
					case <-stop:
						return
					default:
						if n := int64(runtime.NumGoroutine()); n > peak.Load() {
							peak.Store(n)
						}
						time.Sleep(200 * time.Microsecond)
					}
				}
			}()
			// The honest child is called all the while.
			calls := make(chan error, 1)
			go func() {
				var err error
				for i := 0; i < 20 && err == nil; i++ {
					_, err = h.call(t, h.honest.mach.ID, "echo", `1`)
				}
				calls <- err
			}()
			sent := 0
			for start := time.Now(); c.send(frame) == nil; sent++ {
				if time.Since(start) > waitLong {
					t.Fatalf("the master was still taking the flood after %d frames", sent)
				}
			}
			c.ended()
			close(stop)
			<-sampled
			if err := <-calls; err != nil {
				t.Errorf("the honest child's calls during the flood: %v", err)
			}
			if sent < requestBurst {
				t.Errorf("%d frames were taken before the link ended; the bucket holds %d", sent, requestBurst)
			}
			// The sampler, the caller and the honest child's handlers are in the count too.
			if grew := peak.Load() - int64(base); grew > requestBurst+32 {
				t.Errorf("goroutines grew by %d during the flood; the bucket allows %d", grew, requestBurst)
			}
			if m := h.awaitOnline(t, id, false); !strings.Contains(m.LastError, "more requests than the link allows") {
				t.Errorf("last error = %q", m.LastError)
			}
			h.intact(t)
		})
	}
}

// A frame larger than the link takes ends the link, from its header alone.
func TestHostileGiantFrame(t *testing.T) {
	h := newHostile(t)
	c, id := h.raw(t, "bad")
	var hdr bytes.Buffer
	if err := ws.WriteHeader(&hdr, ws.Header{Fin: true, OpCode: ws.OpBinary, Masked: true, Length: transport.DefaultMaxFrameSize + 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.conn.Write(hdr.Bytes()); err != nil {
		t.Fatal(err)
	}
	c.ended()
	if m := h.awaitOnline(t, id, false); !strings.Contains(m.LastError, "frame exceeds size limit") {
		t.Errorf("last error = %q", m.LastError)
	}
	h.intact(t)
}

// A child that starts a frame and never finishes it, saying nothing more,
// is gone within two ping intervals like any silent one.
func TestHostileStallsInsideAFrame(t *testing.T) {
	h := newHostile(t)
	c, id := h.raw(t, "bad")
	c.stop() // it no longer reads, so it no longer answers pings
	<-c.done
	var half bytes.Buffer
	_ = ws.WriteHeader(&half, ws.Header{Fin: true, OpCode: ws.OpBinary, Masked: true, Length: 1 << 20})
	half.WriteString(`{"jsonrpc":"2.0","id":1,"me`)
	start := time.Now()
	if _, err := c.conn.Write(half.Bytes()); err != nil {
		t.Fatal(err)
	}
	h.intact(t) // while it stalls
	m := h.awaitOnline(t, id, false)
	if took := time.Since(start); took > 2*testPing+2*time.Second || !strings.Contains(m.LastError, "no data from the peer") {
		t.Errorf("ended after %v because: %s", took, m.LastError)
	}
	h.intact(t)
}

// The same machine connecting over and over holds one link, the newest, and
// leaves nothing of the others behind.
func TestHostileReconnects(t *testing.T) {
	h := newHostile(t)
	mach, id := h.add(t, "bad", StateConfirmed)
	var last *rawChild
	var held *link
	for range 30 {
		c, err := h.dialRaw(t, id, nil)
		if err != nil {
			t.Fatal(err)
		}
		last, held = c, h.awaitNewLink(t, mach.ID, held)
	}
	if got := last.ask(1, "ping", ""); got.Error != nil {
		t.Errorf("the newest link: %+v", got)
	}
	h.mgr.mu.Lock()
	n := len(h.mgr.links)
	h.mgr.mu.Unlock()
	if n != 2 {
		t.Errorf("%d links are held for 2 machines", n)
	}
	h.intact(t)
}

// --- answers to the master's calls ---

// What a child can answer a call with, other than the truth.
func TestHostileResults(t *testing.T) {
	h := newHostile(t)
	c, id := h.raw(t, "bad")
	// answer runs one call and lets the child send frames built from the
	// request's id; it returns what the caller got.
	answer := func(timeout time.Duration, frames func(id string) []string) (Result, error) {
		t.Helper()
		type out struct {
			res Result
			err error
		}
		done := make(chan out, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			res, err := h.mgr.Call(ctx, id, Call{Tool: "echo", Arguments: json.RawMessage(`{}`), Confirm: h.approve})
			done <- out{res, err}
		}()
		var req mcp.Message
		for req = c.next(); req.Method != methodToolsCall; req = c.next() {
		}
		for _, frame := range frames(string(req.ID.Raw())) {
			_ = c.send(frame)
		}
		o := <-done
		return o.res, o.err
	}
	result := func(id, result string) string { return `{"jsonrpc":"2.0","id":` + id + `,"result":` + result + `}` }

	// The right id, twice: the first is the answer, the second is nothing.
	res, err := answer(waitLong, func(id string) []string {
		return []string{result(id, `{"text":"one"}`), result(id, `{"text":"two"}`)}
	})
	if err != nil || res.Text != "one" {
		t.Errorf("duplicate results: %+v, %v", res, err)
	}
	// A wrong id first: it is not the answer to anything, and it has used up
	// what the request paid for, so the call ends by its deadline. Nothing
	// the child sends can end a call with a result it was not asked for.
	res, err = answer(300*time.Millisecond, func(id string) []string {
		return []string{result(`"`+id+`"`, `{"text":"wrong id"}`), result(id, `{"text":"late"}`)}
	})
	if !errors.Is(err, context.DeadlineExceeded) || res.Text != "" {
		t.Errorf("a wrong id then the right one: %+v, %v", res, err)
	}
	for _, tc := range []struct{ name, result string }{
		{"a string", `"text"`},
		{"an array", `[]`},
		{"a number for the text", `{"text":7}`},
	} {
		if res, err := answer(waitLong, func(id string) []string { return []string{result(id, tc.result)} }); err == nil {
			t.Errorf("%s was taken for a result: %+v", tc.name, res)
		}
	}
	// A result and an error at once is an error.
	if _, err := answer(waitLong, func(id string) []string {
		return []string{`{"jsonrpc":"2.0","id":` + id + `,"result":{"text":"x"},"error":{"code":-32002,"message":"busy\u0000\u001b[31m` + strings.Repeat("!", 500) + `"}}`}
	}); !errors.Is(err, ErrBusy) || len(err.Error()) > 400 || strings.ContainsAny(err.Error(), "\x00\x1b") {
		t.Errorf("an error with a result: %q", err)
	}
	// A result the child did not clip is clipped here; bytes that are not
	// text come out as text.
	res, err = answer(waitLong, func(id string) []string {
		return []string{result(id, `{"text":"`+strings.Repeat("a", maxResultText+100)+`"}`)}
	})
	if err != nil || len(res.Text) != maxResultText || !res.Clipped {
		t.Errorf("an oversized result: %d bytes, clipped=%v, %v", len(res.Text), res.Clipped, err)
	}
	res, err = answer(waitLong, func(id string) []string {
		return []string{`{"jsonrpc":"2.0","id":` + id + `,"result":{"text":"a` + "\xff\xfe" + `b","is_error":true}}`}
	})
	if err != nil || res.Text != "a"+repl+repl+"b" || !res.IsError {
		t.Errorf("a result that is not UTF-8: %q, %v", res.Text, err)
	}
	h.intact(t)
}

// A tool list is a child's own account. Names are held to what a name is;
// the rest is cut to size.
func TestHostileToolList(t *testing.T) {
	h := newHostile(t)
	c, id := h.raw(t, "bad")
	list := func(result string) ([]model.ToolSpec, error) {
		t.Helper()
		type out struct {
			tools []model.ToolSpec
			err   error
		}
		done := make(chan out, 1)
		go func() {
			tools, err := h.mgr.Tools(ctxFor(t), id)
			done <- out{tools, err}
		}()
		var req mcp.Message
		for req = c.next(); req.Method != methodToolsList; req = c.next() {
		}
		c.reply(req, result)
		o := <-done
		return o.tools, o.err
	}
	many := make([]string, maxTools+1)
	for i := range many {
		many[i] = fmt.Sprintf(`{"name":"t%d"}`, i)
	}
	for name, result := range map[string]string{
		"a name with a space":   `{"tools":[{"name":"rm -rf"}]}`,
		"no name":               `{"tools":[{"description":"x"}]}`,
		"a name twice":          `{"tools":[{"name":"a"},{"name":"a"}]}`,
		"a name of control":     `{"tools":[{"name":"a\u0007"}]}`,
		"a name too long":       `{"tools":[{"name":"` + strings.Repeat("a", maxToolLen+1) + `"}]}`,
		"more than any machine": `{"tools":[` + strings.Join(many, ",") + `]}`,
		"not a list":            `{"tools":{"name":"a"}}`,
	} {
		if tools, err := list(result); err == nil {
			t.Errorf("%s: accepted as %+v", name, tools)
		}
	}
	tools, err := list(`{"tools":[{"name":"ok","description":"` + strings.Repeat("d", maxToolDesc+50) + `","parameters":[1,2],"gated":true,"Gate":"x"},{"name":"two","parameters":{"type":"object","x":1}}]}`)
	if err != nil || len(tools) != 2 || len(tools[0].Description) != maxToolDesc || string(tools[0].Parameters) != `{"type":"object"}` ||
		tools[0].Gated || tools[0].Gate != "" || string(tools[1].Parameters) != `{"type":"object","x":1}` {
		t.Errorf("tools = %+v, %v", tools, err)
	}
	if tools, err := list(`{}`); err != nil || len(tools) != 0 {
		t.Errorf("an empty list: %+v, %v", tools, err)
	}
	h.intact(t)
}

// A machine that dials again and again without ever finishing its hello
// holds one connection that is not a link yet, not one per dial.
func TestHostileManyConnectionsStuckInTheHello(t *testing.T) {
	// The hello has its ordinary ten seconds, so that none runs out while the
	// refusals are counted; TestHostileHello has the hello that times out.
	h := newHostile(t)
	mach, id := h.add(t, "bad", StateConfirmed)
	mute := func(*rawChild, mcp.Message) bool { return true } // it answers nothing
	stuck, err := h.dialRaw(t, id, mute)
	if err != nil {
		t.Fatal(err)
	}
	base := runtime.NumGoroutine()
	for range 20 {
		_, err := h.dialRaw(t, id, mute)
		var status ws.StatusError
		if !errors.As(err, &status) || int(status) != http.StatusTooManyRequests {
			t.Fatalf("a second connection during the hello = %v, want 429", err)
		}
	}
	for deadline := time.Now().Add(waitLong); runtime.NumGoroutine() > base+4; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Errorf("20 refused connections left %d goroutines", runtime.NumGoroutine()-base)
			break
		}
	}
	h.intact(t)
	// The connection that owed the hello goes away: the machine's next try
	// is taken.
	_ = stuck.conn.Close()
	for deadline := time.Now().Add(waitLong); ; time.Sleep(5 * time.Millisecond) {
		if _, err := h.dialRaw(t, id, nil); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("the machine never got another try: %v", err)
		}
	}
	h.awaitOnline(t, mach.ID, true)
	h.intact(t)
}
