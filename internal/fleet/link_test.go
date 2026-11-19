package fleet

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/mcp"
	"tenant/internal/mcp/transport"
	"tenant/internal/model"
)

// The link's tests run both ends for real: TLS over loopback, minted
// identities, fleet.json and child.json in temp dirs. Only the waits are
// shortened.

const (
	testPing  = 150 * time.Millisecond // a dead link is noticed in two of these
	testWatch = 25 * time.Millisecond  // how often the master re-reads fleet.json
	waitLong  = 20 * time.Second       // how long a test waits for what must happen
)

// watchLeaks fails the test if it ends with more goroutines than it began
// with. Call it first, so that it runs after every other cleanup.
func watchLeaks(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(waitLong)
		for runtime.NumGoroutine() > before {
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<20)
				t.Errorf("%d goroutines before, %d after:\n%s", before, runtime.NumGoroutine(), buf[:runtime.Stack(buf, true)])
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// counted is a connection that is counted while it is open, and whose bytes
// are counted.
type counted struct {
	net.Conn
	open  *atomic.Int64
	sent  *atomic.Int64 // may be nil
	once  sync.Once
	muted atomic.Bool // nothing gets through any more, either way, and nothing says so
}

func (c *counted) Read(p []byte) (int, error) {
	for {
		n, err := c.Conn.Read(p)
		if err != nil || !c.muted.Load() {
			return n, err
		}
	}
}

func (c *counted) Close() error {
	c.once.Do(func() { c.open.Add(-1) })
	return c.Conn.Close()
}

func (c *counted) Write(p []byte) (int, error) {
	if c.muted.Load() {
		return len(p), nil
	}
	n, err := c.Conn.Write(p)
	if c.sent != nil {
		c.sent.Add(int64(n))
	}
	return n, err
}

type countingListener struct {
	net.Listener
	open, sent *atomic.Int64
}

func (l countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.open.Add(1)
	return &counted{Conn: c, open: l.open, sent: l.sent}, nil
}

// rig is a master holding its fleet on a loopback port.
type rig struct {
	*master
	mgr  *Manager
	addr string
	// Raw connections open: the ones the master accepted, and the ones the
	// children dialed. Both are zero when a test ends.
	accepted, dialed atomic.Int64
	masterSent       atomic.Int64 // bytes the master wrote to its connections
}

// newRig starts a master. tweak changes the manager before it serves.
func newRig(t *testing.T, tweak ...func(*Manager)) *rig {
	t.Helper()
	watchLeaks(t)
	r := &rig{master: newMaster(t)}
	r.store.watchEvery = testWatch
	mgr := must(newManager(ManagerConfig{Store: r.store, Identity: r.id, Role: "test"}))(t)
	mgr.ws = transport.WSConfig{PingInterval: testPing}
	mgr.addressEvery = testWatch
	mgr.helloTime = 2 * time.Second
	for _, f := range tweak {
		f(mgr)
	}
	if err := mgr.hold(); err != nil {
		t.Fatal(err)
	}
	ln := must(net.Listen("tcp", "127.0.0.1:0"))(t)
	r.mgr, r.addr = mgr, ln.Addr().String()
	if err := r.store.SetAddresses([]string{r.addr}); err != nil {
		t.Fatal(err)
	}
	mgr.serve(countingListener{ln, &r.accepted, &r.masterSent})
	t.Cleanup(func() {
		_ = mgr.Close()
		for _, n := range []*atomic.Int64{&r.accepted, &r.dialed} {
			for deadline := time.Now().Add(waitLong); n.Load() != 0; time.Sleep(5 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Errorf("connections left open: %d accepted by the master, %d dialed by children", r.accepted.Load(), r.dialed.Load())
					return
				}
			}
		}
	})
	return r
}

// status is one machine as the manager lists it.
func (r *rig) status(t *testing.T, id string) MachineStatus {
	t.Helper()
	for _, m := range must(r.mgr.Machines())(t) {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("machine %s is not listed", id)
	return MachineStatus{}
}

// awaitOnline waits until the master has, or no longer has, a link to id.
func (r *rig) awaitOnline(t *testing.T, id string, online bool) MachineStatus {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for {
		list, err := r.mgr.Machines()
		for _, m := range list {
			if m.ID == id && m.Online == online {
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("machine %s did not become online=%v (%v)", id, online, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitNewLink waits until the master's link to id is another than old.
func (r *rig) awaitNewLink(t *testing.T, id string, old *link) *link {
	t.Helper()
	for deadline := time.Now().Add(waitLong); ; time.Sleep(5 * time.Millisecond) {
		r.mgr.mu.Lock()
		l := r.mgr.links[id]
		r.mgr.mu.Unlock()
		if l != nil && l != old {
			return l
		}
		if time.Now().After(deadline) {
			t.Fatalf("no new link to %s", id)
		}
	}
}

// link is the manager's live link to id.
func (r *rig) link(t *testing.T, id string) *link {
	t.Helper()
	r.mgr.mu.Lock()
	defer r.mgr.mu.Unlock()
	l := r.mgr.links[id]
	if l == nil {
		t.Fatalf("no link to %s", id)
	}
	return l
}

// call runs a tool on a machine with a deadline.
func (r *rig) call(t *testing.T, id, tool, args string) (Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	return r.mgr.Call(ctx, id, Call{Tool: tool, Arguments: json.RawMessage(args)})
}

// echo proves a machine answers calls.
func (r *rig) echo(t *testing.T, id string) {
	t.Helper()
	if res, err := r.call(t, id, "echo", `"still here"`); err != nil || res.Text != `"still here"` {
		t.Fatalf("echo on %s = %+v, %v", id, res, err)
	}
}

// fakeTools is a child's tool mux.
type fakeTools struct {
	started chan model.ToolCall  // every call, as it starts
	ctxs    chan context.Context // the context of every "keep" call
	fns     map[string]func(ctx context.Context, args json.RawMessage) (string, bool, error)

	mu   sync.Mutex
	held chan struct{} // closed to let every "hold" finish
}

// hold is what a "hold" call waits on.
func (f *fakeTools) hold() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held
}

// release lets every "hold" call that is running finish.
func (f *fakeTools) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	close(f.held)
	f.held = make(chan struct{})
}

func newFakeTools() *fakeTools {
	f := &fakeTools{started: make(chan model.ToolCall, 1024), ctxs: make(chan context.Context, 16), held: make(chan struct{})}
	f.fns = map[string]func(context.Context, json.RawMessage) (string, bool, error){
		"echo": func(_ context.Context, args json.RawMessage) (string, bool, error) { return string(args), false, nil },
		"fail": func(context.Context, json.RawMessage) (string, bool, error) { return "it failed", true, nil },
		"err": func(context.Context, json.RawMessage) (string, bool, error) {
			return "", false, errors.New("no such thing")
		},
		"wait": func(ctx context.Context, _ json.RawMessage) (string, bool, error) {
			<-ctx.Done()
			return "stopped: " + ctx.Err().Error(), true, nil
		},
		"hold": func(ctx context.Context, _ json.RawMessage) (string, bool, error) {
			select {
			case <-f.hold():
				return "held", false, nil
			case <-ctx.Done():
				return "stopped: " + ctx.Err().Error(), true, nil
			}
		},
		"ask": func(ctx context.Context, args json.RawMessage) (string, bool, error) {
			var a struct{ Action, Detail string }
			_ = json.Unmarshal(args, &a)
			if Confirm(ctx, a.Action, a.Detail) {
				return "allowed", false, nil
			}
			return "denied", false, nil
		},
		"big": func(_ context.Context, args json.RawMessage) (string, bool, error) {
			var a struct {
				N    int
				Fill string
			}
			_ = json.Unmarshal(args, &a)
			return strings.Repeat(a.Fill, a.N), false, nil
		},
		"panic": func(context.Context, json.RawMessage) (string, bool, error) { panic("boom") },
		"keep": func(ctx context.Context, _ json.RawMessage) (string, bool, error) {
			f.ctxs <- ctx
			return "kept", false, nil
		},
	}
	return f
}

func (f *fakeTools) All() []model.ToolSpec {
	out := make([]model.ToolSpec, 0, len(f.fns))
	for name := range f.fns {
		out = append(out, model.ToolSpec{Name: name, Description: "the " + name + " tool", Parameters: json.RawMessage(`{"type":"object"}`), Gated: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (f *fakeTools) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	fn := f.fns[call.Name]
	if fn == nil {
		return "", false, fmt.Errorf("unknown tool %q", call.Name)
	}
	f.started <- call
	return fn(ctx, call.Arguments)
}

// kid is a child machine: its files in a temp dir and its client.
type kid struct {
	t     *testing.T
	r     *rig
	mach  Machine
	id    Identity
	dir   string
	tools *fakeTools
	c     *Client

	cancel context.CancelFunc
	done   chan error // what Run returned

	mu    sync.Mutex
	conns []*counted // every raw connection the client dialed
	dials atomic.Int64
}

// kid enrolls a machine without the network, brings it to state and writes
// its child.json. It is not started.
func (r *rig) kid(t *testing.T, name string, state State) *kid {
	t.Helper()
	dir := t.TempDir()
	id := must(MintIdentity(ChildIdentityPath(dir)))(t)
	mach := r.addIdentity(t, name, state, id)
	child := Child{ID: mach.ID, Name: mach.Name, MasterAddresses: []string{r.addr}, MasterFingerprint: r.id.Fingerprint,
		Sandbox: "host", Ceiling: CeilingFull}
	if err := SaveChild(ChildPath(dir), child); err != nil {
		t.Fatal(err)
	}
	return r.kidIn(t, dir, mach)
}

// kidIn makes the client of the machine whose files are in dir.
func (r *rig) kidIn(t *testing.T, dir string, mach Machine) *kid {
	t.Helper()
	k := &kid{t: t, r: r, mach: mach, dir: dir, tools: newFakeTools()}
	k.id = must(LoadIdentity(ChildIdentityPath(dir)))(t)
	k.client()
	return k
}

// client makes a new client over the kid's files, as a restart of the child
// process would.
func (k *kid) client() *Client {
	k.t.Helper()
	c := must(NewClient(ClientConfig{Dir: k.dir, Tools: k.tools, Version: "1.2.3", User: "root",
		Update: func(_ context.Context, u Update) (UpdateAck, error) {
			return UpdateAck{Received: u.Offset + int64(len(u.Data)), Applying: u.Offset+int64(len(u.Data)) == u.Size}, nil
		}}))(k.t)
	c.ws = transport.WSConfig{PingInterval: testPing}
	c.backoffMin, c.backoffMax, c.dialTime = 10*time.Millisecond, 100*time.Millisecond, 5*time.Second
	c.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		k.dials.Add(1)
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		k.r.dialed.Add(1)
		cc := &counted{Conn: conn, open: &k.r.dialed}
		k.mu.Lock()
		k.conns = append(k.conns, cc)
		k.mu.Unlock()
		return cc, nil
	}
	k.c = c
	return c
}

// start runs the client until the test ends or stop is called.
func (k *kid) start() *kid {
	k.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	k.cancel, k.done = cancel, make(chan error, 1)
	c := k.c
	go func() { k.done <- c.Run(ctx) }()
	k.t.Cleanup(k.stop)
	return k
}

// stop ends the client and waits for Run to return.
func (k *kid) stop() {
	if k.cancel == nil {
		return
	}
	k.cancel()
	select {
	case err := <-k.done:
		k.done <- err
	case <-time.After(waitLong):
		k.t.Errorf("the client of %s did not stop", k.mach.Name)
	}
	k.cancel = nil
}

// kill cuts the connection under the link, as a network that died would.
func (k *kid) kill() {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, c := range k.conns {
		_ = c.Close()
	}
	k.conns = nil
}

// silence makes the kid's connections go dead without closing: nothing
// arrives and nothing leaves, as when a cable is pulled.
func (k *kid) silence() {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, c := range k.conns {
		c.muted.Store(true)
	}
}

// await waits until the client is in state.
func (k *kid) await(state LinkState) ChildStatus {
	k.t.Helper()
	deadline := time.Now().Add(waitLong)
	for {
		st := k.c.Status()
		if st.State == state {
			return st
		}
		if time.Now().After(deadline) {
			k.t.Fatalf("%s is %q (%s), not %q", k.mach.Name, st.State, st.Detail, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitDisk waits until child-link.json says state: the file is written a
// moment after the client's own view changes.
func (k *kid) awaitDisk(state LinkState) ChildStatus {
	k.t.Helper()
	for deadline := time.Now().Add(waitLong); ; time.Sleep(5 * time.Millisecond) {
		st, err := ReadChildStatus(k.dir)
		if err == nil && st.State == state {
			return st
		}
		if time.Now().After(deadline) {
			k.t.Fatalf("child-link.json of %s = %+v, %v; want state %q", k.mach.Name, st, err, state)
		}
	}
}

// up starts a confirmed machine and waits until both ends have the link.
func (r *rig) up(t *testing.T, name string) *kid {
	t.Helper()
	k := r.kid(t, name, StateConfirmed).start()
	k.await(LinkConnected)
	r.awaitOnline(t, k.mach.ID, true)
	return k
}

// started waits for a tool call to start on the kid and returns it.
func (k *kid) started() model.ToolCall {
	k.t.Helper()
	select {
	case call := <-k.tools.started:
		return call
	case <-time.After(waitLong):
		k.t.Fatalf("no tool call started on %s", k.mach.Name)
		return model.ToolCall{}
	}
}

// --- the life of a link ---

// A machine enrolls over the fleet port, is told to wait and nothing else,
// is confirmed, connects, and is called.
func TestLinkFromEnrollmentToFirstCall(t *testing.T) {
	r := newRig(t)
	code := must(r.store.CreateEnrollment(r.id.Fingerprint, "box", 0, ""))(t)
	dir := t.TempDir()
	id := must(MintIdentity(ChildIdentityPath(dir)))(t)
	child, digits, err := Enroll(ctxFor(t), code, id, linuxBox)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveChild(ChildPath(dir), child); err != nil {
		t.Fatal(err)
	}
	k := r.kidIn(t, dir, must(r.store.Machine(child.ID))(t)).start()

	// Pending: the answer is "waiting for confirmation", the child keeps
	// asking, and the master has no link and makes no call.
	st := k.await(LinkWaiting)
	if st.Detail != WaitingForConfirmation || st.Address != r.addr {
		t.Errorf("a pending machine's status = %+v", st)
	}
	for start := k.dials.Load(); k.dials.Load() < start+3; time.Sleep(5 * time.Millisecond) {
	}
	if m := r.status(t, child.ID); m.Online || m.State != StatePending {
		t.Errorf("a pending machine is listed as %+v", m)
	}
	if _, err := r.call(t, child.ID, "echo", `1`); !errors.Is(err, ErrPending) {
		t.Errorf("call to a pending machine = %v, want ErrPending", err)
	}
	if len(k.tools.started) != 0 {
		t.Error("a pending machine was sent a call")
	}

	// Confirmed: it connects by itself.
	must(r.store.Confirm(r.id.Fingerprint, child.ID, digits))(t)
	k.await(LinkConnected)
	m := r.awaitOnline(t, child.ID, true)
	if m.Link != LinkVersion || m.Since.IsZero() || m.Remote == "" || m.LastError != "" {
		t.Errorf("a connected machine is listed as %+v", m)
	}
	// The hello told the master what the machine is now.
	want := Reported{OS: runtime.GOOS, User: "root", Version: "1.2.3", Sandbox: "host", Ceiling: "full"}
	if got := must(r.store.Machine(child.ID))(t).Reported; got != want {
		t.Errorf("reported after the hello = %+v, want %+v", got, want)
	}

	res, err := r.call(t, child.ID, "echo", `{"x":"<1 & 2>"}`)
	if err != nil || res.Text != `{"x":"<1 & 2>"}` || res.IsError || res.Clipped || !validCallID(res.CallID) {
		t.Fatalf("first call = %+v, %v", res, err)
	}
	if call := k.started(); call.ID != res.CallID || call.Name != "echo" {
		t.Errorf("the tool saw call %+v, the master %s", call, res.CallID)
	}

	// The tool list is the child's, and what it marks for itself (Gated)
	// does not cross the link.
	tools, err := r.mgr.Tools(ctxFor(t), child.ID)
	if err != nil || len(tools) != len(k.tools.fns) {
		t.Fatalf("Tools = %d tools, %v", len(tools), err)
	}
	for _, tool := range tools {
		if k.tools.fns[tool.Name] == nil || tool.Description != "the "+tool.Name+" tool" || string(tool.Parameters) != `{"type":"object"}` || tool.Gated {
			t.Errorf("tool %+v", tool)
		}
	}
	if d, err := r.mgr.Ping(ctxFor(t), child.ID); err != nil || d < 0 {
		t.Errorf("Ping = %v, %v", d, err)
	}
	if err := k.c.Ping(ctxFor(t)); err != nil {
		t.Errorf("the child's ping of the master: %v", err)
	}

	// The child's status is on disk for another process to read.
	if disk := k.awaitDisk(LinkConnected); disk.Address != r.addr || disk.Certificate != id.Fingerprint || disk.Since.IsZero() {
		t.Errorf("child-link.json = %+v", disk)
	}
}

// What a tool returns, however it ends, comes back as a result.
func TestLinkCallResults(t *testing.T) {
	r := newRig(t)
	k := r.up(t, "box")
	for _, tc := range []struct {
		tool, args, text string
		isError          bool
	}{
		{"echo", `[1,2]`, `[1,2]`, false},
		{"fail", `{}`, "it failed", true},
		{"err", `{}`, "no such thing", true},
		{"nope", `{}`, `unknown tool "nope"`, true},
		{"panic", `{}`, "fleet: the tool panicked: boom", true},
		{"echo", ``, ``, false}, // no arguments at all
	} {
		res, err := r.call(t, k.mach.ID, tc.tool, tc.args)
		if err != nil || res.Text != tc.text || res.IsError != tc.isError {
			t.Errorf("%s(%s) = %+v, %v", tc.tool, tc.args, res, err)
		}
	}
	// Arguments that are not JSON never leave the master.
	if _, err := r.call(t, k.mach.ID, "echo", `{not json`); err == nil || errors.Is(err, ErrOutcomeUnknown) {
		t.Errorf("a call with broken arguments = %v", err)
	}
	// A tool name that is not one is refused by the child, not dispatched.
	if _, err := r.call(t, k.mach.ID, "two words", `{}`); err == nil {
		t.Error("a tool name with a space in it was accepted")
	}
	r.echo(t, k.mach.ID)
}

// Many calls at once, a limit on how many, a deadline on each, and a cancel
// that reaches the tool.
func TestLinkConcurrencyLimitDeadlineAndCancel(t *testing.T) {
	r := newRig(t)
	k := r.up(t, "box")
	id := k.mach.ID

	// As many as the limit run at once.
	type outcome struct {
		res Result
		err error
	}
	held := make(chan outcome, defaultInFlight)
	for range defaultInFlight {
		go func() {
			res, err := r.call(t, id, "hold", `{}`)
			held <- outcome{res, err}
		}()
	}
	ids := map[string]bool{}
	for range defaultInFlight {
		ids[k.started().ID] = true
	}
	if len(ids) != defaultInFlight {
		t.Errorf("%d calls ran under %d ids", defaultInFlight, len(ids))
	}
	if m := r.status(t, id); m.InFlight != defaultInFlight {
		t.Errorf("in flight = %d", m.InFlight)
	}
	// One more is refused at once, and nothing is queued for later.
	start := time.Now()
	if _, err := r.call(t, id, "echo", `1`); !errors.Is(err, ErrBusy) || time.Since(start) > time.Second {
		t.Errorf("a call over the limit = %v after %v, want ErrBusy at once", err, time.Since(start))
	}
	// Pings and the tool list are not calls.
	if _, err := r.mgr.Ping(ctxFor(t), id); err != nil {
		t.Errorf("ping at the limit: %v", err)
	}
	k.tools.release()
	for range defaultInFlight {
		if o := <-held; o.err != nil || o.res.Text != "held" || !ids[o.res.CallID] {
			t.Errorf("held call = %+v, %v", o.res, o.err)
		}
	}
	if len(k.tools.started) != 0 {
		t.Error("the refused call was run after all")
	}
	if m := r.status(t, id); m.InFlight != 0 {
		t.Errorf("in flight after all returned = %d", m.InFlight)
	}

	// A deadline ends the call on the master and the tool on the child.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	_, err := r.mgr.Call(ctx, id, Call{Tool: "wait"})
	cancel()
	var ce *CallError
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &ce) || ce.Machine != id {
		t.Fatalf("a call past its deadline = %v", err)
	}
	waitID := k.started().ID
	if ce.CallID != waitID {
		t.Errorf("the error names call %s, the tool ran %s", ce.CallID, waitID)
	}
	awaitResult(t, r, id, waitID, "stopped: context canceled")

	// A caller that gives up stops the tool too.
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.mgr.Call(ctx, id, Call{Tool: "wait"})
		done <- err
	}()
	waitID = k.started().ID
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled call = %v", err)
	}
	awaitResult(t, r, id, waitID, "stopped: context canceled")

	// A caller with no deadline gets the manager's.
	r.mgr.callTime = 100 * time.Millisecond
	if _, err := r.mgr.Call(context.Background(), id, Call{Tool: "wait"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a call with no deadline of its own = %v", err)
	}
	awaitResult(t, r, id, k.started().ID, "stopped: context canceled")
	r.echo(t, id)
}

// awaitResult waits until the machine reports a finished call with text.
func awaitResult(t *testing.T, r *rig, id, callID, text string) Result {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for {
		res, err := r.mgr.Result(ctxFor(t), id, callID)
		if err == nil {
			if res.Text != text || res.CallID != callID {
				t.Fatalf("result of %s = %+v, want %q", callID, res, text)
			}
			return res
		}
		if time.Now().After(deadline) {
			t.Fatalf("result of %s: %v", callID, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A call to a machine with no link fails at once and says since when.
func TestLinkOfflineFailsAtOnce(t *testing.T) {
	r := newRig(t)
	k := r.kid(t, "box", StateConfirmed)
	id := k.mach.ID

	start := time.Now()
	_, err := r.call(t, id, "echo", `1`)
	var off *OfflineError
	if !errors.Is(err, ErrOffline) || !errors.As(err, &off) || !off.Since.IsZero() || time.Since(start) > time.Second {
		t.Fatalf("call to a machine never seen = %v", err)
	}
	if !strings.Contains(err.Error(), "not seen since this master started at") {
		t.Errorf("error = %q", err)
	}
	for _, try := range []func() error{
		func() error { _, err := r.mgr.Tools(ctxFor(t), id); return err },
		func() error { _, err := r.mgr.Ping(ctxFor(t), id); return err },
		func() error { _, err := r.mgr.Result(ctxFor(t), id, "abc"); return err },
		func() error {
			_, err := r.mgr.Update(ctxFor(t), id, Update{Version: "2", SHA256: fuzzPrint, Size: 1, Data: []byte{1}})
			return err
		},
	} {
		if err := try(); !errors.Is(err, ErrOffline) {
			t.Errorf("request to an offline machine = %v", err)
		}
	}
	if _, err := r.call(t, "m0000000000000000", "echo", `1`); !errors.Is(err, ErrUnknown) {
		t.Errorf("call to no machine = %v", err)
	}

	k.start()
	r.awaitOnline(t, id, true)
	before := time.Now().Add(-time.Second)
	k.stop()
	m := r.awaitOnline(t, id, false)
	if m.Since.Before(before) || m.LastError == "" {
		t.Errorf("after the link ended the machine is listed as %+v", m)
	}
	_, err = r.call(t, id, "echo", `1`)
	if !errors.As(err, &off) || !off.Since.Equal(m.Since) || !strings.Contains(err.Error(), "is offline since "+m.Since.Format(time.DateTime)) {
		t.Errorf("call to a machine that went away = %v", err)
	}
	if len(k.tools.started) != 0 {
		t.Error("something was queued for an offline machine")
	}
}

// The link dies under a call: the caller is told the outcome is unknown,
// with the call's id, nothing is sent again, the call runs on, and once the
// machine is back fleet/result has what became of it.
func TestLinkDroppedMidCall(t *testing.T) {
	r := newRig(t)
	k := r.up(t, "box")
	id := k.mach.ID

	done := make(chan error, 1)
	go func() {
		_, err := r.call(t, id, "hold", `{}`)
		done <- err
	}()
	running := k.started()
	first := r.link(t, id)
	k.kill()

	err := <-done
	var ce *CallError
	if !errors.Is(err, ErrOutcomeUnknown) || !errors.As(err, &ce) || ce.CallID != running.ID {
		t.Fatalf("a call whose link dropped = %v, want ErrOutcomeUnknown with call id %s", err, running.ID)
	}

	// The child dials again by itself. The call is still running there.
	r.awaitNewLink(t, id, first)
	if m := r.status(t, id); m.InFlight != 0 {
		t.Errorf("the new link starts with %d calls in flight", m.InFlight)
	}
	if _, err := r.mgr.Result(ctxFor(t), id, running.ID); !errors.Is(err, ErrCallRunning) {
		t.Fatalf("Result while the call runs = %v, want ErrCallRunning", err)
	}
	if n := len(k.tools.started); n != 0 {
		t.Fatalf("the call was sent again: %d more started", n)
	}
	// It finishes; the master can have the result now.
	k.tools.release()
	res := awaitResult(t, r, id, running.ID, "held")
	if res.IsError {
		t.Errorf("result = %+v", res)
	}
	if _, err := r.mgr.Result(ctxFor(t), id, "neverhappened"); !errors.Is(err, ErrCallUnknown) {
		t.Errorf("Result of a call that never was = %v", err)
	}
	if _, err := r.mgr.Result(ctxFor(t), id, "not an id"); !errors.Is(err, ErrCallUnknown) {
		t.Errorf("Result of a malformed id = %v", err)
	}
	r.echo(t, id)
}

// A link that goes dead without a word, as when a cable is pulled, is
// noticed by both ends within two ping intervals; a call in flight ends with
// the outcome unknown, and the child dials again.
func TestLinkSilenceIsNoticed(t *testing.T) {
	r := newRig(t)
	k := r.up(t, "box")
	id := k.mach.ID
	first := r.link(t, id)
	done := make(chan error, 1)
	go func() {
		_, err := r.call(t, id, "wait", `{}`)
		done <- err
	}()
	k.started()

	start := time.Now()
	k.silence()
	if err := <-done; !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("a call on a link that went silent = %v", err)
	}
	// Two intervals, give or take the quarter the accepting side waits longer.
	if took := time.Since(start); took < testPing || took > 2*testPing+2*time.Second {
		t.Errorf("the master noticed the silence after %v (ping interval %v)", took, testPing)
	}
	if !strings.Contains(first.reason(), "no data from the peer") {
		t.Errorf("the link ended because: %s", first.reason())
	}
	r.awaitNewLink(t, id, first)
	k.await(LinkConnected)
	r.echo(t, id)
}

// Revoking a machine closes the socket it holds within about a second, and
// the machine stops dialing and remembers why.
func TestLinkRevokeIsLive(t *testing.T) {
	r := newRig(t, func(m *Manager) { m.store.watchEvery = time.Second }) // the real interval
	k := r.up(t, "box")
	id := k.mach.ID
	hold := make(chan error, 1)
	go func() {
		_, err := r.call(t, id, "hold", `{}`)
		hold <- err
	}()
	k.started()

	start := time.Now()
	if err := r.store.Revoke(id); err != nil {
		t.Fatal(err)
	}
	r.awaitOnline(t, id, false)
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("the revoked machine's socket was closed after %v", took)
	}
	if err := <-hold; !errors.Is(err, ErrOutcomeUnknown) {
		t.Errorf("a call in flight to a machine that was revoked = %v", err)
	}
	if m := r.status(t, id); m.State != StateRevoked || !strings.Contains(m.LastError, "revoked") {
		t.Errorf("listed as %+v", m)
	}
	if _, err := r.call(t, id, "echo", `1`); !errors.Is(err, ErrRevoked) {
		t.Errorf("call to a revoked machine = %v", err)
	}

	// The child was told, stopped, and wrote it down.
	select {
	case err := <-k.done:
		k.done <- err
		if !errors.Is(err, ErrRevoked) {
			t.Fatalf("Run returned %v, want ErrRevoked", err)
		}
	case <-time.After(waitLong):
		t.Fatal("the revoked child is still running")
	}
	if st := k.awaitDisk(LinkRevoked); !strings.Contains(st.Detail, "the master rejected this machine") {
		t.Errorf("child-link.json = %+v", st)
	}
	// It does not dial again, now or after a restart of the child.
	dials := k.dials.Load()
	time.Sleep(300 * time.Millisecond)
	again := k.client()
	if err := again.Run(context.Background()); !errors.Is(err, ErrRevoked) {
		t.Errorf("Run after a revoke = %v", err)
	}
	if again.Status().State != LinkRevoked {
		t.Errorf("status after a restart = %+v", again.Status())
	}
	if k.dials.Load() != dials {
		t.Errorf("a revoked child dialed %d more times", k.dials.Load()-dials)
	}

	// Enrolled again, the machine has a new certificate, and the old
	// "revoked" is not about it.
	id2 := must(MintIdentity(ChildIdentityPath(k.dir)))(t)
	mach2 := r.addIdentity(t, "box2", StateConfirmed, id2)
	if err := UpdateChild(ChildPath(k.dir), func(c *Child) error { c.ID, c.Name = mach2.ID, mach2.Name; return nil }); err != nil {
		t.Fatal(err)
	}
	k2 := r.kidIn(t, k.dir, mach2).start()
	k2.await(LinkConnected)
	r.awaitOnline(t, mach2.ID, true)
	r.echo(t, mach2.ID)
}

// A machine revoked while it was away is refused at its next handshake. It
// says the master rejected it, and keeps trying slowly: the same refusal is
// what a master that lost its fleet.json gives, and that one comes back.
func TestLinkRevokedWhileOffline(t *testing.T) {
	r := newRig(t)
	k := r.kid(t, "box", StateConfirmed)
	if err := r.store.Revoke(k.mach.ID); err != nil {
		t.Fatal(err)
	}
	k.start()
	st := k.await(LinkRejected)
	if !strings.Contains(st.Detail, "the master rejected this machine") {
		t.Errorf("status = %+v", st)
	}
	k.awaitDisk(LinkRejected)
	for start := k.dials.Load(); k.dials.Load() < start+2; time.Sleep(5 * time.Millisecond) {
	}
	select {
	case err := <-k.done:
		t.Fatalf("a machine refused at the handshake stopped for good: %v", err)
	default:
	}
	if m := r.status(t, k.mach.ID); m.Online {
		t.Error("a revoked machine has a link")
	}
}

// Pause closes the link and stops calls; resume lets the machine back.
func TestLinkPauseAndResume(t *testing.T) {
	r := newRig(t)
	k := r.up(t, "box")
	id := k.mach.ID
	if err := r.store.Pause(id); err != nil {
		t.Fatal(err)
	}
	m := r.awaitOnline(t, id, false)
	if !strings.Contains(m.LastError, "paused") {
		t.Errorf("last error = %q", m.LastError)
	}
	st := k.await(LinkPaused)
	if !strings.Contains(st.Detail, "paused") {
		t.Errorf("status = %+v", st)
	}
	if _, err := r.call(t, id, "echo", `1`); !errors.Is(err, ErrPaused) {
		t.Errorf("call to a paused machine = %v", err)
	}
	if err := r.store.Resume(id); err != nil {
		t.Fatal(err)
	}
	k.await(LinkConnected)
	r.awaitOnline(t, id, true)
	r.echo(t, id)
}

// While fleet.json cannot be read the fleet refuses every call, and what a
// child sends is dropped; when it can be read again the same link carries on.
func TestLinkUnreadableStoreRefusesCalls(t *testing.T) {
	r := newRig(t)
	k := r.up(t, "box")
	id := k.mach.ID
	good := must(os.ReadFile(StorePath(r.dir)))(t)

	approved := make(chan string, 8)
	asked := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitLong)
		defer cancel()
		res, err := r.mgr.Call(ctx, id, Call{Tool: "hold", Confirm: func(_ context.Context, action, _ string) bool {
			approved <- action
			return true
		}})
		if err == nil && res.Text != "held" {
			err = fmt.Errorf("result %+v", res)
		}
		asked <- err
	}()
	held := k.started()

	writeRaw(t, StorePath(r.dir), []byte(`{"version":1,"machines":{`))
	if _, err := r.call(t, id, "echo", `1`); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("call while fleet.json is broken = %v, want ErrUnreadable", err)
	}
	if _, err := r.mgr.Machines(); !errors.Is(err, ErrUnreadable) {
		t.Errorf("Machines = %v", err)
	}
	if _, err := r.mgr.Ping(ctxFor(t), id); !errors.Is(err, ErrUnreadable) {
		t.Errorf("Ping = %v", err)
	}
	// A confirm that arrives now is not decided: nothing is approved.
	l := r.link(t, id)
	raw := must(encode(confirmParams{CallID: held.ID, Action: "os_exec", Detail: "x"}))(t)
	if _, err := l.HandleRequest(context.Background(), methodConfirm, raw); err == nil {
		t.Error("a confirm was decided while fleet.json could not be read")
	}
	g := &guard{l: l, tokens: requestBurst, refilled: time.Now()}
	if pass, err := g.admit([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); pass || err != nil {
		t.Errorf("a frame was let through while fleet.json could not be read: %v, %v", pass, err)
	}
	if len(approved) != 0 {
		t.Error("something was approved")
	}

	writeRaw(t, StorePath(r.dir), good)
	r.echo(t, id) // the same link: it was never closed
	if m := r.status(t, id); !m.Online {
		t.Error("the link did not survive")
	}
	k.tools.release()
	if err := <-asked; err != nil {
		t.Errorf("the call in flight across the outage: %v", err)
	}
}

// A changed address list reaches a connected child within moments, is in its
// child.json after a restart, and every address in it is tried.
func TestLinkAddressPush(t *testing.T) {
	r := newRig(t)
	k := r.up(t, "box")
	// An address nothing listens on, first in the list.
	dead := must(net.Listen("tcp", "127.0.0.1:0"))(t)
	deadAddr := dead.Addr().String()
	_ = dead.Close()
	want := []string{deadAddr, r.addr}
	if err := r.store.SetAddresses(want); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(waitLong); ; time.Sleep(5 * time.Millisecond) {
		if c, err := LoadChild(ChildPath(k.dir)); err == nil && slices.Equal(c.MasterAddresses, want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child.json never got the new addresses: %+v", must(LoadChild(ChildPath(k.dir)))(t))
		}
	}
	// The rest of child.json is as it was.
	if c := must(LoadChild(ChildPath(k.dir)))(t); c.ID != k.mach.ID || c.Ceiling != CeilingFull || c.MasterFingerprint != r.id.Fingerprint {
		t.Errorf("child.json after the push = %+v", c)
	}

	// The child process restarts: it has the list, tries the dead address,
	// and gets through on the next.
	k.stop()
	r.awaitOnline(t, k.mach.ID, false)
	k.client()
	dials := k.dials.Load()
	k.start()
	st := k.await(LinkConnected)
	if st.Address != r.addr || k.dials.Load() < dials+2 {
		t.Errorf("connected to %s after %d dials", st.Address, k.dials.Load()-dials)
	}
	r.awaitOnline(t, k.mach.ID, true)
	r.echo(t, k.mach.ID)

	// A list a child cannot save is still dialed by that process.
	c := k.c
	if err := os.Remove(ChildPath(k.dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.addresses(json.RawMessage(`{"addresses":["10.9.8.7:1"]}`)); err == nil {
		t.Error("an address list that could not be saved was acknowledged")
	}
	if !slices.Equal(c.pushed, []string{"10.9.8.7:1"}) {
		t.Errorf("pushed = %v", c.pushed)
	}
	for _, bad := range []string{`{"addresses":[]}`, `{"addresses":["no port"]}`, `{}`, `[]`, `{"addresses":"10.0.0.1:1"}`} {
		if _, err := c.addresses(json.RawMessage(bad)); err == nil {
			t.Errorf("address list %s was accepted", bad)
		}
	}
}

// Two ends whose link versions do not go together still say hello, ping and
// update; tool calls are refused, by whichever end is the newer, and the
// refusal says why.
func TestLinkVersionMismatch(t *testing.T) {
	update := Update{Version: "9.9.9", SHA256: fuzzPrint, Size: 4, Offset: 0, Data: []byte("abcd")}
	check := func(t *testing.T, r *rig, k *kid, childLink int) {
		id := k.mach.ID
		if m := r.status(t, id); !m.Online || m.Link != childLink {
			t.Fatalf("listed as %+v", m)
		}
		if _, err := r.mgr.Ping(ctxFor(t), id); err != nil {
			t.Errorf("ping: %v", err)
		}
		if ack, err := r.mgr.Update(ctxFor(t), id, update); err != nil || ack != (UpdateAck{Received: 4, Applying: true}) {
			t.Errorf("update = %+v, %v", ack, err)
		}
		_, err := r.call(t, id, "echo", `1`)
		if !errors.Is(err, ErrIncompatible) || !strings.Contains(err.Error(), "link version") {
			t.Errorf("call = %v, want ErrIncompatible", err)
		}
		if _, err := r.mgr.Tools(ctxFor(t), id); !errors.Is(err, ErrIncompatible) {
			t.Errorf("tools = %v", err)
		}
		if _, err := r.mgr.Result(ctxFor(t), id, "abc"); !errors.Is(err, ErrIncompatible) {
			t.Errorf("result = %v", err)
		}
		if len(k.tools.started) != 0 {
			t.Error("a tool ran across a version mismatch")
		}
	}
	t.Run("the child is two versions ahead", func(t *testing.T) {
		r := newRig(t)
		k := r.kid(t, "box", StateConfirmed)
		k.c.linkVersion = 3
		k.start()
		r.awaitOnline(t, k.mach.ID, true)
		check(t, r, k, 3)
		if st := k.c.Status(); st.State != LinkConnected || !strings.Contains(st.Detail, "link version 3") {
			t.Errorf("child status = %+v", st)
		}
		for deadline := time.Now().Add(waitLong); ; time.Sleep(5 * time.Millisecond) {
			if disk, err := ReadChildStatus(k.dir); err == nil && strings.Contains(disk.Detail, "link version 3") {
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("child-link.json = %+v, %v", disk, err)
			}
		}
	})
	t.Run("the child is two versions behind", func(t *testing.T) {
		r := newRig(t, func(m *Manager) { m.linkVersion = 3 })
		k := r.up(t, "box")
		check(t, r, k, 1)
	})
	t.Run("one version apart is spoken to", func(t *testing.T) {
		r := newRig(t, func(m *Manager) { m.linkVersion = 2 })
		k := r.up(t, "box")
		r.echo(t, k.mach.ID)
	})
	for _, tc := range []struct {
		mine, peer int
		want       bool
	}{{1, 1, true}, {1, 0, false}, {1, -1, false}, {2, 1, true}, {3, 1, false}, {1, 3, true}, {3, 2, true}} {
		if got := speaks(tc.mine, tc.peer); got != tc.want {
			t.Errorf("speaks(%d, %d) = %v", tc.mine, tc.peer, got)
		}
	}
}

// One process holds a fleet. A second one learns that it does not, and which
// process does.
func TestSecondManagerLearnsWhoHoldsTheFleet(t *testing.T) {
	r := newRig(t)
	cfg := ManagerConfig{Store: Open(r.dir), Identity: r.id, Addr: r.addr, Role: "tui"}
	_, err := StartManager(cfg)
	var held *HeldError
	if !errors.As(err, &held) {
		t.Fatalf("a second manager on the fleet = %v, want a HeldError", err)
	}
	if held.Holder.PID != os.Getpid() || held.Holder.Role != "test" || held.Holder.Address != r.addr || held.Holder.Since.IsZero() {
		t.Errorf("holder = %+v", held.Holder)
	}
	if want := fmt.Sprintf("tenant test (process %d) holds the fleet, on %s since ", os.Getpid(), r.addr); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to say %q", err, want)
	}
	// On another port too: it is the fleet that is held, not the port.
	cfg.Addr = "127.0.0.1:0"
	if _, err := StartManager(cfg); !errors.As(err, &held) {
		t.Errorf("a second manager on another port = %v", err)
	}

	// The first lets go: the second can hold it, and the first's children
	// come to it.
	k := r.up(t, "box")
	_ = r.mgr.Close()
	cfg.Addr = r.addr
	second, err := StartManager(cfg)
	if err != nil {
		t.Fatalf("after the holder closed: %v", err)
	}
	defer second.Close()
	if second.Addr().String() != r.addr {
		t.Errorf("Addr = %s", second.Addr())
	}
	k.await(LinkConnected)
	for deadline := time.Now().Add(waitLong); ; time.Sleep(5 * time.Millisecond) {
		if _, err := second.Call(ctxFor(t), k.mach.ID, Call{Tool: "echo", Arguments: json.RawMessage(`1`)}); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("the second holder never got the child: %v", err)
		}
	}
	k.stop()
	_ = second.Close()
	_ = second.Close() // twice is once

	// A port that something else has is an ordinary error.
	other := must(net.Listen("tcp", "127.0.0.1:0"))(t)
	defer other.Close()
	cfg.Addr = other.Addr().String()
	if _, err := StartManager(cfg); err == nil || errors.As(err, &held) {
		t.Errorf("a manager on a port in use = %v", err)
	}
	// And the fleet is free again after that failure.
	cfg.Addr = "127.0.0.1:0"
	third, err := StartManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = third.Close()
	if (&HeldError{}).Error() != "fleet: another process holds the fleet" {
		t.Error("the error for a holder that has not said who it is")
	}
	if got := (&HeldError{Holder: Holder{PID: 7, Address: "a:1"}}).Error(); !strings.HasPrefix(got, "fleet: process 7 holds the fleet, on a:1 since ") {
		t.Errorf("error = %q", got)
	}
}

// What a child cannot be started without.
func TestNewClientRefusals(t *testing.T) {
	r := newRig(t)
	k := r.kid(t, "box", StateConfirmed)
	cfg := ClientConfig{Dir: k.dir, Tools: k.tools}
	if _, err := NewClient(ClientConfig{Dir: k.dir}); err == nil {
		t.Error("a client without tools")
	}
	if _, err := NewClient(ClientConfig{Dir: t.TempDir(), Tools: k.tools}); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a client without an identity = %v", err)
	}
	writeRaw(t, ChildPath(k.dir), []byte(strings.Replace(string(must(os.ReadFile(ChildPath(k.dir)))(t)), r.id.Fingerprint, "", 1)))
	if _, err := NewClient(cfg); err == nil {
		t.Error("a client with an empty master fingerprint")
	}
	if err := os.Remove(ChildPath(k.dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(cfg); err == nil {
		t.Error("a client without child.json")
	}
	if _, err := newManager(ManagerConfig{Store: r.store}); err == nil {
		t.Error("a manager without an identity")
	}
	if _, err := ReadChildStatus(t.TempDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("status of a child that never ran = %v", err)
	}
	writeRaw(t, ChildStatusPath(k.dir), []byte("{"))
	if _, err := ReadChildStatus(k.dir); err == nil {
		t.Error("a broken status file was read")
	}
	writeRaw(t, ChildStatusPath(k.dir), []byte(strings.Repeat(" ", maxChildBytes+1)))
	if _, err := ReadChildStatus(k.dir); err == nil {
		t.Error("an oversized status file was read")
	}
	if err := (&Client{}).Ping(context.Background()); !errors.Is(err, ErrOffline) {
		t.Errorf("ping without a link = %v", err)
	}
}

// A child whose child.json goes bad, or whose master is nowhere, keeps
// trying and says why.
func TestClientKeepsTrying(t *testing.T) {
	r := newRig(t)
	k := r.kid(t, "box", StateConfirmed)
	dead := must(net.Listen("tcp", "127.0.0.1:0"))(t)
	deadAddr := dead.Addr().String()
	_ = dead.Close()
	// A listener that is not the master: the pin refuses it.
	other := must(tls.Listen("tcp", "127.0.0.1:0", must(MasterTLS(newIdentity(t), Open(t.TempDir())))(t)))(t)
	defer other.Close()
	go func() {
		for {
			c, err := other.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); _ = c.Close() }()
		}
	}()
	if err := UpdateChild(ChildPath(k.dir), func(c *Child) error {
		c.MasterAddresses = []string{deadAddr, other.Addr().String(), deadAddr}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	k.start()
	k.await(LinkOffline)
	for start := k.dials.Load(); k.dials.Load() < start+4; time.Sleep(5 * time.Millisecond) {
	}
	st := k.c.Status()
	if !strings.Contains(st.Detail, deadAddr) || !strings.Contains(st.Detail, "this is not the master") || len(strings.Split(st.Detail, "; ")) != 2 {
		t.Errorf("status = %+v", st)
	}
	// child.json is repaired under it: no restart is needed.
	writeRaw(t, ChildPath(k.dir), []byte("{"))
	for deadline := time.Now().Add(waitLong); !strings.Contains(k.c.Status().Detail, "child.json"); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("status with a broken child.json = %+v", k.c.Status())
		}
	}
	if err := SaveChild(ChildPath(k.dir), Child{ID: k.mach.ID, Name: k.mach.Name, MasterAddresses: []string{r.addr},
		MasterFingerprint: r.id.Fingerprint, Sandbox: "host", Ceiling: CeilingFull}); err != nil {
		t.Fatal(err)
	}
	k.await(LinkConnected)
	r.awaitOnline(t, k.mach.ID, true)
	r.echo(t, k.mach.ID)
}

// The same machine connecting again takes the place of its old link.
func TestLinkReplacedByANewerConnection(t *testing.T) {
	r := newRig(t)
	k := r.up(t, "box")
	id := k.mach.ID
	done := make(chan error, 1)
	go func() {
		_, err := r.call(t, id, "hold", `{}`)
		done <- err
	}()
	k.started()
	first := r.link(t, id)

	twin := r.kidIn(t, k.dir, k.mach).start() // a second process with the same identity
	twin.await(LinkConnected)
	for deadline := time.Now().Add(waitLong); r.link(t, id) == first; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the newer connection did not replace the old link")
		}
	}
	if err := <-done; !errors.Is(err, ErrOutcomeUnknown) || !strings.Contains(err.Error(), "replaced") {
		t.Errorf("a call on the replaced link = %v", err)
	}
	twin.stop()
	k.stop()
}

// The shapes that can never change: the hello, ping and fleet/update, as
// bytes on the wire. A child of any age has to understand them to be told
// who it is talking to and to be updated. If this test fails, the change
// that broke it is the mistake.
func TestWireShapesThatNeverChange(t *testing.T) {
	r := newRig(t)
	k := r.kid(t, "box", StateConfirmed)
	child := &childLink{c: k.c}
	ctx := context.Background()

	// What the master sends.
	if LinkVersion != 1 {
		t.Error("LinkVersion changed: decide what the new version means for speaks, and extend this test; do not edit the lines below")
	}
	for _, tc := range []struct{ method, name, params, got string }{
		{methodHello, "fleet/hello", `{"link":1}`, string(must(encode(helloParams{Link: 1}))(t))},
		{methodPing, "ping", ``, ``},
		{methodUpdate, "fleet/update",
			`{"version":"1.5.0","sha256":"` + fuzzPrint + `","size":5,"offset":0,"data":"aGVsbG8="}`,
			string(must(encode(Update{Version: "1.5.0", SHA256: fuzzPrint, Size: 5, Data: []byte("hello")}))(t))},
	} {
		if tc.method != tc.name || tc.got != tc.params {
			t.Errorf("%s: method %q with params %s", tc.name, tc.method, tc.got)
		}
	}
	// What the child answers to exactly those bytes, and to the same with a
	// field it has never heard of.
	hello := `{"link":1,"os":"` + runtime.GOOS + `","user":"root","version":"1.2.3","sandbox":"host","ceiling":"full"}`
	for _, tc := range []struct{ method, params, want string }{
		{"fleet/hello", `{"link":1}`, hello},
		{"fleet/hello", `{"link":1,"later":{"x":[1]}}`, hello},
		{"ping", ``, `{}`},
		{"ping", `{"later":1}`, `{}`},
		{"fleet/update", `{"version":"1.5.0","sha256":"` + fuzzPrint + `","size":5,"offset":0,"data":"aGVsbG8="}`, `{"received":5,"applying":true}`},
		{"fleet/update", `{"version":"1.5.0","sha256":"` + fuzzPrint + `","size":5,"offset":0,"data":"aGVsbG8=","later":true}`, `{"received":5,"applying":true}`},
		{"fleet/update", `{"version":"1.5.0","sha256":"` + fuzzPrint + `","size":5,"offset":0,"data":""}`, `{"received":0,"applying":false}`},
	} {
		got, err := child.HandleRequest(ctx, tc.method, json.RawMessage(tc.params))
		if err != nil || string(got) != tc.want {
			t.Errorf("%s %s -> %s, %v; want %s", tc.method, tc.params, got, err, tc.want)
		}
	}
	// What the master reads of the answers, with fields from the future.
	var h helloResult
	if err := json.Unmarshal([]byte(`{"link":4,"os":"linux","ceiling":"full","later":1}`), &h); err != nil || h.Link != 4 {
		t.Errorf("hello result: %+v, %v", h, err)
	}
	var ack UpdateAck
	if err := json.Unmarshal([]byte(`{"received":5,"applying":true,"later":1}`), &ack); err != nil || ack != (UpdateAck{5, true}) {
		t.Errorf("update ack: %+v, %v", ack, err)
	}
	// The master's own ping answer.
	if got, err := (&link{}).HandleRequest(ctx, "ping", nil); err != nil || string(got) != `{}` {
		t.Errorf("the master's ping answer = %s, %v", got, err)
	}

	// And the frames themselves, as the session writes them.
	rec := &recorder{frames: make(chan string, 8), block: make(chan struct{})}
	sess := mcp.NewSession(rec, nil)
	sess.Start(ctx)
	defer func() { close(rec.block); _ = sess.Close() }()
	l := &link{sess: sess}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_ = l.request(short, 0, methodHello, helloParams{Link: 1}, nil)
	_ = l.request(short, 0, methodPing, nil, nil)
	for _, want := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"fleet/hello","params":{"link":1}}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	} {
		if got := <-rec.frames; got != want {
			t.Errorf("frame %s, want %s", got, want)
		}
	}
}

// recorder is a transport that keeps what is sent and receives nothing.
type recorder struct {
	frames chan string
	block  chan struct{}
}

func (r *recorder) Send(_ context.Context, frame []byte) error {
	if strings.Contains(string(frame), `"notifications/cancelled"`) {
		return nil
	}
	r.frames <- string(frame)
	return nil
}

func (r *recorder) Recv(ctx context.Context) ([]byte, error) {
	select {
	case <-r.block:
	case <-ctx.Done():
	}
	return nil, transport.ErrClosed
}

func (r *recorder) Close() error { return nil }
