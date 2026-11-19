package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The acceptance cases of the acknowledgement flow (TEN-535, the plan's
// Acceptance) as real processes: `tenant` built with -tags procprotecttest
// (protecttest.go), in which the environment can say that a control cannot
// be applied. Each run has its own --data and --config, no dashboard, the
// echo backend, and a file in place of the system's log.
//
// Where a case needs a terminal, the build is told that its pipes are one
// (TENANT_TEST_TERMINAL=1): no test has a real terminal, and Windows has no
// pseudo-terminal to make one from. The question and the answer then travel
// over the process's real stdin and stderr.
//
// TENANT_PROTECTTEST_BIN names a binary built beforehand, for a machine with
// no Go toolchain (the tests cross-compiled for Linux and run under WSL):
//
//	GOOS=linux go build -tags procprotecttest -o /tmp/tenant-ten535-test ./cmd/tenant

// forcedControls are a control to force off on this system, and a second one
// for "a different control". On Windows the first is the error-reporting
// exclusion, in every run: it is the one control that writes outside the
// process (the user's registry), and forced off it is not applied.
func forcedControls(t *testing.T) (first, second string) {
	switch runtime.GOOS {
	case "windows":
		return "windows.wer-exclusion", "windows.process-dacl"
	case "linux":
		return "linux.core-limit", "linux.dumpable"
	case "darwin":
		return "darwin.core-limit", "darwin.deny-attach"
	}
	t.Skip("no process-protection controls on " + runtime.GOOS)
	return "", ""
}

func protectTestBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("TENANT_PROTECTTEST_BIN"); bin != "" {
		return bin
	}
	if testing.Short() {
		t.Skip("builds tenant with -tags procprotecttest")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go here to build tenant with -tags procprotecttest; TENANT_PROTECTTEST_BIN names one built elsewhere")
	}
	out := filepath.Join(t.TempDir(), "tenant-ten535-test")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-tags", "procprotecttest", "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build tenant -tags procprotecttest: %v\n%s", err, b)
	}
	return out
}

// tenantUnderTest is one install the tagged binary runs against.
type tenantUnderTest struct {
	bin, home, cfg, data, systemLog string
}

func newTenantUnderTest(t *testing.T, bin string) *tenantUnderTest {
	t.Helper()
	root := t.TempDir()
	u := &tenantUnderTest{bin: bin, home: filepath.Join(root, "home"), cfg: filepath.Join(root, "cfg"),
		data: filepath.Join(root, "data"), systemLog: filepath.Join(root, "system.log")}
	for _, d := range []string{u.home, u.cfg, u.data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return u
}

// command is `tenant args...` with env, away from this account's own Tenant:
// a home of its own, and the dirs given on the command line.
func (u *tenantUnderTest) command(ctx context.Context, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, u.bin, args...)
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "TENANT_") || name == "GOTRACEBACK" {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, "HOME="+u.home, "USERPROFILE="+u.home, "APPDATA="+u.home, "LOCALAPPDATA="+u.home,
		"XDG_CONFIG_HOME="+u.home, "XDG_DATA_HOME="+u.home, "TENANT_TEST_SYSTEM_LOG="+u.systemLog)
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

func (u *tenantUnderTest) startArgs(verb string) []string {
	args := []string{verb, "--backend", "echo", "--data", u.data, "--config", u.cfg, "--self-improve=false"}
	if verb == "serve" {
		args = append(args, "--dashboard=false")
	}
	return args
}

// exits runs a start that must end by itself, and returns how.
func (u *tenantUnderTest) exits(t *testing.T, env []string, typed string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := u.command(ctx, env, args...)
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(typed), &out, &errb
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		t.Fatalf("tenant %v did not exit (it started?):\nstdout:\n%s\nstderr:\n%s", args, &out, &errb)
	case errors.As(err, &exit):
		code = exit.ExitCode()
	case err != nil:
		t.Fatalf("tenant %v: %v", args, err)
	}
	return code, out.String(), errb.String()
}

// serves starts serve, waits until it is up and logged is true (the log book
// writes behind the process, and the stop here is a kill), stops it, and
// returns what it wrote until then.
func (u *tenantUnderTest) serves(t *testing.T, env []string, typed string, logged func() bool) (stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := u.command(ctx, env, u.startArgs("serve")...)
	var errb syncBuffer
	cmd.Stdin, cmd.Stderr = strings.NewReader(typed), &errb
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := sync.OnceFunc(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	t.Cleanup(stop) // also when the test fails
	up := false
	var said strings.Builder
	sc := bufio.NewScanner(pipe)

	for sc.Scan() {
		said.WriteString(sc.Text() + "\n")
		if strings.Contains(sc.Text(), "running 24/7") {
			up = true
			break
		}
	}
	for deadline := time.Now().Add(20 * time.Second); up && !logged() && time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	if !up {
		t.Fatalf("serve did not come up:\nstdout:\n%s\nstderr:\n%s", &said, errb.String())
	}
	return said.String(), errb.String()
}

func (u *tenantUnderTest) systemLogLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(u.systemLog)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestProtectionGateAsProcesses(t *testing.T) {
	first, second := forcedControls(t)
	bin := protectTestBinary(t)
	const reason = "forced by the acceptance test"
	force := func(kv ...string) []string { return []string{"TENANT_TEST_FORCE_FAIL=" + strings.Join(kv, ";")} }
	forced := force(first + "=" + reason)
	terminal := append([]string{"TENANT_TEST_TERMINAL=1"}, forced...)
	const question = "Continue without it? [y/N]"

	// A one-shot command does not ask, and says what it is.
	u := newTenantUnderTest(t, bin)
	code, stdout, stderr := u.exits(t, forced, "", "version", "--protection")
	rep, err := reportedProtection([]byte(stdout))
	if code != 0 || err != nil || len(rep.Missing()) != 1 || rep.Missing()[0].ID != first || rep.Missing()[0].Reason != reason ||
		!strings.Contains(stdout, "TEST BUILD") || strings.Contains(stderr, question) {
		t.Fatalf("version --protection: exit %d, report %+v (%v)\nstdout:\n%s\nstderr:\n%s", code, rep, err, stdout, stderr)
	}
	leaves := rep.Missing()[0].Leaves

	// No terminal and nothing recorded: serve and the TUI exit non-zero with
	// the reason, in Tenant's log and the system's, before the config (which
	// would fail the start by itself) is read.
	if err := os.WriteFile(launchConfigPath(u.cfg), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, verb := range []string{"serve", "tui"} {
		code, _, stderr := u.exits(t, forced, "y\n", u.startArgs(verb)...)
		if code == 0 || strings.Contains(stderr, question) || strings.Contains(stderr, "config.json") {
			t.Fatalf("%s, no terminal: exit %d\n%s", verb, code, stderr)
		}
		book := logBookText(t, filepath.Join(u.data, "logs"))
		lines := u.systemLogLines(t)
		if len(lines) != i+1 {
			t.Fatalf("%s: the system's log has %d lines: %q", verb, len(lines), lines)
		}
		for _, want := range []string{first, reason, leaves} {
			if !strings.Contains(stderr, want) {
				t.Errorf("%s: stderr lacks %q:\n%s", verb, want, stderr)
			}
		}
		for _, want := range []string{verb, "did not start", first, reason, leaves} {
			if !strings.Contains(book, want) {
				t.Errorf("%s: Tenant's log lacks %q:\n%s", verb, want, book)
			}
			if !strings.Contains(lines[i], want) {
				t.Errorf("%s: the system's log lacks %q: %s", verb, want, lines[i])
			}
		}
	}
	if _, err := os.Stat(settingsPath(u.cfg, "main")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refusal wrote the settings file: %v", err)
	}

	// At a terminal, no: exits non-zero, records nothing.
	u = newTenantUnderTest(t, bin)
	code, _, stderr = u.exits(t, terminal, "n\n", u.startArgs("serve")...)
	if code == 0 || !strings.Contains(stderr, question) || !strings.Contains(stderr, reason) || !strings.Contains(stderr, leaves) {
		t.Fatalf("no at the terminal: exit %d\n%s", code, stderr)
	}
	if _, err := os.Stat(settingsPath(u.cfg, "main")); !errors.Is(err, os.ErrNotExist) || len(u.systemLogLines(t)) != 0 {
		t.Errorf("no at the terminal: settings %v, system log %q", err, u.systemLogLines(t))
	}
	// The end of the input is not a yes either.
	if code, _, _ := u.exits(t, terminal, "", u.startArgs("serve")...); code == 0 {
		t.Error("the end of the input counted as a yes")
	}

	// At a terminal, yes: serve comes up, and the yes is recorded with the
	// control, the reason and the version.
	warned := func() int {
		return strings.Count(logBookText(t, filepath.Join(u.data, "logs")), "Running without a process protection")
	}
	stdout, stderr = u.serves(t, terminal, "y\n", func() bool { return warned() == 1 })
	if !strings.Contains(stderr, question) || !strings.Contains(stdout, "process protection: running without") {
		t.Errorf("yes at the terminal: no question, or no warning in the feed:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	acks := savedAcks(t, u.cfg)
	if len(acks) != 1 || acks[0].Control != first || acks[0].Reason != reason || acks[0].Version == "" || acks[0].At.IsZero() {
		t.Fatalf("recorded %+v", acks)
	}
	if n := warned(); n != 1 {
		t.Errorf("the warning is in Tenant's log %d times after the first start, want 1", n)
	}

	// No terminal, acknowledged: it starts, unasked, and warns again.
	stdout, stderr = u.serves(t, forced, "", func() bool { return warned() == 2 })

	if strings.Contains(stderr, question) || !strings.Contains(stdout, "process protection: running without") || !strings.Contains(stdout, first) || !strings.Contains(stdout, reason) {
		t.Errorf("acknowledged, no terminal: asked again, or no warning in the feed:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	if n := warned(); n != 2 {
		t.Errorf("the warning is in Tenant's log %d times after the second start, want 2", n)
	}
	if n := len(u.systemLogLines(t)); n != 0 {
		t.Errorf("an acknowledged start wrote to the system's log: %q", u.systemLogLines(t))
	}

	// A different failure asks again, so with no terminal it exits with the
	// reason: another reason, another control, a new version.
	for name, tc := range map[string]struct {
		env  []string
		want string
	}{
		"another reason":  {force(first + "=another reason"), "another reason"},
		"another control": {force(first+"="+reason, second+"="+reason), second},
		"a new version":   {append([]string{"TENANT_TEST_VERSION=99.0.0"}, forced...), "99.0.0"},
	} {
		code, _, stderr := u.exits(t, tc.env, "", u.startArgs("serve")...)
		if code == 0 || !strings.Contains(stderr, "process protection is incomplete") || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: exit %d, want a refusal naming %q:\n%s", name, code, tc.want, stderr)
		}
	}
	if name := "another control"; true { // the one already acknowledged is not asked about again
		code, _, stderr := u.exits(t, append([]string{"TENANT_TEST_TERMINAL=1"}, force(first+"="+reason, second+"="+reason)...), "n\n", u.startArgs("serve")...)
		asked, _, _ := strings.Cut(stderr, question)
		if code == 0 || !strings.Contains(asked, second) || strings.Contains(asked, "("+first+")") {
			t.Errorf("%s at a terminal: exit %d, asked:\n%s", name, code, asked)
		}
	}
	if got := savedAcks(t, u.cfg); len(got) != 1 {
		t.Errorf("the refusals changed the record: %+v", got)
	}
}

// The tags that weaken process protection belong to no release build.
func TestReleaseBuildsCarryNoProtectionTags(t *testing.T) {
	for _, rel := range []string{".goreleaser.yaml", filepath.Join(".github", "workflows", "release.yml")} {
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, tag := range []string{"procprotecttest", "debugattach"} {
			if strings.Contains(string(b), tag) {
				t.Errorf("%s mentions the %s build tag: a release must not be built with it", rel, tag)
			}
		}
	}
}
