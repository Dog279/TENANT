package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/fleet"
	"tenant/internal/model"
	"tenant/internal/plugins/osys"
	"tenant/internal/secrets"
)

// No test reaches this machine's own system directory or writes a line to
// its system log, which could not be taken out again: the seams of
// childctl.go point at a folder that does not exist and at a recorder for
// the whole test binary, the helper processes included.
var childOSLog = &lineRecorder{}

func init() {
	childSystemDir = func() string { return filepath.Join(os.TempDir(), "tenant-test-no-system-child") }
	openChildOSLog = func() (func(string) error, func(), error) { return childOSLog.write, func() {}, nil }
	registerChildOSLog = func() error { return nil }
	unregisterChildOSLog = func() error { return nil }
	childPoll = 20 * time.Millisecond
}

type lineRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *lineRecorder) write(line string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	return nil
}

func (r *lineRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.lines)
}

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

const childWait = 45 * time.Second // how long a test waits for what must happen

// --- a master ---

// fleetMaster is a real master on a loopback port: its fleet.json, its
// identity and the manager that holds the links. It answers every approval
// a child asks for with allow, and remembers what it was asked.
type fleetMaster struct {
	t     *testing.T
	store *fleet.Store
	id    fleet.Identity
	mgr   *fleet.Manager

	allow atomic.Bool
	mu    sync.Mutex
	asked []string // "action|detail" of every approval that reached the master
}

func newFleetMaster(t *testing.T) *fleetMaster {
	t.Helper()
	dir := t.TempDir()
	id, err := fleet.MintIdentity(fleet.MasterIdentityPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	m := &fleetMaster{t: t, store: fleet.Open(dir), id: id}
	m.allow.Store(true)
	if m.mgr, err = fleet.StartManager(fleet.ManagerConfig{Store: m.store, Identity: id, Addr: "127.0.0.1:0", Role: "test", Log: quietLog}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.mgr.Close() })
	if err := m.store.SetAddresses([]string{m.mgr.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	return m
}

// code makes a one-time enrollment code for a machine called name.
func (m *fleetMaster) code(name string) string {
	m.t.Helper()
	code, err := m.store.CreateEnrollment(m.id.Fingerprint, name, 0, "")
	if err != nil {
		m.t.Fatal(err)
	}
	return code
}

// confirm types the six digits on the master.
func (m *fleetMaster) confirm(name, digits string) fleet.Machine {
	m.t.Helper()
	mach, err := m.store.ByName(name)
	if err != nil {
		m.t.Fatalf("the master has no machine %s: %v", name, err)
	}
	if mach, err = m.store.Confirm(m.id.Fingerprint, mach.ID, digits); err != nil {
		m.t.Fatalf("confirm %s with %s: %v", name, digits, err)
	}
	return mach
}

func (m *fleetMaster) awaitOnline(id string, online bool) {
	m.t.Helper()
	for deadline := time.Now().Add(childWait); ; time.Sleep(10 * time.Millisecond) {
		list, err := m.mgr.Machines()
		for _, mach := range list {
			if mach.ID == id && mach.Online == online {
				return
			}
		}
		if time.Now().After(deadline) {
			m.t.Fatalf("machine %s did not become online=%v (%v): %+v", id, online, err, list)
		}
	}
}

// call runs a tool on a machine. args is marshalled as the tool's arguments.
func (m *fleetMaster) call(id, tool string, args map[string]any) (fleet.Result, error) {
	m.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		m.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return m.mgr.Call(ctx, id, fleet.Call{Tool: tool, Arguments: raw, Confirm: func(_ context.Context, action, detail string) bool {
		m.mu.Lock()
		m.asked = append(m.asked, action+"|"+detail)
		m.mu.Unlock()
		return m.allow.Load()
	}})
}

// ok is call for a call that must come back with a result that is no error.
func (m *fleetMaster) ok(id, tool string, args map[string]any) fleet.Result {
	m.t.Helper()
	res, err := m.call(id, tool, args)
	if err != nil || res.IsError {
		m.t.Fatalf("%s %v = %q (error %v), %v", tool, args, res.Text, res.IsError, err)
	}
	return res
}

// refused is call for a call the machine must turn down.
func (m *fleetMaster) refused(id, tool string, args map[string]any, because string) fleet.Result {
	m.t.Helper()
	res, err := m.call(id, tool, args)
	if err != nil || !res.IsError || !strings.Contains(res.Text, because) {
		m.t.Fatalf("%s %v = %q (error %v), %v; want a refusal that says %q", tool, args, res.Text, res.IsError, err, because)
	}
	return res
}

func (m *fleetMaster) askedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.asked)
}

func (m *fleetMaster) lastAsked() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.asked) == 0 {
		return ""
	}
	return m.asked[len(m.asked)-1]
}

func (m *fleetMaster) tools(id string) []string {
	m.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), childWait)
	defer cancel()
	specs, err := m.mgr.Tools(ctx, id)
	if err != nil {
		m.t.Fatalf("tools of %s: %v", id, err)
	}
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
	}
	return names
}

// --- a child ---

var confirmationCode = regexp.MustCompile(`confirmation code: (\d{6})`)

// realTemp is a temp dir by its real path: the file tools resolve every
// path, and a refusal names the resolved one.
func realTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// enrollChild runs `tenant child install` for a new machine against m, with
// the service manager recorded instead of asked. It returns the child's
// directory, the six digits it printed and what it said.
func enrollChild(t *testing.T, m *fleetMaster, name string, o childInstallOpts) (dir, digits, said string) {
	t.Helper()
	childServiceSeams(t)
	(&cmdRecorder{}).install(t)
	dir = filepath.Join(realTemp(t), "child")
	if o.ceiling == "" {
		o.ceiling = "full"
	}
	if o.sandbox == "" {
		o.sandbox = "host"
	}
	var out bytes.Buffer
	if err := childInstall(context.Background(), strings.NewReader(m.code(name)+"\n"), &out, dir, o); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	found := confirmationCode.FindStringSubmatch(out.String())
	if found == nil {
		t.Fatalf("install printed no confirmation code:\n%s", out.String())
	}
	return dir, found[1], out.String()
}

// startChild runs the child's run loop, as the installed service would,
// until the test ends or stop is called.
func startChild(t *testing.T, dir string) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- childRun(ctx, dir, quietLog, childRunOpts{}) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("the child's run loop ended with %v", err)
				}
			case <-time.After(childWait):
				t.Error("the child's run loop did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// upChild is a machine enrolled, confirmed and connected.
func upChild(t *testing.T, o childInstallOpts) (m *fleetMaster, dir string, mach fleet.Machine) {
	t.Helper()
	m = newFleetMaster(t)
	dir, digits, _ := enrollChild(t, m, "box", o)
	mach = m.confirm("box", digits)
	startChild(t, dir)
	m.awaitOnline(mach.ID, true)
	return m, dir, mach
}

// status is what `tenant child status` prints, and its error.
func status(dir string) (string, error) {
	var out bytes.Buffer
	err := childStatus(context.Background(), &out, dir)
	return out.String(), err
}

func exitCode(err error) int {
	var ex exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ex):
		return ex.code
	}
	return 1
}

// say is a command osys.ReadOnly judges read-only on this OS, and what it
// prints.
func say(word string) string {
	if runtime.GOOS == "windows" {
		return "Write-Output " + word
	}
	return "echo " + word
}

// destructive is a command the classifier flags as destructive, for the word
// in it, that would do nothing but print that word if a test's gate ever
// failed to hold it back.
func destructive() string { return say("diskpart") }

// beat is a command that adds a line to file ten times a second until it is
// killed: a command whose death a test can see.
func beat(file string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`while ($true) { Add-Content -LiteralPath '%s' -Value x; Start-Sleep -Milliseconds 100 }`, file)
	}
	return fmt.Sprintf(`while true; do echo x >> '%s'; sleep 0.1; done`, file)
}

func size(file string) int64 {
	fi, err := os.Stat(file)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// awaitBeating waits until the command writing file is running.
func awaitBeating(t *testing.T, file string) {
	t.Helper()
	for deadline := time.Now().Add(childWait); size(file) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the command never wrote %s", file)
		}
	}
}

// awaitStill waits until the command writing file has stopped: nothing is
// added for a second.
func awaitStill(t *testing.T, file string) {
	t.Helper()
	deadline := time.Now().Add(childWait)
	for last, since := size(file), time.Now(); time.Since(since) < time.Second; time.Sleep(50 * time.Millisecond) {
		if now := size(file); now != last {
			last, since = now, time.Now()
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command writing %s is still running", file)
		}
	}
}

// --- what is refused before anything is touched ---

// The one-time code never travels as an argument: it would be in the process
// list while the command runs and in the shell's history afterwards.
func TestChildInstallRefusesTheCodeAsAnArgument(t *testing.T) {
	m := newFleetMaster(t)
	code := m.code("box")
	dir := filepath.Join(t.TempDir(), "child")
	for _, args := range [][]string{
		{"install", code},
		{"install", "--dir", dir, code},
		{"install", "--dir", dir, "--ceiling", "read-only", code, "--system"},
	} {
		err := cmdChild(context.Background(), args)
		if exitCode(err) != 2 || !strings.Contains(err.Error(), "never from an argument") {
			t.Errorf("tenant child %s = %v (exit %d); want the refusal, exit 2", strings.Join(args, " "), err, exitCode(err))
		}
	}
	if exists(dir) {
		t.Errorf("a refused install left %s behind", dir)
	}
	// The code was not spent by being refused.
	if _, digits, _ := enrollWith(t, code, childInstallOpts{}); digits == "" {
		t.Error("the code no longer enrolls")
	}
}

// enrollWith is enrollChild for a code the test already holds.
func enrollWith(t *testing.T, code string, o childInstallOpts) (dir, digits string, err error) {
	t.Helper()
	childServiceSeams(t)
	(&cmdRecorder{}).install(t)
	dir = filepath.Join(realTemp(t), "child")
	if o.ceiling == "" {
		o.ceiling = "full"
	}
	if o.sandbox == "" {
		o.sandbox = "host"
	}
	var out bytes.Buffer
	err = childInstall(context.Background(), strings.NewReader(code), &out, dir, o)
	if found := confirmationCode.FindStringSubmatch(out.String()); found != nil {
		digits = found[1]
	}
	return dir, digits, err
}

func TestChildInstallRefusals(t *testing.T) {
	m := newFleetMaster(t)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"a ceiling that is none":       {[]string{"install", "--dir", filepath.Join(t.TempDir(), "c"), "--ceiling", "root"}, "full, no-exec or read-only"},
		"--system with a directory":    {[]string{"install", "--system", "--dir", filepath.Join(t.TempDir(), "c")}, "does not take --dir"},
		"--system without elevation":   {[]string{"install", "--system"}, "needs an elevated terminal"},
		"a sandbox this build has not": {[]string{"install", "--dir", filepath.Join(t.TempDir(), "c"), "--sandbox", "chroot"}, `--sandbox "chroot"`},
		"an argument to another verb":  {[]string{"pause", "now"}, "unexpected argument"},
		"no verb":                      {nil, "usage: tenant child"},
		"an unknown verb":              {[]string{"reinstall"}, "unknown child command"},
	} {
		fakeElevated(t, false)
		err := cmdChild(context.Background(), tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if exists(childSystemDir()) {
		t.Errorf("a refused --system install made %s", childSystemDir())
	}

	// No code at all: nothing is minted.
	if dir, _, err := enrollWith(t, "  \n\n", childInstallOpts{}); exitCode(err) != 2 || !strings.Contains(err.Error(), "no enrollment code") || exists(dir) {
		t.Errorf("an empty standard input: %v; directory made: %v", err, exists(dir))
	}
	// A code that is none, and one the master does not take: the key that was
	// minted for it is gone again, so the next attempt starts clean.
	for name, code := range map[string]string{"not a code": "hunter2", "a peer invite": "tntpeer1_abc", "a code the master never made": forgedCode(t, m)} {
		dir, _, err := enrollWith(t, code, childInstallOpts{})
		if err == nil {
			t.Errorf("%s enrolled", name)
		}
		if exists(fleet.ChildIdentityPath(dir)) || exists(fleet.ChildPath(dir)) {
			t.Errorf("%s: the failed enrollment left a key or a child.json in %s", name, dir)
		}
	}

	// A code works once. The second machine that presents it is refused, and
	// the first is untouched.
	code := m.code("box")
	first, digits, err := enrollWith(t, code, childInstallOpts{})
	if err != nil || digits == "" {
		t.Fatalf("the first use of a code: %v", err)
	}
	if dir, _, err := enrollWith(t, code, childInstallOpts{}); err == nil || exists(fleet.ChildPath(dir)) {
		t.Errorf("the second use of a code: %v", err)
	}
	// An enrolled machine is not enrolled over: its key is its identity.
	key := mustRead(t, fleet.ChildIdentityPath(first))
	var out bytes.Buffer
	err = childInstall(context.Background(), strings.NewReader(m.code("other")), &out, first, childInstallOpts{ceiling: "full", sandbox: "host"})
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "already enrolled") || !bytes.Equal(key, mustRead(t, fleet.ChildIdentityPath(first))) {
		t.Errorf("install over an enrolled machine: %v", err)
	}
}

// forgedCode is a well-formed code for m whose secret m never issued.
func forgedCode(t *testing.T, m *fleetMaster) string {
	t.Helper()
	other := fleet.Open(t.TempDir())
	if err := other.SetAddresses([]string{m.mgr.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	code, err := other.CreateEnrollment(m.id.Fingerprint, "box", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The code may come in the environment instead, and does not stay there for
// what the command starts.
func TestChildInstallReadsTheCodeFromTheEnvironment(t *testing.T) {
	m := newFleetMaster(t)
	t.Setenv(childCodeEnv, m.code("box"))
	dir, digits, err := enrollWith(t, "", childInstallOpts{ceiling: "no-exec"})
	if err != nil || digits == "" {
		t.Fatalf("install with the code in $%s: %v", childCodeEnv, err)
	}
	if v, set := os.LookupEnv(childCodeEnv); set {
		t.Errorf("$%s is still set to %q after the install read it", childCodeEnv, v)
	}
	child, err := fleet.LoadChild(fleet.ChildPath(dir))
	if err != nil || child.Ceiling != fleet.CeilingNoExec || child.Name != "box" || child.MasterFingerprint != m.id.Fingerprint {
		t.Errorf("child.json = %+v, %v", child, err)
	}
	for _, p := range []string{fleet.ChildPath(dir), fleet.ChildIdentityPath(dir)} {
		if pr := secrets.Probe(p); !pr.Exists || !pr.Protected {
			t.Errorf("%s is not owner-only: %+v", p, pr)
		}
	}
	// What the machine said of itself reached the master, pending.
	mach, err := m.store.ByName("box")
	if err != nil || mach.State != fleet.StatePending || mach.Reported.Ceiling != "no-exec" || mach.Reported.OS != runtime.GOOS {
		t.Errorf("on the master: %+v, %v", mach, err)
	}
}

// --- the mux ---

// A child serves the tools that act on its machine and none that need an
// agent or its master's state; and every shell command asks, the ones a
// master's own broker would wave through as read-only included.
func TestChildMuxAsksForEveryCommand(t *testing.T) {
	dir := filepath.Join(realTemp(t), "child")
	var asked []string
	allow := false
	confirm := func(_ context.Context, action, detail string) bool {
		asked = append(asked, action+"|"+detail)
		return allow
	}
	current := func() (fleet.Child, error) { return fleet.Child{Sandbox: "host", Ceiling: fleet.CeilingFull}, nil }
	mux, _ := buildChildMux(context.Background(), dir, confirm, current, quietLog)
	defer mux.Close()

	var names []string
	for _, spec := range mux.All() {
		names = append(names, spec.Name)
	}
	for _, want := range []string{"os_sysinfo", "os_read_file", "os_list_dir", "os_processes", "os_write_file", "os_edit_file", "os_exec", "os_process", "web_navigate", "web_search"} {
		if !slices.Contains(names, want) {
			t.Errorf("a child does not serve %s", want)
		}
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "os_") && !strings.HasPrefix(name, "web_") {
			t.Errorf("a child with no config.json serves %s", name)
		}
	}

	for _, command := range []string{say("hello"), "ls", "git status", "cat /etc/hostname", "Get-ChildItem"} {
		if !osys.ReadOnly(command) {
			t.Fatalf("%q is not a command the classifier calls read-only; pick another", command)
		}
		asked = nil
		args, _ := json.Marshal(map[string]string{"command": command})
		out, failed, err := mux.Dispatch(context.Background(), model.ToolCall{Name: "os_exec", Arguments: args})
		if err != nil || !failed || !strings.Contains(out, "wasn't approved") {
			t.Errorf("%q, denied: %q, failed %v, %v", command, out, failed, err)
		}
		if len(asked) != 1 || asked[0] != "os_exec|"+command {
			t.Errorf("%q: asked %q, want one approval for the command", command, asked)
		}
	}
	// Approved, it runs.
	allow, asked = true, nil
	args, _ := json.Marshal(map[string]string{"command": say("hello-from-the-mux")})
	if out, failed, err := mux.Dispatch(context.Background(), model.ToolCall{Name: "os_exec", Arguments: args}); err != nil || failed || !strings.Contains(out, "hello-from-the-mux") {
		t.Errorf("approved: %q, failed %v, %v", out, failed, err)
	}
	if len(asked) != 1 {
		t.Errorf("approved: asked %q", asked)
	}
	// A destructive one asks as destructive, a write as a write: the plugins'
	// own allow switches are off. Both are denied here: no test lets a command
	// that changes this machine's own Tenant through.
	allow, asked = false, nil
	// The first names a directory with no child in it: were the gate ever to
	// fail, the command would find nothing to act on.
	for _, command := range []string{"tenant child uninstall --dir " + filepath.Join(t.TempDir(), "no-child-here"), destructive()} {
		if d, _ := osys.Classify(command); !d {
			t.Fatalf("%q is not flagged destructive", command)
		}
		args, _ = json.Marshal(map[string]string{"command": command})
		if out, failed, _ := mux.Dispatch(context.Background(), model.ToolCall{Name: "os_exec", Arguments: args}); !failed {
			t.Fatalf("a destructive command that was not approved: %q", out)
		}
	}
	args, _ = json.Marshal(map[string]string{"path": filepath.Join(realTemp(t), "new.txt"), "content": "x"})
	if out, failed, _ := mux.Dispatch(context.Background(), model.ToolCall{Name: "os_write_file", Arguments: args}); !failed {
		t.Errorf("a write that was not approved: %q", out)
	}
	if len(asked) != 3 || !strings.HasPrefix(asked[0], "os_exec_dangerous|tenant child uninstall") ||
		!strings.HasPrefix(asked[1], "os_exec_dangerous|") || !strings.HasPrefix(asked[2], "os_write|") {
		t.Errorf("asked %q, want os_exec_dangerous twice and then os_write", asked)
	}
}

// The no-exec ceiling's question, answered for the actions a child's tools
// raise: only what is known not to run a command passes.
func TestChildRunsCommand(t *testing.T) {
	for action, runs := range map[string]bool{
		"os_exec": true, "os_exec_dangerous": true, actionExecUnsandboxed: true,
		"mcp:files:write_file": true, "something_new": true, "": true,
		"os_write": false, "os_append": false, "os_edit": false, "os_mkdir": false, "os_write_outside": false,
		"web_interact": false, "web_transact": false, "sql_ddl": false, "wiki_save": false, "imessage_send": false,
	} {
		if got := childRunsCommand(action); got != runs {
			t.Errorf("childRunsCommand(%q) = %v, want %v", action, got, runs)
		}
	}
}

// SQL and the wiki are served once the child's own config.json configures
// them, and not before.
func TestChildServesWhatItsConfigConfigures(t *testing.T) {
	dir := filepath.Join(realTemp(t), "child")
	notes := filepath.Join(realTemp(t), "notes")
	if err := os.MkdirAll(notes, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notes, "a.md"), []byte("# A note\n\nabout the fleet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{
		"skills": map[string]any{
			"sql":      map[string]any{"enabled": true, "settings": map[string]string{"db": filepath.Join(realTemp(t), "data.db")}},
			"wiki":     map[string]any{"enabled": true, "settings": map[string]string{"dir": notes}},
			"github":   map[string]any{"enabled": true, "settings": map[string]string{"repo": "a/b"}},
			"imessage": map[string]any{"enabled": false},
		},
		"mcp_servers": []map[string]any{{"name": "broken", "command": []string{filepath.Join(dir, "no-such-server")}, "sandbox": "host"}},
		"mcp_remotes": []string{"https://mcp.example.invalid/sse"},
	})
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launchConfigPath(dir), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	confirm := func(context.Context, string, string) bool { return false }
	current := func() (fleet.Child, error) { return fleet.Child{Sandbox: "host", Ceiling: fleet.CeilingFull}, nil }
	mux, _ := buildChildMux(context.Background(), dir, confirm, current, quietLog)
	defer mux.Close()
	var names []string
	for _, spec := range mux.All() {
		names = append(names, spec.Name)
	}
	for _, want := range []string{"sql_schema", "sql_query", "wiki_list", "wiki_read", "wiki_save", "os_exec"} {
		if !slices.Contains(names, want) {
			t.Errorf("configured, and not served: %s (serving %v)", want, names)
		}
	}
	for _, name := range names {
		for _, never := range []string{"github", "imessage", "mcp", "memory", "fleet", "ui_", "peer", "cron"} {
			if strings.HasPrefix(name, never) {
				t.Errorf("a child serves %s", name)
			}
		}
	}
	// A write through SQL is not something a child does: the plugin's own
	// switch is off and it has no way to ask.
	args, _ := json.Marshal(map[string]string{"sql": "CREATE TABLE t (x)"})
	if out, failed, _ := mux.Dispatch(context.Background(), model.ToolCall{Name: "sql_exec", Arguments: args}); !failed {
		t.Errorf("DDL that was not approved ran: %q", out)
	}
	if out, failed, err := mux.Dispatch(context.Background(), model.ToolCall{Name: "wiki_list", Arguments: json.RawMessage(`{}`)}); err != nil || failed || !strings.Contains(out, "a.md") {
		t.Errorf("wiki_list = %q, failed %v, %v", out, failed, err)
	}
}

// --- the file tools' boundary ---

func TestChildFileBoundaryLists(t *testing.T) {
	none := func(string) string { return "" }
	if got := machineSecrets("windows", none); !slices.Contains(got, `C:\Windows\System32\config`) || !slices.Contains(got, `C:\ProgramData\ssh\ssh_host_*_key`) {
		t.Errorf("windows, default folders: %v", got)
	}
	moved := func(k string) string {
		return map[string]string{"SystemRoot": `D:\WinNT`, "ProgramData": `E:\PD`, "SystemDrive": "D:"}[k]
	}
	if got := machineSecrets("windows", moved); !slices.Contains(got, `D:\WinNT\System32\config`) || !slices.Contains(got, `E:\PD\Microsoft\Crypto`) || !slices.Contains(got, `D:\pagefile.sys`) {
		t.Errorf("windows, moved folders: %v", got)
	}
	if got := machineSecrets("linux", none); !slices.Contains(got, "/etc/shadow") || !slices.Contains(got, "/etc/ssh/ssh_host_*_key") {
		t.Errorf("linux: %v", got)
	}
	if got := machineSecrets("darwin", none); !slices.Contains(got, "/etc/master.passwd") || !slices.Contains(got, "/Library/Keychains") {
		t.Errorf("darwin: %v", got)
	}
	if got := childDenyRoots("linux"); !slices.Contains(got, "/proc") || !slices.Contains(got, "/dev") {
		t.Errorf("linux deny: %v", got)
	}
	if !slices.Contains(accountHomes("linux", none), "/root") || !slices.Contains(accountHomes("darwin", none), "/var/root") {
		t.Error("root's own home is not among the homes")
	}
	home, _ := os.UserHomeDir()
	if got := accountHomes(runtime.GOOS, os.Getenv); !slices.Contains(got, home) {
		t.Errorf("this account's home %s is not among %v", home, got)
	}

	for p, device := range map[string]bool{
		`\\.\PhysicalDrive0`: true, `//./C:`: true, `\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy1\Windows\System32\config\SAM`: true,
		`\\?\C:\Users\x\file.txt`: true, `\\localhost\C$\Windows\System32\config\SAM`: true, `  \\server\share\f`: true,
		`C:\Users\x\file.txt`: false, `D:\data`: false, `notes.txt`: false, `..\x`: false,
	} {
		if got := notAFilePath("windows", p, p); got != device {
			t.Errorf("notAFilePath(windows, %q) = %v, want %v", p, got, device)
		}
		if notAFilePath("linux", p, p) {
			t.Errorf("notAFilePath(linux, %q): a rule for Windows was applied elsewhere", p)
		}
	}
	if !notAFilePath("windows", `C:\link`, `\\?\GLOBALROOT\Device\HarddiskVolume9\x`) {
		t.Error("a link that resolves into a volume's object name passed")
	}

	// The roots a child gets: its own directory and the operator's additions
	// secret, the binary protected, and no write roots.
	dir := filepath.Join(realTemp(t), "child")
	roots := childRoots(dir, []string{"/operator/vault"})
	if len(roots.Write) != 0 || !slices.Contains(roots.Secret, dir) || !slices.Contains(roots.Secret, "/operator/vault") ||
		!slices.Contains(roots.Protect, exePath()) || !slices.Contains(roots.Secret, filepath.Join(home, ".ssh")) {
		t.Errorf("roots: %d write roots; protect %v", len(roots.Write), roots.Protect)
	}
}

// --- end to end, in one process ---

// A machine is enrolled, waits, is confirmed and connects; `tenant child
// status` says each state in words, with the six digits while it waits.
func TestChildWaitsForConfirmation(t *testing.T) {
	m := newFleetMaster(t)
	dir, digits, said := enrollChild(t, m, "box", childInstallOpts{})
	if !strings.Contains(said, "Enrolled as box") || !strings.Contains(said, "tenant fleet confirm box") || !strings.Contains(said, "Installed") {
		t.Errorf("install said:\n%s", said)
	}
	if out, err := status(dir); exitCode(err) != 3 || !strings.Contains(out, "not started yet") || !strings.Contains(out, "ceiling:  full") {
		t.Errorf("status before the service ran (exit %d):\n%s", exitCode(err), out)
	}
	stop := startChild(t, dir)
	awaitStatus(t, dir, "waiting for confirmation", 3)
	if out, _ := status(dir); !strings.Contains(out, "type "+digits+" on the master (tenant fleet confirm box)") {
		t.Errorf("waiting, and the six digits %s are not shown again:\n%s", digits, out)
	}
	if _, err := m.store.Confirm(m.id.Fingerprint, mustMachine(t, m, "box").ID, wrongDigits(digits)); err == nil {
		t.Fatal("the master took the wrong digits")
	}
	mach := m.confirm("box", digits)
	m.awaitOnline(mach.ID, true)
	out := awaitStatus(t, dir, "connected to the master at "+m.mgr.Addr().String(), 0)
	if !strings.Contains(out, "machine:  box ("+mach.ID+")") {
		t.Errorf("status:\n%s", out)
	}
	// Revoked on the master: the child is told, stops, and says so.
	if err := m.store.Revoke(mach.ID); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, dir, "the master rejected this machine: it is revoked", 3)
	stop() // the run loop has returned by itself; this collects it
	// And it does not dial again with that key.
	stop2 := startChild(t, dir)
	time.Sleep(300 * time.Millisecond)
	stop2()
	if out, _ := status(dir); !strings.Contains(out, "it is revoked") {
		t.Errorf("after a restart of a revoked child:\n%s", out)
	}
}

func mustMachine(t *testing.T, m *fleetMaster, name string) fleet.Machine {
	t.Helper()
	mach, err := m.store.ByName(name)
	if err != nil {
		t.Fatal(err)
	}
	return mach
}

func wrongDigits(digits string) string {
	if digits[0] == '9' {
		return "0" + digits[1:]
	}
	return "9" + digits[1:]
}

// awaitStatus waits until `tenant child status` says want, with that exit
// code, and returns what it printed.
func awaitStatus(t *testing.T, dir, want string, code int) string {
	t.Helper()
	for deadline := time.Now().Add(childWait); ; time.Sleep(20 * time.Millisecond) {
		out, err := status(dir)
		if strings.Contains(out, want) && exitCode(err) == code {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("status never said %q with exit %d; it says (exit %d, %v):\n%s", want, code, exitCode(err), err, out)
		}
	}
}

// The states a child can be in, as status words them.
func TestChildStatusWords(t *testing.T) {
	if _, err := status(filepath.Join(t.TempDir(), "nothing")); err == nil || !strings.Contains(err.Error(), "no child is enrolled here") {
		t.Errorf("status with no child: %v", err)
	}
	m := newFleetMaster(t)
	dir, _, _ := enrollChild(t, m, "box", childInstallOpts{ceiling: "read-only"})
	child, err := fleet.LoadChild(fleet.ChildPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	// A child process holds the directory, so what it last wrote is current.
	held, err := fleet.OpenAudit(dir, nil)
	if err != nil || held.Ready() != nil {
		t.Fatal(err)
	}
	cert := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		st   fleet.ChildStatus
		want string
	}{
		{fleet.ChildStatus{State: fleet.LinkWaiting, Certificate: cert}, "waiting for confirmation since 2026-11-18 12:00:00: type " + fleet.ConfirmCode(child.MasterFingerprint, cert) + " on the master"},
		{fleet.ChildStatus{State: fleet.LinkPaused}, "paused on the master"},
		{fleet.ChildStatus{State: fleet.LinkRejected, Detail: "x"}, "the master rejected this machine (since"},
		{fleet.ChildStatus{State: fleet.LinkRevoked}, "the master rejected this machine: it is revoked"},
		{fleet.ChildStatus{State: fleet.LinkOffline, Detail: "10.0.0.1:7443: connection refused"}, "offline since 2026-11-18 12:00:00, trying again: 10.0.0.1:7443: connection refused"},
		{fleet.ChildStatus{State: fleet.LinkOffline}, "trying again: the master could not be reached"},
	} {
		tc.st.Since = time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
		b, _ := json.Marshal(tc.st)
		if err := os.WriteFile(fleet.ChildStatusPath(dir), b, 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := status(dir); exitCode(err) != 3 || !strings.Contains(out, tc.want) || !strings.Contains(out, "ceiling:  read-only") {
			t.Errorf("%s (exit %d):\n%s\nwant %q", tc.st.State, exitCode(err), out, tc.want)
		}
	}
	// With no child process, the last state is history, except "revoked".
	_ = held.Close()
	if out, _ := status(dir); !strings.Contains(out, "not running: no child process holds this directory (it last recorded \"offline\"") {
		t.Errorf("not running:\n%s", out)
	}
	// Paused here comes before anything the link says.
	var said bytes.Buffer
	if err := childPause(&said, dir, true); err != nil || !fleet.Paused(dir) || !strings.Contains(said.String(), "Paused") {
		t.Fatalf("pause: %v, %q", err, said.String())
	}
	if out, err := status(dir); exitCode(err) != 3 || !strings.Contains(out, "paused on this machine") {
		t.Errorf("paused:\n%s", out)
	}
	if err := childPause(&said, dir, false); err != nil || fleet.Paused(dir) {
		t.Fatalf("resume: %v", err)
	}
	// A child.json this machine cannot make sense of.
	if err := os.WriteFile(fleet.ChildPath(dir), bytes.Replace(mustRead(t, fleet.ChildPath(dir)), []byte(`"read-only"`), []byte(`"anything"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := status(dir); err == nil || !strings.Contains(err.Error(), "serves nothing") {
		t.Errorf("status on an invalid child.json: %v", err)
	}
	if err := childPause(&said, filepath.Join(t.TempDir(), "nothing"), true); err == nil {
		t.Error("paused a child that is not there")
	}
}

// The whole of it, through a real link: reads that do not ask, commands and
// writes that do, a background command, a pause that kills what runs, a
// resume, and the audit log of it all.
func TestChildEndToEnd(t *testing.T) {
	m, dir, mach := upChild(t, childInstallOpts{})
	work := realTemp(t)
	id := mach.ID

	// What it serves: this machine's tools, and no way on to another machine.
	names := m.tools(id)
	for _, want := range []string{"os_read_file", "os_exec", "os_process", "os_write_file", "web_navigate"} {
		if !slices.Contains(names, want) {
			t.Errorf("the child does not serve %s", want)
		}
	}
	for _, name := range names {
		for _, never := range []string{"fleet_", "memory_", "ui_", "peer", "cron", "team", "helper", "research", "goal", "github", "gsuite", "discord"} {
			if strings.HasPrefix(name, never) {
				t.Errorf("the child serves %s", name)
			}
		}
	}

	// A read does not ask, and its result names its audit record.
	note := filepath.Join(work, "note.txt")
	if err := os.WriteFile(note, []byte("the note's own words\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := m.ok(id, "os_read_file", map[string]any{"path": note})
	if !strings.Contains(res.Text, "the note's own words") || m.askedCount() != 0 {
		t.Errorf("read = %q; approvals asked: %d", res.Text, m.askedCount())
	}
	if sum, err := fleet.VerifyAudit(dir); err != nil || sum.Records != 2 || sum.LastHash != res.Audit || len(res.Audit) != 64 {
		t.Errorf("audit after one call: %+v, %v; the result carries %q", sum, err, res.Audit)
	}

	// A command asks, whatever it is; denied it does not run.
	marker := filepath.Join(work, "ran.txt")
	m.allow.Store(false)
	m.refused(id, "os_exec", map[string]any{"command": beat(marker)}, "wasn't approved")
	if m.askedCount() != 1 || !strings.HasPrefix(m.lastAsked(), "os_exec|") {
		t.Errorf("asked: %q", m.lastAsked())
	}
	m.refused(id, "os_exec", map[string]any{"command": say("x")}, "wasn't approved")
	if m.askedCount() != 2 || m.lastAsked() != "os_exec|"+say("x") {
		t.Errorf("a command the classifier calls read-only: asked %q", m.lastAsked())
	}
	time.Sleep(300 * time.Millisecond)
	if exists(marker) {
		t.Error("a denied command ran")
	}
	m.allow.Store(true)
	if res := m.ok(id, "os_exec", map[string]any{"command": say("hello-from-the-child")}); !strings.Contains(res.Text, "hello-from-the-child") {
		t.Errorf("exec = %q", res.Text)
	}

	// A write asks too, anywhere: a child has no writable roots of its own.
	target := filepath.Join(work, "made", "by-the-master.txt")
	before := m.askedCount()
	m.ok(id, "os_write_file", map[string]any{"path": target, "content": "written"})
	if got := string(mustRead(t, target)); got != "written" || m.askedCount() != before+1 || !strings.HasPrefix(m.lastAsked(), "os_write|") {
		t.Errorf("write: file %q, asked %q", got, m.lastAsked())
	}

	// A background command, and a foreground one that is still running.
	back, front := filepath.Join(work, "back.txt"), filepath.Join(work, "front.txt")
	if res := m.ok(id, "os_exec", map[string]any{"command": beat(back), "background": true}); !strings.Contains(res.Text, "in the background") {
		t.Errorf("background = %q", res.Text)
	}
	awaitBeating(t, back)
	if res := m.ok(id, "os_process", map[string]any{"action": "list"}); !strings.Contains(res.Text, "running") {
		t.Errorf("os_process list = %q", res.Text)
	}
	running := make(chan error, 1)
	go func() {
		_, err := m.call(id, "os_exec", map[string]any{"command": beat(front), "timeout": 600})
		running <- err
	}()
	awaitBeating(t, front)

	// Pause: the link drops, the call ends, both commands die.
	var said bytes.Buffer
	if err := cmdChild(context.Background(), []string{"pause", "--dir", dir}); err != nil {
		t.Fatal(err)
	}
	m.awaitOnline(id, false)
	select {
	case err := <-running:
		if err == nil {
			t.Error("the call that was running when the machine was paused came back as if it had finished")
		}
	case <-time.After(childWait):
		t.Fatal("the call that was running when the machine was paused never returned")
	}
	awaitStill(t, front)
	awaitStill(t, back)
	if out, err := status(dir); exitCode(err) != 3 || !strings.Contains(out, "paused on this machine") {
		t.Errorf("status while paused:\n%s", out)
	}
	if _, err := m.call(id, "os_read_file", map[string]any{"path": note}); !errors.Is(err, fleet.ErrOffline) {
		t.Errorf("a call to a paused machine: %v, want offline", err)
	}

	// Resume: it connects again, and what was running is gone.
	if err := childPause(&said, dir, false); err != nil {
		t.Fatal(err)
	}
	m.awaitOnline(id, true)
	if res := m.ok(id, "os_process", map[string]any{"action": "list"}); strings.Contains(res.Text, "running") {
		t.Errorf("after pause and resume a background command is still listed as running: %q", res.Text)
	}
	awaitStatus(t, dir, "connected to the master", 0)

	// The audit log has every call, chained, and the system log a line each.
	last := m.ok(id, "os_sysinfo", nil)
	sum, err := fleet.VerifyAudit(dir)
	if err != nil || sum.Records < 16 || sum.Records%2 != 0 || sum.LastHash != last.Audit {
		t.Errorf("audit: %+v, %v; the last result carries %s", sum, err, last.Audit)
	}
	var out bytes.Buffer
	if err := childAudit(&out, dir); err != nil || !strings.Contains(out.String(), "chain holds") {
		t.Errorf("tenant child audit: %v\n%s", err, out.String())
	}
	lines := childOSLog.all()
	if len(lines) < sum.Records || !slices.ContainsFunc(lines, func(l string) bool { return strings.HasSuffix(l, "sha256="+last.Audit) }) {
		t.Errorf("%d lines for the system log, %d records; none for the last record", len(lines), sum.Records)
	}
	for _, l := range lines {
		if strings.Contains(l, "hello-from-the-child") || strings.Contains(l, work) {
			t.Errorf("the system log line carries what a tool was given: %q", l)
		}
	}
	// Everything the child wrote is in its two directories, under these names.
	for where, known := range map[string][]string{
		dir:                 {"child.json", "child.json.lock", "child-identity.pem", "child-link.json", "child-paused", "audit", "memory"},
		fleet.AuditDir(dir): {"audit.jsonl", "audit.lock"},
		childDataDir(dir):   {"proc", "screenshots", "wiki", "mcp", "logs"},
	} {
		entries, err := os.ReadDir(where)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !slices.Contains(known, e.Name()) {
				t.Errorf("the child left %s in %s; the files a child creates are a list the documentation gives", e.Name(), where)
			}
		}
	}

	// Tampering with it is found.
	logFile := filepath.Join(fleet.AuditDir(dir), "audit.jsonl")
	if err := os.WriteFile(logFile, bytes.Replace(mustRead(t, logFile), []byte("hello-from-the-child"), []byte("nothing-to-see-here!"), 1), 0o600); err == nil {
		if err := childAudit(&out, dir); err == nil {
			t.Error("a changed audit log verifies")
		}
	}
}

// The ceiling is this machine's word, read from its disk at every check: a
// change takes effect with no restart, and the master is not asked about
// what the machine rules out.
func TestChildCeilingOnDisk(t *testing.T) {
	m, dir, mach := upChild(t, childInstallOpts{})
	id := mach.ID
	work := realTemp(t)
	note := filepath.Join(work, "note.txt")
	if err := os.WriteFile(note, []byte("readable"), 0o644); err != nil {
		t.Fatal(err)
	}
	set := func(edit func(*fleet.Child)) {
		t.Helper()
		if err := fleet.UpdateChild(fleet.ChildPath(dir), func(c *fleet.Child) error { edit(c); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	m.ok(id, "os_exec", map[string]any{"command": say("full")})
	asked := m.askedCount()

	// read-only: every gated action is refused here; reads go on.
	set(func(c *fleet.Child) { c.Ceiling = fleet.CeilingReadOnly })
	m.refused(id, "os_exec", map[string]any{"command": say("ro")}, "os_exec (ceiling read-only)")
	m.refused(id, "os_exec", map[string]any{"command": destructive()}, "os_exec_dangerous (ceiling read-only)")
	m.refused(id, "os_write_file", map[string]any{"path": filepath.Join(work, "w.txt"), "content": "x"}, "os_write (ceiling read-only)")
	m.refused(id, "os_make_dir", map[string]any{"path": filepath.Join(work, "d")}, "os_mkdir (ceiling read-only)")
	m.refused(id, "os_process", map[string]any{"action": "kill", "id": "abc123"}, "ceiling read-only")
	m.ok(id, "os_read_file", map[string]any{"path": note})
	m.ok(id, "os_list_dir", map[string]any{"path": work})
	if m.askedCount() != asked || exists(filepath.Join(work, "w.txt")) || exists(filepath.Join(work, "d")) {
		t.Errorf("under read-only the master was asked %d times, or something was written", m.askedCount()-asked)
	}

	// no-exec: commands are refused here, a write is the master's to decide.
	set(func(c *fleet.Child) { c.Ceiling = fleet.CeilingNoExec })
	m.refused(id, "os_exec", map[string]any{"command": say("ne")}, "os_exec (ceiling no-exec)")
	if m.askedCount() != asked {
		t.Error("under no-exec the master was asked about a command")
	}
	m.ok(id, "os_write_file", map[string]any{"path": filepath.Join(work, "w.txt"), "content": "x"})
	if m.askedCount() != asked+1 {
		t.Error("under no-exec a write did not ask the master")
	}

	// The tools list: only those.
	set(func(c *fleet.Child) { c.Ceiling, c.Tools = fleet.CeilingFull, []string{"os_read_file", "os_sysinfo"} })
	if got := m.tools(id); !slices.Equal(got, []string{"os_sysinfo", "os_read_file"}) && !slices.Equal(got, []string{"os_read_file", "os_sysinfo"}) {
		t.Errorf("served with a tools list: %v", got)
	}
	m.refused(id, "os_list_dir", map[string]any{"path": work}, "does not serve os_list_dir")
	m.refused(id, "os_exec", map[string]any{"command": say("x")}, "does not serve os_exec")
	m.ok(id, "os_read_file", map[string]any{"path": note})

	// Widened by hand to something that is no ceiling: everything is refused,
	// the read that worked a moment ago included.
	raw := mustRead(t, fleet.ChildPath(dir))
	for name, bad := range map[string][]byte{
		"a ceiling that is none": bytes.Replace(raw, []byte(`"ceiling": "full"`), []byte(`"ceiling": "unlimited"`), 1),
		"an unknown key":         bytes.Replace(raw, []byte(`"ceiling": "full"`), []byte(`"ceiling": "full", "allow_exec": true`), 1),
		"not JSON":               []byte("ceiling = full\n"),
		"empty":                  {},
	} {
		if err := os.WriteFile(fleet.ChildPath(dir), bad, 0o600); err != nil {
			t.Fatal(err)
		}
		m.refused(id, "os_read_file", map[string]any{"path": note}, "child.json cannot be read")
		if got := m.tools(id); len(got) != 0 {
			t.Errorf("%s: the child lists %v", name, got)
		}
	}
	// Put right, it serves again, with no restart.
	if err := os.WriteFile(fleet.ChildPath(dir), bytes.Replace(raw, []byte(`"os_sysinfo"`), []byte(`"os_list_dir"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	m.ok(id, "os_list_dir", map[string]any{"path": work})
}

// Reads do not ask, so the file tools refuse what must not be read: the
// child's own files, and what the operator added, by any name the file has.
func TestChildSecretList(t *testing.T) {
	m, dir, mach := upChild(t, childInstallOpts{})
	id := mach.ID
	work := realTemp(t)
	vault := filepath.Join(work, "vault")
	if err := os.MkdirAll(filepath.Join(vault, "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	secret, deep := filepath.Join(vault, "key.pem"), filepath.Join(vault, "deep", "token.json")
	for _, p := range []string{secret, deep} {
		if err := os.WriteFile(p, []byte("-----BEGIN THE SECRET-----\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Before the operator lists it, it is a file like any other.
	m.ok(id, "os_read_file", map[string]any{"path": secret})
	if err := fleet.UpdateChild(fleet.ChildPath(dir), func(c *fleet.Child) error { c.SecretPaths = []string{vault}; return nil }); err != nil {
		t.Fatal(err)
	}
	asked := m.askedCount()
	other := filepath.Join(work, "elsewhere")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	ways := map[string]string{"by its path": secret, "a file deeper in": deep,
		"with dots": work + string(filepath.Separator) + filepath.Join("elsewhere", "..", "vault", "deep", "token.json")}
	if err := os.Link(secret, filepath.Join(other, "hard.pem")); err == nil {
		ways["through a hard link"] = filepath.Join(other, "hard.pem")
	}
	if err := os.Symlink(secret, filepath.Join(other, "soft.pem")); err == nil {
		ways["through a symlink to the file"] = filepath.Join(other, "soft.pem")
	}
	if err := os.Symlink(vault, filepath.Join(other, "door")); err == nil {
		ways["through a symlink to its folder"] = filepath.Join(other, "door", "deep", "token.json")
	} else if runtime.GOOS != "windows" {
		t.Fatalf("no symlink here: %v", err)
	}
	for how, path := range ways {
		res, err := m.call(id, "os_read_file", map[string]any{"path": path})
		if err != nil || !res.IsError || !strings.Contains(res.Text, "holds secrets") || strings.Contains(res.Text, "BEGIN THE SECRET") {
			t.Errorf("the secret read %s (%s): %q, %v", how, path, res.Text, err)
		}
	}
	m.refused(id, "os_write_file", map[string]any{"path": filepath.Join(vault, "new.txt"), "content": "x"}, "holds secrets")
	m.refused(id, "os_list_dir", map[string]any{"path": vault}, "holds secrets")

	// The child's own files, whoever asks and whatever the operator listed.
	for _, own := range []string{fleet.ChildPath(dir), fleet.ChildIdentityPath(dir), fleet.ChildStatusPath(dir),
		filepath.Join(fleet.AuditDir(dir), "audit.jsonl"), launchConfigPath(dir)} {
		m.refused(id, "os_read_file", map[string]any{"path": own}, "holds secrets")
		m.refused(id, "os_write_file", map[string]any{"path": own, "content": "{}"}, "holds secrets")
	}
	m.refused(id, "os_write_file", map[string]any{"path": fleet.PausedPath(dir), "content": "x"}, "holds secrets")
	if exe := exePath(); exe != "" {
		m.refused(id, "os_write_file", map[string]any{"path": exe, "content": "x"}, "protected file")
	}
	if runtime.GOOS == "windows" {
		m.refused(id, "os_read_file", map[string]any{"path": `\\.\PhysicalDrive0`}, "names a device")
		m.refused(id, "os_read_file", map[string]any{"path": `\\localhost\C$\Windows\win.ini`}, "names a device")
	} else {
		// Where the kernel shows devices and other processes as files, and the
		// machine's own password database.
		device, passwords := "/proc/self/environ", "/etc/shadow"
		if runtime.GOOS == "darwin" {
			device, passwords = "/dev/disk0", "/etc/master.passwd"
		}
		m.refused(id, "os_read_file", map[string]any{"path": device}, "protected directory")
		m.refused(id, "os_read_file", map[string]any{"path": passwords}, "holds secrets")
	}
	if m.askedCount() != asked {
		t.Errorf("the master was asked %d times about what the machine refuses outright", m.askedCount()-asked)
	}
	// Beside the secrets, the working files are ordinary: a screenshot the
	// browser saved can be fetched.
	shot := filepath.Join(childDataDir(dir), "screenshots", "s.txt")
	if err := os.MkdirAll(filepath.Dir(shot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shot, []byte("pixels"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.ok(id, "os_read_file", map[string]any{"path": shot})
}

// A child that cannot write its audit record refuses the call.
func TestChildRefusesCallsItCannotRecord(t *testing.T) {
	m := newFleetMaster(t)
	dir, digits, _ := enrollChild(t, m, "box", childInstallOpts{})
	if err := os.WriteFile(fleet.AuditDir(dir), []byte("in the way of the audit log"), 0o600); err != nil {
		t.Fatal(err)
	}
	mach := m.confirm("box", digits)
	startChild(t, dir)
	m.awaitOnline(mach.ID, true)
	note := filepath.Join(realTemp(t), "note.txt")
	if err := os.WriteFile(note, []byte("unread"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := m.refused(mach.ID, "os_read_file", map[string]any{"path": note}, "could not write its audit record, so it did not run the call")
	if res.Audit != "" || strings.Contains(res.Text, "unread") {
		t.Errorf("a call with no audit record: %+v", res)
	}
	// With the way cleared the next call is recorded and runs: no restart.
	if err := os.Remove(fleet.AuditDir(dir)); err != nil {
		t.Fatal(err)
	}
	if res := m.ok(mach.ID, "os_read_file", map[string]any{"path": note}); res.Audit == "" {
		t.Error("the call after the log came back carries no audit hash")
	}
}

// Two children on one directory would share a key and a chain.
func TestChildRunsOncePerDirectory(t *testing.T) {
	m := newFleetMaster(t)
	dir, _, _ := enrollChild(t, m, "box", childInstallOpts{})
	startChild(t, dir)
	for deadline := time.Now().Add(childWait); !fleet.AuditHeld(dir); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the running child never took its directory")
		}
	}
	if err := childRun(context.Background(), dir, quietLog, childRunOpts{}); !errors.Is(err, fleet.ErrAuditHeld) {
		t.Errorf("a second child on the same directory: %v", err)
	}
	if err := childRun(context.Background(), filepath.Join(t.TempDir(), "nothing"), quietLog, childRunOpts{}); err == nil || !strings.Contains(err.Error(), "no child is enrolled") {
		t.Errorf("a child with no child.json: %v", err)
	}
}

// Uninstall removes the service and the key, and keeps the audit log.
func TestChildUninstall(t *testing.T) {
	m := newFleetMaster(t)
	dir, digits, _ := enrollChild(t, m, "box", childInstallOpts{})
	mach := m.confirm("box", digits)
	stop := startChild(t, dir)
	m.awaitOnline(mach.ID, true)
	m.ok(mach.ID, "os_sysinfo", nil)
	stop() // as the service manager stops the service before its unit is removed
	rec := &cmdRecorder{}
	rec.install(t)
	var out bytes.Buffer
	if err := childUninstall(context.Background(), &out, dir); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out.String())
	}
	for _, gone := range []string{fleet.ChildPath(dir), fleet.ChildIdentityPath(dir), fleet.ChildStatusPath(dir), fleet.PausedPath(dir), childDataDir(dir)} {
		if exists(gone) {
			t.Errorf("uninstall left %s", gone)
		}
	}
	if !exists(filepath.Join(fleet.AuditDir(dir), "audit.jsonl")) || !strings.Contains(out.String(), "Kept: the audit log") ||
		!strings.Contains(out.String(), "tenant fleet revoke box") {
		t.Errorf("uninstall said:\n%s", out.String())
	}
	if len(rec.calls) == 0 {
		t.Error("uninstall did not ask the service manager to remove the unit")
	}
	if err := childUninstall(context.Background(), &out, dir); err == nil {
		t.Error("uninstalled a child that is not there")
	}
}

// --- as real processes ---

// TestChildHelperProcess is not a test: it is `tenant child …` as a process
// of its own, for the tests below. The arguments come in the environment.
func TestChildHelperProcess(t *testing.T) {
	raw := os.Getenv("TENANT_CHILD_HELPER")
	if raw == "" {
		t.Skip("the tests that run tenant child as a process start this")
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(9)
	}
	time.AfterFunc(10*time.Minute, func() { os.Exit(4) }) // never outlives a test run
	if os.Getenv("TENANT_CHILD_HELPER_HOLD") != "" {
		// The test holds the other end of standard input. When it goes away,
		// however it goes, so does this process.
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			os.Exit(3)
		}()
	}
	// A service manager that writes down what it is asked, and no more.
	if log := os.Getenv("TENANT_CHILD_HELPER_CMDS"); log != "" {
		runCmd = func(_ context.Context, name string, a ...string) (string, error) {
			f, err := os.OpenFile(log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
			if err != nil {
				return "", err
			}
			defer f.Close()
			_, err = fmt.Fprintln(f, strings.Join(append([]string{name}, a...), " "))
			return "", err
		}
	}
	childHelperSeams()
	killChildrenOnExit()
	err := cmdChild(context.Background(), args)
	if err != nil && err.Error() != "" {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	os.Exit(exitCode(err))
}

// tenantChild starts `tenant child <args>` as a real process, with no
// window, and a recorded service manager.
func tenantChild(t *testing.T, stdin io.Reader, hold bool, args ...string) *exec.Cmd {
	t.Helper()
	raw, _ := json.Marshal(args)
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildHelperProcess$")
	cmd.Env = append(os.Environ(), "TENANT_CHILD_HELPER="+string(raw), "TENANT_CHILD_HELPER_CMDS="+filepath.Join(helperDir(t), "service-manager.log"),
		"TENANT_CHILD_HELPER_UNITS="+filepath.Join(helperDir(t), "units"))
	if hold {
		cmd.Env = append(cmd.Env, "TENANT_CHILD_HELPER_HOLD=1")
	}
	cmd.Stdin = stdin
	hideWindow(cmd)
	return cmd
}

var helperDirs sync.Map // test name -> dir

func helperDir(t *testing.T) string {
	if d, ok := helperDirs.Load(t.Name()); ok {
		return d.(string)
	}
	d, _ := helperDirs.LoadOrStore(t.Name(), realTemp(t))
	return d.(string)
}

// runChildCmd runs `tenant child <args>` to its end and returns what it
// printed and its exit code.
func runChildCmd(t *testing.T, stdin string, args ...string) (string, int) {
	t.Helper()
	cmd := tenantChild(t, strings.NewReader(stdin), false, args...)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("tenant child %v did not run: %v", args, err)
	}
	return string(out), cmd.ProcessState.ExitCode()
}

// The same, with every piece a process of its own: the master in this one,
// `tenant child install`, the run loop the service would start, and the
// commands an operator types on the machine.
func TestChildAsRealProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("starts several processes")
	}
	m := newFleetMaster(t)
	dir := filepath.Join(realTemp(t), "child")
	code := m.code("box")

	// The code as an argument is refused by the real command line too.
	if out, exit := runChildCmd(t, "", "install", "--dir", dir, code); exit != 2 || !strings.Contains(out, "never from an argument") || exists(dir) {
		t.Fatalf("install with the code as an argument: exit %d\n%s", exit, out)
	}
	out, exit := runChildCmd(t, code+"\n", "install", "--dir", dir, "--ceiling", "full")
	found := confirmationCode.FindStringSubmatch(out)
	if exit != 0 || found == nil || !strings.Contains(out, "Installed") {
		t.Fatalf("install: exit %d\n%s", exit, out)
	}
	asked := string(mustRead(t, filepath.Join(helperDir(t), "service-manager.log")))
	if !strings.Contains(asked, "tenant-child") && !strings.Contains(asked, `\Tenant\Child`) && !strings.Contains(asked, "com.tenant.child") {
		t.Errorf("the service manager was asked:\n%s", asked)
	}
	if strings.Contains(asked, code) || strings.Contains(out, code) {
		t.Error("the enrollment code is in the service manager's commands or in what install printed")
	}
	mach := m.confirm("box", found[1])

	// The run loop, as the service starts it.
	hold, holdW := io.Pipe()
	var logs bytes.Buffer
	run := tenantChild(t, hold, true, "run", "--dir", dir)
	run.Stdout, run.Stderr = &logs, &logs
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- run.Wait() }()
	t.Cleanup(func() {
		_ = holdW.Close()
		select {
		case <-ended:
		case <-time.After(childWait):
			_ = run.Process.Kill()
			<-ended
			t.Errorf("the child process had to be killed; it logged:\n%s", logs.String())
		}
	})
	m.awaitOnline(mach.ID, true)

	note := filepath.Join(realTemp(t), "note.txt")
	if err := os.WriteFile(note, []byte("read by another process"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := m.ok(mach.ID, "os_read_file", map[string]any{"path": note}); !strings.Contains(res.Text, "read by another process") || res.Audit == "" {
		t.Errorf("read = %+v", res)
	}
	if res := m.ok(mach.ID, "os_exec", map[string]any{"command": say("from-a-real-child")}); !strings.Contains(res.Text, "from-a-real-child") {
		t.Errorf("exec = %q", res.Text)
	}
	m.allow.Store(false)
	m.refused(mach.ID, "os_exec", map[string]any{"command": say("denied")}, "wasn't approved")
	m.allow.Store(true)
	if out, exit := runChildCmd(t, "", "status", "--dir", dir); exit != 0 || !strings.Contains(out, "connected to the master") {
		t.Errorf("status: exit %d\n%s", exit, out)
	}

	// A write asks and lands; then the operator edits child.json on the
	// machine, and the running process holds to it without a restart: a path
	// closed to the file tools, by any way to it, and a ceiling.
	vault := filepath.Join(realTemp(t), "vault")
	secret := filepath.Join(vault, "key.pem")
	m.ok(mach.ID, "os_write_file", map[string]any{"path": secret, "content": "the key"})
	if got := string(mustRead(t, secret)); got != "the key" || !strings.HasPrefix(m.lastAsked(), "os_write|") {
		t.Errorf("write: file %q, asked %q", got, m.lastAsked())
	}
	if err := fleet.UpdateChild(fleet.ChildPath(dir), func(c *fleet.Child) error {
		c.SecretPaths, c.Ceiling = []string{vault}, fleet.CeilingReadOnly
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.refused(mach.ID, "os_read_file", map[string]any{"path": secret}, "holds secrets")
	if door := filepath.Join(realTemp(t), "door"); os.Symlink(vault, door) == nil {
		m.refused(mach.ID, "os_read_file", map[string]any{"path": filepath.Join(door, "key.pem")}, "holds secrets")
	}
	before := m.askedCount()
	m.refused(mach.ID, "os_exec", map[string]any{"command": say("under-a-ceiling")}, "os_exec (ceiling read-only)")
	if m.askedCount() != before {
		t.Error("under read-only the master was asked")
	}
	if err := fleet.UpdateChild(fleet.ChildPath(dir), func(c *fleet.Child) error { c.Ceiling = fleet.CeilingFull; return nil }); err != nil {
		t.Fatal(err)
	}

	// Pause and resume, typed on the machine.
	beatFile := filepath.Join(realTemp(t), "beat.txt")
	m.ok(mach.ID, "os_exec", map[string]any{"command": beat(beatFile), "background": true})
	awaitBeating(t, beatFile)
	if out, exit := runChildCmd(t, "", "pause", "--dir", dir); exit != 0 {
		t.Fatalf("pause: exit %d\n%s", exit, out)
	}
	m.awaitOnline(mach.ID, false)
	awaitStill(t, beatFile)
	if out, exit := runChildCmd(t, "", "status", "--dir", dir); exit != 3 || !strings.Contains(out, "paused on this machine") {
		t.Errorf("status while paused: exit %d\n%s", exit, out)
	}
	if out, exit := runChildCmd(t, "", "resume", "--dir", dir); exit != 0 {
		t.Fatalf("resume: exit %d\n%s", exit, out)
	}
	m.awaitOnline(mach.ID, true)
	m.ok(mach.ID, "os_sysinfo", nil)
	if out, exit := runChildCmd(t, "", "audit", "--dir", dir); exit != 0 || !strings.Contains(out, "chain holds") {
		t.Errorf("audit: exit %d\n%s", exit, out)
	}

	// The service stopped: status says so, and uninstall cleans up.
	_ = holdW.Close()
	select {
	case <-ended:
		ended <- nil
	case <-time.After(childWait):
		t.Fatalf("the child process did not end when its test let go of it; it logged:\n%s", logs.String())
	}
	m.awaitOnline(mach.ID, false)
	if out, exit := runChildCmd(t, "", "status", "--dir", dir); exit != 3 || !strings.Contains(out, "not running") {
		t.Errorf("status with no child process: exit %d\n%s", exit, out)
	}
	if out, exit := runChildCmd(t, "", "uninstall", "--dir", dir); exit != 0 || exists(fleet.ChildIdentityPath(dir)) || !strings.Contains(out, "Kept: the audit log") {
		t.Errorf("uninstall: exit %d\n%s", exit, out)
	}
	if out, exit := runChildCmd(t, "", "status", "--dir", dir); exit != 1 || !strings.Contains(out, "no child is enrolled here") {
		t.Errorf("status after uninstall: exit %d\n%s", exit, out)
	}
}

// The sandbox backend is the machine's own choice, read from child.json for
// every command. One it does not have runs nothing, approved or not: a
// machine that asked for confinement is never given the host instead.
func TestChildSandboxFailsClosed(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS has the restricted backend this test counts on being absent")
	}
	dir := filepath.Join(realTemp(t), "child")
	marker := filepath.Join(realTemp(t), "ran.txt")
	asked := 0
	confirm := func(context.Context, string, string) bool { asked++; return true }
	child := fleet.Child{Sandbox: "restricted", Ceiling: fleet.CeilingFull}
	var broken error
	current := func() (fleet.Child, error) { return child, broken }
	mux, _ := buildChildMux(context.Background(), dir, confirm, current, quietLog)
	defer mux.Close()
	run := func(command string) (string, bool) {
		t.Helper()
		args, _ := json.Marshal(map[string]any{"command": command})
		out, failed, err := mux.Dispatch(context.Background(), model.ToolCall{Name: "os_exec", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return out, failed
	}
	if out, failed := run(beat(marker)); !failed || !strings.Contains(out, "fail closed") || asked != 1 {
		t.Errorf("on a backend this machine has not: %q, failed %v, asked %d times", out, failed, asked)
	}
	// A child.json that stops loading while the child runs: the command the
	// master already approved is not run on a guess.
	broken = errors.New("child.json: not valid")
	if out, failed := run(beat(marker)); !failed || !strings.Contains(out, "child.json: not valid") {
		t.Errorf("with no child.json to read the backend from: %q, failed %v", out, failed)
	}
	time.Sleep(300 * time.Millisecond)
	if exists(marker) {
		t.Fatal("a command ran outside the sandbox the machine asked for")
	}
	// On the host, as the file says again, it runs.
	child.Sandbox, broken = "host", nil
	if out, failed := run(say("on-the-host")); failed || !strings.Contains(out, "on-the-host") {
		t.Errorf("on the host: %q, failed %v", out, failed)
	}
}

// A child whose child.json does not load when it starts stays up, holds no
// link and serves nothing; put right, the file is taken up with no restart.
func TestChildStartsOnAChildFileItCannotRead(t *testing.T) {
	m := newFleetMaster(t)
	dir, digits, _ := enrollChild(t, m, "box", childInstallOpts{})
	mach := m.confirm("box", digits)
	good := mustRead(t, fleet.ChildPath(dir))
	if err := os.WriteFile(fleet.ChildPath(dir), bytes.Replace(good, []byte(`"ceiling": "full"`), []byte(`"ceiling": "everything"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	startChild(t, dir)
	time.Sleep(500 * time.Millisecond)
	if list, _ := m.mgr.Machines(); len(list) != 1 || list[0].Online {
		t.Fatalf("a child with a ceiling that is none connected: %+v", list)
	}
	if _, err := status(dir); err == nil || !strings.Contains(err.Error(), "serves nothing") {
		t.Errorf("status: %v", err)
	}
	if err := os.WriteFile(fleet.ChildPath(dir), good, 0o600); err != nil {
		t.Fatal(err)
	}
	m.awaitOnline(mach.ID, true)
	m.ok(mach.ID, "os_sysinfo", nil)
}

// A config.json that does not load takes away what it would have added and
// nothing else; and an audit log nobody has written to says so.
func TestChildWithoutAConfigItCanRead(t *testing.T) {
	dir := filepath.Join(realTemp(t), "child")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launchConfigPath(dir), []byte(`{"skills": {"sql": `), 0o600); err != nil {
		t.Fatal(err)
	}
	current := func() (fleet.Child, error) { return fleet.Child{Sandbox: "host", Ceiling: fleet.CeilingFull}, nil }
	mux, _ := buildChildMux(context.Background(), dir, func(context.Context, string, string) bool { return false }, current, quietLog)
	defer mux.Close()
	names := 0
	for _, spec := range mux.All() {
		names++
		if !strings.HasPrefix(spec.Name, "os_") && !strings.HasPrefix(spec.Name, "web_") {
			t.Errorf("with a config.json that does not load the child serves %s", spec.Name)
		}
	}
	if names == 0 {
		t.Error("with a config.json that does not load the child serves nothing at all")
	}
	var out bytes.Buffer
	if err := childAudit(&out, dir); err != nil || !strings.Contains(out.String(), "is empty") {
		t.Errorf("audit of a child that was never called: %v\n%s", err, out.String())
	}
	if got := childDenyRoots("darwin"); !slices.Contains(got, "/dev") || childDenyRoots("windows") != nil {
		t.Errorf("deny roots: darwin %v, windows %v", got, childDenyRoots("windows"))
	}
}
