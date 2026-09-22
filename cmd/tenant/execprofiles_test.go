package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/sandbox"
)

// (a) A shell command runs and returns its output through sandboxRunner on the
// default host backend.
func TestSandboxRunnerRunsShellCommand(t *testing.T) {
	p := resolveExecProfile("os-exec", "", "", "")
	run := profileRunner(p, surfaceLocal, nil)
	out, err := run(context.Background(), "echo hello-sandbox", 5*time.Second)
	if err != nil {
		t.Fatalf("sandboxRunner returned error: %v", err)
	}
	if !strings.Contains(out, "hello-sandbox") {
		t.Fatalf("output %q does not contain the echoed text", out)
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
	wantExeDir := filepath.Dir(exe)

	for _, want := range []string{cfg, data, wantExeDir} {
		if !slices.Contains(p.Policy.DenyRoots, want) {
			t.Errorf("DenyRoots %v is missing %q", p.Policy.DenyRoots, want)
		}
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
