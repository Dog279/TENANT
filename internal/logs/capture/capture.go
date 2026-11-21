// Package capture files a child process in the log book (logging v2 §8.4):
// a line when it starts, what it writes to stderr, line by line and
// rate-limited, and a line when it exits, all in the log of the subsystem
// that started it and tagged with who it is (server=<name>, child=<name>).
//
// The parent files the child's output as data: each line is the message of
// an event whose log and tags the parent chose, marked stream=stderr. A line
// the child prints never lands in another log, or passes for one of Tenant's
// own, however it is shaped (§11).
//
// The child's environment gets TENANT_TRACE_ID and TENANT_ACTIVITY_ID, so
// what it logs itself, through the API, joins the work that started it (§7).
//
// One Child is one process, whatever runs it: exec directly (Run), or a
// sandbox backend, fed through the sandbox's hooks (EnvPolicy.Add takes
// Env, Policy.Stderr takes the Child), so every backend is captured the same
// way.
//
// While it runs, a child is a component (internal/lifecycle): the Components
// view lists it, and its start and end are system events.
package capture

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"tenant/internal/lifecycle"
	"tenant/internal/logs/event"
	"tenant/internal/procprotect"
	"tenant/internal/runtrace"
)

// The variables a child's environment gets.
const (
	EnvTrace    = "TENANT_TRACE_ID"
	EnvActivity = "TENANT_ACTIVITY_ID"
)

// A child's stderr is rate-limited by a token bucket per child: a burst that
// holds a crash's stack trace, then a steady rate, so a noisy child can't
// flood the log book. What the limit drops is counted and filed as one line
// when lines flow again, and at exit.
const (
	burst  = 200
	perSec = 2.0
	// maxLine is where a line with no end is cut. The rest of it is dropped,
	// not filed as a second line a secret could straddle, out of the
	// scrubber's sight.
	maxLine = 16 << 10
)

// Child is one child process as its parent files it. Its methods are safe
// for concurrent use.
type Child struct {
	ctx  context.Context // the work that started it: correlation, and whether Tenant stopped it
	log  string
	tags []any

	mu      sync.Mutex
	started time.Time
	running bool
	partial []byte
	cut     bool // dropping the rest of an over-long line
	tokens  float64
	refill  time.Time
	dropped int
	comp    *lifecycle.Comp // while it runs
}

// New describes a child about to start. Its lines go into log, each tagged
// with tags ("server", name or "child", name). ctx is the work that starts
// it; its trace and activity are the child's.
func New(ctx context.Context, log string, tags ...any) *Child {
	now := time.Now()
	return &Child{ctx: ctx, log: log, tags: tags, started: now, tokens: burst, refill: now}
}

// Env is what the child's environment gets: the trace and the activity its
// ctx runs under, when it runs under them.
func (c *Child) Env() []string {
	var env []string
	if id := runtrace.IDFromContext(c.ctx); id != "" {
		env = append(env, EnvTrace+"="+id)
	}
	if id := event.ActivityFromContext(c.ctx); id != "" {
		env = append(env, EnvActivity+"="+id)
	}
	return env
}

// Started files the start line, with kv (pid, backend, limits…), and starts
// the clock the exit line's duration is measured by.
func (c *Child) Started(kv ...any) {
	comp := lifecycle.Track(c.ctx, c.name(), lifecycle.Child, c.log, lifecycle.Running, nil)
	c.mu.Lock()
	c.started, c.running, c.comp = time.Now(), true, comp
	c.mu.Unlock()
	c.file(event.Info, "process started", kv)
}

// name is the child's name in the Components view: its first tag's value
// (the server's or the child's name, or the sandbox's surface), else its log.
func (c *Child) name() string {
	if len(c.tags) > 1 {
		if s, ok := c.tags[1].(string); ok && s != "" {
			return s
		}
	}
	return c.log
}

// Write files each line of what the child writes to stderr.
func (c *Child) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			c.add(p)
			break
		}
		c.add(p[:i])
		if !c.cut {
			c.line()
		}
		c.cut = false
		p = p[i+1:]
	}
	return n, nil
}

// add appends p to the line being read, up to maxLine.
func (c *Child) add(p []byte) {
	if c.cut {
		return
	}
	if room := maxLine - len(c.partial); len(p) >= room {
		c.partial = append(c.partial, p[:room]...)
		c.line()
		c.cut = true
		return
	}
	c.partial = append(c.partial, p...)
}

// line files the line read so far, if the rate limit lets it.
func (c *Child) line() {
	s := strings.TrimSuffix(string(c.partial), "\r")
	c.partial = c.partial[:0]
	if strings.TrimSpace(s) == "" {
		return
	}
	now := time.Now()
	c.tokens = min(burst, c.tokens+now.Sub(c.refill).Seconds()*perSec)
	c.refill = now
	if c.tokens < 1 {
		c.dropped++
		return
	}
	c.tokens--
	c.flushDropped()
	c.file(event.Info, s, []any{"stream", "stderr"})
}

// flushDropped files how many lines the rate limit dropped since the last
// such line: loss is data.
// ponytail: the count waits for the child's next filed line or its exit, so a
// flood followed by silence is reported late; a timer if that ever matters.
func (c *Child) flushDropped() {
	if c.dropped > 0 {
		c.file(event.Warning, "suppressed "+strconv.Itoa(c.dropped)+" lines of stderr (rate limit)", []any{"suppressed", c.dropped})
		c.dropped = 0
	}
}

// Exited files the exit line, after a last line left with no newline and
// the count of lines the rate limit dropped. err is what Wait returned: nil
// is exit 0, and an error with an ExitCode method (*exec.ExitError) gives
// the code. A child that failed is a warning, unless its ctx was cancelled:
// Tenant stopped it. kv are added (killed, timed_out, pid…).
func (c *Child) Exited(err error, kv ...any) {
	c.mu.Lock()
	if !c.cut {
		c.line()
	}
	c.partial, c.cut = nil, false
	c.flushDropped()
	running, took, comp := c.running, time.Since(c.started), c.comp
	c.mu.Unlock()

	lvl, msg := event.Info, "process exited"
	var fields []any
	var code interface{ ExitCode() int }
	switch {
	case err == nil:
		fields = append(fields, "exit", 0)
	case !running:
		lvl, msg = event.Warning, "process did not start: "+err.Error()
	default:
		msg += ": " + err.Error()
		if errors.As(err, &code) {
			fields = append(fields, "exit", code.ExitCode())
		}
		if errors.Is(c.ctx.Err(), context.Canceled) {
			fields = append(fields, "cancelled", true)
		} else {
			lvl = event.Warning
		}
	}
	if running {
		fields = append(fields, "duration", took)
	}
	c.file(lvl, msg, append(fields, kv...))
	st := lifecycle.Stopped
	if lvl == event.Warning {
		st = lifecycle.Failed
	}
	switch {
	case comp != nil:
		comp.Set(st, err)
	case st == lifecycle.Failed: // it never ran: filed, never listed
		lifecycle.Track(c.ctx, c.name(), lifecycle.Child, c.log, st, err)
	}
}

// Run runs cmd as this child: its environment gets Env, and its start and
// exit are filed. Its output goes where the caller sent it; to file its
// stderr too, send that to the Child as well (io.MultiWriter(&buf, c)).
func (c *Child) Run(cmd *exec.Cmd) error {
	if env := c.Env(); len(env) > 0 {
		if cmd.Env == nil {
			cmd.Env = os.Environ()
		}
		cmd.Env = slices.Concat(cmd.Env, env)
	}
	if err := procprotect.Start(cmd); err != nil {
		c.Exited(err)
		return err
	}
	c.Started("pid", cmd.Process.Pid)
	err := cmd.Wait()
	c.Exited(err)
	return err
}

func (c *Child) file(lvl event.Level, msg string, kv []any) {
	event.Line(c.ctx, c.log, lvl, msg, slices.Concat(c.tags, kv)...)
}
