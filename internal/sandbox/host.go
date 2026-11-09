package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
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
		Shell:      shellName(runtime.GOOS),
		Detail:     "no filesystem or network confinement; wall-timeout, output cap and env allowlist only",
	}
}

// Start runs argv on the host, unconfined, in the policy's work dir with its
// environment allowlist. It applies no resource caps.
func (hostBackend) Start(ctx context.Context, argv []string, pol Policy) (*Process, error) {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if pol.WorkDir != "" {
		c.Dir = pol.WorkDir
	}
	c.Env = pol.Env.materialize(os.Environ())
	detach(c) // its own session, as Run's commands get: no /dev/tty for sudo to grab (TEN-489)
	return startProcess(c, pol.Stderr, nil, nil)
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
	detach(c)
	c.WaitDelay = time.Second // a backgrounded child holding the pipe can't hang the call

	// Output is bounded as it arrives (TEN-438): the same head+tail shape clip
	// gave, without ever holding the whole stream.
	out := &headTailBuffer{max: pol.Limits.MaxOutputBytes}
	c.Stdout, c.Stderr = out, out
	err := c.Run()
	res := Result{Output: out.String(), PID: pidOf(c)}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The command finished; something it started in the background still
		// holds the pipe. It keeps running on the host (unlike the sandbox,
		// which reaps it), but nothing reads its output any more.
		err = nil
		res.Output += "\n[a background process is still running; its later output is not captured]"
	}

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
