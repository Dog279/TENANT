package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/control"
	croneng "tenant/internal/cron"
	"tenant/internal/fleet"
	"tenant/internal/logs"
	"tenant/internal/memory/soul"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
	"tenant/internal/runtrace"
)

// Where the three halves of the fleet meet (TEN-531): the master's hub
// (fleethub.go), the fleet_* tools (fleettools.go) and the child's run loop
// (childctl.go). Each test here runs all three in this process: the `tenant
// fleet` commands and the hub of fleetLab, the tools wired to that hub as
// serve wires them, and a machine that runs childRun, the loop an installed
// child runs. fleet_e2e_test.go does the same with every piece a process of
// its own.

// No process of the test binary, a helper process included, binds the fleet
// port on anything but a loopback address: the default is every interface
// (defaultFleetListen), and the first listener there raises a firewall
// prompt on the screen of whoever is using this computer.
func init() { fleetListenGuard = loopbackOnly }

func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("a test binds the fleet port on a loopback address only, not %q: set fleet.listen first (tenant fleet init --listen 127.0.0.1:0)", addr)
	}
	return nil
}

func TestNoTestBindsTheFleetPortBeyondLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:0": true, "127.0.0.1:9777": true, "[::1]:0": true,
		defaultFleetListen: false, ":9777": false, "[::]:0": false, "192.168.1.5:0": false, "localhost:0": false, "": false,
	} {
		if err := loopbackOnly(addr); (err == nil) != ok {
			t.Errorf("loopbackOnly(%q) = %v, want allowed=%v", addr, err, ok)
		}
	}
	// The mistake the guard is for: a fleet made with no --listen, so the hub
	// would bind the default. In a test process it does not listen at all.
	l := newFleetLab(t)
	l.must("init")
	l.broker = newApprovalBroker(slog.Default())
	l.stop = startFleetHub(context.Background(), l.c, "serve", l.broker, l.tell, slog.Default())
	if mgr := fleetManager(); mgr != nil {
		t.Fatalf("a test process listens on %s", mgr.Addr())
	}
	if told := strings.Join(l.told(), "\n"); !strings.Contains(told, "listener not started") || !strings.Contains(told, "loopback address only") {
		t.Fatalf("the hub did not say why it does not listen: %q", told)
	}
}

// wiredLab is a fleetLab that is a master, with the fleet tools wired to its
// hub the way serve and the terminal app wire them.
func wiredLab(t *testing.T) *fleetLab {
	t.Helper()
	l := newFleetLab(t)
	l.master()
	old := fleetDeps
	wireFleetTools()
	t.Cleanup(func() { fleetDeps = old })
	return l
}

// machine enrolls and confirms a machine with the lab's command line and, if
// run is set, starts the loop an installed child runs on it (childRun) and
// waits for its link.
func (l *fleetLab) machine(name string, run bool) labChild {
	l.t.Helper()
	ch := l.enroll(name, fleet.Reported{OS: runtime.GOOS, User: "svc", Version: "test", Sandbox: "host", Ceiling: "full"})
	l.confirm(name, ch)
	l.sync() // the settings watcher's tick: the scheme the command line saved
	if run {
		// The lab points this account's home at a folder nobody made, and a
		// command with no workdir of its own runs in the home folder.
		if home, err := os.UserHomeDir(); err != nil {
			l.t.Fatal(err)
		} else if err := os.MkdirAll(home, 0o700); err != nil {
			l.t.Fatal(err)
		}
		startChild(l.t, ch.dir)
		eventually(l.t, name+" links to the master", func() bool { return online(fleetManager(), ch.id) })
	}
	return ch
}

// cards answers every approval card the lab's operator is shown with d, and
// returns the cards shown so far.
func (l *fleetLab) cards(d control.ApprovalDecision) func() []control.ApprovalRequest {
	var mu sync.Mutex
	var shown []control.ApprovalRequest
	l.broker.SetAsk(func(_ context.Context, req control.ApprovalRequest) control.ApprovalDecision {
		mu.Lock()
		defer mu.Unlock()
		shown = append(shown, req)
		return d
	})
	return func() []control.ApprovalRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]control.ApprovalRequest{}, shown...)
	}
}

// fleetTool makes one fleet tool call as a turn of the lab's operator would.
func fleetTool(t *testing.T, ctx context.Context, tool string, args map[string]any) (string, bool) {
	t.Helper()
	out, isErr, err := (&fleetToolPlugin{}).Dispatch(ctx, fleetCall("c1", tool, args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return out, isErr
}

// memoryLogs sends the process's log records to a hub in memory until the
// test ends.
func memoryLogs(t *testing.T) *logs.Hub {
	t.Helper()
	hub, prev := logs.NewMemory(0), logs.Default()
	logs.Install(hub)
	t.Cleanup(func() { logs.Install(prev) })
	return hub
}

// The fleet tools reach the fleet this process holds, once serve or the
// terminal app has wired them, and not before.
func TestTheFleetToolsWorkOnThisProcessesFleet(t *testing.T) {
	l := newFleetLab(t)
	l.master()
	old := fleetDeps
	fleetDeps = nil
	t.Cleanup(func() { fleetDeps = old })
	l.machine("web1", true)
	shown := l.cards(control.ApproveOnce)

	local := newToolMux()
	sync := fleetToolSync(local, nil)
	if sync(); local.HasPlugin(fleetLabel) || fleetRoster() != "" {
		t.Fatal("a process that has not wired its fleet has fleet tools or a roster")
	}
	wireFleetTools()
	if sync(); !local.HasPlugin(fleetLabel) {
		t.Fatal("wired, and with a confirmed machine, the fleet tools are not registered")
	}
	if want, got := "- web1 ("+fleetOS(runtime.GOOS)+"): online\n", fleetRoster(); !strings.Contains(got, want) {
		t.Fatalf("the roster is %q, want a line %q", got, want)
	}
	out, isErr := fleetTool(t, fleetTurnCtx(), "fleet_exec", map[string]any{"host": "web1", "command": say("wired-through")})
	if isErr || !strings.Contains(out, "wired-through") {
		t.Fatalf("fleet_exec through the hub: %q (error %v)", out, isErr)
	}
	if cards := shown(); len(cards) != 1 || cards[0].HostName != "web1" || !strings.Contains(cards[0].Detail, "wired-through") {
		t.Fatalf("the command's card: %+v", cards)
	}
}

// serve and the terminal app wire the tools where they build the main
// agent's tool set: before any turn can run.
func TestServeAndTheTerminalAppWireTheFleetTools(t *testing.T) {
	for file, fn := range map[string]string{"serve.go": "cmdServe", "commands.go": "cmdTUI"} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		var wired, synced token.Pos
		for _, decl := range f.Decls {
			d, ok := decl.(*ast.FuncDecl)
			if !ok || d.Name.Name != fn || d.Body == nil {
				continue
			}
			ast.Inspect(d.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					switch id, _ := call.Fun.(*ast.Ident); {
					case id == nil:
					case id.Name == "wireFleetTools" && wired == 0:
						wired = call.Pos()
					case id.Name == "fleetToolSync" && synced == 0:
						synced = call.Pos()
					}
				}
				return true
			})
		}
		if wired == 0 || synced == 0 || wired > synced {
			t.Errorf("%s: %s must call wireFleetTools before it builds the main agent's tools (fleetToolSync): wired at %v, tools at %v",
				file, fn, fset.Position(wired), fset.Position(synced))
		}
	}
}

// When another process holds the fleet, a fleet tool says which, where the
// operator's /fleet does.
func TestAFleetToolNamesTheProcessThatHoldsTheFleet(t *testing.T) {
	l := wiredLab(t)
	l.machine("web1", false)
	l.stop() // this process lets go of the fleet ...
	id, err := fleetIdentity(l.cfg)
	if err != nil {
		t.Fatal(err)
	}
	// ... and "another" takes it: a manager of its own on the same files.
	other, err := fleet.StartManager(fleet.ManagerConfig{Store: fleet.Open(l.cfg), Identity: id, Addr: "127.0.0.1:0", Role: "serve"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	l.stop = startFleetHub(context.Background(), l.c, "tui", l.broker, l.tell, slog.Default())
	if fleetManager() != nil {
		t.Fatal("two holders of one fleet")
	}
	out, isErr := fleetTool(t, fleetTurnCtx(), "fleet_read", map[string]any{"host": "web1", "path": "/etc/hostname"})
	want := fmt.Sprintf("tenant serve (process %d) holds the fleet, on %s", os.Getpid(), other.Addr())
	if !isErr || !strings.Contains(out, want) || !strings.Contains(out, "No call was made") {
		t.Fatalf("a fleet call from a process that does not hold the fleet said %q, want it to name the holder: %q", out, want)
	}
}

// The master keeps the hash of the child's audit record with its own record
// of the call, and that record, the run trace and the activity rail all name
// the machine.
func TestTheMastersRecordOfACallKeepsTheChildsAuditHash(t *testing.T) {
	l := wiredLab(t)
	ch := l.machine("web1", true)
	l.cards(control.ApproveOnce)
	hub := memoryLogs(t)

	local := newToolMux()
	tools := composite{local: local, refresh: fleetToolSync(local, nil)}
	s := newScriptedFleetAgent(t, tools, tools, fleetRoster)
	res := s.turn(t, fleetCall("c1", "fleet_exec", map[string]any{"host": "web1", "command": say("audited")}))

	// The run trace and the rail are filled from the call's arguments and its
	// result (agent.go: span.AddTool, EventToolResult): the machine is in both.
	if len(res.ToolTrace) != 1 {
		t.Fatalf("the turn made %d calls", len(res.ToolTrace))
	}
	call := res.ToolTrace[0]
	if !strings.Contains(string(call.Call.Arguments), `"host":"web1"`) || !strings.HasPrefix(call.Result, "[fleet web1 ("+ch.id+", ") || !strings.Contains(call.Result, "audited") {
		t.Fatalf("the call as the run trace and the rail keep it: %s -> %q", call.Call.Arguments, call.Result)
	}

	sum, err := fleet.VerifyAudit(ch.dir)
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(sum.LastHash) {
		t.Fatalf("the child's audit log: %+v %v", sum, err)
	}
	var found bool
	entries, _ := hub.Snapshot()
	for _, e := range entries {
		if e.Source != "fleet" || e.Field("event") != "fleet/1201" {
			continue
		}
		found = true
		if e.Field("host") != ch.id || e.Field("name") != "web1" || e.Field("tool") != "os_exec" || e.Field("trace") != res.TraceID {
			t.Errorf("the master's record of the call does not name its machine and its turn: %s", e.Line())
		}
		if e.Field("audit") != sum.LastHash {
			t.Errorf("the master's record carries audit %q; the child's audit log ends with %s", e.Field("audit"), sum.LastHash)
		}
	}
	if !found {
		t.Fatal("the master wrote no record of the call")
	}
}

// What a machine refuses by its own ceiling reaches the model in the
// machine's own words, inside the frame, and the operator is not asked: the
// "no" is not theirs, and nothing on the master says it was.
func TestACeilingRefusalReachesTheModelAsTheChildWroteIt(t *testing.T) {
	l := wiredLab(t)
	ch := l.machine("web1", true)
	shown := l.cards(control.ApproveOnce)
	hub := memoryLogs(t)
	if err := fleet.UpdateChild(fleet.ChildPath(ch.dir), func(c *fleet.Child) error { c.Ceiling = fleet.CeilingReadOnly; return nil }); err != nil {
		t.Fatal(err)
	}

	out, isErr := fleetTool(t, fleetTurnCtx(), "fleet_exec", map[string]any{"host": "web1", "command": say("under-a-ceiling")})
	lines := strings.Split(out, "\n")
	const theirs = "[fleet: this machine refused, without asking its master: os_exec (ceiling read-only)]"
	if !isErr || len(lines) < 3 || !strings.HasPrefix(lines[0], "[fleet web1 ("+ch.id+", ") || !strings.Contains(lines[0], "the tool reported a failure") ||
		lines[len(lines)-2] != theirs || !regexp.MustCompile(`^\[/fleet [0-9a-f]{12}\]$`).MatchString(lines[len(lines)-1]) {
		t.Fatalf("the refusal as the model is shown it:\n%s", out)
	}
	if cards := shown(); len(cards) != 0 {
		t.Fatalf("the operator was asked about a call the machine refused by itself: %+v", cards)
	}
	entries, _ := hub.Snapshot()
	for _, e := range entries {
		switch {
		case e.Source == "fleet" && e.Field("event") == "fleet/1201":
			if e.Field("outcome") != "the tool failed" {
				t.Errorf("the master's record of the call: %s", e.Line())
			}
		case e.Field("host") == ch.id:
			t.Errorf("the master recorded a decision of its own about a call it was never asked about: %s", e.Line())
		}
	}
}

// promptOf is the system text and the last user message of the last request
// the planner was sent.
func promptOf(t *testing.T, planner *testllm.Fake) (system, user string) {
	t.Helper()
	reqs := append(append([]model.GenerateRequest{}, planner.Generated...), planner.Streamed...)
	if len(reqs) == 0 {
		t.Fatal("the model was never asked")
	}
	for _, m := range reqs[len(reqs)-1].Messages {
		switch m.Role {
		case "system":
			system += m.Content + "\n"
		case "user":
			user = m.Content
		}
	}
	return system, user
}

// A relay's agent is told which machines exist once the operator has opened
// that relay for the fleet, and not before: a relay that may make no fleet
// call learns nothing about the fleet.
func TestARelaysAgentIsToldTheFleetOnceTheRelayIsOpened(t *testing.T) {
	l := wiredLab(t)
	l.machine("web1", false)
	live := &soul.Live{}
	live.Store(soul.NewDefault("main"))
	mux := newToolMux()

	for _, relay := range []string{surfaceDiscord, surfaceIMessage} {
		planner := testllm.New()
		router := fleetTestRouter(t, planner)
		var ag interface {
			Turn(context.Context, agent.TurnRequest) (*agent.TurnResult, error)
		}
		var err error
		if relay == surfaceDiscord {
			ag, _, _, _, err = buildDiscordAgent("not-a-token", discordAgentDeps{router: router, soulLive: live, fullTools: mux, fullDisp: mux, log: quietLog})
		} else {
			ag, err = buildIMessageAgent(imessageAgentDeps{router: router, soulLive: live, fullTools: mux, fullDisp: mux, log: quietLog})
		}
		if err != nil {
			t.Fatal(err)
		}
		told := func() string {
			t.Helper()
			if _, err := ag.Turn(context.Background(), agent.TurnRequest{UserQuery: "hi"}); err != nil {
				t.Fatal(err)
			}
			system, _ := promptOf(t, planner)
			return system
		}
		if system := told(); strings.Contains(system, "web1") || strings.Contains(system, "Fleet:") {
			t.Fatalf("%s is not opened for the fleet, and its agent is told:\n%s", relay, system)
		}
		l.must("relay", relay, "on")
		eventually(t, "the hub sees "+relay+" opened", func() bool { return fleetRelayRoster(relay) != "" })
		if system := told(); !strings.Contains(system, "- web1 ("+fleetOS(runtime.GOOS)+"): offline\n") {
			t.Fatalf("%s is opened for the fleet, and its agent is not told the machines:\n%s", relay, system)
		}
		l.must("relay", relay, "off")
		eventually(t, "the hub sees "+relay+" closed", func() bool { return fleetRelayRoster(relay) == "" })
	}
}

// A cron job is told the machines on its own list, in its own turn, and a
// job with no list is told nothing.
func TestACronJobIsToldItsOwnMachines(t *testing.T) {
	l := wiredLab(t)
	web, _ := l.machine("web1", false), l.machine("db1", false)
	live := &soul.Live{}
	live.Store(soul.NewDefault("main"))
	planner := testllm.New()
	mux := newToolMux()
	runs, err := runtrace.Open(":memory:", runtrace.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer runs.Close()
	runner, err := buildCronRunner(cronAgentDeps{router: fleetTestRouter(t, planner), soulLive: live, runs: runs,
		fullReg: mux, fullDisp: mux, log: quietLog, cfgDir: l.cfg, dataDir: l.data})
	if err != nil {
		t.Fatal(err)
	}
	said := func(ids ...string) string {
		t.Helper()
		if _, _, err := runner(context.Background(), croneng.Job{JobDef: croneng.JobDef{ID: "j1", Name: "nightly", Prompt: "check the disks", Fleet: ids}}); err != nil {
			t.Fatal(err)
		}
		_, user := promptOf(t, planner)
		return user
	}
	if user := said(); strings.Contains(user, "Fleet:") || strings.Contains(user, "web1") {
		t.Fatalf("a job with no machine list is told about the fleet:\n%s", user)
	}
	user := said(web.id)
	if !strings.Contains(user, "check the disks") || !strings.Contains(user, "- web1 ("+fleetOS(runtime.GOOS)+"): offline\n") || strings.Contains(user, "db1") {
		t.Fatalf("a job opened for web1 alone is told:\n%s", user)
	}
}
