package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/approval"
	"tenant/internal/fleet"
)

// The fleet is held by one process and changed by others. These tests run
// `tenant serve` and `tenant fleet …` as real processes: this test binary,
// started again as TestFleetHelperProcess with the command in its environment,
// on temp dirs and a loopback port the system picks, with no window.

const (
	fleetHelperArgs   = "TENANT_FLEETCTL_ARGS"   // the helper's command line, as a JSON array
	fleetHelperAnswer = "TENANT_FLEETCTL_ANSWER" // what the operator types when the helper asks
)

// TestFleetHelperProcess is not a test: it is `tenant <args>` as a process of
// its own, for TestFleetTwoProcesses.
func TestFleetHelperProcess(t *testing.T) {
	raw := os.Getenv(fleetHelperArgs)
	if raw == "" {
		return
	}
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil || len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "helper: bad arguments:", err)
		os.Exit(2)
	}
	if answer, ok := os.LookupEnv(fleetHelperAnswer); ok { // the helper has no terminal; this stands in for the person at one
		fleetAsk = func(string) (string, error) { return answer, nil }
	}
	fleetWatchInterval = 100 * time.Millisecond
	c := cliRegistry().Lookup(argv[0])
	if c == nil {
		fmt.Fprintln(os.Stderr, "helper: unknown command", argv[0])
		os.Exit(2)
	}
	if err := c.Run(context.Background(), argv[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// fleetProcs runs tenant commands as processes on one pair of temp dirs.
type fleetProcs struct {
	t         *testing.T
	cfg, data string
}

func (p *fleetProcs) command(answer *string, args ...string) *exec.Cmd {
	argv, _ := json.Marshal(append(args, "--config", p.cfg, "--data", p.data))
	cmd := exec.Command(os.Args[0], "-test.run=^TestFleetHelperProcess$")
	cmd.Env = append(os.Environ(), fleetHelperArgs+"="+string(argv))
	if answer != nil {
		cmd.Env = append(cmd.Env, fleetHelperAnswer+"="+*answer)
	}
	hideWindow(cmd)
	return cmd
}

// tenant runs one command to its end: its standard output, its standard
// error, and whether it exited zero.
func (p *fleetProcs) tenant(answer *string, args ...string) (stdout, stderr string, err error) {
	p.t.Helper()
	cmd := p.command(answer, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

func (p *fleetProcs) must(answer *string, args ...string) string {
	p.t.Helper()
	out, errOut, err := p.tenant(answer, args...)
	if err != nil {
		p.t.Fatalf("tenant %s: %v\n%s\n%s", strings.Join(args, " "), err, out, errOut)
	}
	return out + errOut
}

// lines collects what a long-running process prints.
type lines struct {
	mu  sync.Mutex
	all []string
}

func (l *lines) read(r *bufio.Scanner) {
	for r.Scan() {
		l.mu.Lock()
		l.all = append(l.all, r.Text())
		l.mu.Unlock()
	}
}

func (l *lines) has(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.all {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

func (l *lines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.all, "\n")
}

// A running `tenant serve` takes the fleet when `tenant fleet init` runs in
// another process, with no restart; enroll, confirm, permissions and revoke
// from the command line reach it; a second holder is told who holds the
// fleet, and takes it when that process is gone.
func TestFleetTwoProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("boots serve as a process")
	}
	root := t.TempDir()
	for _, env := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
		t.Setenv(env, filepath.Join(root, "home"))
	}
	p := &fleetProcs{t: t, cfg: filepath.Join(root, "cfg"), data: filepath.Join(root, "data")}

	// serve, with no fleet yet.
	serve := p.command(nil, "serve", "--backend", "echo", "--dashboard=false", "--self-improve=false")
	stdout, err := serve.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var serveErr bytes.Buffer
	serve.Stderr = &serveErr
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	said := &lines{}
	go said.read(bufio.NewScanner(stdout))
	stopped := false
	stopServe := func() {
		if !stopped {
			stopped = true
			_ = serve.Process.Kill()
			_ = serve.Wait()
		}
	}
	t.Cleanup(stopServe)
	wait := func(what string, limit time.Duration, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(limit); !ok(); time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("after %s: %s\nserve said:\n%s\n%s", limit, what, said, serveErr.String())
			}
		}
	}
	wait("serve is up", 120*time.Second, func() bool { return said.has("running 24/7") })
	if said.has("fleet:") {
		t.Fatalf("a serve with no fleet talks about one:\n%s", said)
	}

	// init, in another process: the running serve takes the fleet.
	out := p.must(nil, "fleet", "init", "--listen", "127.0.0.1:0", "--wait", "30s")
	held := regexp.MustCompile(`tenant serve \(process (\d+)\) holds the fleet, on (127\.0\.0\.1:\d+)`).FindStringSubmatch(out)
	if held == nil || held[1] != fmt.Sprint(serve.Process.Pid) {
		t.Fatalf("init did not see the running serve (process %d) take the fleet:\n%s", serve.Process.Pid, out)
	}
	addr := held[2]
	wait("serve says it listens", 10*time.Second, func() bool { return said.has("fleet: listening on " + addr) })
	p.must(nil, "fleet", "init", "--address", addr)

	// enroll: the code alone on standard output.
	code, _, err := p.tenant(nil, "fleet", "enroll", "web1")
	if err != nil {
		t.Fatal(err)
	}
	code = strings.TrimSpace(code)
	dir := t.TempDir()
	ident, err := fleet.MintIdentity(fleet.ChildIdentityPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	child, six, err := fleet.Enroll(context.Background(), code, ident, fleet.Reported{OS: "linux", User: "svc", Version: "9.9.9", Sandbox: "host", Ceiling: "full"})
	if err != nil {
		t.Fatalf("the new machine could not enroll with serve: %v", err)
	}
	if err := fleet.SaveChild(fleet.ChildPath(dir), child); err != nil {
		t.Fatal(err)
	}
	ch := labChild{dir: dir, id: child.ID, code: six}
	client, ran := ch.start(t, &labTools{})
	wait("the pending machine is told to wait", 20*time.Second, func() bool { return client.Status().State == fleet.LinkWaiting })
	if out := p.must(nil, "fleet", "list"); !strings.Contains(out, "web1") || !strings.Contains(out, "pending") || !strings.Contains(out, `"svc"`) {
		t.Fatalf("list:\n%s", out)
	}

	// confirm: the wrong digits fail the command and change nothing.
	wrong := "000000"
	if wrong == six {
		wrong = "111111"
	}
	if out, errOut, err := p.tenant(&wrong, "fleet", "confirm", "web1"); err == nil || !strings.Contains(errOut, "not the code") {
		t.Fatalf("confirm with the wrong digits: %v\n%s\n%s", err, out, errOut)
	}
	// With no person at a terminal, there is no confirming at all.
	if _, errOut, err := p.tenant(nil, "fleet", "confirm", "web1"); err == nil || !strings.Contains(errOut, "terminal") {
		t.Fatalf("confirm with nobody at a terminal: %v\n%s", err, errOut)
	}
	if m, _ := fleet.Open(p.cfg).Machine(ch.id); m.State != fleet.StatePending {
		t.Fatalf("the machine is %s after two refused confirmations", m.State)
	}
	if out := p.must(&six, "fleet", "confirm", "web1"); !strings.Contains(out, "Reported by the machine") || !strings.Contains(out, "confirmed web1") {
		t.Fatalf("confirm:\n%s", out)
	}
	// serve, which holds the fleet, lets the machine in, and reads the scheme
	// the command saved.
	wait("the confirmed machine links to serve", 30*time.Second, func() bool { return client.Status().State == fleet.LinkConnected })
	wait("serve applies the machine's scheme from the settings file", 20*time.Second, func() bool { return said.has("fleet scheme change") })

	// A second holder, this process, is told who holds the fleet.
	prevEvery := fleetWatchInterval
	fleetWatchInterval = 100 * time.Millisecond
	var feed lines
	stop := startFleetHub(context.Background(), &commonFlags{cfgDir: p.cfg, dataDir: p.data, agent: "main"}, "tui",
		newApprovalBroker(slog.Default()), func(s string) { feed.mu.Lock(); feed.all = append(feed.all, s); feed.mu.Unlock() }, slog.Default())
	t.Cleanup(func() { stop(); fleetWatchInterval = prevEvery })
	if fleetManager() != nil {
		t.Fatal("two processes hold one fleet")
	}
	who := fmt.Sprintf("tenant serve (process %d) holds the fleet, on %s", serve.Process.Pid, addr)
	if elsewhere := fleetHeldElsewhere(); !strings.Contains(elsewhere, who) || !feed.has(who) {
		t.Fatalf("the second holder was told %q and %q, want %q", elsewhere, feed.String(), who)
	}
	if fleetStore() == nil {
		t.Fatal("the second process cannot read the fleet it does not hold")
	}

	// permissions, from the command line, in the file serve reads.
	p.must(nil, "fleet", "permissions", "web1", "set", "exec", "allow")
	stg, err := loadSettings(p.cfg, "main")
	if err != nil {
		t.Fatal(err)
	}
	var scheme approval.Scheme
	if err := json.Unmarshal(stg.FleetSchemes[ch.id], &scheme); err != nil || scheme.Permissions["exec"] != "allow" || scheme.Permissions["write"] != "ask" {
		t.Fatalf("the machine's scheme in the settings file: %s %v", stg.FleetSchemes[ch.id], err)
	}

	// revoke, from the command line: serve closes the socket it holds.
	p.must(nil, "fleet", "revoke", "web1", "--keep-scheme")
	select {
	case err := <-ran:
		if !errors.Is(err, fleet.ErrRevoked) {
			t.Fatalf("the revoked machine stopped with %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("serve did not close the revoked machine's link\n%s", said)
	}

	// The holder goes; the other process takes the fleet, and the command
	// line says so.
	stopServe()
	wait("the second process takes the fleet", 20*time.Second, func() bool { return fleetManager() != nil })
	if out := p.must(nil, "fleet", "list"); !strings.Contains(out, fmt.Sprintf("tenant tui (process %d) holds the fleet", os.Getpid())) || !strings.Contains(out, "revoked") {
		t.Fatalf("list after the holder changed:\n%s", out)
	}
}
