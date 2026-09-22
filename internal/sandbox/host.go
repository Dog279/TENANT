package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// hostBackend runs a command directly on the host with NO filesystem or network
// confinement — today's behaviour, named honestly. It still honours the cheap,
// portable parts of a Policy: the working directory, an environment allowlist, a
// wall-clock deadline, and an output cap. Probe reports that nothing stronger is
// enforced, so the security audit never overstates the boundary.
type hostBackend struct{}

// Host returns the unconfined host backend. It is the default for every surface
// until a profile opts into a stronger backend, so wiring os_exec/cron through
// the seam with the host backend is a behavioural no-op.
func Host() Backend { return hostBackend{} }

func (hostBackend) Name() string         { return "host" }
func (hostBackend) Isolation() Isolation { return IsolationNone }

func (hostBackend) Probe(context.Context) Capabilities {
	return Capabilities{
		Backend:    "host",
		Isolation:  IsolationNone,
		Filesystem: EnforceNone,
		Network:    EnforceNone,
		Limits:     EnforcePartial, // wall-timeout + output cap only
		Env:        EnforceFull,    // the env allowlist is honoured
		Detail:     "no filesystem or network confinement; wall-timeout, output cap and env allowlist only",
	}
}

func (hostBackend) Run(ctx context.Context, cmd Command, pol Policy) (Result, error) {
	if pol.Limits.Wall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pol.Limits.Wall)
		defer cancel()
	}

	name, args := shellArgs(cmd)
	c := exec.CommandContext(ctx, name, args...)
	if strings.TrimSpace(cmd.Stdin) != "" {
		c.Stdin = strings.NewReader(cmd.Stdin)
	}
	if pol.WorkDir != "" {
		c.Dir = pol.WorkDir
	}
	c.Env = pol.Env.materialize(os.Environ())

	out, err := c.CombinedOutput()
	res := Result{Output: clip(string(out), pol.Limits.MaxOutputBytes)}

	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		res.ExitStatus = ee.Error() // "exit status N" / "signal: killed", as exec reports it
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		if res.ExitCode == 0 {
			res.ExitCode = -1
		}
		return res, nil
	}
	if err != nil && ee == nil {
		// Could not start the process at all: an infrastructure failure.
		return res, err
	}
	return res, nil // a non-zero exit is signal, not an infra error
}
