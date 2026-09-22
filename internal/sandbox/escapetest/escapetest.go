// Package escapetest is a backend-agnostic containment suite for sandbox.Backend
// implementations. It builds a small work dir with a SECRET file placed OUTSIDE
// every policy root, then runs adversarial commands (read the secret, write
// outside the write roots, escape via "..", reach the network) alongside a
// legitimate twin for each that must succeed on any host with no network.
//
// Harness(t, backend, expectContained) asserts, per case, that the adversarial
// command's Result.Contained() equals expectContained and that the legitimate
// twin exits 0. Passing expectContained=false against the host backend documents
// the gap (the host backend does not confine the filesystem); passing
// expectContained=true against a real restricted backend proves containment.
//
// Assertions are kept honest: the network case is the only one that needs
// connectivity, and its containment assertion is skipped (with a log line) when
// the host itself has no outbound network — the filesystem cases are never
// skipped or weakened.
package escapetest

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/sandbox"
)

// secretSentinel is the marker written into the out-of-roots secret file. When a
// backend is expected to contain, no adversarial output may contain it.
const secretSentinel = "TOP-SECRET-DO-NOT-LEAK-9f3a1c7e"

// Harness runs the containment suite against b. expectContained is what every
// adversarial case's Result.Contained() must equal: true for a confining backend,
// false for the unconfined host backend (which the suite then documents).
func Harness(t *testing.T, b sandbox.Backend, expectContained bool) {
	t.Helper()

	// (a) read a secret file OUTSIDE all roots vs read a file inside the work dir.
	t.Run("read-secret-outside-roots", func(t *testing.T) {
		e := newEnv(t)
		adv := sandbox.Command{Shell: "cat " + shq(e.secretFile), Surface: "escapetest", Origin: "read-secret"}
		e.assertContainment(t, b, adv, expectContained, true)
		legit := sandbox.Command{Shell: "cat " + shq(e.legitFile), Surface: "escapetest", Origin: "read-inside"}
		e.assertLegit(t, b, legit)
	})

	// (b) write to $HOME (outside the write roots) vs write inside the work dir.
	t.Run("write-outside-roots", func(t *testing.T) {
		e := newEnv(t)
		if home, err := os.UserHomeDir(); err == nil {
			t.Cleanup(func() { _ = os.Remove(filepath.Join(home, "tenant_sbx_escape")) })
		}
		adv := sandbox.Command{Shell: `echo pwned > "$HOME/tenant_sbx_escape"`, Surface: "escapetest", Origin: "write-home"}
		e.assertContainment(t, b, adv, expectContained, false)
		legit := sandbox.Command{Shell: "echo hi > " + shq(filepath.Join(e.workDir, "legit_write.txt")), Surface: "escapetest", Origin: "write-inside"}
		e.assertLegit(t, b, legit)
	})

	// (c) path-escape the work dir with ".." vs a plain echo.
	t.Run("path-escape-dotdot", func(t *testing.T) {
		e := newEnv(t)
		escape := e.workDir + "/../" + filepath.Base(e.secretDir) + "/" + filepath.Base(e.secretFile)
		adv := sandbox.Command{Shell: "cat " + shq(escape), Surface: "escapetest", Origin: "dotdot"}
		e.assertContainment(t, b, adv, expectContained, true)
		legit := sandbox.Command{Shell: "echo ok", Surface: "escapetest", Origin: "echo"}
		e.assertLegit(t, b, legit)
	})

	// (d) OPTIONAL: reach the network vs a plain echo. The containment assertion is
	// skipped when the host has no outbound network (the filesystem cases above are
	// never skipped).
	t.Run("network-outbound", func(t *testing.T) {
		e := newEnv(t)
		legit := sandbox.Command{Shell: "echo ok", Surface: "escapetest", Origin: "echo"}
		if !hostHasNetwork() {
			t.Log("host has no outbound TCP/53; skipping only the network containment assertion (filesystem cases still enforced)")
			e.assertLegit(t, b, legit)
			return
		}
		adv := sandbox.Command{Shell: "exec 3<>/dev/tcp/1.1.1.1/53 && echo connected", Surface: "escapetest", Origin: "net"}
		e.assertContainment(t, b, adv, expectContained, false)
		e.assertLegit(t, b, legit)
	})
}

// env is one case's isolated filesystem: a work dir plus a secret dir that sits
// OUTSIDE every policy root.
type env struct {
	workDir    string
	secretDir  string
	secretFile string
	legitFile  string
}

func newEnv(t *testing.T) env {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "work")
	secret := filepath.Join(base, "secret")
	mustMkdir(t, work)
	mustMkdir(t, secret)
	secretFile := filepath.Join(secret, "secret.txt")
	mustWrite(t, secretFile, secretSentinel+"\n")
	legitFile := filepath.Join(work, "inside.txt")
	mustWrite(t, legitFile, "inside-ok\n")
	return env{workDir: work, secretDir: secret, secretFile: secretFile, legitFile: legitFile}
}

// policy pins the standard escape-suite policy: work dir readable+writable, the
// secret dir denied, no network, a wall cap and an output cap.
func (e env) policy() sandbox.Policy {
	return sandbox.Policy{
		WorkDir:    e.workDir,
		ReadRoots:  []string{e.workDir},
		WriteRoots: []string{e.workDir},
		DenyRoots:  []string{e.secretDir},
		Network:    sandbox.NetNone,
		Limits:     sandbox.Limits{Wall: 10 * time.Second, MaxOutputBytes: 8192},
		Enforce:    true,
	}
}

func (e env) assertContainment(t *testing.T, b sandbox.Backend, cmd sandbox.Command, want, checkLeak bool) {
	t.Helper()
	res, err := b.Run(context.Background(), cmd, e.policy())
	if err != nil {
		t.Fatalf("adversary %q: unexpected infra error: %v", cmd.Shell, err)
	}
	if res.Contained() != want {
		t.Fatalf("adversary %q: Contained()=%v, want %v (exit=%d timedOut=%v violations=%v output=%q)",
			cmd.Shell, res.Contained(), want, res.ExitCode, res.TimedOut, res.Violations, res.Output)
	}
	if want && checkLeak && strings.Contains(res.Output, secretSentinel) {
		t.Fatalf("adversary %q reported contained but leaked the secret: output=%q", cmd.Shell, res.Output)
	}
}

func (e env) assertLegit(t *testing.T, b sandbox.Backend, cmd sandbox.Command) {
	t.Helper()
	res, err := b.Run(context.Background(), cmd, e.policy())
	if err != nil {
		t.Fatalf("legit twin %q: unexpected infra error: %v", cmd.Shell, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("legit twin %q must exit 0, got exit=%d timedOut=%v output=%q",
			cmd.Shell, res.ExitCode, res.TimedOut, res.Output)
	}
}

// hostHasNetwork reports whether the host itself can reach 1.1.1.1:53 outbound,
// used to decide whether the network containment assertion is meaningful.
func hostHasNetwork() bool {
	c, err := net.DialTimeout("tcp", "1.1.1.1:53", 1500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// shq single-quotes a path for POSIX sh.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
