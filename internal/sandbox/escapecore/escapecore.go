// Package escapecore is the testing-free core of Tenant's sandbox escape suite:
// the case table, the per-case filesystem (a work dir plus a SECRET placed
// OUTSIDE every policy root), the standard escape policy, and Evaluate, which
// runs every adversarial case and its legitimate twin against a
// sandbox.Backend and reports what happened.
//
// It deliberately does not import "testing", so one suite backs both the go
// test harness (internal/sandbox/escapetest.Harness) and the operator's
// `tenant sandbox selftest`, which runs it against the live backends on the
// box itself.
//
// Honesty rules shared by both consumers:
//
//   - A case is contained only when the backend stopped the command (a recorded
//     Violation, a non-zero exit, or a timeout) AND, for a case that reads the
//     secret, the secret's sentinel did not come back in the output.
//   - The network case is the only one that needs connectivity. Its adversarial
//     half is Skipped (with a reason) only when the host itself has no outbound
//     network; the filesystem cases are never skipped.
//   - Every legitimate twin must exit 0 on any backend: a boundary that "contains"
//     escapes by breaking legitimate work is not a pass.
package escapecore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tenant/internal/sandbox"
)

// Kind values name the policy dimension a case exercises. They match
// sandbox.Violation.Kind and the sandbox.Capabilities field a backend's Probe
// reports for that dimension, so a consumer can hold each case to the backend's
// own claim.
const (
	KindFilesystem = "filesystem"
	KindNetwork    = "network"
)

// SecretSentinel is the marker written into the out-of-roots secret file. A
// contained read case never returns it; seeing it in output means the secret
// escaped regardless of exit status.
const SecretSentinel = "TOP-SECRET-DO-NOT-LEAK-9f3a1c7e"

// suiteSurface tags every command the suite runs (sandbox.Command.Surface).
const suiteSurface = "escapetest"

// homeArtifact is the file the write-outside-roots adversary tries to create in
// $HOME. Evaluate removes it after that case whether or not the write landed.
const homeArtifact = "tenant_sbx_escape"

// netProbeAddr is the outbound target of the network case and of the host
// connectivity pre-check that decides whether that case can be evaluated.
const netProbeAddr = "1.1.1.1:53"

// CaseResult is the outcome of one escape case against one backend.
type CaseResult struct {
	Name string `json:"name"` // stable case id, e.g. "read-secret-outside-roots"
	Kind string `json:"kind"` // policy dimension: KindFilesystem | KindNetwork

	// AdversarialContained: the backend stopped the escape (a Violation, a
	// non-zero exit, or a timeout) and, for a read case, the secret did not leak.
	AdversarialContained bool   `json:"adversarial_contained"`
	AdversarialOutput    string `json:"adversarial_output"`
	// LegitOK: the legitimate twin exited 0 (it must, on every backend).
	LegitOK     bool   `json:"legit_ok"`
	LegitOutput string `json:"legit_output"`

	// Skipped: the adversarial half was not evaluated (the legitimate twin still
	// ran). Only the network case is ever skipped, and only when the host itself
	// has no outbound network.
	Skipped    bool   `json:"skipped"`
	SkipReason string `json:"skip_reason,omitempty"`

	// Diagnostics for failure messages and the operator CLI.
	AdversarialCommand    string              `json:"adversarial_command,omitempty"`
	AdversarialExit       int                 `json:"adversarial_exit"`
	AdversarialTimedOut   bool                `json:"adversarial_timed_out"`
	AdversarialViolations []sandbox.Violation `json:"adversarial_violations,omitempty"`
	LegitCommand          string              `json:"legit_command"`
	LegitExit             int                 `json:"legit_exit"`
}

// escapeCase is one adversarial probe plus its legitimate twin. Commands are
// POSIX sh, run through the backend's platform shell exactly as os_exec and
// cron-shell commands are.
type escapeCase struct {
	name        string
	kind        string
	checkLeak   bool // the adversary reads the secret: a returned sentinel is an escape
	needsNet    bool // skip the adversarial half when the host has no outbound network
	advOrigin   string
	legitOrigin string
	adversary   func(e env) string
	legit       func(e env) string
	cleanup     func() // optional: remove what an uncontained adversary leaves behind
}

// cases is the suite, in run order.
var cases = []escapeCase{
	// (a) read a secret file OUTSIDE all roots vs read a file inside the work dir.
	{
		name: "read-secret-outside-roots", kind: KindFilesystem, checkLeak: true,
		advOrigin: "read-secret", legitOrigin: "read-inside",
		adversary: func(e env) string { return "cat " + shq(e.secretFile) },
		legit:     func(e env) string { return "cat " + shq(e.legitFile) },
	},
	// (b) write to $HOME (outside the write roots) vs write inside the work dir.
	{
		name: "write-outside-roots", kind: KindFilesystem,
		advOrigin: "write-home", legitOrigin: "write-inside",
		adversary: func(env) string { return `echo pwned > "$HOME/` + homeArtifact + `"` },
		legit: func(e env) string {
			return "echo hi > " + shq(filepath.Join(e.workDir, "legit_write.txt"))
		},
		cleanup: removeHomeArtifact,
	},
	// (c) path-escape the work dir with ".." vs a plain echo.
	{
		name: "path-escape-dotdot", kind: KindFilesystem, checkLeak: true,
		advOrigin: "dotdot", legitOrigin: "echo",
		adversary: func(e env) string {
			escape := e.workDir + "/../" + filepath.Base(e.secretDir) + "/" + filepath.Base(e.secretFile)
			return "cat " + shq(escape)
		},
		legit: func(env) string { return "echo ok" },
	},
	// (d) reach the network vs a plain echo. The adversarial half is skipped when
	// the host has no outbound network (the filesystem cases above never are).
	{
		name: "network-outbound", kind: KindNetwork, needsNet: true,
		advOrigin: "net", legitOrigin: "echo",
		adversary: func(env) string { return "exec 3<>/dev/tcp/1.1.1.1/53 && echo connected" },
		legit:     func(env) string { return "echo ok" },
	},
}

// Evaluate runs every escape case against b: for each, a fresh temp work dir and
// a secret dir outside every root, the standard escape Policy, the adversarial
// command (unless Skipped), then its legitimate twin. Temp dirs are removed
// before it returns.
//
// A non-nil error means the suite could not finish: the backend failed to start
// a command (an infrastructure failure, never a mere non-zero exit), a temp dir
// could not be built, or ctx was cancelled (an interrupted run must not read as
// containment). The results completed before the failure are still returned.
func Evaluate(ctx context.Context, b sandbox.Backend) ([]CaseResult, error) {
	if b == nil {
		return nil, errors.New("escapecore: nil backend")
	}
	results := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		r, err := runCase(ctx, b, c)
		if err != nil {
			return results, fmt.Errorf("escape case %s: %w", c.name, err)
		}
		results = append(results, r)
	}
	return results, nil
}

func runCase(ctx context.Context, b sandbox.Backend, c escapeCase) (CaseResult, error) {
	e, err := newEnv()
	if err != nil {
		return CaseResult{}, err
	}
	defer e.remove()
	if c.cleanup != nil {
		defer c.cleanup()
	}
	pol := e.policy()
	r := CaseResult{Name: c.name, Kind: c.kind}

	if c.needsNet && !hostHasNetwork(ctx) {
		r.Skipped = true
		r.SkipReason = "host has no outbound TCP to " + netProbeAddr +
			"; only the network containment check is skipped (filesystem cases are still enforced)"
	} else {
		cmd := sandbox.Command{Shell: c.adversary(e), Surface: suiteSurface, Origin: c.advOrigin}
		res, err := run(ctx, b, cmd, pol)
		if err != nil {
			return r, fmt.Errorf("adversary %q: %w", cmd.Shell, err)
		}
		leaked := c.checkLeak && strings.Contains(res.Output, SecretSentinel)
		r.AdversarialContained = res.Contained() && !leaked
		r.AdversarialOutput = res.Output
		r.AdversarialCommand = cmd.Shell
		r.AdversarialExit = res.ExitCode
		r.AdversarialTimedOut = res.TimedOut
		r.AdversarialViolations = res.Violations
	}

	legit := sandbox.Command{Shell: c.legit(e), Surface: suiteSurface, Origin: c.legitOrigin}
	res, err := run(ctx, b, legit, pol)
	if err != nil {
		return r, fmt.Errorf("legit twin %q: %w", legit.Shell, err)
	}
	r.LegitOK = res.ExitCode == 0 && !res.TimedOut
	r.LegitOutput = res.Output
	r.LegitCommand = legit.Shell
	r.LegitExit = res.ExitCode
	return r, nil
}

// run executes one command, turning a cancelled ctx into an error: backends
// report a killed child as a non-zero exit, which would otherwise read as
// containment.
func run(ctx context.Context, b sandbox.Backend, cmd sandbox.Command, pol sandbox.Policy) (sandbox.Result, error) {
	res, err := b.Run(ctx, cmd, pol)
	if cerr := ctx.Err(); cerr != nil {
		return res, fmt.Errorf("interrupted: %w", cerr)
	}
	if err != nil {
		return res, fmt.Errorf("backend %s could not run it: %w", b.Name(), err)
	}
	return res, nil
}

// env is one case's isolated filesystem: a work dir plus a secret dir that sits
// OUTSIDE every policy root, both under a private temp base.
type env struct {
	base       string
	workDir    string
	secretDir  string
	secretFile string
	legitFile  string
}

func newEnv() (env, error) {
	base, err := os.MkdirTemp("", "tenant-escape-")
	if err != nil {
		return env{}, fmt.Errorf("create temp dir: %w", err)
	}
	e := env{
		base:      base,
		workDir:   filepath.Join(base, "work"),
		secretDir: filepath.Join(base, "secret"),
	}
	e.secretFile = filepath.Join(e.secretDir, "secret.txt")
	e.legitFile = filepath.Join(e.workDir, "inside.txt")
	if err := e.populate(); err != nil {
		e.remove()
		return env{}, err
	}
	return e, nil
}

func (e env) populate() error {
	for _, dir := range []string{e.workDir, e.secretDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	files := []struct{ path, content string }{
		{e.secretFile, SecretSentinel + "\n"},
		{e.legitFile, "inside-ok\n"},
	}
	for _, f := range files {
		if err := os.WriteFile(f.path, []byte(f.content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", f.path, err)
		}
	}
	return nil
}

func (e env) remove() { _ = os.RemoveAll(e.base) }

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

// removeHomeArtifact deletes the file the write-outside-roots adversary may have
// created in $HOME.
func removeHomeArtifact() {
	if home, err := os.UserHomeDir(); err == nil {
		_ = os.Remove(filepath.Join(home, homeArtifact))
	}
}

// hostHasNetwork reports whether the host itself can reach netProbeAddr
// outbound, which decides whether the network case's containment check is
// meaningful. A variable so tests can pin it without real connectivity.
var hostHasNetwork = func(ctx context.Context) bool {
	d := net.Dialer{Timeout: 1500 * time.Millisecond}
	c, err := d.DialContext(ctx, "tcp", netProbeAddr)
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
