//go:build darwin

package sandbox_test

// Adversarial (red-team) containment suite for the macOS Seatbelt restricted
// backend (SEC-010 / TEN-346). Each test drives the REAL backend returned by
// sandbox.Restricted() with a policy shaped like the escape suite's (temp
// WorkDir; ReadRoots/WriteRoots = WorkDir; DenyRoots = a dir holding a secret;
// Network = NetNone) and one attack per test.
//
// Convention (per the SEC-010 red-team brief):
//   - Attacks that ARE contained assert containment (regression tests).
//   - Genuine ESCAPES / gaps are written to assert the SECURE behaviour but
//     t.Skip("KNOWN ...") at the TOP, so the suite stays green while the gap is
//     recorded. The lead removes the skip once the backend is fixed.
//
// Every test cleans up whatever it creates, including background processes and
// any $HOME / temp files, and never prints real secret CONTENTS (byte counts and
// existence only).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"tenant/internal/sandbox"
)

// rtSecret is the marker written into the out-of-roots secret. A contained
// adversary must never echo it back.
const rtSecret = "REDTEAM-SECRET-DO-NOT-LEAK-5b2d9f"

// rtEnv is one attack's isolated filesystem: a work dir plus a secret dir that
// sits OUTSIDE every policy root (its parent, base, is also outside the roots).
type rtEnv struct {
	base       string
	work       string
	secretDir  string
	secretFile string
}

func newRTEnv(t *testing.T) rtEnv {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "work")
	secret := filepath.Join(base, "secret")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	sf := filepath.Join(secret, "secret.txt")
	if err := os.WriteFile(sf, []byte(rtSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "inside.txt"), []byte("inside-ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return rtEnv{base: base, work: work, secretDir: secret, secretFile: sf}
}

// basePolicy is the standard escape-suite policy: work dir readable+writable,
// the secret dir denied, no network, a wall + output cap.
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

// wholeHostPolicy is the PRODUCTION-DEFAULT shape: ReadRoots empty => the whole
// host is readable and only DenyRoots masks it.
func (e rtEnv) wholeHostPolicy() sandbox.Policy {
	p := e.basePolicy()
	p.ReadRoots = nil
	return p
}

func restricted(t *testing.T) sandbox.Backend {
	t.Helper()
	b, err := sandbox.Restricted()
	if err != nil {
		t.Fatalf("sandbox.Restricted() on darwin: %v", err)
	}
	return b
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

// shq single-quotes a path for POSIX sh.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// processAlive reports whether pid is a live process (kill -0 from the test
// process, which is NOT sandboxed).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// ---------------------------------------------------------------------------
//  1. Hardlink a DenyRoot file into the writable WorkDir, then read the link.
//     Seatbelt matches on path, not inode — but creating the link requires
//     file access to the (denied) SOURCE, so `ln` is refused. CONTAINED.
//
// ---------------------------------------------------------------------------
func TestRedteam_HardlinkSecretIntoWorkDir(t *testing.T) {
	b := restricted(t)
	e := newRTEnv(t)
	link := filepath.Join(e.work, "hardlink")
	res := mustRun(t, b, e.basePolicy(),
		"ln "+shq(e.secretFile)+" "+shq(link)+" && cat "+shq(link))
	assertContained(t, res, "hardlink-into-workdir")
	assertNoLeak(t, res, "hardlink-into-workdir")
}

// ---------------------------------------------------------------------------
//  2. Symlink tricks. Seatbelt canonicalizes symlinks and matches the RESOLVED
//     path, so a link INTO the DenyRoot is denied on read and a link pointing
//     OUTSIDE is denied on write-through. CONTAINED.
//
// ---------------------------------------------------------------------------
func TestRedteam_SymlinkReadSecret(t *testing.T) {
	b := restricted(t)
	e := newRTEnv(t)
	link := filepath.Join(e.work, "s")
	// Absolute target so the symlink resolves to the real secret regardless of cwd.
	res := mustRun(t, b, e.basePolicy(),
		"ln -s "+shq(e.secretFile)+" "+shq(link)+" && cat "+shq(link))
	assertContained(t, res, "symlink-read-secret")
	assertNoLeak(t, res, "symlink-read-secret")
}

func TestRedteam_SymlinkWriteOutside(t *testing.T) {
	b := restricted(t)
	e := newRTEnv(t)
	victim := filepath.Join(e.base, "outside_victim.txt") // outside every write root
	if err := os.WriteFile(victim, []byte("ORIGINAL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(e.work, "s2")
	res := mustRun(t, b, e.basePolicy(),
		"ln -s "+shq(victim)+" "+shq(link)+" && echo PWNED > "+shq(link))
	assertContained(t, res, "symlink-write-outside")
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "PWNED") {
		t.Fatalf("ESCAPE: wrote through a symlink to a file outside the write roots: %q", got)
	}
}

// ---------------------------------------------------------------------------
//  3. rename()/mv across the boundary: a DenyRoot file cannot be renamed into the
//     WorkDir, and a WorkDir file cannot be renamed out. CONTAINED.
//
// ---------------------------------------------------------------------------
func TestRedteam_RenameAcrossBoundary(t *testing.T) {
	b := restricted(t)

	t.Run("secret-into-workdir", func(t *testing.T) {
		e := newRTEnv(t)
		dst := filepath.Join(e.work, "stolen.txt")
		res := mustRun(t, b, e.basePolicy(),
			"mv "+shq(e.secretFile)+" "+shq(dst)+" && cat "+shq(dst))
		assertContained(t, res, "mv-secret-in")
		assertNoLeak(t, res, "mv-secret-in")
		if _, err := os.Stat(e.secretFile); err != nil {
			t.Fatalf("ESCAPE: the secret was moved out of the DenyRoot: %v", err)
		}
	})

	t.Run("workfile-out-of-workdir", func(t *testing.T) {
		e := newRTEnv(t)
		src := filepath.Join(e.work, "tomove.txt")
		if err := os.WriteFile(src, []byte("exfil-me\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		exfil := filepath.Join(e.base, "exfil.txt") // outside write roots
		res := mustRun(t, b, e.basePolicy(), "mv "+shq(src)+" "+shq(exfil))
		assertContained(t, res, "mv-out")
		if _, err := os.Stat(exfil); err == nil {
			t.Fatalf("ESCAPE: a work-dir file was renamed outside the write roots to %s", exfil)
		}
	})
}

// ---------------------------------------------------------------------------
//
//  4. KNOWN ESCAPE — process persistence. A backgrounded child OUTLIVES the run:
//     the backend runs sandbox-exec via exec.CommandContext with the DEFAULT
//     cancel (kills only the direct child) and no process group, so an orphaned
//     `sleep` reparents to launchd and keeps running after Run returns (even when
//     the wall deadline fires). Confirmed against the real backend.
//
//     Fix (Go side, restricted_darwin.go Run): put the child in its own process
//     group — c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} — set
//     c.Cancel to signal the group (syscall.Kill(-pgid, SIGKILL)), and after
//     Wait returns kill the whole group so no descendant survives.
//
// ---------------------------------------------------------------------------
func TestRedteam_BackgroundProcessOutlivesRun(t *testing.T) {
	t.Skip("KNOWN ESCAPE: backgrounded children outlive the run (no process-group kill in restricted_darwin.go Run); orphaned processes reparent to launchd")

	b := restricted(t)
	e := newRTEnv(t)
	pol := e.basePolicy()
	pol.Limits.Wall = 3 * time.Second

	// Redirect the child's stdio off the output pipe so CombinedOutput returns
	// promptly with just the pid; the sleep keeps running in the background.
	res := mustRun(t, b, pol, "nohup sleep 120 >/dev/null 2>&1 & echo $!")
	pid, err := strconv.Atoi(strings.TrimSpace(res.Output))
	if err != nil {
		t.Fatalf("could not parse backgrounded pid from %q: %v", res.Output, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	time.Sleep(500 * time.Millisecond)
	// SECURE behaviour (asserted once the backend group-kills): the child must be
	// gone after the run returns.
	if processAlive(pid) {
		t.Fatalf("ESCAPE: backgrounded child pid=%d is still alive after Run returned", pid)
	}
}

// ---------------------------------------------------------------------------
//  5. Signals — a sandboxed command must not be able to kill an UNRELATED process
//     started outside the sandbox (e.g. Tenant itself). `(deny default)` denies
//     the Seatbelt `signal` operation. CONTAINED.
//
// ---------------------------------------------------------------------------
func TestRedteam_CannotSignalUnrelatedProcess(t *testing.T) {
	b := restricted(t)
	e := newRTEnv(t)

	victim := exec.Command("/bin/sleep", "300") // NOT sandboxed; stands in for Tenant
	if err := victim.Start(); err != nil {
		t.Fatalf("could not start victim sleep: %v", err)
	}
	pid := victim.Process.Pid
	t.Cleanup(func() {
		_ = victim.Process.Kill()
		_, _ = victim.Process.Wait()
	})

	res := mustRun(t, b, e.basePolicy(), "kill -9 "+strconv.Itoa(pid)+" ; echo done")
	// The kill must fail; the victim must survive.
	if !processAlive(pid) {
		t.Fatalf("ESCAPE: sandboxed command killed an unrelated outside process pid=%d (output=%q)", pid, res.Output)
	}
}

// ---------------------------------------------------------------------------
//  6. Mach/XPC surfaces. `(deny default)` denies mach-lookup, so launchd, the
//     pasteboard, cfprefsd, securityd (Keychain) and LaunchServices are all
//     unreachable. CONTAINED.
//
// ---------------------------------------------------------------------------
func TestRedteam_MachServicesContained(t *testing.T) {
	b := restricted(t)

	t.Run("launchd-launchctl", func(t *testing.T) {
		e := newRTEnv(t)
		assertContained(t, mustRun(t, b, e.basePolicy(), "launchctl list"), "launchctl-list")
	})

	t.Run("pasteboard-pbpaste", func(t *testing.T) {
		e := newRTEnv(t)
		// pbpaste must not return clipboard bytes; the Mach lookup is denied.
		assertContained(t, mustRun(t, b, e.basePolicy(), "pbpaste"), "pbpaste")
	})

	t.Run("cfprefsd-defaults", func(t *testing.T) {
		e := newRTEnv(t)
		assertContained(t, mustRun(t, b, e.basePolicy(), "defaults read com.apple.dock"), "defaults-read")
	})

	t.Run("keychain-securityd", func(t *testing.T) {
		if _, err := exec.LookPath("security"); err != nil {
			t.Skip("security tool unavailable")
		}
		e := newRTEnv(t)
		// Build a throwaway keychain OUTSIDE the login keychain so the test never
		// touches the operator's real Keychain.
		kc := filepath.Join(t.TempDir(), "rt.keychain")
		const kcSentinel = "KEYCHAIN-SENTINEL-3af9c1"
		if out, err := exec.Command("security", "create-keychain", "-p", "", kc).CombinedOutput(); err != nil {
			t.Skipf("cannot create scratch keychain: %v %s", err, out)
		}
		t.Cleanup(func() { _ = exec.Command("security", "delete-keychain", kc).Run() })
		_ = exec.Command("security", "unlock-keychain", "-p", "", kc).Run()
		if out, err := exec.Command("security", "add-generic-password", "-a", "rt", "-s", "tenant-rt-kc", "-w", kcSentinel, "-A", kc).CombinedOutput(); err != nil {
			t.Skipf("cannot seed scratch keychain: %v %s", err, out)
		}
		// Sanity: the host itself can read it back (else the test proves nothing).
		if ctl, _ := exec.Command("security", "find-generic-password", "-w", "-s", "tenant-rt-kc", kc).CombinedOutput(); !strings.Contains(string(ctl), kcSentinel) {
			t.Skip("keychain control read did not return the sentinel; environment cannot exercise this test")
		}
		res := mustRun(t, b, e.basePolicy(), "security find-generic-password -w -s tenant-rt-kc "+shq(kc))
		if strings.Contains(res.Output, kcSentinel) {
			t.Fatalf("ESCAPE: sandboxed command read a Keychain secret: %q", res.Output)
		}
	})

	t.Run("launchservices-open", func(t *testing.T) {
		e := newRTEnv(t)
		before := textEditRunning()
		// -g -j: launch in background, hidden, to minimise disruption if it escaped.
		res := mustRun(t, b, e.basePolicy(), "open -g -j -a TextEdit")
		t.Cleanup(func() {
			if !before && textEditRunning() {
				_ = exec.Command("killall", "TextEdit").Run()
			}
		})
		assertContained(t, res, "open-textedit")
		if !before && textEditRunning() {
			t.Fatalf("ESCAPE: `open -a TextEdit` launched a GUI app outside the sandbox")
		}
	})
}

func textEditRunning() bool {
	return exec.Command("pgrep", "-x", "TextEdit").Run() == nil
}

// ---------------------------------------------------------------------------
//  7. Network under NetNone. `(deny network*)` cuts TCP, UDP/DNS and unix-domain
//     sockets; the connect is refused at the kernel regardless of host
//     connectivity, so these need no outbound network to assert. CONTAINED.
//
// ---------------------------------------------------------------------------
func TestRedteam_NetworkNetNoneContained(t *testing.T) {
	b := restricted(t)

	t.Run("tcp-dev-tcp", func(t *testing.T) {
		e := newRTEnv(t)
		assertContained(t, mustRun(t, b, e.basePolicy(),
			"exec 3<>/dev/tcp/1.1.1.1/53 && echo connected"), "dev-tcp")
	})

	t.Run("curl", func(t *testing.T) {
		if _, err := exec.LookPath("curl"); err != nil {
			t.Skip("curl unavailable")
		}
		e := newRTEnv(t)
		assertContained(t, mustRun(t, b, e.basePolicy(),
			"curl -sS -m 3 http://1.1.1.1/"), "curl")
	})

	t.Run("dns-host", func(t *testing.T) {
		if _, err := exec.LookPath("host"); err != nil {
			t.Skip("host unavailable")
		}
		e := newRTEnv(t)
		assertContained(t, mustRun(t, b, e.basePolicy(),
			"host -W 2 example.com"), "dns-host")
	})

	t.Run("unix-domain-socket", func(t *testing.T) {
		if _, err := exec.LookPath("nc"); err != nil {
			t.Skip("nc unavailable")
		}
		e := newRTEnv(t)
		assertContained(t, mustRun(t, b, e.basePolicy(),
			"nc -U -G 2 -w 2 /var/run/mDNSResponder </dev/null"), "unix-socket")
	})
}

// ---------------------------------------------------------------------------
//  8. Read confidentiality with ReadRoots EMPTY (production default: whole host
//     readable minus DenyRoots). The DenyRoot mask is what stops reads — a path
//     OUTSIDE the work dir and NOT in any DenyRoot is fully readable. This is the
//     documented design, and this test pins it so the risk is explicit:
//     production profiles MUST enumerate every credential dir (~/.ssh, ~/.aws,
//     ~/.config/gh, ~/.docker, ~/Library/Keychains, ...) in DenyRoots or the
//     sandbox is a full read-exfil surface for user secrets. Verified live: with
//     ReadRoots empty, ~/.config/gh/hosts.yml and ~/.docker/config.json were
//     readable and ~/.ssh listable.
//
// ---------------------------------------------------------------------------
func TestRedteam_WholeHostReadReachesOutsideWorkDir(t *testing.T) {
	b := restricted(t)
	e := newRTEnv(t)

	// A stand-in credential OUTSIDE every root (but not in a DenyRoot).
	cred := filepath.Join(e.base, "fake_cred.txt")
	const marker = "WHOLE-HOST-READ-REACHED-OUTSIDE-WORKDIR"
	if err := os.WriteFile(cred, []byte(marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// With ReadRoots empty the file is readable even though it is outside WorkDir.
	got := mustRun(t, b, e.wholeHostPolicy(), "cat "+shq(cred))
	if got.ExitCode != 0 || !strings.Contains(got.Output, marker) {
		t.Fatalf("expected whole-host read to reach a file outside the work dir; exit=%d output=%q", got.ExitCode, got.Output)
	}

	// The ONLY thing that stops a read under whole-host mode is a DenyRoot: the
	// secret dir stays masked.
	den := mustRun(t, b, e.wholeHostPolicy(), "cat "+shq(e.secretFile))
	assertContained(t, den, "wholehost-denyroot")
	assertNoLeak(t, den, "wholehost-denyroot")
}

// ---------------------------------------------------------------------------
//
//  9. KNOWN GAP — DenyRoot metadata leak. `ls` on a DenyRoot is denied, but
//     `stat` on a KNOWN filename inside it SUCCEEDS and leaks size / existence /
//     mtime / mode: the broad `(allow file-read-metadata)` is not masked by the
//     `(deny file-read* (subpath denyroot))` for the metadata operation. This
//     diverges from the DenyRoots "never readable" contract (Policy.DenyRoots).
//     No CONTENTS leak. Confirmed against the real backend (stat returned size=32
//     for a 32-byte secret).
//
//     Fix (SBPL, restricted_darwin.go seatbeltProfile): emit an explicit
//     `(deny file-read-metadata (subpath <denyroot>))` for each DenyRoot after
//     the existing deny lines. Validated: with that extra deny, `stat` on the
//     secret returns "Operation not permitted" while legitimate reads still work.
//
// ---------------------------------------------------------------------------
func TestRedteam_DenyRootMetadataLeak(t *testing.T) {
	t.Skip("KNOWN GAP: `stat` leaks size/existence/mtime of DenyRoot files (broad file-read-metadata is not masked by the DenyRoot deny); add (deny file-read-metadata (subpath denyroot))")

	b := restricted(t)
	e := newRTEnv(t)
	// SECURE behaviour (asserted once the metadata deny is added): stat inside a
	// DenyRoot is refused.
	assertContained(t, mustRun(t, b, e.basePolicy(),
		`stat -f "%N size=%z" `+shq(e.secretFile)), "stat-denyroot")
}

// DenyRoot directory ENUMERATION (readdir) is already contained — `ls` cannot
// list names inside a DenyRoot. Regression test.
func TestRedteam_DenyRootListingContained(t *testing.T) {
	b := restricted(t)
	e := newRTEnv(t)
	assertContained(t, mustRun(t, b, e.basePolicy(), "ls -la "+shq(e.secretDir)), "ls-denyroot")
}

// ---------------------------------------------------------------------------
//  10. Write persistence — writes to $HOME dotfiles, LaunchAgents (persistence)
//     and /tmp are all outside the write roots and denied. Non-existent sentinel
//     names are used so nothing real is touched even if an escape occurred.
//     CONTAINED.
//
// ---------------------------------------------------------------------------
func TestRedteam_WritePersistenceContained(t *testing.T) {
	b := restricted(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	targets := map[string]string{
		"zshrc":        filepath.Join(home, ".zshrc_tenant_rt_probe"),
		"launchagents": filepath.Join(home, "Library", "LaunchAgents", "tenant_rt_probe.plist"),
		"tmp":          "/tmp/tenant_rt_probe",
	}
	for name, path := range targets {
		path := path
		t.Run(name, func(t *testing.T) {
			e := newRTEnv(t)
			t.Cleanup(func() { _ = os.Remove(path) })
			res := mustRun(t, b, e.basePolicy(), "echo pwned > "+shq(path))
			assertContained(t, res, "write-"+name)
			if _, err := os.Stat(path); err == nil {
				t.Fatalf("ESCAPE: wrote a persistence file outside the write roots at %s", path)
			}
		})
	}
}

// ---------------------------------------------------------------------------
//  11. Nested sandbox-exec must not RE-ENTER the sandbox with a more permissive
//     profile to escalate. macOS refuses to re-apply a profile inside an already
//     sandboxed process, so the inner `(allow default)` cannot un-deny the
//     secret. CONTAINED.
//
// ---------------------------------------------------------------------------
func TestRedteam_NestedSandboxExecNoEscalation(t *testing.T) {
	b := restricted(t)
	e := newRTEnv(t)
	inner := "/usr/bin/sandbox-exec -p '(version 1)(allow default)' sh -c " + shq("cat "+shq(e.secretFile))
	res := mustRun(t, b, e.basePolicy(), inner)
	assertContained(t, res, "nested-sandbox-exec")
	assertNoLeak(t, res, "nested-sandbox-exec")
}

// ---------------------------------------------------------------------------
//  12. Environment scoping. A ZERO EnvPolicy (the escape-suite / default shape)
//     inherits the FULL parent environment — any secret in Tenant's own env is
//     visible to a sandboxed command. Isolate=true scopes it away. This pins
//     both facts so the default's sharp edge is explicit: unattended surfaces
//     should set EnvPolicy.Isolate (or an explicit Pass allowlist) rather than
//     rely on the zero value.
//
// ---------------------------------------------------------------------------
func TestRedteam_EnvInheritanceAndScoping(t *testing.T) {
	b := restricted(t)
	e := newRTEnv(t)
	const envSentinel = "ENV-SENTINEL-LEAK-9c2f"
	t.Setenv("TENANT_RT_ENV_SENTINEL", envSentinel)

	// Zero EnvPolicy: the child sees the parent env (documented inheritance).
	leaked := mustRun(t, b, e.basePolicy(), `printf '%s' "$TENANT_RT_ENV_SENTINEL"`)
	if !strings.Contains(leaked.Output, envSentinel) {
		t.Fatalf("expected the default (zero) EnvPolicy to inherit the parent env; output=%q", leaked.Output)
	}

	// Isolate=true: the child no longer sees it (PATH is set so sh is found).
	pol := e.basePolicy()
	pol.Env = sandbox.EnvPolicy{Isolate: true, Set: map[string]string{"PATH": "/usr/bin:/bin"}}
	scoped := mustRun(t, b, pol, `printf '%s' "${TENANT_RT_ENV_SENTINEL:-<absent>}"`)
	if strings.Contains(scoped.Output, envSentinel) {
		t.Fatalf("ESCAPE: an isolated EnvPolicy still leaked the parent env sentinel: %q", scoped.Output)
	}
}

// ---------------------------------------------------------------------------
//  13. Resource limits. The wall-clock deadline IS enforced (an infinite loop
//     times out => contained). Probe must NOT overstate the rest: macOS has no
//     PID or memory cap (no cgroups / Job Object), so Limits is reported partial.
//     This regression keeps the disclosure honest.
//
// ---------------------------------------------------------------------------
func TestRedteam_ResourceLimits_WallEnforced_Disclosed(t *testing.T) {
	b := restricted(t)

	caps := b.Probe(context.Background())
	if caps.Limits == sandbox.EnforceFull {
		t.Fatalf("Probe claims Limits fully enforced, but the macOS backend caps neither PIDs nor memory; expected partial")
	}

	e := newRTEnv(t)
	pol := e.basePolicy()
	pol.Limits.Wall = 2 * time.Second
	res := mustRun(t, b, pol, "while :; do :; done")
	if !res.TimedOut {
		t.Fatalf("wall-clock deadline not enforced: TimedOut=false (exit=%d output=%q)", res.ExitCode, res.Output)
	}
	assertContained(t, res, "wall-timeout")
}

// KNOWN GAP — no MaxPIDs / memory cap on macOS. Recorded and skipped (the wall
// timeout is the only Limits dimension the Seatbelt backend can enforce; a real
// PID/RSS cap needs the container backend, TEN-318). Body left as the regression
// to run once a cap exists.
func TestRedteam_MaxPIDsNotEnforced(t *testing.T) {
	t.Skip("KNOWN GAP: the macOS backend enforces no MaxPIDs/memory cap (Probe reports Limits=partial, wall-only); a PID/RSS cap needs the container backend (TEN-318)")

	b := restricted(t)
	e := newRTEnv(t)
	pol := e.basePolicy()
	pol.Limits.MaxPIDs = 4
	// SECURE behaviour (once a cap exists): a bounded fork loop past the cap is
	// refused / reaped rather than spawning freely.
	res := mustRun(t, b, pol, "i=0; while [ $i -lt 16 ]; do sh -c 'exit 0'; i=$((i+1)); done; echo spawned=$i")
	assertContained(t, res, "maxpids")
}
