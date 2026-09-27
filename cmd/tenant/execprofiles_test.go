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

// os_exec on the host backend is filed in the sandbox log (TEN-364): a start
// line with the command, scrubbed, and the limits, and an exit line with the
// pid, the exit code, the duration and the output's tail. The command sees
// the trace it runs under.
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
	out, err := run(ctx, "echo trace=$"+capture.EnvTrace+"; exit 3 # "+token, 30*time.Second)
	if err == nil || !strings.Contains(out, "trace="+trace) {
		t.Fatalf("out = %q, err = %v; want exit 3 and the trace", out, err)
	}
	all, _ := hub.Snapshot()
	var lines []string
	for _, e := range all {
		if e.Source != "sandbox" || e.Field("backend") != "host" || e.Field("surface") != surfaceLocal {
			continue
		}
		lines = append(lines, e.Line())
		switch e.Msg {
		case "process started":
			if cmd := e.Field("command"); strings.Contains(cmd, token) || !strings.Contains(cmd, "«secret:") || e.Field("limits") != "wall=30s" {
				t.Errorf("start line = %s", e.Line())
			}
		case "process exited: exit status 3":
			if e.Field("exit") != "3" || e.Field("pid") == "0" || e.Field("duration") == "" || e.Field("tail") != "trace="+trace || e.Level != slog.LevelWarn {
				t.Errorf("exit line = %s", e.Line())
			}
		default:
			t.Errorf("unexpected line %s", e.Line())
		}
	}
	if len(lines) != 2 {
		t.Errorf("want a start and an exit line, got %q", lines)
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
