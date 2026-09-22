//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func init() { registerRestricted(newLinuxRestricted) }

// newLinuxRestricted returns the Linux restricted backend, or an error when
// bubblewrap is not installed — the caller then fails closed on unattended
// surfaces rather than silently running unconfined. We deliberately take NO
// go-landlock dependency in this pass; Landlock LSM tightening layered on top of
// bubblewrap is TODO(TEN-346).
func newLinuxRestricted() (Backend, error) {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("sandbox: restricted backend unavailable: bubblewrap (bwrap) not on PATH: %w", err)
	}
	return &linuxRestricted{bwrap: path}, nil
}

// linuxRestricted confines a command with bubblewrap: a whole-host read-only
// bind, an ephemeral /tmp, read-write binds for WorkDir + WriteRoots, tmpfs
// masks over DenyRoots, and (for NetNone) a fresh network namespace. The argv is
// built by the pure bwrapArgs (spec.go), so the interesting logic is unit-tested
// off-Linux; this file is only the thin exec wrapper.
type linuxRestricted struct{ bwrap string }

func (*linuxRestricted) Name() string         { return "restricted" }
func (*linuxRestricted) Isolation() Isolation { return IsolationProcess }

func (*linuxRestricted) Probe(context.Context) Capabilities {
	return Capabilities{
		Backend:    "restricted",
		Isolation:  IsolationProcess,
		Filesystem: EnforceFull,    // bubblewrap bind/tmpfs confinement is present
		Network:    EnforceFull,    // NetNone is fully cut via --unshare-net
		Limits:     EnforcePartial, // wall-timeout + output cap only; rlimits/cgroups TODO
		Env:        EnforceFull,    // the env allowlist is honoured via materialize
		Detail: "bubblewrap: whole-host ro-bind, rw WorkDir/WriteRoots, tmpfs-masked DenyRoots, " +
			"network cut with --unshare-net; TODO(TEN-346): Landlock LSM, NetAllowlist, rlimit/cgroup caps",
	}
}

func (b *linuxRestricted) Run(ctx context.Context, cmd Command, pol Policy) (Result, error) {
	if pol.Limits.Wall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pol.Limits.Wall)
		defer cancel()
	}

	shell, args := shellArgs(cmd)
	c := exec.CommandContext(ctx, b.bwrap, bwrapArgs(pol, shell, args)...)
	if strings.TrimSpace(cmd.Stdin) != "" {
		c.Stdin = strings.NewReader(cmd.Stdin)
	}
	// cwd is handled INSIDE the sandbox by bwrap's --chdir; we do not set c.Dir
	// (the bwrap process itself runs from Tenant's own cwd).
	c.Env = pol.Env.materialize(os.Environ())

	// TODO(TEN-346): apply rlimits to the child via SysProcAttr — RLIMIT_AS from
	// Limits.MemoryBytes, RLIMIT_CPU from Limits.CPU, RLIMIT_NPROC from
	// Limits.MaxPIDs — and/or a cgroup v2 scope. bubblewrap does not itself cap
	// memory/CPU/PIDs, which is why Probe reports Limits as EnforcePartial.

	out, err := c.CombinedOutput()
	res := Result{Output: clip(string(out), pol.Limits.MaxOutputBytes)}

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
		if ee != nil {
			return res, nil // a non-zero exit is signal, not an infra error
		}
		// Could not start bwrap at all (e.g. it vanished after Probe): infra error.
		return res, err
	}
	return res, nil
}
