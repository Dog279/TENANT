package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"tenant/internal/mcp"
)

// fleet/confirm is the one thing a child asks its master: may this action
// of the call you started go ahead. These tests are that round trip, from a
// tool's Confirm on the child to the caller's decision on the master.

type turnKey struct{}

// An honest child: allowed, denied, nobody to decide, and outside a call.
func TestConfirmRoundTrip(t *testing.T) {
	r := newRig(t)
	k := r.up(t, "box")
	id := k.mach.ID

	type asked struct {
		turn           any
		action, detail string
	}
	var seen []asked
	decide := func(allow bool) func(context.Context, string, string) bool {
		return func(ctx context.Context, action, detail string) bool {
			seen = append(seen, asked{ctx.Value(turnKey{}), action, detail})
			return allow
		}
	}
	ask := func(confirm func(context.Context, string, string) bool, action, detail string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), turnKey{}, "turn 7"), waitLong)
		defer cancel()
		args, _ := json.Marshal(map[string]string{"Action": action, "Detail": detail})
		res, err := r.mgr.Call(ctx, id, Call{Tool: "ask", Arguments: args, Confirm: confirm})
		if err != nil {
			t.Fatalf("ask: %v", err)
		}
		return res.Text
	}

	if got := ask(decide(true), "os_exec_dangerous", "rm -rf /tmp/x"); got != "allowed" {
		t.Errorf("a confirm the master allowed = %q", got)
	}
	if got := ask(decide(false), "os_write_outside", "write /etc/hosts"); got != "denied" {
		t.Errorf("a confirm the master denied = %q", got)
	}
	// The decision was made with the caller's context and the tool's words.
	want := []asked{{"turn 7", "os_exec_dangerous", "rm -rf /tmp/x"}, {"turn 7", "os_write_outside", "write /etc/hosts"}}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Errorf("the master was asked %v, want %v", seen, want)
	}
	seen = nil
	// A call made with nobody to decide: denied.
	if got := ask(nil, "os_exec", "ls"); got != "denied" {
		t.Errorf("a confirm with no Confirm on the call = %q", got)
	}
	// An action that is not a category name is refused before anyone is asked.
	for _, action := range []string{"", "rm -rf /", strings.Repeat("a", maxActionLen+1), "os_exec\n"} {
		if got := ask(decide(true), action, "x"); got != "denied" {
			t.Errorf("a confirm for the action %q = %q", action, got)
		}
	}
	if len(seen) != 0 {
		t.Errorf("the master was asked about %v", seen)
	}
	// A long detail is cut, on a rune boundary, not refused.
	if got := ask(decide(true), "os_exec", strings.Repeat("é", maxDetailLen)); got != "allowed" {
		t.Errorf("a confirm with a long detail = %q", got)
	}
	if d := seen[0].detail; len(d) > maxDetailLen || len(d) < maxDetailLen-4 || !utf8.ValidString(d) {
		t.Errorf("the detail arrived as %d bytes, valid=%v", len(d), utf8.ValidString(d))
	}

	// Outside a call there is nobody to ask.
	if Confirm(context.Background(), "os_exec", "ls") {
		t.Error("Confirm outside a call said yes")
	}
	// After the call ended its context asks no more, whatever the master
	// would say.
	if _, err := r.mgr.Call(ctxFor(t), id, Call{Tool: "keep", Confirm: decide(true)}); err != nil {
		t.Fatal(err)
	}
	seen = nil
	if Confirm(<-k.tools.ctxs, "os_exec", "ls") || len(seen) != 0 {
		t.Errorf("Confirm after its call ended said yes, or asked: %v", seen)
	}
	r.echo(t, id)
}

// The link drops while the master is deciding: the child's tool is told no,
// the decision's context ends, and the caller gets "outcome unknown".
func TestConfirmLinkDroppedWhileWaiting(t *testing.T) {
	r := newRig(t)
	k := r.up(t, "box")
	id := k.mach.ID
	first := r.link(t, id)

	deciding := make(chan context.Context, 1)
	done := make(chan error, 1)
	go func() {
		_, err := r.mgr.Call(ctxFor(t), id, Call{Tool: "ask", Arguments: json.RawMessage(`{"Action":"os_exec","Detail":"x"}`),
			Confirm: func(ctx context.Context, _, _ string) bool {
				deciding <- ctx
				<-ctx.Done()
				return true // too late: nobody is there to hear it
			}})
		done <- err
	}()
	callID := k.started().ID
	dctx := soon(t, deciding, "the master to be asked")
	k.kill()
	select {
	case <-dctx.Done():
	case <-time.After(waitLong):
		t.Fatal("the decision was left waiting after the link dropped")
	}
	if err := <-done; !errors.Is(err, ErrOutcomeUnknown) {
		t.Errorf("the call = %v", err)
	}
	r.awaitNewLink(t, id, first)
	awaitResult(t, r, id, callID, "denied")

	// The same when the caller gives up while the master is deciding.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, err := r.mgr.Call(ctx, id, Call{Tool: "ask", Arguments: json.RawMessage(`{"Action":"os_exec","Detail":"x"}`),
			Confirm: func(ctx context.Context, _, _ string) bool {
				deciding <- ctx
				<-ctx.Done()
				return true
			}})
		done <- err
	}()
	callID = k.started().ID
	dctx = soon(t, deciding, "the master to be asked")
	cancel()
	soon(t, dctx.Done(), "the decision's context to end with the call")
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("the call = %v", err)
	}
	awaitResult(t, r, id, callID, "denied")
	r.echo(t, id)
}

// A child that asks for approvals it has no right to: for no call, for
// another machine's call, in a shape that is not one, twice at once, too
// often, and after its call ended. None is decided by anyone, let alone
// allowed.
func TestHostileConfirm(t *testing.T) {
	h := newHostile(t)
	c, id := h.raw(t, "bad")
	code := func(msg mcp.Message) int {
		if msg.Error == nil {
			return 0
		}
		return msg.Error.Code
	}
	confirm := func(n int, callID, action, detail string) mcp.Message {
		t.Helper()
		params, _ := json.Marshal(confirmParams{CallID: callID, Action: action, Detail: detail})
		return c.ask(n, methodConfirm, string(params))
	}

	// No call is in flight to it at all.
	if got := confirm(1, "abc", "os_exec", "x"); code(got) != codeNoCall {
		t.Errorf("a confirm with no call in flight: %+v", got)
	}
	// The honest machine has a call in flight, one whose Confirm would say
	// yes. Its id is no use to another machine.
	held := make(chan error, 1)
	go func() {
		_, err := h.mgr.Call(ctxFor(t), h.honest.mach.ID, Call{Tool: "hold", Confirm: h.approve})
		held <- err
	}()
	theirs := h.honest.started().ID
	if got := confirm(2, theirs, "os_exec", "x"); code(got) != codeNoCall {
		t.Errorf("a confirm for another machine's call: %+v", got)
	}

	// Its own call, with a decision the test holds.
	type question struct {
		action, detail string
		answer         chan bool
	}
	questions := make(chan question, 64)
	done := make(chan error, 1)
	go func() {
		_, err := h.mgr.Call(ctxFor(t), id, Call{Tool: "echo", Confirm: func(ctx context.Context, action, detail string) bool {
			q := question{action, detail, make(chan bool)}
			questions <- q
			select {
			case allow := <-q.answer:
				return allow
			case <-ctx.Done():
				return true // what must not count
			}
		}})
		done <- err
	}()
	var req mcp.Message
	for req = c.next(); req.Method != methodToolsCall; req = c.next() {
	}
	var p callParams
	if err := json.Unmarshal(req.Params, &p); err != nil || !validCallID(p.CallID) || p.TimeoutMS <= 0 {
		t.Fatalf("tools/call params %s: %v", req.Params, err)
	}
	mine := p.CallID

	// Shapes that are not a confirm.
	for i, params := range []string{
		``, `null`, `[]`, `"x"`, `{}`,
		`{"call_id":"` + mine + `"}`,
		`{"call_id":"` + mine + `","action":"rm -rf /","detail":"x"}`,
		`{"call_id":"` + mine + `","action":7}`,
		`{"call_id":["` + mine + `"],"action":"os_exec"}`,
		`{"call_id":"` + mine + ` ","action":"os_exec"}`,
		`{"call_id":"` + strings.Repeat("a", maxCallIDLen+1) + `","action":"os_exec"}`,
	} {
		if got := c.ask(10+i, methodConfirm, params); code(got) != mcp.ErrInvalidParams {
			t.Errorf("confirm params %s: %+v", params, got)
		}
	}
	if len(questions) != 0 {
		t.Fatal("a malformed confirm reached the decision")
	}

	// A real one is put to the caller, exactly as asked, with a detail of
	// any size cut down and made text.
	answers := make(chan mcp.Message, 4)
	go func() { answers <- confirm(30, mine, "os_exec_dangerous", strings.Repeat("x", 100<<10)+"\xff") }()
	q := soon(t, questions, "the caller to be asked")
	if q.action != "os_exec_dangerous" || len(q.detail) != maxDetailLen || !utf8.ValidString(q.detail) {
		t.Errorf("the caller was asked about %q with %d bytes of detail", q.action, len(q.detail))
	}
	// While it is being decided a second one for the same call is refused.
	if got := confirm(31, mine, "os_exec", "another"); code(got) != codeTooMany {
		t.Errorf("a second confirm while one is pending: %+v", got)
	}
	q.answer <- false
	if got := <-answers; got.Error != nil || string(got.Result) != `{"allow":false}` {
		t.Errorf("a denied confirm: %+v", got)
	}
	// Yes is yes, for this call.
	go func() { answers <- confirm(32, mine, "os_exec", "ls") }()
	q = soon(t, questions, "the caller to be asked")
	q.answer <- true
	if got := <-answers; got.Error != nil || string(got.Result) != `{"allow":true}` {
		t.Errorf("an allowed confirm: %+v", got)
	}
	// One call asks only so often.
	for n := 2; n < maxConfirms; n++ {
		go func() { answers <- confirm(40+n, mine, "os_exec", "again") }()
		q = soon(t, questions, "the caller to be asked")
		q.answer <- false
		<-answers
	}
	if got := confirm(90, mine, "os_exec", "once more"); code(got) != codeTooMany {
		t.Errorf("confirm number %d of one call: %+v", maxConfirms+1, got)
	}
	if len(questions) != 0 {
		t.Error("a confirm over the limit reached the decision")
	}

	// The call ends. Its id is worth nothing afterwards.
	c.reply(req, `{"text":"done"}`)
	if err := <-done; err != nil {
		t.Fatalf("the call: %v", err)
	}
	if got := confirm(91, mine, "os_exec", "after the call"); code(got) != codeNoCall {
		t.Errorf("a confirm after its call ended: %+v", got)
	}
	if len(questions) != 0 {
		t.Error("a confirm after the call reached the decision")
	}

	h.honest.tools.release()
	if err := <-held; err != nil {
		t.Errorf("the honest machine's call: %v", err)
	}
	h.intact(t)
}

// The link drops while a hostile child's confirm is being decided: the
// decision's context ends, and whatever it then says goes nowhere.
func TestHostileConfirmThenHangsUp(t *testing.T) {
	h := newHostile(t)
	c, id := h.raw(t, "bad")
	deciding := make(chan context.Context, 1)
	done := make(chan error, 1)
	go func() {
		_, err := h.mgr.Call(ctxFor(t), id, Call{Tool: "echo", Confirm: func(ctx context.Context, _, _ string) bool {
			deciding <- ctx
			<-ctx.Done()
			return true
		}})
		done <- err
	}()
	var req mcp.Message
	for req = c.next(); req.Method != methodToolsCall; req = c.next() {
	}
	var p callParams
	_ = json.Unmarshal(req.Params, &p)
	params, _ := json.Marshal(confirmParams{CallID: p.CallID, Action: "os_exec", Detail: "x"})
	if err := c.send(`{"jsonrpc":"2.0","id":1,"method":"fleet/confirm","params":` + string(params) + `}`); err != nil {
		t.Fatal(err)
	}
	dctx := soon(t, deciding, "the master to be asked")
	_ = c.conn.Close()
	select {
	case <-dctx.Done():
	case <-time.After(waitLong):
		t.Fatal("the decision outlived the link")
	}
	if err := <-done; !errors.Is(err, ErrOutcomeUnknown) {
		t.Errorf("the call = %v", err)
	}
	h.awaitOnline(t, id, false)
	h.intact(t)
}
