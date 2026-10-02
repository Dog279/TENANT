package osys

// process.go (TEN-490): commands that outlive a tool call. os_exec can start a
// command in the background (background: true), and a foreground command
// still running at its timeout is moved there instead of being killed, so a
// long install or a dev server survives the turn. os_process then lists,
// reads, waits for, feeds and kills them. Output goes to <data>/proc/<id>.log,
// which the model pages like a file. Processes die with Tenant (Close), and a
// finished one is forgotten an hour after it exited.
//
// The service never spawns these itself: cmd/tenant's starter runs them under
// the turn's exec profile, on the host only (a confined backend reaps what it
// started, so there ErrNoBackground keeps the old kill-at-timeout runner).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Proc is a started command the service watches, feeds and stops: what
// sandbox.Process offers, named here so osys needs no sandbox import.
type Proc interface {
	Pid() int
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Done() <-chan struct{}
	Wait() error
	Close(grace time.Duration) error
}

// starter starts command for the turn in ctx (its surface and workdir) with
// stderr going to log; the service reads stdout itself. ErrNoBackground means
// this surface's backend cannot, and os_exec runs the command the old way.
type starter func(ctx context.Context, command string, log io.Writer) (Proc, error)

// ErrNoBackground is a starter's answer on a confined surface.
var ErrNoBackground = errors.New("background processes run on the host only; this surface runs commands in a sandbox")

const (
	procExpiry = time.Hour        // a finished process is forgotten this long after it exited
	procGrace  = 2 * time.Second  // kill: stdin closes, then SIGKILL after this
	waitMax    = 10 * time.Minute // the most os_process wait may block
	logTail    = 200              // os_process log with no offset: the last lines
)

type proc struct {
	ID, Command, Log string
	PID              int
	Started          time.Time
	p                Proc
	pumped           chan struct{} // closed once stdout is drained and the exit recorded
	mu               sync.Mutex
	exited           time.Time
	status           string // "" while running, else exec's text: "exit status 2", "signal: killed"
}

func (p *proc) state() (status string, exited time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status, p.exited
}

// launch starts command under the service's starter and pumps its output to
// a log beside the others. The process is not tied to ctx's cancellation:
// the caller decides what a cancelled turn means (Exec kills a foreground
// command; a background one goes on).
func (s *Service) launch(ctx context.Context, command string) (*proc, error) {
	if s.start == nil {
		return nil, ErrNoBackground
	}
	if err := os.MkdirAll(s.procDir, 0o700); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("%06x", rand.Uint32()&0xffffff) // ponytail: 24 bits; collisions are refused below
	logPath := filepath.Join(s.procDir, id+".log")
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	p, err := s.start(context.WithoutCancel(ctx), command, f)
	if err != nil {
		f.Close()
		os.Remove(logPath)
		return nil, err
	}
	pr := &proc{ID: id, Command: command, Log: logPath, PID: p.Pid(), Started: time.Now(), p: p, pumped: make(chan struct{})}
	s.mu.Lock()
	s.procs[id] = pr
	s.mu.Unlock()
	go func() {
		_, _ = io.Copy(f, p.Stdout())
		err := p.Wait()
		f.Close()
		pr.mu.Lock()
		pr.exited = time.Now()
		pr.status = ""
		if err != nil {
			pr.status = err.Error()
		}
		pr.mu.Unlock()
		close(pr.pumped)
	}()
	return pr, nil
}

// forget drops a process from the registry and removes its log.
func (s *Service) forget(pr *proc) {
	s.mu.Lock()
	delete(s.procs, pr.ID)
	s.mu.Unlock()
	os.Remove(pr.Log)
}

// Background starts command and returns at once (os_exec background: true).
func (s *Service) Background(ctx context.Context, command string) (string, error) {
	pr, err := s.launch(ctx, command)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("started %s in the background (pid %d); log: %s\nos_process log|wait|kill %s follows it", pr.ID, pr.PID, pr.Log, pr.ID), nil
}

// execStarted is Exec on the starter path: wait for the command up to
// timeout, then leave it running in the background with its id.
func (s *Service) execStarted(ctx context.Context, pr *proc, timeout time.Duration) (string, error) {
	select {
	case <-pr.pumped:
		out := s.tail(pr, maxOutputBytes)
		status, _ := pr.state()
		s.forget(pr) // a finished foreground command is its tool result
		if status != "" {
			return out, errors.New(status)
		}
		return out, nil
	case <-ctx.Done():
		_ = pr.p.Close(procGrace) // the turn ended: a foreground command does not outlive it
		<-pr.pumped
		out := s.tail(pr, maxOutputBytes)
		s.forget(pr)
		return out, ctx.Err()
	case <-time.After(timeout):
		return s.tail(pr, maxOutputBytes) + fmt.Sprintf("\n[still running after %s; moved to the background as %s — os_process log|wait|kill %s]", timeout, pr.ID, pr.ID), nil
	}
}

// tail is the last n bytes of a process's log so far, head and tail kept.
func (s *Service) tail(pr *proc, n int) string {
	b, err := os.ReadFile(pr.Log)
	if err != nil {
		return ""
	}
	return clipMiddle(string(b), n)
}

// prune forgets processes that exited more than procExpiry ago.
func (s *Service) prune() {
	s.mu.Lock()
	var old []*proc
	for _, pr := range s.procs {
		if _, exited := pr.state(); !exited.IsZero() && time.Since(exited) > procExpiry {
			old = append(old, pr)
		}
	}
	s.mu.Unlock()
	for _, pr := range old {
		s.forget(pr)
	}
}

func (s *Service) find(id string) (*proc, error) {
	s.prune()
	s.mu.Lock()
	pr := s.procs[strings.TrimSpace(id)]
	s.mu.Unlock()
	if pr == nil {
		return nil, fmt.Errorf("no background process %q (os_process list shows them; a finished one is kept an hour)", id)
	}
	return pr, nil
}

// ProcessList is os_process list: one line per process, running first.
func (s *Service) ProcessList() string {
	s.prune()
	s.mu.Lock()
	all := make([]*proc, 0, len(s.procs))
	for _, pr := range s.procs {
		all = append(all, pr)
	}
	s.mu.Unlock()
	if len(all) == 0 {
		return "no background processes"
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Started.Before(all[j].Started) })
	var b strings.Builder
	for _, pr := range all {
		status, exited := pr.state()
		state := "running"
		if !exited.IsZero() {
			state = "exited 0"
			if status != "" {
				state = status
			}
			state += " " + exited.Format("15:04:05")
		}
		fmt.Fprintf(&b, "%s  pid %d  %s  started %s  %s\n", pr.ID, pr.PID, state, pr.Started.Format("15:04:05"), clip(pr.Command, 120))
	}
	return strings.TrimRight(b.String(), "\n")
}

// ProcessLog is os_process log: the log's lines from offset for limit lines
// (ReadFile's paging), or with neither the last logTail lines.
func (s *Service) ProcessLog(id string, offset, limit int) (string, error) {
	pr, err := s.find(id)
	if err != nil {
		return "", err
	}
	f, err := os.Open(pr.Log)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if offset <= 0 && limit <= 0 {
		// ponytail: two passes (count, then read); a seek from the end if logs get big.
		b, err := io.ReadAll(f)
		if err != nil {
			return "", err
		}
		if n := strings.Count(string(b), "\n"); n > logTail {
			offset = n - logTail + 1
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
	}
	out, err := readLines(f, offset, limit, maxOutputBytes)
	if err != nil {
		return "", err
	}
	status, _ := pr.state()
	switch {
	case status != "":
		out += "\n[" + status + "]"
	case s.running(pr):
		out += "\n[still running]"
	default:
		out += "\n[exited 0]"
	}
	return out, nil
}

func (s *Service) running(pr *proc) bool {
	select {
	case <-pr.pumped:
		return false
	default:
		return true
	}
}

// ProcessWait is os_process wait: block until the process exits or timeout
// passes, then the exit and the log's tail.
func (s *Service) ProcessWait(ctx context.Context, id string, timeout time.Duration) (string, error) {
	pr, err := s.find(id)
	if err != nil {
		return "", err
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > waitMax {
		timeout = waitMax
	}
	select {
	case <-pr.pumped:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(timeout):
		tail, _ := s.ProcessLog(id, 0, 0)
		return fmt.Sprintf("%s is still running after %s\n%s", pr.ID, timeout, tail), nil
	}
	return s.ProcessLog(id, 0, 0)
}

// ProcessKill is os_process kill: stdin closes, then the process is killed
// after procGrace.
func (s *Service) ProcessKill(id string) (string, error) {
	pr, err := s.find(id)
	if err != nil {
		return "", err
	}
	if !s.running(pr) {
		status, _ := pr.state()
		return fmt.Sprintf("%s had already exited (%s)", pr.ID, orExit0(status)), nil
	}
	if err := pr.p.Close(procGrace); err != nil {
		return "", err
	}
	<-pr.pumped
	status, _ := pr.state()
	return fmt.Sprintf("killed %s (%s)", pr.ID, orExit0(status)), nil
}

// ProcessWrite is os_process write: one line to the process's stdin, which
// answers a y/n prompt without a terminal.
func (s *Service) ProcessWrite(id, input string) (string, error) {
	pr, err := s.find(id)
	if err != nil {
		return "", err
	}
	if !s.running(pr) {
		return "", fmt.Errorf("%s has exited; nothing reads its stdin", pr.ID)
	}
	if _, err := io.WriteString(pr.p.Stdin(), input+"\n"); err != nil {
		return "", fmt.Errorf("write to %s: %w", pr.ID, err)
	}
	return fmt.Sprintf("sent %q to %s", input, pr.ID), nil
}

func orExit0(status string) string {
	if status == "" {
		return "exit status 0"
	}
	return status
}

// Close kills every running background process: they die with Tenant. The
// plugin's cleanup calls it at shutdown.
func (s *Service) Close() {
	s.mu.Lock()
	all := make([]*proc, 0, len(s.procs))
	for _, pr := range s.procs {
		all = append(all, pr)
	}
	s.mu.Unlock()
	for _, pr := range all {
		if s.running(pr) {
			_ = pr.p.Close(procGrace)
		}
	}
}
