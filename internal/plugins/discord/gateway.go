package discord

// gateway.go is TEN-115: the inbound Discord Gateway client — the "Surface B"
// socket the package header (discord.go) deferred. Hand-rolled on
// github.com/gobwas/ws (already a direct dep via internal/dashboard/ws.go), zero
// new modules, matching the no-SDK ethos. Operator-DM scope: intents =
// DIRECT_MESSAGES|GUILDS (4097), so NO MESSAGE_CONTENT privileged intent and no
// portal verification are required (DM content is intent-exempt).
//
// Design: the protocol FSM (Hello/IDENTIFY/RESUME/heartbeat/dispatch + the
// reconnect/resume/give-up decision) runs over a gwConn TRANSPORT SEAM so it is
// unit-testable with a scripted fake conn (gateway_test.go); the real gobwas
// frame I/O lives in the thin gobwasConn adapter. Guardrails (TEN-115 review):
// idempotency on MESSAGE_CREATE.id (dedupe a replayed event after a resume),
// backoff with jitter that resets ONLY after READY/RESUMED, and a hard give-up
// (alert, no spin) on the fatal close codes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// Gateway opcodes (Discord Gateway v10).
const (
	opDispatch       = 0  // recv: an event (T names it; MESSAGE_CREATE, READY, …)
	opHeartbeat      = 1  // bidirectional
	opIdentify       = 2  // send
	opResume         = 6  // send
	opReconnect      = 7  // recv: server asks us to reconnect+resume
	opInvalidSession = 9  // recv: d is a bool (true=resumable, false=re-identify)
	opHello          = 10 // recv: heartbeat_interval
	opHeartbeatACK   = 11 // recv: ack of our heartbeat
)

// intentsDMGuilds = GUILDS(1<<0) | DIRECT_MESSAGES(1<<12) | MESSAGE_CONTENT(1<<15) = 32773.
// Enough to receive MESSAGE_CREATE content in both DM and guilds.
const intentsDMGuilds = (1 << 0) | (1 << 12) | (1 << 15)

// gwPayload is the {op,d,s,t} gateway envelope. S is nullable (only Dispatch
// frames carry a sequence); D is delayed-decoded per opcode/event.
type gwPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int            `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// Inbound is one received Discord message, normalized for the relay. v1 cares
// about DMs from the operator; GuildID=="" marks a DM.
type Inbound struct {
	MessageID string
	ChannelID string
	GuildID   string
	AuthorID  string
	AuthorBot bool
	Content   string
}

// Interaction is a button/component click (INTERACTION_CREATE, type 3) — the v2
// approval primitive. ID+Token are needed to ACK within Discord's 3-second
// window; CustomID carries the bound action (e.g. "approve:<nonce>").
type Interaction struct {
	ID        string
	Token     string
	CustomID  string
	UserID    string
	ChannelID string
	MessageID string
}

// gwClose is the error a gwConn.Read returns when the server sent a Close
// frame; code drives the reconnect FSM.
type gwClose struct {
	code   int
	reason string
}

func (e *gwClose) Error() string { return fmt.Sprintf("gateway closed: %d %s", e.code, e.reason) }

// gwConn is the transport seam. The FSM only speaks gwPayload; the real impl
// (gobwasConn) does WebSocket framing, tests script frames.
type gwConn interface {
	Read() (gwPayload, error) // next gateway message; *gwClose on a close frame
	Write(gwPayload) error
	Close() error
}

// dialer opens a gwConn to a gateway URL. Defaults to gobwasDial; tests inject.
type dialer func(ctx context.Context, gatewayURL string) (gwConn, error)

// --- real gobwas transport ---

type gobwasConn struct {
	conn net.Conn
	rd   *wsutil.Reader
	ctrl wsutil.FrameHandlerFunc
	wmu  sync.Mutex
}

// gobwasDial connects to urlstr and wraps it as a gwConn. Critical gotcha:
// ws.Dial returns a *bufio.Reader that holds bytes already read off the socket
// during the handshake — Discord sends Hello immediately, so it is typically
// NON-nil and MUST be the read source (reading it transparently continues into
// the conn); reading conn directly would miss the Hello frame.
func gobwasDial(ctx context.Context, urlstr string) (gwConn, error) {
	conn, br, _, err := ws.Dial(ctx, urlstr)
	if err != nil {
		return nil, err
	}
	var src io.Reader = conn
	if br != nil {
		src = br
	}
	ctrl := wsutil.ControlFrameHandler(conn, ws.StateClientSide)
	rd := &wsutil.Reader{Source: src, State: ws.StateClientSide, OnIntermediate: ctrl}
	return &gobwasConn{conn: conn, rd: rd, ctrl: ctrl}, nil
}

func (c *gobwasConn) Read() (gwPayload, error) {
	for {
		hdr, err := c.rd.NextFrame()
		if err != nil {
			return gwPayload{}, err // EOF / abnormal — session layer maps to a resume
		}
		if hdr.OpCode == ws.OpClose {
			err := c.ctrl(hdr, c.rd) // reads close body, returns wsutil.ClosedError
			var ce wsutil.ClosedError
			if errors.As(err, &ce) {
				return gwPayload{}, &gwClose{code: int(ce.Code), reason: ce.Reason}
			}
			return gwPayload{}, &gwClose{code: 1006}
		}
		if hdr.OpCode.IsControl() {
			if err := c.ctrl(hdr, c.rd); err != nil {
				return gwPayload{}, err
			}
			continue
		}
		if hdr.OpCode != ws.OpText {
			_, _ = io.Copy(io.Discard, c.rd) // skip a non-text data frame
			continue
		}
		data, err := io.ReadAll(c.rd)
		if err != nil {
			return gwPayload{}, err
		}
		var p gwPayload
		if err := json.Unmarshal(data, &p); err != nil {
			return gwPayload{}, fmt.Errorf("gateway: bad payload: %w", err)
		}
		return p, nil
	}
}

func (c *gobwasConn) Write(p gwPayload) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return wsutil.WriteClientText(c.conn, b)
}

func (c *gobwasConn) Close() error { return c.conn.Close() }

// --- the Gateway FSM ---

// Gateway maintains one persistent inbound connection (with resume) and calls
// OnMessage for each received message. Token/GetURL/OnMessage are required;
// OnFatal (optional) is called once before Run returns on a non-resumable
// failure (bad token / disallowed intents). The dial field defaults to the real
// gobwas dialer; tests override it.
type Gateway struct {
	Token         string
	GetURL        func(ctx context.Context) (string, error)
	OnMessage     func(Inbound)
	OnInteraction func(Interaction) // v2: button clicks (INTERACTION_CREATE)
	OnReady       func(userID string) // called once with the bot's user ID from READY
	OnFatal       func(error)
	Log           *slog.Logger

	BotUserID string // set from READY; used for mention stripping

	dial dialer // nil → gobwasDial (test seam)
	seen *seenSet
}

func (g *Gateway) logf(format string, args ...any) {
	if g.Log != nil {
		g.Log.Warn(fmt.Sprintf(format, args...))
	}
}

// sessionState persists across reconnects so a RESUME can replay.
type sessionState struct {
	sessionID string
	resumeURL string
	lastSeq   *int
}

func (s *sessionState) seq() int {
	if s.lastSeq == nil {
		return 0
	}
	return *s.lastSeq
}

type action int

const (
	actResume   action = iota // reconnect to resume_gateway_url + RESUME
	actIdentify               // fresh connection + IDENTIFY
	actGiveup                 // fatal — alert + stop
	actStop                   // ctx cancelled — clean stop
)

type outcome struct {
	action   action
	gotReady bool // a READY/RESUMED happened this connection (→ reset backoff)
	err      error
}

// Run drives the gateway until ctx is cancelled (returns nil) or a fatal close
// (returns the error, after OnFatal). It reconnects with resume/identify per the
// FSM and backs off with jitter, resetting backoff only after a READY/RESUMED.
func (g *Gateway) Run(ctx context.Context) error {
	if g.dial == nil {
		g.dial = gobwasDial
	}
	if g.seen == nil {
		g.seen = newSeenSet(1024)
	}
	bo := newBackoff()
	var st sessionState
	resume := false

	for {
		if ctx.Err() != nil {
			return nil
		}
		urlStr := st.resumeURL
		if !resume || urlStr == "" {
			u, err := g.GetURL(ctx)
			if err != nil {
				g.logf("gateway: get url: %v", err)
				if !bo.wait(ctx) {
					return nil
				}
				continue
			}
			urlStr = u
		}
		conn, err := g.dial(ctx, withGatewayQuery(urlStr))
		if err != nil {
			g.logf("gateway: dial: %v", err)
			resume = false
			if !bo.wait(ctx) {
				return nil
			}
			continue
		}
		o := g.session(ctx, conn, &st, resume)
		_ = conn.Close()
		if o.gotReady {
			bo.reset()
		}
		switch o.action {
		case actStop:
			return nil
		case actGiveup:
			if g.OnFatal != nil {
				g.OnFatal(o.err)
			}
			return o.err
		case actIdentify:
			st = sessionState{}
			resume = false
		case actResume:
			resume = st.sessionID != ""
		}
		if !bo.wait(ctx) {
			return nil
		}
	}
}

// session runs exactly one connection: read Hello, start the heartbeat,
// IDENTIFY or RESUME, then process dispatches until the connection ends. The
// returned outcome tells Run how to reconnect.
func (g *Gateway) session(ctx context.Context, conn gwConn, st *sessionState, resume bool) outcome {
	first, err := conn.Read()
	if err != nil {
		return classifyReadErr(ctx, err)
	}
	if first.Op != opHello {
		return outcome{action: actIdentify} // protocol desync → fresh identify
	}
	var hello struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	_ = json.Unmarshal(first.D, &hello)
	interval := time.Duration(maxInt(hello.HeartbeatInterval, 1000)) * time.Millisecond

	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	hb := &heart{seqv: st.seq()}
	if resume && st.sessionID != "" {
		if err := conn.Write(resumePayload(g.Token, st.sessionID, st.seq())); err != nil {
			return outcome{action: actResume}
		}
	} else {
		if err := conn.Write(identifyPayload(g.Token)); err != nil {
			return outcome{action: actResume}
		}
	}
	go g.heartbeatLoop(sessCtx, conn, interval, hb, cancel)

	gotReady := false
	for {
		p, err := conn.Read()
		if err != nil {
			o := classifyReadErr(ctx, err)
			o.gotReady = gotReady
			return o
		}
		if p.S != nil {
			st.lastSeq = p.S
			hb.setSeq(*p.S)
		}
		switch p.Op {
		case opHeartbeat:
			_ = conn.Write(heartbeatPayload(hb.getSeq()))
		case opHeartbeatACK:
			hb.ack()
		case opReconnect:
			return outcome{action: actResume, gotReady: gotReady}
		case opInvalidSession:
			var resumable bool
			_ = json.Unmarshal(p.D, &resumable)
			if resumable {
				return outcome{action: actResume, gotReady: gotReady}
			}
			return outcome{action: actIdentify, gotReady: gotReady}
		case opDispatch:
			switch p.T {
			case "READY":
				var rd struct {
					SessionID        string `json:"session_id"`
					ResumeGatewayURL string `json:"resume_gateway_url"`
					User             struct {
						ID string `json:"id"`
					} `json:"user"`
				}
				_ = json.Unmarshal(p.D, &rd)
				st.sessionID = rd.SessionID
				st.resumeURL = rd.ResumeGatewayURL
				if rd.User.ID != "" {
					g.BotUserID = rd.User.ID
					if g.OnReady != nil {
						g.OnReady(rd.User.ID)
					}
				}
				gotReady = true
			case "RESUMED":
				gotReady = true
			case "MESSAGE_CREATE":
				g.dispatchMessage(p.D)
			case "INTERACTION_CREATE":
				g.dispatchInteraction(p.D)
			}
		}
	}
}

// dispatchMessage decodes a MESSAGE_CREATE, dedupes by id (idempotency —
// kills a replayed event after a resume), and emits it.
func (g *Gateway) dispatchMessage(d json.RawMessage) {
	var m struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
		GuildID   string `json:"guild_id"`
		Content   string `json:"content"`
		Author    struct {
			ID  string `json:"id"`
			Bot bool   `json:"bot"`
		} `json:"author"`
	}
	if err := json.Unmarshal(d, &m); err != nil || m.ID == "" {
		return
	}
	if !g.seen.add(m.ID) {
		return // already processed this message id (replay) — drop
	}
	if g.OnMessage != nil {
		g.OnMessage(Inbound{
			MessageID: m.ID, ChannelID: m.ChannelID, GuildID: m.GuildID,
			AuthorID: m.Author.ID, AuthorBot: m.Author.Bot, Content: m.Content,
		})
	}
}

// dispatchInteraction decodes an INTERACTION_CREATE. Only message-component
// (button) interactions (type 3) are surfaced; the clicker is user.id in a DM
// or member.user.id in a guild.
func (g *Gateway) dispatchInteraction(d json.RawMessage) {
	var iv struct {
		ID        string `json:"id"`
		Token     string `json:"token"`
		Type      int    `json:"type"`
		ChannelID string `json:"channel_id"`
		Data      struct {
			CustomID string `json:"custom_id"`
		} `json:"data"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Member struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"member"`
	}
	if err := json.Unmarshal(d, &iv); err != nil || iv.Type != 3 || iv.Data.CustomID == "" {
		return // only button (component) interactions
	}
	uid := iv.User.ID
	if uid == "" {
		uid = iv.Member.User.ID
	}
	if g.OnInteraction != nil {
		g.OnInteraction(Interaction{
			ID: iv.ID, Token: iv.Token, CustomID: iv.Data.CustomID,
			UserID: uid, ChannelID: iv.ChannelID, MessageID: iv.Message.ID,
		})
	}
}

// heartbeatLoop sends op-1 heartbeats every interval (first beat after
// interval*jitter). If a beat goes un-ACKed by the next interval the connection
// is zombied: cancel the session + close the conn so the read loop unwinds and
// Run reconnects (resume).
func (g *Gateway) heartbeatLoop(ctx context.Context, conn gwConn, interval time.Duration, hb *heart, cancel context.CancelFunc) {
	timer := time.NewTimer(time.Duration(rand.Float64() * float64(interval)))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if hb.pending() { // previous heartbeat never ACKed → zombie
			cancel()
			_ = conn.Close()
			return
		}
		if err := conn.Write(heartbeatPayload(hb.getSeq())); err != nil {
			cancel()
			_ = conn.Close()
			return
		}
		hb.markPending()
		timer.Reset(interval)
	}
}

// classifyReadErr maps a read error to a reconnect action: a server Close uses
// the close code; ctx cancellation is a clean stop; anything else (abnormal
// 1006-style drop) resumes.
func classifyReadErr(ctx context.Context, err error) outcome {
	if ctx.Err() != nil {
		return outcome{action: actStop}
	}
	var ce *gwClose
	if errors.As(err, &ce) {
		return decideClose(ce.code)
	}
	return outcome{action: actResume} // abnormal drop → resume
}

// decideClose is the close-code FSM. Fatal (auth/intents/shard/version) →
// give up + alert, never spin. Everything else → resume.
func decideClose(code int) outcome {
	switch code {
	case 4004, 4010, 4011, 4012, 4013, 4014:
		return outcome{action: actGiveup, err: fmt.Errorf("gateway: fatal close %d (auth/intents — check the bot token + intents)", code)}
	default:
		return outcome{action: actResume}
	}
}

// --- payload builders ---

func identifyPayload(token string) gwPayload {
	d, _ := json.Marshal(map[string]any{
		"token":   token,
		"intents": intentsDMGuilds,
		"properties": map[string]string{
			"os": "linux", "browser": "tenant", "device": "tenant",
		},
		"presence": map[string]any{
			"status":     "online",
			"afk":        false,
			"activities": []any{},
		},
	})
	return gwPayload{Op: opIdentify, D: d}
}

func resumePayload(token, sessionID string, seq int) gwPayload {
	d, _ := json.Marshal(map[string]any{"token": token, "session_id": sessionID, "seq": seq})
	return gwPayload{Op: opResume, D: d}
}

func heartbeatPayload(seq int) gwPayload {
	d, _ := json.Marshal(seq)
	return gwPayload{Op: opHeartbeat, D: d}
}

// --- small helpers ---

// heart holds the per-connection sequence + un-ACKed heartbeat flag, shared
// between the read loop (setSeq/ack) and the heartbeat goroutine (getSeq/
// pending/markPending).
type heart struct {
	mu       sync.Mutex
	seqv     int
	unackedP bool
}

func (h *heart) setSeq(s int)  { h.mu.Lock(); h.seqv = s; h.mu.Unlock() }
func (h *heart) getSeq() int   { h.mu.Lock(); defer h.mu.Unlock(); return h.seqv }
func (h *heart) ack()          { h.mu.Lock(); h.unackedP = false; h.mu.Unlock() }
func (h *heart) markPending()  { h.mu.Lock(); h.unackedP = true; h.mu.Unlock() }
func (h *heart) pending() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.unackedP }

// seenSet is a bounded FIFO de-dup of MESSAGE_CREATE ids (idempotency).
type seenSet struct {
	mu    sync.Mutex
	set   map[string]struct{}
	order []string
	cap   int
}

func newSeenSet(capN int) *seenSet {
	return &seenSet{set: make(map[string]struct{}, capN), cap: capN}
}

// add returns true if id is new (and records it); false if already seen.
func (s *seenSet) add(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[id]; ok {
		return false
	}
	s.set[id] = struct{}{}
	s.order = append(s.order, id)
	if len(s.order) > s.cap {
		old := s.order[0]
		s.order = s.order[1:]
		delete(s.set, old)
	}
	return true
}

// backoff is exponential with a floor + jitter; reset() returns to the base
// (a hard floor between IDENTIFYs, so a flapping gateway can't storm Discord).
type backoff struct {
	cur, base, max time.Duration
}

func newBackoff() *backoff {
	return &backoff{base: time.Second, max: 64 * time.Second, cur: time.Second}
}

func (b *backoff) reset() { b.cur = b.base }

// nextDelay returns a jittered delay in [cur/2, cur] and advances cur (×2,
// capped at max). Pure (no sleep) so the backoff schedule is unit-testable
// independent of wall-clock timing.
func (b *backoff) nextDelay() time.Duration {
	half := b.cur / 2
	d := half + time.Duration(rand.Int63n(int64(half)+1))
	if next := b.cur * 2; next <= b.max {
		b.cur = next
	} else {
		b.cur = b.max
	}
	return d
}

// wait sleeps a jittered interval in [cur/2, cur] then grows cur (×2, capped).
// Returns false if ctx is cancelled during the sleep.
func (b *backoff) wait(ctx context.Context) bool {
	t := time.NewTimer(b.nextDelay())
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// withGatewayQuery appends Discord's required ?v=10&encoding=json.
func withGatewayQuery(u string) string {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "v=10&encoding=json"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
