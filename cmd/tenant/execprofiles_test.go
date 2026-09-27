package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/capture"
	"tenant/internal/runtrace"
	"tenant/internal/sandbox"
)

// (a) A shell command runs and returns its output through sandboxRunner on the
// default host backend.
func TestSandboxRunnerRunsShellCommand(t *testing.T) {
	p := resolveExecProfile("os-exec", "", "", "")
	run := profileRunner(p, surfaceLocal, nil)
	// 30s, not 5s: this asserts the command runs and returns its output, not its
	// latency. On a loaded CI runner a cold PowerShell start (the host backend's
	// shell on Windows) can exceed a 5s budget and flake the assertion.
	out, err := run(context.Background(), "echo hello-sandbox", 30*time.Second)
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
	if _, err := profileRunner(execProfile{Backend: "container"}, surfaceDiscord, nil)(context.Background(), "true", time.Second); err == nil {
		t.Fatal("an unavailable backend ran the command")
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

// (d) resolveExecProfile defaults Backend to "host" for every surface, so
// routing a spawn site through the seam is a behavioural no-op.
func TestResolveExecProfileDefaultsHost(t *testing.T) {
	for _, surface := range []string{"os-exec", "cron-shell", "anything-else", ""} {
		p := resolveExecProfile(surface, "", "", "")
		if p.Backend != "host" {
			t.Errorf("surface %q: Backend=%q, want host", surface, p.Backend)
		}
	}
}

// The cron path (runSandboxShell) reproduces the legacy scrubbedEnv default via
// the seam: a plain var survives, a secret-named var is dropped, and output is
// trimmed. Covers deliverable (c) for the cron surface too.
func TestRunSandboxShellScrubsSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell $VAR expansion assumes sh; skipping on windows")
	}
	t.Setenv("CRON_PLAIN_VAR", "visible")
	t.Setenv("CRON_API_KEY", "topsecret")

	data := t.TempDir()
	p := resolveExecProfile("cron-shell", "", data, data)
	out, err := runSandboxShell(context.Background(),
		"echo P=$CRON_PLAIN_VAR K=$CRON_API_KEY", p)
	if err != nil {
		t.Fatalf("runSandboxShell returned error: %v", err)
	}
	if !strings.Contains(out, "P=visible") {
		t.Errorf("plain var not forwarded to cron shell; output=%q", out)
	}
	if strings.Contains(out, "topsecret") {
		t.Errorf("secret-named var leaked into cron shell env; output=%q", out)
	}
	// runSandboxShell trims combined output (host backend does not).
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
