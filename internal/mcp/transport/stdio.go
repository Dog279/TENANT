package transport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
)

// DefaultMaxFrameSize caps a single inbound frame. Override per
// instance via WithMaxFrameSize.
const DefaultMaxFrameSize = 4 * 1024 * 1024

// Stdio is the newline-delimited JSON transport. Three constructors
// cover the deployment shapes we care about:
//
//   - NewStdioSelf:    Tenant running AS an MCP server (talks via os.Stdin/Stdout)
//   - NewStdioStreams: bring-your-own io.ReadCloser/WriteCloser (tests, pipes)
//   - NewStdioProcess: spawn a child process and pipe to its stdio
type Stdio struct {
	in  io.ReadCloser
	out io.WriteCloser
	cmd *exec.Cmd // nil unless spawned via NewStdioProcess

	br      *bufio.Reader
	wmu     sync.Mutex // serializes Send
	maxSize int
	closed  atomic.Bool
}

// StdioOption configures a Stdio transport at construction.
type StdioOption func(*Stdio)

// WithMaxFrameSize overrides DefaultMaxFrameSize.
func WithMaxFrameSize(n int) StdioOption {
	return func(s *Stdio) { s.maxSize = n }
}

// NewStdioSelf wires the transport to the current process's stdio.
// Use this when Tenant is invoked as an MCP server by a parent agent.
func NewStdioSelf(opts ...StdioOption) *Stdio {
	return newStdio(os.Stdin, os.Stdout, nil, opts...)
}

// NewStdioStreams wires the transport to caller-supplied streams.
// Useful for in-memory testing via io.Pipe.
func NewStdioStreams(r io.ReadCloser, w io.WriteCloser, opts ...StdioOption) *Stdio {
	return newStdio(r, w, nil, opts...)
}

// NewStdioProcess spawns cmd and connects its stdio to the transport.
// The caller MUST eventually call Close to reap the child. cmd.Stderr
// is wired to os.Stderr by default for observability — override before
// calling if you need to capture it.
func NewStdioProcess(cmd *exec.Cmd, opts ...StdioOption) (*Stdio, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	return newStdio(stdout, stdin, cmd, opts...), nil
}

func newStdio(r io.ReadCloser, w io.WriteCloser, cmd *exec.Cmd, opts ...StdioOption) *Stdio {
	s := &Stdio{
		in:      r,
		out:     w,
		cmd:     cmd,
		br:      bufio.NewReaderSize(r, 64*1024),
		maxSize: DefaultMaxFrameSize,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Send writes frame plus a trailing newline if absent. Frames are
// serialized through wmu so concurrent callers compose safely.
func (s *Stdio) Send(_ context.Context, frame []byte) error {
	if s.closed.Load() {
		return ErrClosed
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if _, err := s.out.Write(frame); err != nil {
		return err
	}
	if len(frame) == 0 || frame[len(frame)-1] != '\n' {
		if _, err := s.out.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	return nil
}

// Recv reads one newline-terminated frame. ctx cancellation is
// advisory: stdio reads cannot be interrupted, so cancellation
// only takes effect on the next Recv unless Close is called.
func (s *Stdio) Recv(_ context.Context) ([]byte, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	line, err := s.br.ReadBytes('\n')
	if err != nil {
		if s.closed.Load() {
			return nil, ErrClosed
		}
		return nil, err
	}
	if len(line) > s.maxSize {
		return nil, ErrFrameTooLarge
	}
	// Trim trailing newline before returning — the session works
	// with bare JSON, framing is the transport's secret.
	return bytes.TrimRight(line, "\r\n"), nil
}

// Close terminates the transport. Idempotent. If the transport was
// constructed via NewStdioProcess, the child process is reaped.
func (s *Stdio) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	var errs []error
	if err := s.in.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := s.out.Close(); err != nil {
		errs = append(errs, err)
	}
	if s.cmd != nil {
		if err := s.cmd.Wait(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
