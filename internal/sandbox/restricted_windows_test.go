//go:build windows

package sandbox_test

// Live tests for the Windows restricted backend (the Job Object). The sh-based
// host tests in sandbox_test.go skip on Windows and point here; this file is
// their Windows-native counterpart. Every test asserts a POSITIVE property the
// backend's Probe claims to enforce — resource limits (job memory, per-process
// CPU time, active-process count), kill-on-close reaping, and the environment
// allowlist — plus basic run parity (exit code, output, work dir, timeout,
// output cap). The filesystem and network dimensions are deliberately NOT
// tested for containment here: Probe reports them as EnforceNone until the
// account/ACE/WFP work lands (TEN-346), and TestWindowsProbe_HonestClaims pins
// exactly that so the suite fails loudly if a future change over-claims.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"tenant/internal/sandbox"
)

// winRestricted returns the Windows restricted backend (always available on
// Windows) and fails the test if construction ever errors.
func winRestricted(t *testing.T) sandbox.Backend {
	t.Helper()
	b, err := sandbox.Restricted()
	if err != nil {
		t.Fatalf("sandbox.Restricted() on windows: %v", err)
	}
	return b
}

// winRun runs one PowerShell command under pol and returns the Result, failing
// only on an infrastructure error (a non-zero exit is signal, not an error).
func winRun(t *testing.T, shell string, pol sandbox.Policy) sandbox.Result {
	t.Helper()
	res, err := winRestricted(t).Run(context.Background(), sandbox.Command{Shell: shell}, pol)
	if err != nil {
		t.Fatalf("infra error running %q: %v", shell, err)
	}
	return res
}

// TestWindowsProbe_HonestClaims pins the backend's advertised enforcement so a
// change can never silently over-claim. The Job Object enforces limits and the
// env allowlist; it does NOT yet confine the filesystem or network.
func TestWindowsProbe_HonestClaims(t *testing.T) {
	c := winRestricted(t).Probe(context.Background())
	if c.Isolation != sandbox.IsolationProcess {
		t.Errorf("isolation = %v, want process", c.Isolation)
	}
	if c.Filesystem != sandbox.EnforceNone {
		t.Errorf("filesystem = %v, want none (no ACEs yet, TEN-346)", c.Filesystem)
	}
	if c.Network != sandbox.EnforceNone {
		t.Errorf("network = %v, want none (no WFP filter yet, TEN-346)", c.Network)
	}
	if c.Limits != sandbox.EnforceFull {
		t.Errorf("limits = %v, want enforced (Job Object)", c.Limits)
	}
	if c.Env != sandbox.EnforceFull {
		t.Errorf("env = %v, want enforced (allowlist)", c.Env)
	}
}

func TestWindowsRun_BasicAndExit(t *testing.T) {
	res := winRun(t, "Write-Output hello", sandbox.Policy{})
	if strings.TrimSpace(res.Output) != "hello" {
		t.Fatalf("output = %q, want hello", res.Output)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", res.ExitCode)
	}
	res = winRun(t, "exit 3", sandbox.Policy{})
	if res.ExitCode != 3 {
		t.Fatalf("exit = %d, want 3", res.ExitCode)
	}
}

func TestWindowsRun_WorkDir(t *testing.T) {
	dir := t.TempDir()
	res := winRun(t, "(Get-Location).Path", sandbox.Policy{WorkDir: dir})
	if !strings.Contains(strings.ToLower(res.Output), strings.ToLower(dir)) {
		t.Fatalf("Get-Location = %q, want work dir %q", strings.TrimSpace(res.Output), dir)
	}
}

func TestWindowsRun_Timeout(t *testing.T) {
	res := winRun(t, "Start-Sleep -Seconds 10", sandbox.Policy{Limits: sandbox.Limits{Wall: 400 * time.Millisecond}})
	if !res.TimedOut {
		t.Fatalf("expected TimedOut, got exit=%d output=%q", res.ExitCode, res.Output)
	}
}

func TestWindowsRun_OutputCap(t *testing.T) {
	res := winRun(t, "1..50 | ForEach-Object { 'aaaaaaaaaa' }", sandbox.Policy{Limits: sandbox.Limits{MaxOutputBytes: 25}})
	if !strings.Contains(res.Output, "truncated") {
		t.Fatalf("expected truncation marker, got %q", res.Output)
	}
}

// TestWindowsEnv_Allowlist proves the environment allowlist holds on Windows: a
// forwarded name survives, an injected Set value appears, and a variable whose
// name looks like a secret but is NOT on the allowlist is dropped.
func TestWindowsEnv_Allowlist(t *testing.T) {
	t.Setenv("TENANT_SBX_SECRET_KEY", "leak-me")
	t.Setenv("TENANT_SBX_KEEP", "keep-me")
	pol := sandbox.Policy{Env: sandbox.EnvPolicy{
		Isolate: true,
		Pass:    []string{"TENANT_SBX_KEEP", "SystemRoot", "PATH"}, // SystemRoot/PATH so PowerShell starts
		Set:     map[string]string{"TENANT_SBX_ADDED": "added-value"},
	}}
	res := winRun(t, "Write-Output \"keep=[$env:TENANT_SBX_KEEP] added=[$env:TENANT_SBX_ADDED] secret=[$env:TENANT_SBX_SECRET_KEY]\"", pol)
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

// TestWindowsLimits_JobMemory proves the job-memory cap is live: a large
// allocation under the cap is refused, while a trivial command under the SAME
// cap runs cleanly (so the failure is the cap, not a broken shell).
func TestWindowsLimits_JobMemory(t *testing.T) {
	pol := sandbox.Policy{Limits: sandbox.Limits{Wall: 30 * time.Second, MemoryBytes: 300 << 20}}

	if res := winRun(t, "Write-Output ok", pol); res.ExitCode != 0 || !strings.Contains(res.Output, "ok") {
		t.Fatalf("a trivial command must run under a 300MB cap: exit=%d out=%q", res.ExitCode, res.Output)
	}

	big := "try { $b = New-Object byte[] (2GB); [System.GC]::KeepAlive($b); Write-Output ALLOCATED } " +
		"catch { Write-Output ('refused:' + $_.Exception.GetType().Name); exit 1 }"
	res := winRun(t, big, pol)
	if res.ExitCode == 0 || strings.Contains(res.Output, "ALLOCATED") {
		t.Fatalf("a 2GB allocation under a 300MB job-memory cap must fail: exit=%d out=%q", res.ExitCode, res.Output)
	}
}

// TestWindowsLimits_CPUTime proves the per-process CPU-time cap is live: a busy
// loop under a small CPU cap is killed by the cap (non-zero exit) BEFORE the far
// larger wall-clock deadline, so the kill is the CPU limit, not the timeout.
func TestWindowsLimits_CPUTime(t *testing.T) {
	pol := sandbox.Policy{Limits: sandbox.Limits{Wall: 60 * time.Second, CPU: 2 * time.Second}}
	start := time.Now()
	res := winRun(t, "while ($true) { $x = 1 }", pol)
	elapsed := time.Since(start)
	if res.TimedOut {
		t.Fatalf("the CPU cap should fire before the 60s wall deadline; got a wall timeout after %v", elapsed)
	}
	if res.ExitCode == 0 {
		t.Fatalf("a busy loop under a 2s CPU cap must be killed (non-zero exit); got exit 0 after %v", elapsed)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("CPU-capped loop ran %v, far past the 2s cap — cap likely not enforced", elapsed)
	}
}

// TestWindowsLimits_ActiveProcess proves the active-process cap is live: with a
// low cap, an attempt to spawn several child processes hits the quota, so not
// all of them start.
func TestWindowsLimits_ActiveProcess(t *testing.T) {
	pol := sandbox.Policy{Limits: sandbox.Limits{Wall: 30 * time.Second, MaxPIDs: 2}}
	shell := "$fail = 0; 1..8 | ForEach-Object { " +
		"try { Start-Process -FilePath cmd.exe -ArgumentList '/c','exit' -WindowStyle Hidden -PassThru -ErrorAction Stop | Out-Null } " +
		"catch { $fail++ } }; Write-Output \"failed=$fail\""
	res := winRun(t, shell, pol)
	if res.ExitCode != 0 {
		t.Fatalf("the spawn loop itself should exit 0: exit=%d out=%q", res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "failed=") {
		t.Fatalf("unexpected output: %q", res.Output)
	}
	if strings.Contains(res.Output, "failed=0") {
		t.Fatalf("with MaxPIDs=2, spawning 8 children must hit the active-process quota; got %q", strings.TrimSpace(res.Output))
	}
}

// TestWindowsRestricted_ReapsBackgroundChildOnClose proves kill-on-close: a
// background child a command leaves running must not outlive the run. When the
// backend closes the Job Object handle, JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE tears
// the whole tree down.
func TestWindowsRestricted_ReapsBackgroundChildOnClose(t *testing.T) {
	dir := t.TempDir()
	// Start a long-running child detached from our output pipe (Hidden), record
	// its PID, and return immediately. The child would run for ~30s on its own.
	shell := "$p = Start-Process -FilePath ping.exe -ArgumentList '-n','30','127.0.0.1' -WindowStyle Hidden -PassThru; " +
		"$p.Id | Out-File -Encoding ascii -FilePath child.pid; Write-Output started"
	res := winRun(t, shell, sandbox.Policy{WorkDir: dir, Limits: sandbox.Limits{Wall: 20 * time.Second}})

	raw, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatalf("child pid not recorded (out=%q): %v", res.Output, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("bad child pid %q: %v", raw, err)
	}
	// The run has returned and the job handle is closed; give the OS a moment to
	// tear the tree down, then the child must be gone.
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(pid) {
		if p, e := os.FindProcess(pid); e == nil {
			_ = p.Kill() // don't leak the child if the assertion fails
		}
		t.Fatalf("background child %d outlived the sandboxed run; kill-on-close did not reap it", pid)
	}
}

// processAlive reports whether pid names a still-running process.
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
