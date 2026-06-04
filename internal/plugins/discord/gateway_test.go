package discord

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// fakeConn scripts server→client frames and records client→client writes, so
// the gateway FSM is tested with zero real sockets.
type fakeConn struct {
	mu     sync.Mutex
	reads  []readResult
	ri     int
	writes []gwPayload
	done   chan struct{}
}

type readResult struct {
	p   gwPayload
	err error
}

func newFakeConn(rr ...readResult) *fakeConn {
	return &fakeConn{reads: rr, done: make(chan struct{})}
}

func (f *fakeConn) Read() (gwPayload, error) {
	f.mu.Lock()
	if f.ri < len(f.reads) {
		r := f.reads[f.ri]
		f.ri++
		f.mu.Unlock()
		return r.p, r.err
	}
	f.mu.Unlock()
	<-f.done // no more scripted frames → block until Close, then report a clean close
	return gwPayload{}, &gwClose{code: 1000}
}

func (f *fakeConn) Write(p gwPayload) error {
	f.mu.Lock()
	f.writes = append(f.writes, p)
	f.mu.Unlock()
	return nil
}

func (f *fakeConn) Close() error {
	f.mu.Lock()
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	f.mu.Unlock()
	return nil
}

func (f *fakeConn) firstWrite() (gwPayload, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) == 0 {
		return gwPayload{}, false
	}
	return f.writes[0], true
}

func srv(op int, d string, s *int, t string) readResult {
	return readResult{p: gwPayload{Op: op, D: json.RawMessage(d), S: s, T: t}}
}

func intp(n int) *int { return &n }

func msgD(id, chanID, guild, author string, bot bool, content string) string {
	b, _ := json.Marshal(map[string]any{
		"id": id, "channel_id": chanID, "guild_id": guild, "content": content,
		"author": map[string]any{"id": author, "bot": bot},
	})
	return string(b)
}

// The session FSM: Hello → IDENTIFY → READY (caches session) → MESSAGE_CREATE
// (emitted once; the duplicate id is de-duped) → op7 Reconnect (→ resume).
func TestGateway_SessionFSM(t *testing.T) {
	var got []Inbound
	g := &Gateway{Token: "tok", OnMessage: func(in Inbound) { got = append(got, in) }, seen: newSeenSet(64)}
	fake := newFakeConn(
		srv(opHello, `{"heartbeat_interval":60000}`, nil, ""),
		srv(opDispatch, `{"session_id":"sess1","resume_gateway_url":"wss://resume.gg"}`, intp(1), "READY"),
		srv(opDispatch, msgD("m1", "chan1", "", "user1", false, "hello there"), intp(2), "MESSAGE_CREATE"),
		srv(opDispatch, msgD("m1", "chan1", "", "user1", false, "hello there"), intp(3), "MESSAGE_CREATE"), // replay: same id
		srv(opReconnect, `null`, intp(3), ""),
	)
	var st sessionState
	o := g.session(context.Background(), fake, &st, false)

	if o.action != actResume {
		t.Fatalf("op7 should drive a resume, got action %d", o.action)
	}
	if !o.gotReady {
		t.Error("READY should set gotReady (→ backoff reset)")
	}
	if st.sessionID != "sess1" || st.resumeURL != "wss://resume.gg" {
		t.Errorf("READY did not cache session: %+v", st)
	}
	if len(got) != 1 {
		t.Fatalf("idempotency failed: want 1 emitted message, got %d", len(got))
	}
	if got[0].MessageID != "m1" || got[0].Content != "hello there" || got[0].AuthorID != "user1" {
		t.Errorf("decoded message wrong: %+v", got[0])
	}
	if w, ok := fake.firstWrite(); !ok || w.Op != opIdentify {
		t.Errorf("first client frame should be IDENTIFY, got %+v", w)
	}
}

// resume=true with a cached session sends RESUME (op6), not IDENTIFY.
func TestGateway_SessionResumes(t *testing.T) {
	g := &Gateway{Token: "tok", seen: newSeenSet(8)}
	fake := newFakeConn(
		srv(opHello, `{"heartbeat_interval":60000}`, nil, ""),
		srv(opInvalidSession, `false`, nil, ""), // → re-identify
	)
	st := sessionState{sessionID: "prev", resumeURL: "wss://r", lastSeq: intp(7)}
	o := g.session(context.Background(), fake, &st, true)
	if o.action != actIdentify {
		t.Fatalf("op9 d=false should re-identify, got %d", o.action)
	}
	if w, ok := fake.firstWrite(); !ok || w.Op != opResume {
		t.Errorf("first client frame should be RESUME, got %+v", w)
	}
}

func TestDecideClose(t *testing.T) {
	for _, code := range []int{4004, 4010, 4011, 4012, 4013, 4014} {
		if o := decideClose(code); o.action != actGiveup {
			t.Errorf("close %d should give up, got %d", code, o.action)
		}
	}
	for _, code := range []int{4000, 4007, 4008, 4009, 4999} {
		if o := decideClose(code); o.action != actResume {
			t.Errorf("close %d should resume, got %d", code, o.action)
		}
	}
}

func TestClassifyReadErr(t *testing.T) {
	if o := classifyReadErr(context.Background(), &gwClose{code: 4004}); o.action != actGiveup {
		t.Error("a 4004 close should give up")
	}
	if o := classifyReadErr(context.Background(), &gwClose{code: 1006}); o.action != actResume {
		t.Error("an abnormal 1006 close should resume")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if o := classifyReadErr(ctx, errADrop{}); o.action != actStop {
		t.Error("a cancelled ctx should stop")
	}
}

type errADrop struct{}

func (errADrop) Error() string { return "drop" }

func TestSeenSet_DedupAndEvict(t *testing.T) {
	s := newSeenSet(2)
	if !s.add("a") || !s.add("b") {
		t.Fatal("fresh ids should be new")
	}
	if s.add("a") {
		t.Error("repeat id must be reported as seen")
	}
	s.add("c") // evicts oldest ("a")
	if !s.add("a") {
		t.Error("after eviction, 'a' should be treatable as new again")
	}
}

func TestGateway_DispatchInteraction(t *testing.T) {
	var got []Interaction
	g := &Gateway{OnInteraction: func(in Interaction) { got = append(got, in) }}

	// DM button click (top-level user).
	g.dispatchInteraction(json.RawMessage(`{"id":"i1","token":"tok","type":3,"channel_id":"dm1","data":{"custom_id":"approve:ABC"},"message":{"id":"m1"},"user":{"id":"op"}}`))
	if len(got) != 1 {
		t.Fatalf("want 1 interaction, got %d", len(got))
	}
	if in := got[0]; in.ID != "i1" || in.Token != "tok" || in.CustomID != "approve:ABC" || in.UserID != "op" || in.ChannelID != "dm1" || in.MessageID != "m1" {
		t.Errorf("bad decode: %+v", in)
	}
	// A non-component interaction (type 2 = slash command) is ignored.
	g.dispatchInteraction(json.RawMessage(`{"id":"i2","type":2,"data":{"name":"x"}}`))
	if len(got) != 1 {
		t.Error("a non-component interaction must be ignored")
	}
	// Guild click resolves the clicker from member.user.
	g.dispatchInteraction(json.RawMessage(`{"id":"i3","token":"t3","type":3,"data":{"custom_id":"deny:Z"},"member":{"user":{"id":"guser"}}}`))
	if len(got) != 2 || got[1].UserID != "guser" {
		t.Errorf("guild member.user decode wrong: %+v", got)
	}
}

func TestWithGatewayQuery(t *testing.T) {
	if got := withGatewayQuery("wss://gateway.discord.gg"); got != "wss://gateway.discord.gg?v=10&encoding=json" {
		t.Errorf("got %q", got)
	}
	if got := withGatewayQuery("wss://x/?foo=1"); got != "wss://x/?foo=1&v=10&encoding=json" {
		t.Errorf("got %q", got)
	}
}
