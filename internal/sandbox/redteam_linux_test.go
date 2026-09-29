//go:build linux

package sandbox_test

// Live containment suite for the Linux bubblewrap restricted backend (SEC-010 /
// TEN-346), the counterpart of redteam_darwin_test.go: the escape suite, then
// one test per attack class in the plan's cases 1-8 (docs/plans/sandboxing-plan.md
// §5), each driving the REAL backend sandbox.Restricted() returns.
//
// It needs a working bubblewrap. Where there is none every test skips, unless
// TENANT_SANDBOX_LIVE=1 (set by CI): then an unavailable backend fails the run,
// so the suite can never pass by not running.
//
// Fixtures live outside /tmp: the sandbox's /tmp is a private tmpfs, which would
// hide a secret under the host's /tmp whether or not the mask under test works.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/sandbox"
	"tenant/internal/sandbox/escapetest"
)

// rtSecret is the marker written into the out-of-roots secret. A contained
// adversary must never echo it back.
const rtSecret = "REDTEAM-SECRET-DO-NOT-LEAK-5b2d9f"

func liveBackend(t *testing.T) sandbox.Backend {
	t.Helper()
	b, err := sandbox.Restricted()
	if err != nil {
		if os.Getenv("TENANT_SANDBOX_LIVE") == "1" {
			t.Fatalf("TENANT_SANDBOX_LIVE=1 but the restricted backend is unavailable: %v", err)
		}
		t.Skipf("no restricted backend here (TENANT_SANDBOX_LIVE=1 makes this a failure): %v", err)
	}
	return b
}

// liveTempDir is a fresh directory outside /tmp, removed after the test.
func liveTempDir(t *testing.T) string {
	t.Helper()
	parent, err := os.UserCacheDir()
	if err == nil {
		err = os.MkdirAll(parent, 0o700)
	}
	if err != nil {
		t.Fatalf("no cache dir for fixtures outside /tmp: %v", err)
	}
	dir, err := os.MkdirTemp(parent, "tenant-redteam-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// rtEnv is one attack's filesystem: a work dir plus a secret dir that sits
// OUTSIDE every policy root (their parent, base, is outside the roots too).
type rtEnv struct{ base, work, secretDir, secretFile string }

func newRTEnv(t *testing.T) rtEnv {
	t.Helper()
	base := liveTempDir(t)
	e := rtEnv{base: base, work: filepath.Join(base, "work"), secretDir: filepath.Join(base, "secret")}
	e.secretFile = filepath.Join(e.secretDir, "secret.txt")
	for _, d := range []string{e.work, e.secretDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(e.secretFile, []byte(rtSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

// basePolicy is the escape suite's: work dir readable+writable (an allowlist),
// the secret dir denied, no network.
func (e rtEnv) basePolicy() sandbox.Policy {
	return sandbox.Policy{
		WorkDir:    e.work,
		ReadRoots:  []string{e.work},
		WriteRoots: []string{e.work},
		DenyRoots:  []string{e.secretDir},
		Network:    sandbox.NetNone,
		Limits:     sandbox.Limits{Wall: 10 * time.Second, MaxOutputBytes: 1 << 20},
		Enforce:    true,
	}
}

// wholeHostPolicy is the PRODUCTION-DEFAULT shape: ReadRoots empty, so the whole
// host is readable and only DenyRoots (and the hidden /run and /tmp) mask it.
func (e rtEnv) wholeHostPolicy() sandbox.Policy {
	p := e.basePolicy()
	p.ReadRoots = nil
	return p
}

// shapes are the two read layers bubblewrap builds, each attack runs under both.
var shapes = []string{"allowlist", "whole-host"}

func (e rtEnv) policy(shape string) sandbox.Policy {
	if shape == "whole-host" {
		return e.wholeHostPolicy()
	}
	return e.basePolicy()
}

// mustRun runs one shell command under pol and fails only on an INFRA error (a
// non-zero exit is signal, not an error).
func mustRun(t *testing.T, b sandbox.Backend, pol sandbox.Policy, shell string) sandbox.Result {
	t.Helper()
	res, err := b.Run(context.Background(), sandbox.Command{Shell: shell, Surface: "redteam", Origin: t.Name()}, pol)
	if err != nil {
		t.Fatalf("infra error running %q: %v", shell, err)
	}
	return res
}

func assertContained(t *testing.T, res sandbox.Result, what string) {
	t.Helper()
	if !res.Contained() {
		t.Fatalf("%s: expected CONTAINED, but Contained()=false (exit=%d timedOut=%v violations=%v output=%q)",
			what, res.ExitCode, res.TimedOut, res.Violations, res.Output)
	}
}

func assertNoLeak(t *testing.T, res sandbox.Result, what string) {
	t.Helper()
	if strings.Contains(res.Output, rtSecret) {
		t.Fatalf("%s: SECRET LEAKED in output: %q", what, res.Output)
	}
}

func assertOK(t *testing.T, res sandbox.Result, what string) {
	t.Helper()
	if res.ExitCode != 0 || res.TimedOut {
		t.Fatalf("%s: the legitimate twin must succeed (exit=%d timedOut=%v output=%q)", what, res.ExitCode, res.TimedOut, res.Output)
	}
}

func need(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
}

// shq single-quotes a path for POSIX sh.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// TestEscape_BwrapContains runs the shared escape suite (secret read, write
// outside the roots, a .. escape, outbound network) with expectContained=true.
func TestEscape_BwrapContains(t *testing.T) {
	b := liveBackend(t)
	t.Setenv("TMPDIR", liveTempDir(t)) // the suite's fixtures, outside the private /tmp
	escapetest.Harness(t, b, true)
}

func TestBwrap_ProbeHonest(t *testing.T) {
	c := liveBackend(t).Probe(context.Background())
	if c.Isolation != sandbox.IsolationProcess || c.Filesystem != sandbox.EnforceFull || c.Network != sandbox.EnforceFull {
		t.Errorf("bubblewrap confines the filesystem and the network: %+v", c)
	}
	want := sandbox.EnforceFull
	if os.Geteuid() == 0 {
		want = sandbox.EnforcePartial // the kernel exempts root from the process cap
	}
	if c.Limits != want || c.Shell != "sh" {
		t.Errorf("limits = %v shell = %q, want %v and sh: %+v", c.Limits, c.Shell, want, c)
	}
}

// Where the backend is unavailable, the error says why in plain words; on an
// Ubuntu host with AppArmor's user-namespace restriction on, it names it. CI
// runs this before it lifts the restriction.
func TestLinuxRestricted_UnavailableIsExplained(t *testing.T) {
	_, err := sandbox.Restricted()
	if err == nil {
		t.Skip("the restricted backend is available here")
	}
	msg := err.Error()
	t.Log(msg)
	if !strings.Contains(msg, "restricted unavailable") {
		t.Errorf("the error must say restricted is unavailable: %q", msg)
	}
	// Without bwrap that is the reason, whatever else the host restricts: the
	// user-namespace settings only explain a bwrap that is there but can't run.
	if _, lerr := exec.LookPath("bwrap"); lerr != nil {
		if !strings.Contains(msg, "bubblewrap not installed") {
			t.Errorf("without bwrap the error must say bubblewrap is not installed: %q", msg)
		}
		return
	}
	if v, _ := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); strings.TrimSpace(string(v)) == "1" &&
		!strings.Contains(msg, "apparmor_restrict_unprivileged_userns=1") {
		t.Errorf("AppArmor restricts user namespaces here; the error must name it: %q", msg)
	}
}

// Cases 1 and 3: the secret is unreadable directly, through a symlink or a
// hardlink planted in the work dir, and via "..".
func TestBwrap_ReadEscapesContained(t *testing.T) {
	b := liveBackend(t)
	for _, shape := range shapes {
		for name, cmd := range map[string]func(e rtEnv) string{
			"direct":   func(e rtEnv) string { return "cat " + shq(e.secretFile) },
			"symlink":  func(e rtEnv) string { return "ln -s " + shq(e.secretFile) + " s && cat s" },
			"hardlink": func(e rtEnv) string { return "ln " + shq(e.secretFile) + " h && cat h" },
			"dotdot":   func(rtEnv) string { return "cat ../secret/secret.txt" },
			"list":     func(e rtEnv) string { return "ls -la " + shq(e.secretDir) + " | grep secret.txt" },
			"stat":     func(e rtEnv) string { return "stat " + shq(e.secretFile) },
		} {
			t.Run(shape+"/"+name, func(t *testing.T) {
				e := newRTEnv(t)
				res := mustRun(t, b, e.policy(shape), cmd(e))
				assertContained(t, res, name)
				assertNoLeak(t, res, name)
			})
		}
	}
}

// Cases 2 and 3: writes outside the write roots (through a symlink, by
// renaming out, to a dotfile) never land on the host; a write to /tmp lands
// only in the sandbox's own.
func TestBwrap_WriteEscapesDoNotLand(t *testing.T) {
	b := liveBackend(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	for _, shape := range shapes {
		t.Run(shape, func(t *testing.T) {
			e := newRTEnv(t)
			pol := e.policy(shape)
			victim := filepath.Join(e.base, "outside_victim.txt")
			if err := os.WriteFile(victim, []byte("ORIGINAL\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			dotfile := filepath.Join(home, ".tenant_rt_probe")
			hostTmp := filepath.Join("/tmp", "tenant_rt_probe_"+filepath.Base(e.base))
			t.Cleanup(func() { _ = os.Remove(dotfile); _ = os.Remove(hostTmp) })

			res := mustRun(t, b, pol, "ln -s "+shq(victim)+" s && echo PWNED > s")
			assertContained(t, res, "symlink-through")
			if got, _ := os.ReadFile(victim); string(got) != "ORIGINAL\n" {
				t.Fatalf("ESCAPE: wrote through a symlink to a file outside the write roots: %q", got)
			}
			res = mustRun(t, b, pol, "echo exfil > f && mv f "+shq(filepath.Join(e.base, "exfil.txt")))
			assertContained(t, res, "rename-out")
			res = mustRun(t, b, pol, "mv "+shq(e.secretFile)+" stolen && cat stolen")
			assertContained(t, res, "rename-in")
			assertNoLeak(t, res, "rename-in")
			res = mustRun(t, b, pol, "echo pwned > "+shq(dotfile))
			assertContained(t, res, "home-dotfile")
			if shape == "whole-host" && !res.Refused() {
				t.Errorf("a read-only-mount refusal must be attributed (escalation, dry-run): %+v", res)
			}
			assertOK(t, mustRun(t, b, pol, "echo pwned > "+shq(hostTmp)), "private /tmp")
			for _, p := range []string{filepath.Join(e.base, "exfil.txt"), dotfile, hostTmp} {
				if _, err := os.Stat(p); err == nil {
					t.Errorf("ESCAPE: a sandboxed write landed on the host at %s", p)
				}
			}
			if _, err := os.Stat(e.secretFile); err != nil {
				t.Errorf("ESCAPE: the secret was moved out of its deny root: %v", err)
			}
			assertOK(t, mustRun(t, b, pol, "echo hi > inside.txt && cat inside.txt"), "write inside the work dir")
		})
	}
}

// Case 4: with the network cut, the host's loopback listeners (Tenant's own
// dashboard among them) are out of reach. With it shared the same probe
// connects, so only the namespace stopped it.
func TestBwrap_NetNoneBlocksHostLoopback(t *testing.T) {
	b := liveBackend(t)
	need(t, "bash")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	probe := fmt.Sprintf("bash -c 'exec 3<>/dev/tcp/127.0.0.1/%d && echo connected'", ln.Addr().(*net.TCPAddr).Port)
	e := newRTEnv(t)
	res := mustRun(t, b, e.wholeHostPolicy(), probe)
	assertContained(t, res, "loopback")
	if strings.Contains(res.Output, "connected") {
		t.Fatalf("ESCAPE: reached a host loopback listener with network none: %q", res.Output)
	}
	pol := e.wholeHostPolicy()
	pol.Network = sandbox.NetFull
	assertOK(t, mustRun(t, b, pol, probe), "the same probe with the network shared")
}

// Case 4 (IPC): unix sockets are files, and reachable through a read-only
// mount and across network namespaces. The ones in the host's /tmp and /run,
// where session and system daemons listen, are hidden.
func TestBwrap_HostSocketsHidden(t *testing.T) {
	b := liveBackend(t)
	dir, err := os.MkdirTemp("/tmp", "tenant-rt-sock-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	e := newRTEnv(t)
	host, err := sandbox.Host().Run(context.Background(), sandbox.Command{Shell: "test -S " + shq(sock)}, e.wholeHostPolicy())
	if err != nil || host.ExitCode != 0 {
		t.Fatalf("the host must see its own socket (the twin): %v %+v", err, host)
	}
	assertContained(t, mustRun(t, b, e.wholeHostPolicy(), "test -S "+shq(sock)), "socket in the host /tmp")

	res := mustRun(t, b, e.wholeHostPolicy(), "ls -A /run")
	assertOK(t, res, "ls /run")
	if strings.TrimSpace(res.Output) != "" {
		t.Errorf("/run must be empty with the network cut, got %q", res.Output)
	}
	pol := e.wholeHostPolicy()
	pol.Network = sandbox.NetFull
	res = mustRun(t, b, pol, "find /run ! -type d ! -type f")
	if strings.TrimSpace(res.Output) != "" {
		t.Errorf("with the network shared only the resolver files come back into /run, got %q", res.Output)
	}
	if _, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		assertOK(t, mustRun(t, b, pol, "cat /etc/resolv.conf >/dev/null"), "DNS config with the network shared")
	}
}

// Case 5: a fork bomb stops at the process cap, which counts the sandbox's own
// processes, not the user's: the host user is crowded past it first.
func TestBwrap_ProcessCap(t *testing.T) {
	b := liveBackend(t)
	if os.Geteuid() == 0 {
		t.Skip("the kernel exempts root from RLIMIT_NPROC (Probe reports limits partial)")
	}
	for i := 0; i < 24; i++ {
		c := exec.Command("sleep", "60")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	}
	e := newRTEnv(t)
	pol := e.basePolicy()
	pol.Limits.MaxPIDs = 8
	assertOK(t, mustRun(t, b, pol, "sleep 0.1 & sleep 0.1 & wait; echo spawned"), "children under the cap")
	// Long sleeps: bash retries a failed fork, and must not find a slot freed.
	res := mustRun(t, b, pol, "i=0; while [ $i -lt 32 ]; do sleep 30 & i=$((i+1)); done; wait; echo spawned=$i")
	assertContained(t, res, "fork past the cap")
	if strings.Contains(res.Output, "spawned=32") {
		t.Fatalf("ESCAPE: 32 processes spawned under a cap of 8: %q", res.Output)
	}
}

// Case 5: an allocation past the memory cap fails; a small program runs.
func TestBwrap_MemoryCap(t *testing.T) {
	b := liveBackend(t)
	need(t, "awk")
	e := newRTEnv(t)
	pol := e.basePolicy()
	pol.Limits.MemoryBytes = 128 << 20
	assertOK(t, mustRun(t, b, pol, `awk 'BEGIN{print "small"}'`), "awk under the cap")
	// A 256 MiB string: bounded, so a regression cannot take the host down with it.
	res := mustRun(t, b, pol, `awk 'BEGIN{s="x"; for(i=0;i<28;i++) s=s s; print length(s)}'`)
	assertContained(t, res, "allocation past the cap")
	if strings.Contains(res.Output, "268435456") {
		t.Fatalf("ESCAPE: allocated 256 MiB under a 128 MiB cap: %q", res.Output)
	}
}

// Case 5: a busy loop is killed at its CPU-time cap, well before the wall clock.
func TestBwrap_CPUCap(t *testing.T) {
	b := liveBackend(t)
	e := newRTEnv(t)
	pol := e.basePolicy()
	pol.Limits.CPU = time.Second
	pol.Limits.Wall = 30 * time.Second
	start := time.Now()
	res := mustRun(t, b, pol, "while :; do :; done")
	if res.TimedOut || res.ExitCode == 0 || time.Since(start) > 15*time.Second {
		t.Fatalf("the CPU cap did not stop the loop (exit=%d timedOut=%v after %s)", res.ExitCode, res.TimedOut, time.Since(start))
	}
}

// Case 5: a command that floods its output returns only the capped output
// when the wall clock stops it; an endless loop is stopped the same way.
func TestBwrap_OutputFloodAndWall(t *testing.T) {
	b := liveBackend(t)
	e := newRTEnv(t)
	pol := e.basePolicy()
	pol.Limits.Wall = 2 * time.Second
	pol.Limits.MaxOutputBytes = 4096
	res := mustRun(t, b, pol, "yes")
	if !res.TimedOut || len(res.Output) > 4096+len("\n…[truncated]") {
		t.Fatalf("flood: timedOut=%v, %d bytes returned for a 4096-byte cap", res.TimedOut, len(res.Output))
	}
	if res := mustRun(t, b, pol, "while :; do :; done"); !res.TimedOut {
		t.Fatalf("wall clock not enforced: %+v", res)
	}
}

// Case 6: nothing the command starts outlives it: not a background job, not
// one that called setsid, not one under nohup. Each would write a marker a
// second after the command returned.
func TestBwrap_NothingOutlivesTheCommand(t *testing.T) {
	b := liveBackend(t)
	need(t, "setsid", "nohup")
	e := newRTEnv(t)
	res := mustRun(t, b, e.basePolicy(),
		"(sleep 1; echo alive > bg) >/dev/null 2>&1 & "+
			"setsid sh -c 'sleep 1; echo alive > setsid' >/dev/null 2>&1 & "+
			"nohup sh -c 'sleep 1; echo alive > nohup' >/dev/null 2>&1 & "+
			"sleep 30 & echo started") // the last one holds the output pipe
	assertOK(t, res, "the command itself")
	time.Sleep(2500 * time.Millisecond)
	for _, m := range []string{"bg", "setsid", "nohup"} {
		if _, err := os.Stat(filepath.Join(e.work, m)); err == nil {
			t.Errorf("ESCAPE: a %s process outlived the command and wrote its marker", m)
		}
	}
}

// Case 7: a secret in a benignly named variable. A zero EnvPolicy inherits it
// (documented: confined profiles scrub the environment); an allowlist drops it.
func TestBwrap_EnvScoping(t *testing.T) {
	b := liveBackend(t)
	const token = "ghp_FAKEtoken00000000000000000000000000"
	t.Setenv("TENANT_RT_BENIGN", token)
	e := newRTEnv(t)
	if res := mustRun(t, b, e.basePolicy(), `printf %s "$TENANT_RT_BENIGN"`); !strings.Contains(res.Output, token) {
		t.Fatalf("a zero EnvPolicy inherits the parent environment; output=%q", res.Output)
	}
	pol := e.basePolicy()
	pol.Env = sandbox.EnvPolicy{Isolate: true, Pass: []string{"PATH"}}
	res := mustRun(t, b, pol, `printf %s "${TENANT_RT_BENIGN:-absent}"`)
	if strings.Contains(res.Output, token) {
		t.Fatalf("ESCAPE: an env allowlist leaked a variable it does not name: %q", res.Output)
	}
}

// Case 8: Tenant's own config, data and binary under the production-default
// policy. The cron work dir inside the data dir stays usable (the carve-out);
// the settings are refused (attributed) and unchanged; the data around the
// work dir is neither readable nor writable; the binary reads empty and a
// write to it is discarded.
func TestBwrap_TenantDirsStayUnwritable(t *testing.T) {
	b := liveBackend(t)
	base := liveTempDir(t)
	cfg, data, bin := filepath.Join(base, "cfg"), filepath.Join(base, "data"), filepath.Join(base, "tenant")
	work := filepath.Join(data, "cron-work")
	for _, d := range []string{cfg, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, s := range map[string]string{filepath.Join(cfg, "settings.json"): "ORIGINAL\n", filepath.Join(data, "secret.txt"): rtSecret + "\n", bin: "BINARY\n"} {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pol := sandbox.Policy{WorkDir: work, DenyRoots: []string{cfg, data, bin}, Network: sandbox.NetNone,
		Limits: sandbox.Limits{Wall: 10 * time.Second}, Enforce: true}

	assertOK(t, mustRun(t, b, pol, "echo notes >> notes.md && cat notes.md"), "append to the work dir")
	res := mustRun(t, b, pol, "echo pwned >> "+shq(filepath.Join(cfg, "settings.json")))
	assertContained(t, res, "settings")
	if !res.Refused() {
		t.Errorf("a write into a deny root must be attributed as a refusal: %+v", res)
	}
	res = mustRun(t, b, pol, "cat ../secret.txt; echo pwned > ../cron.json")
	assertContained(t, res, "data dir around the work dir")
	assertNoLeak(t, res, "data dir around the work dir")
	res = mustRun(t, b, pol, "cat "+shq(bin)+"; echo pwned > "+shq(bin))
	if strings.Contains(res.Output, "BINARY") {
		t.Errorf("the binary must read empty: %q", res.Output)
	}
	for p, want := range map[string]string{filepath.Join(cfg, "settings.json"): "ORIGINAL\n", bin: "BINARY\n"} {
		if got, _ := os.ReadFile(p); string(got) != want {
			t.Errorf("ESCAPE: %s changed to %q", p, got)
		}
	}
	if _, err := os.Stat(filepath.Join(data, "cron.json")); err == nil {
		t.Errorf("ESCAPE: wrote into the data dir")
	}
}

// A nested user namespace cannot unmount a deny mask to see what it hides:
// mounts inherited from a more privileged namespace are locked.
func TestBwrap_NestedNamespaceCannotUnmask(t *testing.T) {
	b := liveBackend(t)
	need(t, "unshare")
	e := newRTEnv(t)
	res := mustRun(t, b, e.wholeHostPolicy(), "unshare -Urm sh -c "+
		shq("umount "+shq(e.secretDir)+"; umount -l "+shq(e.secretDir)+"; cat "+shq(e.secretFile)))
	assertContained(t, res, "nested unmount")
	assertNoLeak(t, res, "nested unmount")
}

// Run as root, bubblewrap keeps every capability unless told otherwise, and
// the command could unmount a deny mask. The backend drops them all.
func TestBwrap_RootHasNoCapabilities(t *testing.T) {
	b := liveBackend(t)
	if os.Geteuid() != 0 {
		t.Skip("only meaningful when Tenant runs as root")
	}
	e := newRTEnv(t)
	res := mustRun(t, b, e.wholeHostPolicy(), "grep CapEff /proc/self/status; umount "+shq(e.secretDir)+" && cat "+shq(e.secretFile))
	if !strings.Contains(res.Output, "CapEff:\t0000000000000000") {
		t.Errorf("the sandboxed command kept capabilities: %q", res.Output)
	}
	assertNoLeak(t, res, "root umount")
}
