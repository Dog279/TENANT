package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for a long-lived server: with
// TENANT_SANDBOX_HELPER set it runs that helper instead of the tests.
func TestMain(m *testing.M) {
	if os.Getenv(fakeDockerEnv) == "1" {
		os.Exit(fakeDockerMain()) // the fake docker CLI (container_fake_test.go)
	}
	switch os.Getenv("TENANT_SANDBOX_HELPER") {
	case "echo": // a line server: echo each line until stdin closes
		fmt.Fprintln(os.Stderr, "helper ready")
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			fmt.Println("echo:" + sc.Text())
		}
		os.Exit(0)
	case "stuck": // ignores stdin closing, as a hung server would
		time.Sleep(time.Hour)
		os.Exit(0)
	case "env":
		fmt.Println(os.Getenv("FOO_318") + "|" + os.Getenv("SECRET_318"))
		os.Exit(0)
	case "alloc": // grab and touch 1 GiB
		b := make([]byte, 1<<30)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		fmt.Println("allocated")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// helperPolicy runs the test binary as helper mode with a scoped environment:
// the variables a process needs to start, and nothing else of the parent's.
func helperPolicy(mode string) Policy {
	return Policy{Env: EnvPolicy{
		Isolate: true,
		Pass:    []string{"SYSTEMROOT", "SystemRoot", "PATH", "TEMP", "TMP", "HOME"},
		Set:     map[string]string{"TENANT_SANDBOX_HELPER": mode},
	}}
}

func helperArgv() []string { return []string{os.Args[0], "-test.run=^$"} }

// readers keeps one buffered reader per process: a fresh reader per call
// would lose what an earlier one buffered.
var readers sync.Map // *Process -> *bufio.Reader

func readLine(t *testing.T, p *Process) string {
	t.Helper()
	r, _ := readers.LoadOrStore(p, bufio.NewReader(p.Stdout()))
	lines := make(chan string, 1)
	go func() {
		line, _ := r.(*bufio.Reader).ReadString('\n')
		lines <- strings.TrimSpace(line)
	}()
	select {
	case l := <-lines:
		return l
	case <-time.After(20 * time.Second):
		select {
		case <-p.Done():
			t.Fatalf("the process exited (%v) without a line; stderr: %s", p.Wait(), p.Stderr())
		default:
		}
		t.Fatalf("no line from the process; stderr so far: %s", p.Stderr())
		return ""
	}
}

// A started process serves over its pipes and ends when its stdin closes.
func TestStartHostServesAndCloses(t *testing.T) {
	p, err := Start(context.Background(), Host(), helperArgv(), helperPolicy("echo"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(p.Stdin(), "hello"); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, p); got != "echo:hello" {
		t.Errorf("got %q", got)
	}
	if err := p.Close(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Errorf("a server that exits on EOF: %v", err)
	}
	if !strings.Contains(p.Stderr(), "helper ready") {
		t.Errorf("stderr = %q", p.Stderr())
	}
}

// Policy.Stderr gets the process's stderr as it is written, beside the tail;
// EnvPolicy.Add joins the environment; a process that exits on its own was
// not killed.
func TestStartCopiesStderrAndAddsEnv(t *testing.T) {
	var stderr syncBuffer
	pol := helperPolicy("echo")
	pol.Stderr = &stderr
	p, err := Start(context.Background(), Host(), helperArgv(), pol)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	_ = p.Wait()
	if !strings.Contains(stderr.String(), "helper ready") || !strings.Contains(p.Stderr(), "helper ready") {
		t.Errorf("stderr copy = %q, tail = %q", stderr.String(), p.Stderr())
	}
	if p.Killed() {
		t.Error("a process that exited on EOF reports a kill")
	}

	pol = helperPolicy("env")
	pol.Env.Add = []string{"FOO_318=added"}
	p, err = Start(context.Background(), Host(), helperArgv(), pol)
	if err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, p); got != "added|" {
		t.Errorf("child env = %q", got)
	}
	_ = p.Close(5 * time.Second)
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The environment is the policy's allowlist: a parent secret does not pass.
func TestStartScopesTheEnvironment(t *testing.T) {
	t.Setenv("SECRET_318", "hunter2")
	pol := helperPolicy("env")
	pol.Env.Set["FOO_318"] = "bar"
	p, err := Start(context.Background(), Host(), helperArgv(), pol)
	if err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, p); got != "bar|" {
		t.Errorf("child env = %q; the parent's secret must not pass", got)
	}
	_ = p.Close(5 * time.Second)
}

// Close kills a server that does not exit when its stdin closes.
func TestCloseKillsAStuckProcess(t *testing.T) {
	p, err := Start(context.Background(), Host(), helperArgv(), helperPolicy("stuck"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := p.Close(300 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("Close returned with the process still running")
	}
	if !p.Killed() {
		t.Error("a process killed after its grace does not say so")
	}
	if p.Wait() == nil {
		t.Error("a killed process reported a clean exit")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Close took %v", d)
	}
}

// The context bounds the process's life.
func TestStartEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, err := Start(ctx, Host(), helperArgv(), helperPolicy("stuck"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the process outlived its context")
	}
}

// runOnly is a Backend with no Start.
type runOnly struct{}

func (runOnly) Name() string                                         { return "runonly" }
func (runOnly) Isolation() Isolation                                 { return IsolationNone }
func (runOnly) Probe(context.Context) Capabilities                   { return Capabilities{} }
func (runOnly) Run(context.Context, Command, Policy) (Result, error) { return Result{}, nil }

// A backend that cannot start a long-lived process, and an empty command, are
// refused.
func TestStartRefuses(t *testing.T) {
	if _, err := Start(context.Background(), runOnly{}, helperArgv(), Policy{}); err == nil || !strings.Contains(err.Error(), "runonly") {
		t.Errorf("a Run-only backend: %v", err)
	}
	if _, err := Start(context.Background(), Host(), nil, Policy{}); err == nil {
		t.Error("an empty command started")
	}
	if _, err := Start(context.Background(), Host(), []string{"no-such-program-318"}, Policy{}); err == nil {
		t.Error("a missing program started")
	}
}

func TestPosixLimitArgv(t *testing.T) {
	argv := []string{"server", "--x"}
	if got := posixLimitArgv(argv, Limits{}); !reflect.DeepEqual(got, argv) {
		t.Errorf("no caps: %q", got)
	}
	// MaxPIDs is ignored: outside a user namespace ulimit counts the whole user.
	got := posixLimitArgv(argv, Limits{MemoryBytes: 256 << 20, CPU: 1500 * time.Millisecond, MaxPIDs: 8})
	want := []string{"/bin/sh", "-c", `ulimit -v 262144 || exit 125; ulimit -t 2 || exit 125; exec "$@"`, "sh", "server", "--x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("caps:\n got %q\nwant %q", got, want)
	}
	if got := posixLimitArgv(argv, Limits{MaxPIDs: 8}); !reflect.DeepEqual(got, argv) {
		t.Errorf("a process cap alone must leave argv unchanged: %q", got)
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	tb := &tailBuffer{max: 8}
	fmt.Fprint(tb, "0123456789")
	fmt.Fprint(tb, "ab")
	if got := tb.String(); got != "456789ab" {
		t.Errorf("tail = %q", got)
	}
}

func TestStartLimitsNamesWhatIsCapped(t *testing.T) {
	if got := StartLimits(Host()); got != nil {
		t.Errorf("host caps %v", got)
	}
	if runtime.GOOS == "windows" {
		jb, err := For("jobobject")
		if err != nil {
			t.Fatal(err)
		}
		if got := StartLimits(jb); !reflect.DeepEqual(got, []string{"memory", "cpu", "processes"}) {
			t.Errorf("jobobject caps %v", got)
		}
	} else if _, err := For("jobobject"); err == nil {
		t.Error("jobobject resolved off Windows")
	}
	// A restricted backend by name, whether or not this host has one.
	want := []string{"memory", "cpu"}
	if got := StartLimits(&named{name: "restricted"}); !reflect.DeepEqual(got, want) {
		t.Errorf("restricted caps %v, want %v", got, want)
	}
}

// named is the fake backend under another name, for StartLimits.
type named struct {
	fakeBackend
	name string
}

func (n *named) Name() string { return n.name }

// A command that exits before anyone reads its stdout still delivers its
// output: Wait must not close the pipe under a reader that is late to it
// (os_exec "printf ok" came back as "(no output)" on a loaded CI runner).
func TestStartKeepsTheOutputOfAFastExit(t *testing.T) {
	pol := helperPolicy("env")
	pol.Env.Set["FOO_318"] = "fast"
	p, err := Start(context.Background(), Host(), helperArgv(), pol)
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done() // it has exited; only now does the reader turn up
	time.Sleep(100 * time.Millisecond)
	out, err := io.ReadAll(p.Stdout())
	if err != nil || strings.TrimSpace(string(out)) != "fast|" {
		t.Fatalf("stdout after a fast exit = %q, %v", out, err)
	}
	if err := p.Wait(); err != nil {
		t.Errorf("exit: %v", err)
	}
}
