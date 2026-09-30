//go:build windows

package sandbox

// Live tests for the Windows Job Object limits primitive (newJobObjectBackend),
// which enforces resource limits (job memory, per-process CPU time,
// active-process count, kill-on-close), so it is validated directly here. The
// sh-based host tests in sandbox_test.go skip on Windows and point at this file.
//
// Every test asserts a POSITIVE property the primitive's Probe claims. The
// filesystem and network dimensions are deliberately NOT asserted for
// containment: Probe reports them as EnforceNone (the Job Object does not confine
// them), and TestJobObject_ProbeHonest pins exactly that so a change cannot
// silently over-claim.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func jobRun(t *testing.T, shell string, pol Policy) Result {
	t.Helper()
	res, err := newJobObjectBackend().Run(context.Background(), Command{Shell: shell}, pol)
	if err != nil {
		t.Fatalf("infra error running %q: %v", shell, err)
	}
	return res
}

func TestJobObject_ProbeHonest(t *testing.T) {
	c := newJobObjectBackend().Probe(context.Background())
	if c.Isolation != IsolationProcess {
		t.Errorf("isolation = %v, want process", c.Isolation)
	}
	if c.Filesystem != EnforceNone {
		t.Errorf("filesystem = %v, want none (the Job Object does not confine the filesystem)", c.Filesystem)
	}
	if c.Network != EnforceNone {
		t.Errorf("network = %v, want none", c.Network)
	}
	if c.Limits != EnforceFull {
		t.Errorf("limits = %v, want enforced", c.Limits)
	}
	if c.Env != EnforceFull {
		t.Errorf("env = %v, want enforced", c.Env)
	}
	if c.Shell != "powershell" {
		t.Errorf("shell = %q, want powershell", c.Shell)
	}
}

func TestJobObject_BasicAndExit(t *testing.T) {
	res := jobRun(t, "Write-Output hello", Policy{})
	if strings.TrimSpace(res.Output) != "hello" {
		t.Fatalf("output = %q, want hello", res.Output)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", res.ExitCode)
	}
	if res = jobRun(t, "exit 3", Policy{}); res.ExitCode != 3 {
		t.Fatalf("exit = %d, want 3", res.ExitCode)
	}
}

func TestJobObject_WorkDir(t *testing.T) {
	dir := t.TempDir()
	// Prove the process ran with cwd == dir by a RELATIVE write landing there,
	// rather than string-comparing paths: on some hosts t.TempDir() is an 8.3
	// short name (C:\Users\RUNNER~1\…) while Get-Location resolves the long form
	// (…\runneradmin\…), so a substring match spuriously fails.
	res := jobRun(t, "Set-Content -LiteralPath wd_marker.txt -Value ok; Write-Output done", Policy{WorkDir: dir})
	if res.ExitCode != 0 {
		t.Fatalf("command failed: exit=%d out=%q", res.ExitCode, res.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "wd_marker.txt")); err != nil {
		t.Fatalf("work dir not honoured: relative write did not land in %q: %v (out=%q)", dir, err, res.Output)
	}
}

func TestJobObject_Timeout(t *testing.T) {
	res := jobRun(t, "Start-Sleep -Seconds 10", Policy{Limits: Limits{Wall: 400 * time.Millisecond}})
	if !res.TimedOut {
		t.Fatalf("expected TimedOut, got exit=%d output=%q", res.ExitCode, res.Output)
	}
}

func TestJobObject_OutputCap(t *testing.T) {
	res := jobRun(t, "1..50 | ForEach-Object { 'aaaaaaaaaa' }", Policy{Limits: Limits{MaxOutputBytes: 25}})
	if !strings.Contains(res.Output, "truncated") {
		t.Fatalf("expected truncation marker, got %q", res.Output)
	}
}

func TestJobObject_EnvAllowlist(t *testing.T) {
	t.Setenv("TENANT_SBX_SECRET_KEY", "leak-me")
	t.Setenv("TENANT_SBX_KEEP", "keep-me")
	pol := Policy{Env: EnvPolicy{
		Isolate: true,
		Pass:    []string{"TENANT_SBX_KEEP", "SystemRoot", "PATH"}, // SystemRoot/PATH so PowerShell starts
		Set:     map[string]string{"TENANT_SBX_ADDED": "added-value"},
	}}
	res := jobRun(t, "Write-Output \"keep=[$env:TENANT_SBX_KEEP] added=[$env:TENANT_SBX_ADDED] secret=[$env:TENANT_SBX_SECRET_KEY]\"", pol)
	out := strings.TrimSpace(res.Output)
	if !strings.Contains(out, "keep=[keep-me]") {
		t.Errorf("allowlisted var not forwarded: %q", out)
	}
	if !strings.Contains(out, "added=[added-value]") {
		t.Errorf("Set var not injected: %q", out)
	}
	if !strings.Contains(out, "secret=[]") {
		t.Errorf("un-listed secret var must be dropped: %q", out)
	}
}

func TestJobObject_MemoryCap(t *testing.T) {
	pol := Policy{Limits: Limits{Wall: 30 * time.Second, MemoryBytes: 300 << 20}}
	if res := jobRun(t, "Write-Output ok", pol); res.ExitCode != 0 || !strings.Contains(res.Output, "ok") {
		t.Fatalf("a trivial command must run under a 300MB cap: exit=%d out=%q", res.ExitCode, res.Output)
	}
	big := "try { $b = New-Object byte[] (2GB); [System.GC]::KeepAlive($b); Write-Output ALLOCATED } " +
		"catch { Write-Output ('refused:' + $_.Exception.GetType().Name); exit 1 }"
	res := jobRun(t, big, pol)
	if res.ExitCode == 0 || strings.Contains(res.Output, "ALLOCATED") {
		t.Fatalf("a 2GB allocation under a 300MB job-memory cap must fail: exit=%d out=%q", res.ExitCode, res.Output)
	}
}

func TestJobObject_CPUTimeCap(t *testing.T) {
	pol := Policy{Limits: Limits{Wall: 60 * time.Second, CPU: 2 * time.Second}}
	start := time.Now()
	res := jobRun(t, "while ($true) { $x = 1 }", pol)
	elapsed := time.Since(start)
	if res.TimedOut {
		t.Fatalf("the CPU cap should fire before the 60s wall deadline; got a wall timeout after %v", elapsed)
	}
	if res.ExitCode == 0 {
		t.Fatalf("a busy loop under a 2s CPU cap must be killed (non-zero exit); got exit 0 after %v", elapsed)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("CPU-capped loop ran %v, far past the 2s cap", elapsed)
	}
}

func TestJobObject_ActiveProcessCap(t *testing.T) {
	pol := Policy{Limits: Limits{Wall: 30 * time.Second, MaxPIDs: 2}}
	shell := "$fail = 0; 1..8 | ForEach-Object { " +
		"try { Start-Process -FilePath cmd.exe -ArgumentList '/c','exit' -WindowStyle Hidden -PassThru -ErrorAction Stop | Out-Null } " +
		"catch { $fail++ } }; Write-Output \"failed=$fail\""
	res := jobRun(t, shell, pol)
	if res.ExitCode != 0 {
		t.Fatalf("the spawn loop itself should exit 0: exit=%d out=%q", res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "failed=") || strings.Contains(res.Output, "failed=0") {
		t.Fatalf("with MaxPIDs=2, spawning 8 children must hit the active-process quota; got %q", strings.TrimSpace(res.Output))
	}
}

func TestJobObject_ReapsBackgroundChildOnClose(t *testing.T) {
	dir := t.TempDir()
	shell := "$p = Start-Process -FilePath ping.exe -ArgumentList '-n','30','127.0.0.1' -WindowStyle Hidden -PassThru; " +
		"$p.Id | Out-File -Encoding ascii -FilePath child.pid; Write-Output started"
	res := jobRun(t, shell, Policy{WorkDir: dir, Limits: Limits{Wall: 20 * time.Second}})

	raw, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatalf("child pid not recorded (out=%q): %v", res.Output, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("bad child pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(pid) {
		if p, e := os.FindProcess(pid); e == nil {
			_ = p.Kill()
		}
		t.Fatalf("background child %d outlived the run; kill-on-close did not reap it", pid)
	}
}

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259 // STILL_ACTIVE
	return code == stillActive
}
