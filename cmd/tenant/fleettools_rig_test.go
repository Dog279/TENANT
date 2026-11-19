package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/approval"
	"tenant/internal/control"
	"tenant/internal/fleet"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
	"tenant/internal/runtrace"
)

var fleetQuiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fleetToolRig is a master with a real fleet on loopback: a manager, a store
// in a temp dir, schemes of its own, and an attended origin whose prompts the
// test answers and counts. fleetDeps points at it until the test ends.
type fleetToolRig struct {
	t       *testing.T
	dir     string
	store   *fleet.Store
	mgr     *fleet.Manager
	master  fleet.Identity
	schemes *approval.Fleet
	asker   *approvalBroker
	plugin  *fleetToolPlugin
	addr    string // what children dial: the manager, or a proxy in front of it

	mu     sync.Mutex
	origin approval.Origin
	holds  bool // this process holds the fleet
	answer control.ApprovalDecision
	shown  []control.ApprovalRequest
}

func newFleetToolRig(t *testing.T) *fleetToolRig {
	t.Helper()
	r := &fleetToolRig{t: t, dir: t.TempDir(), holds: true, answer: control.DenyOnce, schemes: approval.NewFleet(categorize)}
	var err error
	if r.master, err = fleet.MintIdentity(fleet.MasterIdentityPath(r.dir)); err != nil {
		t.Fatal(err)
	}
	r.store = fleet.Open(r.dir)
	if r.mgr, err = fleet.StartManager(fleet.ManagerConfig{Store: r.store, Identity: r.master, Addr: "127.0.0.1:0", Role: "test", Log: fleetQuiet}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.mgr.Close() })
	r.dial(r.mgr.Addr().String())

	r.asker = newDiscordApprovalBroker(fleetQuiet)
	r.asker.SetRules(newToolRules())
	r.asker.SetAsk(func(_ context.Context, req control.ApprovalRequest) control.ApprovalDecision {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.shown = append(r.shown, req)
		return r.answer
	})
	r.origin = approval.Origin{Name: "local", Open: true, Asker: r.asker}
	r.plugin = &fleetToolPlugin{}

	old := fleetDeps
	fleetDeps = &fleetToolDeps{
		manager: func() *fleet.Manager {
			r.mu.Lock()
			defer r.mu.Unlock()
			if !r.holds {
				return nil
			}
			return r.mgr
		},
		store: func() *fleet.Store { return r.store },
		origin: func(context.Context) approval.Origin {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.origin
		},
		schemes: r.schemes,
	}
	t.Cleanup(func() { fleetDeps = old })
	return r
}

// dial makes addr the address children enroll at and dial.
func (r *fleetToolRig) dial(addr string) {
	r.t.Helper()
	r.addr = addr
	if err := r.store.SetAddresses([]string{addr}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *fleetToolRig) from(o approval.Origin) {
	r.mu.Lock()
	r.origin = o
	r.mu.Unlock()
}

func (r *fleetToolRig) answers(d control.ApprovalDecision) {
	r.mu.Lock()
	r.answer = d
	r.mu.Unlock()
}

// prompts is how many cards the operator has been shown, and the last one.
func (r *fleetToolRig) prompts() (int, control.ApprovalRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.shown) == 0 {
		return 0, control.ApprovalRequest{}
	}
	return len(r.shown), r.shown[len(r.shown)-1]
}

// fleetKid is one child machine of a rig.
type fleetKid struct {
	id, name, dir string
	stop          func()
}

// enroll takes a machine through enrollment as far as pending.
func (r *fleetToolRig) enroll(name string) *fleetKid {
	r.t.Helper()
	k := &fleetKid{name: name, dir: r.t.TempDir(), stop: func() {}}
	id, err := fleet.MintIdentity(fleet.ChildIdentityPath(k.dir))
	if err != nil {
		r.t.Fatal(err)
	}
	code, err := r.store.CreateEnrollment(r.master.Fingerprint, name, time.Minute, "")
	if err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	child, _, err := fleet.Enroll(ctx, code, id, fleet.Reported{OS: "linux", User: "svc", Sandbox: "host", Ceiling: "full"})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := fleet.SaveChild(fleet.ChildPath(k.dir), child); err != nil {
		r.t.Fatal(err)
	}
	k.id = child.ID
	r.t.Cleanup(func() { k.stop() })
	return k
}

// confirm confirms an enrolled machine and gives it a scheme that asks for
// everything.
func (r *fleetToolRig) confirm(k *fleetKid) {
	r.t.Helper()
	m, err := r.store.Machine(k.id)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.store.Confirm(r.master.Fingerprint, k.id, fleet.ConfirmCode(r.master.Fingerprint, m.Fingerprint)); err != nil {
		r.t.Fatal(err)
	}
	if err := r.schemes.Put(k.id, approval.Scheme{}); err != nil {
		r.t.Fatal(err)
	}
}

// run starts the machine's child, serving tools, and waits for its link.
func (r *fleetToolRig) run(k *fleetKid, tools fleet.Tools) {
	r.t.Helper()
	c, err := fleet.NewClient(fleet.ClientConfig{Dir: k.dir, Tools: tools, Version: "test", User: "svc", Log: fleetQuiet})
	if err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(ctx)
	}()
	var once sync.Once
	k.stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	r.online(k, true)
}

// child is a confirmed machine with a running child that serves tools.
func (r *fleetToolRig) child(name string, tools fleet.Tools) *fleetKid {
	r.t.Helper()
	k := r.enroll(name)
	r.confirm(k)
	r.run(k, tools)
	return k
}

// online waits until the master has, or no longer has, a link to k.
func (r *fleetToolRig) online(k *fleetKid, want bool) {
	r.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		list, err := r.mgr.Machines()
		for _, m := range list {
			if m.ID == k.id && m.Online == want {
				return
			}
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("machine %s did not become online=%v (%v)", k.name, want, err)
		}
	}
}

// fleetTurnCtx is the context of one turn: it carries a span, as a turn's
// tool calls do.
func fleetTurnCtx() context.Context {
	return runtrace.WithSpan(context.Background(), runtrace.NewSpan(runtrace.NewID()))
}

// call makes one fleet tool call as the agent's dispatcher would.
func (r *fleetToolRig) call(ctx context.Context, tool string, args map[string]any) (string, bool) {
	r.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		r.t.Fatal(err)
	}
	out, isErr, err := r.plugin.Dispatch(ctx, model.ToolCall{ID: "t", Name: tool, Arguments: raw})
	if err != nil {
		r.t.Fatalf("%s: a fleet tool returns its failures as results, got the error %v", tool, err)
	}
	return out, isErr
}

// --- what a test child serves ---

// kidTools is a child's tool set under test: what it lists and what it does.
// With no handler it is an honest child (honestKid).
type kidTools struct {
	mu    sync.Mutex
	specs []model.ToolSpec
	on    func(ctx context.Context, call model.ToolCall) (string, bool, error)
	calls []model.ToolCall
	lists int
}

func (k *kidTools) All() []model.ToolSpec {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.lists++
	return k.specs
}

func (k *kidTools) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	k.mu.Lock()
	k.calls = append(k.calls, call)
	on := k.on
	k.mu.Unlock()
	if on != nil {
		return on(ctx, call)
	}
	return honestKid(ctx, call)
}

// seen is how many tool calls reached the child, and how often it was asked
// for its tool list.
func (k *kidTools) seen() (calls, lists int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.calls), k.lists
}

func (k *kidTools) last() model.ToolCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.calls) == 0 {
		return model.ToolCall{}
	}
	return k.calls[len(k.calls)-1]
}

// honestKid behaves as a child's own tools do where it matters here: a
// command and a write ask the master over the link before they act, and a
// read does not ask.
func honestKid(ctx context.Context, call model.ToolCall) (string, bool, error) {
	var a struct{ Command, Path, Action, ID, Content string }
	_ = json.Unmarshal(call.Arguments, &a)
	ask := func(action, detail string) (string, bool, error) {
		if !fleet.Confirm(ctx, action, detail) {
			return "blocked: this wasn't approved", true, nil
		}
		return call.Name + " done: " + detail, false, nil
	}
	switch call.Name {
	case "os_exec":
		return ask("os_exec", a.Command)
	case "os_write_file", "os_edit_file":
		return ask("os_write", "write "+a.Path)
	case "os_make_dir":
		return ask("os_mkdir", "create directory "+a.Path)
	case "os_process":
		if a.Action == "kill" || a.Action == "write" {
			return ask("os_exec", "os_process "+a.Action+" "+a.ID)
		}
	case "web_click":
		return ask("web_interact", "click")
	}
	return call.Name + " answered " + string(call.Arguments), false, nil
}

// --- a proxy a test can cut ---

// cutProxy stands between children and the master, so a test can drop every
// link at once the way a network does.
type cutProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []net.Conn
}

func newCutProxy(t *testing.T, target string) *cutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{ln: ln, target: target}
	t.Cleanup(func() {
		_ = ln.Close()
		p.cut()
	})
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", p.target)
			if err != nil {
				_ = in.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, in, out)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(out, in); _ = out.Close() }()
			go func() { _, _ = io.Copy(in, out); _ = in.Close() }()
		}
	}()
	return p
}

func (p *cutProxy) cut() {
	p.mu.Lock()
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// --- a real agent on a scripted model ---

// scriptedFleetAgent is a real agent whose model makes the tool calls a test
// queues, one per step, and then answers "done".
type scriptedFleetAgent struct {
	ag      *agent.Agent
	planner *testllm.Fake

	mu     sync.Mutex
	script []model.ToolCall
	egress []string // what the egress check asked about
	allow  bool     // its answer
}

// fleetTestRouter is a router whose planner is planner: an agent built on it
// asks that fake, and the fake keeps every request it was sent.
func fleetTestRouter(t *testing.T, planner *testllm.Fake) *model.Router {
	t.Helper()
	reg := model.NewEmptyRegistry()
	for _, p := range []model.Profile{
		{ID: "plan", Role: model.RolePlanner, Backend: "plan", ProviderKind: "vllm", Endpoint: "http://plan", ContextLength: 64000},
		{ID: "embed", Role: model.RoleEmbedder, Backend: "embed", ProviderKind: "vllm", Endpoint: "http://embed", ContextLength: 8000},
	} {
		if err := reg.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	router := model.NewRouter(reg, nil)
	embedder := testllm.New()
	router.RegisterBackend("plan", func(context.Context, model.Profile, *slog.Logger) (any, error) { return planner, nil })
	router.RegisterBackend("embed", func(context.Context, model.Profile, *slog.Logger) (any, error) { return embedder, nil })
	return router
}

func newScriptedFleetAgent(t *testing.T, tools agent.ToolRegistry, disp agent.ToolDispatcher, render func() string) *scriptedFleetAgent {
	t.Helper()
	s := &scriptedFleetAgent{planner: testllm.New()}
	s.planner.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.script) == 0 {
			return &model.GenerateResponse{Text: "done", FinishReason: "stop"}, nil
		}
		next := s.script[0]
		s.script = s.script[1:]
		return &model.GenerateResponse{ToolCalls: []model.ToolCall{next}}, nil
	}
	router := fleetTestRouter(t, s.planner)
	var err error
	s.ag, err = agent.New(agent.Config{
		AgentID: "main", SessionID: "fleet", Router: router, Working: working.New(),
		Tools: tools, Dispatcher: disp, SystemPrompt: "You are the test agent.", RenderAgents: render,
		ConfirmSensitiveEgress: func(_ context.Context, _, detail string) bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.egress = append(s.egress, detail)
			return s.allow
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// turn runs one turn in which the model makes calls in order.
func (s *scriptedFleetAgent) turn(t *testing.T, calls ...model.ToolCall) *agent.TurnResult {
	t.Helper()
	s.mu.Lock()
	s.script = calls
	s.mu.Unlock()
	res, err := s.ag.Turn(context.Background(), agent.TurnRequest{UserQuery: "work on the fleet", LoopCeiling: 32})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// prompt is the last request the model was sent: its system text and the
// names of the tools it was offered.
func (s *scriptedFleetAgent) prompt(t *testing.T) (system string, tools []string) {
	t.Helper()
	gen := s.planner.Generated
	if len(gen) == 0 {
		t.Fatal("the model was never asked")
	}
	req := gen[len(gen)-1]
	for _, m := range req.Messages {
		if m.Role == "system" {
			system += m.Content + "\n"
		}
	}
	for _, sp := range req.Tools {
		tools = append(tools, sp.Name)
	}
	return system, tools
}

func fleetToolNames(tools []string) []string {
	var out []string
	for _, n := range tools {
		if strings.HasPrefix(n, "fleet_") {
			out = append(out, n)
		}
	}
	return out
}

func fleetCall(id, name string, args map[string]any) model.ToolCall {
	raw, _ := json.Marshal(args)
	return model.ToolCall{ID: id, Name: name, Arguments: raw}
}

// plant writes n more confirmed machines (node00, node01, ...) straight into
// fleet.json: the enroll endpoint, rightly, takes only a few attempts a minute.
func (r *fleetToolRig) plant(n int) {
	r.t.Helper()
	path := fleet.StorePath(r.dir)
	raw, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		r.t.Fatal(err)
	}
	machines, _ := file["machines"].(map[string]any)
	if machines == nil {
		machines = map[string]any{}
		file["machines"] = machines
	}
	for i := range n {
		sum := sha256.Sum256(fmt.Append(nil, "planted ", i))
		machines[fmt.Sprintf("m%016x", 0xabc000+i)] = map[string]any{
			"name": fmt.Sprintf("node%02d", i), "fingerprint": hex.EncodeToString(sum[:]), "state": "confirmed",
			"reported": map[string]any{"os": "linux", "ceiling": "full"}, "enrolled_at": time.Now().UTC().Format(time.RFC3339),
		}
	}
	out, err := json.Marshal(file)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.store.Machines(); err != nil {
		r.t.Fatalf("the planted fleet.json does not load: %v", err)
	}
}
