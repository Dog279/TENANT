package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tenant/internal/procprotect"
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
func startProcess(c *exec.Cmd, stderr io.Writer, afterStart func(pid int) error, release func()) (p *Process, err error) {
	procprotect.Child(c)
	procprotect.Ordinary(func() { p, err = startProcessHere(c, stderr, afterStart, release) }) // its pipes are made here
	return p, err
}

func startProcessHere(c *exec.Cmd, stderr io.Writer, afterStart func(pid int) error, release func()) (*Process, error) {
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
	// Not c.StdoutPipe(): Wait closes that pipe the moment the process exits,
	// and the goroutine below calls Wait at once, so a command that finished
	// before its reader got to the pipe lost its output ("printf ok" came
	// back empty on a loaded CI runner). Our own pipe keeps the kernel's
	// buffer and gives EOF when the last writer is gone, and Wait leaves it
	// alone; Close shuts it after a kill so a reader never waits on a
	// grandchild that kept the write end.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		cleanup()
		return nil, err
	}
	c.Stdout = stdoutW
	tail := &tailBuffer{max: StderrTail}
	c.Stderr = tail
	if stderr != nil {
		c.Stderr = io.MultiWriter(tail, stderr)
	}
	if err := c.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		cleanup()
		return nil, fmt.Errorf("sandbox: start %s: %w", c.Path, err)
	}
	stdoutW.Close() // the child has its own copy; ours would keep the reader from EOF
	if afterStart != nil {
		if err := afterStart(c.Process.Pid); err != nil {
			_ = c.Process.Kill()
			_ = c.Wait()
			stdoutR.Close()
			cleanup()
			return nil, err
		}
	}
	p := &Process{cmd: c, stdin: stdin, stdout: &eofCloser{File: stdoutR}, stderr: tail, done: make(chan struct{}), release: release}
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
		_ = p.stdout.Close() // whatever still held the write end, the reader is done
		return nil
	case <-time.After(grace):
		return errors.New("sandbox: process did not exit after kill")
	}
}

// eofCloser is the stdout pipe's read end: it closes itself at EOF, so a
// reader that drains it leaks no descriptor, and keeps answering EOF after.
type eofCloser struct {
	*os.File
	closed atomic.Bool
}

func (e *eofCloser) Read(b []byte) (int, error) {
	if e.closed.Load() {
		return 0, io.EOF
	}
	n, err := e.File.Read(b)
	if errors.Is(err, io.EOF) && !e.closed.Swap(true) {
		_ = e.File.Close()
	}
	return n, err
}

// Close does not wait for the descriptor to close. On Windows closing a pipe
// waits for a Read in flight, and that Read returns only once the process has
// exited; the MCP stdio transport closes stdout before the stdin whose Close
// ends the process, so waiting here deadlocked a server that ignores EOF on
// its stdin. The close finishes on its own as soon as that Read returns.
func (e *eofCloser) Close() error {
	if !e.closed.Swap(true) {
		go e.File.Close()
	}
	return nil
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
// (virtual, ulimit -v) and CPU time (ulimit -t) before exec'ing it, for a
// backend whose confinement runs a Unix userland (Seatbelt). A cap the shell
// cannot set fails the start: the process never runs without a limit the
// operator asked for. There is no process cap here: ulimit's counts every
// process the user runs, not this one's. No caps: argv unchanged.
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
// Policy sets them. Process caps are among them only for the Job Object:
// elsewhere ulimit counts every process the user runs, so it cannot cap one
// server.
func StartLimits(b Backend) []string {
	switch b.Name() {
	case "host":
		return nil
	case "jobobject":
		return []string{"memory", "cpu", "processes"}
	}
	return []string{"memory", "cpu"}
}
