package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Process is a long-lived command started under a Policy with open pipes:
// a local MCP server speaking over its stdin and stdout (TEN-318). Run is for
// commands that finish; Start is for one that serves. The boundary is the
// same: a Starter backend applies the Policy it applies to Run.
type Process struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  *tailBuffer
	done    chan struct{}
	err     error  // Wait's result; set before done closes
	release func() // the backend's cleanup once the process has exited
	killed  atomic.Bool
}

// Starter is a backend that can start a long-lived process.
type Starter interface {
	Start(ctx context.Context, argv []string, pol Policy) (*Process, error)
}

// StderrTail is how much of a process's stderr Stderr keeps: its last bytes,
// for the error a failed server reports.
const StderrTail = 8 << 10

// Start starts argv on b under pol. The process lives until it exits, ctx
// ends, or Close. A backend that cannot run a long-lived process is an error.
func Start(ctx context.Context, b Backend, argv []string, pol Policy) (*Process, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil, errors.New("sandbox: empty command")
	}
	s, ok := b.(Starter)
	if !ok {
		return nil, fmt.Errorf("sandbox: the %s backend cannot run a long-lived process", b.Name())
	}
	return s.Start(ctx, argv, pol)
}

// startProcess wires the pipes, starts c, runs afterStart (the Windows job
// assignment) and waits for the exit in the background. stderr, when not
// nil, also gets what the process writes to stderr (Policy.Stderr). release
// runs once the process has exited, or at once when it fails to start.
func startProcess(c *exec.Cmd, stderr io.Writer, afterStart func(pid int) error, release func()) (*Process, error) {
	cleanup := func() {
		if release != nil {
			release()
		}
	}
	stdin, err := c.StdinPipe()
	if err != nil {
		cleanup()
		return nil, err
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		cleanup()
		return nil, err
	}
	tail := &tailBuffer{max: StderrTail}
	c.Stderr = tail
	if stderr != nil {
		c.Stderr = io.MultiWriter(tail, stderr)
	}
	if err := c.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("sandbox: start %s: %w", c.Path, err)
	}
	if afterStart != nil {
		if err := afterStart(c.Process.Pid); err != nil {
			_ = c.Process.Kill()
			_ = c.Wait()
			cleanup()
			return nil, err
		}
	}
	p := &Process{cmd: c, stdin: stdin, stdout: stdout, stderr: tail, done: make(chan struct{}), release: release}
	go func() {
		p.err = c.Wait()
		cleanup()
		close(p.done)
	}()
	return p, nil
}

// Stdin is the process's standard input.
func (p *Process) Stdin() io.WriteCloser { return p.stdin }

// Stdout is the process's standard output.
func (p *Process) Stdout() io.ReadCloser { return p.stdout }

// Stderr is the last StderrTail bytes the process wrote to stderr.
func (p *Process) Stderr() string { return p.stderr.String() }

// Pid is the started process's id: the backend's launcher, where it has one.
func (p *Process) Pid() int { return p.cmd.Process.Pid }

// Done is closed once the process has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait waits for the process to exit and returns how it did.
func (p *Process) Wait() error {
	<-p.done
	return p.err
}

// Killed reports whether Close had to kill the process after its grace.
func (p *Process) Killed() bool { return p.killed.Load() }

// Close ends the process the way the MCP stdio transport says to: close its
// stdin, give it grace to exit, then kill it. It returns once the process has
// exited, or an error if even a kill did not end it within grace.
func (p *Process) Close(grace time.Duration) error {
	_ = p.stdin.Close()
	select {
	case <-p.done:
		return nil
	case <-time.After(grace):
	}
	p.killed.Store(true)
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
		return nil
	case <-time.After(grace):
		return errors.New("sandbox: process did not exit after kill")
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// posixLimitArgv wraps argv in a POSIX shell that caps the process's memory
// (virtual, ulimit -v) and CPU time (ulimit -t) before exec'ing it, for the
// backends whose confinement runs a Unix userland (bubblewrap, Seatbelt,
// WSL). A cap the shell cannot set fails the start: the process never runs
// without a limit the operator asked for. No caps: argv unchanged.
func posixLimitArgv(argv []string, l Limits) []string {
	var steps []string
	if l.MemoryBytes > 0 {
		kb := (l.MemoryBytes + 1023) / 1024
		steps = append(steps, "ulimit -v "+strconv.FormatInt(kb, 10)+" || exit 125")
	}
	if l.CPU > 0 {
		secs := int64((l.CPU + time.Second - 1) / time.Second)
		steps = append(steps, "ulimit -t "+strconv.FormatInt(secs, 10)+" || exit 125")
	}
	if len(steps) == 0 {
		return argv
	}
	script := strings.Join(steps, "; ") + `; exec "$@"`
	return append([]string{"/bin/sh", "-c", script, "sh"}, argv...)
}

// StartLimits names the resource caps a backend's Start applies when a
// Policy sets them. Process caps are not among them on a POSIX backend:
// ulimit counts every process the user runs, so it cannot cap one server.
func StartLimits(b Backend) []string {
	switch b.Name() {
	case "host":
		return nil
	case "jobobject":
		return []string{"memory", "cpu", "processes"}
	}
	return []string{"memory", "cpu"}
}
