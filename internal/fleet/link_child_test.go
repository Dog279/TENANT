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

// The child's own bounds: what it returns, how much it runs at once, how
// long, and what it keeps. Its master is the owner's and is not distrusted,
// but a broken one must not be able to hurt the machine.

// code is the JSON-RPC error code of err, or zero.
func code(err error) int {
	var e *mcp.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// A result is cut to what the link takes, on the child, before it is sent:
// one the session could not send would be dropped without a word.
func TestChildClipsResults(t *testing.T) {
	r := newRig(t)
	k := r.up(t, "box")
	id := k.mach.ID
	big := func(n int, fill string) Result {
		t.Helper()
		args, _ := json.Marshal(map[string]any{"N": n, "Fill": fill})
		res, err := r.call(t, id, "big", string(args))
		if err != nil {
			t.Fatalf("a result of %d x %q: %v", n, fill, err)
		}
		if !utf8.ValidString(res.Text) {
			t.Errorf("a result of %d x %q came back as something that is not text", n, fill)
		}
		return res
	}
	if res := big(maxResultText, "a"); len(res.Text) != maxResultText || res.Clipped {
		t.Errorf("a result at the limit: %d bytes, clipped=%v", len(res.Text), res.Clipped)
	}
	if res := big(6<<20, "a"); len(res.Text) != maxResultText || !res.Clipped {
		t.Errorf("a result over the limit: %d bytes, clipped=%v", len(res.Text), res.Clipped)
	}
	// Under the limit as text, over the frame as JSON: every byte becomes six.
	if res := big(maxResultText, "\x01"); len(res.Text) != maxResultText/2 || !res.Clipped {
		t.Errorf("a result that grows in JSON: %d bytes, clipped=%v", len(res.Text), res.Clipped)
	}
	// Cut between characters, never inside one.
	if res := big(maxResultText, "é"); len(res.Text) != maxResultText || !res.Clipped || !strings.HasSuffix(res.Text, "é") {
		t.Errorf("a result of two-byte characters: %d bytes, clipped=%v", len(res.Text), res.Clipped)
	}
	// Bytes that are not text come back as text.
	if res, err := r.call(t, id, "bytes", `{}`); err != nil || res.Text != "a"+repl+"b" || res.Clipped {
		t.Errorf("a result that is not UTF-8: %q, %v", res.Text, err)
	}
	// What is kept for fleet/result is the same cut result.
	res := big(3<<20, "b")
	again, err := r.mgr.Result(ctxFor(t), id, res.CallID)
	if err != nil || again != res {
		t.Errorf("the kept result: %d bytes, clipped=%v, %v", len(again.Text), again.Clipped, err)
	}
	r.echo(t, id)

	for _, tc := range []struct {
		in    string
		n     int
		want  string
		short bool
	}{
		{"abc", 3, "abc", false}, {"abc", 2, "ab", true}, {"aé", 2, "a", true}, {"é", 1, "", true}, {"", 0, "", false},
		{"a\xffb", 9, "a" + repl + "b", false}, {"\xff\xff", 3, repl, false}, {"\xffa", 3, repl, true},
	} {
		if got, short := cut(tc.in, tc.n); got != tc.want || short != tc.short {
			t.Errorf("cut(%q, %d) = %q, %v", tc.in, tc.n, got, short)
		}
	}
	// A frame too small for anything still gets an answer.
	if res, raw := fit(callResult{Text: "abc"}, 10); string(raw) != `{}` || res.Text != "" || !res.Clipped {
		t.Errorf("fit into no room = %+v, %s", res, raw)
	}
}

// With a small frame both ends agree on, results and tool lists still fit
// or are refused out loud.
func TestChildFitsASmallFrame(t *testing.T) {
	const frame = 64 << 10
	r := newRig(t, func(m *Manager) { m.ws.MaxFrameSize = frame })
	k := r.kid(t, "box", StateConfirmed)
	k.c.ws.MaxFrameSize = frame
	for i := range 600 { // more tools than a frame can list
		k.tools.fns[fmt.Sprintf("tool-%03d-%s", i, strings.Repeat("x", 100))] = k.tools.fns["echo"]
	}
	k.start()
	id := k.mach.ID
	r.awaitOnline(t, id, true)

	res, err := r.call(t, id, "big", `{"N":200000,"Fill":"a"}`)
	if err != nil || !res.Clipped || len(res.Text) != 50000 {
		t.Fatalf("a result four times the frame: %d bytes, clipped=%v, %v", len(res.Text), res.Clipped, err)
	}
	// A tool list too long for a frame is an error the master hears, not a
	// request that is never answered.
	if _, err := r.mgr.Tools(ctxFor(t), id); err == nil || !strings.Contains(err.Error(), "does not fit in a frame") {
		t.Errorf("a tool list larger than a frame = %v", err)
	}
	// Arguments too large for the link never leave the master.
	if _, err := r.call(t, id, "echo", `"`+strings.Repeat("a", frame)+`"`); err == nil || errors.Is(err, ErrOutcomeUnknown) || !strings.Contains(err.Error(), "was not sent") {
		t.Errorf("a call larger than a frame = %v", err)
	}
	r.echo(t, id)
}

// A master that sends a frame larger than the child takes loses that link,
// not the child: it dials again.
func TestChildSurvivesAFrameTooLarge(t *testing.T) {
	r := newRig(t)
	k := r.kid(t, "box", StateConfirmed)
	k.c.ws.MaxFrameSize = 64 << 10
	k.start()
	id := k.mach.ID
	r.awaitOnline(t, id, true)
	first := r.link(t, id)
	// The child hangs up at the frame's header; the rest of the write may fail.
	_ = first.sess.Notify(ctxFor(t), "x", json.RawMessage(`"`+strings.Repeat("a", 100<<10)+`"`))
	r.awaitNewLink(t, id, first)
	k.await(LinkConnected)
	r.echo(t, id)
}

// The child runs only so many calls at once, whatever its master's limit.
func TestChildLimitsCallsInFlight(t *testing.T) {
	r := newRig(t)
	k := r.kid(t, "box", StateConfirmed)
	k.c.inFlight = 2
	k.start()
	id := k.mach.ID
	r.awaitOnline(t, id, true)

	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := r.call(t, id, "hold", `{}`)
			done <- err
		}()
	}
	k.started()
	k.started()
	_, err := r.call(t, id, "echo", `1`)
	if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "this machine already runs 2 calls") {
		t.Errorf("a third call = %v, want the child's ErrBusy", err)
	}
	if _, err := r.mgr.Tools(ctxFor(t), id); err != nil {
		t.Errorf("the tool list while busy: %v", err)
	}
	k.tools.release()
	for range 2 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	r.echo(t, id)
}

// A call id is good for one call: the same one again is refused while the
// first runs and for as long as its result is kept.
func TestChildRefusesACallIDTwice(t *testing.T) {
	r := newRig(t)
	k := r.up(t, "box")
	l := r.link(t, k.mach.ID)
	call := func(tool string) (callResult, error) {
		var res callResult
		err := l.request(ctxFor(t), 0, methodToolsCall, callParams{CallID: "same", Name: tool, TimeoutMS: 20000}, &res)
		return res, err
	}
	first := make(chan error, 1)
	go func() {
		_, err := call("hold")
		first <- err
	}()
	k.started()
	if _, err := call("echo"); err == nil || !strings.Contains(err.Error(), "seen that call id") {
		t.Errorf("a call id in use = %v", err)
	}
	k.tools.release()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if _, err := call("echo"); err == nil || !strings.Contains(err.Error(), "seen that call id") {
		t.Errorf("a call id whose result is kept = %v", err)
	}
	if len(k.tools.started) != 0 {
		t.Error("a second call ran under the id")
	}
}

// What ends a tool call on the child, taken one cause at a time at the
// child's handler: over a real link a short deadline, the master's cancel
// and a dropped link arrive within milliseconds of each other and any of
// them can be first. (TestLinkDroppedMidCall has the call that outlives its
// link from end to end.)
func TestChildEndsACallAtItsDeadlineWithoutTheLink(t *testing.T) {
	r := newRig(t)
	k := r.kid(t, "box", StateConfirmed)
	k.c.runCtx = context.Background()
	call := func(l *childLink, reqCtx context.Context, callID string) (string, time.Duration) {
		t.Helper()
		start := time.Now()
		raw, err := l.HandleRequest(reqCtx, methodToolsCall, json.RawMessage(`{"call_id":"`+callID+`","name":"wait","timeout_ms":100}`))
		var res callResult
		if err != nil || json.Unmarshal(raw, &res) != nil {
			t.Fatalf("call %s = %s, %v", callID, raw, err)
		}
		return res.Text, time.Since(start)
	}
	link := func() *childLink {
		l := &childLink{c: k.c}
		l.sess = mcp.NewSession(nowhere{}, l)
		l.master.Store(LinkVersion)
		return l
	}

	// The link is gone, and the request's context with it, as the session
	// ends them: the session first. The tool runs on, and its own deadline,
	// the call's plus the grace, is what ends it.
	gone := link()
	reqCtx, drop := context.WithCancel(context.Background())
	_ = gone.sess.Close()
	drop()
	if text, took := call(gone, reqCtx, "linkgone"); text != "stopped: context deadline exceeded" || took < 100*time.Millisecond+callGrace {
		t.Errorf("a call whose link is gone ended after %v with %q", took, text)
	}
	// The link is there and the request's context ends: the master gave the
	// call up. That stops the tool at once.
	reqCtx, drop = context.WithCancel(context.Background())
	drop()
	if text, _ := call(link(), reqCtx, "givenup"); text != "stopped: context canceled" {
		t.Errorf("a call the master gave up ended with %q", text)
	}
	// Neither: the deadline alone.
	if text, _ := call(link(), context.Background(), "deadline"); text != "stopped: context deadline exceeded" {
		t.Errorf("a call left to its deadline ended with %q", text)
	}
}

// What the child answers a master that asks badly, or too early.
func TestChildRefusesBadRequests(t *testing.T) {
	r := newRig(t)
	k := r.kid(t, "box", StateConfirmed)
	ctx := context.Background()
	k.c.runCtx = ctx

	// Before the hello no tool is served: the child does not know yet
	// whether it can speak to this master.
	early := &childLink{c: k.c}
	for _, method := range []string{methodToolsList, methodToolsCall, methodResult} {
		_, err := early.HandleRequest(ctx, method, json.RawMessage(`{"call_id":"a","name":"echo"}`))
		if code(err) != codeIncompatible || !strings.Contains(err.Error(), "no hello yet") {
			t.Errorf("%s before the hello = %v", method, err)
		}
	}
	for _, params := range []string{``, `{}`, `[]`, `{"link":0}`, `{"link":-1}`, `{"link":"1"}`, `{"link":1048577}`} {
		if _, err := early.HandleRequest(ctx, methodHello, json.RawMessage(params)); code(err) != mcp.ErrInvalidParams {
			t.Errorf("hello %s = %v", params, err)
		}
	}
	if early.master.Load() != 0 {
		t.Error("a bad hello was taken")
	}

	l := &childLink{c: k.c}
	if _, err := l.HandleRequest(ctx, methodHello, json.RawMessage(`{"link":1}`)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, params string
		want           int
	}{
		{"initialize", `{}`, mcp.ErrMethodNotFound},
		{methodConfirm, `{"call_id":"a","action":"os_exec"}`, mcp.ErrMethodNotFound}, // the master does not ask the child for approval
		{noticeRevoked, ``, mcp.ErrMethodNotFound},                                   // as a request it is nothing
		{"", ``, mcp.ErrMethodNotFound},
		{methodToolsCall, ``, mcp.ErrInvalidParams},
		{methodToolsCall, `{"name":"echo"}`, mcp.ErrInvalidParams},
		{methodToolsCall, `{"call_id":"a b","name":"echo"}`, mcp.ErrInvalidParams},
		{methodToolsCall, `{"call_id":"a"}`, mcp.ErrInvalidParams},
		{methodToolsCall, `{"call_id":"a","name":"two words"}`, mcp.ErrInvalidParams},
		{methodToolsCall, `{"call_id":"a","name":"` + strings.Repeat("n", maxToolLen+1) + `"}`, mcp.ErrInvalidParams},
		{methodToolsCall, `{"call_id":7,"name":"echo"}`, mcp.ErrInvalidParams},
		{methodResult, ``, mcp.ErrInvalidParams},
		{methodResult, `{"call_id":""}`, mcp.ErrInvalidParams},
		{methodResult, `{"call_id":"../x"}`, mcp.ErrInvalidParams},
		{methodUpdate, ``, mcp.ErrInvalidParams},
		{methodUpdate, `{"version":"1","sha256":"zz","size":1,"offset":0,"data":"AA=="}`, mcp.ErrInvalidParams},
		{methodUpdate, `{"version":"","sha256":"` + fuzzPrint + `","size":1,"offset":0,"data":"AA=="}`, mcp.ErrInvalidParams},
		{methodUpdate, `{"version":"1","sha256":"` + fuzzPrint + `","size":0,"offset":0}`, mcp.ErrInvalidParams},
		{methodUpdate, `{"version":"1","sha256":"` + fuzzPrint + `","size":1,"offset":-1,"data":"AA=="}`, mcp.ErrInvalidParams},
		{methodUpdate, `{"version":"1","sha256":"` + fuzzPrint + `","size":1,"offset":1,"data":"AA=="}`, mcp.ErrInvalidParams},
		{methodUpdate, `{"version":"1","sha256":"` + fuzzPrint + `","size":2000000000,"offset":0}`, mcp.ErrInvalidParams},
		{methodUpdate, `{"version":"1\n2","sha256":"` + fuzzPrint + `","size":1,"offset":0}`, mcp.ErrInvalidParams},
		{methodUpdate, `{"version":"1","sha256":"` + fuzzPrint + `","size":1,"offset":0,"data":"not base64!"}`, mcp.ErrInvalidParams},
	} {
		if _, err := l.HandleRequest(ctx, tc.method, json.RawMessage(tc.params)); code(err) != tc.want {
			t.Errorf("%s %s = %v, want code %d", tc.method, tc.params, err, tc.want)
		}
	}
	if len(k.tools.started) != 0 {
		t.Error("a bad request ran a tool")
	}
	// A deadline of any kind is still a deadline.
	for _, ms := range []string{"0", "-5", "1", "4611686018427387904"} {
		raw, err := l.HandleRequest(ctx, methodToolsCall, json.RawMessage(`{"call_id":"t`+strings.TrimPrefix(ms, "-")+`","name":"echo","arguments":7,"timeout_ms":`+ms+`}`))
		if err != nil || string(raw) != `{"text":"7"}` {
			t.Errorf("a call with timeout_ms %s = %s, %v", ms, raw, err)
		}
	}

	// A machine with no updater says so, and one whose updater fails says why.
	if (Update{Version: "1", SHA256: fuzzPrint, Size: MaxUpdateChunk + 1, Data: make([]byte, MaxUpdateChunk+1)}).validate() == nil {
		t.Error("a piece over the chunk limit is valid")
	}
	piece := `{"version":"1","sha256":"` + fuzzPrint + `","size":1,"offset":0,"data":"AA=="}`
	k.c.cfg.Update = func(context.Context, Update) (UpdateAck, error) { return UpdateAck{}, errors.New("disk full") }
	if _, err := l.HandleRequest(ctx, methodUpdate, json.RawMessage(piece)); code(err) != codeUnavailable || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("an update that fails = %v", err)
	}
	k.c.cfg.Update = nil
	if _, err := l.HandleRequest(ctx, methodUpdate, json.RawMessage(piece)); code(err) != codeUnavailable || !strings.Contains(err.Error(), "cannot be updated over the link") {
		t.Errorf("an update with no updater = %v", err)
	}
	if _, err := l.HandleRequest(ctx, methodUpdate, json.RawMessage(`garbage`)); code(err) != codeUnavailable {
		t.Errorf("garbage with no updater = %v", err)
	}

	// A child that is stopping starts nothing.
	k.c.stopped = true
	if _, err := l.HandleRequest(ctx, methodToolsCall, json.RawMessage(`{"call_id":"z","name":"echo"}`)); code(err) != codeUnavailable {
		t.Errorf("a call to a stopping child = %v", err)
	}
	// The master validates a piece before it sends one.
	if _, err := r.mgr.Update(ctx, k.mach.ID, Update{}); err == nil || errors.Is(err, ErrOffline) {
		t.Errorf("an empty update = %v", err)
	}
}

// The table of finished calls is short and bounded three ways: by count, by
// bytes and by age.
func TestChildKeepsFewResults(t *testing.T) {
	c := &Client{running: map[string]struct{}{}, inFlight: 1 << 20}
	done := func(id, text string) {
		t.Helper()
		if err := c.begin(id); err != nil {
			t.Fatal(err)
		}
		c.finish(id, callResult{Text: text})
	}
	state := func(id string) string { return c.lookup(id).State }

	for i := range resultsKept + 5 {
		done(fmt.Sprint("n", i), "x")
	}
	if len(c.kept) != resultsKept || state("n4") != callUnknown || state("n5") != callDone || c.keptLen != resultsKept {
		t.Errorf("%d results kept, %d bytes; n4 is %s, n5 is %s", len(c.kept), c.keptLen, state("n4"), state("n5"))
	}
	// Large ones push the old out before the count does.
	large := strings.Repeat("y", 600<<10)
	for i := range 5 {
		done(fmt.Sprint("big", i), large)
	}
	if c.keptLen > resultBytesKept || state("big4") != callDone || state("big0") != callUnknown || state("n36") != callUnknown {
		t.Errorf("%d bytes kept in %d results", c.keptLen, len(c.kept))
	}
	sum := 0
	for _, k := range c.kept {
		sum += len(k.res.Text)
	}
	if sum != c.keptLen {
		t.Errorf("the table says %d bytes and holds %d", c.keptLen, sum)
	}
	// And age does, whether or not anything else arrives.
	for i := range c.kept {
		c.kept[i].at = time.Now().Add(-resultKeep - time.Second)
	}
	done("fresh", "z")
	if len(c.kept) != 1 || state("big4") != callUnknown || state("fresh") != callDone {
		t.Errorf("%d results kept after the old ones aged out", len(c.kept))
	}
	c.kept[0].at = time.Now().Add(-resultKeep - time.Second)
	if state("fresh") != callUnknown || len(c.kept) != 0 || c.keptLen != 0 {
		t.Errorf("an aged result is still there: %d kept, %d bytes", len(c.kept), c.keptLen)
	}
	// A call that is running is neither.
	if err := c.begin("now"); err != nil {
		t.Fatal(err)
	}
	if state("now") != callRunning {
		t.Errorf("a running call is %s", state("now"))
	}
	if err := c.begin("now"); code(err) != codeDuplicate {
		t.Errorf("a running call's id again = %v", err)
	}
	c.inFlight = 1
	if err := c.begin("more"); code(err) != codeBusy {
		t.Errorf("a call over the limit = %v", err)
	}
	c.finish("now", callResult{})
}
