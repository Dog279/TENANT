package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/capture"
	"tenant/internal/runtrace"
	"tenant/internal/sandbox"
	"tenant/internal/tui"
)

// (a) A shell command runs and returns its output through sandboxRunner on the
// default host backend.
func TestSandboxRunnerRunsShellCommand(t *testing.T) {
	p := resolveExecProfile("os-exec", "", "", "")
	run := profileRunner(p, surfaceLocal, nil)
	// This asserts the command runs and returns its output, not its latency,
	// so the deadline is only a hang guard. The host backend already starts
	// PowerShell with -NoProfile -NonInteractive and an empty stdin. On
	// Windows the command is a string, which PowerShell prints, not echo:
	// echo is Write-Output, whose first use autoloads its module, and on a
	// cold runner with many modules on PSModulePath that went past 5s, then
	// past 30s.
	cmd := "echo hello-sandbox"
	if runtime.GOOS == "windows" {
		cmd = "'hello-sandbox'"
	}
	out, err := run(context.Background(), cmd, 2*time.Minute)
	if err != nil {
		t.Fatalf("sandboxRunner returned error: %v", err)
	}
	if !strings.Contains(out, "hello-sandbox") {
		t.Fatalf("output %q does not contain the echoed text", out)
	}
}

// os_exec on the host backend is a sandbox Run activity (TEN-364, TEN-388):
// its start with the command, scrubbed, and the limits, related to the turn
// it runs in; its exit with the pid, the exit code and the output's tail;
// then its stop. The command sees the trace and the run's activity.
func TestHostRunIsFiledInTheSandboxLog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell $VAR expansion assumes sh; skipping on windows")
	}
	hub := logs.NewMemory(100)
	prev := logs.Default()
	logs.Install(hub)
	t.Cleanup(func() { logs.Install(prev) })
	trace := runtrace.NewID()
	ctx := runtrace.WithSpan(context.Background(), runtrace.NewSpan(trace))
	token := "ghp_" + strings.Repeat("a", 36)

	run := profileRunner(resolveExecProfile(surfaceLocal, "", "", ""), surfaceLocal, nil)
	out, err := run(ctx, "echo trace=$"+capture.EnvTrace+" act=$"+capture.EnvActivity+"; exit 3 # "+token, 30*time.Second)
	if err == nil || !strings.Contains(out, "trace="+trace) {
		t.Fatalf("out = %q, err = %v; want exit 3 and the trace", out, err)
	}
	all, _ := hub.Snapshot()
	var got []string
	var act string
	for _, e := range all {
		if e.Source != "sandbox" || e.Field("backend") != "host" || e.Field("surface") != surfaceLocal {
			continue
		}
		kind := e.Field("event") + " " + e.Field("opcode")
		got = append(got, kind)
		switch kind {
		case "sandbox/1 start":
			act = e.Field("activity")
			if cmd := e.Field("command"); strings.Contains(cmd, token) || !strings.Contains(cmd, "«secret:") || e.Field("limits") != "wall=30s" || e.Field("related") != trace {
				t.Errorf("start = %s", e.Line())
			}
		case "sandbox/1001 ":
			if e.Field("exit") != "3" || e.Field("pid") == "0" || e.Field("tail") != "trace="+trace+" act="+act || e.Level != slog.LevelInfo {
				t.Errorf("exit = %s", e.Line())
			}
		case "sandbox/1 stop":
			if e.Field("duration") == "" || e.Field("activity") != act {
				t.Errorf("stop = %s", e.Line())
			}
		}
	}
	if want := []string{"sandbox/1 start", "sandbox/1001 ", "sandbox/1 stop"}; !slices.Equal(got, want) || act == "" {
		t.Errorf("sandbox events = %q, want %q", got, want)
	}
}

// Each outcome of a refusal has its own event, carrying what was refused,
// and a backend that is unavailable is an error before anything runs.
func TestSandboxRefusalsAndUnavailableBackend(t *testing.T) {
	hub := logs.NewMemory(100)
	prev := logs.Default()
	logs.Install(hub)
	t.Cleanup(func() { logs.Install(prev) })
	fs := []sandbox.Violation{{Kind: "filesystem", Detail: "read denied: ~/.ssh"}}
	for _, c := range []struct {
		enforce bool
		res     sandbox.Result
	}{
		{true, sandbox.Result{ExitCode: 1, Violations: fs}},                      // refused
		{false, sandbox.Result{Violations: fs, Escalated: true}},                 // dry run
		{true, sandbox.Result{Violations: fs, Escalated: true}},                  // escalated
		{true, sandbox.Result{ExitCode: 1}},                                      // nothing refused
		{true, sandbox.Result{Violations: []sandbox.Violation{{Kind: "limit"}}}}, // a limit is not a refusal
	} {
		logSandboxRun(context.Background(), surfaceDiscord, "restricted", c.enforce, sandbox.Command{Shell: "cat ~/.ssh/id"}, c.res)
	}
	enforcing := execProfile{Backend: "container", Policy: sandbox.Policy{Enforce: true}}
	if _, err := profileRunner(enforcing, surfaceDiscord, nil)(context.Background(), "true", time.Second); err == nil {
		t.Fatal("an enforcing profile's unavailable backend ran the command")
	}
	all, _ := hub.Snapshot()
	var got []string
	for _, e := range all {
		got = append(got, e.Field("event"))
		if e.Field("event") != "sandbox/3001" && (e.Field("violations") != "filesystem: read denied: ~/.ssh" || e.Level != slog.LevelWarn) {
			t.Errorf("refusal = %s", e.Line())
		}
	}
	if want := []string{"sandbox/2001", "sandbox/2002", "sandbox/2003", "sandbox/3001"}; !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
}

// A dry run never blocks a command: with its backend unavailable, os_exec and a
// cron shell job run on the host, and the sandbox log says so (2005), where
// an enforcing profile fails closed (above).
func TestDryRunUnavailableBackendRunsOnHost(t *testing.T) {
	hub := logs.NewMemory(100)
	prev := logs.Default()
	logs.Install(hub)
	t.Cleanup(func() { logs.Install(prev) })
	dry := execProfile{Backend: "container", Policy: sandbox.Policy{WorkDir: t.TempDir()}}
	if out, err := profileRunner(dry, surfaceDiscord, nil)(context.Background(), "echo ran-on-host", 30*time.Second); err != nil || !strings.Contains(out, "ran-on-host") {
		t.Fatalf("os_exec in dry run: out=%q err=%v", out, err)
	}
	if out, err := runSandboxShell(context.Background(), "echo job-ran", dry); err != nil || !strings.Contains(out, "job-ran") {
		t.Fatalf("cron shell in dry run: out=%q err=%v", out, err)
	}
	all, _ := hub.Snapshot()
	n := 0
	for _, e := range all {
		if e.Field("event") == "sandbox/2005" && e.Field("backend") == "container" {
			n++
		}
		if e.Field("event") == "sandbox/1" && e.Field("backend") != "host" {
			t.Errorf("ran on %s, want the host: %s", e.Field("backend"), e.Line())
		}
	}
	if n != 2 {
		t.Errorf("want one sandbox/2005 per command, got %d", n)
	}
}

// (b) resolveExecProfile ALWAYS pins cfgDir, dataDir and the running
// executable's directory into DenyRoots (de-duplicated, non-empty).
func TestResolveExecProfileDenyRootsAlwaysPinned(t *testing.T) {
	cfg := t.TempDir()
	data := t.TempDir()
	p := resolveExecProfile("os-exec", cfg, data, "")

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	for _, want := range []string{cfg, data, exe} {
		if !slices.Contains(p.Policy.DenyRoots, want) {
			t.Errorf("DenyRoots %v is missing %q", p.Policy.DenyRoots, want)
		}
	}

	// The binary's DIRECTORY must not be denied: it may be shared (/usr/local/bin)
	// and denying it would break every tool installed beside Tenant.
	if slices.Contains(p.Policy.DenyRoots, filepath.Dir(exe)) {
		t.Errorf("DenyRoots %v must not include the executable's directory", p.Policy.DenyRoots)
	}

	// De-duplicated + non-empty: an empty cfgDir is dropped, and passing the
	// same dir twice does not repeat it.
	p2 := resolveExecProfile("os-exec", "", data, "")
	for _, r := range p2.Policy.DenyRoots {
		if r == "" {
			t.Errorf("DenyRoots %v contains an empty entry", p2.Policy.DenyRoots)
		}
	}
	if got := countOccurrences(p2.Policy.DenyRoots, data); got != 1 {
		t.Errorf("dataDir appears %d times in %v, want 1", got, p2.Policy.DenyRoots)
	}
}

// (c) A scoped env-allowlist Policy forwards ONLY the listed vars plus Set, and
// drops an unlisted secret var — proven by running a real echo through
// sandboxRunner on the host backend.
func TestSandboxRunnerEnvAllowlist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell $VAR expansion assumes sh; skipping on windows")
	}
	t.Setenv("SANDBOX_ALLOWED", "keep-me")
	t.Setenv("SANDBOX_SECRET_KEY", "leak-me")

	p := execProfile{
		Backend: "host",
		Policy: sandbox.Policy{
			Env: sandbox.EnvPolicy{
				Isolate: true,
				Pass:    []string{"SANDBOX_ALLOWED"},
				Set:     map[string]string{"SANDBOX_SET": "set-value"},
			},
		},
	}
	run := profileRunner(p, surfaceLocal, nil)
	out, err := run(context.Background(),
		"echo A=$SANDBOX_ALLOWED S=$SANDBOX_SET K=$SANDBOX_SECRET_KEY", 5*time.Second)
	if err != nil {
		t.Fatalf("sandboxRunner returned error: %v", err)
	}
	if !strings.Contains(out, "A=keep-me") {
		t.Errorf("allowlisted var not forwarded; output=%q", out)
	}
	if !strings.Contains(out, "S=set-value") {
		t.Errorf("Set var not injected; output=%q", out)
	}
	if strings.Contains(out, "leak-me") {
		t.Errorf("unlisted secret var leaked into the child env; output=%q", out)
	}
}

// withBuiltinExec makes the built-in exec backend backend (why: its fallback
// reason) for one test; TestMain pins the host.
func withBuiltinExec(t *testing.T, backend, why string) {
	t.Helper()
	prev := builtinExec
	builtinExec = func() (string, string) { return backend, why }
	t.Cleanup(func() { builtinExec = prev })
}

// TEN-347: with no exec_profiles, every surface runs the built-in backend in
// dry run, cron-shell with no network; where restricted can't dry-run, the
// host, with the reason carried to probe, audit and doctor.
func TestResolveExecProfile_BuiltinProfile(t *testing.T) {
	resetExecProfiles(t)
	for _, tc := range []struct{ backend, why string }{{"restricted", ""}, {"host", "no bubblewrap"}} {
		withBuiltinExec(t, tc.backend, tc.why)
		for _, surface := range execSurfaces {
			p := resolveExecProfile(surface, "", "", "")
			wantNet := sandbox.NetInherit
			if surface == surfaceCronShell {
				wantNet = sandbox.NetNone
			}
			if p.Backend != tc.backend || p.Fallback != tc.why || p.Policy.Enforce || p.Policy.Network != wantNet || !p.Policy.Env.Isolate {
				t.Errorf("%s with the %s built-in: %+v", surface, tc.backend, p)
			}
		}
	}
}

// exec_profiles.default.enforce is the one switch; an entry keeps what it sets,
// and one that names its backend keeps the defaults it was written under
// (enforce on, network inherit), so no existing profile is quietly downgraded
// to a dry run.
func TestResolveExecProfile_EnforceSwitch(t *testing.T) {
	resetExecProfiles(t)
	withBuiltinExec(t, "restricted", "")
	on, off := true, false
	for _, tc := range []struct {
		name     string
		profiles map[string]execProfileSetting
		surface  string
		enforce  bool
		network  sandbox.NetworkMode
	}{
		{"built-in", nil, surfaceDiscord, false, sandbox.NetInherit},
		{"switch on", map[string]execProfileSetting{"default": {Enforce: &on}}, surfaceDiscord, true, sandbox.NetInherit},
		{"switch on, cron-shell keeps no network", map[string]execProfileSetting{"default": {Enforce: &on}}, surfaceCronShell, true, sandbox.NetNone},
		{"switch reaches an entry without enforce or backend", map[string]execProfileSetting{
			"default": {Enforce: &on}, surfaceCronShell: {WriteRoots: []string{"/tmp/out"}}}, surfaceCronShell, true, sandbox.NetNone},
		{"an entry's own enforce wins", map[string]execProfileSetting{
			"default": {Enforce: &on}, surfaceDiscord: {Enforce: &off}}, surfaceDiscord, false, sandbox.NetInherit},
		{"a named backend enforces, network inherit", map[string]execProfileSetting{
			surfaceCronShell: {Backend: "restricted"}}, surfaceCronShell, true, sandbox.NetInherit},
		{"default naming a backend flips the switch", map[string]execProfileSetting{
			"default": {Backend: "restricted"}, surfaceLocal: {Network: "none"}}, surfaceLocal, true, sandbox.NetNone},
	} {
		defaultExecProfiles.Load(tc.profiles)
		p := resolveExecProfile(tc.surface, "", "", "")
		if p.Policy.Enforce != tc.enforce || p.Policy.Network != tc.network || p.Backend != "restricted" {
			t.Errorf("%s: enforce=%v network=%s backend=%s, want enforce=%v network=%s", tc.name,
				p.Policy.Enforce, p.Policy.Network, p.Backend, tc.enforce, tc.network)
		}
	}
}

// chooseBuiltinExec: restricted only where it can dry-run without breaking
// commands; the host, with an honest reason, everywhere else.
func TestChooseBuiltinExec(t *testing.T) {
	ok := func(res sandbox.Result, err error) func() (sandbox.Backend, error) {
		return func() (sandbox.Backend, error) { return fakeBackend{res: res, err: err}, nil }
	}
	missing := func() (sandbox.Backend, error) { return nil, errors.New("bubblewrap (bwrap) not on PATH") }
	noWSL := func() (sandbox.Backend, error) {
		return nil, errors.New("sandbox: restricted backend unavailable: WSL is not installed (wsl.exe not found)")
	}
	for _, tc := range []struct {
		name, goos string
		restricted func() (sandbox.Backend, error)
		want, why  string
	}{
		{"windows with a WSL sandbox", "windows", ok(sandbox.Result{}, nil), "restricted", ""},
		{"windows without WSL", "windows", noWSL, "host", "WSL is not installed"},
		{"windows, the sandbox can't run a command", "windows", ok(sandbox.Result{ExitCode: 1, Output: "bwrap: Unknown option --remount-ro"}, nil), "host", "--remount-ro"},
		{"linux without bubblewrap", "linux", missing, "host", "bwrap"},
		{"linux with bubblewrap", "linux", ok(sandbox.Result{}, nil), "host", "deny root"},
		{"macOS", "darwin", ok(sandbox.Result{}, nil), "restricted", ""},
		{"macOS, the test command fails", "darwin", ok(sandbox.Result{ExitCode: 71, Output: "sandbox_apply: Operation not permitted"}, nil), "host", "sandbox_apply"},
		{"macOS, the backend can't start", "darwin", ok(sandbox.Result{}, errors.New("no sandbox-exec")), "host", "no sandbox-exec"},
	} {
		got, why := chooseBuiltinExec(tc.goos, tc.restricted)
		if got != tc.want || !strings.Contains(why, tc.why) || (tc.why == "") != (why == "") {
			t.Errorf("%s: %s (%q), want %s (%q)", tc.name, got, why, tc.want, tc.why)
		}
	}
}

// Windows (owner decision 2026-09-29): the model is told the shell its
// commands really run in, and a command that ran elsewhere says so. Every
// path of the WSL sandbox (the run, a re-run outside it) is Linux sh; only the
// Windows host is PowerShell, and there cron keeps cmd.
func TestWindowsShellDecisions(t *testing.T) {
	missing := func(string) error { return errors.New("no WSL") }
	here := func(string) error { return nil }
	for _, tc := range []struct {
		name      string
		p         execProfile
		available func(string) error
		want      string
	}{
		{"the WSL sandbox", execProfile{Backend: "restricted"}, here, "wsl"},
		{"pinned to the host", execProfile{Backend: "host"}, here, "powershell"},
		{"dry run without WSL runs on the host", execProfile{Backend: "restricted"}, missing, "powershell"},
		{"enforcing without WSL still names WSL (its commands fail closed)", execProfile{Backend: "restricted", Policy: sandbox.Policy{Enforce: true}}, missing, "wsl"},
	} {
		if got := execShell("windows", runsOn(tc.p, tc.available)); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	if execShell("darwin", "restricted") != "sh" || execShell("linux", "host") != "sh" || execShell("windows", "wsl") != "wsl" {
		t.Error("sh off Windows; the WSL twin is WSL")
	}

	if osExecShellNote("powershell") != "" || osExecShellNote("sh") != "" {
		t.Error("the platform's own shell keeps the default description")
	}
	for _, must := range []string{"Linux sh", "not PowerShell", "/mnt/c", "Windows program", "outside"} {
		if !strings.Contains(osExecShellNote("wsl"), must) {
			t.Errorf("the WSL description must say %q: %s", must, osExecShellNote("wsl"))
		}
	}

	for _, tc := range []struct{ described, backend, want string }{
		{"wsl", "restricted", ""},
		{"wsl", "wsl", ""}, // the unsandboxed twin: same language
		{"wsl", "host", "[ran in PowerShell, on this Windows host]\n"},
		{"powershell", "restricted", "[ran in Linux sh, in the WSL sandbox]\n"},
		{"", "host", ""},
	} {
		if got := shellNote("windows", tc.described, tc.backend); got != tc.want {
			t.Errorf("described %q, ran on %s: %q, want %q", tc.described, tc.backend, got, tc.want)
		}
	}
	if shellNote("darwin", "sh", "host") != "" {
		t.Error("sh is sh off Windows")
	}
}

// A folder the WSL sandbox refused is saved as its Windows path, which is how
// profiles name folders; anything not on a drive is not saved.
func TestWindowsPathFromWSL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/mnt/c/Users/dylan/Desktop", `C:\Users\dylan\Desktop`},
		{"/mnt/d", `D:\`},
		{`\mnt\c\Tenant\data`, `C:\Tenant\data`}, // filepath.Clean's work on Windows
		{"/home/me/x", ""},
		{"/mnt/wsl/x", ""},
		{"/c/Users", ""}, // a custom automount root: not guessed
	} {
		got, ok := windowsPathFromWSL(tc.in)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%q: %q %v, want %q", tc.in, got, ok, tc.want)
		}
	}
}

// A result names where it ran when that is not the shell the os_exec
// description named (here: a surface pinned to the host while the tool
// describes the WSL sandbox).
func TestProfileRunner_NamesAnotherShell(t *testing.T) {
	prev, _ := describedShell.Load().(string)
	t.Cleanup(func() { describedShell.Store(prev) })
	describedShell.Store("wsl")
	out, err := profileRunner(execProfile{Backend: "host"}, surfaceLocal, nil)(context.Background(), "echo hi", 30*time.Second)
	if want := shellNote(runtime.GOOS, "wsl", "host"); err != nil || want == "" || !strings.HasPrefix(out, want) || !strings.Contains(out, "hi") {
		t.Fatalf("out=%q err=%v, want it to start with %q", out, err, want)
	}
	describedShell.Store(execShell(runtime.GOOS, "host"))
	if out, _ := profileRunner(execProfile{Backend: "host"}, surfaceLocal, nil)(context.Background(), "echo hi", 30*time.Second); strings.HasPrefix(out, "[ran in") {
		t.Fatalf("the described shell needs no note: %q", out)
	}
}

// Pinning cron-shell to the host restores cmd for its jobs, job by job the
// operator migrates; on the WSL sandbox every shell job is listed to rewrite.
func TestCronJobsInSh_AndPinning(t *testing.T) {
	resetExecProfiles(t)
	withBuiltinExec(t, "restricted", "")
	jobs := []cronJobConfig{
		{ID: "a1", Name: "backup", Kind: "shell", Prompt: `robocopy C:\data D:\bak /MIR`},
		{ID: "b2", Kind: "prompt", Prompt: "daily brief"},
		{ID: "c3", Kind: "shell", Prompt: "dir C:\\logs"},
	}
	p := resolveExecProfile(surfaceCronShell, "", "", "")
	got := cronJobsInSh("windows", runsOn(p, func(string) error { return nil }), jobs)
	if len(got) != 2 || got[0] != `backup: robocopy C:\data D:\bak /MIR` || got[1] != `c3: dir C:\logs` {
		t.Fatalf("jobs to rewrite = %q", got)
	}
	if n := cronShNotice(got); len(n) != 3 || !strings.Contains(n[0], `exec_profiles.cron-shell.backend "host"`) || n[1] != "  "+got[0] {
		t.Fatalf("notice = %q", n)
	}
	if !slices.Equal(cronShellArgvFor("windows", p.Backend, "dir"), []string{"sh", "-c", "dir"}) {
		t.Fatal("cron on the WSL sandbox runs sh")
	}

	defaultExecProfiles.Load(map[string]execProfileSetting{surfaceCronShell: {Backend: "host"}})
	p = resolveExecProfile(surfaceCronShell, "", "", "")
	if got := cronJobsInSh("windows", runsOn(p, nil), jobs); got != nil {
		t.Fatalf("pinned to the host, nothing to rewrite: %q", got)
	}
	if !slices.Equal(cronShellArgvFor("windows", p.Backend, "dir"), []string{"cmd", "/C", "dir"}) {
		t.Fatal("pinned to the host, cron keeps cmd /C")
	}
	if cronJobsInSh("darwin", "restricted", jobs) != nil || cronJobsInSh("linux", "host", jobs) != nil {
		t.Fatal("only Windows jobs change shell")
	}
}

type fakeBackend struct {
	res sandbox.Result
	err error
}

func (fakeBackend) Name() string                               { return "restricted" }
func (fakeBackend) Isolation() sandbox.Isolation               { return sandbox.IsolationProcess }
func (fakeBackend) Probe(context.Context) sandbox.Capabilities { return sandbox.Capabilities{} }
func (f fakeBackend) Run(context.Context, sandbox.Command, sandbox.Policy) (sandbox.Result, error) {
	return f.res, f.err
}

// The child environment is an allowlist on every backend (owner decision,
// TEN-345): the safe set and env_pass pass, by the name Tenant's environment
// has (any case on Windows); anything else, whatever its name, does not.
func TestEnvAllowlist(t *testing.T) {
	parent := []string{"PATH=/bin", "HOME=/h", "LC_ALL=C", "GITHUB_TOKEN=t", "DATABASE_URL=postgres://u:p@h/db",
		"HTTPS_PROXY=http://u:p@proxy", "SSH_AUTH_SOCK=/s", "MY_TOOL_HOME=/opt/tool", "=C:=C:\\x"}
	for _, tc := range []struct {
		goos   string
		parent []string
		pass   []string
		want   []string
	}{
		{"linux", parent, nil, []string{"PATH", "HOME", "LC_ALL"}},
		{"darwin", parent, []string{"MY_TOOL_HOME", "NOT_SET"}, []string{"PATH", "HOME", "LC_ALL", "MY_TOOL_HOME"}},
		{"windows", []string{"Path=C:\\w", "SystemRoot=C:\\Windows", "psmodulepath=x", "ComSpec=cmd", "OPENAI_API_KEY=k", "My_Var=v"},
			[]string{"MY_VAR"}, []string{"Path", "SystemRoot", "psmodulepath", "ComSpec", "My_Var"}},
		{"linux", []string{"Path=/bin"}, nil, nil}, // case-sensitive off Windows
	} {
		e := envAllowlistFrom(tc.goos, tc.parent, tc.pass, map[string]string{"X": "1"})
		if !e.Isolate || !slices.Equal(e.Pass, tc.want) || e.Set["X"] != "1" {
			t.Errorf("%s %v: %+v, want pass %v", tc.goos, tc.pass, e, tc.want)
		}
	}
}

// The cron path: a variable env_pass names reaches the job, anything else is
// dropped whatever its name, and output is trimmed.
func TestRunSandboxShell_EnvAllowlist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell $VAR expansion assumes sh; skipping on windows")
	}
	resetExecProfiles(t)
	t.Setenv("CRON_PLAIN_VAR", "visible")
	t.Setenv("CRON_OTHER_VAR", "dropped")
	t.Setenv("CRON_API_KEY", "topsecret")
	defaultExecProfiles.Load(map[string]execProfileSetting{surfaceCronShell: {EnvPass: []string{"CRON_PLAIN_VAR"}}})

	data := t.TempDir()
	out, err := runSandboxShell(context.Background(),
		"echo P=$CRON_PLAIN_VAR O=$CRON_OTHER_VAR K=$CRON_API_KEY PATH=${PATH:+set}", resolveExecProfile(surfaceCronShell, "", data, data))
	if err != nil {
		t.Fatalf("runSandboxShell returned error: %v", err)
	}
	if !strings.Contains(out, "P=visible") || !strings.Contains(out, "PATH=set") {
		t.Errorf("env_pass and the safe set must reach the job; output=%q", out)
	}
	if strings.Contains(out, "dropped") || strings.Contains(out, "topsecret") {
		t.Errorf("a variable no allowlist names reached the job; output=%q", out)
	}
	if out != strings.TrimSpace(out) {
		t.Errorf("output not trimmed: %q", out)
	}
}

func countOccurrences(ss []string, want string) int {
	n := 0
	for _, s := range ss {
		if s == want {
			n++
		}
	}
	return n
}

// Live, where a restricted backend runs (macOS): the built-in dry run blocks
// nothing. os_exec's write outside the roots is refused, re-run on the host and
// logged; a cron job's lookup with the network cut is too; and a cron job's ~
// is the real home, not the work dir. With the switch on, the same write
// asks, and denied, stays refused.
func TestBuiltinDryRunBlocksNothing(t *testing.T) {
	restrictedOrSkip(t)
	resetExecProfiles(t)
	withBuiltinExec(t, "restricted", "")
	hub := logs.NewMemory(200)
	prev := logs.Default()
	logs.Install(hub)
	t.Cleanup(func() { logs.Install(prev) })

	out := filepath.Join(t.TempDir(), "made-on-host.txt")
	never := func(context.Context, string, string) bool { t.Fatal("a dry run must not ask"); return false }
	res, err := sandboxRunner("", "", never)(context.Background(), "echo hi > "+out, 30*time.Second)
	if err != nil || !strings.Contains(res, "sandbox dry-run") {
		t.Fatalf("os_exec: out=%q err=%v", res, err)
	}
	if b, err := os.ReadFile(out); err != nil || strings.TrimSpace(string(b)) != "hi" {
		t.Fatalf("the host re-run must have written the file: %q %v", b, err)
	}

	data := t.TempDir()
	p := resolveExecProfile(surfaceCronShell, "", data, cronSandboxDir(data)) // as runCronShell resolves it
	if p.Policy.Network != sandbox.NetNone {
		t.Fatalf("cron-shell network = %s, want none", p.Policy.Network)
	}
	if res, _ := runSandboxShell(context.Background(), "curl -sS -m 5 http://tenant-sandbox-probe.invalid/", p); !strings.Contains(res, "sandbox dry-run") {
		t.Fatalf("a cut lookup must re-run on the host in a dry run: %q", res)
	}
	home, _ := os.UserHomeDir()
	if res, err := runSandboxShell(context.Background(), "cd ~ && pwd -P", p); err != nil || res != mustEval(t, home) {
		t.Fatalf("a cron job's ~ must be the real home in a dry run: %q %v", res, err)
	}
	all, _ := hub.Snapshot()
	would := 0
	for _, e := range all {
		if e.Field("event") == "sandbox/2002" {
			would++
		}
	}
	if would != 2 {
		t.Errorf("want 2 would-refuse events (os_exec write, cron lookup), got %d", would)
	}

	on := true
	defaultExecProfiles.Load(map[string]execProfileSetting{"default": {Enforce: &on}})
	asked := false
	deny := func(context.Context, string, string) bool { asked = true; return false }
	blocked := filepath.Join(t.TempDir(), "never.txt")
	if _, err := sandboxRunner("", "", deny)(context.Background(), "echo hi > "+blocked, 30*time.Second); err == nil || !asked {
		t.Fatalf("enforcing, the write must ask and, denied, fail: err=%v asked=%v", err, asked)
	}
	if _, err := os.Stat(blocked); !os.IsNotExist(err) {
		t.Fatalf("a denied escalation wrote the file: %v", err)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// --- exceptions: approve-always on an escalation (TEN-347) ---

// refusedPath finds the absolute path a refusal names, in the forms the tools
// print it on macOS (EPERM) and Linux (EROFS); nothing when none is absolute.
func TestRefusedPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	for _, tc := range []struct{ out, want string }{
		{"touch: /Users/me/Desktop/x.txt: Operation not permitted", "/Users/me/Desktop/x.txt"},
		{"sh: /Users/me/My Docs/out.log: Operation not permitted", "/Users/me/My Docs/out.log"},
		{"PermissionError: [Errno 1] Operation not permitted: '/srv/app/cache.db'", "/srv/app/cache.db"},
		{"Error: EPERM: operation not permitted, open '/srv/app/x.json'", "/srv/app/x.json"},
		{"fatal: Unable to create '/w/repo/.git/index.lock': Operation not permitted", "/w/repo/.git/index.lock"},
		{"touch: cannot touch '/home/u/notes.md': Read-only file system", "/home/u/notes.md"},
		{"sh: 1: cannot create /home/u/out.txt: Read-only file system", "/home/u/out.txt"},
		{"some noise\nmkdir: /opt/build: Operation not permitted\n", "/opt/build"},
		{"fatal: could not create work tree dir 'repo': Operation not permitted", ""}, // relative: never guessed
		{"cat: /etc/x: No such file or directory", ""},                                // not a refusal
	} {
		if got := refusedPath(tc.out); got != tc.want {
			t.Errorf("refusedPath(%q) = %q, want %q", tc.out, got, tc.want)
		}
	}
}

// exceptionScope opens only a folder: the refused one, or a file's. Never a
// deny root, the home folder or above it, a network refusal, or a guess.
func TestExceptionScope(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	work := t.TempDir()
	real := mustEval(t, work) // what is saved: the folder's real path (/var → /private/var on macOS)
	deny := t.TempDir()
	fs := func(out string) sandbox.Result {
		return sandbox.Result{Output: out, Violations: []sandbox.Violation{{Kind: "filesystem", Detail: "x"}}}
	}
	link := filepath.Join(t.TempDir(), "via-link")
	if err := os.Symlink(work, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	const system = "system folder, a drive or volume root"
	for _, tc := range []struct {
		name      string
		res       sandbox.Result
		scope, no string
	}{
		{"a file's folder", fs("touch: " + filepath.Join(work, "new.txt") + ": Operation not permitted"), real, ""},
		{"a folder itself", fs("mkdir: " + work + ": Operation not permitted"), real, ""},
		{"through a symlink: its real path", fs("touch: " + filepath.Join(link, "x") + ": Operation not permitted"), real, ""},
		{"a folder not there yet", fs("mkdir: " + filepath.Join(work, "a", "b") + ": Operation not permitted"), "", "does not exist yet"},
		{"inside a deny root", fs("cat: " + filepath.Join(deny, "k") + ": Operation not permitted"), "", "no exception opens"},
		{"a dotfile in home", fs("sh: " + filepath.Join(home, ".zshrc") + ": Operation not permitted"), "", "whole home folder"},
		{"network", sandbox.Result{Violations: []sandbox.Violation{{Kind: "network"}}}, "", "network"},
		{"a Windows program", sandbox.Result{Violations: []sandbox.Violation{{Kind: "program", Detail: sandbox.WindowsProgramRefusal}}}, "", "only ever runs outside"},
		{"no path", fs("Operation not permitted"), "", "no absolute path"},
		// Crafted by the command, which writes its own output (review P3-1):
		// system folders and roots are refused, never offered.
		{"the root", fs("touch: /x: Operation not permitted"), "", system},
		{"a file in /etc", fs("sh: /etc/hosts: Operation not permitted"), "", system},
		{"a file in /tmp", fs("touch: '/tmp/x': Read-only file system"), "", system},
		{"/usr itself", fs("mkdir: /usr: Operation not permitted"), "", system},
		{"/Users, above every home", fs(`PermissionError: [Errno 1] Operation not permitted: '/Users/x.txt'`), "", ""},
		{"/dev", fs("sh: 1: cannot create /dev/evil: Read-only file system"), "", system},
	} {
		scope, why := exceptionScope(tc.res, []string{deny})
		if scope != tc.scope || !strings.Contains(why, tc.no) {
			t.Errorf("%s: scope %q (%q), want %q (%q)", tc.name, scope, why, tc.scope, tc.no)
		}
	}
}

// systemScope: roots, volume mounts and system folders, and anything above
// them, are never an exception; folders under them are. Every OS's rules,
// tested on any (review P3-1).
func TestSystemScope(t *testing.T) {
	macLinks := map[string]string{"/etc": "/private/etc", "/tmp": "/private/tmp", "/var": "/private/var", "/sneaky": "/usr"}
	resolve := func(links map[string]string) func(string) string {
		return func(p string) string {
			if r, ok := links[p]; ok {
				return r
			}
			return p
		}
	}
	for _, tc := range []struct {
		goos, dir string
		want      bool
	}{
		{"linux", "/", true},
		{"linux", "/etc", true},
		{"linux", "/home", true},
		{"linux", "/srv", true},
		{"linux", "/run", true},
		{"linux", "/mnt/c", true}, // a bare drive mount
		{"linux", "/home/me/proj", false},
		{"linux", "/opt/app/data", false},
		{"linux", "/var/lib/app", false},
		{"linux", "/mnt/c/Users/me/work", false},
		{"linux", "/Etc", false},         // Linux names are case-sensitive: not /etc
		{"darwin", "/private", true},     // above /private/etc
		{"darwin", "/private/etc", true}, // /etc, resolved
		{"darwin", "/sneaky", true},      // a link to /usr
		{"darwin", "/USR", true},         // case-folded
		{"darwin", "/Applications", true},
		{"darwin", "/Volumes/Backup", true}, // a volume's mount
		{"darwin", "/System/Volumes/Data/Users", true},
		{"darwin", "/Users/me/Projects", false},
		{"darwin", "/private/var/folders/xy/T/job", false},
		{"darwin", "/Volumes/Backup/jobs", false},
		{"windows", `C:\`, true},
		{"windows", `D:\`, true},
		{"windows", `c:\windows`, true},
		{"windows", `C:\Program Files`, true},
		{"windows", `C:\Program Files (x86)`, true},
		{"windows", `C:\ProgramData`, true},
		{"windows", `C:\Users`, true},
		{"windows", `C:\Program Files\App`, false},
		{"windows", `C:\Users\me\Desktop`, false},
		{"windows", `D:\projects\x`, false},
	} {
		if got := systemScope(tc.goos, tc.dir, resolve(macLinks)); got != tc.want {
			t.Errorf("%s %q: %v, want %v", tc.goos, tc.dir, got, tc.want)
		}
	}
}

// An exception is a write root only while it is still the folder it was saved
// as: swapped for a symlink (here to the home folder) or gone, it is left out
// and said once in the sandbox log (review P3-2).
func TestLiveExceptions_SymlinkSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	resetExecProfiles(t)
	t.Cleanup(func() { defaultExecProfiles.LoadExceptions(nil) })
	hub := logs.NewMemory(50)
	prev := logs.Default()
	logs.Install(hub)
	t.Cleanup(func() { logs.Install(prev) })

	folder := filepath.Join(mustEval(t, t.TempDir()), "reports")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	defaultExecProfiles.LoadExceptions(map[string][]string{surfaceCronShell: {folder}})
	if got := liveExceptions(surfaceCronShell); !slices.Equal(got, []string{folder}) {
		t.Fatalf("the saved folder must be live: %v", got)
	}

	home := mustEval(t, t.TempDir()) // stands in for $HOME
	if err := os.Remove(folder); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(home, folder); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for i := 0; i < 2; i++ {
		if p := resolveExecProfile(surfaceCronShell, "", "", ""); slices.Contains(p.Policy.WriteRoots, folder) || slices.Contains(p.Policy.WriteRoots, home) {
			t.Fatalf("a swapped-in symlink must not become a write root: %v", p.Policy.WriteRoots)
		}
	}
	os.Remove(folder)
	if got := liveExceptions(surfaceCronShell); len(got) != 0 {
		t.Fatalf("a folder that is gone is not live: %v", got)
	}
	all, _ := hub.Snapshot()
	var whys []string
	for _, e := range all {
		if e.Field("event") == "sandbox/2006" {
			whys = append(whys, e.Field("why"))
		}
	}
	if len(whys) != 2 || !strings.Contains(whys[0], "now resolves to "+home) || !strings.Contains(whys[1], "no longer exists") {
		t.Fatalf("want one warning per drift, once each: %q", whys)
	}
}

func TestExceptionDrift(t *testing.T) {
	dirInfo, _ := os.Stat(t.TempDir())
	fileInfo, _ := os.Stat(os.Args[0])
	for _, tc := range []struct {
		goos, p, resolved string
		err               error
		info              os.FileInfo
		want              string
	}{
		{"linux", "/srv/x", "/srv/x", nil, dirInfo, ""},
		{"linux", "/srv/x", "/home/me", nil, dirInfo, "now resolves to /home/me"},
		{"linux", "/srv/x", "", os.ErrNotExist, nil, "no longer exists"},
		{"linux", "/srv/x", "/srv/x", nil, fileInfo, "no longer a folder"},
		{"windows", `c:\tenant\out`, `C:\Tenant\out`, nil, dirInfo, ""}, // case only: the same folder
		{"linux", "/srv/X", "/srv/x", nil, dirInfo, "now resolves to"},
	} {
		eval := func(string) (string, error) { return tc.resolved, tc.err }
		stat := func(string) (os.FileInfo, error) { return tc.info, nil }
		if got := exceptionDrift(tc.goos, tc.p, eval, stat); !strings.Contains(got, tc.want) || (tc.want == "") != (got == "") {
			t.Errorf("%s %q → %q: %q, want %q", tc.goos, tc.p, tc.resolved, got, tc.want)
		}
	}
}

// Exceptions load per surface, validated, and join only that surface's write
// roots, whichever profile it resolves to.
func TestExecExceptions_LoadAndResolve(t *testing.T) {
	resetExecProfiles(t)
	t.Cleanup(func() { defaultExecProfiles.LoadExceptions(nil) })
	dir := mustEval(t, t.TempDir()) // saved as its real path; a path through a symlink is not used
	n, skipped := defaultExecProfiles.LoadExceptions(map[string][]string{
		surfaceDiscord: {dir, "relative/x"},
		"bogus":        {dir},
	})
	if n != 1 || len(skipped) != 2 {
		t.Fatalf("applied %d, skipped %v; want 1 applied, 2 skipped", n, skipped)
	}
	other := filepath.Join(t.TempDir(), "w")
	defaultExecProfiles.Load(map[string]execProfileSetting{"default": {WriteRoots: []string{other}}})
	if p := resolveExecProfile(surfaceDiscord, "", "", ""); !slices.Equal(p.Policy.WriteRoots, []string{other, dir}) {
		t.Errorf("discord write roots = %v, want the profile's then the exception", p.Policy.WriteRoots)
	}
	if p := resolveExecProfile(surfaceLocal, "", "", ""); slices.Contains(p.Policy.WriteRoots, dir) {
		t.Errorf("an exception for discord leaked to local: %v", p.Policy.WriteRoots)
	}
}

// Approve "always" on an escalation saves the refused folder for that surface,
// and never turns the sandbox category to allow; with nothing scoped to save,
// it approves the one time and saves nothing.
func TestApproveAlwaysOnEscalationSavesScopedException(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	resetExecProfiles(t)
	t.Cleanup(func() { defaultExecProfiles.LoadExceptions(nil); persistExecException = nil })
	cfg := t.TempDir()
	stg := &settings{Tools: map[string]bool{}}
	var mu sync.Mutex
	loadExecSettings(&commonFlags{cfgDir: cfg, agent: "main"}, stg, &mu, func(string) {})

	b := newApprovalBroker(nil)
	b.SetRules(nil)
	b.SetAsk(func(context.Context, tui.ApprovalRequest) tui.ApprovalDecision { return tui.ApproveAlways })
	work := mustEval(t, t.TempDir()) // "always" saves the real path
	refused := sandbox.Result{ExitCode: 1, Output: "touch: " + filepath.Join(work, "a.txt") + ": Operation not permitted",
		Violations: []sandbox.Violation{{Kind: "filesystem", Detail: "x"}}}
	var detail string
	confirm := func(ctx context.Context, action, d string) bool { detail = d; return b.Confirm(ctx, action, d) }
	esc := execEscalator(confirm, sandbox.Policy{})
	if !esc(context.Background(), sandbox.Command{Shell: "touch a.txt", Surface: surfaceDiscord}, refused) {
		t.Fatal("always approves this run")
	}
	if !strings.Contains(detail, "lets discord commands write under "+work) {
		t.Errorf("the prompt must say what always saves: %q", detail)
	}
	if got := b.Modes()[catSandbox]; got != modeAsk.String() {
		t.Fatalf("always on an escalation opened the sandbox category: %s", got)
	}
	saved, err := loadSettings(cfg, "main")
	if err != nil || !slices.Equal(saved.ExecExceptions[surfaceDiscord], []string{work}) {
		t.Fatalf("exec_exceptions on disk = %v (err %v), want discord: [%s]", saved.ExecExceptions, err, work)
	}
	if p := resolveExecProfile(surfaceDiscord, "", "", ""); !slices.Contains(p.Policy.WriteRoots, work) {
		t.Errorf("the exception is not live: %v", p.Policy.WriteRoots)
	}

	// A network refusal has no folder: always approves once, saves nothing.
	net := sandbox.Result{ExitCode: 6, Output: "curl: (6) Could not resolve host: x", Violations: []sandbox.Violation{{Kind: "network"}}}
	if !esc(context.Background(), sandbox.Command{Shell: "curl x", Surface: surfaceLocal}, net) {
		t.Fatal("always approves this run")
	}
	if !strings.Contains(detail, "approves this time only") {
		t.Errorf("the prompt must say always saves nothing here: %q", detail)
	}
	if saved, _ := loadSettings(cfg, "main"); len(saved.ExecExceptions) != 1 || b.Modes()[catSandbox] != modeAsk.String() {
		t.Errorf("a network refusal saved something: %v, sandbox %s", saved.ExecExceptions, b.Modes()[catSandbox])
	}
}

// Only the escalation's own prompt saves an exception: "always" on another
// action carrying the same context value is that action's own grant, and the
// hook without an escalation's context saves nothing.
func TestExecException_OnlyFromTheEscalation(t *testing.T) {
	t.Cleanup(func() { persistExecException = nil })
	var calls int
	persistExecException = func(string, string) error { calls++; return nil }
	ctx := context.WithValue(context.Background(), execExceptionKey{}, execException{surface: surfaceLocal, path: "/tmp"})
	b := newApprovalBroker(nil)
	b.SetRules(nil)
	b.SetAsk(func(context.Context, tui.ApprovalRequest) tui.ApprovalDecision { return tui.ApproveAlways })
	if !b.Confirm(ctx, "os_exec", "ls") || calls != 0 {
		t.Fatalf("always on os_exec saved an exception (calls=%d)", calls)
	}
	saveExecException(context.Background())
	if calls != 0 {
		t.Fatal("the hook saved an exception with no escalation in its context")
	}
}

// --- the brief banner (TEN-347) ---

func TestWithSandboxBanner(t *testing.T) {
	if got := withSandboxBanner("all good", 0); got != "all good" {
		t.Errorf("no violations must leave the summary alone: %q", got)
	}
	if got := withSandboxBanner("brief", 3); !strings.HasPrefix(got, "🛡 SANDBOX: 3 sandbox violation(s) since the last run") || !strings.HasSuffix(got, "\n\nbrief") {
		t.Errorf("banner = %q", got)
	}
}

// The banner's count is the sandbox log's refusals since the job last ran,
// read from the log book.
func TestSandboxViolationsSince(t *testing.T) {
	keepLogDefaults(t)
	hub := openBootTestHub(t, t.TempDir())
	logs.Install(hub)
	ctx := context.Background()
	kv := []any{"surface", surfaceCronShell, "backend", "restricted", "command", "x", "exit", 1, "violations", "filesystem: x"}
	sandbox.Refused.Log(ctx, kv...)
	sandbox.WouldRefuse.Log(ctx, kv...)
	sandbox.Escalated.Log(ctx, kv...)
	sandbox.ExceptionSaved.Log(ctx, "surface", surfaceLocal, "path", "/tmp") // not a violation
	if err := hub.Flush(); err != nil {
		t.Fatal(err)
	}
	count := sandboxViolationsSince(hub)
	if n := count(time.Now().Add(-time.Hour)); n != 3 {
		t.Errorf("violations in the last hour = %d, want 3", n)
	}
	if n := count(time.Now().Add(time.Hour)); n != 0 {
		t.Errorf("violations since a later run = %d, want 0", n)
	}
	if n := sandboxViolationsSince(logs.NewMemory(10))(time.Time{}); n != 0 {
		t.Errorf("no store counts nothing, got %d", n)
	}
}
