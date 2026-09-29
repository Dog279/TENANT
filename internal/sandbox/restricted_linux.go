//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

func init() { registerRestricted(newLinuxRestricted) }

// bwrapPath is the bubblewrap whose self-check passed. Only success is cached,
// so a host that gains bubblewrap or user namespaces later gets a working
// backend without a restart.
var (
	bwrapMu   sync.Mutex
	bwrapPath string
)

// newLinuxRestricted returns the Linux restricted backend, or an error naming
// why this host cannot have one: bubblewrap is not installed, or it cannot
// create its namespaces here. The caller then fails closed rather than running
// the command unconfined. There is no Landlock fallback: bubblewrap only, and
// no new dependency (decision 2026-09-29).
func newLinuxRestricted() (Backend, error) {
	bwrapMu.Lock()
	defer bwrapMu.Unlock()
	if bwrapPath == "" {
		path, err := exec.LookPath("bwrap")
		if err != nil {
			return nil, errors.New("sandbox: bubblewrap not installed: restricted unavailable (install the bubblewrap package)")
		}
		if err := bwrapSelfCheck(path); err != nil {
			return nil, err
		}
		bwrapPath = path
	}
	return &linuxRestricted{bwrap: bwrapPath}, nil
}

// bwrapSelfCheck runs an empty command in the sandbox Run builds (the host
// read-only, fresh namespaces, no network). bwrap on disk proves nothing: with
// unprivileged user namespaces disabled, or restricted by AppArmor as on Ubuntu
// 23.10+, every command would fail, so this names the cause up front instead.
func bwrapSelfCheck(bwrap string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bwrap, bwrapArgs(Policy{Network: NetNone}, "/bin/sh", []string{"-c", ":"})...).CombinedOutput()
	if err == nil {
		return nil
	}
	why := strings.TrimSpace(string(out))
	if why == "" {
		why = err.Error()
	}
	if hint := usernsHint(os.ReadFile); hint != "" {
		why += " (" + hint + ")"
	}
	return fmt.Errorf("sandbox: bubblewrap cannot create a sandbox on this host: restricted unavailable: %s", why)
}

// linuxRestricted confines a command with bubblewrap. The argv is built by the
// pure bwrapArgs (spec.go) and the caps by bwrapLimitArgv (process.go), so the
// interesting logic is unit-tested off-Linux; this file is the exec wrapper.
type linuxRestricted struct{ bwrap string }

func (*linuxRestricted) Name() string         { return "restricted" }
func (*linuxRestricted) Isolation() Isolation { return IsolationProcess }

func (b *linuxRestricted) Probe(context.Context) Capabilities {
	limits, root := EnforceFull, ""
	if os.Geteuid() == 0 {
		limits, root = EnforcePartial, "; Tenant runs as root, which the kernel exempts from the process cap"
	}
	return Capabilities{
		Backend:    "restricted",
		Isolation:  IsolationProcess,
		Filesystem: EnforceFull, // bind/tmpfs confinement, deny masks read-only, capabilities dropped
		Network:    EnforceFull, // NetNone is a fresh network namespace
		Limits:     limits,      // wall, output, and rlimits inside the sandbox
		Env:        EnforceFull, // the env allowlist is honoured via materialize
		Shell:      "sh",
		Detail: "bubblewrap (" + b.bwrap + "): the host read-only without /run (or a system+ReadRoots allowlist), " +
			"rw WorkDir/WriteRoots, read-only masks over DenyRoots, no capabilities, fresh pid/ipc/uts namespaces " +
			"(nothing outlives the command), network cut with --unshare-net (NetAllowlist fails closed); " +
			"memory (address space), CPU time and processes (counted per sandbox) capped with rlimits; " +
			"write and network refusals are attributed, reads of masked roots are not; " +
			"a unix socket the sandbox can see stays connectable" + root,
	}
}

// Start runs argv under bubblewrap with the same confinement as Run, its
// memory, CPU time and processes capped by the shell that execs it
// (bwrapLimitArgv).
func (b *linuxRestricted) Start(ctx context.Context, argv []string, pol Policy) (*Process, error) {
	argv = bwrapLimitArgv(argv, pol.Limits)
	c := exec.CommandContext(ctx, b.bwrap, bwrapArgs(pol, argv[0], argv[1:])...)
	c.Env = pol.Env.materialize(os.Environ())
	return startProcess(c, pol.Stderr, nil, nil)
}

func (b *linuxRestricted) Run(ctx context.Context, cmd Command, pol Policy) (Result, error) {
	if pol.Limits.Wall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pol.Limits.Wall)
		defer cancel()
	}

	shell, args := shellArgs(cmd)
	argv := bwrapLimitArgv(append([]string{shell}, args...), pol.Limits)
	c := exec.CommandContext(ctx, b.bwrap, bwrapArgs(pol, argv[0], argv[1:])...)
	if strings.TrimSpace(cmd.Stdin) != "" {
		c.Stdin = strings.NewReader(cmd.Stdin)
	}
	// cwd is handled INSIDE the sandbox by bwrap's --chdir; we do not set c.Dir
	// (the bwrap process itself runs from Tenant's own cwd).
	c.Env = pol.Env.materialize(os.Environ())
	// Nothing needs reaping here: when the command exits so does bwrap, and with
	// it the PID namespace and whatever the command left running in it.
	out := &headBuffer{max: pol.Limits.MaxOutputBytes}
	c.Stdout, c.Stderr = out, out

	err := c.Run()
	res := Result{Output: out.String(), PID: pidOf(c)}

	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		res.ExitStatus = ee.Error()
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		if res.ExitCode == 0 {
			res.ExitCode = -1
		}
		return res, nil
	}
	if err != nil {
		if ee == nil {
			// Could not start bwrap at all (e.g. it vanished after Probe): infra error.
			return res, err
		}
		if v, ok := attributeBwrapDenial(res.Output, pol); ok {
			res.Violations = append(res.Violations, v)
		}
	}
	return res, nil // a non-zero exit is signal, not an infra error
}
