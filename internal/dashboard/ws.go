package dashboard

// WebSocket bridge between a browser client and the live agent (TEN-78).
// Each /ws connection subscribes to the broker's event fan-out and streams
// every turn Event out as a JSON text frame, while reading client command
// frames (turn / interject / stop) back in.
//
// Turn execution is serialized SERVER-WIDE, not per connection (TEN-80): the
// agent is a single shared instance, so the Server's wsCoordinator (session.go)
// allows at most one turn at a time across ALL clients. A "turn" arriving
// while one is active gets a busy notice; every client still receives the full
// shared event stream from the broker.
//
// Dependency: github.com/gobwas/ws (promoted from indirect to direct). It
// was already in the module graph via the dependency-minimal tree, and its
// wsutil helpers (UpgradeHTTP + ReadClientData/WriteServerText) cover this
// handler cleanly, so no brand-new dependency was added.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"

	"tenant/internal/agent"
)

// wsWriteTimeout bounds a single frame write. A browser that stops reading
// must not wedge the writer goroutine forever; a timed-out write closes the
// connection, which unblocks the reader and tears the whole handler down.
const wsWriteTimeout = 10 * time.Second

// wsEventOut is the wire shape sent to clients for each agent Event. Only
// the fields a browser renders are included (Kind/Iter/Text/Tool/Args/
// Result/IsErr); the Budget pointer and token counts are intentionally
// omitted from this minimal surface.
type wsEventOut struct {
	Kind   string `json:"kind"`
	Iter   int    `json:"iter,omitempty"`
	Text   string `json:"text,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"`
	IsErr  bool   `json:"isErr,omitempty"`
}

// wsClientMsg is one inbound command from the browser. Type is one of
// "turn" | "interject" | "stop"; Text carries the payload for turn/interject.
type wsClientMsg struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// mountWS registers GET /ws. See the CONTRACT block in server.go.
func (s *Server) mountWS(mux *http.ServeMux) {
	mux.HandleFunc("GET /ws", s.handleWS)
}

// handleWS upgrades the request to a WebSocket and runs the per-connection
// reader/writer until the client disconnects or a fatal I/O error occurs.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		// UpgradeHTTP already wrote an HTTP error response on a bad
		// handshake; nothing more to send.
		s.log.Warn("dashboard: ws upgrade failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	s.serveWSConn(conn)
}

// wsConn wraps one upgraded socket with a write mutex so the writer pump and
// the reader loop's advisory notices never interleave a frame on the wire.
// (The Server struct is owned by server.go and can't carry per-connection
// state, so write serialization lives here.)
type wsConn struct {
	server *Server
	raw    net.Conn
	wmu    sync.Mutex
}

// serveWSConn drives one upgraded connection: a broker subscription fed to a
// writer goroutine, and a reader loop that dispatches client commands. It
// returns only after both goroutines have stopped and the socket is closed,
// so no goroutine outlives the connection.
func (s *Server) serveWSConn(raw net.Conn) {
	defer raw.Close()
	c := &wsConn{server: s, raw: raw}

	// connCtx is the lifetime of this connection; canceling it stops the
	// writer pump and any in-flight turn.
	connCtx, connCancel := context.WithCancel(context.Background())
	defer connCancel()

	ch, unsub := s.broker.Subscribe()
	defer unsub()

	var wg sync.WaitGroup

	// WRITER pump: marshal each Event and write it as a text frame. Stops
	// when the broker channel closes (unsub) or connCtx is canceled. A
	// write error (slow/dead client, hit deadline) tears the connection
	// down so the reader unblocks too.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer connCancel() // a writer exit (e.g. broken pipe) ends the reader
		for {
			select {
			case <-connCtx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return // subscription canceled
				}
				if err := c.writeEvent(ev); err != nil {
					return
				}
			}
		}
	}()

	// Turn execution is serialized by the Server-wide coordinator, not per
	// connection (TEN-80). If THIS connection owns the active turn when it
	// goes away, that turn is canceled; otherwise the running turn (owned by
	// another client) is left alone.
	defer s.coord.disconnected(c)

	// READER loop: blocks on client frames. Runs on this goroutine so the
	// handler stays alive until the client goes away.
	c.readLoop(connCtx)

	// Reader returned (disconnect or read error): tear everything down and
	// wait for the writer pump to exit before the deferred raw.Close().
	connCancel()
	unsub()
	wg.Wait()
}

// readLoop reads client text frames until EOF/error or connCtx cancellation,
// dispatching each decoded command to the Server-wide turn coordinator.
// wsutil.ReadClientData also answers any client control frames (ping/close) on
// the socket for us. connCtx is passed as the parent of any turn this client
// starts, so the connection's teardown cancels its own turn.
func (c *wsConn) readLoop(connCtx context.Context) {
	for {
		if connCtx.Err() != nil {
			return
		}
		data, op, err := wsutil.ReadClientData(c.raw)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				c.server.log.Debug("dashboard: ws read ended", "err", err)
			}
			return
		}
		if op == ws.OpClose {
			return
		}
		if op != ws.OpText {
			continue // ignore binary/other data frames
		}

		var msg wsClientMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			c.notice("ignored malformed message")
			continue
		}
		switch msg.Type {
		case "turn":
			c.server.coord.startTurn(c, connCtx, msg.Text)
		case "interject":
			c.server.coord.interject(msg.Text)
		case "stop":
			c.server.coord.stop(c)
		default:
			c.notice("unknown message type: " + msg.Type)
		}
	}
}

// writeEvent marshals one Event to the wire shape and writes it as a text
// frame under a write deadline (slow-client guard).
func (c *wsConn) writeEvent(ev agent.Event) error {
	payload, err := json.Marshal(wsEventOut{
		Kind:   string(ev.Kind),
		Iter:   ev.Iter,
		Text:   ev.Text,
		Tool:   ev.Tool,
		Args:   ev.Args,
		Result: ev.Result,
		IsErr:  ev.IsErr,
	})
	if err != nil {
		return err // unreachable for these field types, but don't swallow
	}
	return c.writeFrame(payload)
}

// notice sends a small advisory frame to the client. Best-effort: a write
// failure here just means the connection is already going away, which the
// writer/reader teardown handles.
func (c *wsConn) notice(text string) {
	payload, err := json.Marshal(wsEventOut{Kind: "notice", Text: text})
	if err != nil {
		return
	}
	_ = c.writeFrame(payload)
}

// writeFrame writes one text frame under wsWriteTimeout. Concurrent writers
// (the writer pump and the reader-loop's notices) are serialized by wmu so a
// frame is never interleaved on the wire.
func (c *wsConn) writeFrame(payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.raw.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return wsutil.WriteServerText(c.raw, payload)
}
