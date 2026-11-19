package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"tenant/internal/mcp"
	"tenant/internal/mcp/transport"
	"tenant/internal/model"
)

// The two ends of the link, each fed frames the other could send. A frame
// goes the way a real one does after the transport: through the guard and
// the handler on the master, through the handler on the child. Each target
// checks more than "does not panic": nothing is approved, run or saved that
// the frame had no right to.

// nowhere is a transport that is not connected to anything.
type nowhere struct{ closed chan struct{} }

func (nowhere) Send(context.Context, []byte) error { return transport.ErrClosed }
func (n nowhere) Recv(ctx context.Context) ([]byte, error) {
	select {
	case <-n.closed:
	case <-ctx.Done():
	}
	return nil, transport.ErrClosed
}
func (nowhere) Close() error { return nil }

// rpc builds a JSON-RPC frame for the seed corpus.
func rpc(id, method, params string) []byte {
	s := `{"jsonrpc":"2.0"`
	if id != "" {
		s += `,"id":` + id
	}
	if method != "" {
		s += `,"method":"` + method + `"`
	}
	if params != "" {
		s += `,"params":` + params
	}
	return []byte(s + "}")
}

// FuzzMasterTakesAChildsFrame: every frame a child can send, admitted by the
// guard and dispatched by the master's handler, with one call in flight to
// that child whose Confirm says yes to whatever reaches it.
func FuzzMasterTakesAChildsFrame(f *testing.F) {
	const live = "0123456789abcdef0123456789abcdef"
	for _, seed := range [][]byte{
		rpc("1", methodConfirm, `{"call_id":"`+live+`","action":"os_exec_dangerous","detail":"rm -rf /"}`),
		rpc("1", methodConfirm, `{"call_id":"other","action":"os_exec","detail":"x"}`),
		rpc("1", methodConfirm, `{"call_id":"`+live+`","action":"rm -rf /","detail":"x"}`),
		rpc("1", methodConfirm, `{"call_id":"`+live+`","action":"os_exec","detail":"`+strings.Repeat("d", maxDetailLen+10)+`"}`),
		rpc("1", methodConfirm, `{"call_id":["`+live+`"],"action":"os_exec"}`),
		rpc("1", methodConfirm, ``),
		rpc(`"a"`, methodPing, ``),
		rpc("1", methodPing, `{"x":1}`),
		rpc("1", methodToolsCall, `{"call_id":"`+live+`","name":"os_exec","arguments":{}}`),
		rpc("1", methodToolsList, ``), rpc("1", methodResult, `{"call_id":"`+live+`"}`), rpc("1", methodUpdate, `{}`),
		rpc("1", methodHello, `{"link":1}`), rpc("1", methodAddresses, `{"addresses":["10.0.0.1:1"]}`),
		rpc("1", "initialize", `{}`), rpc("1", noticeRevoked, ``),
		rpc("", noticeRevoked, ``), rpc("", mcp.MethodCancelled, `{"requestId":1}`), rpc("", mcp.MethodProgress, `{"progressToken":1}`),
		rpc("", "notifications/initialized", ``),
		[]byte(`{"jsonrpc":"2.0","id":1,"result":{"text":"hello"}}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"result":{"link":1}}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32002,"message":"busy"}}`),
		[]byte(`{"jsonrpc":"2.0","id":null,"method":"ping"}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"fleet/confirm","result":{}}`),
		[]byte(`{"id":1,"method":"fleet\/confirm","params":{"call_id":"` + live + `","action":"os_exec"}}`),
		[]byte(`{"ID":1,"METHOD":"ping"}`),
		[]byte(`{"id":1,"method":"ping","method":"tools/call"}`),
		[]byte(``), []byte(`{}`), []byte(`[]`), []byte(`null`), []byte(`7`), []byte(`not json`), []byte(strings.Repeat("[", 200)),
		[]byte(`{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`),
	} {
		f.Add(seed)
	}

	dir := f.TempDir()
	id := must(MintIdentity(MasterIdentityPath(dir)))(f)
	store := Open(dir)
	if err := store.SetAddresses([]string{"127.0.0.1:1"}); err != nil {
		f.Fatal(err)
	}
	childID := must(MintIdentity(ChildIdentityPath(f.TempDir())))(f)
	code := must(store.CreateEnrollment(id.Fingerprint, "box", 0, ""))(f)
	ec := must(parseEnrollCode(code))(f)
	mach := must(store.enroll(ec.Secret, childID.Fingerprint, linuxBox))(f)
	must(store.Confirm(id.Fingerprint, mach.ID, ConfirmCode(id.Fingerprint, childID.Fingerprint)))(f)
	mgr := must(newManager(ManagerConfig{Store: store, Identity: id, Log: quiet}))(f)
	ctx := context.Background()

	f.Fuzz(func(t *testing.T, frame []byte) {
		var asked []confirmParams
		l := &link{m: mgr, id: mach.ID, slots: make(chan struct{}, 1), calls: map[string]*call{
			live: {ctx: ctx, confirm: func(_ context.Context, action, detail string) bool {
				asked = append(asked, confirmParams{Action: action, Detail: detail})
				return true
			}},
		}}
		l.sess = mcp.NewSession(nowhere{}, l)
		l.credit.Store(1) // one request of the master's is unanswered
		g := &guard{l: l, tokens: requestBurst, refilled: time.Now()}

		pass, err := g.admit(frame)
		if err != nil {
			t.Fatalf("one frame ended the link: %v", err)
		}
		var msg mcp.Message
		parsed := json.Unmarshal(frame, &msg) == nil
		if !pass {
			if g.strikes != 1 {
				t.Fatalf("a dropped frame counted %d strikes", g.strikes)
			}
			if parsed && (msg.IsResponse() || msg.Method == mcp.MethodCancelled || msg.IsRequest()) {
				t.Fatalf("a frame the link has was dropped: %q", frame)
			}
			return
		}
		switch {
		case !parsed:
			t.Fatalf("bytes that are not a message were let through: %q", frame)
		case msg.IsResponse():
			if l.credit.Load() != 0 || g.strikes != 0 {
				t.Fatalf("a result was let through and not paid for")
			}
		case msg.IsNotification():
			if msg.Method != mcp.MethodCancelled || g.strikes != 0 {
				t.Fatalf("the notification %q was let through", msg.Method)
			}
			l.HandleNotification(ctx, msg.Method, msg.Params)
		case msg.IsRequest():
			res, err := l.HandleRequest(ctx, msg.Method, msg.Params)
			if (res == nil) == (err == nil) {
				t.Fatalf("%s: result %q and error %v", msg.Method, res, err)
			}
			var rpcErr *mcp.Error
			if err != nil && !errors.As(err, &rpcErr) {
				t.Fatalf("%s: an error that is not a JSON-RPC one: %v", msg.Method, err)
			}
			switch msg.Method {
			case methodPing:
				if string(res) != `{}` || g.strikes != 0 {
					t.Fatalf("ping: %q, %v", res, err)
				}
			case methodConfirm:
				if g.strikes != 0 {
					t.Fatal("a confirm was a strike")
				}
				if err != nil {
					break
				}
				// It was decided: then it was exactly this call's to ask, once,
				// in words of bounded size.
				var p confirmParams
				if json.Unmarshal(msg.Params, &p) != nil || p.CallID != live || !validAction(p.Action) {
					t.Fatalf("a confirm was decided for params %q", msg.Params)
				}
				want, _ := cut(p.Detail, maxDetailLen)
				if len(asked) != 1 || asked[0].Action != p.Action || asked[0].Detail != want || !utf8.ValidString(want) || string(res) != `{"allow":true}` {
					t.Fatalf("the decision was asked %+v and the child told %s", asked, res)
				}
				return
			default:
				if rpcErr == nil || rpcErr.Code != mcp.ErrMethodNotFound || g.strikes != 1 {
					t.Fatalf("a request for %q: %q, %v, %d strikes", msg.Method, res, err, g.strikes)
				}
			}
		default:
			t.Fatalf("a frame that is nothing was let through: %q", frame)
		}
		if len(asked) != 0 {
			t.Fatalf("an approval was asked for by %q", frame)
		}
	})
}

// fuzzTools is a mux that runs anything and counts.
type fuzzTools struct{ ran atomic.Int64 }

func (*fuzzTools) All() []model.ToolSpec {
	return []model.ToolSpec{{Name: "echo", Description: "<&>", Parameters: json.RawMessage(`{"type":"object"}`)}}
}

func (f *fuzzTools) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	f.ran.Add(1)
	if call.Name == "ask" && Confirm(ctx, "os_exec", "x") {
		return "allowed", false, nil // nobody is there to allow it
	}
	return string(call.Arguments), false, nil
}

// FuzzChildTakesAMastersFrame: every frame a master can send, dispatched by
// the child's handler, before and after the hello.
func FuzzChildTakesAMastersFrame(f *testing.F) {
	piece := `{"version":"1.5.0","sha256":"` + fuzzPrint + `","size":5,"offset":0,"data":"aGVsbG8="}`
	for _, seed := range [][]byte{
		rpc("1", methodHello, `{"link":1}`), rpc("1", methodHello, `{"link":3}`), rpc("1", methodHello, `{"link":0}`),
		rpc("1", methodPing, ``),
		rpc("1", methodUpdate, piece), rpc("1", methodUpdate, `{"version":"1","sha256":"x","size":-1}`),
		rpc("1", methodToolsList, ``),
		rpc("1", methodToolsCall, `{"call_id":"abc","name":"echo","arguments":{"a":"<b>"},"timeout_ms":1000}`),
		rpc("1", methodToolsCall, `{"call_id":"abd","name":"ask","timeout_ms":-1}`),
		rpc("1", methodToolsCall, `{"call_id":"a b","name":"echo"}`),
		rpc("1", methodToolsCall, `{"call_id":"abe","name":"two words"}`),
		rpc("1", methodToolsCall, `{"call_id":"abf","name":"echo","timeout_ms":9223372036854775807}`),
		rpc("1", methodResult, `{"call_id":"abc"}`), rpc("1", methodResult, `{"call_id":"../../x"}`),
		rpc("1", methodAddresses, `{"addresses":["127.0.0.1:1"]}`),
		rpc("1", methodAddresses, `{"addresses":["10.0.0.1:7443","[::1]:7443"]}`),
		rpc("1", methodAddresses, `{"addresses":["http://evil/"]}`), rpc("1", methodAddresses, `{"addresses":[]}`),
		rpc("1", methodConfirm, `{"call_id":"abc","action":"os_exec"}`),
		rpc("1", "initialize", `{}`), rpc("1", "", ``),
		rpc("", noticeRevoked, ``), rpc("", mcp.MethodCancelled, `{"requestId":1}`), rpc("", "x", ``),
		[]byte(`{"jsonrpc":"2.0","id":1,"result":{"allow":true}}`),
		[]byte(``), []byte(`{}`), []byte(`[]`), []byte(`null`), []byte(`not json`),
	} {
		f.Add(seed, true)
		f.Add(seed, false)
	}

	dir := f.TempDir()
	id := must(MintIdentity(ChildIdentityPath(dir)))(f)
	original := Child{ID: "m0123456789abcdef", Name: "box", MasterAddresses: []string{"127.0.0.1:1"},
		MasterFingerprint: id.Fingerprint, Sandbox: "host", Ceiling: CeilingReadOnly, Tools: []string{"echo"}}
	if err := SaveChild(ChildPath(dir), original); err != nil {
		f.Fatal(err)
	}
	tools := &fuzzTools{}
	var updates []Update
	c := must(NewClient(ClientConfig{Dir: dir, Tools: tools, Version: "1.2.3", User: "root", Log: quiet,
		Update: func(_ context.Context, u Update) (UpdateAck, error) {
			updates = append(updates, u)
			return UpdateAck{Received: u.Offset + int64(len(u.Data))}, nil
		}}))(f)
	ctx := context.Background()
	c.runCtx = ctx
	budget := frameMax(c.ws) - frameReserve

	f.Fuzz(func(t *testing.T, frame []byte, hello bool) {
		var msg mcp.Message
		if json.Unmarshal(frame, &msg) != nil {
			return // the session skips it
		}
		l := &childLink{c: c}
		l.sess = mcp.NewSession(nowhere{}, l) // never started: a confirm goes nowhere
		if hello {
			l.master.Store(LinkVersion)
		}
		if msg.IsNotification() {
			l.HandleNotification(ctx, msg.Method, msg.Params)
			if (l.sess.Err() != nil) != (msg.Method == noticeRevoked) {
				t.Fatalf("after the notification %q the link is closed=%v", msg.Method, l.sess.Err() != nil)
			}
			return
		}
		if !msg.IsRequest() {
			return
		}
		ran, updated := tools.ran.Load(), len(updates)
		res, err := l.HandleRequest(ctx, msg.Method, msg.Params)
		var rpcErr *mcp.Error
		if (res == nil) == (err == nil) || (err != nil && !errors.As(err, &rpcErr)) {
			t.Fatalf("%s: result %q and error %v", msg.Method, res, err)
		}
		if err == nil && (!json.Valid(res) || len(res) > budget) {
			t.Fatalf("%s: a result of %d bytes that is not JSON or does not fit a frame", msg.Method, len(res))
		}
		// A tool ran only for a well-formed call after a hello, and once.
		var p callParams
		wellFormed := msg.Method == methodToolsCall && hello && json.Unmarshal(msg.Params, &p) == nil && validCallID(p.CallID) && validToolName(p.Name)
		switch delta := tools.ran.Load() - ran; {
		case delta > 1, delta == 1 && !wellFormed:
			t.Fatalf("%d tools ran for %q", delta, frame)
		case delta == 1:
			var out callResult
			if err != nil || json.Unmarshal(res, &out) != nil || out.Text == "allowed" {
				t.Fatalf("the call's result: %q, %v", res, err)
			}
		}
		// The updater saw only a valid piece.
		if len(updates) > updated {
			if msg.Method != methodUpdate || len(updates) != updated+1 || updates[updated].validate() != nil {
				t.Fatalf("the updater was given %+v by %q", updates[updated:], frame)
			}
			updates = updates[:0]
		}
		// child.json is still this machine's, with only its addresses changed,
		// and to addresses that are addresses.
		now, lerr := LoadChild(ChildPath(dir))
		if lerr != nil || validAddresses(now.MasterAddresses) != nil || len(now.MasterAddresses) == 0 {
			t.Fatalf("child.json after %q: %+v, %v", frame, now, lerr)
		}
		if msg.Method != methodAddresses || err != nil {
			if !slices.Equal(now.MasterAddresses, original.MasterAddresses) {
				t.Fatalf("%q changed the addresses to %v", frame, now.MasterAddresses)
			}
		} else {
			original.MasterAddresses = now.MasterAddresses
		}
		now.MasterAddresses = original.MasterAddresses
		if now.ID != original.ID || now.Ceiling != original.Ceiling || now.MasterFingerprint != original.MasterFingerprint ||
			now.Name != original.Name || now.Sandbox != original.Sandbox || !slices.Equal(now.Tools, original.Tools) {
			t.Fatalf("child.json after %q: %+v", frame, now)
		}
	})
}
